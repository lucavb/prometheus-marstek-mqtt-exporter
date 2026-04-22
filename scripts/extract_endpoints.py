#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Exhaustive endpoint extraction from the B2500-D firmware.

Pulls every printable-ASCII run of length >= 4, then filters for anything
that looks network-addressable: URLs, bare hosts, path fragments, MQTT
topic templates, AT-command `AT+QMTOPEN`/`AT+QHTTPCFG` format strings,
IPv4 literals.

Run with:  uv run scripts/extract_endpoints.py firmware/B2500_All_HMJ.bin
"""
from __future__ import annotations

import re
import sys
from pathlib import Path


MIN_LEN = 4


def ascii_strings(data: bytes, minlen: int = MIN_LEN):
    buf = bytearray()
    start = 0
    for i, b in enumerate(data):
        if 0x20 <= b < 0x7F:
            if not buf:
                start = i
            buf.append(b)
        else:
            if len(buf) >= minlen:
                yield start, buf.decode("ascii")
            buf.clear()
    if len(buf) >= minlen:
        yield start, buf.decode("ascii")


HOST_RE = re.compile(r"(?:[a-z0-9-]+\.)+(?:com|net|cn|org|io|php)\b", re.I)
PATH_RE = re.compile(r"/(?:app|ems|prod|ota|api|v1|v2|v3|Solar|neng|apk|uploads)/[A-Za-z0-9_./?=&%{}:<>-]*")
IP_RE = re.compile(r"\b(?:\d{1,3}\.){3}\d{1,3}\b")
URL_RE = re.compile(r"https?://[A-Za-z0-9._~:/?#\[\]@!$&'()*+,;=%{}<>-]+", re.I)
TOPIC_RE = re.compile(r"\b(?:hame_energy|marstek_energy)/[^ \"\\]{1,80}")
AT_NET_RE = re.compile(
    r"AT\+(?:QMTOPEN|QMTCONN|QMTCFG|QMTSUB|QMTPUB|QHTTPCFG|QIOPEN|QSTAAPINFODEF|QSSLCFG|QSSLCERT|QWLANOTA)[^\r\n\x00]*",
    re.I,
)
JSONRPC_RE = re.compile(r'\{"id":\d+,"method":"[A-Za-z0-9_.]+","params":\{[^}]{0,80}\}\}')


def main() -> int:
    path = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("firmware/B2500_All_HMJ.bin")
    data = path.read_bytes()

    urls: dict[str, int] = {}
    hosts: dict[str, int] = {}
    paths: dict[str, int] = {}
    ips: dict[str, int] = {}
    topics: dict[str, int] = {}
    at_cmds: dict[str, int] = {}
    rpcs: dict[str, int] = {}

    for off, s in ascii_strings(data, 4):
        for m in URL_RE.finditer(s):
            urls.setdefault(m.group().strip(), off + m.start())
        for m in HOST_RE.finditer(s):
            h = m.group().lower().strip()
            if h and h not in ("hm.com", "a.com"):
                hosts.setdefault(h, off + m.start())
        for m in PATH_RE.finditer(s):
            p = m.group().strip()
            if len(p) >= 6:
                paths.setdefault(p, off + m.start())
        for m in IP_RE.finditer(s):
            ip = m.group()
            octs = [int(x) for x in ip.split(".")]
            if all(0 <= o <= 255 for o in octs) and ip not in ("0.0.0.0", "255.255.255.255", "1.1.1.1"):
                ips.setdefault(ip, off + m.start())
        for m in TOPIC_RE.finditer(s):
            topics.setdefault(m.group().strip(), off + m.start())
        for m in AT_NET_RE.finditer(s):
            at_cmds.setdefault(m.group().strip(), off + m.start())
        for m in JSONRPC_RE.finditer(s):
            rpcs.setdefault(m.group(), off + m.start())

    def dump(title: str, d: dict[str, int]) -> None:
        print(f"\n=== {title} ({len(d)}) ===")
        for k in sorted(d):
            print(f"  0x{d[k]:06x}  {k}")

    dump("Full URLs", urls)
    dump("Host names", hosts)
    dump("URL path fragments", paths)
    dump("IPv4 literals", ips)
    dump("MQTT topic templates", topics)
    dump("Networking AT commands (Quectel)", at_cmds)
    dump("JSON-RPC requests (LAN)", rpcs)
    return 0


if __name__ == "__main__":
    sys.exit(main())
