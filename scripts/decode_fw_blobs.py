#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Decode the three base64 blobs at 0x26820 / 0x26E7C / 0x2776C."""
import base64
import sys
from pathlib import Path


def extract_b64(data: bytes, start: int) -> str:
    end = start
    while end < len(data) and (
        (0x41 <= data[end] <= 0x5A)
        or (0x61 <= data[end] <= 0x7A)
        or (0x30 <= data[end] <= 0x39)
        or data[end] in (ord("+"), ord("/"), ord("="))
    ):
        end += 1
    return data[start:end].decode("ascii")


def show(label: str, s: str) -> None:
    print(f"\n=== {label}  len(b64)={len(s)} ===")
    print(s[:120] + ("..." if len(s) > 120 else ""))
    try:
        raw = base64.b64decode(s)
    except Exception as e:
        print(f"  [!] not valid base64: {e}")
        return
    print(f"  decoded length: {len(raw)} bytes")
    print(f"  first 48 bytes: {raw[:48].hex()}")
    print(f"  last  16 bytes: {raw[-16:].hex()}")
    # Heuristics
    if raw[:1] == b"\x30" and len(raw) > 4:
        print("  [hint] starts with 0x30 -> ASN.1 SEQUENCE (likely DER cert / key)")
    if b"CERTIFICATE" in raw or b"RSA" in raw or b"PRIVATE" in raw:
        print("  [hint] contains PEM header text")
    ent = 0.0
    from collections import Counter
    import math
    c = Counter(raw)
    for v in c.values():
        p = v / len(raw)
        ent -= p * math.log2(p)
    print(f"  byte entropy:   {ent:.2f} / 8.00")


def main() -> int:
    data = Path(sys.argv[1]).read_bytes()
    for off in (0x26820, 0x26E7C, 0x2776C):
        s = extract_b64(data, off)
        show(f"blob @ 0x{off:06x}", s)
    return 0


if __name__ == "__main__":
    sys.exit(main())
