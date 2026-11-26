"""Tests for the stdlib-only ESP32 certificate pusher."""
import importlib.util
import json
import os
import types
import unittest
from unittest import mock

HERE = os.path.dirname(__file__)
SCRIPT = os.path.join(HERE, "push_esp32_cert.py")
spec = importlib.util.spec_from_file_location("push_esp32_cert", SCRIPT)
pusher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pusher)

TOKEN = "0123456789abcdef"
CERT = "-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n"
KEY = "-----BEGIN PRIVATE KEY-----\nAA==\n-----END PRIVATE KEY-----\n"


class FakeFiles:
    def __init__(self):
        self.tmp = None

    def __enter__(self):
        import tempfile
        self.tmp = tempfile.TemporaryDirectory()
        self.cert = os.path.join(self.tmp.name, "cert.pem")
        self.key = os.path.join(self.tmp.name, "key.pem")
        with open(self.cert, "w") as f:
            f.write(CERT)
        with open(self.key, "w") as f:
            f.write(KEY)
        return self

    def __exit__(self, *args):
        self.tmp.cleanup()


class SslContextTests(unittest.TestCase):
    class FakeContext:
        def __init__(self):
            self.check_hostname = True
            self.verify_mode = object()

    def test_insecure_disables_verification(self):
        fake = self.FakeContext()
        with mock.patch.object(pusher.ssl, "create_default_context", return_value=fake):
            context = pusher._ssl_context(types.SimpleNamespace(insecure=True, ca=""))
        self.assertFalse(context.check_hostname)
        self.assertEqual(context.verify_mode, pusher.ssl.CERT_NONE)

    def test_ca_is_passed_to_context(self):
        fake = self.FakeContext()
        with mock.patch.object(pusher.ssl, "create_default_context", return_value=fake) as build:
            pusher._ssl_context(types.SimpleNamespace(insecure=False, ca="/tmp/ca.pem"))
        build.assert_called_once_with(cafile="/tmp/ca.pem")

    def test_default_context(self):
        fake = self.FakeContext()
        with mock.patch.object(pusher.ssl, "create_default_context", return_value=fake) as build:
            pusher._ssl_context(types.SimpleNamespace(insecure=False, ca=""))
        build.assert_called_once_with()


class PusherTests(unittest.TestCase):
    def setUp(self):
        self.calls = []

    def fake_request(self, url, method, token, body, context, timeout):
        self.calls.append((url, method, token, body))
        if method == "POST":
            data = json.loads(body)
            self.assertEqual(data["certificate"], CERT)
            self.assertEqual(data["private_key"], KEY)
            return 200, json.dumps({"ok": True, "changed": True}).encode()
        return 200, json.dumps({"installed": True}).encode()

    def run_main(self, argv):
        with mock.patch.object(pusher, "_request", self.fake_request), \
                mock.patch.object(pusher, "_ssl_context", return_value=None):
            return pusher.main(argv)

    def test_endpoint_normalization(self):
        self.assertEqual(pusher._endpoint("http://x"), "http://x/api/tls")
        self.assertEqual(pusher._endpoint("https://x/"), "https://x/api/tls")
        self.assertEqual(pusher._endpoint("https://x/api/tls"), "https://x/api/tls")

    def test_push_builds_json_body(self):
        with FakeFiles() as files:
            rc = self.run_main([
                "--url", "http://esp32.local", "--cert", files.cert,
                "--key", files.key, "--token", TOKEN,
            ])
        self.assertEqual(rc, 0)
        self.assertEqual(self.calls[0][1], "POST")
        self.assertEqual(self.calls[0][0], "http://esp32.local/api/tls")

    def test_force_adds_force_to_payload(self):
        seen = {}

        def capture(url, method, token, body, context, timeout):
            seen.update(json.loads(body))
            return 200, json.dumps({"ok": True, "changed": True}).encode()

        with FakeFiles() as files, mock.patch.object(pusher, "_request", capture), \
                mock.patch.object(pusher, "_ssl_context", return_value=None):
            rc = pusher.main([
                "--url", "http://esp32.local", "--cert", files.cert,
                "--key", files.key, "--token", TOKEN, "--force",
            ])
        self.assertEqual(rc, 0)
        self.assertTrue(seen["force"])

    def test_status_uses_get(self):
        rc = self.run_main([
            "--url", "https://esp32.local", "--token", TOKEN, "--status",
        ])
        self.assertEqual(rc, 0)
        self.assertEqual(self.calls[0][1], "GET")

    def test_short_token_is_rejected_before_network(self):
        rc = self.run_main(["--url", "http://x", "--token", "short", "--status"])
        self.assertEqual(rc, 1)
        self.assertEqual(self.calls, [])

    def test_auth_error_is_not_retried(self):
        calls = {"n": 0}

        def unauthorized(*args):
            calls["n"] += 1
            return 401, json.dumps({"error": "unauthorized"}).encode()

        with mock.patch.object(pusher, "_request", unauthorized), \
                mock.patch.object(pusher, "_ssl_context", return_value=None):
            rc = pusher.main([
                "--url", "http://x", "--token", TOKEN, "--status",
                "--retries", "5", "--retry-delay", "0",
            ])
        self.assertEqual(rc, 2)
        self.assertEqual(calls["n"], 1)

    def test_server_error_is_retried(self):
        calls = {"n": 0}

        def flaky(*args):
            calls["n"] += 1
            if calls["n"] == 1:
                return 500, b"nope"
            return 200, json.dumps({"ok": True}).encode()

        with mock.patch.object(pusher, "_request", flaky), \
                mock.patch.object(pusher, "_ssl_context", return_value=None):
            rc = pusher.main([
                "--url", "http://x", "--token", TOKEN, "--status",
                "--retries", "2", "--retry-delay", "0",
            ])
        self.assertEqual(rc, 0)
        self.assertEqual(calls["n"], 2)

    def test_redirect_is_fatal(self):
        with mock.patch.object(pusher, "_request", return_value=(302, b"")), \
                mock.patch.object(pusher, "_ssl_context", return_value=None):
            rc = pusher.main([
                "--url", "http://x", "--token", TOKEN, "--status",
                "--retries", "5", "--retry-delay", "0",
            ])
        self.assertEqual(rc, 2)

    def test_redirect_handler_refuses_to_forward(self):
        handler = pusher._NoRedirect()
        self.assertIsNone(handler.redirect_request(None, None, 302, "", {}, "https://evil"))

    def test_uppercase_https_scheme_installs_context(self):
        captured = {}

        class FakeResponse:
            status = 200

            def read(self, _limit):
                return b"{}"

            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

        class FakeOpener:
            def open(self, request, timeout):
                return FakeResponse()

        context = object()
        with mock.patch.object(pusher.urllib.request, "build_opener",
                               side_effect=lambda *handlers: (captured.update(
                                   handlers=handlers) or FakeOpener())):
            pusher._request("HTTPS://esp32.local", "GET", TOKEN, None, context, 1)
        self.assertTrue(any(isinstance(h, pusher.urllib.request.HTTPSHandler)
                            for h in captured["handlers"]))

    def test_request_sets_auth_content_type_and_https_context(self):
        captured = {}

        class FakeResponse:
            status = 200

            def read(self, _limit):
                return b'{"ok": true}'

            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

        class FakeOpener:
            def open(self, request, timeout):
                captured["request"] = request
                captured["timeout"] = timeout
                return FakeResponse()

        context = object()
        with mock.patch.object(pusher.urllib.request, "build_opener",
                               return_value=FakeOpener()) as build:
            status, payload = pusher._request(
                "https://esp32.local", "POST", TOKEN, b'{"x":1}', context, 7)
        self.assertEqual(status, 200)
        self.assertEqual(payload, b'{"ok": true}')
        request = captured["request"]
        self.assertEqual(request.get_header("Authorization"), "Bearer " + TOKEN)
        self.assertEqual(request.get_header("Content-type"), "application/json")
        self.assertEqual(captured["timeout"], 7)
        handlers = build.call_args.args
        self.assertTrue(any(isinstance(h, pusher._NoRedirect) for h in handlers))
        self.assertTrue(any(isinstance(h, pusher.urllib.request.HTTPSHandler)
                            and h._context is context for h in handlers))


if __name__ == "__main__":
    unittest.main()
