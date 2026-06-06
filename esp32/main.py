"""ESP32-S3 bridge: resilient WiFi, BLE loop, HTTP server."""
import asyncio
import gc
import json
import network
import time

import marstek_ble as ble
import webserver

DEFAULT_HOSTNAME = "esp32-marstek-watchdog"
WIFI_CHECK_INTERVAL_S = 15
WIFI_RETRY_DELAY_S = 5
APP_RESTART_DELAY_S = 5
WIFI_ATTEMPT_TIMEOUT_S = 45

_WLAN_STATUS = {
    1000: "idle",
    1001: "connecting",
    1010: "got IP",
    201: "no AP found",
    202: "auth failed",
}


def load_config() -> dict:
    with open("config.json") as f:
        return json.load(f)


def _wifi_status_text(status: int) -> str:
    return _WLAN_STATUS.get(status, str(status))


def _ensure_wlan() -> network.WLAN:
    wlan = network.WLAN(network.STA_IF)
    wlan.active(True)
    return wlan


def _set_hostname(wlan: network.WLAN, hostname: str) -> None:
    try:
        wlan.config(hostname=hostname)
    except Exception as exc:
        print("WiFi: hostname not set:", exc)


def _has_ip(wlan: network.WLAN) -> bool:
    return wlan.isconnected() and wlan.ifconfig()[0] != "0.0.0.0"


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

    print("WiFi: connecting to", ssid)
    if wlan.status() != network.STAT_CONNECTING:
        wlan.connect(ssid, password)

    for tick in range(wait_s):
        if _has_ip(wlan):
            ip = wlan.ifconfig()[0]
            print("WiFi: connected, IP =", ip)
            return ip
        if tick and tick % 10 == 0:
            print("WiFi: waiting, status:", _wifi_status_text(wlan.status()))
        await asyncio.sleep(1)

    return None


async def connect_wifi(ssid: str, password: str, hostname: str) -> str:
    attempt = 0
    while True:
        attempt += 1
        print("WiFi: connect attempt", attempt)
        ip = await try_connect_wifi(ssid, password, hostname)
        if ip:
            return ip

        wlan = _ensure_wlan()
        print("WiFi: failed, status:", _wifi_status_text(wlan.status()))
        try:
            wlan.disconnect()
        except OSError:
            pass
        await asyncio.sleep(WIFI_RETRY_DELAY_S)


async def wifi_monitor(ssid: str, password: str, hostname: str) -> None:
    while True:
        await asyncio.sleep(WIFI_CHECK_INTERVAL_S)
        wlan = _ensure_wlan()
        if _has_ip(wlan):
            continue

        print("WiFi: link lost, reconnecting")
        while not _has_ip(_ensure_wlan()):
            ip = await try_connect_wifi(ssid, password, hostname, wait_s=20)
            if ip:
                print("WiFi: restored, IP =", ip)
                break
            wlan = _ensure_wlan()
            print("WiFi: still down, status:", _wifi_status_text(wlan.status()))
            try:
                wlan.disconnect()
            except OSError:
                pass
            await asyncio.sleep(WIFI_RETRY_DELAY_S)


async def watchdog_feeder() -> None:
    try:
        import machine

        wdt = machine.WDT(timeout=60000)
    except Exception:
        return

    while True:
        try:
            wdt.feed()
        except Exception:
            return
        await asyncio.sleep(10)


async def run_app() -> None:
    cfg = load_config()
    ssid = cfg["wifi_ssid"]
    password = cfg["wifi_password"]
    hostname = cfg.get("hostname", DEFAULT_HOSTNAME)

    ip = await connect_wifi(ssid, password, hostname)
    print("Web UI: http://" + ip + "/")

    asyncio.create_task(wifi_monitor(ssid, password, hostname))
    asyncio.create_task(watchdog_feeder())
    asyncio.create_task(
        ble.ble_loop(
            name_prefix=cfg.get("battery_name", "HM_B2500"),
            mac=cfg.get("battery_mac"),
        )
    )

    await webserver.app.start_server(host="0.0.0.0", port=80, debug=False)


def run_forever() -> None:
    while True:
        try:
            gc.collect()
            asyncio.run(run_app())
        except KeyboardInterrupt:
            raise
        except Exception as exc:
            print("App crashed, restarting in", APP_RESTART_DELAY_S, "s:", exc)
            time.sleep(APP_RESTART_DELAY_S)


run_forever()
