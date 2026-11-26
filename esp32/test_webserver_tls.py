"""HTTP-layer tests for /api/tls using CPython stubs for MicroPython modules."""
import asyncio
import json
import sys
import time
import types
import unittest
from unittest import mock

# webserver/logbuf only need MicroPython-specific modules at import time.
time.ticks_ms = lambda: int(time.time() * 1000)
time.ticks_diff = lambda a, b: a - b

ujson = types.ModuleType("ujson")
ujson.dumps = json.dumps
sys.modules.setdefault("ujson", ujson)

ble = types.ModuleType("marstek_ble")
ble.connected = False
ble.state = "idle"
ble.negotiated_mtu = 0
ble.last_error = ""


async def _no_ble(*_args, **_kwargs):
    return None


ble.send_command = _no_ble
ble.send_command_multi = _no_ble
ble.send_ff06_reset = _no_ble
sys.modules.setdefault("marstek_ble", ble)

import tls_store
import webserver

TOKEN = "0123456789abcdef-token"


class FakeRequest:
    def __init__(self, app=None, token=None, body=None, content_type="application/json",
                 host="esp32.local"):
        self.app = app or webserver.app
        self.headers = {"Host": host}
        if token is not None:
            self.headers["Authorization"] = "Bearer " + token
        self.client_addr = ("192.0.2.10", 12345)
        self.scheme = "http"
        self.content_type = content_type
        self.json = body
        self.body = json.dumps(body).encode() if body is not None else b""
        self.args = {}


def run(coro):
    return asyncio.run(coro)


class RouteTests(unittest.TestCase):
    def setUp(self):
        webserver.configure_request_limits(False)
        tls_store.init({"tls_enabled": True, "tls_push_token": TOKEN})

    def tearDown(self):
        tls_store.init({})
        webserver.configure_request_limits(False)

    def test_disabled_returns_404(self):
        tls_store.init({})
        result = run(webserver.api_tls_status(FakeRequest()))
        self.assertEqual(result[1], 404)

    def test_missing_token_returns_401_with_challenge(self):
        result = run(webserver.api_tls_status(FakeRequest()))
        self.assertEqual(result[1], 401)
        self.assertEqual(result[2]["WWW-Authenticate"], 'Bearer realm="esp32-tls"')

    def test_wrong_token_returns_401(self):
        result = run(webserver.api_tls_status(FakeRequest(token="wrong")))
        self.assertEqual(result[1], 401)

    def test_authorized_status_reports_listener_state(self):
        tls_store.set_tls_active(True)
        with mock.patch.object(tls_store, "installed_info",
                               return_value={"installed": True, "valid": True}):
            result = run(webserver.api_tls_status(FakeRequest(token=TOKEN)))
        self.assertTrue(result["active"])
        self.assertTrue(result["enabled"])

    def test_admin_listener_refuses_token_over_http_when_tls_active(self):
        tls_store.set_tls_active(True)
        tls_store.set_pair_healthy(True)
        request = FakeRequest(app=webserver.admin_app, token=TOKEN)
        result = run(webserver.api_tls_status(request))
        self.assertEqual(result[1], 403)
        self.assertEqual(result[0]["code"], "https_required")

    def test_admin_listener_refuses_while_pair_healthy(self):
        tls_store.set_tls_active(False)
        tls_store.set_pair_healthy(True)
        result = run(webserver.api_tls_status(
            FakeRequest(app=webserver.admin_app, token=TOKEN)))
        self.assertEqual(result[1], 403)

    def test_admin_listener_allows_push_while_tls_inactive(self):
        tls_store.set_tls_active(False)
        with mock.patch.object(tls_store, "installed_info",
                               return_value={"installed": True, "valid": False}):
            result = run(webserver.api_tls_status(
                FakeRequest(app=webserver.admin_app, token=TOKEN)))
        self.assertFalse(result["active"])

    def test_push_changed_sets_reboot_and_returns_fingerprint(self):
        tls_store.set_tls_active(False)
        with mock.patch.object(tls_store, "save", return_value={
                "changed": True, "fingerprint_sha256": "abc123", "info": {}}), \
                mock.patch.object(tls_store, "request_reboot") as reboot:
            result = run(webserver.api_tls_push(FakeRequest(
                token=TOKEN, body={"certificate": "c", "private_key": "k"})))
        self.assertTrue(result["changed"])
        self.assertEqual(result["reboot_in_s"], tls_store.REBOOT_DELAY_S)
        reboot.assert_called_once()

    def test_push_passes_force_flag_to_storage(self):
        with mock.patch.object(tls_store, "save", return_value={
                "changed": False, "fingerprint_sha256": "abc123", "info": {}}) as save:
            run(webserver.api_tls_push(FakeRequest(
                token=TOKEN,
                body={"certificate": "c", "private_key": "k", "force": True})))
        self.assertTrue(save.call_args.kwargs["force"])

    def test_push_unchanged_does_not_reboot(self):
        with mock.patch.object(tls_store, "save", return_value={
                "changed": False, "fingerprint_sha256": "abc123", "info": {}}), \
                mock.patch.object(tls_store, "request_reboot") as reboot:
            result = run(webserver.api_tls_push(FakeRequest(
                token=TOKEN, body={"certificate": "c", "private_key": "k"})))
        self.assertFalse(result["changed"])
        reboot.assert_not_called()

    def test_push_requires_json_content_type(self):
        result = run(webserver.api_tls_push(FakeRequest(
            token=TOKEN, content_type="text/plain",
            body={"certificate": "c", "private_key": "k"})))
        self.assertEqual(result[1], 415)

    def test_push_requires_both_fields(self):
        result = run(webserver.api_tls_push(FakeRequest(
            token=TOKEN, body={"certificate": "c"})))
        self.assertEqual(result[1], 400)

    def test_push_rejects_non_object_body(self):
        result = run(webserver.api_tls_push(FakeRequest(token=TOKEN, body=[1])))
        self.assertEqual(result[1], 400)

    def test_rate_limited_auth_maps_to_429(self):
        with mock.patch.object(tls_store, "auth_status", return_value="rate_limited"):
            result = run(webserver.api_tls_status(FakeRequest(token=TOKEN)))
        self.assertEqual(result[1], 429)

    def test_push_maps_validation_error_to_400(self):
        with mock.patch.object(tls_store, "save", side_effect=ValueError("invalid key")):
            result = run(webserver.api_tls_push(FakeRequest(
                token=TOKEN, body={"certificate": "c", "private_key": "k"})))
        self.assertEqual(result[1], 400)

    def test_admin_root_redirects_only_for_matching_certificate_cn(self):
        tls_store.set_tls_active(True)
        with mock.patch.object(tls_store, "common_name", return_value="esp32.local"):
            response = run(webserver.admin_root(FakeRequest(app=webserver.admin_app)))
        self.assertEqual(response.status_code, 308)
        self.assertEqual(response.headers["Location"], "https://esp32.local/")

    def test_admin_root_does_not_reflect_foreign_host(self):
        tls_store.set_tls_active(True)
        with mock.patch.object(tls_store, "common_name", return_value="esp32.local"):
            response = run(webserver.admin_root(
                FakeRequest(app=webserver.admin_app, host="evil.example")))
        self.assertEqual(response.status_code, 200)
        self.assertNotIn("Location", response.headers)

    def test_admin_api_redirects_to_https_for_certificate_cn(self):
        tls_store.set_tls_active(True)
        with mock.patch.object(tls_store, "common_name", return_value="esp32.local"):
            response = run(webserver.admin_api_redirect(
                FakeRequest(app=webserver.admin_app, host="esp32.local:80"), "status"))
        self.assertEqual(response.status_code, 308)
        self.assertEqual(response.headers["Location"], "https://esp32.local/api/status")

    def test_admin_api_does_not_reflect_foreign_host(self):
        tls_store.set_tls_active(True)
        with mock.patch.object(tls_store, "common_name", return_value="esp32.local"):
            result = run(webserver.admin_api_redirect(
                FakeRequest(app=webserver.admin_app, host="evil.example"), "status"))
        self.assertEqual(result[1], 403)

    def test_admin_api_404_when_tls_inactive(self):
        tls_store.set_tls_active(False)
        result = run(webserver.admin_api_redirect(
            FakeRequest(app=webserver.admin_app), "status"))
        self.assertEqual(result[1], 404)

    def test_admin_root_reports_inactive_tls(self):
        tls_store.set_tls_active(False)
        response = run(webserver.admin_root(FakeRequest(app=webserver.admin_app)))
        self.assertEqual(response.status_code, 503)

    def test_request_limits_follow_opt_in(self):
        webserver.configure_request_limits(False)
        self.assertEqual(webserver.Request.max_content_length, 16 * 1024)
        webserver.configure_request_limits(True)
        self.assertEqual(webserver.Request.max_content_length, 24 * 1024)

    def test_invalid_host_rejected(self):
        self.assertFalse(webserver._valid_host("bad host"))
        self.assertTrue(webserver._valid_host("esp32.local"))


if __name__ == "__main__":
    unittest.main()
