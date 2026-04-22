# Cloud / Network Endpoints

## DNS and mock-bypass

The local network in this lab mocks hamedata DNS and redirects HTTP traffic:

```
$ dig +short eu.hamedata.com            # local mocked resolver
10.1.1.5
$ dig +short eu.hamedata.com @8.8.8.8   # real
3.122.27.237
3.68.141.219
$ dig +short www.hamedata.com @8.8.8.8  # firmware CDN (real)
120.25.59.188
$ dig +short static-eu.marstekenergy.com @8.8.8.8
d31gbd0siulc45.cloudfront.net.  18.173.154.{22,45,47,116}   # CloudFront
```

### Bypassing the mock

Use `curl --resolve` (or `/etc/hosts`) to hit the real servers from this
machine without disabling the mock:

```bash
curl -sSLI --resolve www.hamedata.com:443:120.25.59.188 \
  https://www.hamedata.com/app/download/neng/B2500_All_HMJ.bin
```

The convenience wrapper `scripts/probe_endpoints.sh` does this for every
known endpoint at once.

## Hosts observed / extracted


| Host                          | Real IP / CNAME                                   | Role                                                                     |
| ----------------------------- | ------------------------------------------------- | ------------------------------------------------------------------------ |
| `eu.hamedata.com`             | `3.122.27.237`, `3.68.141.219` (AWS eu-central-1) | EU production API + MQTT broker (plaintext)                              |
| `eu-staging.hamedata.com`     | n/a (in APK)                                      | EU staging API                                                           |
| `eu-dev.hamedata.com`         | n/a (in APK)                                      | EU dev API                                                               |
| `us.hamedata.com`             | `35.172.49.157` (AWS us-east-1)                   | US production API                                                        |
| `www.hamedata.com`            | `120.25.59.188` (Alibaba Cloud, CN)               | **Firmware / OTA CDN** — serves `.bin` only, :80 drops other paths       |
| `static-eu.marstekenergy.com` | CloudFront `d31gbd0siulc45` → S3                  | Marstek EU CDN for `Mars2` APK and `update.zip` ROM (discovered via API) |


`eu.hamedata.com` answers from an API gateway that looks like **Kong**:

- Routes that Kong knows about but rejects return `{"code":8,"msg":"Forbidden"}`
(seen on e.g. `/ems/api/v2/mConfig`, `/ems/api/v4/mConfig`).
- Routes that don't hit Kong's path-matcher at all fall through to nginx and
get a plain `404 Not Found` HTML page.

So `{"code":8,"msg":"Forbidden"}` does **not** mean "this endpoint exists but
requires auth" — it means "Kong has no matching route". Interpret with care.

## HTTP endpoints observed live (from pcaps)

### 1. Time-sync / heartbeat

```
GET http://eu.hamedata.com/app/neng/getDateInfoeu.php
    ?uid=<cloud_uid>
    &fcv=<firmware_compile_ts>
    &aid=HMJ-2
    &sv=<soft_ver>
    &sbv=<sub_ver>
    &mv=<mcu_ver>
```

Response body is plain text, fixed width, underscore-delimited:

```
_2026_04_22_09_02_31_04_0_0_0
 └── YYYY_MM_DD_HH_MM_SS_DOW_<flag>_<flag>_<flag>
```

This is **only** a time feed. It does **not** return an OTA/upgrade URL or
broker address, regardless of the `sv`/`sbv`/`mv` values passed. Each shard
returns its own *local* time (Europe/Berlin for `eu.`, America/Los_Angeles
for `us.`), not UTC.

When `eu.hamedata.com` is unreachable, the battery's self-adjustment mode
stops functioning — this is that endpoint.

### 2. Telemetry upload (encrypted)

```
GET http://eu.hamedata.com/prod/api/v1/setB2500Report?v=<base64url-blob>
```

The `v` parameter is AES-128-ECB / PKCS#7 encrypted URL-encoded telemetry,
using the key `hamedatahamedata` (see `[firmware.md](firmware.md)` and
`[emulator/report.go](../emulator/report.go)`).

**The cloud does not decrypt before ack'ing.** Probing with:

- `v=DEADBEEF_not_real_ciphertext`  → `{"code":1,"msg":"ok"}`
- `v=<valid ciphertext, fake fields>` → `{"code":1,"msg":"ok"}`

returns an identical response. The endpoint is write-only and has no signal
we can use from the client side; if the cloud validates at all, it does so
asynchronously after responding.

### 3. Other `/ems/api/v1/` calls (live, return JSON)

```
GET http://eu.hamedata.com/ems/api/v1/getRealtimeSoc?devid=<mac>&type=<n>
GET http://eu.hamedata.com/ems/api/v1/getDeviceFire?devid=<mac>
```

For an unknown MAC both return `{"code":1,"msg":"ok","data":{...zeros/defaults...}}`
rather than an error — the cloud is lenient about unknown device IDs.

## HTTP endpoints extracted from the APK (probed in this session)


| Endpoint                                        | Status             | Response                                                                                                                                                      |
| ----------------------------------------------- | ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /ems/api/v3/mConfig`                       | 200                | `{"upgradeMethod":"3","speed":8000}` — static global tuning (parameter-agnostic)                                                                              |
| `GET /ems/api/v1/mConfig`                       | 200                | Older shape, `{"upgradeMethod":"3"}` (no `speed`)                                                                                                             |
| `GET /ems/api/v1/getAppCodeUrl?app_name=Mars`   | 200                | APK update manifest: `apk_url=https://static-eu.marstekenergy.com/hameemsmars/APK/Mars2_<ver>_<ts>.apk`                                                       |
| `GET /ems/api/v1/getAppRomSystem?app_name=Mars` | 200                | ROM bundle: `rom_url=https://static-eu.marstekenergy.com/hameemsmars/UPGRADE/update.zip`, Chinese `remark` field                                              |
| `POST /app/Solar/puterrinfo.php`                | 200                | Returns `_1` (real cloud ack). Body is a colon/comma/dot event log; see [B2500 error event log](#4-b2500-error-event-log-puterrrinfophp) below.                |
| `GET/POST /app/Solar/get_device.php`            | **timeout on :80** | `www.hamedata.com` on Alibaba CN does not actually answer anything but `/app/download/neng/*.bin` on port 80. The legacy PHP API host has not yet been found. |


The `Mars2_*.apk` / `update.zip` pipeline is served from
`**static-eu.marstekenergy.com`**, which is **CloudFront → S3**
(`x-amz-cf-pop: MUC50-P3`, `server: AmazonS3`). Together with the baked
AWS IoT credential in the firmware, this confirms that **Marstek's EU
infrastructure sits on AWS**; only the Chinese `www.hamedata.com` firmware
CDN is on Alibaba Cloud.

### 4. B2500 error/event log (`puterrinfo.php`)

```
POST http://eu.hamedata.com/app/Solar/puterrinfo.php
Content-Type: application/x-www-form-urlencoded

<uid>:<type>:<sw_ver>:<field2>:<field3>:<field4>:<field5>:<event>[,<event>...]
```

The device uploads a buffered error/event ring (up to 42 entries). The real
cloud always responds with `_1` regardless of body content.

**Derived from Ghidra static analysis** of `B2500_All_HMJ.bin` (HMJ fw 110,
ARM Cortex-M, base `0x08000000`). See `emulator/solar_errinfo.go` for the
full protocol grammar and event-code dictionary, and `docs/firmware.md` for
the Ghidra analysis notes.

#### Header fields

The colon-separated header has 7 fields (1 string + 6 integers):

| Position | Name            | Description                                                          |
| -------- | --------------- | -------------------------------------------------------------------- |
| 0        | `uid`           | Device UID (hex string, e.g. `3601115030374d33300f1365`)             |
| 1        | `report_type`   | Battery slot: `0` = slot 0 (triple events), `1` = slot 1, `2` = slot 2 (quintuple events) |
| 2        | `sw_version`    | Firmware version number (e.g. `110`)                                 |
| 3        | `field2`        | SoC % at the time the event ring was flushed (confirmed via marstek-7.pcap: field2=100 matched soc=100% in surrounding cloud telemetry reports) |
| 4        | `field3`        | Status flags byte at battery_state+0x4d / +0xde / +0x16f            |
| 5        | `field4`        | Status flags byte at battery_state+0x4e / +0xdf / +0x170            |
| 6        | `field5`        | Status flags byte at battery_state+0x4f / +0xe0 / +0x171            |

Firmware format string (at `0x0121a4`): `"%s:%d:%d:%d:%d:%d:%d:"`

#### Event formats

**Type 0** (battery slot 0) — triple: `<code>.<unix_ts_seconds>.<value>`

**Type 1/2** (battery slots 1/2) — quintuple: `<a>.<b>.<c>.<d>.<value>`
- `a` = event code (same dictionary as type 0)
- `b`, `c`, `d` = sub-fields (likely cell index, phase, severity — TBC vs live capture)
- `value` = raw u32 measurement

The device ring buffer (`enqueue_event`, `0x0802152c`) holds 42 entries.
Dedup: if `(last_code, last_value)` matches the incoming call, the event is
dropped silently. Overflow: FIFO — oldest evicted, newest appended.

#### Event-code dictionary (49 confirmed codes from 95 call sites)

| Code | Name                            | Trigger / value semantics                                              |
| ---- | ------------------------------- | ---------------------------------------------------------------------- |
|   0  | `startup_init`                  | Battery poll init on startup; value 0                                  |
|  12  | `soc_threshold_crossed`         | SoC debounce or 100% HTTP parse; value = SoC                           |
|  15  | `cell_overvoltage_charge`       | Cell voltage > two-level charge limit; value = mV                      |
|  18  | `cell_voltage_high`             | Cell voltage > high threshold (debounced); value = max cell mV         |
|  24  | `discharge_status_flag`         | Discharge status bit changed; value = flags byte                       |
|  25  | `bms_probe_no_response`         | BMS probe returned error; value = 0xFF                                 |
|  33  | `shelly_ct_meter_status`        | Shelly CT-meter state byte at state+0x4d changed (top bit set); value = status byte |
|  35  | `undervoltage_discharge`        | Battery < undervoltage threshold (debounced); value = mV               |
|  36  | `soc_zero_or_overvoltage_low`   | SoC==0 or voltage < low limit; value = voltage or SoC                  |
|  38  | `charge_voltage_limit`          | Charge voltage crossing boundary; value = mV                           |
|  39  | `overcurrent_charge`            | Charge current > threshold; value = current                            |
|  42  | `discharge_protection_flag`     | Discharge protection bit set; value = protection byte                  |
|  43  | `battery_capacity_low`          | Capacity low on poll timer; value = raw poll data                      |
|  50  | `thermal_discharge_high`        | Max temp > discharge high limit; value = temperature (i16)             |
|  51  | `thermal_discharge_low`         | Min temp > discharge high limit; value = temperature (i16)             |
|  52  | `thermal_charge_high`           | Max temp > charge high limit; value = temperature (i16)                |
|  53  | `thermal_charge_low`            | Min temp > charge high limit; value = temperature (i16)                |
|  62  | `ble_energy_accumulated`        | BLE GATT energy accumulation complete; value = wH (i32)                |
|  64  | `battery_poll_status`           | Battery poll FSM state changed; value = raw poll data                  |
|  65  | `reboot_pending`                | MCU reset imminent; value = 0                                          |
|  66  | `ble_soc_state_changed`         | BLE SoC state transition; value = state byte                           |
|  73  | `bms_comm_watchdog`             | BMS watchdog triggered; value = 0x11 constant                          |
|  74  | `mqtt_ext_conn_failed`          | Secondary/aux MQTT client AT+QMTCONN rejected after 4 retries; value = broker connect-reject status |
|  75  | `fault_flags_bitmap`            | BMS fault-flags word changed OR wifi_scan_fsm gave up after 2 timeouts; value = byte \| (u16 << 16) or 0 |
|  77  | `soc_below_threshold`           | SoC < charge threshold; value = SoC %                                  |
|  78  | `mqtt_ext_session_up`           | Secondary MQTT session established (AT+QMTSUB accepted); value = 1 or subscribed-topics feature bitmap (a0b<<3 \| a0c<<2 \| a0d<<1 \| base) |
|  80  | `setreport_response_parsed`     | setreport HTTP response parsed via alternate JSON key branch; value = response flag byte |
|  81  | `battery_pack_init_no_response` | BMS init probe (0x81 cmd) returned 0 or 0xFF; value = raw probe return |
|  82  | `battery_pack_init_cell_fault`  | Init-time cell-voltage fault: one of 3 thresholds crossed (0x31/0x12/0x50); value = fault bitmask (bits 3/4/5) |
|  84  | `heartbeat`                     | 60s timer or HTTP response; value = 0 (timer), 327679/0x4FFFF (HTTP OK composite), or HTTP status code (e.g. 404) on non-OK response |
|  85  | `mqtt_ext_subscribe_failed`     | Secondary MQTT client AT+QMTSUB rejected after 4 retries; value = broker subscribe-reject byte |
|  86  | `tls_cert_inventory_missing`    | AT+QFLST did not list all 3 TLS files (User_Cert_1 / User_Key_1 / CA); triggers re-provisioning; value = inventory bitmap |
|  87  | `tls_cert_slots_cleared`        | AT+QSSLCERT="CA",0 → "User_Cert",0 → "User_Key",0 all OK (modem ready to flash new certs); value = retry counter at state+0xb2a |
|  88  | `tls_user_key_written`          | TLS User-Key payload flashed to modem cert store via QFUPL (AT response \x02); value = 0 |
|  89  | `charge_current_high_ch0`       | ch0 > 16 A for 60 s; value = current or state                          |
|  90  | `charge_current_high_ch1`       | ch1 > 16 A for 60 s; value = current or state                          |
|  91  | `http_post_failed`              | puterrinfo POST returned non-200; value = HTTP status code             |
|  92  | `cert_slot_selected`            | TLS cert slot chosen; value = slot index (0–2)                         |
|  93  | `cert_activated`                | TLS cert activated; value = offset + slot_idx + soc×10000              |
|  95  | `cell_fault_flags_packed`       | Pack cell-fault flag bytes logged every 50% SoC crossing; value = (flags[0x171]<<24) \| (flags[0xe0]<<16) \| (flags[0x4f]<<8) \| (soc & 0xff) |
|  96  | `mqtt_auth_max_retries`         | MQTT auth exhausted; value = (b_retries << 16) \| a_retries            |
|  98  | `subsystem_state_event`         | Subsystem state transition; value = subsystem-specific                 |
|  99  | `cert_change_triggered`         | TLS cert rotation; value = cert ID byte                                |
| 100  | `charge_setpoint_exceeded`      | Charge setpoint voltage exceeded; value = CONCAT(v0, v1)               |
| 101  | `passthrough_mode_changed`      | Pass-through mode toggled; value = 0 (off) or 1 (on)                   |
| 103  | `ac_coupling_event`             | AC coupling fault/timeout; value = 710/709/810/811 or v+8000           |
| 104  | `battery_pack_reset_complete`   | Battery pack reset sequence completed (battery_pack_reset); value = 0  |
| 105  | `ble_adv_retry_exhausted`       | BLE advertising retry counter at state+0x18 exceeded 3 (adv restart give-up); value = final retry count |
| 106  | `wifi_disconnect`               | Supplicant reason 1 or 2 reported 3× in a row (wifi_connect_fsm); value = disconnect reason (1 or 2) |

All 49 codes in the firmware are now identified. Codes outside this set are
logged as `unknown_<N>` but none have been observed live so far.

#### Observed wire sample (marstek-6.pcap, 42 events)

```
3601115030374d33300f1365:0:110:41:0:0:67:84.1776748256.0,84.1776748262.327679,...
```

Parsed by `emulator/solar_errinfo.go`:

```
uid=3601115030374d33300f1365  report_type=0  sw_version=110
events=[
  {t=2026-03-22T...Z  code=84 (heartbeat)  value=0}
  {t=2026-03-22T...Z  code=84 (heartbeat)  value=327679}
  ...42 total heartbeat toggles...
]
```

#### Prometheus metrics

The emulator exposes per-battery named gauges and per-event counters:

| Metric                                   | Labels              | Description                        |
| ---------------------------------------- | ------------------- | ---------------------------------- |
| `marstek_solar_errinfo_report_type`      | `uid, battery`      | Report type (0/1/2)                |
| `marstek_solar_errinfo_sw_version`       | `uid, battery`      | Firmware sw_version field          |
| `marstek_solar_errinfo_field2`           | `uid, battery`      | Header field 2 (SoC/voltage TBC)   |
| `marstek_solar_errinfo_field3..5`        | `uid, battery`      | Status flag bytes                  |
| `marstek_solar_errinfo_event_total`      | `uid, battery, code, name` | Count of events received    |
| `marstek_solar_errinfo_last_event_ts_seconds` | `uid, battery, code, name` | Unix ts of latest event  |
| `marstek_cloud_solar_errinfo_header_value` | `index`           | Positional header integers (backward compat) |

### Legacy Solar/neng PHP API (not reachable from here)

Extracted from `libapp.so` strings. Host not confirmed — `www.hamedata.com:80`
drops these requests (only serves the firmware CDN). These are catalogued for
completeness:

```
/app/Solar/save_user.php          register user
/app/Solar/identify.php           login
/app/Solar/update_pwd.php
/app/Solar/findpwd.php
/app/Solar/delete_user.php
/app/Solar/add_device.php         bind device to account
/app/Solar/unbind_device.php
/app/Solar/share_device.php
/app/Solar/edit_name.php
/app/Solar/get_device.php         list devices
/app/Solar/get_deviceinfo.php
/app/Solar/get_tenmins_one.php    10-min resolution history
/app/Solar/get_month_b2500.php    monthly aggregates (B2500-specific)
/app/Solar/get_year_b2500.php
/app/Solar/get_total_b2500.php
/app/neng/check_edit.php
/app/neng/print_zero.php
```

### Privacy policy documents

```
https://eu.hamedata.com/app/privacy-policy/marstek/<lang>-doc.html
```

## MQTT broker (plaintext)

- Host: `eu.hamedata.com`, port **1883** (plain MQTT, no TLS)
- Observed in pcaps as `10.1.1.5:1883` because of the local DNS mock
- Topic scheme: `[mqtt.md](mqtt.md)`

Because MQTT is plaintext and device-reported (no client certificate), it is
trivial to intercept on the LAN by redirecting `eu.hamedata.com` to a local
broker. That is how both this project and `hm2mqtt` obtain the telemetry.

## AWS IoT Core (mTLS) — inferred but not yet seen on the wire

The firmware carries a matched AWS IoT mTLS triple
(CA + device cert + RSA-2048 private key — see
`[firmware.md](firmware.md)`) but the backing broker hostname is **not**
present anywhere as plaintext. Together with:

- the fleet-wide subject (`CN=AWS IoT Certificate`, no per-device fields),
- the `.amazonaws.com` / `-ats` / `.iot.` searches all coming back empty,
- and the rest of Marstek's EU infra (APK CDN, `eu.hamedata.com` itself)
sitting on AWS,

this has the shape of **AWS IoT Fleet Provisioning by Claim**: a fleet-shared
"claim" cert is baked in, used *once* to open an mTLS connection to
`<prefix>-ats.iot.<region>.amazonaws.com:8883`, publish to
`$aws/certificates/create/`* and `$aws/provisioning-templates/`* to receive a
per-device cert, then the device reconnects with the per-device identity.
Confirming this requires either a clean pcap of a freshly-provisioned device
(the AWS IoT hostname is passed in as a TLS SNI and will be visible there) or
dynamically intercepting `AT+QMTOPEN` arguments on the MCU↔radio UART.

## Known firmware quirks

### Bare GET with empty path

Observed in marstek-7.pcap at t=4200s and t=4260s (60 seconds apart): the
device sends an HTTP request with **no URI path**:

```
GET  HTTP/1.1
Host: eu.hamedata.com
Connection: keep-alive
User-Agent: quectel-fc41d
```

Note the double space between `GET` and `HTTP/1.1` — the path is missing
entirely. This is a race condition in the Quectel FC41D AT HTTP stack where
`AT+QHTTPGET` fires before the URL is fully configured. The emulator's
catch-all handler responds with 404. No action needed; the device retries
with a correct request on the next reporting cycle.

### `date` field frozen at noon

The `date` field in every `setB2500Report` payload reports the same timestamp
(e.g. `2026-4-22 12:00:00`) regardless of actual time of day. The device only
calls `getDateInfo` at boot for time synchronisation. If the boot occurred
before DNS was redirected to the emulator (or while the real cloud was
unreachable), the device clock is stuck at the date of boot with a hardcoded
noon time. This makes `marstek_cloud_device_timestamp_seconds` unreliable for
precise clock-drift detection; it only confirms which **date** the device
thinks it is.

## Summary

For the Prometheus exporter we only need **MQTT** on the LAN. The cloud
endpoints are documented here purely so we know what the device is reaching
out to (and so we can keep a stub broker/responder happy if the device expects
them).