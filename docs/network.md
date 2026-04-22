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
| `POST /app/Solar/puterrinfo.php`                | 200                | Returns literal `_2` for empty body (likely an "accepted N entries" counter)                                                                                  |
| `GET/POST /app/Solar/get_device.php`            | **timeout on :80** | `www.hamedata.com` on Alibaba CN does not actually answer anything but `/app/download/neng/*.bin` on port 80. The legacy PHP API host has not yet been found. |


The `Mars2_*.apk` / `update.zip` pipeline is served from
`**static-eu.marstekenergy.com`**, which is **CloudFront → S3**
(`x-amz-cf-pop: MUC50-P3`, `server: AmazonS3`). Together with the baked
AWS IoT credential in the firmware, this confirms that **Marstek's EU
infrastructure sits on AWS**; only the Chinese `www.hamedata.com` firmware
CDN is on Alibaba Cloud.

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

## Summary

For the Prometheus exporter we only need **MQTT** on the LAN. The cloud
endpoints are documented here purely so we know what the device is reaching
out to (and so we can keep a stub broker/responder happy if the device expects
them).