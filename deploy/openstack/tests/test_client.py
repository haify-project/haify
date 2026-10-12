import io
import json
import os
import sys
import unittest
import urllib.error

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from haify_cinder.client import Client, HaifyError, Unreachable, endpoints  # noqa: E402


class Response(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


def opener(script):
    """Answers each request from script: url prefix -> body dict or exception."""
    seen = []

    def open_(req, timeout=None):
        seen.append((req.get_method(), req.full_url, req.data and json.loads(req.data), timeout,
                     req.get_header("Authorization")))
        for prefix, answer in script.items():
            if req.full_url.startswith(prefix):
                if isinstance(answer, Exception):
                    raise answer
                return Response(json.dumps(answer).encode())
        raise urllib.error.URLError("connection refused")

    return open_, seen


class Endpoints(unittest.TestCase):
    def test_forms(self):
        self.assertEqual(endpoints("a, b:4000 ,https://c/,http://[fd00::1]:3375"),
                         ["http://a:3375", "http://b:4000", "https://c:3375", "http://[fd00::1]:3375"])
        self.assertEqual(endpoints(["", "x"]), ["http://x:3375"])


class Proxy(unittest.TestCase):
    def test_controller_is_reached_directly(self):
        import http.server
        import threading

        seen = []

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                seen.append(self.path)
                body = b'{"success": true, "pools": []}'
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *a):
                pass

        srv = http.server.HTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        old = {k: os.environ.get(k) for k in ("http_proxy", "no_proxy", "HTTP_PROXY", "NO_PROXY")}
        # A proxy that does not exist: going through it would fail.
        os.environ.update(http_proxy="http://127.0.0.1:9", HTTP_PROXY="http://127.0.0.1:9",
                          no_proxy="10.0.0.0/8", NO_PROXY="10.0.0.0/8")
        try:
            self.assertEqual(Client(f"127.0.0.1:{srv.server_port}").pools(), [])
        finally:
            for k, v in old.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v
            srv.shutdown()
        self.assertEqual(seen, ["/v1/pools"])


class Request(unittest.TestCase):
    def test_next_address_when_one_refuses(self):
        op, seen = opener({"http://b:3375": {"success": True, "resources": [{"name": "r"}]}})
        c = Client("a,b", token="t", opener=op)
        self.assertEqual(c.resources(), [{"name": "r"}])
        self.assertEqual([s[1] for s in seen], ["http://a:3375/v1/resources", "http://b:3375/v1/resources"])
        self.assertEqual(seen[-1][4], "Bearer t")

    def test_all_down(self):
        op, _ = opener({})
        with self.assertRaisesRegex(Unreachable, "a:3375"):
            Client("a", opener=op).pools()

    def test_success_false_is_an_error(self):
        op, _ = opener({"http://a": {"message": "pool full"}})
        with self.assertRaisesRegex(HaifyError, "pool full"):
            Client("a", opener=op).create_resource({"name": "x"})

    def test_http_error_message(self):
        err = urllib.error.HTTPError("http://a", 404, "Not Found", {},
                                     io.BytesIO(b'{"code":5,"message":"resource x not found"}'))
        op, _ = opener({"http://a": err})
        with self.assertRaisesRegex(HaifyError, "resource x not found") as cm:
            Client("a", opener=op).resource("x")
        self.assertNotIsInstance(cm.exception, Unreachable, "an answer is not an outage")

    def test_paths_are_escaped_and_timeouts_passed(self):
        op, seen = opener({"http://a": {"success": True}})
        c = Client("a", opener=op)
        c.populate("r", "src", "s1", "n1", timeout=3600)
        self.assertEqual(seen[0][:4], ("POST", "http://a:3375/v1/resources/r/populate",
                                       {"resource": "r", "volumeId": 0, "node": "n1",
                                        "sourceSnapshot": {"resource": "src", "volumeId": 0, "name": "s1",
                                                           "replicated": True}}, 3600))
        c.detach_client("r", "a/b")
        self.assertEqual(seen[1][1], "http://a:3375/v1/resources/r/diskless-clients/a%2Fb")

    def test_find_resource(self):
        op, seen = opener({"http://a:3375/v1/resources/r": {"success": True, "resource": {"name": "r"}},
                           "http://a:3375/v1/resources/nope": {"success": False,
                                                               "message": "resource not found: nope"}})
        c = Client("a", opener=op)
        self.assertEqual(c.find_resource("r"), {"name": "r"})
        self.assertIsNone(c.find_resource("nope"))
        self.assertEqual([s[1] for s in seen], ["http://a:3375/v1/resources/r", "http://a:3375/v1/resources/nope"],
                         "one lookup each, never the whole list")

    def test_find_resource_other_errors_raise(self):
        op, _ = opener({"http://a": {"success": False, "message": "database not available"}})
        with self.assertRaisesRegex(HaifyError, "database not available"):
            Client("a", opener=op).find_resource("r")


if __name__ == "__main__":
    unittest.main()
