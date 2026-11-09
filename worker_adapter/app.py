"""为现有 VMind 图片与 LTX 实例提供统一执行接口。模型侧持有执行状态和对象上传。"""

from __future__ import annotations

import asyncio
import base64
import fcntl
import hashlib
import hmac
import io
import json
import logging
import os
import re
import tempfile
from contextlib import asynccontextmanager
from pathlib import Path
from uuid import UUID

import httpx
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse

LOG = logging.getLogger("vmind-worker-adapter")
MAX_JSON = 65 * 1024**2
MAX_VIDEO = 200 * 1024**2
ROUTES = {
    "cloth_gen": ("Vmind_wardrobe_v1.0", "/v2/images/product_generation", ("image",)),
    "garment_extraction": (
        "Vmind_wardrobe_v1.0", "/v2/images/garment_extraction", ("image", "parse_mask")
    ),
    "vton": ("Vmind_outfit_v1.0", "/v2/images/vton", ("person_image", "garment_image")),
    "body_model": ("Vmind_bodymodel_v1.0", "/v2/images/body_model", ("image",)),
    "video": ("Vmind_outfit_video_v1.0", "/vton", ("image",)),
}


class Objects:
    def __init__(self):
        self.provider = os.environ["VMIND_RESULT_PROVIDER"]
        self.region = os.environ["VMIND_RESULT_REGION"]
        self.bucket = os.environ["VMIND_RESULT_BUCKET"]
        endpoint = os.environ.get("VMIND_RESULT_ENDPOINT")
        if not self.region or not self.bucket:
            raise ValueError("result region and bucket are required")
        if self.provider == "s3":
            import boto3
            from botocore.config import Config

            # AWS 默认走执行身份。测试和私有端点由部署配置决定，调用方不能覆盖。
            self.client = boto3.client(
                "s3", region_name=self.region, endpoint_url=endpoint,
                config=Config(request_checksum_calculation="when_required"),
            )
        elif self.provider == "oss":
            import oss2

            if not endpoint:
                raise ValueError("OSS result endpoint is required")
            auth = oss2.Auth(os.environ["OSS_ACCESS_KEY_ID"], os.environ["OSS_ACCESS_KEY_SECRET"])
            self.client = oss2.Bucket(auth, endpoint, self.bucket)
        else:
            raise ValueError("result provider must be s3 or oss")

    def put(self, key, content_type, body):
        if self.provider == "s3":
            self.client.put_object(Bucket=self.bucket, Key=key, ContentType=content_type, Body=body)
        else:
            self.client.put_object(key, body, headers={"Content-Type": content_type})
        return {"provider": self.provider, "region": self.region, "bucket": self.bucket, "key": key}


class Adapter:
    def __init__(self):
        self.state_dir = Path(os.environ["VMIND_WORKER_STATE_DIR"])
        self.state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.bearer = "Bearer " + os.environ["VMIND_WORKER_BEARER"]
        if len(self.bearer) < len("Bearer ") + 24:
            raise ValueError("worker bearer is too short")
        self.capabilities = set(os.environ["VMIND_WORKER_CAPABILITIES"].split(","))
        if not self.capabilities or not self.capabilities <= ROUTES.keys():
            raise ValueError("invalid worker capabilities")
        self.instance_id = os.environ["VMIND_WORKER_INSTANCE_ID"]
        self.artifact = os.environ["VMIND_WORKER_ARTIFACT_DIGEST"]
        self.native = os.environ["VMIND_NATIVE_ORIGIN"].rstrip("/")
        self.health_url = os.environ["VMIND_NATIVE_HEALTH_URL"]
        self.native_headers = {}
        if token := os.environ.get("VMIND_NATIVE_TOKEN"):
            self.native_headers["X-Token"] = token
        self.objects = Objects()
        self.http = httpx.AsyncClient(
            timeout=httpx.Timeout(180.0, connect=5.0), trust_env=False, follow_redirects=False
        )
        self.running: dict[str, asyncio.Task] = {}

    def authorize(self, request):
        if not hmac.compare_digest(request.headers.get("Authorization", "").encode(), self.bearer.encode()):
            raise HTTPException(401, "invalid worker credential")

    def load(self, request_id):
        path = self.state_dir / (request_id + ".json")
        try:
            return json.loads(path.read_text())
        except FileNotFoundError:
            return None

    def save(self, row):
        # 先持久写完整记录再原子替换，崩溃后查询只会看到一份完整快照。
        with tempfile.NamedTemporaryFile(mode="w", dir=self.state_dir, delete=False) as temp:
            name = temp.name
            json.dump(row, temp, ensure_ascii=False)
            temp.flush()
            os.fsync(temp.fileno())
        try:
            os.replace(name, self.state_dir / (row["request_id"] + ".json"))
            directory = os.open(self.state_dir, os.O_RDONLY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            Path(name).unlink(missing_ok=True)

    def spawn(self, request_id, coroutine):
        task = asyncio.create_task(coroutine)
        self.running[request_id] = task
        task.add_done_callback(lambda _task: self.running.pop(request_id, None))
        return task

    def fail(self, row, code, message):
        row.update(state="failed", error={"code": code, "message": message})
        self.save(row)

    def response(self, row):
        keys = ("request_id", "state", "backend_task_id", "result", "error")
        return JSONResponse(
            {key: row[key] for key in keys if key in row},
            status_code=202 if row["state"] == "processing" else 200,
        )

    @staticmethod
    def multipart(value, fields):
        business = value["input"]
        images = business["images"]
        minimum = len(fields) if value["capability"] != "garment_extraction" else 1
        if not isinstance(images, list) or not minimum <= len(images) <= len(fields):
            raise ValueError("invalid images")
        files = []
        for name, image in zip(fields, images):
            data = base64.b64decode(image["data"], validate=True)
            mime = image["mime_type"]
            if not data or not isinstance(mime, str) or not re.fullmatch(r"image/[A-Za-z0-9.+-]+", mime):
                raise ValueError("invalid image bytes or MIME type")
            files.append((name, (name, data, mime)))
        form = {
            key: item if isinstance(item, str) else json.dumps(item, ensure_ascii=False)
            for key, item in business.items() if key != "images"
        }
        metadata = value.get("metadata") or {}
        headers = {"X-Gateway-Request-ID": value["request_id"], "X-Gateway-Dispatch": "immediate"}
        if trace := metadata.get("trace_id"):
            form["trace_id"] = trace
            headers["X-Trace-ID"] = trace
        if user := metadata.get("user_id"):
            # 图片接口按白名单读表单，用户归属只走请求头。
            headers["X-User-ID"] = user
        return form, files, headers

    async def execute(self, row, path, form, files, headers):
        # 一旦标记已发送，重启后只核对原任务，绝不再次触发推理。
        row["sent"] = True
        self.save(row)
        headers = {**self.native_headers, **headers}
        try:
            if row["capability"] == "video":
                multipart = self.http.build_request("POST", self.native + path, data=form, files=files)
                envelope = {
                    "task_id": UUID(row["request_id"]).hex,
                    "fingerprint": row["fingerprint"],
                    "model_version": self.artifact,
                    "instance_id": self.instance_id,
                    "endpoint": path,
                    "content_type": multipart.headers["content-type"],
                    "body_b64": base64.b64encode(multipart.read()).decode(),
                }
                response = await self.http.post(self.native + "/_gateway/execute", json=envelope, headers=headers)
            else:
                response = await self.http.post(self.native + path, data=form, files=files, headers=headers)
            if response.status_code in (400, 401, 403, 413, 415, 422, 429):
                self.fail(row, "model_rejected", "native model rejected the request before execution")
                return True
            if not response.is_success:
                return False
        except (httpx.ConnectError, httpx.ConnectTimeout):
            self.fail(row, "native_unreachable", "model connection was not established")
            return True
        except (httpx.HTTPError, ValueError):
            return False
        if row["capability"] == "video":
            expected = "gw-" + UUID(row["request_id"]).hex
            try:
                if response.json().get("job_id") != expected:
                    return False
            except (ValueError, AttributeError):
                return False
            row["backend_task_id"] = expected
            self.save(row)
            return True
        try:
            refs = []
            for index, image in enumerate(response.json()["data"]):
                data = base64.b64decode(image["b64_json"], validate=True)
                suffix, mime = image_type(data)
                key = f"outputs/{row['request_id']}/{index}.{suffix}"
                refs.append(await asyncio.to_thread(self.objects.put, key, mime, io.BytesIO(data)))
            if not refs:
                raise ValueError("empty image results")
            row.update(state="succeeded", result={"files": refs})
            self.save(row)
        except Exception as exc:
            # 此时图片执行已返回，上传失败属于结果发布失败，可以安全释放 GPU。
            LOG.warning("image result publication failed (%s)", type(exc).__name__)
            self.fail(row, "result_unavailable", "cannot publish image result")
        return True

    async def publish_video(self, row, job_id):
        try:
            # 先流式落临时文件，再由 SDK 上传，避免在适配器内存保留整个视频。
            with tempfile.TemporaryFile() as body:
                async with self.http.stream("GET", self.native + f"/jobs/{job_id}/video", headers=self.native_headers) as response:
                    response.raise_for_status()
                    size = 0
                    async for chunk in response.aiter_bytes(1 << 20):
                        size += len(chunk)
                        if size > MAX_VIDEO:
                            raise ValueError("video result is too large")
                        body.write(chunk)
                if not size:
                    raise ValueError("empty video result")
                body.seek(0)
                key = f"outputs/{row['request_id']}/0.mp4"
                result = await asyncio.to_thread(self.objects.put, key, "video/mp4", body)
            row.update(state="succeeded", result={"files": [result]})
            self.save(row)
        except Exception as exc:
            LOG.warning("video result publication failed (%s)", type(exc).__name__)
            self.fail(row, "result_unavailable", "cannot publish video result")

    async def refresh_video(self, row):
        request_id = row["request_id"]
        native_id = UUID(request_id).hex
        try:
            response = await self.http.get(
                self.native + "/_gateway/tasks/" + native_id, headers=self.native_headers, timeout=2.0
            )
            if not response.is_success:
                return
            native = response.json()
        except (httpx.HTTPError, ValueError):
            return
        # 查询期间另一副本可能已开始上传或写终态，需要重读最新记录。
        latest = self.load(request_id)
        if latest["state"] != "processing" or request_id in self.running:
            return
        job = "gw-" + native_id
        if native.get("job_id") != job:
            return
        latest["backend_task_id"] = job
        if native.get("status") == "done":
            self.spawn(request_id, self.publish_video(latest, job))
        elif native.get("status") in ("error", "cancelled", "interrupted"):
            self.fail(latest, "model_failed", "native video execution failed")


def image_type(data):
    if data.startswith(b"\x89PNG\r\n\x1a\n"):
        return "png", "image/png"
    if data.startswith(b"\xff\xd8\xff"):
        return "jpg", "image/jpeg"
    if data.startswith(b"RIFF") and data[8:12] == b"WEBP":
        return "webp", "image/webp"
    raise ValueError("unknown output image format")


def canonical_id(value):
    if str(UUID(value)) != value:
        raise ValueError("request_id must be a canonical UUID")
    return value


@asynccontextmanager
async def lifespan(app):
    worker = Adapter()
    # 一个部署实例持有一个本地账本。禁止第二个进程以同一身份同时派发。
    with (worker.state_dir / ".lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        app.state.worker = worker
        try:
            yield
        finally:
            jobs = list(worker.running.values())
            for job in jobs:
                job.cancel()
            await asyncio.gather(*jobs, return_exceptions=True)
            await worker.http.aclose()


app = FastAPI(lifespan=lifespan)


@app.get("/health/ready")
async def ready(request: Request):
    worker = request.app.state.worker
    worker.authorize(request)
    try:
        response = await worker.http.get(worker.health_url, headers=worker.native_headers, timeout=2.0)
        data = response.json()
        ready = response.is_success and (data.get("ready") is True or ("ready" not in data and data.get("status") == "ok"))
    except (httpx.HTTPError, ValueError):
        ready = False
    return {"ready": ready}


@app.post("/requests")
async def submit(request: Request):
    worker = request.app.state.worker
    worker.authorize(request)
    raw = bytearray()
    async for chunk in request.stream():
        raw.extend(chunk)
        if len(raw) > MAX_JSON:
            raise HTTPException(413, "request is too large")
    try:
        value = json.loads(raw)
        request_id = canonical_id(value["request_id"])
        capability = value["capability"]
        model, path, fields = ROUTES[capability]
        if capability not in worker.capabilities or model != value["model_key"]:
            raise ValueError("unexpected model capability")
        fingerprint = hashlib.sha256(
            json.dumps([model, capability, value["input"]], sort_keys=True, allow_nan=False).encode()
        ).hexdigest()
        form, files, headers = worker.multipart(value, fields)
    except (ValueError, KeyError, TypeError, AttributeError) as exc:
        raise HTTPException(400, "invalid model request") from exc
    row = worker.load(request_id)
    if row is not None:
        if row["fingerprint"] != fingerprint:
            raise HTTPException(409, "request_id already used")
        return worker.response(row)
    row = {"request_id": request_id, "fingerprint": fingerprint, "capability": capability,
           "state": "processing", "sent": False}
    worker.save(row)
    task = worker.spawn(request_id, worker.execute(row, path, form, files, headers))
    # 网关连接取消时仍要获取本次推理终态，不能把 HTTP 连接当作执行生命周期。
    if not await asyncio.shield(task):
        raise HTTPException(502, "native model outcome is unknown")
    return worker.response(worker.load(request_id))


@app.get("/requests/{request_id}")
async def status(request_id: str, request: Request):
    worker = request.app.state.worker
    worker.authorize(request)
    try:
        canonical_id(request_id)
    except ValueError as exc:
        raise HTTPException(400, "invalid request_id") from exc
    row = worker.load(request_id)
    if row is None:
        raise HTTPException(404, "request not found")
    if row["state"] == "processing" and request_id not in worker.running:
        if not row["sent"]:
            worker.fail(row, "not_dispatched", "worker stopped before dispatch")
        elif row["capability"] == "video":
            await worker.refresh_video(row)
        # 图片模型尚未提供按网关 ID 查询执行状态。发生未知断线时保留记录，等待核对。
    return worker.response(worker.load(request_id))
