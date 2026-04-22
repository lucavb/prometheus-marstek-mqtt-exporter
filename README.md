# prometheus-marstek-mqtt-exporter

Prometheus exporter for the Marstek B2500-D home battery, using MQTT.

Connects to a local MQTT broker, periodically polls the battery by publishing `cd=1` to its command topic, parses the status response, and exposes the data as Prometheus metrics on an HTTP `/metrics` endpoint.

No BLE, no Marstek cloud — purely MQTT.

## Metrics

All metrics carry the labels `device_type` and `device_id`.

### MQTT metrics


| Metric                                  | Labels              | Description                                               |
| --------------------------------------- | ------------------- | --------------------------------------------------------- |
| `marstek_battery_soc_percent`           |                     | State of charge in percent                                |
| `marstek_battery_remaining_wh`          |                     | Remaining battery capacity in Wh                          |
| `marstek_battery_dod_percent`           |                     | Depth of discharge setting in percent                     |
| `marstek_output_threshold_watts`        |                     | Minimum load to engage output in watts                    |
| `marstek_daily_battery_charge_wh`       |                     | Daily battery charge energy in Wh (resets at midnight)    |
| `marstek_daily_battery_discharge_wh`    |                     | Daily battery discharge energy in Wh (resets at midnight) |
| `marstek_daily_load_charge_wh`          |                     | Daily load charge energy in Wh                            |
| `marstek_daily_load_discharge_wh`       |                     | Daily load discharge energy in Wh                         |
| `marstek_rated_output_watts`            |                     | Rated output power in watts                               |
| `marstek_rated_input_watts`             |                     | Rated input power in watts                                |
| `marstek_surplus_feed_in_enabled`       |                     | 1 if surplus feed-in is enabled, 0 otherwise              |
| `marstek_up`                            |                     | 1 if the last MQTT poll received a response, 0 otherwise  |
| `marstek_last_update_timestamp_seconds` |                     | Unix timestamp of the last successful MQTT update         |
| `marstek_solar_input_watts`             | `input` (1, 2)      | Solar input power in watts                                |
| `marstek_output_watts`                  | `output` (1, 2)     | Output power in watts                                     |
| `marstek_output_enabled`                | `output` (1, 2)     | Output enabled state (1=on, 0=off)                        |
| `marstek_temperature_celsius`           | `sensor` (min, max) | Device temperature in Celsius                             |
| `marstek_extra_pack_connected`          | `pack` (1, 2)       | Extra battery pack connected (1=yes, 0=no)                |
| `marstek_battery_pack_soc_percent`      | `pack` (0, 1, 2)    | Per-pack state of charge. `pack=0` is the aggregate `pe`; `pack=1`/`pack=2` are the `a1`/`a2` channels |
| `marstek_mqtt_m_channel`                | `channel` (0–3)     | Raw `m0`..`m3` channels from cd=0. Semantics partially decoded (m3 ≈ load watts); keep for anomaly detection |
| `marstek_scrapes_total`                 |                     | Total number of cd=1 polls sent                           |
| `marstek_scrape_errors_total`           |                     | Polls that received no response within the timeout        |


### Cloud emulator metrics (only present when `MARSTEK_EMULATOR_LISTEN_ADDR` is set)


| Metric                                                 | Labels                                                                                        | Description                                                                                                                             |
| ------------------------------------------------------ | --------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `marstek_device_info`                                  | `uid`, `device_type_reported`, `firmware_version`, `sw_version`, `sub_version`, `mod_version` | Device metadata parsed from the cloud time-sync request. Value is always 1; use label values for joins or alerts.                       |
| `marstek_cloud_reports_total`                          | `endpoint` (`date_info`, `report`, `solar_errinfo`, `unknown`)                                | Total requests received by the cloud emulator per endpoint.                                                                             |
| `marstek_cloud_last_report_timestamp_seconds`          | `endpoint` (`date_info`, `report`, `solar_errinfo`)                                           | Unix timestamp of the last request per known endpoint.                                                                                  |
| `marstek_cloud_last_unknown_request_timestamp_seconds` |                                                                                               | Unix timestamp of the last request to an unrecognised endpoint. Non-zero means a new firmware endpoint was discovered — check the logs. |
| `marstek_cloud_report_payload_bytes`                   |                                                                                               | Decoded plaintext size of the latest telemetry report. A change may indicate a firmware update.                                         |
| `marstek_cloud_report_decode_errors_total`             |                                                                                               | Payloads that could not be decrypted or parsed. A non-zero value may indicate a firmware key rotation.                                  |
| `marstek_cloud_solar_errinfo_header_value`             | `index` (0, 1, 2, …)                                                                          | Integer values from the `puterrinfo` request header, keyed by zero-based position. `index=1` is `sw_version`; `index=2..4` are `pe0..pe2`; other indices are not yet fully identified. A new index appearing means firmware added a header field. |
| `marstek_cell_voltage_millivolts`                      | `pack` (0, 1, 2), `bound` (min, max)                                                          | Per-pack min/max cell voltage in millivolts, from the cloud telemetry report.                                                           |
| `marstek_cell_voltage_cell_index`                      | `pack` (0, 1, 2), `bound` (min, max)                                                          | Index of the min/max voltage cell within each pack, from the cloud telemetry report.                                                    |
| `marstek_solar_input_voltage_millivolts`               | `input` (1, 2)                                                                                | Per-solar-input voltage in millivolts, from the cloud telemetry report.                                                                 |
| `marstek_solar_input_power_watts`                      | `input` (1, 2)                                                                                | Per-solar-input power in watts, from the cloud telemetry report (`pv1`/`pv2` fields). The sum equals the aggregate `pv` field.          |
| `marstek_output_voltage_millivolts`                    | `output` (1, 2)                                                                               | Per-output-port voltage in millivolts, from the cloud telemetry report.                                                                 |
| `marstek_cloud_device_timestamp_seconds`               |                                                                                               | Device self-reported local time as a Unix timestamp. Use to detect clock drift.                                                         |
| `marstek_wifi_bt_status`                               |                                                                                               | Raw `wbs` field from the cloud telemetry report, indicating Wi-Fi/Bluetooth connectivity state.                                         |
| `marstek_cloud_battery_pack_soc_percent`               | `pack` (0, 1, 2)                                                                              | Per-pack state of charge from the `pe0`/`pe1`/`pe2` cloud-report fields. Distinct from the MQTT `marstek_battery_pack_soc_percent`: the HTTP path is authoritative when both are populated, but the MQTT series updates faster. |
| `marstek_battery_pack_fault_flags`                     | `pack` (0, 1, 2)                                                                              | Per-pack fault-flag bitmap from `b0f`/`b1f`/`b2f`. Non-zero means the pack has at least one active fault condition (see event code 75 `fault_flags_bitmap`). |
| `marstek_battery_pack_temperature_raw`                 |                                                                                               | Raw `tn` field from the cloud telemetry report. Scale unverified (likely deci-Celsius or a packed bitfield); do **not** divide by 10 without cross-checking against a physical thermometer. |


`marstek_up` is strictly tied to MQTT. Cloud reachability is tracked independently via `marstek_cloud_last_report_timestamp_seconds`.

Example PromQL alerts:

```promql
# Device hasn't sent a telemetry report in 15 minutes (steady-state cadence is ~10 min,
# so 900 s gives a single-interval grace period; use 600 s only if you need burst-phase coverage):
(time() - marstek_cloud_last_report_timestamp_seconds{endpoint="report"}) > 900

# Device hasn't synced time since boot (getDateInfoeu.php fires once at boot only,
# so this fires when the device hasn't rebooted in more than 24 h — use as a boot-loop detector):
(time() - marstek_cloud_last_report_timestamp_seconds{endpoint="date_info"}) > 86400

# A new, unrecognised firmware endpoint was seen — check the logs:
changes(marstek_cloud_last_unknown_request_timestamp_seconds[1h]) > 0

# Firmware version changed:
changes(count by (firmware_version) (marstek_device_info)[1h:]) > 0

# A new, unlabelled puterrinfo header index appeared — document and promote to a named metric:
max by (index) (marstek_cloud_solar_errinfo_header_value) unless on(index) marstek_cloud_solar_errinfo_header_value offset 7d
```

## Configuration

Configuration is loaded in order of precedence: **defaults → environment variables → CLI flags** (flags win).

`MARSTEK_MQTT_HOST` and `MARSTEK_DEVICE_ID` are **required** — the exporter exits with code 2 if either is missing.


| Environment Variable           | Flag                     | Default      | Description                                                                         |
| ------------------------------ | ------------------------ | ------------ | ----------------------------------------------------------------------------------- |
| `MARSTEK_MQTT_HOST`            | `--mqtt-host`            | *(required)* | Broker host                                                                         |
| `MARSTEK_MQTT_PORT`            | `--mqtt-port`            | `1883`       | Broker port                                                                         |
| `MARSTEK_MQTT_USERNAME`        | `--mqtt-username`        | `""`         | Optional broker username (empty = anonymous)                                        |
| `MARSTEK_MQTT_PASSWORD`        | `--mqtt-password`        | `""`         | Optional broker password                                                            |
| `MARSTEK_MQTT_PASSWORD_FILE`   | `--mqtt-password-file`   | `""`         | Path to file containing broker password; overrides `MQTT_PASSWORD` if set           |
| `MARSTEK_MQTT_CLIENT_ID`       | `--mqtt-client-id`       | auto         | MQTT client ID (auto-generated as `marstek-exporter-<hostname>-<pid>` if empty)     |
| `MARSTEK_DEVICE_TYPE`          | `--device-type`          | `HMJ-2`      | MQTT topic device type segment                                                      |
| `MARSTEK_DEVICE_ID`            | `--device-id`            | *(required)* | MQTT topic device ID segment                                                        |
| `MARSTEK_POLL_INTERVAL`        | `--poll-interval`        | `30s`        | How often to send `cd=1`                                                            |
| `MARSTEK_RESPONSE_TIMEOUT`     | `--response-timeout`     | `8s`         | Max wait for device response                                                        |
| `MARSTEK_METRIC_TTL`           | `--metric-ttl`           | `3×poll-interval` | How long to keep device gauge values after the last successful update before dropping them from `/metrics`; empty = 3× poll interval |
| `MARSTEK_LISTEN_ADDR`          | `--listen-addr`          | `:9734`      | HTTP metrics listen address                                                         |
| `MARSTEK_LOG_LEVEL`            | `--log-level`            | `info`       | Log level: `debug`, `info`, `warn`, `error`                                         |
| `MARSTEK_LOG_FORMAT`           | `--log-format`           | `text`       | Log format: `text` or `json` (Docker image defaults to `json`)                      |
| `MARSTEK_LOG_SOURCE`           | `--log-source`           | `false`      | Add source file/line to log records                                                 |
| `MARSTEK_EMULATOR_LISTEN_ADDR` | `--emulator-listen-addr` | `""`         | Listen address for the cloud emulator; **empty = disabled**                         |
| `MARSTEK_EMULATOR_TZ`          | `--emulator-tz`          | `""`         | Timezone for the time-sync response (e.g. `Europe/Berlin`); empty = system timezone |


## Usage

### Docker Compose

```yaml
services:
  marstek-exporter:
    image: ghcr.io/lucavb/prometheus-marstek-mqtt-exporter:latest
    environment:
      - MARSTEK_MQTT_HOST=<your-mqtt-broker-host>   # required
      - MARSTEK_DEVICE_ID=<your-device-id>           # required
    ports:
      - "9734:9734"
    restart: unless-stopped
```

The image includes a built-in healthcheck on `/health`.

> **Note:** The GHCR package is private by default after the first push. To make it public, go to **GitHub → Packages → prometheus-marstek-mqtt-exporter → Package settings → Change visibility**.

### Prometheus Configuration

```yaml
scrape_configs:
  - job_name: marstek
    static_configs:
      - targets: ["marstek-exporter:9734"]
```

### Binary

```bash
./marstek-exporter --mqtt-host <your-mqtt-broker-host> --device-id <your-device-id>
```

## Cloud emulator (optional)

Marstek battery devices contact the vendor cloud server (`eu.hamedata.com` on port 80) for three purposes:

1. **Time sync** — `GET /app/neng/getDateInfoeu.php` — the device synchronises its real-time clock. This request fires **once per boot**, immediately after the device establishes its MQTT connection. It does not repeat in normal operation, so `marstek_cloud_last_report_timestamp_seconds{endpoint="date_info"}` is effectively a last-reboot timestamp.

2. **Telemetry report** — `GET /prod/api/v1/setB2500Report` — the device uploads an encrypted status blob (see [Telemetry report encryption](#telemetry-report-encryption) below). The upload cadence is **bimodal**:
   - **Burst phase** (~15 s interval, lasting ~8 min after boot/reconnect): the device reports aggressively while settling.
   - **Steady-state** (~10 min interval, indefinite): the device settles into a slow background rate.

   Set staleness alerts against `marstek_cloud_last_report_timestamp_seconds{endpoint="report"}` with a threshold of at least 900 s (one interval + grace); 600 s will produce false positives in steady-state.

3. **Error-event log** — `POST /app/Solar/puterrinfo.php` — the device uploads a buffered batch of error/event transitions as `code.timestamp.value` triples. The server always returns a fixed `_1` ack. This is **event-driven**, not periodic; a single upload typically covers the last 24 h of events.

When the cloud is unreachable the device can behave erratically. By running the built-in emulator and redirecting `eu.hamedata.com` to the exporter host on your LAN, both calls are answered locally with byte-compatible responses, keeping the device stable — completely offline.

### Enabling the emulator

Set `MARSTEK_EMULATOR_LISTEN_ADDR=:80` (or the equivalent CLI flag). The emulator is **disabled by default** because it requires port 80, which may conflict with other services.

```yaml
services:
  marstek-exporter:
    image: ghcr.io/lucavb/prometheus-marstek-mqtt-exporter:latest
    environment:
      - MARSTEK_MQTT_HOST=<your-mqtt-broker-host>
      - MARSTEK_DEVICE_ID=<your-device-id>
      - MARSTEK_EMULATOR_LISTEN_ADDR=:80
      - MARSTEK_EMULATOR_TZ=Europe/Berlin   # replace with your timezone
    ports:
      - "9734:9734"   # Prometheus metrics
      - "80:80"       # Cloud emulator — must be port 80
    restart: unless-stopped
```

> **Note:** The `TZ` environment variable used by the Go runtime is also a valid way to set the system timezone. `MARSTEK_EMULATOR_TZ` takes precedence over it for the emulator response only.

### DNS hijack (your responsibility)

The exporter does **not** perform any DNS rewriting. You must configure your LAN so that `eu.hamedata.com` resolves to the IP address of the host running the exporter. Common approaches:

- Override the DNS record in your router's DNS settings.
- Add a local DNS override in a resolver like Pi-hole or AdGuard Home.
- Add an entry to the hosts file on your router or gateway.

Port 80 is mandatory — the device firmware hardcodes it and does not use HTTPS.

### Telemetry report encryption

The `v=` query parameter on `GET /prod/api/v1/setB2500Report` is **AES-128-ECB** encrypted with the fixed 16-byte ASCII key `hamedatahamedata` (the vendor string `hamedata` repeated twice), followed by standard PKCS#7 padding. The plaintext is a URL-encoded `key=value&...` query string.

A single captured sample (firmware `HMJ-2 fcv=202310231502`) decrypts to **51 fields** including several not available via the MQTT `cd=1` path:

| Field(s) | Description |
|---|---|
| `b0max`, `b0min`, `b0maxn`, `b0minn` (also `b1*`/`b2*`) | Per-pack min/max cell voltage (mV) and cell index |
| `pe0`, `pe1`, `pe2` | Per-pack state of charge (%). The MQTT `cd=0` path only carries the aggregate `pe` plus `a0`/`a1`/`a2` channels |
| `b0f`, `b1f`, `b2f` | Per-pack fault-flag bitmap. Non-zero means an active fault; same byte as event code 75 (`fault_flags_bitmap`) in `emulator/solar_errinfo_codes.go` |
| `tn` | Pack temperature — scale unverified (likely deci-Celsius or a packed bitfield) |
| `pv1v`, `pv2v` | Per-solar-input voltage (mV) |
| `out1v`, `out2v` | Per-output-port voltage (mV) |
| `wbs` | Wi-Fi/Bluetooth status |
| `date` | Device self-reported local time |

The emulator decrypts every incoming report and exposes these as the `marstek_cell_voltage_millivolts`, `marstek_cloud_battery_pack_soc_percent`, `marstek_battery_pack_fault_flags`, `marstek_battery_pack_temperature_raw`, `marstek_solar_input_voltage_millivolts`, `marstek_output_voltage_millivolts`, `marstek_wifi_bt_status`, and `marstek_cloud_device_timestamp_seconds` metrics. Fields already exported via MQTT are not re-exported.

If a future firmware version rotates the key, `marstek_cloud_report_decode_errors_total` will increment and a `WARN` log line will appear. The reproduction script `scripts/crack_report.py` can be run against new pcap captures to recover a new key:

```bash
uv run --with scapy --with pycryptodome --python 3.12 \
    python3 scripts/crack_report.py capture.pcap
# or pass a base64url value directly:
uv run --with pycryptodome --python 3.12 \
    python3 scripts/crack_report.py --b64 '<value from v= parameter>'
```

**Prior art:** [`tomquist/marsrelay`](https://github.com/tomquist/marsrelay) and [`fignew/MarstACK`](https://github.com/fignew/MarstACK) both proxy this endpoint but treat `v=` as opaque. As of this writing, the AES-128-ECB key and plaintext schema have not been published elsewhere.

**Credits:** [`tomquist/hame-relay`](https://github.com/tomquist/hame-relay), [`tomquist/esphome-b2500`](https://github.com/tomquist/esphome-b2500), and [`tomquist/hm2mqtt`](https://github.com/tomquist/hm2mqtt) provided the MQTT-side groundwork and established that Hame firmware consistently uses short ASCII brand-name strings as AES keys.

**What this repo adds beyond prior art:** All three of the above projects, plus the community work on Hame/Marstek in general, focus on the MQTT and HTTP surfaces visible from outside the device. This repository is believed to be the first to publish:

- The `hamedatahamedata` AES-128-ECB key with a complete 51-field plaintext schema for `setB2500Report`.
- A full reverse-engineered decode of the 49-event `puterrinfo` error log, including trigger conditions and value semantics for every event code.
- The BLE OTA protocol at the packet level: GATT service `FF00`, characteristics `ff01`/`ff02`/`ff06`, opcode table, checksum algorithm, and the unauthenticated-RCE security finding. See [`docs/ota.md`](docs/ota.md).
- The Main MCU ↔ BMS I²C2 link: frame format, command table, charge/discharge algorithm literals, and the boundary between MCU-side and BMS-side logic. See [`docs/bms-protocol.md`](docs/bms-protocol.md).
- A working local cloud emulator that keeps the device stable offline, exposing 15+ additional metrics not available via MQTT.

### Discovery of new firmware endpoints

While the `solar_errinfo` payload schema is still being validated against real traffic, every upload to `/app/Solar/puterrinfo.php` is logged at **info** level with the raw body and a best-effort parse (uid, header integers, event count, distinct codes/values, oldest/newest timestamps). Once the format is confirmed this can be lowered to `debug`.

Any request that does not match a known path is:

- Logged at **warn** level with method, path, query string, remote address, user-agent, content-type, and a hex-encoded body snippet (up to 4 KiB). Rate-limited to one warn per path per minute so retries don't flood the log.
- Counted in `marstek_cloud_reports_total{endpoint="unknown"}`.
- Timestamped in `marstek_cloud_last_unknown_request_timestamp_seconds`.

This means a new firmware version introducing an unknown endpoint will surface loudly in your logs and via an alert on the gauge above, so you can investigate and report it upstream.

## How it works

The Marstek B2500-D (device type `HMJ-2`) uses the Hame MQTT protocol. Once configured to connect to a local broker, it listens on:

- **Command topic:** `hame_energy/HMJ-2/App/<device_id>/ctrl`
- **Status topic:** `hame_energy/HMJ-2/device/<device_id>/ctrl`

The device **does not push telemetry unprompted**. Publishing `cd=1` to the command topic causes it to immediately respond with a full status payload on the device topic. The exporter does this on every `--poll-interval`.

The status payload is a flat `key=value,key=value,...` string. The exporter splits it on `,`, maps known keys to Prometheus metrics, and updates the gauges. If no response arrives within `--response-timeout`, `marstek_up` is set to `0` and `marstek_scrape_errors_total` is incremented.

Automatic reconnection to the broker is handled by the Paho MQTT client (`SetAutoReconnect(true)`, `SetConnectRetry(true)`).

## Logging

The Docker image emits structured JSON on stdout (`MARSTEK_LOG_FORMAT=json`), which is ready for ingestion by any log collector that scrapes container stdout (Grafana Alloy, Promtail, Fluent Bit, Vector). Field names follow `log/slog` defaults (`time`, `level`, `msg`); levels are lowercase.

Example LogQL query in Grafana:

```logql
{app="marstek-exporter"} | json | level="error"
```

For local development the binary uses plain text output. Switch to JSON explicitly with `--log-format json`.

## Security findings

Two independent security findings have been documented from static analysis of
`firmware/B2500_All_HMJ.bin` and the companion Android app.

**1. Unauthenticated BLE OTA (remote code execution)**

Any BLE client within ~10 m can flash arbitrary firmware onto the B2500-D MCU.
No authentication, no pairing, no cryptographic signature — the only integrity
check is a `~sum()` byte-sum that is trivially forgeable. This is a remote code
execution primitive on a grid-tied power device.

Full write-up: [`docs/ota.md`](docs/ota.md)

**2. Shared AES-128-ECB key in cloud telemetry**

The `v=` parameter on `GET /prod/api/v1/setB2500Report` is AES-128-ECB with the
fixed key `hamedatahamedata`. The same key also encrypts the AWS IoT mTLS
credentials embedded in the firmware image. The key is embedded in the firmware
binary and was extracted from network captures.

See [§ Telemetry report encryption](#telemetry-report-encryption) below.

## Protocol + firmware reference

Deep-dive documentation lives under [`docs/`](docs/):

- [`docs/ota.md`](docs/ota.md) — BLE OTA protocol, flash layout, integrity check, threat model, APK corroboration, and security implications.
- [`docs/mqtt.md`](docs/mqtt.md) — MQTT topic layout, `cd=` command set, and per-field semantics (including which fields each Prometheus gauge is derived from).
- [`docs/firmware.md`](docs/firmware.md) — static-analysis notes on `firmware/B2500_All_HMJ.bin`: hardware target, AES key derivation, Ghidra symbols.
- [`docs/bms-protocol.md`](docs/bms-protocol.md) — reverse-engineered Main MCU ↔ BMS link (I²C2 @ `0x40005800` runtime bus + GPIO bit-bang boot probe), frame format, command table, charging-algorithm literals, and the hardware-tap path needed to unlock per-cell telemetry. Closes roadmap items 12 + 13.
- [`docs/roadmap.md`](docs/roadmap.md) — open and completed reverse-engineering items.
- [`scripts/probe_cd.py`](scripts/probe_cd.py) — read-only probe that enumerates `cd=` query responses against a live device (dry-run by default; opt-in to live transmission with `--i-know-what-im-doing`).

## Build

```bash
go build -o marstek-exporter .
```

### Future additions (not in v1)

- MQTT TLS (`MARSTEK_MQTT_TLS`, CA / cert / key) — the broker currently runs on plain `:1883`.

## License

MIT