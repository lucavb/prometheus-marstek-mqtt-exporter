"""Marstek B2500 BLE protocol — Central role via aioble."""
import asyncio
import aioble
import bluetooth
import logbuf
import time

SERVICE_UUID = bluetooth.UUID(0xFF00)
WRITE_UUID = bluetooth.UUID(0xFF01)
NOTIFY_UUID = bluetooth.UUID(0xFF02)
FF06_UUID = bluetooth.UUID(0xFF06)

_START = 0x73
_IDENT = 0x23

_MAX_QUEUE_PER_OPCODE = 6
_MIN_RUNTIME_FRAME_LEN = 39
_HANDSHAKE_RETRIES = 5
_CONNECT_TIMEOUT_MS = 15000
_MTU_EXCHANGE_TIMEOUT_MS = 5000
_GATT_DISCOVERY_TIMEOUT_MS = 5000
_POST_MTU_SETTLE_MS = 400
_POST_SERVICE_SETTLE_MS = 300
_DISCONNECT_TEARDOWN_MS = 1000
_BLE_RADIO_RESET_MS = 800
_FAILED_CYCLE_COOLDOWN_S = 12
_FAILED_CYCLES_LONG_COOLDOWN_S = 30
_FAILED_CYCLES_ESCALATE_AFTER = 3
_NOTIFY_DEAD_WATCHDOG_MS = 5000
# Active liveness probe while holding a session: if nothing else has
# exercised the link for this long, send a lightweight status read. Catches
# a battery that goes silent without a clean BLE disconnect, which would
# otherwise leave `_wait_for_link_drop` parked forever with connected=True.
_KEEPALIVE_IDLE_MS = 20000
_KEEPALIVE_MAX_MISSES = 3

_conn = None
_write_char = None
_notify_char = None
_ff06_char = None
_notify_task = None
_session_nonce = 0
_failed_cycles = 0
_heartbeat_cb = None

_lock = asyncio.Lock()
_teardown_lock = asyncio.Lock()
_notify_queues = {}
_crc_mismatch_count = {}

state = "disconnected"
last_error = None
negotiated_mtu = 23
connected = False


def _format_exc(exc: BaseException) -> str:
    if isinstance(exc, OSError) and exc.args:
        return "OSError" + repr(exc.args)
    msg = str(exc)
    return msg if msg else repr(exc)


def _is_link_lost(exc: BaseException) -> bool:
    if isinstance(exc, OSError) and exc.args and exc.args[0] == -128:
        return True
    msg = str(exc)
    return "Not connected" in msg or "Disconnect" in msg or "-128" in msg


def _beat() -> None:
    if _heartbeat_cb is not None:
        try:
            _heartbeat_cb()
        except Exception:
            pass


def _set_state(new_state: str, error: str | None = None) -> None:
    global state, connected, last_error
    state = new_state
    connected = (new_state == "ready")
    if error:
        last_error = error
    logbuf.log("BLE", "state -> " + new_state + ("" if not error else " (" + error + ")"))
    _beat()


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
        logbuf.log(
            "BLE",
            "notify crc mismatch, accepting by opcode: "
            + hex(opcode) + " count=" + str(count) + " len=" + str(len(frame)),
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
    exit_exc = None
    try:
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
                exit_exc = exc
                if _is_link_lost(exc):
                    logbuf.log("BLE", "notify loop: link lost (" + _format_exc(exc) + ")")
                else:
                    _set_state("degraded", "notify loop error: " + _format_exc(exc))
                return

            opcode, crc_ok = _extract_opcode(frame)
            if opcode is None:
                logbuf.log("BLE", "dropped malformed notify: " + frame.hex())
                continue
            if not crc_ok:
                # Firmware 116 appears to emit valid 0x73/0x23 frames with non-XOR
                # tails on some notifications; accept only with shape checks.
                if not _accept_crc_mismatch_frame(opcode, frame):
                    logbuf.log("BLE", "dropped crc-mismatch notify: " + hex(opcode) + " len=" + str(len(frame)))
                    continue
                _log_crc_mismatch(opcode, frame)
            _queue_notification(opcode, bytes(frame))
    finally:
        if local_nonce == _session_nonce and _conn is not None:
            asyncio.create_task(_force_teardown("notify loop exit"))


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


def _clear_discover_slot(conn) -> None:
    if conn is not None:
        conn._discover = None


async def _disconnect_current() -> None:
    global _conn, _write_char, _notify_char, _ff06_char, _notify_task
    if _notify_task is not None:
        _notify_task.cancel()
        _notify_task = None
    if _conn is not None:
        _clear_discover_slot(_conn)
        try:
            await _conn.disconnect()
        except Exception:
            pass
    _conn = None
    _write_char = None
    _notify_char = None
    _ff06_char = None
    _clear_notify_queues()
    # Let NimBLE finish teardown before the next connect attempt.
    await asyncio.sleep_ms(_DISCONNECT_TEARDOWN_MS)


async def _force_teardown(reason: str) -> None:
    async with _teardown_lock:
        if _conn is None:
            return
        logbuf.log("BLE", "force teardown: " + reason)
        await _disconnect_current()


async def _reset_ble_radio() -> None:
    logbuf.log("BLE", "radio reset")
    ble = bluetooth.BLE()
    ble.active(False)
    await asyncio.sleep_ms(_BLE_RADIO_RESET_MS)
    try:
        ble.config(rxbuf=512)
    except Exception:
        pass
    ble.active(True)
    await asyncio.sleep_ms(_BLE_RADIO_RESET_MS)


def _discovery_in_progress_error(exc: BaseException) -> bool:
    return "Discovery in progress" in str(exc)


def _transient_gatt_error(exc: BaseException) -> bool:
    msg = str(exc)
    if _discovery_in_progress_error(exc):
        return False
    if _is_link_lost(exc):
        return False
    return (
        "EALREADY" in msg
        or "Not connected" in msg
        or "can't convert NoneType to int" in msg
    )


async def _await_gatt(conn, step: str, fn, retries: int = 10, delay_ms: int = 200):
    """Retry GATT lookups; fail fast on poisoned discovery slot."""
    last_exc = None
    for i in range(retries):
        _clear_discover_slot(conn)
        try:
            val = await fn()
            if val is not None:
                return val
        except Exception as exc:
            last_exc = exc
            if _is_link_lost(exc):
                _clear_discover_slot(conn)
                raise
            if _discovery_in_progress_error(exc):
                logbuf.log("BLE", step + " failed: " + _format_exc(exc))
                raise RuntimeError(step + " failed: " + _format_exc(exc)) from exc
            if not _transient_gatt_error(exc):
                raise
            logbuf.log("BLE", step + " retry " + str(i + 1) + ": " + _format_exc(exc))
        await asyncio.sleep_ms(delay_ms + i * 75)
    if last_exc is not None:
        raise RuntimeError(step + " failed: " + _format_exc(last_exc)) from last_exc
    raise RuntimeError(step + " timed out")


async def _discover_service(conn, uuid, timeout_ms: int = _GATT_DISCOVERY_TIMEOUT_MS):
    found = None
    async for svc in conn.services(uuid, timeout_ms=timeout_ms):
        if svc.uuid == uuid:
            found = svc
    return found


async def _discover_all_characteristics(svc, timeout_ms: int = _GATT_DISCOVERY_TIMEOUT_MS) -> dict:
    chars = {}
    async for ch in svc.characteristics(timeout_ms=timeout_ms):
        chars[ch.uuid] = ch
    return chars


async def _exchange_mtu(conn) -> int:
    if not hasattr(conn, "exchange_mtu"):
        logbuf.log("BLE", "MTU exchange unsupported, continuing with default MTU")
        return 23

    mtu_link_lost = False
    # Match observed battery-side behavior: try 247 first.
    for preferred in (247, 517):
        try:
            mtu = await conn.exchange_mtu(preferred, timeout_ms=_MTU_EXCHANGE_TIMEOUT_MS)
            if isinstance(mtu, int):
                return mtu
        except Exception as exc:
            if _discovery_in_progress_error(exc):
                raise
            if _is_link_lost(exc):
                mtu_link_lost = True
                logbuf.log("BLE", "MTU exchange failed for " + str(preferred) + ": " + _format_exc(exc))
                continue
            if _transient_gatt_error(exc):
                await asyncio.sleep_ms(250)
                continue
            logbuf.log("BLE", "MTU exchange failed for " + str(preferred) + ": " + _format_exc(exc))
            await asyncio.sleep_ms(120)

    if mtu_link_lost:
        raise RuntimeError("MTU exchange failed: link lost")

    logbuf.log("BLE", "MTU exchange failed, continuing with default MTU")
    return 23


async def _connect_with_hints(device):
    try:
        return await device.connect(
            timeout_ms=_CONNECT_TIMEOUT_MS,
            min_conn_interval_us=30000,
            max_conn_interval_us=45000,
        )
    except TypeError:
        return await device.connect(timeout_ms=_CONNECT_TIMEOUT_MS)
    except asyncio.TimeoutError as exc:
        raise RuntimeError("BLE connect timed out") from exc


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
    # Give NimBLE a settle window before GATT discovery / MTU exchange.
    await asyncio.sleep_ms(800)

    _set_state("negotiating")
    negotiated_mtu = await _exchange_mtu(conn)
    logbuf.log("BLE", "negotiated MTU = " + str(negotiated_mtu))
    await asyncio.sleep_ms(_POST_MTU_SETTLE_MS)

    logbuf.log("BLE", "GATT service discovery start")
    svc = await _await_gatt(
        conn,
        "service discovery",
        lambda: _discover_service(conn, SERVICE_UUID),
    )
    if svc is None:
        raise RuntimeError("service discovery failed: FF00 not found")
    logbuf.log("BLE", "GATT service discovery done")

    await asyncio.sleep_ms(_POST_SERVICE_SETTLE_MS)

    logbuf.log("BLE", "GATT characteristic discovery start")
    chars = await _await_gatt(
        conn,
        "characteristic discovery",
        lambda: _discover_all_characteristics(svc),
    )
    logbuf.log("BLE", "GATT characteristic discovery done")
    wc = chars.get(WRITE_UUID)
    nc = chars.get(NOTIFY_UUID)
    fc = chars.get(FF06_UUID)
    if wc is None or nc is None:
        raise RuntimeError(
            "characteristic discovery failed: missing "
            + ("FF01 " if wc is None else "")
            + ("FF02" if nc is None else "")
        )

    _set_state("subscribing")
    logbuf.log("BLE", "subscribe FF02")
    await nc.subscribe(notify=True)
    if fc is not None:
        try:
            logbuf.log("BLE", "subscribe FF06")
            await fc.subscribe(notify=True)
        except Exception as exc:
            logbuf.log("BLE", "FF06 subscribe failed: " + _format_exc(exc))
            fc = None

    _write_char = wc
    _notify_char = nc
    _ff06_char = fc
    _clear_notify_queues()

    _session_nonce += 1
    local_nonce = _session_nonce
    _set_state("priming")
    logbuf.log("BLE", "priming")
    _notify_task = asyncio.create_task(_notification_loop(local_nonce))
    await asyncio.sleep_ms(80)

    if not await _prime_session():
        raise RuntimeError("startup probe failed: missing runtime response")

    _set_state("ready")
    logbuf.log("BLE", "ready, FF06=" + ("yes" if fc else "no"))
    last_error = None
    return True


async def _wait_for_link_drop() -> None:
    conn = _conn
    if conn is None:
        return

    drop_task = asyncio.create_task(conn.disconnected())
    notify_done_since = None
    last_activity = time.ticks_ms()
    keepalive_misses = 0

    try:
        while True:
            if drop_task.done():
                await drop_task
                return

            notify_task = _notify_task
            unhealthy = state == "degraded" or (notify_task is not None and notify_task.done())
            if unhealthy:
                if notify_done_since is None:
                    notify_done_since = time.ticks_ms()
                elif time.ticks_diff(time.ticks_ms(), notify_done_since) >= _NOTIFY_DEAD_WATCHDOG_MS:
                    logbuf.log("BLE", "disconnect watchdog: session unhealthy")
                    await _force_teardown("watchdog")
                    if drop_task.done():
                        await drop_task
                    return
            else:
                notify_done_since = None

            # Active keepalive: a battery that goes silent without a clean
            # BLE disconnect would otherwise leave us parked here forever
            # with connected=True, never returning to scan/reconnect.
            if time.ticks_diff(time.ticks_ms(), last_activity) >= _KEEPALIVE_IDLE_MS:
                last_activity = time.ticks_ms()
                resp = await send_command(0x03)
                if resp is not None:
                    keepalive_misses = 0
                    _beat()
                else:
                    keepalive_misses += 1
                    logbuf.log(
                        "BLE",
                        "keepalive miss " + str(keepalive_misses) + "/" + str(_KEEPALIVE_MAX_MISSES),
                    )
                    if keepalive_misses >= _KEEPALIVE_MAX_MISSES:
                        logbuf.log("BLE", "keepalive timeout: no response, forcing teardown")
                        await _force_teardown("keepalive timeout")
                        if drop_task.done():
                            await drop_task
                        return
            else:
                _beat()

            await asyncio.sleep_ms(500)
    except Exception as exc:
        logbuf.log("BLE", "wait for link drop: " + _format_exc(exc))
    finally:
        if not drop_task.done():
            drop_task.cancel()


async def ble_loop(name_prefix: str, mac: str | None = None, heartbeat=None) -> None:
    """Persistent scan-connect-hold loop. Reconnects automatically on disconnect."""
    global last_error, _failed_cycles, _heartbeat_cb

    _heartbeat_cb = heartbeat
    target_mac = None if mac is None else mac.lower().replace(":", "")

    while True:
        _set_state("scanning")
        logbuf.log("BLE", "scanning for " + name_prefix + "*")
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
                    logbuf.log("BLE", "found " + name + " " + addr)
                    break
        except Exception as exc:
            last_error = "scan failed: " + _format_exc(exc)
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
                logbuf.log("BLE", "setup attempt " + str(attempt))
                await _setup_connection(device)
                setup_ok = True
                _failed_cycles = 0
                break
            except Exception as exc:
                last_error = "setup failed: " + _format_exc(exc)
                _set_state("degraded", last_error)
                await _disconnect_current()
                if _is_link_lost(exc):
                    await _reset_ble_radio()
                    break
                await asyncio.sleep(2 + attempt * 2)

        if not setup_ok:
            _failed_cycles += 1
            await _reset_ble_radio()
            _set_state("disconnected", last_error)
            cooldown = _FAILED_CYCLE_COOLDOWN_S
            if _failed_cycles >= _FAILED_CYCLES_ESCALATE_AFTER:
                cooldown = _FAILED_CYCLES_LONG_COOLDOWN_S
            logbuf.log("BLE", "failed cycle cooldown " + str(cooldown) + "s")
            await asyncio.sleep(cooldown)
            continue

        try:
            await _wait_for_link_drop()
            _set_state("disconnected", "link dropped")
            await _disconnect_current()
        except Exception as exc:
            # Belt-and-suspenders: ble_loop is a detached task, so an
            # unhandled exception here would otherwise kill BLE permanently
            # (silently, since MicroPython asyncio only logs uncaught
            # exceptions from detached tasks — it doesn't crash the app).
            logbuf.log("BLE", "hold/teardown error: " + _format_exc(exc))
            try:
                await _disconnect_current()
            except Exception:
                pass
        await asyncio.sleep(3)
