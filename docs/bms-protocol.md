# Marstek B2500-D — BMS protocol reverse engineering

This note is the consolidated result of the "Main MCU ↔ BMS" RE pass
(roadmap items 12 + 13). It is purely the result of **static analysis on
`firmware/B2500_All_HMJ.bin`** (via the Ghidra MCP) plus **pcap audit**
of the six `marstek*.pcap` captures. No hardware access, no live probes.

TL;DR — the single most important finding:

> **The firmware does NOT carry per-cell voltages, coulomb-counter raw
> samples, or an SoH percentage over any MQTT, HTTP, or BLE channel we
> have been able to reach from software.** The wire protocol only
> exposes per-pack min/max cell voltages, an aggregate pack voltage,
> SoC, and a single pack temperature. Every on-demand "rich" response
> we found (`cd=221`, `cd=206`, `cd=222`) adds settings, calibration
> constants, or CT-meter status — none of them contain per-cell detail.
> Unlocking per-cell data therefore requires a **hardware UART tap on
> the BMS I²C bus** (§ "Hardware-tap follow-up" below).

Phase 0 of the plan (exposing the BMS-adjacent fields that the device
already emits but the exporter was dropping) shipped in the same pass.
See `README.md` §Metrics for the new gauges.

## Architectural summary

Three separate "BMS-adjacent" communication surfaces exist in the HMJ
firmware, and they are easy to confuse:

1. **Bit-banged GPIOB serial** — one-shot boot handshake. Used only by
  `battery_pack_init_fsm` to read 6 pack-identity registers. This is
   the "BMS probe" mentioned by event codes 25 and 81. **It does not
   carry live telemetry.**
2. **I²C2 @ `0x40005800`, BMS address `0x40`** — the live BMS bus. Used
  by `battery_data_poll_fsm` (every 500 ms) and `battery_cell_fault_handler`
   (fault-clear writes). This is what fires event codes 73 / 82 / 95.
   The **reply payload is NOT key-value MQTT text — it's a binary frame
   over I²C that the firmware already decodes into the per-pack summary
   fields** exposed in `cd=0` / `setB2500Report` (`b0max`, `b0min`,
   `b0maxn`, `b0minn`, etc).
3. **Host-side on-chip ADC** — pack voltage, pack current, and pack
  temperature all read through the STM32's own ADC peripherals. Not
   part of this write-up.

The three surfaces, with the Ghidra addresses where each one is
implemented:


| Surface           | Entry function                  | Addr         | Peripheral          | Used by                                                                    |
| ----------------- | ------------------------------- | ------------ | ------------------- | -------------------------------------------------------------------------- |
| Bit-banged serial | `bms_bitbang_xfer_byte`         | `0x0801b3a8` | GPIOB (PB3/4/5)     | `battery_pack_init_fsm` only (boot probe)                                  |
| I²C2 master       | `bms_i2c_xfer` (`FUN_08023d5c`) | `0x08023d5c` | I²C2 @ `0x40005800` | `battery_data_poll_fsm` (poll), `battery_cell_fault_handler` (fault clear) |
| ADC (host-side)   | (not RE'd in this pass)         |              | ADC1/ADC2           | pack voltage, current, temperature                                         |


## Surface 1 — bit-banged GPIOB pack-identity probe

Discovered during Phase 1 of the plan.

**Pinout** (derived from decompiled `gpio_configure_input`,
`gpio_configure_output_pp`, and the port-ptr label
`g_bms_gpio_port_ptr_init` at `0x0801b3a0`):


| STM32 pin | Role                     |
| --------- | ------------------------ |
| PB3       | Chip select (active-low) |
| PB4       | Bidirectional data       |
| PB5       | Clock                    |


All three sit on **GPIOB @ `0x40010C00`**. `gpio_set_pin` / `gpio_clear_pin`
/ `gpio_read_pin` / `bitbang_delay_short` together implement a
software-timed half-duplex frame — 8 command bits out MSB=0 LSB-first,
then 8 reply bits in.

**Command table** — 7 call sites in total, all inside
`battery_pack_init_fsm`:


| Cmd    | Meaning                   | Threshold for event 82 (cell-fault)                                                            |
| ------ | ------------------------- | ---------------------------------------------------------------------------------------------- |
| `0x81` | Primary probe / handshake | value ≤ `0x60`                                                                                 |
| `0x83` | Pack-ID register 1        | value ≤ `0x60`                                                                                 |
| `0x85` | Pack-ID register 2        | value ≤ `0x24` (36)                                                                            |
| `0x87` | Pack-ID register 3        | value ≤ `0x31` (49)                                                                            |
| `0x89` | Pack-ID register 4        | value ≤ `0x12` (18) — documented as `battery_pack_init_cell_fault` in `solar_errinfo_codes.go` |
| `0x8D` | Pack-ID register 5        | value ≤ `0x50` (80)                                                                            |


Each reply is a single byte interpreted as packed BCD
(`(hi_nibble * 10) + lo_nibble`). The six values are stored in
`DAT_0801b5c0..DAT_0801b5c8` as the runtime **pack signature**; they
are NOT per-cell voltages or coulomb counters.

Failure modes:

- `bms_bitbang_xfer_byte(0x81) == 0 || == 0xFF` → `enqueue_event(81, …)`
(`battery_pack_init_no_response`)
- any register exceeds its threshold → `enqueue_event(82, raw_byte)`
(`battery_pack_init_cell_fault`)

## Surface 2 — I²C2 runtime BMS bus

Discovered during Phase 2 of the plan. This is the active bus.

**Peripheral**: STM32F1 **I²C2**, MMIO base `0x40005800`
(label `g_bms_i2c2_base_ptr` at `0x08023fd4`). Slave address `0x40`
(shifted; raw 7-bit address `0x20`). Other globals around
`0x08023fd4`:


| Addr         | Value        | Role (inferred)                                                   |
| ------------ | ------------ | ----------------------------------------------------------------- |
| `0x08023fd4` | `0x40005800` | I²C2 peripheral base                                              |
| `0x08023fd8` | `0x00030001` | Combined SR1/SR2 flag mask for "Start Bit + Master/Busy"          |
| `0x08023fdc` | `0x00070082` | Flag mask for "Address Sent / TRA / MSL / BUSY" (master-transmit) |
| `0x08023fe0` | `0x10000002` | Flag mask for "BTF / ADDR", end-of-transfer                       |


**Frame format** (from `bms_i2c_xfer` at `0x08023d5c` + tail-of-function
verification loop):

```
+----+-----+-----+----- payload (len bytes) -----+----+
| 34 | cmd | len |                               | 35 |
+----+-----+-----+-------------------------------+----+
     ^        payload bytes are read from I²C          ^
     |                                                  |
     start-of-frame sentinel (0x34, "4")               end-of-frame sentinel (0x35, "5")
```

The sentinels `0x34` / `0x35` are literal bytes, verified against a
reference frame via `FUN_0800b518` (a simple byte-sum or CRC-style
check; exact polynomial not yet confirmed). The I²C transaction is
a standard STM32 master sequence: `BUSY` wait → `START` → write address
with W-bit → ACK check → write command → read `len` + N bytes → `STOP`.

**Known command codes** — only two are called from the runtime
firmware, both by `bms_i2c_queue_cmd` (`FUN_08024198`) via
`bms_clear_fault` (`FUN_08022510`):


| Cmd    | Issued by                                                 | Meaning (inferred)                                              |
| ------ | --------------------------------------------------------- | --------------------------------------------------------------- |
| `0x11` | `bms_set_discharge_enable` (`FUN_0801c2f4`) with flag 0/1 | enable/disable pack discharge; used to clear undervoltage fault |
| `0x22` | `bms_set_charge_enable` (`FUN_0801c354`) with flag 0/1    | enable/disable pack charge; used to clear overvoltage fault     |


No **read-side commands** were found in the firmware — the poll loop
in `battery_data_poll_fsm` case 3/5 calls `bms_i2c_read_byte(0x40, …)`
which reads a single byte from address `0x40` without an explicit
command word, suggesting the BMS autonomously streams the per-pack
summary fields that feed `b{0,1,2}{max,min}` etc. We did **not**
locate a "read cell voltage array" command on this bus; the firmware
never asks for one, and the BMS firmware (in the battery pack itself)
therefore never sends one.

**Event codes on this bus**:

- Event 41 (`0x29`) fires in case 5 of `battery_data_poll_fsm` when
`bms_i2c_read_byte` fails (terminal — enters an infinite loop).
- Event 73 (`0x49`, `bms_comm_watchdog`) fires from
`bms_fault_flags_monitor` on prolonged I²C silence.
- Event 95 (`0x5F`, `cell_fault_flags_packed`) packs the three per-pack
fault bytes into a single u32: `(pack2_flags << 24) | (pack1_flags << 16) | (pack0_flags << 8) | counter_low_byte`.

## Pack state RAM layout

From `battery_cell_fault_handler` (`0x08023924`): a 3-element array
of 145-byte structs, stride `0x91`, starting at `DAT_08023ce0`.


| Struct offset | Inferred meaning                                                                                                      |
| ------------- | --------------------------------------------------------------------------------------------------------------------- |
| `+0x01`       | Pack state byte ('\v'/0x0b in normal running)                                                                         |
| `+0x02`       | Fault-flag latch bitmap                                                                                               |
| `+0x4f`       | Raw BMS fault flags (bit 0 = charge fault, bit 1 = discharge fault, bit 31 = pack-absent) — read from the I²C replies |
| `+0x53`       | Cell SoC (uint16, `0..100`)                                                                                           |


Per-pack lookup macro: `pack_base = DAT_08023ce0 + pack_idx * 0x91`.

On single-pack HMJ hardware, only `pack_idx == 0` is active; the other
two structs are zeroed.

## Charging algorithm literals

Extracted during Phase 2b from `battery_charge_monitor`
(`0x08019774`) and `battery_discharge_monitor` (`0x08019a84`).

### Charge side (event codes 89/90)


| Literal         | Where                               | Inferred meaning                                                      |
| --------------- | ----------------------------------- | --------------------------------------------------------------------- |
| `16000`         | debounce ceiling for pack voltage   | 16.000 V = pack-level over-voltage threshold (normal mode)            |
| `30000`         | debounce ceiling for pack voltage   | 30.000 V = over-voltage threshold when the secondary flag is set      |
| `12000`         | early-completion limiter            | 12.000 V = cut-off above which the charge-complete debouncer is armed |
| `11999`         | strict-less-than guard for the same | same threshold, offset-by-one guard                                   |
| `500`           | current-taper gate                  | 500 mA = current below which "charge taper" is considered reached     |
| `2000` / `1999` | taper hold threshold                | 2000 mA = current above which the cycle counter is suppressed         |
| `60 000` (ms)   | event debounce                      | 60 s debounce before `enqueue_event(89/90)` fires                     |
| `0x708` (1800)  | inactivity reset                    | 1 800 s (30 min) without a current transition → counter reset         |
| `0x3c` (60)     | per-attempt debounce                | 60 s base window, multiplied by attempt number                        |


Pack-specific event codes: 89 (`0x59`) for pack 0, 90 (`0x5A`) for pack 1.
No dedicated event for pack 2 in the observed HMJ firmware.

### Discharge side (event codes 24/35)


| Literal     | Where                 | Inferred meaning                                                      |
| ----------- | --------------------- | --------------------------------------------------------------------- |
| `1000` ms   | undervoltage debounce | 1 s continuous dip before `state == 1`                                |
| `3000` ms   | re-arm debounce       | 3 s required with current > 500 mA to re-enable discharge after fault |
| `500`       | current gate          | 500 mA = re-arm current threshold                                     |
| `0x32` (50) | hysteresis            | 50 mV margin above the configured load threshold                      |


Pack-specific event codes: 24 (`0x18`, `soc_low`) and 35 (`0x23`,
`undervoltage_discharge`).

Both monitors call into the I²C bus via `bms_set_discharge_enable(0/1)`
(cmd `0x11`) and `bms_set_charge_enable(0/1)` (cmd `0x22`), so the
charge-algorithm state machine is implemented **on the MCU side**, not
in the BMS firmware. The BMS only executes the enable/disable commands.

**Missing items** the plan asked for but the firmware does not appear
to contain as literals:

- Per-cell CC/CV cut-off voltages — these are almost certainly **inside
the BMS firmware** (inaccessible) rather than the main MCU.
- Temperature-derated current table — not found as a contiguous array
in the code area we traced. It may live in the 6 KiB lookup-table
block at flash `0x24C00` (flagged by `scripts/analyze_firmware.py` as
monotonically decreasing 16-bit values — consistent with an NTC curve
or an OCV-SoC table, but no xrefs were chased in this pass).
- Balance thresholds — not found. Almost certainly BMS-side.

## MQTT exposure audit (Phase 3 result)

Cross-reference of the three rich cloud-pollable responses whose
format strings live in firmware flash but that are **not** seen in any
of our six pcaps:


| cd= | Format string @      | Response shape                                                                                                                                                                                                                                                                                                   | Contains per-cell data? |
| --- | -------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------- |
| 221 | `0x08027eb0`, ~650 B | `p1,p2,w1,w2,pe,vv,sv,cs,cd,am,o1,o2,do,lv,cj,kn,g1,g2,b1,b2,md,d1..d5,e1..e5,f1..f5,h1..h5,sg,sp,st,tl,th,tc,tf,fc,id,a0..a2,l0,l1,c0,c1,bc,bs,pt,it,m0..m3,lmo,lmi,lmf,uv,sm,bn,ct_t,tc_dis,ws,fktc,fk_chg_f,fk_chg_sc0,fk_chg_sc1,fk_dsg_sp0,fk_dsg_sp1,bts,btr` — full settings snapshot, superset of `cd=0` | **No**                  |
| 206 | `0x08028824`, ~288 B | 48 calibration constants indexed hex-style `a0..af, b0..bf, c0..cf`                                                                                                                                                                                                                                              | **No** (calibration)    |
| 222 | `0x08028944`, ~120 B | `ct_t,phase_t,dchrg_t,seq_s,c0,c1,c2,cp,op` — CT meter state                                                                                                                                                                                                                                                     | **No** (CT only)        |


Dispatch — the responses are built inside `FUN_08017ddc`, an internal
command-dispatch function with numeric builder IDs:


| Builder ID     | Builder function                     | Response |
| -------------- | ------------------------------------ | -------- |
| `0x01`         | `FUN_08018080`                       | cd=221   |
| `0x0D`         | `FUN_08017900`                       | cd=206   |
| `0x0E`..`0x51` | 14 other builders (not fully traced) | cd=?     |
| `0x1C`         | `FUN_08017a20`                       | cd=222   |


The upstream caller is `http_setreport_fsm` (~20 call sites). That FSM
translates an inbound `cd=N` value on the MQTT control topic into a
builder ID. We did **not** fully map inbound-cd → builder-id in this
pass — `scripts/probe_cd.py` (delivered alongside this doc) is a
strictly read-only whitelist-based probe that can confirm the mapping
on a live device; running it is left to the operator.

Pcap audit (from six captures) confirms neither the real Marstek
cloud nor the phone app poll `cd=221`, `cd=206`, or `cd=222` in
steady state in our data. Even if they did, the responses would not
contain per-cell telemetry.

## Hardware-tap follow-up (the Phase 4 fallback)

Since per-cell / coulomb / SoH is not on any software channel, the
only route to it is physical access to Surface 2.

**Target bus**: I²C2 at BMS address `0x40`. The MCU is the master; the
BMS inside the battery pack is the slave.

**Physical pads to probe** — pin assignment for the STM32F1
connectivity-line I²C2:

- SCL = `PB10`
- SDA = `PB11`

(Default alternate-function mapping; no AFIO remap bit is set in the
init we traced, so these are the default pins.)

Given a logic analyzer or an MCU-as-slave sniffer on PB10/PB11:

1. **Confirm** the `0x34..[cmd][len][payload]..0x35` frame format from
  this doc on the wire.
2. **Decode** the autonomous push stream (every 500 ms) from the BMS
  to the main MCU — this is where the per-cell voltage vector, any
   coulomb counter, and any SoH byte must live if they exist at all.
3. **Match** decoded fields against the `b0max` / `b0min` / `b0maxn` /
  `b0minn` numbers reported by the cloud path, to verify that the
   sniffed per-cell vector aggregates to the same min/max pair.

The frame-checksum routine `FUN_0800b518` called at the tail of
`bms_i2c_xfer` is a small function we can lift into a standalone
validator before decoding.

**Recommended hardware** is anything that can do ~400 kHz I²C capture
— a Saleae Logic or a `bus pirate` plus `sigrok-cli` is sufficient.

Baud rate: not extracted in this pass; the I²C clock-stretch and
`FUN_08012bbc` timeout parameters suggest default STM32 standard-mode
(100 kHz) or fast-mode (400 kHz). The BRR/CCR register writes inside
the peripheral init (callers of the I²C helper family `FUN_08012b74`…
`FUN_08012d04`) would pin this down.

Explicitly **out of scope for this exporter project** — the
hardware-tap path is documented here so the work is not lost, and
listed on `docs/roadmap.md` under a dedicated follow-up item.

## Glossary of Ghidra renames / labels created in this pass

Persisted in `ghidra/firmware.rep/`:

### Functions


| Address      | New name                   | Old name       | Purpose                                             |
| ------------ | -------------------------- | -------------- | --------------------------------------------------- |
| `0x0801b3a8` | `bms_bitbang_xfer_byte`    | `FUN_0801b3a8` | 8-bit GPIO bit-bang transfer (pack-ID surface)      |
| `0x08011052` | `gpio_set_pin`             | `FUN_08011052` | set-bit helper                                      |
| `0x0801104e` | `gpio_clear_pin`           | `FUN_0801104e` | clear-bit helper                                    |
| `0x08011032` | `gpio_read_pin`            | `FUN_08011032` | input-bit helper                                    |
| `0x08012e34` | `gpio_configure_output_pp` | `FUN_08012e34` | output push-pull config                             |
| `0x08012d88` | `gpio_configure_input`     | `FUN_08012d88` | input config                                        |
| `0x080229a4` | `bitbang_delay_short`      | `FUN_080229a4` | software delay loop                                 |
| `0x08023d5c` | `bms_i2c_xfer`             | `FUN_08023d5c` | core I²C2 master transfer                           |
| `0x08023fe4` | `bms_i2c_read_byte`        | `FUN_08023fe4` | single-byte I²C read wrapper                        |
| `0x08024198` | `bms_i2c_queue_cmd`        | `FUN_08024198` | enqueue a command for pack 1/2; pack 0 is immediate |
| `0x08022510` | `bms_clear_fault`          | `FUN_08022510` | fault-clear dispatch (cmds 0x11 / 0x22)             |
| `0x0801c2f4` | `bms_set_discharge_enable` | `FUN_0801c2f4` | cmd 0x11 with 0/1 flag                              |
| `0x0801c354` | `bms_set_charge_enable`    | `FUN_0801c354` | cmd 0x22 with 0/1 flag                              |


### Labels


| Address      | Name                       | Value        | Meaning                         |
| ------------ | -------------------------- | ------------ | ------------------------------- |
| `0x0801b490` | `g_bms_gpio_port_ptr_rx`   | `0x40010C00` | GPIOB base (bit-bang rx path)   |
| `0x0801b3a0` | `g_bms_gpio_port_ptr_init` | `0x40010C00` | GPIOB base (bit-bang init path) |
| `0x08023fd4` | `g_bms_i2c2_base_ptr`      | `0x40005800` | I²C2 peripheral base            |


## Cross-reference

- Related roadmap items: **12** (BMS protocol), **13** (charging
algorithm internals) — both struck through in `docs/roadmap.md`
after this pass, with the hardware-tap work surfacing as a new
follow-up.
- Related event codes in `emulator/solar_errinfo_codes.go`:
`25`, `35`, `73`, `75`, `81`, `82`, `89`, `90`, `95`, `104`.
- Related metrics exposed during Phase 0 of this plan (see
`README.md` §Metrics): `marstek_battery_pack_soc_percent`,
`marstek_mqtt_m_channel`, `marstek_cloud_battery_pack_soc_percent`,
`marstek_battery_pack_fault_flags`,
`marstek_battery_pack_temperature_raw`.
- Probe script: `scripts/probe_cd.py` (dry-run first;
`--i-know-what-im-doing` required for live transmission).

