"""CPython tests for the ESP32 listener/startup decision in main.py."""
import asyncio
import json
import sys
import time
import types
import unittest
from unittest import mock

if not hasattr(time, "ticks_ms"):
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


async def _noop(*_args, **_kwargs):
    return None


ble.send_command = _noop
ble.send_command_multi = _noop
ble.send_ff06_reset = _noop
ble.ble_loop = _noop
sys.modules.setdefault("marstek_ble", ble)

machine = types.ModuleType("machine")
machine.WDT = lambda timeout=0: types.SimpleNamespace(feed=lambda: None)
machine.reset = lambda: None
sys.modules.setdefault("machine", machine)

network = types.ModuleType("network")
network.STA_IF = 0
network.WLAN = lambda *_args: types.SimpleNamespace(
    active=lambda _v=None: True,
    isconnected=lambda: False,
    ifconfig=lambda: ("0.0.0.0",),
    status=lambda: 0,
    config=lambda **_kwargs: None,
)
network.STAT_CONNECTING = 1001
sys.modules.setdefault("network", network)

import tls_store
import webserver
import main

TOKEN = "0123456789abcdef-token"


class ListenerPlanTests(unittest.TestCase):
    def setUp(self):
        tls_store.init({"tls_enabled": True, "tls_push_token": TOKEN})
        main.wifi_ip = "192.0.2.10"
        self.calls = []

    def tearDown(self):
        main.wifi_ip = None
        tls_store.init({})

    def _fake_start(self, **kwargs):
        async def _run():
            self.calls.append(kwargs)
            await asyncio.sleep(3600)
        return _run()

    def _run_mode(self, installed, context, probe):
        with mock.patch.object(webserver.app, "start_server", self._fake_start), \
                mock.patch.object(webserver.admin_app, "start_server", self._fake_start), \
                mock.patch.object(tls_store, "installed_info",
                                  return_value={"installed": installed}), \
                mock.patch.object(tls_store, "server_context", return_value=context), \
                mock.patch.object(main, "_tls_version_ok", return_value=True), \
                mock.patch.object(main, "_tls_loopback_self_test",
                                  new=mock.AsyncMock(return_value=probe)):
            try:
                asyncio.run(asyncio.wait_for(main.http_task(), 0.15))
            except asyncio.TimeoutError:
                pass

    def test_admin_mode_starts_only_admin_app_on_80(self):
        self._run_mode(installed=True, context=None, probe=False)
        self.assertEqual(len(self.calls), 1)
        self.assertEqual(self.calls[0]["port"], 80)
        self.assertIsNone(self.calls[0].get("ssl"))

    def test_tls_mode_starts_443_and_admin_80(self):
        self._run_mode(installed=True, context=object(), probe=True)
        ports = sorted(call["port"] for call in self.calls)
        self.assertEqual(ports, [80, 443])
        by_port = {call["port"]: call for call in self.calls}
        self.assertIsNotNone(by_port[443]["ssl"])

    def test_failed_self_test_keeps_443_closed(self):
        self._run_mode(installed=True, context=object(), probe=False)
        self.assertEqual([call["port"] for call in self.calls], [80])
        self.assertIsNone(self.calls[0].get("ssl"))

    def test_bootstrap_mode_starts_full_app_on_80(self):
        self._run_mode(installed=False, context=None, probe=False)
        self.assertEqual([call["port"] for call in self.calls], [80])

    def test_plain_mode_starts_full_app_on_80(self):
        tls_store.init({})
        with mock.patch.object(webserver.app, "start_server", self._fake_start), \
                mock.patch.object(webserver.admin_app, "start_server", self._fake_start), \
                mock.patch.object(main, "_tls_version_ok", return_value=True):
            try:
                asyncio.run(asyncio.wait_for(main.http_task(), 0.15))
            except asyncio.TimeoutError:
                pass
        self.assertEqual([call["port"] for call in self.calls], [80])

    def test_version_gate(self):
        with mock.patch.object(main.sys, "implementation",
                               types.SimpleNamespace(version=(1, 24, 1))):
            self.assertTrue(main._tls_version_ok())
        with mock.patch.object(main.sys, "implementation",
                               types.SimpleNamespace(version=(1, 23, 0))):
            self.assertFalse(main._tls_version_ok())
        with mock.patch.object(main.sys, "implementation",
                               types.SimpleNamespace(version=())):
            self.assertFalse(main._tls_version_ok())


class FakeServer:
    def close(self):
        pass

    async def wait_closed(self):
        pass


class FakeReader:
    async def read(self, _n):
        return b"x"


class FakeWriter:
    def write(self, _data):
        pass

    async def drain(self):
        pass

    async def aclose(self):
        pass

    def close(self):
        pass


class SelfTestTests(unittest.TestCase):
    def run_self_test(self, start, connect):
        with mock.patch.object(main.asyncio, "start_server", start), \
                mock.patch.object(main.asyncio, "open_connection", connect):
            return asyncio.run(main._tls_loopback_self_test(object()))

    def test_successful_loopback_handshake(self):
        self.assertTrue(self.run_self_test(
            mock.AsyncMock(return_value=FakeServer()),
            mock.AsyncMock(return_value=(FakeReader(), FakeWriter()))))

    def test_ssl_keyword_rejected(self):
        self.assertFalse(self.run_self_test(
            mock.AsyncMock(side_effect=TypeError("no ssl")),
            mock.AsyncMock()))

    def test_all_loopback_ports_busy(self):
        self.assertFalse(self.run_self_test(
            mock.AsyncMock(side_effect=OSError("busy")),
            mock.AsyncMock()))

    def test_handshake_failure(self):
        self.assertFalse(self.run_self_test(
            mock.AsyncMock(return_value=FakeServer()),
            mock.AsyncMock(side_effect=OSError("handshake failed"))))


class LifecycleTests(unittest.TestCase):
    def test_apply_tls_task_reboots_after_delay(self):
        async def scenario():
            with mock.patch.object(tls_store, "reboot_pending", return_value=True), \
                    mock.patch.object(main, "_sleep_ms", mock.AsyncMock()), \
                    mock.patch.object(main.asyncio, "sleep", mock.AsyncMock()), \
                    mock.patch.object(main.machine, "reset",
                                      side_effect=RuntimeError("reset")) as reset:
                with self.assertRaises(RuntimeError):
                    await main.apply_tls_task()
            reset.assert_called_once()

        asyncio.run(scenario())

    def test_retry_task_promotes_healthy_pair(self):
        async def scenario():
            with mock.patch.object(main, "TLS_RETRY_INTERVAL_S", 0), \
                    mock.patch.object(tls_store, "reset_context"), \
                    mock.patch.object(tls_store, "server_context", return_value=object()), \
                    mock.patch.object(main, "_tls_loopback_self_test",
                                      mock.AsyncMock(return_value=True)), \
                    mock.patch.object(main.asyncio, "sleep", mock.AsyncMock()), \
                    mock.patch.object(tls_store, "set_pair_healthy") as healthy, \
                    mock.patch.object(main.machine, "reset",
                                      side_effect=RuntimeError("reset")) as reset:
                with self.assertRaises(RuntimeError):
                    await main._tls_retry_task()
            healthy.assert_called_once_with(True)
            reset.assert_called_once()

        asyncio.run(scenario())

    def test_listener_loop_marks_state_and_shuts_down_on_cancel(self):
        class FakeApp:
            def __init__(self):
                self.server = None
                self.shutdown_called = False

            def shutdown(self):
                self.shutdown_called = True

            async def start_server(self, **_kwargs):
                self.server = object()
                await asyncio.sleep(10)

        async def scenario():
            app = FakeApp()
            states = []

            def on_state(up):
                states.append(up)

            task = asyncio.create_task(
                main._listener_loop(app, 443, None, "test", on_state=on_state))
            await asyncio.sleep(0.2)
            self.assertEqual(states, [True])
            task.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await task
            self.assertEqual(states, [True, False])
            self.assertTrue(app.shutdown_called)
            self.assertIsNone(app.server)

        asyncio.run(scenario())


if __name__ == "__main__":
    unittest.main()
