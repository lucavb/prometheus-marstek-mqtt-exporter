#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["capstone>=5.0"]
# ///
"""Static analysis of the B2500-D (HMJ) firmware image.

Run with: uv run scripts/analyze_firmware.py firmware/B2500_All_HMJ.bin
"""
from __future__ import annotations

import argparse
import struct
import sys
from collections import Counter
from pathlib import Path

from capstone import Cs, CS_ARCH_ARM, CS_MODE_THUMB, CS_MODE_MCLASS


FLASH_BASE = 0x08000000
SRAM_BASE = 0x20000000


def load(path: Path) -> bytes:
    data = path.read_bytes()
    print(f"[+] Loaded {path} ({len(data)} bytes)")
    return data


def entropy(block: bytes) -> float:
    if not block:
        return 0.0
    counts = Counter(block)
    total = len(block)
    import math

    return -sum((c / total) * math.log2(c / total) for c in counts.values())


def print_vector_table(data: bytes) -> tuple[int, int]:
    print("\n=== Cortex-M vector table ===")
    names = [
        "Initial SP",
        "Reset",
        "NMI",
        "HardFault",
        "MemManage",
        "BusFault",
        "UsageFault",
        "Reserved",
        "Reserved",
        "Reserved",
        "Reserved",
        "SVCall",
        "DebugMon",
        "Reserved",
        "PendSV",
        "SysTick",
    ]
    vt = struct.unpack_from("<16I", data, 0)
    for n, v in zip(names, vt):
        tag = ""
        if n == "Initial SP":
            tag = f"  (SRAM top; {v - SRAM_BASE} B RAM visible)"
        elif v & 1 and FLASH_BASE <= (v & ~1) < FLASH_BASE + len(data):
            tag = "  -> thumb handler in flash"
        print(f"  {n:12s}  0x{v:08x}{tag}")
    reset = vt[1] & ~1
    init_sp = vt[0]
    return init_sp, reset


def find_irq_vectors(data: bytes) -> int:
    """Count valid-looking thumb vectors after the CPU vector table (offset >= 0x40)."""
    count = 0
    off = 0x40
    while off + 4 <= len(data):
        v = struct.unpack_from("<I", data, off)[0]
        if v == 0 or v == 0xFFFFFFFF:
            break
        if not (v & 1) or (v & ~1) < FLASH_BASE or (v & ~1) >= FLASH_BASE + len(data):
            break
        count += 1
        off += 4
    return count


def scan_sections(data: bytes, block: int = 1024) -> None:
    print(f"\n=== Entropy map (per {block}-byte block) ===")
    runs = []  # (start, end, kind)
    cur_kind = None
    cur_start = 0
    for i in range(0, len(data), block):
        chunk = data[i : i + block]
        e = entropy(chunk)
        if all(b == 0xFF for b in chunk):
            kind = "erased"
        elif all(b == 0x00 for b in chunk):
            kind = "zero"
        elif e > 7.5:
            kind = "high-entropy"
        elif e > 5.5:
            kind = "code/mixed"
        else:
            kind = "low-entropy/data"
        if kind != cur_kind:
            if cur_kind is not None:
                runs.append((cur_start, i, cur_kind))
            cur_start = i
            cur_kind = kind
    runs.append((cur_start, len(data), cur_kind))
    for s, e, k in runs:
        print(f"  0x{s:06x} - 0x{e:06x}  ({e - s:6d} B)  {k}")


def extract_strings(data: bytes, minlen: int = 5) -> list[tuple[int, str]]:
    out: list[tuple[int, str]] = []
    buf = bytearray()
    start = 0
    for i, b in enumerate(data):
        if 0x20 <= b < 0x7F:
            if not buf:
                start = i
            buf.append(b)
        else:
            if len(buf) >= minlen:
                out.append((start, buf.decode("ascii", errors="replace")))
            buf.clear()
    if len(buf) >= minlen:
        out.append((start, buf.decode("ascii", errors="replace")))
    return out


def categorize_strings(strs: list[tuple[int, str]]) -> None:
    print(f"\n=== Strings ({len(strs)} total, >=5 printable ASCII) ===")

    buckets: dict[str, list[tuple[int, str]]] = {
        "URL / host":         [],
        "MQTT topic":         [],
        "Format string":      [],
        "JSON key":           [],
        "AT command":         [],
        "Version / build":    [],
        "Error / debug":      [],
        "Other":              [],
    }
    url_kw = ("http://", "https://", "hamedata", ".com", ".net", ".cn", "://")
    mqtt_kw = ("hame_energy", "/device/", "/ctrl/", "/App/", "/ota", "HM-", "hm_", "/#")
    fmt_kw = ("%d", "%s", "%02x", "%x", "%u", "%f", "\\n", "\\r")
    json_kw = ('"', "{", "}", ":", ",")
    at_kw = ("AT+", "AT\r", "+OK", "+ERR")
    ver_kw = ("V1", "v1", "V2", "v2", "Version", "version", "build", "Build", "B2500", "HMJ", "HMA", "HMB", "HMK", "__DATE__", "gcc", "GCC")
    err_kw = ("ERR", "err", "fail", "Fail", "FAIL", "assert", "Assert", "warn", "Warn")

    for off, s in strs:
        low = s.lower()
        if any(k in low for k in url_kw):
            buckets["URL / host"].append((off, s))
        elif any(k in s for k in mqtt_kw):
            buckets["MQTT topic"].append((off, s))
        elif any(k in s for k in at_kw):
            buckets["AT command"].append((off, s))
        elif any(k in s for k in ver_kw):
            buckets["Version / build"].append((off, s))
        elif any(k in s for k in fmt_kw):
            buckets["Format string"].append((off, s))
        elif any(k in s for k in err_kw):
            buckets["Error / debug"].append((off, s))
        elif s.count('"') >= 2 or (s.startswith("{") and s.endswith("}")):
            buckets["JSON key"].append((off, s))
        else:
            buckets["Other"].append((off, s))

    for name, items in buckets.items():
        if not items:
            continue
        print(f"\n-- {name} ({len(items)}) --")
        shown = items if name != "Other" else items[:40]
        for off, s in shown:
            print(f"  0x{off:06x}  {s!r}")
        if name == "Other" and len(items) > 40:
            print(f"  ... +{len(items) - 40} more")


def find_sbox(data: bytes) -> list[int]:
    # The canonical forward AES S-box starts with 63 7c 77 7b
    sig = bytes.fromhex("637c777bf26b6fc53001672bfed7ab76")
    hits = []
    i = 0
    while True:
        i = data.find(sig, i)
        if i < 0:
            break
        hits.append(i)
        i += 1
    # inverse S-box starts with 52 09 6a d5
    inv_sig = bytes.fromhex("52096ad53036a538bf40a39e81f3d7fb")
    i = 0
    while True:
        i = data.find(inv_sig, i)
        if i < 0:
            break
        hits.append(("inv", i))
        i += 1
    return hits


def disassemble_reset(data: bytes, init_sp: int, reset: int, count: int = 60) -> None:
    print(f"\n=== Disassembly around reset handler (0x{reset:08x}) ===")
    md = Cs(CS_ARCH_ARM, CS_MODE_THUMB + CS_MODE_MCLASS)
    md.detail = True
    file_off = reset - FLASH_BASE
    if not (0 <= file_off < len(data)):
        print("  reset vector out of range")
        return
    code = data[file_off : file_off + 4 * count]
    for i, insn in enumerate(md.disasm(code, reset)):
        print(f"  0x{insn.address:08x}  {insn.mnemonic:<6s} {insn.op_str}")
        if i >= count:
            break


def find_rom_table(data: bytes) -> None:
    """STM32 devices typically store the device ID register address / unique ID pointer.
    A reliable fingerprint: references to 0xE0042000 (DBGMCU_IDCODE) or
    0xE000ED00 (SCB CPUID).
    """
    print("\n=== Hardware fingerprints (hex refs to well-known MMIO) ===")
    candidates = {
        0xE000ED00: "SCB CPUID",
        0xE0042000: "STM32 DBGMCU_IDCODE",
        0x40023800: "STM32F4 RCC",
        0x40021000: "STM32F1/F3 RCC",
        0x50000000: "USB OTG FS (STM32F4)",
        0x40013000: "STM32 USART1",
        0x40010000: "STM32F1 AFIO",
        0x1FFFF7E0: "STM32F1 flash size reg",
        0x1FFF7A10: "STM32F4 Unique ID",
        0x1FFFF7AC: "STM32F1 Unique ID",
        0x40003800: "STM32 SPI2",
        0x42440000: "Nuvoton (bit-band)",
        0x50000200: "Nuvoton SYS_BA",
        0x50000100: "Nuvoton CLK_BA",
        0x40004000: "Nuvoton / generic",
    }
    found: list[tuple[int, int, str]] = []
    for off in range(0, len(data) - 3, 4):
        w = struct.unpack_from("<I", data, off)[0]
        if w in candidates:
            found.append((off, w, candidates[w]))
    for off, w, name in found[:40]:
        print(f"  file+0x{off:06x}  ->  0x{w:08x}  ({name})")
    if len(found) > 40:
        print(f"  ... +{len(found) - 40} more")
    if not found:
        print("  (no hits among common MMIO targets; MCU may be non-STM32)")


def summarise(data: bytes) -> None:
    print("\n=== Summary ===")
    used = sum(1 for b in data if b != 0xFF)
    print(f"  image size       : {len(data)} B  ({len(data)/1024:.1f} KiB)")
    print(f"  non-0xFF bytes   : {used} ({100*used/len(data):.1f}%)")
    print(f"  0x00 byte count  : {data.count(0):d}")
    print(f"  last 16 bytes    : {data[-16:].hex()}")
    print(f"  first 16 bytes   : {data[:16].hex()}")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("path", type=Path)
    ap.add_argument("--max-strings", type=int, default=5)
    args = ap.parse_args()

    data = load(args.path)
    summarise(data)
    init_sp, reset = print_vector_table(data)
    n_irq = find_irq_vectors(data)
    print(f"\n  device IRQ vectors after VT: ~{n_irq}")

    scan_sections(data)

    sbox_hits = find_sbox(data)
    if sbox_hits:
        print(f"\n=== AES tables ===")
        for hit in sbox_hits:
            if isinstance(hit, tuple):
                print(f"  inverse S-box @ 0x{hit[1]:06x}")
            else:
                print(f"  forward S-box @ 0x{hit:06x}")

    find_rom_table(data)

    strs = extract_strings(data, minlen=args.max_strings)
    categorize_strings(strs)

    disassemble_reset(data, init_sp, reset, count=40)

    return 0


if __name__ == "__main__":
    sys.exit(main())
