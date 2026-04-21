#!/usr/bin/env python3
"""
Brute-force the eu.hamedata.com /prod/api/v1/setB2500Report?v=... payload.

Usage:
  # Scan one or more pcap captures for encrypted report payloads:
  uv run --with scapy --with pycryptodome --python 3.12 \\
      python3 scripts/crack_report.py capture.pcap [capture2.pcap ...]

  # Pass a base64url value directly (no pcap needed):
  uv run --with pycryptodome --python 3.12 \\
      python3 scripts/crack_report.py --b64 '<value from v= parameter>'

The confirmed answer for firmware HMJ-2 fcv=202310231502 is:
  AES-128-ECB, key = b"hamedatahamedata", PKCS#7 padding, plaintext is a
  URL-encoded key=value&... query string.

The script re-derives the key from scratch via a scored dictionary attack so
the derivation is auditable and resilient to future firmware key rotations.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import math
import re
import sys
import urllib.parse
from collections import Counter
from typing import Iterable

from Crypto.Cipher import AES


# ---------------------------------------------------------------------------
# 1. Payload extraction from pcap files
# ---------------------------------------------------------------------------

REPORT_RE = re.compile(rb"/prod/api/v1/setB2500Report\?v=([A-Za-z0-9_\-=]+)")
DATE_INFO_RE = re.compile(rb"/app/neng/getDateInfoeu\.php\?([A-Za-z0-9_=&%.\\-]+)")


def payloads_from_pcap(path: str) -> list[tuple[str, dict[str, str]]]:
    """Return (b64_payload, context) tuples extracted from a pcap.

    context carries uid/device_type/fcv/etc. from any co-located
    getDateInfo request so the key-guess harness can use device-specific
    strings.
    """
    from scapy.all import rdpcap, TCP, IP  # noqa: PLC0415 – lazy import (no scapy in --b64 mode)

    pkts = rdpcap(path)
    streams: dict[tuple, bytes] = {}
    for p in pkts:
        if TCP in p and IP in p:
            raw = bytes(p[TCP].payload)
            if not raw:
                continue
            key = (p[IP].src, p[TCP].sport, p[IP].dst, p[TCP].dport)
            streams.setdefault(key, b"")
            streams[key] += raw

    context: dict[str, str] = {}
    for data in streams.values():
        m = DATE_INFO_RE.search(data)
        if m:
            qs = urllib.parse.parse_qs(m.group(1).decode("latin1"))
            for k in ("uid", "aid", "fcv", "sv", "sbv", "mv"):
                if k in qs:
                    context[k] = qs[k][0]

    results: list[tuple[str, dict[str, str]]] = []
    for data in streams.values():
        for m in REPORT_RE.finditer(data):
            results.append((m.group(1).decode("ascii"), dict(context)))
    return results


# ---------------------------------------------------------------------------
# 2. Candidate key generator
# ---------------------------------------------------------------------------

def key_candidates(ctx: dict[str, str]) -> Iterable[tuple[str, bytes]]:
    """Yield (label, key_bytes) pairs. Keys are 16 or 32 bytes."""
    uid = ctx.get("uid", "")
    device_type = ctx.get("aid", "HMJ-2")
    fcv = ctx.get("fcv", "")

    seen: set[tuple[int, bytes]] = set()

    def emit(label: str, k: bytes) -> Iterable[tuple[str, bytes]]:
        if 1 <= len(k) <= 32 and (len(k), k) not in seen:
            seen.add((len(k), k))
            yield label, k

    strings = [
        device_type, fcv, uid, uid.upper(),
        # brand / domain strings — the single most productive source of
        # embedded-firmware keys (authors routinely reuse the vendor brand)
        "hame", "Hame", "HAME",
        "hamedata", "Hamedata", "HAMEDATA",
        "hamedata.com", "eu.hamedata.com",
        "hame_energy", "hame_iot", "hame_iot_secret",
        "hm_cloud_key", "hm_cloud_secret",
        "hame-2024", "hame-2025",
        # device family strings
        "b2500", "B2500", "B2500Report", "setB2500Report",
        "marstek", "Marstek", "MARSTEK",
        "quectel-fc41d", "fc41d",
        # raw hex chunks of the uid as ASCII
        uid[:16], uid[-16:], uid.upper()[:16],
    ]

    for s in strings:
        if not s:
            continue
        b = s.encode()

        # ASCII repeated / null-padded to 16 or 32 bytes
        for L in (16, 32):
            yield from emit(
                f"ascii_repeat[{L}]({s!r})",
                (b * ((L // max(1, len(b))) + 1))[:L],
            )
            if len(b) < L:
                yield from emit(
                    f"ascii_null[{L}]({s!r})",
                    b + b"\x00" * (L - len(b)),
                )

        # hash-derived keys
        for L, fn in [
            (16, lambda x: hashlib.md5(x).digest()),           # noqa: S324
            (16, lambda x: hashlib.sha1(x).digest()[:16]),     # noqa: S324
            (16, lambda x: hashlib.sha256(x).digest()[:16]),
            (32, lambda x: hashlib.sha256(x).digest()),
        ]:
            yield from emit(f"hash[{L}]({s!r})", fn(b))

    # pairwise / triple combinations with different separators
    parts_list = [
        (uid,), (uid, device_type), (uid, fcv), (device_type, fcv),
        (uid, device_type, fcv),
        ("hame", uid), ("hamedata", uid),
        (uid, device_type), (uid, "B2500"),
    ]
    for parts in parts_list:
        if not all(parts):
            continue
        for sep in ("", "_", ":", "|"):
            joined = sep.join(parts).encode()
            for L, fn in [
                (16, lambda x: hashlib.md5(x).digest()),       # noqa: S324
                (16, lambda x: hashlib.sha1(x).digest()[:16]), # noqa: S324
                (16, lambda x: hashlib.sha256(x).digest()[:16]),
                (32, lambda x: hashlib.sha256(x).digest()),
            ]:
                yield from emit(
                    f"combo_hash[{L}]({parts!r},sep={sep!r})",
                    fn(joined),
                )


# ---------------------------------------------------------------------------
# 3. Scoring and PKCS#7
# ---------------------------------------------------------------------------

def score_plaintext(pt: bytes) -> float:
    """Higher = more plausible plaintext. Random bytes score near zero."""
    if not pt:
        return 0.0
    c = Counter(pt)
    H = -sum(v / len(pt) * math.log2(v / len(pt)) for v in c.values())
    printable = sum(1 for b in pt if 32 <= b < 127 or b in (0, 9, 10, 13))
    zeros = sum(1 for b in pt if b == 0)
    return (printable / len(pt)) - 0.3 * (H - 4) + 0.05 * (zeros / len(pt) * 10)


def pkcs7_unpad(pt: bytes) -> bytes | None:
    """Return unpadded bytes if PKCS#7 padding is valid, else None."""
    if not pt:
        return None
    n = pt[-1]
    if 1 <= n <= 16 and pt[-n:] == bytes([n]) * n:
        return pt[:-n]
    return None


# ---------------------------------------------------------------------------
# 4. Attack harness
# ---------------------------------------------------------------------------

def crack(b64: str, ctx: dict[str, str]) -> None:
    try:
        ct = base64.urlsafe_b64decode(b64)
    except Exception as e:
        print(f"  base64 decode failed: {e}")
        return

    print(f"  ciphertext: {len(ct)} bytes, len%16={len(ct) % 16}")
    if len(ct) == 0 or len(ct) % 16 != 0:
        print("  not a multiple of 16 bytes — skipping AES modes")
        return

    zero_iv = bytes(16)
    iv_first = ct[:16]

    best: list[tuple[float, str, str, bytes, bytes]] = []
    n_tried = 0
    for label, key in key_candidates(ctx):
        n_tried += 1
        try:
            candidates = [
                ("AES-ECB",              AES.new(key, AES.MODE_ECB).decrypt(ct)),
                ("AES-CBC(IV=0)",        AES.new(key, AES.MODE_CBC, zero_iv).decrypt(ct)),
                ("AES-CBC(IV=CT[0:16])", AES.new(key, AES.MODE_CBC, iv_first).decrypt(ct[16:])),
            ]
        except ValueError:
            continue
        for mode, pt in candidates:
            s = score_plaintext(pt) + (2.0 if pkcs7_unpad(pt) is not None else 0.0)
            best.append((s, mode, label, key, pt))

    print(f"  tried {n_tried} keys × 3 modes = {n_tried * 3} decryptions")

    best.sort(key=lambda x: -x[0])
    print("\n  top 5 by score:")
    for s, mode, label, key, pt in best[:5]:
        print(f"    score={s:6.3f}  {mode:22}  {label[:55]:55}  head={pt[:32]!r}")

    s, mode, label, key, pt = best[0]
    unpadded = pkcs7_unpad(pt)
    if s > 2.0 and unpadded is not None:
        print(f"\n  >>> HIT <<<  mode={mode}  key={key!r}  ({label})")
        text = unpadded.decode("utf-8", errors="replace")
        print(f"  plaintext ({len(unpadded)} bytes):")
        print("    " + text)
        fields = urllib.parse.parse_qsl(text, keep_blank_values=True)
        print(f"\n  parsed {len(fields)} fields:")
        for k, v in fields:
            print(f"    {k:12s} = {v}")
    else:
        print("\n  no clear winner — extend the candidate list or try more modes")


# ---------------------------------------------------------------------------
# 5. CLI
# ---------------------------------------------------------------------------

def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    ap.add_argument("pcaps", nargs="*", help="pcap file(s) to scan")
    ap.add_argument(
        "--b64",
        action="append",
        default=[],
        metavar="BASE64URL",
        help="base64url ciphertext payload (repeatable)",
    )
    args = ap.parse_args()

    jobs: list[tuple[str, dict[str, str]]] = []
    for pcap in args.pcaps:
        print(f"=== scanning {pcap} ===")
        found = payloads_from_pcap(pcap)
        print(f"  {len(found)} payload(s) found")
        jobs.extend(found)
    for payload in args.b64:
        jobs.append((payload, {"aid": "HMJ-2"}))

    if not jobs:
        ap.error("no payloads supplied — pass pcap files or --b64 <base64url>")

    for i, (b64, ctx) in enumerate(jobs, 1):
        print(f"\n--- payload #{i} (ctx={ctx}) ---")
        crack(b64, ctx)

    return 0


if __name__ == "__main__":
    sys.exit(main())
