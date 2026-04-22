#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["paho-mqtt>=1.6"]
# ///
"""Marstek B2500 read-only `cd=` probe.

Issues a set of bare `cd=N` queries against a live device over MQTT and
records every `device->app` payload seen on the control topic, diffing
each response against the steady-state `cd=0` baseline key set.

Safety rails (enforced, not optional):
  - Only ever sends bare `cd=N` payloads. No key=value writes, no scheduler
    updates, no mode changes, no OTA triggers, nothing with a comma or `=`
    inside the value list.
  - Dry-run by default. Pass `--i-know-what-im-doing` AND `--broker` to
    transmit.
  - Candidate list is a whitelist derived from firmware static analysis
    (http_setreport_fsm @ 0x08015d14). Only codes confirmed read-only or
    conditional-on-missing-sub-field are included. Codes that execute
    unconditional destructive actions (cd=10 = hard reboot, cd=11 = MQTT
    disconnect) are explicitly excluded and documented in the CANDIDATE_CODES
    comment block.
  - Unknown codes are rejected even if listed on the CLI.
  - Per-probe timeout and inter-probe cooldown.
  - Writes a detailed JSONL log to `docs/bms-probe-results.jsonl` and a
    human-readable summary to `docs/bms-probe-results.md`.

Usage:
    # Dry-run (no network) — generate the candidate list and exit:
    uv run scripts/probe_cd.py --dry-run

    # Live read-only probe (requires operator consent via the flag):
    uv run scripts/probe_cd.py \
        --broker 10.1.1.5 --port 1883 \
        --device aabbccddeeff \
        --topic-prefix hame_energy/HMJ-2 \
        --i-know-what-im-doing

Findings from static firmware analysis (see docs/bms-protocol.md):
    - The firmware dispatcher in http_setreport_fsm has ~20 builder IDs.
    - Known builders (found via format-string xref):
        builder 0x01 -> response format at 0x08027eb0 (superset of cd=0)
        builder 0x0D -> response format at 0x08028824 (48 calibration regs)
        builder 0x1C -> response format at 0x08028944 (CT meter state)
    - None of the three rich responses contain a per-cell voltage vector,
      coulomb counter, or SoH. The probe is therefore mainly useful to
      confirm the incoming `cd=` -> builder-ID mapping, NOT to unlock
      hidden per-cell data.
"""
from __future__ import annotations

import argparse
import json
import sys
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path

try:
    import paho.mqtt.client as mqtt
except ImportError:
    mqtt = None


CANDIDATE_CODES: list[str] = [
    # Read-only: firmware calls mqtt_setup_channel -> sends a response, no state change.
    "1", "01",   # 0x01 -> full settings snapshot
    "13",        # 0x0D -> calibration constants
    "14",        # 0x0E -> response (builder unknown)
    "15",        # 0x0F -> response (builder unknown)
    "16",        # 0x10 -> response (builder unknown)
    "21",        # 0x15 -> response (builder unknown)
    "28",        # 0x1C -> CT meter state
    # No handler in firmware -> returns immediately, true no-ops.
    "2", "6",
    "100", "101", "102",
    "200", "201", "205", "206", "207",
    "220", "221", "222", "223",
    # Conditional writes: firmware guards on a required sub-field (e.g. v=, sl=, sp=,
    # sm=). Bare cd=N without that field hits the strstr_find(…)==0 guard and returns
    # without writing anything. Listed last so they are easy to exclude via --codes.
    "3", "4", "5", "7", "8", "9", "12", "22",
    # DELIBERATELY EXCLUDED (unconditional writes — dangerous even as bare cd=N):
    #   "10"  -> 0x0A: reset_mqtt_connection() + MCU register write + infinite loop (hard reboot)
    #   "11"  -> 0x0B: AT+QMTCLOSE=0 + multiple cleanup calls (drops MQTT connection)
    #   "17"  -> 0x11: alias for cd=3 (writes config, guards on v= but same risk class)
    #   "18"  -> 0x12: alias for cd=4
    #   "19"  -> 0x13: alias for cd=5
    #   "20"  -> 0x14: alias for cd=7
    #   "23"  -> 0x17: sets charge thresholds (multi-field write, no safe bare form)
    #   "25"  -> 0x19: sets connection state machine flag
    #   "27"  -> 0x1B: writes CT meter/sequence config + save_calibration_result()
    #   "29"  -> 0x1D: single_mode + b2500_num -> mqtt_channel_stop + set_inverter_power_mode
    #   "31"  -> 0x1F: tc_dis flag write + enqueue_event(0x65,…)
    #   "33"  -> 0x21: OTA/WiFi provision (type= + SSID URL fields + at_modem_error_handler)
    #   "34"  -> 0x22: sets *DAT_08016efc (connection mode flag)
    #   "35"  -> 0x23: writes network IP config + save_calibration_result()
    #   "36"  -> 0x24: writes MQTT port config + save_calibration_result()
    #   "37"  -> 0x25: selects CT channel ID (write)
    #   "38"  -> 0x26: fktc flag write + handle_power_limit_exceeded()
    #   "39"  -> 0x27: CT type write + mqtt_setup_channel (also sets timing params)
    #   "55"  -> 0x37: battery enable/disable state machine (enable= 0/1/2/3)
    #   "81"  -> 0x51: firmware provisioning (hge_vid=/hge_gid=/hge_xid= fields)
]


@dataclass
class ProbeConfig:
    broker: str
    port: int
    device_id: str
    topic_prefix: str
    response_timeout_s: float = 2.0
    cooldown_s: float = 1.0
    dry_run: bool = True
    consent: bool = False
    codes: list[str] = field(default_factory=lambda: list(CANDIDATE_CODES))

    @property
    def command_topic(self) -> str:
        return f"{self.topic_prefix}/App/{self.device_id}/ctrl"

    @property
    def response_topic(self) -> str:
        return f"{self.topic_prefix}/device/{self.device_id}/ctrl"


@dataclass
class ProbeResult:
    code: str
    sent_at_iso: str
    response_payloads: list[str] = field(default_factory=list)

    def as_dict(self) -> dict:
        return {
            "code": self.code,
            "sent_at": self.sent_at_iso,
            "responses": self.response_payloads,
        }


def parse_args(argv: list[str]) -> ProbeConfig:
    p = argparse.ArgumentParser(
        description="Read-only cd= probe for the Marstek B2500 BMS.",
    )
    p.add_argument("--broker", help="MQTT broker host (e.g. 10.1.1.5)")
    p.add_argument("--port", type=int, default=1883)
    p.add_argument("--device", dest="device_id", help="Device id / MAC (no colons)")
    p.add_argument("--topic-prefix", default="hame_energy/HMJ-2")
    p.add_argument("--response-timeout", type=float, default=2.0)
    p.add_argument("--cooldown", type=float, default=1.0)
    p.add_argument(
        "--dry-run",
        action="store_true",
        help="Do not connect; print the plan and exit.",
    )
    p.add_argument(
        "--i-know-what-im-doing",
        dest="consent",
        action="store_true",
        help="Explicit consent flag required for live transmission.",
    )
    p.add_argument(
        "--codes",
        nargs="+",
        default=None,
        help="Restrict to the given candidate codes (must be in whitelist).",
    )
    args = p.parse_args(argv)

    if args.codes:
        bad = [c for c in args.codes if c not in CANDIDATE_CODES]
        if bad:
            p.error(f"codes not in whitelist: {bad}")
        codes = args.codes
    else:
        codes = list(CANDIDATE_CODES)

    if not args.dry_run and (not args.broker or not args.device_id):
        p.error("--broker and --device are required when not --dry-run")

    return ProbeConfig(
        broker=args.broker or "",
        port=args.port,
        device_id=args.device_id or "",
        topic_prefix=args.topic_prefix,
        response_timeout_s=args.response_timeout,
        cooldown_s=args.cooldown,
        dry_run=args.dry_run,
        consent=args.consent,
        codes=codes,
    )


def validate_payload(code: str) -> bytes:
    """Enforce that the payload is *exactly* `cd=N` and nothing else.

    Any comma, equals sign past the first, or non-digit after `cd=`
    is rejected. This guarantees we never accidentally emit a write.
    """
    if code not in CANDIDATE_CODES:
        raise ValueError(f"code not whitelisted: {code!r}")
    payload = f"cd={code}"
    if payload.count("=") != 1 or "," in payload:
        raise ValueError(f"payload failed safety check: {payload!r}")
    if not code.isdigit() and code != "01":
        raise ValueError(f"non-numeric code: {code!r}")
    return payload.encode("ascii")


def run_probe(cfg: ProbeConfig, out_dir: Path) -> list[ProbeResult]:
    if cfg.dry_run:
        print("[dry-run] Would connect to", cfg.broker, cfg.port)
        print("[dry-run] Command topic :", cfg.command_topic)
        print("[dry-run] Response topic:", cfg.response_topic)
        for c in cfg.codes:
            payload = validate_payload(c)
            print(f"[dry-run] would publish -> {cfg.command_topic}  body={payload!r}")
        return []

    if not cfg.consent:
        raise SystemExit(
            "refusing to transmit without --i-know-what-im-doing; "
            "re-run with --dry-run to see the plan first"
        )
    if mqtt is None:
        raise SystemExit("paho-mqtt not installed; `pip install paho-mqtt`")

    out_dir.mkdir(parents=True, exist_ok=True)
    results: list[ProbeResult] = []
    current_bucket: list[str] = []
    bucket_lock = threading.Lock()

    client = mqtt.Client(client_id=f"probe-{int(time.time())}")

    def on_message(_cli, _userdata, msg):
        try:
            decoded = msg.payload.decode("utf-8", errors="replace")
        except Exception:
            decoded = repr(msg.payload)
        with bucket_lock:
            current_bucket.append(decoded)

    client.on_message = on_message
    client.connect(cfg.broker, cfg.port, keepalive=30)
    client.subscribe(cfg.response_topic, qos=0)
    client.loop_start()

    try:
        for code in cfg.codes:
            payload = validate_payload(code)
            with bucket_lock:
                current_bucket.clear()
            sent_at = datetime.now(timezone.utc).isoformat()
            client.publish(cfg.command_topic, payload, qos=0)
            print(f"[probe] cd={code} sent at {sent_at}", flush=True)
            time.sleep(cfg.response_timeout_s)
            with bucket_lock:
                snapshot = list(current_bucket)
            results.append(ProbeResult(code=code, sent_at_iso=sent_at, response_payloads=snapshot))
            time.sleep(cfg.cooldown_s)
    finally:
        client.loop_stop()
        client.disconnect()

    return results


def write_report(results: list[ProbeResult], out_dir: Path) -> None:
    out_dir.mkdir(parents=True, exist_ok=True)
    jsonl_path = out_dir / "bms-probe-results.jsonl"
    md_path = out_dir / "bms-probe-results.md"

    with jsonl_path.open("w", encoding="utf-8") as fh:
        for r in results:
            fh.write(json.dumps(r.as_dict()) + "\n")

    baseline_keys = _derive_baseline_keys(results)
    with md_path.open("w", encoding="utf-8") as fh:
        fh.write("# BMS cd= probe results\n\n")
        fh.write("Probe run at " + datetime.now(timezone.utc).isoformat() + "\n\n")
        fh.write(f"Baseline cd=0 key set (derived from first response): "
                 f"{len(baseline_keys)} keys\n\n")
        for r in results:
            fh.write(f"## cd={r.code}\n\n")
            if not r.response_payloads:
                fh.write("_no response within timeout_\n\n")
                continue
            for i, body in enumerate(r.response_payloads):
                new_keys = _diff_keys(body, baseline_keys)
                fh.write(f"- response[{i}] ({len(body)} B); new keys: "
                         f"{sorted(new_keys) if new_keys else '(none)'}\n")
                fh.write(f"  ```\n  {body[:400]}\n  ```\n")
            fh.write("\n")


def _derive_baseline_keys(results: list[ProbeResult]) -> set[str]:
    for r in results:
        if r.code in ("1", "01"):
            for body in r.response_payloads:
                return _extract_keys(body)
    return set()


def _extract_keys(body: str) -> set[str]:
    keys: set[str] = set()
    for part in body.split(","):
        part = part.strip()
        if "=" not in part:
            continue
        k = part.split("=", 1)[0]
        keys.add(k)
    return keys


def _diff_keys(body: str, baseline: set[str]) -> set[str]:
    return _extract_keys(body) - baseline


def main(argv: list[str]) -> int:
    cfg = parse_args(argv)
    repo_root = Path(__file__).resolve().parent.parent
    out_dir = repo_root / "docs"
    results = run_probe(cfg, out_dir)
    if results:
        write_report(results, out_dir)
        print(f"wrote {out_dir/'bms-probe-results.md'}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
