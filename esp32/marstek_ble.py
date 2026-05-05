"""Marstek B2500 BLE protocol — Central role via aioble."""
import asyncio
import aioble
import bluetooth

SERVICE_UUID  = bluetooth.UUID(0xFF00)
WRITE_UUID    = bluetooth.UUID(0xFF01)
NOTIFY_UUID   = bluetooth.UUID(0xFF02)

_START = 0x73
_IDENT = 0x23

# Module-level connection state
_conn        = None
_write_char  = None
_notify_char = None
_lock        = asyncio.Lock()
connected    = False


def build_frame(opcode: int, payload: bytes = b"") -> bytes:
    length = 4 + len(payload) + 1
    body = bytes([_START, length, _IDENT, opcode]) + payload
    checksum = 0
    for b in body:
        checksum ^= b
    return body + bytes([checksum])


async def send_command(opcode: int, payload: bytes = b"") -> bytes | None:
    """Write a command and wait for the notify response. Returns raw response bytes."""
    if not connected or _write_char is None or _notify_char is None:
        return None
    async with _lock:
        try:
            await _write_char.write(build_frame(opcode, payload), response=False)
            return await _notify_char.notified(timeout_ms=3000)
        except Exception:
            return None


async def ble_loop(name_prefix: str, mac: str | None = None) -> None:
    """Persistent scan-connect-hold loop. Reconnects automatically on disconnect."""
    global _conn, _write_char, _notify_char, connected

    while True:
        print("BLE: scanning for", name_prefix + "*")
        device = None
        try:
            async with aioble.scan(duration_ms=10000, interval_us=30000, window_us=30000, active=True) as scanner:
                async for result in scanner:
                    name = result.name()
                    addr = str(result.device.addr_hex())
                    if name and name.startswith(name_prefix):
                        if mac is None or addr.lower().replace(":", "") == mac.lower().replace(":", ""):
                            device = result.device
                            print("BLE: found", name, addr)
                            break
        except Exception as e:
            print("BLE: scan error:", e)
            await asyncio.sleep(5)
            continue

        if device is None:
            print("BLE: not found, retrying in 10s")
            await asyncio.sleep(10)
            continue

        try:
            print("BLE: connecting…")
            conn = await device.connect(timeout_ms=10000)
        except Exception as e:
            print("BLE: connect failed:", e)
            await asyncio.sleep(5)
            continue

        try:
            svc = await conn.service(SERVICE_UUID)
            if svc is None:
                raise RuntimeError("service not found")
            wc = await svc.characteristic(WRITE_UUID)
            nc = await svc.characteristic(NOTIFY_UUID)
            if wc is None or nc is None:
                raise RuntimeError("characteristics not found")
            await nc.subscribe(notify=True)
        except Exception as e:
            print("BLE: GATT setup failed:", e)
            try:
                await conn.disconnect()
            except Exception:
                pass
            await asyncio.sleep(5)
            continue

        _conn = conn
        _write_char = wc
        _notify_char = nc
        connected = True
        print("BLE: ready")

        # Hold until disconnected
        try:
            await conn.disconnected()
        except Exception:
            pass

        print("BLE: disconnected")
        connected = False
        _conn = None
        _write_char = None
        _notify_char = None
        await asyncio.sleep(3)
