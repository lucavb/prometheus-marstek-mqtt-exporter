"""Marstek B2500 BLE protocol — Central role via aioble."""
import asyncio
import aioble
import bluetooth
import time

SERVICE_UUID = bluetooth.UUID(0xFF00)
WRITE_UUID = bluetooth.UUID(0xFF01)
NOTIFY_UUID = bluetooth.UUID(0xFF02)
FF06_UUID = bluetooth.UUID(0xFF06)

_START = 0x73
_IDENT = 0x23

_MAX_QUEUE_PER_OPCODE = 6
_MIN_RUNTIME_FRAME_LEN = 39
_HANDSHAKE_RETRIES = 3

_conn = None
_write_char = None
_notify_char = None
_ff06_char = None
_notify_task = None
_session_nonce = 0

_lock = asyncio.Lock()
_notify_queues = {}
_crc_mismatch_count = {}

state = "disconnected"
last_error = None
negotiated_mtu = 23
connected = False


def _set_state(new_state: str, error: str | None = None) -> None:
    global state, connected, last_error
    state = new_state
    connected = (new_state == "ready")
    if error:
        last_error = error
    print("BLE: state ->", new_state + ("" if not error else " (" + error + ")"))


def _clear_notify_queues() -> None:
    _notify_queues.clear()
    _crc_mismatch_count.clear()


def _queue_notification(opcode: int, data: bytes) -> None:
    queue = _notify_queues.get(opcode)
    if queue is None:
        queue = []
        _notify_queues[opcode] = queue
    queue.append(data)
    if len(queue) > _MAX_QUEUE_PER_OPCODE:
        del queue[0]


def _dequeue_notification(opcode: int) -> bytes | None:
    queue = _notify_queues.get(opcode)
    if not queue:
        return None
    data = queue[0]
    del queue[0]
    if not queue:
        _notify_queues.pop(opcode, None)
    return data


def _extract_opcode(frame: bytes) -> tuple[int | None, bool]:
    if len(frame) < 5:
        return None, False
    if frame[0] != _START or frame[2] != _IDENT:
        return None, False

    crc = 0
    for b in frame[:-1]:
        crc ^= b
    return frame[3], (crc == frame[-1])


def _is_printable_ascii_payload(payload: bytes) -> bool:
    if not payload:
        return False
    printable = 0
    for b in payload:
        if 32 <= b <= 126:
            printable += 1
    return printable >= (len(payload) * 2 // 3)


def _accept_crc_mismatch_frame(opcode: int, frame: bytes) -> bool:
    # We still enforce opcode-specific shape checks for non-XOR frames.
    if opcode == 0x03:
        return len(frame) >= _MIN_RUNTIME_FRAME_LEN
    if opcode == 0x09:
        payload = frame[4:-1]
        # fw 116 may prepend two status bytes before ASCII name.
        if len(payload) >= 2:
            payload = payload[2:]
        return _is_printable_ascii_payload(payload)
    if opcode == 0x23:
        return _is_printable_ascii_payload(frame[4:-1])
    if opcode in (0x12, 0x14, 0x35, 0x02, 0x25, 0x26):
        return len(frame) >= 6
    if opcode == 0x30:
        return len(frame) >= 6
    return len(frame) >= 5


def _log_crc_mismatch(opcode: int, frame: bytes) -> None:
    count = _crc_mismatch_count.get(opcode, 0) + 1
    _crc_mismatch_count[opcode] = count
    # Avoid log spam: print the first 3, then every 20th.
    if count <= 3 or (count % 20) == 0:
        print(
            "BLE: notify crc mismatch, accepting by opcode:",
            hex(opcode),
            "count=" + str(count),
            "len=" + str(len(frame)),
        )


async def _wait_for_opcode(opcode: int, timeout_ms: int = 3000) -> bytes | None:
    deadline = time.ticks_add(time.ticks_ms(), timeout_ms)
    while time.ticks_diff(deadline, time.ticks_ms()) > 0:
        frame = _dequeue_notification(opcode)
        if frame is not None:
            return frame
        if state not in ("ready", "priming"):
            return None
        await asyncio.sleep_ms(20)
    return None


async def _notification_loop(local_nonce: int) -> None:
    while (
        local_nonce == _session_nonce
        and _conn is not None
        and _notify_char is not None
        and state in ("priming", "ready")
    ):
        try:
            frame = await _notify_char.notified(timeout_ms=1200)
        except asyncio.TimeoutError:
            continue
        except Exception as exc:
            _set_state("degraded", "notify loop error: " + str(exc))
            return

        opcode, crc_ok = _extract_opcode(frame)
        if opcode is None:
            print("BLE: dropped malformed notify:", frame.hex())
            continue
        if not crc_ok:
            # Firmware 116 appears to emit valid 0x73/0x23 frames with non-XOR
            # tails on some notifications; accept only with shape checks.
            if not _accept_crc_mismatch_frame(opcode, frame):
                print("BLE: dropped crc-mismatch notify:", hex(opcode), "len=" + str(len(frame)))
                continue
            _log_crc_mismatch(opcode, frame)
        _queue_notification(opcode, bytes(frame))


def build_frame(opcode: int, payload: bytes = b"") -> bytes:
    length = 4 + len(payload) + 1
    body = bytes([_START, length, _IDENT, opcode]) + payload
    crc = 0
    for b in body:
        crc ^= b
    return body + bytes([crc])


def build_aa_frame(value: int) -> bytes:
    """FF06 hardware-reset: aa 05 01 00 01 <value> 00 CRC (sum of LL..pad mod 256)."""
    data = bytes([0x05, 0x01, 0x00, 0x01, value, 0x00])
    crc = sum(data) & 0xFF
    return bytes([0xAA]) + data + bytes([crc])


async def _write_and_wait(opcode: int, payload: bytes = b"", timeout_ms: int = 3000) -> bytes | None:
    if _write_char is None or state not in ("priming", "ready"):
        return None
    _notify_queues.pop(opcode, None)
    await _write_char.write(build_frame(opcode, payload), response=False)
    return await _wait_for_opcode(opcode, timeout_ms=timeout_ms)


async def send_command(opcode: int, payload: bytes = b"") -> bytes | None:
    """Write a command and wait for a response with matching opcode."""
    if not connected or _write_char is None or _notify_char is None:
        return None
    async with _lock:
        try:
            return await _write_and_wait(opcode, payload, timeout_ms=3000)
        except Exception:
            return None


async def send_command_multi(opcode: int, payload: bytes = b"", count: int = 2) -> list | None:
    """Write a command and collect up to `count` matching notifications."""
    if not connected or _write_char is None or _notify_char is None:
        return None
    async with _lock:
        try:
            _notify_queues.pop(opcode, None)
            await _write_char.write(build_frame(opcode, payload), response=False)
            results = []
            for _ in range(count):
                frame = await _wait_for_opcode(opcode, timeout_ms=3000)
                if frame is None:
                    break
                results.append(frame)
            return results if results else None
        except Exception:
            return None


async def send_ff06_reset() -> bool:
    """Hardware reset via FF06: press (0x01) then release (0x00) ~500 ms apart."""
    if not connected or _ff06_char is None:
        return False
    async with _lock:
        try:
            await _ff06_char.write(build_aa_frame(0x01), response=False)
            await asyncio.sleep_ms(500)
            await _ff06_char.write(build_aa_frame(0x00), response=False)
            return True
        except Exception:
            return False


async def _disconnect_current() -> None:
    global _conn, _write_char, _notify_char, _ff06_char, _notify_task
    if _notify_task is not None:
        _notify_task.cancel()
        _notify_task = None
    if _conn is not None:
        try:
            await _conn.disconnect()
        except Exception:
            pass
    _conn = None
    _write_char = None
    _notify_char = None
    _ff06_char = None
    _clear_notify_queues()


async def _exchange_mtu(conn) -> int:
    if not hasattr(conn, "exchange_mtu"):
        print("BLE: MTU exchange unsupported, continuing with default MTU")
        return 23

    # Match observed battery-side behavior: try 247 first.
    for preferred in (247, 517):
        try:
            mtu = await conn.exchange_mtu(preferred, timeout_ms=3000)
            if isinstance(mtu, int):
                return mtu
        except Exception as exc:
            print("BLE: MTU exchange failed for", preferred, ":", exc)
            await asyncio.sleep_ms(120)

    print("BLE: MTU exchange failed, continuing with default MTU")
    return 23


async def _connect_with_hints(device):
    try:
        return await device.connect(
            timeout_ms=15000,
            min_conn_interval_us=30000,
            max_conn_interval_us=45000,
        )
    except TypeError:
        return await device.connect(timeout_ms=15000)


async def _prime_session() -> bool:
    async with _lock:
        if _write_char is None:
            return False
        # App startup sends 0x13/0x09/0x03 as a burst.
        await _write_char.write(build_frame(0x13, b"\x01"), response=False)
        await _write_char.write(build_frame(0x09, b"\x01"), response=False)
        await _write_char.write(build_frame(0x03, b"\x01"), response=False)
        runtime = await _wait_for_opcode(0x03, timeout_ms=2500)
        return runtime is not None and len(runtime) >= _MIN_RUNTIME_FRAME_LEN and runtime[3] == 0x03


async def _setup_connection(device) -> bool:
    global _conn, _write_char, _notify_char, _ff06_char, _notify_task, _session_nonce, negotiated_mtu, last_error

    # Avoid stale MTU in status when a new attempt fails.
    negotiated_mtu = 23
    _set_state("connecting")
    conn = await _connect_with_hints(device)
    _conn = conn
    # Give NimBLE a short post-connect settle window before GATT procedures.
    await asyncio.sleep_ms(250)

    _set_state("negotiating")
    negotiated_mtu = await _exchange_mtu(conn)
    print("BLE: negotiated MTU =", negotiated_mtu)

    svc = await conn.service(SERVICE_UUID)
    if svc is None:
        raise RuntimeError("service FF00 not found")

    wc = await svc.characteristic(WRITE_UUID)
    nc = await svc.characteristic(NOTIFY_UUID)
    if wc is None or nc is None:
        raise RuntimeError("FF01/FF02 characteristics not found")

    _set_state("subscribing")
    await nc.subscribe(notify=True)
    fc = None
    try:
        fc = await svc.characteristic(FF06_UUID)
        if fc:
            await fc.subscribe(notify=True)
    except Exception:
        fc = None

    _write_char = wc
    _notify_char = nc
    _ff06_char = fc
    _clear_notify_queues()

    _session_nonce += 1
    local_nonce = _session_nonce
    _set_state("priming")
    _notify_task = asyncio.create_task(_notification_loop(local_nonce))
    await asyncio.sleep_ms(80)

    if not await _prime_session():
        raise RuntimeError("startup probe failed: missing runtime response")

    _set_state("ready")
    print("BLE: ready, FF06=" + ("yes" if fc else "no"))
    last_error = None
    return True


async def ble_loop(name_prefix: str, mac: str | None = None) -> None:
    """Persistent scan-connect-hold loop. Reconnects automatically on disconnect."""
    global last_error

    target_mac = None if mac is None else mac.lower().replace(":", "")

    while True:
        _set_state("scanning")
        print("BLE: scanning for", name_prefix + "*")
        device = None
        try:
            async with aioble.scan(duration_ms=10000, interval_us=30000, window_us=30000, active=True) as scanner:
                async for result in scanner:
                    name = result.name()
                    addr = str(result.device.addr_hex())
                    if not (name and name.startswith(name_prefix)):
                        continue
                    if target_mac is not None and addr.lower().replace(":", "") != target_mac:
                        continue
                    device = result.device
                    print("BLE: found", name, addr)
                    break
        except Exception as exc:
            last_error = "scan failed: " + str(exc)
            _set_state("disconnected", last_error)
            await asyncio.sleep(5)
            continue

        if device is None:
            _set_state("disconnected", "battery not found")
            await asyncio.sleep(10)
            continue

        setup_ok = False
        for attempt in range(1, _HANDSHAKE_RETRIES + 1):
            try:
                print("BLE: setup attempt", attempt)
                await _setup_connection(device)
                setup_ok = True
                break
            except Exception as exc:
                last_error = "setup failed: " + str(exc)
                _set_state("degraded", last_error)
                await _disconnect_current()
                await asyncio.sleep(1 + attempt)

        if not setup_ok:
            _set_state("disconnected", last_error)
            await asyncio.sleep(4)
            continue

        try:
            await _conn.disconnected()
        except Exception:
            pass

        _set_state("disconnected", "link dropped")
        await _disconnect_current()
        await asyncio.sleep(3)
