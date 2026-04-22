# Roadmap — open reverse-engineering leads

Things we have **not** figured out yet about the Marstek B2500 but could,
given the assets already in this repo (decompiled firmware in Ghidra, the
`MARSTEK_1.6.61_APKPure.xapk`, six `marstek*.pcap` captures, decrypted cert/key
blobs under `firmware/decrypted/`, and a working emulator + exporter).

Grouped by theme and rough payoff. Items marked with `★` are the highest-value
next steps.

## Top three to do next

In order of bang-for-buck:

1. **Decompile the APK** (`MARSTEK_1.6.61_APKPure.xapk`) with JADX and cross-
   reference its strings against firmware. Single action that unlocks the MQTT
   control command set, scheduler JSON schema, human-readable event names, and
   any undocumented cloud endpoints — see items 1, 2, 14 below.
2. **Map the NVS / flash config layout and the OTA mechanism** (items 9 + 10).
   Turns the device from "black box" into a documented embedded system and
   enables config backup/restore tooling.
3. **Grafana dashboard + Prometheus alert rules on the 49 event codes we just
   mapped** (items 16 + 17). Immediate user-visible payoff and a natural
   regression testbed for future firmware changes.

---

## Protocol reverse engineering — the biggest remaining gap

1. **★ The MQTT `cd=1` control command set.** We parse telemetry (`cd=1/2/3`
   outbound), but the *inbound* writes on `hame_energy/HMJ-2/App/<mac>/ctrl`
   are undocumented. The APK has the full dictionary. Cross-reference APK
   JSON keys against firmware's MQTT command dispatcher; build the emulator
   so it can *accept* commands, not just emit telemetry.
2. **★ Time-slot scheduler encoding.** The "three charge/discharge windows
   with target watts + SoC floor" feature is a packed struct in NVS.
   Decode it → render in Grafana → generate from Home Assistant based on
   Tibber/EPEX price signals. This is the single most-asked-about setting.
3. **Auxiliary MQTT session.** Codes 74, 78, 85 proved there is a *secondary*
   MQTT client used by `http_getdateinfo_fsm`. What broker, what topics, what
   payloads? Candidates: OTA push notifications, server-side commands, or
   duplicated telemetry for the cloud UI.
4. **BLE provisioning protocol.** First-boot WiFi setup is Bluetooth. Map
   the GATT service UUIDs, characteristics, and command framing — enables
   headless provisioning and re-provisioning without factory reset.
5. **Shelly CT ↔ battery bidirectional path.** Code 33 (`shelly_ct_meter_status`)
   gave us the *read* side. The *write* side — how grid-import readings
   modulate the discharge setpoint — is the zero-export control loop and the
   piece most directly replaceable by Home Assistant if we understand it.

## Cloud / auth

6. **Device-binding secret derivation.** Cloud POST bodies are signed.
   Figure out how the per-device HMAC key is derived (probably MAC + salt).
   Tells us whether (a) the emulator's signatures are accepted by the real
   cloud and (b) a cloned device ID would work at all.
7. **Full `eu.hamedata.com` endpoint inventory.** `scripts/extract_endpoints.py`
   already has partial results. Re-run against firmware `.rodata` + the APK,
   diff against what the emulator handles, and document the gap (OTA manifest
   URL, account bind/unbind, firmware list, push-notification endpoint).
8. **Identify the three decrypted cert/key blobs** under `firmware/decrypted/`
   (`026820.cert.pem`, `026e7c.key.pem`, `02776c.cert.pem`). Match by CN/SAN
   and by xref in firmware: which is the MQTT mTLS client cert, which is the
   HTTPS pinning cert, which (if any) verifies OTA payload signatures.

## Firmware / OTA / flash

9. ~~**★ OTA update mechanism.**~~ **Done (2026-04-22).** BLE-only (phone app
   streams image), unauthenticated, no pairing required, integrity = `~sum()`
   byte-sum (trivially forgeable), no cryptographic signature, no anti-rollback.
   Any BLE client within ~10 m can flash arbitrary firmware. Full write-up in
   [`docs/ota.md`](ota.md).
10. **★ NVS / flash config layout.** We have one 508-byte region mapped
    (the `errinfo_ring` at the pointer referenced by `g_errinfo_ring_ptr`).
    Still unmapped: WiFi credentials block, MQTT broker override, cloud token,
    scheduler struct, user account binding. Full flash map enables backup and
    restore tooling.
11. **Bootloader / debug interfaces.** `uboot_version` is tracked separately
    from the app image — is there a serial console on a UART pad? Is
    JTAG/SWD enabled? If yes, we can dump live RAM and watch FSMs run
    instead of inferring them from decompilation.

## The BMS (mostly decoded — see [`bms-protocol.md`](bms-protocol.md))

12. ~~**★ Main MCU ↔ BMS UART/I²C protocol.**~~ **Done (2026-04-22).** Bus
    identified as **I²C2 @ `0x40005800`, slave `0x40`**, frame format
    `[0x34][cmd][len][payload...][0x35]` with tail checksum. Separately, a
    GPIO bit-bang link on PB3/PB4/PB5 carries a one-shot boot pack-identity
    probe (commands `0x81..0x8D`). **Finding that closed this item: per-cell
    voltages, coulomb counts, and SoH are NOT exchanged on any software-visible
    surface** — the BMS only exposes pack-level min/max summaries. See the
    protocol doc for the Ghidra symbols, command table, and the hardware-tap
    path required to unlock per-cell data.
13. ~~**Charging algorithm internals.**~~ **Done (2026-04-22).** Extracted
    from `battery_charge_monitor` / `battery_discharge_monitor`: 16.000 V
    normal-mode and 30.000 V extended over-voltage ceilings, 12.000 V
    taper threshold, 500 mA taper/re-arm current gate, 60 s over-voltage
    debounce, 1 s undervoltage debounce, 3 s re-arm debounce. CC/CV per-cell
    cut-offs and balance thresholds live inside BMS-side firmware and are
    **not** present as literals on the main MCU — confirmed by exhaustive
    xref sweep around the I²C dispatcher. See `bms-protocol.md` §"Charging
    algorithm literals".

**New follow-up surfacing from 12 + 13:**

12a. **BMS hardware tap on I²C2 (PB10/PB11).** The only remaining route
     to per-cell voltages / coulomb / SoH. 100–400 kHz logic-analyzer
     capture, frame-boundary parsing per `bms-protocol.md`, match the
     autonomous push stream against the already-decoded
     `b{0,1,2}{max,min}` cloud fields. Out of scope for the software-only
     exporter, but documented so the work isn't lost.

## Integrations / the Android app

14. **★ Decompile `MARSTEK_1.6.61_APKPure.xapk`** (JADX on the inner APKs).
    Probably the single highest-ROI remaining action. Gives us named command
    constants, the scheduler JSON schema, human-readable error strings
    (cross-check vs our 49 codes!), and undocumented cloud endpoints.
15. **Jupiter / Venus family compatibility.** Is B2500 firmware shared with
    the larger Jupiter battery or the Venus inverter? The `device_type`
    byte gating is all over the firmware — mapping it tells us whether this
    exporter covers the whole Marstek product line for free.

## Observability / delivery — the payoff layer

16. **★ Grafana dashboard using the new metrics.** 49 named events ×
    per-code counters × last-seen timestamps → "device health heatmap" plus
    a "recent events" table, driven by the dictionary in
    `emulator/solar_errinfo_codes.go`. The `grafana-dashboard-with-metrics`
    skill is designed for exactly this.
17. **★ Prometheus alert rules** for event-code patterns. Now that we know
    which codes are benign-periodic vs actual-fault, the rules are one-liners:
    `wifi_disconnect` bursts, sustained `mqtt_ext_conn_failed`, any BMS
    fault code within 10 minutes, etc.
18. **Pcap replay regression test.** The six `marstek*.pcap` files are gold.
    Wire them into a test harness that replays real device traffic through
    the emulator and asserts the exporter produces stable metrics —
    prevents silent regressions when firmware changes.

## Security / "can I trust this box on my LAN"

19. **LAN-side listeners.** Does the device open any ports on the local
    network (hidden HTTP config page, UPnP, mDNS)? `nmap` against it,
    cross-checked against firmware `bind()` / `listen()` call sites, gives
    a concrete answer.
20. **Cloud-detached operation.** The emulator is one data point. But does
    the real device *degrade gracefully* when cloud is unreachable — or does
    it throttle or refuse to charge after a timeout? This is the
    "self-hosted peace of mind" question and determines whether the emulator
    is truly optional at runtime.

---

_Last updated: 2026-04-22. See [`firmware.md`](firmware.md) for the state of
the Ghidra analysis that underpins most of the above._
