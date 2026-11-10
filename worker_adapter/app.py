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
from contextlib import ExitStack, asynccontextmanager
from pathlib import Path
from uuid import UUID

import httpx
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse

LOG = logging.getLogger("vmind-worker-adapter")
MAX_JSON = 65 * 1024**2
MAX_VIDEO = 200 * 1024**2
WARDROBE_MODEL = "Vmind_wardrobe_v1.0"
OUTFIT_MODEL = "Vmind_outfit_v1.0"
VIDEO_MODEL = "Vmind_outfit_video_v1.0"
ROUTES = {
    # 网关和适配器使用同一模型键，拆衣输入不会流入商品图接口。
    "Vmind_cloth_gen_v1.0": ("/v2/images/product_generation", ("image",)),
    WARDROBE_MODEL: ("/v2/images/garment_extraction", ("image", "parse_mask")),
    OUTFIT_MODEL: ("/v2/images/vton", ("person_image", "garment_image")),
    "Vmind_bodymodel_v1.0": ("/v2/images/body_model", ("image",)),
    VIDEO_MODEL: ("/vton", ("image",)),
}


def native_route_not_found(response: httpx.Response) -> bool:
    content_type = response.headers.get("content-type", "").partition(";")[0].strip().lower()
    if content_type == "text/plain":
        return response.content.strip().lower() == b"not found"
    if content_type == "application/json":
        try:
            return response.json() == {"detail": "Not Found"}
        except ValueError:
            return False
    return False


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

    def create_state(self, row, phase):
        from oss2.exceptions import ServerError

        key = f"outputs/{row['request_id']}/_worker-{phase}.json"
        try:
            self.client.put_object(key, json.dumps(row).encode(), headers={
                "Content-Type": "application/json", "x-oss-forbid-overwrite": "true",
            })
        except ServerError as exc:
            if exc.code == "FileAlreadyExists":
                return False
            raise
        return True

    def load_state(self, request_id):
        from oss2.exceptions import NoSuchKey

        # 三份小记录只增不改。后续阶段优先，重启时不会退回到尚未受理。
        for phase in ("terminal", "accepted", "claimed"):
            try:
                result = self.client.get_object(f"outputs/{request_id}/_worker-{phase}.json")
            except NoSuchKey:
                continue
            try:
                return json.loads(result.read(64 * 1024))
            finally:
                result.close()
        return None


class Adapter:
    def __init__(self):
        self.objects = Objects()
        state_backend = os.environ.get("VMIND_WORKER_STATE_BACKEND", "file")
        if state_backend not in ("file", "oss") or (state_backend == "oss" and self.objects.provider != "oss"):
            raise ValueError("invalid worker state backend")
        self.state_dir = None
        if state_backend == "file":
            self.state_dir = Path(os.environ["VMIND_WORKER_STATE_DIR"])
            self.state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.bearer = "Bearer " + os.environ["VMIND_WORKER_BEARER"]
        if len(self.bearer) < len("Bearer ") + 24:
            raise ValueError("worker bearer is too short")
        self.model_keys = set(os.environ["VMIND_WORKER_MODEL_KEYS"].split(","))
        if not self.model_keys <= ROUTES.keys():
            raise ValueError("invalid worker model keys")
        self.instance_id = os.environ["VMIND_WORKER_INSTANCE_ID"]
        self.artifact = os.environ["VMIND_WORKER_ARTIFACT_DIGEST"]
        self.native = os.environ["VMIND_NATIVE_ORIGIN"].rstrip("/")
        self.health_url = os.environ["VMIND_NATIVE_HEALTH_URL"]
        self.vton_edits = os.environ.get("VMIND_NATIVE_VTON_EDITS") == "true"
        self.video_protocol = os.environ.get("VMIND_NATIVE_VIDEO_PROTOCOL", "gateway")
        if self.video_protocol not in ("gateway", "jobs"):
            raise ValueError("invalid native video protocol")
        self.video_target = os.environ.get("VMIND_NATIVE_VIDEO_TARGET", "").strip()
        if self.video_protocol == "jobs" and VIDEO_MODEL in self.model_keys and not self.video_target:
            raise ValueError("native video target instance is required")
        self.native_headers = {}
        if token := os.environ.get("VMIND_NATIVE_TOKEN"):
            self.native_headers["X-Token"] = token
        username = os.environ.get("VMIND_NATIVE_USERNAME")
        password = os.environ.get("VMIND_NATIVE_PASSWORD")
        if bool(username) != bool(password):
            raise ValueError("native basic authentication requires both fields")
        self.http = httpx.AsyncClient(
            auth=(username, password) if username else None,
            timeout=httpx.Timeout(180.0, connect=5.0), trust_env=False, follow_redirects=False,
        )
        self.running: dict[str, asyncio.Task] = {}

    def authorize(self, request):
        if not hmac.compare_digest(request.headers.get("Authorization", "").encode(), self.bearer.encode()):
            raise HTTPException(401, "invalid worker credential")

    def load_file(self, request_id):
        path = self.state_dir / (request_id + ".json")
        try:
            return json.loads(path.read_text())
        except FileNotFoundError:
            return None

    def save_file(self, row):
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

    async def claim(self, row):
        if self.state_dir is None:
            # ACK 没有持久盘。先独占写下发送意图，才能跨进程去重并安全回查。
            row["sent"] = True
            return await asyncio.to_thread(self.objects.create_state, row, "claimed")
        if self.load_file(row["request_id"]) is not None:
            return False
        self.save_file(row)
        return True

    async def load(self, request_id):
        if self.state_dir is None:
            return await asyncio.to_thread(self.objects.load_state, request_id)
        return self.load_file(request_id)

    async def save(self, row):
        if self.state_dir is None:
            phase = "accepted" if row["state"] == "processing" else "terminal"
            await asyncio.to_thread(self.objects.create_state, row, phase)
        else:
            self.save_file(row)

    def spawn(self, request_id, coroutine):
        task = asyncio.create_task(coroutine)
        self.running[request_id] = task
        task.add_done_callback(lambda _task: self.running.pop(request_id, None))
        return task

    async def fail(self, row, code, message):
        row.update(state="failed", error={"code": code, "message": message})
        await self.save(row)

    def response(self, row):
        keys = ("request_id", "state", "backend_task_id", "result", "error")
        return JSONResponse(
            {key: row[key] for key in keys if key in row},
            status_code=202 if row["state"] == "processing" else 200,
        )

    def multipart(self, value, path, fields):
        business = value["input"]
        images = business["images"]
        model_key = value["model_key"]
        minimum = 1 if model_key == WARDROBE_MODEL else len(fields)
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
        if self.vton_edits and model_key == OUTFIT_MODEL:
            # 国内 FLUX 服务的试衣入口使用两个同名 image 文件，由 LoRA 选择试衣能力。
            path = "/v1/images/edits"
            files = [("image", (f"{name}.{mime[6:]}", data, mime)) for name, (_, data, mime) in files]
            form.update(model="FLUX.2-klein-9B", lora_name="vton", lora_strength="1.0",
                        n="1", size="768x1024", response_format="b64_json")
        if model_key == VIDEO_MODEL and self.video_protocol == "jobs":
            # 原生视频服务包含多个实例，固定本部署的目标以兑现网关登记的 GPU 分组。
            form["target_instance"] = self.video_target
        metadata = value.get("metadata") or {}
        headers = {"X-Gateway-Request-ID": value["request_id"], "X-Gateway-Dispatch": "immediate"}
        if trace := metadata.get("trace_id"):
            form["trace_id"] = trace
            headers["X-Trace-ID"] = trace
        if user := metadata.get("user_id"):
            # 图片接口按白名单读表单，用户归属只走请求头。
            headers["X-User-ID"] = user
        return path, form, files, headers

    async def execute(self, row, path, form, files, headers):
        # 一旦标记已发送，重启后只核对原任务，绝不再次触发推理。
        if not row["sent"]:
            row["sent"] = True
            await self.save(row)
        headers = {**self.native_headers, **headers}
        try:
            if row["model_key"] == VIDEO_MODEL and self.video_protocol == "gateway":
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
                await self.fail(row, "model_rejected", "native model rejected the request before execution")
                return True
            # 提交时的标准路由拒绝能确认模型没接单，其他 404 仍需保留执行记录等待核对。
            if response.status_code == 404 and native_route_not_found(response):
                await self.fail(row, "native_route_unavailable", "native model route is unavailable")
                return True
            if not response.is_success:
                LOG.warning("native model response status=%s model_key=%s request_id=%s",
                            response.status_code, row["model_key"], row["request_id"])
                return False
        except (httpx.ConnectError, httpx.ConnectTimeout):
            await self.fail(row, "native_unreachable", "model connection was not established")
            return True
        except (httpx.HTTPError, ValueError):
            return False
        if row["model_key"] == VIDEO_MODEL:
            expected = "gw-" + UUID(row["request_id"]).hex
            try:
                job = response.json().get("job_id")
                if not isinstance(job, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,127}", job):
                    return False
                if self.video_protocol == "gateway" and job != expected:
                    return False
            except (ValueError, AttributeError):
                return False
            # /vton 分配自己的 job_id，受理后先持久化，再允许网关继续轮询。
            row["backend_task_id"] = job
            await self.save(row)
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
        except Exception as exc:
            # 此时图片执行已返回，上传失败属于结果发布失败，可以安全释放 GPU。
            LOG.warning("image result publication failed (%s)", type(exc).__name__)
            await self.fail(row, "result_unavailable", "cannot publish image result")
            return True
        await self.save(row)
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
        except Exception as exc:
            LOG.warning("video result publication failed (%s)", type(exc).__name__)
            await self.fail(row, "result_unavailable", "cannot publish video result")
            return
        await self.save(row)

    async def refresh_video(self, row):
        request_id = row["request_id"]
        native_id = UUID(request_id).hex
        if self.video_protocol == "jobs":
            job = row.get("backend_task_id")
            # 国内原生端无法按外部 UUID 查找，受理响应不明时保持原执行占位。
            if not job:
                return
            path = "/jobs/" + job
        else:
            job = "gw-" + native_id
            path = "/_gateway/tasks/" + native_id
        try:
            response = await self.http.get(
                self.native + path, headers=self.native_headers, timeout=2.0
            )
            if not response.is_success:
                return
            native = response.json()
        except (httpx.HTTPError, ValueError):
            return
        # 查询期间另一副本可能已开始上传或写终态，需要重读最新记录。
        latest = await self.load(request_id)
        if not latest or latest["state"] != "processing" or request_id in self.running:
            return
        if native.get("job_id", job if self.video_protocol == "jobs" else "") != job:
            return
        latest["backend_task_id"] = job
        if native.get("status") == "done":
            self.spawn(request_id, self.publish_video(latest, job))
        elif native.get("status") in ("error", "cancelled", "interrupted"):
            await self.fail(latest, "model_failed", "native video execution failed")


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


def request_fingerprint(identity):
    return hashlib.sha256(json.dumps(identity, sort_keys=True, allow_nan=False).encode()).hexdigest()


@asynccontextmanager
async def lifespan(app):
    worker = Adapter()
    with ExitStack() as stack:
        if worker.state_dir is not None:
            # 本地目录只允许一个持有者。OSS 使用每个请求的独占写入去重。
            lock = stack.enter_context((worker.state_dir / ".lock").open("a"))
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

@app.get("/health/live")
async def live():
    return {"live": True}


@app.get("/health/ready")
async def ready(request: Request):
    worker = request.app.state.worker
    worker.authorize(request)
    try:
        response = await worker.http.get(worker.health_url, headers=worker.native_headers, timeout=2.0)
        data = response.json()
        ready = response.is_success and (data.get("ready") is True or ("ready" not in data and data.get("status") == "ok"))
        if worker.video_protocol == "jobs" and VIDEO_MODEL in worker.model_keys:
            ready = ready and any(item.get("name") == worker.video_target for item in data.get("instances", []))
        if "workers" in data:
            # 国内图片服务以 ready 标记可接单的实例，外层 status 只代表服务进程存活。
            ready = ready and any(item.get("ready", item.get("healthy")) is True for item in data["workers"])
    except (httpx.HTTPError, ValueError, AttributeError):
        ready = False
    # 发布入口把当前 Worker 镜像的真实摘要注入环境，网关登记时可从健康响应读取。
    return {"ready": ready, "artifact_digest": worker.artifact}


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
        # 旧消息的两个字段可能表达不同业务，升级后只接受单一模型键的请求。
        if "capability" in value:
            raise ValueError("use model_key only")
        request_id = canonical_id(value["request_id"])
        model_key = value["model_key"]
        if model_key not in worker.model_keys:
            raise ValueError("unexpected model key")
        path, fields = ROUTES[model_key]
        fingerprint = request_fingerprint([model_key, value["input"]])
        path, form, files, headers = worker.multipart(value, path, fields)
    except (ValueError, KeyError, TypeError, AttributeError) as exc:
        raise HTTPException(400, "invalid model request") from exc
    row = {"request_id": request_id, "fingerprint": fingerprint, "model_key": model_key,
           "state": "processing", "sent": False}
    if not await worker.claim(row):
        existing = await worker.load(request_id)
        if existing is None:
            raise HTTPException(503, "cannot read the existing execution record")
        # 旧记录的指纹包含历史能力。保留旧记录的重试语义，避免同一请求重复执行。
        if "model_key" not in existing:
            fingerprint = request_fingerprint([model_key, existing.get("capability"), value["input"]])
        if existing["fingerprint"] != fingerprint:
            raise HTTPException(409, "request_id already used")
        return worker.response(existing)
    task = worker.spawn(request_id, worker.execute(row, path, form, files, headers))
    # 网关连接取消时仍要获取本次推理终态，不能把 HTTP 连接当作执行生命周期。
    if not await asyncio.shield(task):
        raise HTTPException(502, "native model outcome is unknown")
    return worker.response(await worker.load(request_id))


@app.get("/requests/{request_id}")
async def status(request_id: str, request: Request):
    worker = request.app.state.worker
    worker.authorize(request)
    try:
        canonical_id(request_id)
    except ValueError as exc:
        raise HTTPException(400, "invalid request_id") from exc
    row = await worker.load(request_id)
    if row is None:
        raise HTTPException(404, "request not found")
    if row["state"] == "processing" and request_id not in worker.running:
        if not row["sent"]:
            await worker.fail(row, "not_dispatched", "worker stopped before dispatch")
        # 旧适配器账本只有能力字段。按原任务回查视频，等旧账本退出保留期后可删除这个分支。
        elif row.get("model_key") == VIDEO_MODEL or ("model_key" not in row and row.get("capability") == "video"):
            await worker.refresh_video(row)
        # 图片模型尚未提供按网关 ID 查询执行状态。发生未知断线时保留记录，等待核对。
    return worker.response(await worker.load(request_id))
