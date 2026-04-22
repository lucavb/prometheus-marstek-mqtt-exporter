# Device Identification

## Observed device

| Field | Value | Source |
| --- | --- | --- |
| Marketed name | Marstek **B2500-D** | Product |
| Internal hardware ID (`aid`) | **HMJ-2** | MQTT topic / HTTP `aid=` param |
| MAC | `60:32:3b:d1:4b:6e` | MQTT topic / cloud `uid` |
| Cloud UID | `3601115030374d33300f1365` | HTTP `uid=` param |
| Running firmware version | **v110.9** (`sv=110`, `sbv=9`) | MQTT telemetry and HTTP query string |
| MCU / sub-version (`mv`) | `105` | HTTP `mv=` param |
| Firmware compile stamp (`fc`) | `202310231502` | MQTT telemetry |

The MAC's OUI `60:32:3b` belongs to Shenzhen Hame Technology — the OEM that
produces the device and runs the cloud backend under the `hamedata.com` domain.

## Marstek B-series hardware → firmware family mapping

From community reverse-engineering (`noone2k/hm2500pub` wiki) and confirmed by
the set of `B2500_All_*.bin` files published on the vendor CDN:

| Hardware ID | Marstek model | Firmware branch |
| --- | --- | --- |
| `HMB-x` | B2500 Gen 1 | `v1xx` (123 → 144.x) |
| `HMA-x` | B2500 Gen 1.2 / V2 | `v2xx` (119 → 232.x) |
| `HMK-x` | B2500 Gen 3 | `v2xx` (same branch as HMA) |
| **`HMJ-x`** | **B2500-D** | **`v1xx` (100 → 118.x)** |

The suffix after the dash (e.g. `-2`) is a label/OEM variant. `HMJ-2` is the
Marstek-branded B2500-D. Identical hardware ships as Bluepalm, Be Cool, Revolt,
GreenSolar, etc.

## Firmware version naming

The captured HTTP request exposes the three version numbers the device reports:

```
sv=110   software (app) version → major firmware number
sbv=9    sub-version             → minor firmware number
mv=105   MCU / bootloader version
fc=202310231502  firmware compile timestamp (YYYYMMDDHHMM)
```

Running firmware is therefore presented as **`v110.9`** (MCU `v105`).

## Known HMJ firmware versions

From the community changelog
([hm2500pub wiki](https://github.com/noone2k/hm2500pub/wiki/Changelogs)):

| Version | Notes |
| --- | --- |
| 100 | Ship firmware |
| 101, 104.6, 106.20 | early updates |
| 108.2 | AES crypto layer added |
| **110.6 / 110.9** | **current on this device** — 110.9 added ecotracker support |
| 112.2, 113.1, 114.19, 115.1 | interim releases |
| 116.6 | Noted as most stable; only released on request |
| 118.2 / 118.3 | Latest known (Aug 2025) |
