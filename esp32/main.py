"""ESP32-S3 bridge: resilient WiFi, BLE loop, HTTP server.

Lifecycle design: the top-level coroutine (`main`) never returns. Each
subsystem (WiFi, BLE, HTTP) runs inside `supervise()`, which restarts it
with backoff if it ever raises or exits — a subsystem can never die
permanently. A `supervisor()` task feeds the hardware watchdog only when
every subsystem has posted a recent heartbeat; if one wedges, the WDT
starves and the board is hard-reset. `run_forever()` never re-enters
`asyncio.run()` on failure (that used to leak the previous run's tasks —
see git history) — the only recovery path from a truly fatal error is a
full `machine.reset()`, which guarantees a clean slate for the WiFi/BLE
radio drivers.
"""
import asyncio
import gc
import json
import machine
import network
import sys
import time

import logbuf
import marstek_ble as ble
import tls_store
import webserver

DEFAULT_HOSTNAME = "esp32-marstek-watchdog"
WIFI_CHECK_INTERVAL_S = 15
WIFI_RETRY_DELAY_S = 5
WIFI_ATTEMPT_TIMEOUT_S = 45
WIFI_DRIVER_RESET_MS = 500

SUBSYSTEM_RESTART_DELAY_S = 5
SUPERVISOR_INTERVAL_S = 5
WDT_TIMEOUT_MS = 120000
HTTP_HEARTBEAT_INTERVAL_S = 10
TLS_REBOOT_POLL_MS = 500
TLS_MIN_VERSION = (1, 24, 0)
TLS_RETRY_INTERVAL_S = 300

# How long a subsystem may go without a heartbeat before it's considered
# wedged and the watchdog stops being fed. A subsystem that hasn't posted its
# *first* heartbeat yet (e.g. HTTP waiting on WiFi) is never "stale" — only
# started-but-stuck subsystems should block feeding. Must stay below
# WDT_TIMEOUT_MS so a real wedge is guaranteed to trip the hardware reset.
HEARTBEAT_STALE_S = {
    "wifi": 90,
    "ble": 100,
    "http": 45,
}

_WLAN_STATUS = {
    1000: "idle",
    1001: "connecting",
    1010: "got IP",
    201: "no AP found",
    202: "auth failed",
}
# network.WLAN.status() values that will never resolve on their own —
# fail fast and reset the driver instead of waiting out the full timeout.
_WLAN_TERMINAL_FAIL = (201, 202)

_heartbeats = {}
wifi_ip = None


def load_config() -> dict:
    with open("config.json") as f:
        return json.load(f)


async def _sleep_ms(ms: int) -> None:
    try:
        await asyncio.sleep_ms(ms)
    except AttributeError:  # CPython / unittest
        await asyncio.sleep(ms / 1000)


def _wifi_status_text(status: int) -> str:
    return _WLAN_STATUS.get(status, str(status))


def beat(name: str) -> None:
    _heartbeats[name] = time.ticks_ms()


def _is_stale(name: str, limit_s: int, now: int) -> bool:
    last = _heartbeats.get(name)
    if last is None:
        return False
    return time.ticks_diff(now, last) > limit_s * 1000


def _ensure_wlan() -> network.WLAN:
    wlan = network.WLAN(network.STA_IF)
    wlan.active(True)
    try:
        # Keep WiFi responsive while BLE is scanning/connecting.
        wlan.config(pm=network.WLAN.PM_NONE)
    except Exception:
        pass
    return wlan


def _set_hostname(wlan: network.WLAN, hostname: str) -> None:
    try:
        wlan.config(hostname=hostname)
    except Exception as exc:
        logbuf.log("WiFi", "hostname not set: " + str(exc))


def _has_ip(wlan: network.WLAN) -> bool:
    return wlan.isconnected() and wlan.ifconfig()[0] != "0.0.0.0"


async def _reset_wifi_driver(wlan: network.WLAN) -> network.WLAN:
    """Power-cycle the WiFi driver instead of a soft disconnect. A wedged
    STA interface (stuck STAT_CONNECTING, internal driver error) often needs
    a full active(False)/active(True) cycle to recover."""
    logbuf.log("WiFi", "driver reset")
    try:
        wlan.disconnect()
    except OSError:
        pass
    try:
        wlan.active(False)
    except Exception:
        pass
    await _sleep_ms(WIFI_DRIVER_RESET_MS)
    return _ensure_wlan()


async def try_connect_wifi(
    ssid: str,
    password: str,
    hostname: str,
    wait_s: int = WIFI_ATTEMPT_TIMEOUT_S,
) -> str | None:
    wlan = _ensure_wlan()
    _set_hostname(wlan, hostname)

    if _has_ip(wlan):
        return wlan.ifconfig()[0]

    logbuf.log("WiFi", "connecting to " + ssid)
    if wlan.status() != network.STAT_CONNECTING:
        wlan.connect(ssid, password)

    for tick in range(wait_s):
        beat("wifi")
        if _has_ip(wlan):
            ip = wlan.ifconfig()[0]
            logbuf.log("WiFi", "connected, IP = " + ip)
            return ip
        status = wlan.status()
        if status in _WLAN_TERMINAL_FAIL:
            logbuf.log("WiFi", "terminal failure, aborting attempt: " + _wifi_status_text(status))
            return None
        if tick and tick % 10 == 0:
            logbuf.log("WiFi", "waiting, status: " + _wifi_status_text(status))
        await asyncio.sleep(1)

    return None


async def connect_wifi(ssid: str, password: str, hostname: str) -> str:
    attempt = 0
    wlan = _ensure_wlan()
    while True:
        attempt += 1
        beat("wifi")
        logbuf.log("WiFi", "connect attempt " + str(attempt))
        ip = await try_connect_wifi(ssid, password, hostname)
        if ip:
            return ip

        logbuf.log("WiFi", "failed, status: " + _wifi_status_text(wlan.status()))
        wlan = await _reset_wifi_driver(wlan)
        await asyncio.sleep(WIFI_RETRY_DELAY_S)


async def wifi_task(ssid: str, password: str, hostname: str) -> None:
    """Owns the WiFi link for the process lifetime: initial connect, then
    monitors and repairs the link. A single instance ever runs (supervised)."""
    global wifi_ip

    ip = await connect_wifi(ssid, password, hostname)
    wifi_ip = ip
    beat("wifi")
    logbuf.log("WiFi", "IP = " + ip)

    while True:
        await asyncio.sleep(WIFI_CHECK_INTERVAL_S)
        beat("wifi")
        wlan = _ensure_wlan()
        if _has_ip(wlan):
            wifi_ip = wlan.ifconfig()[0]
            continue

        logbuf.log("WiFi", "link lost, reconnecting")
        wifi_ip = None
        ip = await connect_wifi(ssid, password, hostname)
        wifi_ip = ip
        beat("wifi")
        logbuf.log("WiFi", "restored, IP = " + ip)


async def _stop_servers() -> None:
    """Close anything left behind by a previous supervised HTTP incarnation."""
    for app in (webserver.app, webserver.admin_app):
        server = getattr(app, "server", None)
        if server is None:
            continue
        try:
            app.shutdown()
        except Exception:
            pass
        try:
            await asyncio.wait_for(server.wait_closed(), 2)
        except BaseException:
            pass


def _tls_version_ok() -> bool:
    """MicroPython < 1.24 lacks ssl support in asyncio.start_server, and
    microdot silently falls back to plaintext when the kwarg is rejected."""
    try:
        version = sys.implementation.version
        return (int(version[0]), int(version[1])) >= (TLS_MIN_VERSION[0], TLS_MIN_VERSION[1])
    except Exception:
        return False


async def _tls_loopback_self_test(ctx) -> bool:
    """Prove asyncio accepts ssl= and that the cert/key pair really works.

    microdot retries without ssl if the kwarg raises TypeError, so a failed
    probe must prevent the 443 listener rather than silently serve plaintext.
    The loopback handshake also catches a parseable but mismatched key pair,
    which mbedTLS accepts at load_cert_chain time.
    """
    async def _server(reader, writer):
        try:
            await reader.read(1)
            writer.write(b"x")
            await writer.drain()
        except Exception:
            pass
        finally:
            try:
                await writer.aclose()
            except Exception:
                try:
                    writer.close()
                except Exception:
                    pass

    server = None
    port = None
    try:
        for candidate in (18443, 18444, 18445):
            try:
                server = await asyncio.start_server(
                    _server, "127.0.0.1", candidate, ssl=ctx)
                port = candidate
                break
            except OSError:
                server = None
        if server is None or port is None:
            logbuf.log("HTTP", "TLS self-test could not bind loopback")
            return False

        import ssl
        try:
            protocol = ssl.PROTOCOL_TLS_CLIENT
        except AttributeError:
            protocol = 0  # MBEDTLS_SSL_IS_CLIENT
        client = ssl.SSLContext(protocol)
        try:
            client.check_hostname = False
        except Exception:
            pass
        try:
            client.verify_mode = ssl.CERT_NONE
        except Exception:
            pass

        last_error = None
        for _attempt in range(2):
            writer = None
            try:
                reader, writer = await asyncio.wait_for(
                    asyncio.open_connection("127.0.0.1", port, ssl=client), 5)
                writer.write(b"x")
                await writer.drain()
                reply = await asyncio.wait_for(reader.read(1), 5)
                if reply == b"x":
                    return True
                last_error = "unexpected loopback reply"
            except Exception as exc:
                last_error = str(exc)
            finally:
                if writer is not None:
                    try:
                        await writer.aclose()
                    except Exception:
                        try:
                            writer.close()
                        except Exception:
                            pass
            await _sleep_ms(100)
        logbuf.log("HTTP", "TLS self-test handshake failed: " + str(last_error))
        return False
    except TypeError:
        logbuf.log("HTTP", "TLS self-test: asyncio rejected ssl=")
        return False
    except Exception as exc:
        logbuf.log("HTTP", "TLS self-test failed: " + str(exc))
        return False
    finally:
        if server is not None:
            try:
                server.close()
                await server.wait_closed()
            except Exception:
                pass


async def _tls_retry_task() -> None:
    """In admin-only mode, retry a transient context/self-test failure forever.

    The backoff caps at TLS_RETRY_INTERVAL_S. A permanent failure (bad pair or
    unsupported TLS) simply keeps the device in admin-only mode without a
    reboot loop; the operator can push over :80 or use USB.
    """
    delay = 60
    while True:
        await asyncio.sleep(delay)
        delay = min(delay * 2, TLS_RETRY_INTERVAL_S)
        tls_store.reset_context()
        ctx = tls_store.server_context()
        if ctx is None:
            continue
        try:
            ok = await asyncio.wait_for(_tls_loopback_self_test(ctx), 10)
        except Exception:
            ok = False
        if ok:
            tls_store.set_pair_healthy(True)
            logbuf.log("TLS", "TLS recovered; rebooting")
            await asyncio.sleep(tls_store.REBOOT_DELAY_S)
            machine.reset()
            return


async def _listener_loop(app, port: int, ssl_ctx, name: str, on_state=None) -> None:
    """Own one listener and retry it without taking the other one down."""
    while True:
        app.server = None
        task = asyncio.create_task(
            app.start_server(host="0.0.0.0", port=port, ssl=ssl_ctx, debug=False))
        try:
            while not task.done() and getattr(app, "server", None) is None:
                await _sleep_ms(50)
            if task.done():
                try:
                    await task
                except asyncio.CancelledError:
                    raise
                except Exception as exc:
                    logbuf.log("HTTP", name + " failed: " + str(exc))
            else:
                if on_state is not None:
                    on_state(True)
                logbuf.log("HTTP", name + " listening on :" + str(port))
                try:
                    await task
                except asyncio.CancelledError:
                    raise
                except Exception as exc:
                    logbuf.log("HTTP", name + " crashed: " + str(exc))
        finally:
            if getattr(app, "server", None) is not None:
                try:
                    app.shutdown()
                except Exception:
                    pass
            if not task.done():
                task.cancel()
                try:
                    await task
                except BaseException:
                    pass
            if on_state is not None:
                on_state(False)
            app.server = None
        await asyncio.sleep(SUBSYSTEM_RESTART_DELAY_S)


async def http_task() -> None:
    """Starts the HTTP(S) server(s) once WiFi has an IP and supervises them as
    subtasks so this coroutine can post heartbeats independently of whatever
    microdot's accept loop is doing internally.

    With a certificate installed the main app moves to :443 and :80 keeps only
    the certificate-management routes, so a renewal can still replace an
    expired certificate. Without one, everything is served over plain HTTP on
    :80 (including the provisioning endpoint)."""
    while wifi_ip is None:
        await _sleep_ms(200)

    beat("http")
    await _stop_servers()

    enabled = tls_store.tls_enabled()
    installed = tls_store.installed_info().get("installed", False) if enabled else False
    version_ok = _tls_version_ok() if enabled else False
    tls_ctx = None
    probe_ok = False

    if enabled and installed:
        tls_ctx = tls_store.server_context()
        if tls_ctx is not None:
            try:
                probe_ok = await asyncio.wait_for(
                    _tls_loopback_self_test(tls_ctx), 10)
            except Exception:
                probe_ok = False

    mode = tls_store.startup_mode(
        enabled, installed, tls_ctx is not None, version_ok, probe_ok)
    tls_store.set_tls_active(False)
    tls_store.set_pair_healthy(probe_ok and tls_ctx is not None)

    server_tasks = []
    if mode == "tls":
        logbuf.log("HTTP", "TLS active: HTTPS on :443, certificate admin on :80")
        main_failures = [0]

        def _main_state(up):
            tls_store.set_tls_active(up)
            if up:
                main_failures[0] = 0
                tls_store.set_pair_healthy(True)
            else:
                main_failures[0] += 1
                if main_failures[0] >= 3:
                    # Persistent bind/runtime failure: keep the admin listener
                    # stable and allow token-authenticated HTTP recovery.
                    tls_store.set_pair_healthy(False)
                    logbuf.log("TLS", "persistent :443 failure; HTTP recovery enabled")

        server_tasks.append(asyncio.create_task(
            _listener_loop(webserver.app, 443, tls_ctx, "http-main",
                           on_state=_main_state)))
        server_tasks.append(asyncio.create_task(
            _listener_loop(webserver.admin_app, 80, None, "http-admin")))
    elif mode == "admin":
        err = tls_store.context_error()
        if err:
            logbuf.log("HTTP", "TLS unavailable (" + err + "); certificate admin on :80")
        elif not version_ok:
            logbuf.log("HTTP", "TLS requires MicroPython >= 1.24; certificate admin on :80")
        else:
            logbuf.log("HTTP", "TLS self-test failed; certificate admin on :80")
        server_tasks.append(asyncio.create_task(
            _listener_loop(webserver.admin_app, 80, None, "http-admin")))
        server_tasks.append(asyncio.create_task(
            supervise(_tls_retry_task, "tls-retry")))
    else:
        if mode == "bootstrap":
            logbuf.log("HTTP", "TLS enabled, no certificate yet; HTTP on :80")
        else:
            logbuf.log("HTTP", "TLS disabled in config, serving HTTP on :80")
        server_tasks.append(asyncio.create_task(
            _listener_loop(webserver.app, 80, None, "http-main")))

    logbuf.log("HTTP", "starting listeners")
    beat("http")
    try:
        while True:
            await asyncio.sleep(HTTP_HEARTBEAT_INTERVAL_S)
            if mode == "tls":
                # Either listener being up keeps the board stable; persistent
                # :443 failure demotes to HTTP recovery via _main_state.
                healthy = (getattr(webserver.app, "server", None) is not None or
                           getattr(webserver.admin_app, "server", None) is not None)
            elif mode == "admin":
                healthy = getattr(webserver.admin_app, "server", None) is not None
            else:
                healthy = getattr(webserver.app, "server", None) is not None
            if healthy:
                beat("http")
    finally:
        for task in server_tasks:
            if not task.done():
                task.cancel()
        for task in server_tasks:
            try:
                await task
            except BaseException:
                pass


async def apply_tls_task() -> None:
    """Reboot after a certificate push so the new context is applied cleanly.

    A reboot (rather than a live listener swap) keeps the BLE/WiFi supervisor
    model untouched and guarantees mbedTLS starts from a clean heap. It only
    happens when the pushed certificate actually changed."""
    while True:
        await _sleep_ms(TLS_REBOOT_POLL_MS)
        if tls_store.reboot_pending():
            logbuf.log("TLS", "rebooting to apply certificate")
            await asyncio.sleep(tls_store.REBOOT_DELAY_S)
            machine.reset()


async def supervise(factory, name: str, restart_delay_s: int = SUBSYSTEM_RESTART_DELAY_S) -> None:
    """Run factory() forever; if it ever raises or returns, log and restart
    it after a short backoff. Ensures no subsystem can die permanently."""
    while True:
        try:
            await factory()
            logbuf.log("App", name + " task exited unexpectedly, restarting")
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            logbuf.log("App", name + " crashed: " + str(exc) + ", restarting")
        await asyncio.sleep(restart_delay_s)


async def supervisor(wdt) -> None:
    while True:
        await asyncio.sleep(SUPERVISOR_INTERVAL_S)
        try:
            now = time.ticks_ms()
            stale = [
                name for name, limit_s in HEARTBEAT_STALE_S.items()
                if _is_stale(name, limit_s, now)
            ]
            if stale:
                logbuf.log("App", "stale heartbeats: " + ",".join(stale) + " (WDT not fed)")
            elif wdt is not None:
                wdt.feed()
        except Exception as exc:
            logbuf.log("App", "supervisor error: " + str(exc))


async def main() -> None:
    # Create the watchdog before anything else so the entire boot path
    # (including a slow/failing initial WiFi connect) is covered.
    try:
        wdt = machine.WDT(timeout=WDT_TIMEOUT_MS)
    except Exception as exc:
        logbuf.log("App", "WDT unavailable: " + str(exc))
        wdt = None

    cfg = load_config()
    ssid = cfg["wifi_ssid"]
    password = cfg["wifi_password"]
    hostname = cfg.get("hostname", DEFAULT_HOSTNAME)
    name_prefix = cfg.get("battery_name", "HM_B2500")
    mac = cfg.get("battery_mac")

    tls_store.init(cfg)
    webserver.configure_request_limits(tls_store.tls_enabled())
    if not tls_store.tls_enabled():
        logbuf.log("TLS", "disabled in config (set tls_enabled=true to opt in)")
    else:
        info = tls_store.installed_info()
        if info.get("installed") and info.get("valid"):
            cn = info.get("common_name") or "(no CN)"
            logbuf.log(
                "TLS",
                "certificate installed: CN=" + cn +
                " fp=" + info["fingerprint_sha256"][:16],
            )
        elif info.get("installed"):
            logbuf.log("TLS", "installed certificate is invalid: " + info.get("error", "unknown"))
        problem = tls_store.token_problem()
        if problem:
            logbuf.log("TLS", "certificate push disabled: " + problem)

    asyncio.create_task(supervisor(wdt))
    if tls_store.tls_enabled():
        asyncio.create_task(supervise(apply_tls_task, "tls-apply"))
    asyncio.create_task(
        supervise(lambda: wifi_task(ssid, password, hostname), "wifi")
    )
    asyncio.create_task(
        supervise(
            lambda: ble.ble_loop(name_prefix=name_prefix, mac=mac, heartbeat=lambda: beat("ble")),
            "ble",
        )
    )
    asyncio.create_task(supervise(http_task, "http"))

    while True:
        await asyncio.sleep(3600)


def run_forever() -> None:
    gc.collect()
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        raise
    except Exception as exc:
        logbuf.log("App", "fatal: " + str(exc) + ", hard-resetting for a clean slate")
        time.sleep(2)
        machine.reset()


if __name__ == "__main__":
    run_forever()
