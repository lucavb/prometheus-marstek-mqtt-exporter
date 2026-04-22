# Marstek B2500-D Reverse-Engineering Notes

Working notes from investigating a Marstek **B2500-D** balcony battery for the
purpose of building a local Prometheus exporter.

These docs capture what was learned from:

- Packet captures of the device's WAN traffic (`marstek*.pcap` in the repo root)
- Decompiling the official Android app (`com.hamedata.marstek`, v1.6.61)
- Probing the cloud infrastructure directly with `curl` (bypassing local DNS
  mocking by resolving the vendor's real IPs via public DNS)

## Index

| File | Contents |
| --- | --- |
| [`device.md`](device.md) | Hardware identification, firmware version scheme, model/hardware mapping |
| [`network.md`](network.md) | Cloud endpoints, real IPs, DNS behaviour, network-mocking bypass |
| [`mqtt.md`](mqtt.md) | MQTT topic structure and telemetry payload fields |
| [`firmware.md`](firmware.md) | Firmware download URLs, binary format, version history |
| [`apk-analysis.md`](apk-analysis.md) | How the Flutter APK was dissected and what was extracted |
| [`roadmap.md`](roadmap.md) | Open reverse-engineering leads and nice-to-have follow-ups |

## TL;DR

- The device in this repo is a **B2500-D** whose internal hardware ID is
  **HMJ-2**, running firmware **v110.9** at the time of capture.
- It talks to **`eu.hamedata.com`** for MQTT (port 1883) and a time-sync HTTP
  endpoint; firmware binaries are served from **`www.hamedata.com`**
  (Alibaba Cloud, China).
- Firmware for this exact device:
  `https://www.hamedata.com/app/download/neng/B2500_All_HMJ.bin`
- All status/control traffic is plain-text MQTT with a `key=value,key=value`
  payload on topic `hame_energy/HMJ-2/device/<mac>/ctrl`.
