#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["pycryptodome>=3.20"]
# ///
"""Decrypt the three base64 blobs at 0x26820 / 0x26E7C / 0x2776C using the
'hamedatahamedata' AES-128-ECB key already known from
emulator.DecryptReport (which decrypts the setB2500Report v= payload).

The same key and mode (AES-128-ECB + PKCS#7) work on the firmware blobs.
"""
from __future__ import annotations

import base64
import sys
from pathlib import Path

from Crypto.Cipher import AES


KEY = b"hamedatahamedata"
BLOBS = [
    (0x26820,),
    (0x26E7C,),
    (0x2776C,),
]


def extract_b64(data: bytes, start: int) -> bytes:
    end = start
    while end < len(data) and (
        (0x41 <= data[end] <= 0x5A)
        or (0x61 <= data[end] <= 0x7A)
        or (0x30 <= data[end] <= 0x39)
        or data[end] in (ord("+"), ord("/"), ord("="))
    ):
        end += 1
    return data[start:end]


def pkcs7_unpad(b: bytes) -> bytes | None:
    if not b:
        return None
    pad = b[-1]
    if pad < 1 or pad > AES.block_size:
        return None
    if any(x != pad for x in b[-pad:]):
        return None
    return b[:-pad]


def classify(pt: bytes) -> tuple[str, str]:
    """Return (description, short_tag)."""
    if pt.startswith(b"-----BEGIN"):
        header = pt.split(b"\n", 1)[0].decode("ascii", errors="replace").strip()
        if b"PRIVATE KEY" in pt[:80]:
            return f"PEM: {header}", "key"
        if b"CERTIFICATE" in pt[:80]:
            return f"PEM: {header}", "cert"
        return f"PEM: {header}", "pem"
    if pt.startswith(b"\x30\x82"):
        inner_len = int.from_bytes(pt[2:4], "big")
        return f"ASN.1 SEQUENCE, inner len {inner_len}", "der"
    if all(32 <= c < 127 or c in (9, 10, 13) for c in pt[:64]):
        return "printable text", "txt"
    return "binary, no container", "bin"


def main() -> int:
    path = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("firmware/B2500_All_HMJ.bin")
    data = path.read_bytes()
    outdir = Path("firmware/decrypted")
    outdir.mkdir(parents=True, exist_ok=True)

    cipher = AES.new(KEY, AES.MODE_ECB)
    for (off,) in BLOBS:
        raw_b64 = extract_b64(data, off)
        ct = base64.b64decode(raw_b64)
        if len(ct) % 16 != 0:
            print(f"[!] 0x{off:06x}: ciphertext {len(ct)} B not a multiple of 16, skipping")
            continue
        pt = cipher.decrypt(ct)
        unpadded = pkcs7_unpad(pt)
        if unpadded is None:
            print(f"[!] 0x{off:06x}: PKCS#7 unpad failed, keeping raw")
            unpadded = pt
        desc, tag = classify(unpadded)
        fname = outdir / f"blob_{off:06x}.{tag}.pem"
        fname.write_bytes(unpadded)
        print(f"0x{off:06x}  {len(ct):5d} B ct -> {len(unpadded):5d} B pt  {desc}")
        print(f"           wrote {fname}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
