#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["pycryptodome>=3.20"]
# ///
"""Produce a base64url-encoded `v=` payload for setB2500Report, using the
recovered AES-128-ECB + PKCS#7 key (hamedatahamedata).

Mirrors the format the emulator's report.go expects: a URL-encoded
k=v&k=v plaintext. Fields are passed as KEY=VALUE args on the CLI so
this script never hard-codes real device data.
"""
from __future__ import annotations

import base64
import sys
from urllib.parse import urlencode

from Crypto.Cipher import AES

KEY = b"hamedatahamedata"


def pkcs7_pad(b: bytes, blk: int = 16) -> bytes:
    pad = blk - (len(b) % blk)
    return b + bytes([pad]) * pad


def main() -> int:
    if len(sys.argv) < 2:
        print("usage: encrypt_report.py KEY=VAL [KEY=VAL ...]", file=sys.stderr)
        return 2
    pairs = []
    for arg in sys.argv[1:]:
        if "=" not in arg:
            print(f"bad arg: {arg!r}", file=sys.stderr)
            return 2
        k, v = arg.split("=", 1)
        pairs.append((k, v))
    plaintext = urlencode(pairs).encode()
    ct = AES.new(KEY, AES.MODE_ECB).encrypt(pkcs7_pad(plaintext))
    print(base64.urlsafe_b64encode(ct).decode())
    return 0


if __name__ == "__main__":
    sys.exit(main())
