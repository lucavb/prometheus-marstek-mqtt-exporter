"""ESP32-S3 boot: connect WiFi, start BLE loop + HTTP server."""
import asyncio
import json
import network
import time
import marstek_ble as ble
import webserver


def load_config() -> dict:
    with open("config.json") as f:
        return json.load(f)


def connect_wifi(ssid: str, password: str) -> str:
    wlan = network.WLAN(network.STA_IF)
    wlan.active(True)
    if wlan.isconnected():
        return wlan.ifconfig()[0]
    print("WiFi: connecting to", ssid)
    wlan.connect(ssid, password)
    for _ in range(20):
        if wlan.isconnected():
            ip = wlan.ifconfig()[0]
            print("WiFi: connected, IP =", ip)
            return ip
        time.sleep(1)
    raise RuntimeError("WiFi connection failed")


async def main() -> None:
    cfg = load_config()
    ip = connect_wifi(cfg["wifi_ssid"], cfg["wifi_password"])
    print("Web UI: http://" + ip + "/")

    asyncio.create_task(
        ble.ble_loop(
            name_prefix=cfg.get("battery_name", "HM_B2500"),
            mac=cfg.get("battery_mac"),
        )
    )

    await webserver.app.start_server(host="0.0.0.0", port=80, debug=False)


asyncio.run(main())
