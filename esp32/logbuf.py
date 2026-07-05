"""Ephemeral in-RAM log ring buffer for web UI tail."""
import asyncio
import time

_RING_SIZE = 80

_entries = []
_seq = 0
_wake = None


def _get_wake() -> asyncio.Event:
    global _wake
    if _wake is None:
        _wake = asyncio.Event()
    return _wake


def log(tag: str, msg: str) -> None:
    global _seq
    _seq += 1
    _entries.append({
        "seq": _seq,
        "ts_ms": time.ticks_ms(),
        "tag": tag,
        "msg": msg,
    })
    if len(_entries) > _RING_SIZE:
        del _entries[0]
    print(tag + ": " + msg)
    _get_wake().set()


def get_since(since: int) -> tuple[list, int]:
    out = [e for e in _entries if e["seq"] > since]
    return out, _seq


async def wait_new(since: int, timeout_ms: int) -> None:
    deadline = time.ticks_add(time.ticks_ms(), timeout_ms)
    while True:
        if get_since(since)[0]:
            return
        remaining = time.ticks_diff(deadline, time.ticks_ms())
        if remaining <= 0:
            return
        wake = _get_wake()
        wake.clear()
        try:
            await asyncio.wait_for(wake.wait(), remaining / 1000)
        except asyncio.TimeoutError:
            return
