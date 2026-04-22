# Firmware

## Quick reference — B2500-D (this device)

```
URL:           https://www.hamedata.com/app/download/neng/B2500_All_HMJ.bin
Real IP:       120.25.59.188  (Alibaba Cloud, CN — www.hamedata.com)
Size:          167 936 bytes (164 KB)
Last-Modified: Fri, 26 Sep 2025 07:11:46 GMT
SHA-256:       a63d26ed6decc19bc73e969e0b4fa89912edc0b106b080df12984b5cbe24f5a9
Local copy:    firmware/B2500_All_HMJ.bin
```

Download, bypassing the local DNS mock:

```bash
curl -sSL --resolve www.hamedata.com:443:120.25.59.188 \
  https://www.hamedata.com/app/download/neng/B2500_All_HMJ.bin \
  -o firmware/B2500_All_HMJ.bin
```

## Binary format

The file is **not** wrapped in an OTA container — it's a raw flash image for
the main MCU. First 32 bytes:

```
00000000: b030 0020 b501 0008 bd01 0008 bf01 0008
00000010: c101 0008 c301 0008 c501 0008 0000 0000
```

Interpreted little-endian:


| Offset  | Value        | Meaning                                                      |
| ------- | ------------ | ------------------------------------------------------------ |
| `0x00`  | `0x20003030` | Initial stack pointer (RAM @ `0x20000000`)                   |
| `0x04`  | `0x080001b5` | Reset vector (thumb bit set → Cortex-M)                      |
| `0x08`… | `0x080001…`  | NMI / HardFault / MemManage / BusFault / UsageFault handlers |


That is the canonical **ARM Cortex-M vector table** for an STM32-family MCU
loaded at flash base `0x08000000`.

Implications:

- The image is raw firmware, ready to be flashed at `0x08000000` via SWD / DFU
/ UART bootloader — no header, no CRC tail (though the boot process may
still do its own integrity check in flash).
- A Cortex-M disassembler (Ghidra, IDA, radare2 with `arm / 16`) can load it
with base address `0x08000000` and vector-table parsing enabled.

## All B2500 variants currently published on the CDN

Confirmed 2026-04-22 via HEAD requests:


| Variant           | URL                        | Size          | Last-Modified  |
| ----------------- | -------------------------- | ------------- | -------------- |
| HMA (Gen 1.2/V2)  | `…/neng/B2500_All_HMA.bin` | 198 656 B     | 2025-09-15     |
| HMB (Gen 1)       | `…/neng/B2500_All_HMB.bin` | 104 448 B     | 2025-02-13     |
| **HMJ (B2500-D)** | `…/neng/B2500_All_HMJ.bin` | **167 936 B** | **2025-09-26** |
| HMK (Gen 3)       | `…/neng/B2500_All_HMK.bin` | 147 456 B     | 2025-06-13     |


Each `_All_` file is the rolling "current stable" blob. Specific-version
blobs for staged rollouts live at:

```
http://www.hamedata.com/app/neng/admin/upload/<model>V<hw>_All_V<fw>.bin
     e.g. …/B2500V23_All_V221.1.bin
https://eu.hamedata.com/ems/uploads/ota/YYYYMMDD/<hash>.bin
```

The URL that ends up in the MQTT OTA trigger from the cloud will normally be
one of those explicit-version paths, not the `_All_` alias.

## Other known firmware URLs extracted from the APK

Catalog of every `…/app/download/neng/*.bin` string found in the Flutter Dart
snapshot. Kept here as a reference for siblings and related Hame products
that may share code paths.

```
A1000_All.bin           A1000PRO_All.bin       A2200_All.bin          A500PRO_All.bin
AIOM_HMN.bin            AIOS_HMM.bin           AIOS_HMM_BMS.bin       AIOS_HMM_INV.bin
AIOS_HMM_MPPT.bin       B1200_All_HMF.bin
B2500_All_HMA.bin       B2500_All_HMB.bin      B2500_All_HMJ.bin      B2500_All_HMK.bin
CT002_All.bin           CT003_All.bin          E1000S_All.bin
I-02KS_All_HMI.bin      I-0350_All_HMI.bin     I-0500_All_HMI.bin     I-0600_All_HMI.bin
I-0700_All_HMI.bin      I-0800_All_HMI.bin     I-0900_All_HMI.bin     I-1000_All_HMI.bin
I-1000_LAOHUA_HMI.bin   I-2000_All_HMI.bin     I-350S_All_HMI.bin     I-500S_All_HMI.bin
M1200_All.bin           M1200N_All.bin         M2200_All.bin          M2200N_All.bin
M2200N_HV_All.bin       M2200N_LV_All.bin      M3600_All.bin
M5000_All_HMC1.bin  …  M5000_All_HMC7.bin    M5000_All_SCH1.bin
M5000N_BMS.bin          M5000N_BMS_PACK1.bin   M5000N_BMS_PACK2.bin
M5000N_INV.bin          M5000N_MPPT.bin        M5000N_PMU.bin         MC5000_DCDC.bin
PACK1_V6000.bin         V3000_PMU_All.bin      V6000_BMS.bin          V6000_INV.bin
V6000_PMU_All.bin       S1000PRO_All.bin       S2200_All.bin          S500PRO_All.bin
Sensor_All.bin          SMR0_All.bin           SMR1_All.bin           SMR2_All.bin
TPM2_All.bin            vnse3_ota_130.bin      vnse3_ota_131.bin      vnse3_ota_release.bin
Mars2.apk
```

Plus the companion Venus-E OTA file for the Quectel FC41D Wi-Fi chipset,
which the community had already documented:

```
http://www.hamedata.com/app/download/neng/HM_HIE_FC41D_remote_ota.rbl
```

## Static analysis of `B2500_All_HMJ.bin`

Reproduce with:

```bash
uv run scripts/analyze_firmware.py firmware/B2500_All_HMJ.bin
uv run scripts/decode_fw_blobs.py  firmware/B2500_All_HMJ.bin
uv run scripts/decrypt_fw_blobs.py firmware/B2500_All_HMJ.bin
```

### Hardware target


| Evidence                                                                | Conclusion                                                                                     |
| ----------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| Vector table at `0x08000000`, thumb bit set on every handler            | ARMv7-M / Cortex-M (Thumb-2)                                                                   |
| Refs to `0xE0042000` DBGMCU_IDCODE, `0x40021000` RCC, `0x40010000` AFIO | STM32F1-family (or pin-compatible GigaDevice GD32 clone)                                       |
| ~86 IRQ vectors after the 16-entry Cortex-M exception table             | "Connectivity-line" STM32F1 (F105/F107) or GD32F1x0 equivalent                                 |
| Image size `0x29000` (164 KiB), initial SP `0x200030B0`                 | ≥ 192 KiB flash, ≥ 16 KiB SRAM part — most likely a GD32F103/F107 in a ≥ 256 KiB flash package |


The reset handler at `0x080001b4` is the canonical **Keil MDK / µVision
`__main` bootstrap** — `LDR r0, [PC,#0x18]; BLX r0` (→ `SystemInit`),
`LDR r0, [PC,#0x18]; BX r0` (→ `__main` / scatter-load), followed by the
default infinite-loop fault handlers and `__aeabi_memcpy4` /
`__aeabi_memset`.

### Flash layout

From the block-entropy scan (`scripts/analyze_firmware.py`):


| File range                    | Size                | Content                                                                                                                  |
| ----------------------------- | ------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `0x00000 – 0x00400`           | 1 KiB               | Cortex-M vector table + IRQ vector table (~86 entries)                                                                   |
| `0x00400 – 0x03800`           | 13 KiB              | Startup / HAL / AT-command driver code                                                                                   |
| `0x03800 – 0x04000`           | 2 KiB               | **Model-variant string table #1** (HMA-1…14, HMK-1…11, HMJ-1…14 × `"HM_B2500"`)                                          |
| `0x04000 – 0x07000`           | 12 KiB              | **Erased (`0xFF`)** — reserved hole, per-device config/calibration page populated at provisioning                        |
| `0x07000 – 0x07800`           | 2 KiB               | Template config block: `0x00020018 0xFF21E8D2 0x11223344 0x55667788` then zeros                                          |
| `0x07800 – 0x24C00`           | ~118 KiB            | Main application (MQTT / BLE / HTTP / battery FSM)                                                                       |
| `0x24C00 – 0x26400`           | 6 KiB               | **16-bit lookup tables** (likely NTC temperature curve + battery OCV-SoC; values decrease monotonically in 2-byte steps) |
| `0x26400 – 0x26D00`           | ~2 KiB              | Mixed code; contains the **AES S-boxes**                                                                                 |
| `0x26820 / 0x26E7C / 0x2776C` | 1.2 / 1.7 / 1.2 KiB | **Three Base64-wrapped, AES-encrypted blobs** — TLS credentials pushed to the Quectel radio via `AT+QSSLCERT`            |
| `0x27C00 – 0x28800`           | 3 KiB               | Large `printf`-style MQTT report format strings                                                                          |
| `0x28800 – 0x28C00`           | 1 KiB               | Trailing code (version helpers / CRC dispatch table)                                                                     |
| `0x28C00 – 0x29000`           | 1 KiB               | Image tail: a small obfuscated table + `0xFF` padding                                                                    |


The 12 KiB erased gap at `0x4000` is a 3-page hole the OTA blob ships empty.
Deployed devices almost certainly write their serial number, calibration
constants, saved Wi-Fi credentials and stored cloud tokens there.

### AES crypto material

`binwalk` finds both AES lookup tables contiguous with each other:

```
0x265d8  forward S-box  (0x100 bytes, 63 7c 77 7b …)
0x266d8  inverse S-box  (0x100 bytes, 52 09 6a d5 …)
```

This confirms the release note "firmware 108 added AES". Immediately after
the S-boxes sit three base64 strings. Their decoded contents are ≈ 1.2 –
1.7 KiB with byte-entropy **7.83 – 7.89 bits/byte** — indistinguishable
from random. Two of them (`0x26820` and `0x2776C`) share an **identical
first 16 decoded bytes**:

```
c271391d 458ec38d 818fa825 0c74cdfe    (first AES block of both blobs)
```

That is the fingerprint of **AES-ECB (or AES-CBC with a fixed IV)
applied to two plaintexts whose first 16 bytes are identical**. Given
the nearby AT-command strings (`AT+QSSLCERT="CA"/"User Cert"/"User Key"`) and the PEM headers inside, both colliding slots are indeed
certificates whose plaintext starts with `-----BEGIN CERTIFICATE-----\n`
— exactly 16 bytes that get mapped to the same ciphertext block under
ECB.

### Decryption — the key is already known

The same 16-byte literal the `setB2500Report` cloud endpoint uses —
`hamedatahamedata`, reverse-engineered from `marstek-2.pcap` captures
and implemented in `[emulator/report.go](../emulator/report.go)` as
`DecryptReport` — also decrypts the three firmware blobs. Mode is
**AES-128-ECB with PKCS#7 padding**. The ASCII literal `"hamedata"`
appears 5× in the image near offset `0x28400`; the full 16-byte key is
assembled at runtime by doubling it.

Running `[scripts/decrypt_fw_blobs.py](../scripts/decrypt_fw_blobs.py)`
writes the three PEMs to `firmware/decrypted/` and yields:


| Offset    | Plaintext size | What it actually is                                                     |
| --------- | -------------- | ----------------------------------------------------------------------- |
| `0x26820` | 1 208 B        | **Amazon Root CA 1** (public, `CN=Amazon Root CA 1`, serial `06…5BCA`)  |
| `0x26E7C` | 1 706 B        | **RSA-2048 private key** (matches the cert at `0x2776C`)                |
| `0x2776C` | 1 244 B        | **AWS IoT device certificate** (`CN=AWS IoT Certificate`, valid → 2049) |


`openssl rsa -modulus` on the private key and on the cert's public key
produce the same SHA-256 (`996f4439b57b176c…`), so it is a
self-consistent mTLS identity. The cert subject carries no per-device
fields, so **every B2500-D unit running firmware 108 ships with the
same AWS IoT Core credential** — a single fleet-wide identity baked
into the OTA image rather than a per-device one.

The naive "CA → User Cert → User Key" mapping from offset order is
wrong in the middle: the image stores CA, **key**, **cert**, not CA,
cert, key. Matches the `AT+QSSLCERT` sequence in which the radio needs
its CA first and expects the key-then-cert pair last.

The backing AWS IoT endpoint hostname is **not** present as plaintext
anywhere in the image (`rg` for `amazonaws.com`, `.iot.`, `-ats` all
come back empty), so it is presumably returned by a hamedata.com
bootstrap call and pushed into the Quectel radio via `AT+QMTCFG` /
`AT+QMTOPEN` at runtime.

One consequence worth flagging: the same 128-bit key protects both the
in-transit `setB2500Report` telemetry **and** the at-rest AWS IoT
credentials. Rotating either one requires rotating the other, which is
presumably why they haven't.

### Architectural picture

Cross-referencing the 86 distinct `AT+…` strings, the firmware is a
**dumb master + smart radio** design: the STM32 is the application CPU,
and a **Quectel FC41D Wi-Fi/BLE SiP** on a UART is driven entirely by AT
commands. The MCU does **not** run its own TCP/IP stack.

Command surface by subsystem:


| Subsystem   | Commands used                                                                                                                                          |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| BLE         | `QBLEINIT`, `QBLEADDR?`, `QBLENAME`, `QBLEGATTSSRV`, `QBLEGATTSCHAR`, `QBLEADVPARAM`, `QBLEADVSTART/STOP`, `QBLEGATTSNTFY`, `QBLETRANMODE`, `QBLESTAT` |
| HTTP        | `QHTTPCFG "url"/"sslctxid"`, `QHTTPGET`, `QHTTPPOST`, `QHTTPREAD`                                                                                      |
| Raw TCP/UDP | `QIOPEN=1,"UDP SERVICE"`, `QIOPEN=2,"TCP"`, `QISEND`, `QIRD`, `QICLOSE`, `QISTATE?`                                                                    |
| MQTT        | `QMTOPEN`, `QMTCONN`, `QMTSUB`, `QMTPUB`, `QMTCLOSE`, `QMTCFG "ssl"/"version"/"datatype"/"keepalive"`                                                  |
| TLS         | `QSSLCERT "CA"/"User Cert"/"User Key"`, `QSSLCFG "verify"/"ciphersuite"/"version"`                                                                     |
| Wi-Fi       | `QSTAAPINFODEF`, `QSTAST`, `QGETWIFISTATE`, `QGETIP=station`, `QWLANOTA` (radio firmware OTA), `QVERSION`, `QRST`                                      |


### Cloud / LAN endpoints baked in

Literal format strings, by purpose:


| Kind                      | Template (from flash)                                                                       |
| ------------------------- | ------------------------------------------------------------------------------------------- |
| Device enumeration (boot) | `http://%s.hamedata.com/app/neng/getDateInfo%s.php?uid=%s&fcv=%s&aid=%s&sv=%d&sbv=%d&mv=%d` |
| Error reporting           | `http://%s.hamedata.com/app/Solar/puterrinfo.php`                                           |
| Fire/fault lookup         | `http://eu.hamedata.com/ems/api/v1/getDeviceFire?devid=%s`                                  |
| Real-time SoC             | `http://%s.hamedata.com/ems/api/v1/getRealtimeSoc?devid=%s&type=%s`                         |
| Daily telemetry           | `http://%s.hamedata.com/prod/api/v1/setB2500Report?v=%s`                                    |
| Generic (MCU → radio)     | `http://%s/v1/json`                                                                         |
| Cloud MQTT control topic  | `hame_energy/<mac>/App/<app>/ctrl` **and** `marstek_energy/<mac>/App/<app>/ctrl`            |
| Cloud MQTT device topic   | `hame_energy/<mac>/device/<mac>/ctrl` (+ `marstek_energy/…`)                                |
| **LAN CT meter read**     | `{"id":1,"method":"EM.GetStatus","params":{"id":0}}` and `EM1.GetStatus`                    |


The last row is significant: this is the **Shelly Pro EM / Shelly Gen2+
JSON-RPC request**, sent over a local TCP socket opened with
`AT+QISEND=1,…,"%s","%s",%d` (at `0x01eaa4`). The B2500-D can therefore
read a Shelly CT-clamp meter on the LAN directly, without the cloud —
a useful integration point for the exporter.

### MQTT payload format strings

The long format strings at `0x27DF0`, `0x27EB0`, `0x280F0`, `0x2826C`
and `0x28824` are the `printf` templates for `/ctrl`-channel reports.
They match the `ReportXXX` structs in `emulator/report.go`.

Notable ones:

- `**cd=221` full settings snapshot (~650 B)** at `0x027EB0` — includes
HMJ-only fields `fk_chg_`*, `fk_dsg_`*, `ct_t`, `tc_dis`, `fktc`,
`lmo/lmi/lmf`.
- `**cd=222` CT state (~200 B)** at `0x028944`:
`ct_t=%d,phase_t=%d,dchrg_t=%d,seq_s=%d,c0=%d,c1=%d,c2=%d,cp=%d,op=%d`.
- `**cd=206` calibration raw** at `0x028824` — a hex-indexed ADC
calibration dump with 48 `aN/bN/cN` triplets the cloud can poll.

### BLE GATT schema (what the phone app connects to)

Set up once at boot:

```
Service  FF00
 ├── Char ff01   (write, request from phone)
 ├── Char ff02   (notify, response from device)
 └── Char ff06   (probably "large transfer" / OTA channel)

Advertise name: "HM_B2500_XXXX"   (XXXX = last 4 ASCII hex of MAC)
Advertise interval: 150 ms
```

Notifications are emitted via `AT+QBLEGATTSNTFY=ff02,<len>,<hex>`, where
`<hex>` is the encoded MQTT-style `cd=…` string from the payload-format
table — i.e. the BLE and MQTT channels carry the same wire format.

### Model / hardware-revision table

A 6×-repeated block covering `HMA-1…14`, `HMK-1…11`, `HMJ-1…14`, each
paired with `"HM_B2500"`, occupies parts of `0x03800–0x03C00` and
`0x25268–0x26580`. The repetition with a fixed stride is the footprint
of an array of **per-variant configuration structs** where the first
field is the string and the remaining fields are GPIO / calibration /
feature-flag values. Mapping those fields is the natural next step if
we want to understand what makes HMJ different from HMA/HMK at runtime.

### Versioning on the wire

The MCU self-announces over BLE/MQTT with:

```
type=%s,id=%s,mac=%s,version=%d.%d,uboot_version=%d
```

The two integers after `version=` are the `NNN.M` pairs from the rollout
table below. `uboot_version` tracks the STM32 bootloader image separately
from the application blob, and is not exposed in any of the public
version strings.

## Firmware version scheme (HMJ branch)

Firmware the B2500-D (HMJ) has been seen on:


| Version                            | Notes                                        |
| ---------------------------------- | -------------------------------------------- |
| 100                                | Ship firmware (V4 first release, 23.12.2024) |
| 101, 104.6, 106.20, 108.2          | Early updates (108 added AES crypto layer)   |
| **110.6 → 110.9**                  | Current device is on **110.9**               |
| 112.2, 113.1, 114.19, 115.1, 116.6 | Staged stable releases                       |
| 118.2, 118.3                       | Latest known (Aug 2025)                      |


Versions in the same branch are fetched from the same `_All_HMJ.bin` alias;
the MQTT OTA trigger picks the actual per-version upload.