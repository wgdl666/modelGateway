"""对已应用补丁的上游源码做隔离协议测试，不加载 GPU 模型。"""
import argparse
import ast
import asyncio
import io
import json
import threading
import types
import urllib.request
import urllib.error
from pathlib import Path


def definitions(path, names, env):
    tree = ast.parse(Path(path).read_text())
    nodes = [n for n in tree.body if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)) and n.name in names]
    for node in nodes:
        node.decorator_list = []
    exec(compile(ast.Module(body=nodes, type_ignores=[]), path, "exec"), env)


def test_decision(path):
    calls = []
    fail = False

    def open_request(req, timeout):
        calls.append(req.full_url)
        if fail:
            raise OSError("response lost after send")
        return io.BytesIO(b'{"prediction":"worker-1"}')

    env = {"threading": threading, "time": types.SimpleNamespace(), "json": json,
           "urllib": types.SimpleNamespace(request=types.SimpleNamespace(Request=urllib.request.Request, urlopen=open_request), error=urllib.error)}
    definitions(path, {"Pool", "invoke_worker"}, env)
    pool = env["Pool"](["http://127.0.0.1:19100", "http://127.0.0.1:19101"])
    pool.ready = {worker: True for worker in pool.workers}
    status, _ = env["invoke_worker"](pool, "/predict", b"image", "multipart/form-data", pool.workers[1])
    assert status == 200 and calls == [pool.workers[1] + "/predict"] and sum(pool.inflight.values()) == 0
    calls.clear()
    fail = True
    status, result = env["invoke_worker"](pool, "/predict", b"image", "multipart/form-data", pool.workers[1])
    assert status == 502 and result["_gateway_outcome_unknown"] and len(calls) == 1
    calls.clear()
    pool.ready[pool.workers[1]] = False
    status, _ = env["invoke_worker"](pool, "/predict", b"image", "multipart/form-data", pool.workers[1])
    assert status == 503 and calls == []

    # 自定义题目完成后不能预热其他卡，避免请求终态之后出现账本外的 GPU 执行。
    tree = ast.parse(Path(path).read_text())
    cls = next(n for n in tree.body if isinstance(n, ast.ClassDef) and n.name == "UIHandler")
    method = next(n for n in cls.body if isinstance(n, ast.FunctionDef) and n.name == "_public_predict")
    method.name = "predict"
    warmups = []
    env.update({"Path": Path,
                "parse_form": lambda *_: {"image": ("photo.jpg", b"image"), "prompt": (None, b"question")},
                "options_from_prompt": lambda _: ["A", "B"],
                "worker_multipart": lambda *_: (b"image", "multipart/form-data"),
                "invoke_worker": lambda *_: (200, {}),
                "send_json": lambda *_args, **_kwargs: None,
                "public_prediction": lambda result, **_: result,
                "warm_workers": lambda *_: None,
                "threading": types.SimpleNamespace(Thread=lambda **_: types.SimpleNamespace(start=lambda: warmups.append(True)))})
    exec(compile(ast.Module(body=[method], type_ignores=[]), path, "exec"), env)
    handler = types.SimpleNamespace(pool=pool, catalog=[])
    env["predict"](handler, b"body", "multipart/form-data; boundary=x", pool.workers[1])
    assert warmups == []
    env["predict"](handler, b"body", "multipart/form-data; boundary=x")
    assert warmups == [True]


async def test_g6(path):
    class Response:
        def __init__(self, body, status_code=200, headers=None, **_):
            self.body, self.status_code, self.headers = body, status_code, headers or {}

    class URL(str):
        def copy_with(self, **_): return self

    class Client:
        fail = False
        calls = []
        def build_request(self, method, url, **kwargs):
            return (method, url, kwargs)
        async def send(self, req, **_):
            self.calls.append(req)
            if self.fail: raise HTTPError()
            async def raw(): yield b"image-result"
            async def close(): pass
            return types.SimpleNamespace(status_code=200, headers={"content-type":"image/png"}, aiter_raw=raw, aclose=close)

    class HTTPError(Exception): pass
    env = {"Request": object, "JSONResponse": Response, "StreamingResponse": Response,
           "BackgroundTask": lambda fn: fn, "HOP": set(),
           "httpx": types.SimpleNamespace(URL=URL, HTTPError=HTTPError),
           "PORTS": {"segment-person":6106, "human-yolo":6102}}
    definitions(path, {"choose_worker", "proxy"}, env)
    client = Client()
    state = types.SimpleNamespace(workers=[{"port":6106,"ready":True,"inflight":0,"last_selected":0},
                                           {"port":6111,"ready":True,"inflight":0,"last_selected":0}], selection=0, client=client)
    req = types.SimpleNamespace(app=types.SimpleNamespace(state=state), path_params={"target_port":"6111"},
                                method="POST", url=types.SimpleNamespace(query=""), headers={}, stream=lambda: b"image")
    response = await env["proxy"]("segment-person", "api/segment", req)
    assert client.calls[0][1] == "http://127.0.0.1:6111/api/segment"
    assert state.workers[1]["inflight"] == 1 and state.workers[0]["inflight"] == 0
    assert b"".join([part async for part in response.body]) == b"image-result"
    assert state.workers[1]["inflight"] == 0
    state.workers[1]["ready"] = False
    response = await env["proxy"]("segment-person", "api/segment", req)
    assert response.status_code == 503 and len(client.calls) == 1
    req.path_params["target_port"] = "9999"
    response = await env["proxy"]("segment-person", "api/segment", req)
    assert response.status_code == 404 and len(client.calls) == 1
    req.path_params["target_port"] = "6106"
    client.fail = True
    response = await env["proxy"]("segment-person", "api/segment", req)
    assert response.status_code == 502 and response.headers["x-model-outcome"] == "unknown"
    assert state.workers[0]["inflight"] == 0


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--g6", required=True)
    parser.add_argument("--decision", required=True)
    args = parser.parse_args()
    test_decision(args.decision)
    asyncio.run(test_g6(args.g6))
    print("PASS: exact upstream replica, no cross-GPU retry/warmup, unknown outcome propagation")
