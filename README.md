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
| `marstek_scrapes_total`                 |                     | Total number of cd=1 polls sent                           |
| `marstek_scrape_errors_total`           |                     | Polls that received no response within the timeout        |


### Cloud emulator metrics (only present when `MARSTEK_EMULATOR_LISTEN_ADDR` is set)


| Metric                                                 | Labels                                                                                        | Description                                                                                                                             |
| ------------------------------------------------------ | --------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `marstek_device_info`                                  | `uid`, `device_type_reported`, `firmware_version`, `sw_version`, `sub_version`, `mod_version` | Device metadata parsed from the cloud time-sync request. Value is always 1; use label values for joins or alerts.                       |
| `marstek_cloud_reports_total`                          | `endpoint` (`date_info`, `report`, `solar_errinfo`, `unknown`)                                | Total requests received by the cloud emulator per endpoint.                                                                             |
| `marstek_cloud_last_report_timestamp_seconds`          | `endpoint` (`date_info`, `report`, `solar_errinfo`)                                           | Unix timestamp of the last request per known endpoint.                                                                                  |
| `marstek_cloud_last_unknown_request_timestamp_seconds` |                                                                                               | Unix timestamp of the last request to an unrecognised endpoint. Non-zero means a new firmware endpoint was discovered — check the logs. |
| `marstek_cloud_report_payload_bytes`                   |                                                                                               | Decoded byte size of the latest telemetry report payload. A change may indicate a firmware update.                                      |


`marstek_up` is strictly tied to MQTT. Cloud reachability is tracked independently via `marstek_cloud_last_report_timestamp_seconds`.

Example PromQL alerts:

```promql
# Device hasn't contacted the cloud emulator in 10 minutes:
(time() - marstek_cloud_last_report_timestamp_seconds{endpoint="date_info"}) > 600

# A new, unrecognised firmware endpoint was seen — check the logs:
changes(marstek_cloud_last_unknown_request_timestamp_seconds[1h]) > 0

# Firmware version changed:
changes(count by (firmware_version) (marstek_device_info)[1h:]) > 0
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

Marstek battery devices periodically connect to the vendor cloud server (`eu.hamedata.com` on port 80) for two purposes:

1. **Time sync** — `GET /app/neng/getDateInfoeu.php` — the device synchronises its real-time clock.
2. **Telemetry report** — `GET /prod/api/v1/setB2500Report` — the device uploads an encrypted status blob.
3. **Error-event log** — `POST /app/Solar/puterrinfo.php` — the device uploads a buffered batch of error/event transitions as `code.timestamp.value` triples. The server always returns a fixed `_1` ack.

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

## Build

```bash
go build -o marstek-exporter .
```

### Future additions (not in v1)

- MQTT TLS (`MARSTEK_MQTT_TLS`, CA / cert / key) — the broker currently runs on plain `:1883`.

## License

MIT