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
import time

import logbuf
import marstek_ble as ble
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
    await asyncio.sleep_ms(WIFI_DRIVER_RESET_MS)
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
    logbuf.log("WiFi", "Web UI: http://" + ip + "/")

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


async def http_task() -> None:
    """Starts the HTTP server once WiFi has an IP, and supervises it as a
    subtask so this coroutine can post heartbeats independently of whatever
    microdot's accept loop is doing internally."""
    while wifi_ip is None:
        await asyncio.sleep_ms(200)

    old_server = getattr(webserver.app, "server", None)
    if old_server is not None:
        try:
            webserver.app.shutdown()
            await old_server.wait_closed()
        except Exception:
            pass

    logbuf.log("HTTP", "starting server")
    beat("http")
    server_task = asyncio.create_task(
        webserver.app.start_server(host="0.0.0.0", port=80, debug=False)
    )
    try:
        while True:
            await asyncio.sleep(HTTP_HEARTBEAT_INTERVAL_S)
            if server_task.done():
                # Re-raise whatever killed it so supervise() logs + restarts us.
                await server_task
                return
            beat("http")
    finally:
        if not server_task.done():
            server_task.cancel()


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

    asyncio.create_task(supervisor(wdt))
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


run_forever()
