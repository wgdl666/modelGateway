"""Regional management client; bearer secrets remain in controller memory."""

import json
import urllib.request
import urllib.error


class GatewayError(RuntimeError):
    def __init__(self, status, body):
        self.status, self.body = status, body
        super().__init__(
            "Gateway returned HTTP "
            + str(status)
            + ": "
            + str(body.get("error", {}).get("code", "unknown"))
            + ": "
            + str(body.get("error", {}).get("message", ""))[:300]
        )


class Gateway:
    def __init__(self, url, token, ledger):
        self.url, self.token, self.ledger = url.rstrip("/"), token, ledger
        self.opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}), NoRedirect()
        )

    def call(self, method, path, body=None):
        headers = {
            "Authorization": "Bearer " + self.token,
            "Accept": "application/json",
        }
        if self.ledger.epoch is not None:
            headers.update(self.ledger.headers())
        data = None if body is None else json.dumps(body).encode()
        if data is not None:
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(
            self.url + path, data=data, headers=headers, method=method
        )
        try:
            with self.opener.open(request, timeout=30) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            try:
                body = json.load(e)
            except ValueError:
                body = {}
            raise GatewayError(e.code, body) from None

    def instances(self):
        return self.call("GET", "/v1/instances")

    def drain(self, id):
        return self.call("POST", "/v1/instances/" + id + "/drain", {"draining": True})

    def resume(self, id):
        return self.call("POST", "/v1/instances/" + id + "/drain", {"draining": False})

    def probe(self, item):
        keys = [
            "model_key",
            "protocol",
            "endpoint",
            "health_url",
            "gpu_devices",
            "deployment_id",
            "artifact_digest",
        ]
        return self.call(
            "POST", "/v1/instances/probe", {k: item[k] for k in keys if k in item}
        )


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None
