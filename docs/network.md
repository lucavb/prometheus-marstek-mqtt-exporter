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
```

### Bypassing the mock

Use `curl --resolve` (or `/etc/hosts`) to hit the real servers from this
machine without disabling the mock:

```bash
curl -sSLI --resolve www.hamedata.com:443:120.25.59.188 \
  https://www.hamedata.com/app/download/neng/B2500_All_HMJ.bin
```

## Hosts observed / extracted

| Host | Real IP | Role |
| --- | --- | --- |
| `eu.hamedata.com` | `3.122.27.237`, `3.68.141.219` (AWS eu-central-1) | EU production API + MQTT broker |
| `eu-staging.hamedata.com` | n/a (in APK) | EU staging API |
| `eu-dev.hamedata.com` | n/a (in APK) | EU dev API |
| `us.hamedata.com` | n/a (in APK) | US production API |
| `www.hamedata.com` | `120.25.59.188` (Alibaba Cloud, CN) | **Firmware / OTA CDN** |

The EU API gateway sits behind **Kong** (identifiable by `X-Kong-Request-Id`
and `X-Kong-Response-Latency` headers). Unknown routes are answered by a
default handler with `{"code":8,"msg":"Forbidden"}`, which looks like a 403
but is actually the gateway's fallback — not a real indicator of an existing
protected endpoint.

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

This is **only** a time feed. It does **not** return an OTA/upgrade URL,
regardless of the `sv`/`sbv`/`mv` values passed.

When `eu.hamedata.com` is unreachable, the battery's self-adjustment mode
stops functioning — this is that endpoint.

### 2. Telemetry upload (encrypted)

```
GET http://eu.hamedata.com/prod/api/v1/setB2500Report?v=<base64-like-blob>
```

The `v` parameter is the encrypted telemetry payload (same data the device
also publishes over MQTT). New firmwares (HMJ ≥ 108, HMA/HMK ≥ 226) use an
AES crypto layer on this payload.

## HTTP endpoints extracted from the APK (not yet observed on the wire)

All found as plaintext in `libapp.so`. Legacy PHP API (`/app/Solar/…`) and
newer REST API (`/ems/api/…`) are both present.

### Legacy Solar/neng PHP API — `http://www.hamedata.com/`

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

### New EMS API — `https://eu.hamedata.com/`

```
/ems/api/v3/mConfig                 app/device runtime config
/ems/api/v1/getAppCodeUrl?app_name=Mars
/ems/api/v1/getAppRomSystem?app_name=Mars
/ems/apk/marstek/index.html         app landing page
/ems/uploads/ota/YYYYMMDD/<hash>.bin   newer per-release OTA files
```

### Privacy policy documents

```
https://eu.hamedata.com/app/privacy-policy/marstek/<lang>-doc.html
```

## MQTT broker

- Host: `eu.hamedata.com`, port **1883** (plain MQTT, no TLS)
- Observed in pcaps as `10.1.1.5:1883` because of the local DNS mock
- Topic scheme: [`mqtt.md`](mqtt.md)

Because MQTT is plaintext and device-reported (no client certificate), it is
trivial to intercept on the LAN by redirecting `eu.hamedata.com` to a local
broker. That is how both this project and `hm2mqtt` obtain the telemetry.

## Summary

For the Prometheus exporter we only need **MQTT** on the LAN. The cloud
endpoints are documented here purely so we know what the device is reaching
out to (and so we can keep a stub broker/responder happy if the device expects
them).
