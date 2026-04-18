# prometheus-marstek-mqtt-exporter

Prometheus exporter for the Marstek B2500-D home battery, using MQTT.

Connects to a local MQTT broker, periodically polls the battery by publishing `cd=1` to its command topic, parses the status response, and exposes the data as Prometheus metrics on an HTTP `/metrics` endpoint.

No BLE, no Marstek cloud — purely MQTT.

## Metrics

All metrics carry the labels `device_type` and `device_id`.

| Metric | Labels | Description |
|--------|--------|-------------|
| `marstek_battery_soc_percent` | | State of charge in percent |
| `marstek_battery_remaining_wh` | | Remaining battery capacity in Wh |
| `marstek_battery_dod_percent` | | Depth of discharge setting in percent |
| `marstek_output_threshold_watts` | | Minimum load to engage output in watts |
| `marstek_daily_battery_charge_wh` | | Daily battery charge energy in Wh (resets at midnight) |
| `marstek_daily_battery_discharge_wh` | | Daily battery discharge energy in Wh (resets at midnight) |
| `marstek_daily_load_charge_wh` | | Daily load charge energy in Wh |
| `marstek_daily_load_discharge_wh` | | Daily load discharge energy in Wh |
| `marstek_rated_output_watts` | | Rated output power in watts |
| `marstek_rated_input_watts` | | Rated input power in watts |
| `marstek_surplus_feed_in_enabled` | | 1 if surplus feed-in is enabled, 0 otherwise |
| `marstek_up` | | 1 if the last poll received a response, 0 otherwise |
| `marstek_last_update_timestamp_seconds` | | Unix timestamp of the last successful update |
| `marstek_solar_input_watts` | `input` (1, 2) | Solar input power in watts |
| `marstek_output_watts` | `output` (1, 2) | Output power in watts |
| `marstek_output_enabled` | `output` (1, 2) | Output enabled state (1=on, 0=off) |
| `marstek_temperature_celsius` | `sensor` (min, max) | Device temperature in Celsius |
| `marstek_extra_pack_connected` | `pack` (1, 2) | Extra battery pack connected (1=yes, 0=no) |
| `marstek_scrapes_total` | | Total number of cd=1 polls sent |
| `marstek_scrape_errors_total` | | Polls that received no response within the timeout |

## Configuration

Configuration is loaded in order of precedence: **defaults → environment variables → CLI flags** (flags win).

| Environment Variable | Flag | Default | Description |
|---------------------|------|---------|-------------|
| `MARSTEK_MQTT_HOST` | `--mqtt-host` | `10.1.1.5` | Broker host |
| `MARSTEK_MQTT_PORT` | `--mqtt-port` | `1883` | Broker port |
| `MARSTEK_MQTT_USERNAME` | `--mqtt-username` | `""` | Optional broker username (empty = anonymous) |
| `MARSTEK_MQTT_PASSWORD` | `--mqtt-password` | `""` | Optional broker password |
| `MARSTEK_MQTT_PASSWORD_FILE` | `--mqtt-password-file` | `""` | Path to file containing broker password; overrides `MQTT_PASSWORD` if set |
| `MARSTEK_MQTT_CLIENT_ID` | `--mqtt-client-id` | auto | MQTT client ID (auto-generated as `marstek-exporter-<hostname>-<pid>` if empty) |
| `MARSTEK_DEVICE_TYPE` | `--device-type` | `HMJ-2` | MQTT topic device type segment |
| `MARSTEK_DEVICE_ID` | `--device-id` | `60323bd14b6e` | MQTT topic device ID segment |
| `MARSTEK_POLL_INTERVAL` | `--poll-interval` | `30s` | How often to send `cd=1` |
| `MARSTEK_RESPONSE_TIMEOUT` | `--response-timeout` | `8s` | Max wait for device response |
| `MARSTEK_LISTEN_ADDR` | `--listen-addr` | `:9734` | HTTP metrics listen address |
| `MARSTEK_LOG_LEVEL` | `--log-level` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `MARSTEK_LOG_FORMAT` | `--log-format` | `text` | Log format: `text` or `json` (Docker image defaults to `json`) |
| `MARSTEK_LOG_SOURCE` | `--log-source` | `false` | Add source file/line to log records |

## Usage

### Docker Compose

```yaml
services:
  marstek-exporter:
    image: ghcr.io/lucavb/prometheus-marstek-mqtt-exporter:latest
    environment:
      - MARSTEK_MQTT_HOST=10.1.1.5
      - MARSTEK_DEVICE_ID=60323bd14b6e
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
./marstek-exporter --mqtt-host 10.1.1.5 --device-id 60323bd14b6e
```

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
