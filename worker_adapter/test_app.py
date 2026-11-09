import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from uuid import uuid4

import httpx

from app import Adapter, OUTFIT_MODEL, VIDEO_MODEL, WARDROBE_MODEL, app


class ModelKeyWorkerTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.calls = []
        self.native_response = httpx.Response(422)

        async def native(request):
            self.calls.append((request.method, request.url.path))
            if request.method == "GET":
                return httpx.Response(200, json={"job_id": "old-job", "status": "error"})
            return self.native_response

        # 保留真实的请求处理和文件账本，只替换原生服务与对象存储启动配置。
        worker = Adapter.__new__(Adapter)
        worker.state_dir = Path(directory.name)
        worker.bearer = "Bearer worker-test"
        worker.model_keys = {OUTFIT_MODEL, WARDROBE_MODEL, "Vmind_cloth_gen_v1.0", VIDEO_MODEL}
        worker.native = "http://native"
        worker.native_headers = {}
        worker.video_protocol = "jobs"
        worker.video_target = "ltx-gpu0"
        worker.vton_edits = False
        worker.running = {}
        worker.http = httpx.AsyncClient(transport=httpx.MockTransport(native))
        self.addAsyncCleanup(worker.http.aclose)
        app.state.worker = self.worker = worker
        self.client = httpx.AsyncClient(
            transport=httpx.ASGITransport(app=app),
            base_url="http://worker",
            headers={"Authorization": worker.bearer},
        )
        self.addAsyncCleanup(self.client.aclose)
        self.input = {"images": [{"data": "eA==", "mime_type": "image/png"}]}

    async def test_new_requests_route_by_model_key(self):
        for model, path in (
            ("Vmind_cloth_gen_v1.0", "/v2/images/product_generation"),
            (WARDROBE_MODEL, "/v2/images/garment_extraction"),
        ):
            request_id = str(uuid4())
            body = {"request_id": request_id, "model_key": model, "input": self.input}
            result = await self.client.post("/requests", json=body)
            self.assertEqual(result.status_code, 200, result.text)
            self.assertEqual(result.json()["error"]["code"], "model_rejected")
            self.assertEqual(self.calls[-1], ("POST", path))
            row = self.worker.load_file(request_id)
            self.assertEqual(row["model_key"], model)
            self.assertNotIn("capability", row)
        result = await self.client.post("/requests", json={
            "request_id": str(uuid4()), "model_key": WARDROBE_MODEL, "capability": "cloth_gen", "input": self.input,
        })
        self.assertEqual(result.status_code, 400)
        self.assertEqual(len(self.calls), 2)

    async def test_native_404_only_finishes_confirmed_rejections(self):
        for response, expected_state in (
            (httpx.Response(404, json={"detail": "Not Found"}), "failed"),
            (httpx.Response(404, text="not found\n"), "failed"),
            (httpx.Response(404, json={"detail": "garment not found"}), "processing"),
        ):
            with self.subTest(body=response.text):
                self.native_response = response
                request_id = str(uuid4())
                result = await self.client.post("/requests", json={
                    "request_id": request_id, "model_key": OUTFIT_MODEL,
                    "input": {"images": self.input["images"] * 2},
                })
                self.assertEqual(result.status_code, 200 if expected_state == "failed" else 502, result.text)
                self.assertEqual(self.calls[-1], ("POST", "/v2/images/vton"))
                status = await self.client.get(f"/requests/{request_id}")
                self.assertEqual(status.json()["state"], expected_state)
                if expected_state == "failed":
                    self.assertEqual(status.json()["error"]["code"], "native_route_unavailable")

    async def test_old_video_keeps_fingerprint_and_backend_job(self):
        request_id = str(uuid4())
        old_identity = json.dumps([VIDEO_MODEL, "video", self.input], sort_keys=True, allow_nan=False).encode()
        self.worker.save_file({
            "request_id": request_id, "capability": "video", "fingerprint": hashlib.sha256(old_identity).hexdigest(),
            "sent": True, "state": "processing", "backend_task_id": "old-job",
        })
        body = {"request_id": request_id, "model_key": VIDEO_MODEL, "input": self.input}
        retry = await self.client.post("/requests", json=body)
        self.assertEqual(retry.status_code, 202, retry.text)
        self.assertEqual(retry.json()["backend_task_id"], "old-job")
        conflict = await self.client.post("/requests", json={**body, "input": {**self.input, "prompt": "changed"}})
        self.assertEqual(conflict.status_code, 409)
        status = await self.client.get(f"/requests/{request_id}")
        self.assertEqual(status.json()["state"], "failed")
        self.assertEqual(self.calls, [("GET", "/jobs/old-job")])
