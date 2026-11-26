# ESP32 HTTPS certificate provisioning

The ESP32 bridge normally serves its UI and API over plain HTTP on port 80.
This feature lets a controller — typically a cert-manager `CronJob` — push a
PEM certificate chain and private key to the device. The device stores them in
LittleFS, reboots once to apply them, and then serves the UI/API over HTTPS on
**port 443**. Port 80 is reduced to a certificate-management/redirect listener;
once TLS is active it refuses to accept the bearer token or private key in
cleartext.

```
cert-manager ──issues──▶ Kubernetes Secret (tls.crt / tls.key / ca.crt)
                              │
                    CronJob runs push_esp32_cert.py
                              │  POST /api/tls  + Bearer token
                              ▼
                        ESP32: validate → persist → reboot
                              │
                    HTTPS UI/API on :443
                    HTTP admin (403/redirect) on :80
```

## Behaviour

The feature is **opt-in**. With the default `"tls_enabled": false` the bridge
behaves exactly as before: plain HTTP on port 80, no certificate required, and
`/api/tls` returns 404. You can deploy/provision the device without any token,
certificate or private key.

Set `"tls_enabled": true` to opt in:

| State (`tls_enabled: true`) | Port 80 | Port 443 |
| --- | --- | --- |
| no certificate yet | full UI + API, including `POST /api/tls` | closed |
| usable certificate | admin only: `/api/tls` returns **403 HTTPS required**; other `/api/*` paths 308 to HTTPS; `/` redirects if Host matches the cert CN | full UI + API over TLS |
| certificate present but TLS unusable | admin only: `POST /api/tls` still works for recovery; `/` returns 503 | closed |

`POST /api/tls` is allowed over HTTP only while TLS is not active (first
bootstrap or a recovery state where the stored pair cannot build a context).
Once TLS is active, use `https://device/`; for an expired certificate use
`--insecure` so the handshake still happens. The endpoint is idempotent on the
**full pair** (certificate chain + key): the same bytes return
`"changed": false` and do not reboot; a changed key or intermediate chain with
the same leaf is installed.

## Persistence and reboots

The certificate chain + key live in `/tls/pair.pem`, metadata in
`/tls/meta.json`, and `tls_enabled` / `tls_push_token` in `config.json`. All of
it survives `machine.reset()`, power cycles and normal `deploy.sh` updates.
The pair is committed as one file, so a power loss cannot leave a new cert
paired with an old key. On boot the firmware reads it again and starts HTTPS if
`tls_enabled=true` and the pair is usable.

Recovery details:

- `deploy.sh --flash` erases the whole flash (including `/tls`); that is the
  only deploy path that discards the certificate. A normal `deploy.sh` does
  not. Every deploy also copies the host `config.json` over the device's copy,
  so keep `tls_enabled` and `tls_push_token` in that host file or the next
  deploy will disable them (the pair itself stays in `/tls`).
- If the stored pair is malformed and cannot build a context, boot starts the
  admin-only listener on port 80; `POST /api/tls` remains available there to
  repair it.
- mbedTLS does not verify that the key matches the certificate when loading a
  pair. Boot therefore performs a loopback TLS handshake; a mismatched pair
  fails it and lands in the admin-only state, where `POST /api/tls` can replace
  it. A push of the exact same bytes needs `--force` (the endpoint is
  idempotent).
- `tls_enabled=false` disables HTTPS and the push endpoint without deleting
  the stored pair; re-enabling it serves the same pair.

## Device configuration

Opt in and add a bearer token in `esp32/config.json`:

```json
{
  "wifi_ssid": "MyIoT",
  "wifi_password": "...",
  "tls_enabled": true,
  "tls_push_token": "REPLACE_WITH_AT_LEAST_16_RANDOM_CHARACTERS"
}
```

Generate the token with `openssl rand -base64 32`. Setting `tls_enabled` does
**not** require a certificate or private key: the device boots over HTTP and
waits for the first push. The push endpoint is disabled (HTTP 404) until the
token is at least 16 characters; an empty or short token is logged at boot and
never enables an unauthenticated endpoint.

The token is the only device-specific secret needed by the controller, but the
certificate/key in the cert-manager Secret are sensitive too. Store the token
in a Kubernetes `Secret`, not in the CronJob manifest; pass it through the
environment rather than `--token` on a command line. `tls_enabled` and
`tls_push_token` live in the device's `config.json` and must be JSON strings;
a non-string token disables the endpoint with a boot log instead of crashing.
They survive reboots just like the pushed pair.

## Private key formats and algorithms

The endpoint accepts **unencrypted PEM** keys in any of the common containers:

- PKCS#8: `-----BEGIN PRIVATE KEY-----` (RSA or EC)
- PKCS#1: `-----BEGIN RSA PRIVATE KEY-----`
- SEC1: `-----BEGIN EC PRIVATE KEY-----`

Encrypted keys (`BEGIN ENCRYPTED PRIVATE KEY`) are rejected because the
endpoint has no passphrase channel.

On the ESP32-S3 MicroPython 1.24.1 image used by `deploy.sh`, the mbedTLS build
supports the TLS 1.2 server key types below. This is what matters for a
cert-manager `Certificate`:

| Key | Status | Notes |
| --- | --- | --- |
| ECDSA P-256 | **recommended** | smallest handshake cost; cert-manager `algorithm: ECDSA`, `size: 256` |
| ECDSA P-384 | supported | works, more CPU/RAM per handshake |
| ECDSA P-521 | compiled | technically works, heavier; avoid unless required |
| secp256k1 | compiled | browser/TLS client support is inconsistent; avoid |
| RSA 2048 | supported | use if your issuer requires RSA; more handshake cost than P-256 |
| RSA 3072 / 4096 | supported | 4096 is noticeably slower and can strain the handshake heap |
| Ed25519 / X25519 | **not enabled** | MicroPython disables the 25519 curve in its ESP32 mbedTLS config |
| DSA | not supported | not a practical TLS certificate choice here |

P-192/P-224 are technically compiled into mbedTLS but are obsolete and should
not be used for new certificates. The certificate signature algorithm should be
SHA-256 (SHA-384/512 also work); RSA PKCS#1 v1.5 and ECDSA certificate chains
are the well-tested paths. TLS 1.2 is the reliable server configuration.
This table reflects the pinned ESP-IDF mbedTLS configuration; CI does not
exercise real handshakes, so verify exotic algorithms on the board before
relying on them.

The key must match the certificate. mbedTLS does not reject a mismatched pair
at `load_cert_chain` time, so boot verifies the pair with a loopback TLS
handshake. A mismatch lands in the admin-only state; push the corrected
certificate/key over HTTP (or used `--force` if the bytes are identical to what
is already stored).

## API

### `POST /api/tls`

Headers:

```
Authorization: Bearer <tls_push_token>
Content-Type: application/json
```

Body:

```json
{
  "certificate": "-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----\n",
  "private_key": "-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----\n",
  "force": false
}
```

`force` is optional; set it to `true` (or pass `--force` to the pusher) to
rewrite and reboot even when the exact pair is already stored. This is for
recovering a transient/context failure with identical bytes; normal renewals
should omit it.

`certificate` may contain a full chain (leaf first, followed by intermediates).
Accepted key encodings are PKCS#8 (`BEGIN PRIVATE KEY`), PKCS#1
(`BEGIN RSA PRIVATE KEY`) and SEC1 (`BEGIN EC PRIVATE KEY`). Encrypted private
keys are rejected. Limits: 12 KiB for the certificate chain, 8 KiB for the key,
24 KiB for the whole request; microdot returns `413` above that.

Success:

```json
{
  "ok": true,
  "changed": true,
  "fingerprint_sha256": "c0e799e50e422efc...",
  "reboot_in_s": 3
}
```

`fingerprint_sha256` is the standard SHA-256 fingerprint of the leaf
certificate DER, matching `openssl x509 -fingerprint -sha256`. When the exact
pair is already installed the response is `{"ok": true, "changed": false, ...}`
and no reboot is scheduled; a changed key or intermediate chain with the same
leaf is still `changed: true`.

Errors are JSON `{"error": "...", "code": "..."}`: `400` invalid PEM/key,
`401` missing/bad token, `403` cleartext certificate operation while TLS is
active, `404` endpoint disabled, `413` body too large (microdot's plain-text
response), `415` wrong content type, `429` too many failed authentication
attempts. Some `400`/`415`/`500` responses omit `code`.

### `GET /api/tls`

Same bearer token. Returns metadata without ever exposing the private key:

```json
{
  "enabled": true,
  "installed": true,
  "valid": true,
  "active": true,
  "fingerprint_sha256": "c0e799e50e422efc...",
  "common_name": "esp32.lan",
  "not_before": 1795694380,
  "not_after": 1811236780,
  "valid_for_days": 180,
  "chain_length": 2,
  "cert_bytes": 3120,
  "key_bytes": 241,
  "generation": 1,
  "reboot_pending": false
}
```

`valid` means the stored pair parsed and looks like a certificate/key; `active`
means the HTTPS listener is actually running. `active=false` with
`installed=true` means the device is in the admin-only recovery state. The
device has no RTC/NTP, so `not_after` is an absolute epoch value rather than a
computed "days left".

### `curl` examples

Bootstrap (device still on HTTP):

```bash
jq -n --rawfile crt tls.crt --rawfile key tls.key \
  '{certificate: $crt, private_key: $key}' |
curl -fsS -X POST http://esp32.local/api/tls \
  -H "Authorization: Bearer $ESP32_TLS_TOKEN" \
  -H "Content-Type: application/json" \
  --data-binary @-
```

Renewal / status over HTTPS:

```bash
curl -fsS --cacert ca.crt \
  -H "Authorization: Bearer $ESP32_TLS_TOKEN" \
  https://esp32.local/api/tls | jq
```

## Host-side pusher

[`scripts/push_esp32_cert.py`](../scripts/push_esp32_cert.py) is stdlib-only and
does the file reading, JSON encoding, retries, TLS verification and idempotency
checks for you (export `ESP32_TLS_TOKEN`):

```bash
python3 scripts/push_esp32_cert.py \
  --url https://esp32.local --ca ca.crt \
  --cert tls.crt --key tls.key
```

Use `--status` to inspect the device, `--insecure` only for a self-signed
device or an expired certificate, and `ESP32_URL` / `ESP32_TLS_TOKEN` /
`ESP32_CA_FILE` for environment-driven deployments. The script never follows
HTTP redirects, so the token and key cannot be forwarded to another origin.

## cert-manager CronJob

cert-manager itself does **not** push certificates to devices: it writes
`tls.crt`, `tls.key` and (when the issuer provides one) `ca.crt` into a
Kubernetes `Secret`. A CronJob that reads that Secret and calls the endpoint is
the supported pattern.

### Certificate

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: esp32-tls
  namespace: iot
spec:
  secretName: esp32-tls
  duration: 2160h      # 90 days
  renewBefore: 720h    # renew with 30 days left
  privateKey:
    algorithm: ECDSA
    size: 256
    # Explicit: cert-manager >= 1.18 defaults to Always, older releases to Never.
    rotationPolicy: Always
  dnsNames:
    - esp32.lan
  issuerRef:
    name: internal-ca
    kind: ClusterIssuer
```

ECDSA P-256 is recommended on the ESP32: the TLS handshake needs roughly
25–40 KiB of heap per connection, and RSA handshakes are significantly more
expensive. RSA 2048 works; 4096 is likely to be painful.

### Token secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: esp32-tls-push
  namespace: iot
stringData:
  token: "REPLACE_WITH_THE_DEVICE_TLS_PUSH_TOKEN"
```

### Pusher script ConfigMap

```bash
kubectl -n iot create configmap esp32-cert-pusher \
  --from-file=push_esp32_cert.py=scripts/push_esp32_cert.py
```

### CronJob

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: esp32-cert-push
  namespace: iot
spec:
  schedule: "17 */6 * * *"
  concurrencyPolicy: Forbid
  successfulJobsHistoryLimit: 1
  failedJobsHistoryLimit: 3
  jobTemplate:
    spec:
      backoffLimit: 2
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: push
              image: python:3.12-slim
              command: ["python", "/scripts/push_esp32_cert.py"]
              args:
                - "--url=https://esp32.lan"
                - "--ca=/certs/ca.crt"
                - "--cert=/certs/tls.crt"
                - "--key=/certs/tls.key"
              env:
                - name: ESP32_TLS_TOKEN
                  valueFrom:
                    secretKeyRef:
                      name: esp32-tls-push
                      key: token
              volumeMounts:
                - name: tls
                  mountPath: /certs
                  readOnly: true
                - name: script
                  mountPath: /scripts
                  readOnly: true
          volumes:
            - name: tls
              secret:
                secretName: esp32-tls
            - name: script
              configMap:
                name: esp32-cert-pusher
```

The CronJob reads the Secret at every run, so it naturally picks up a renewed
certificate. Running it every few hours is cheap: an unchanged full pair
(chain + key) is detected by hash and does not reboot the device.

If the issuer is public (for example Let's Encrypt) the Secret may not contain
`ca.crt`; drop `--ca` and the container's normal trust store is used. For a
private CA, `ca.crt` is required to verify `https://esp32.lan` unless you use
`--insecure`.

The first certificate must be bootstrapped manually, because the CronJob above
uses `https://` and the device only serves HTTP until a certificate exists.
Enable `tls_enabled` and `tls_push_token` in `config.json`, deploy, wait for the
device to reconnect, export `ESP32_TLS_TOKEN`, then run:

```bash
python3 scripts/push_esp32_cert.py \
  --url http://esp32.lan \
  --cert tls.crt --key tls.key
```

After the reboot, run the same command with `--url https://esp32.lan --ca ca.crt`
to verify the TLS listener before relying on the CronJob.

If the installed certificate has already expired, the CronJob's normal CA
verification fails. Recover with an encrypted-but-unverified handshake:

```bash
python3 scripts/push_esp32_cert.py \
  --url https://esp32.lan --insecure \
  --cert tls.crt --key tls.key
```

If the stored pair is structurally invalid or the loopback self-test failed
(the device is in admin-only mode on :80), the bootstrap command with
`--url http://esp32.lan` still works. Use `--force` when re-sending the exact
same bytes during recovery.

## Applying the certificate

A changed certificate triggers a clean reboot after about 3 seconds, so the
existing BLE/WiFi supervisor starts from a fresh heap and mbedTLS builds one
server context. The HTTP response is sent first. Expect a short telemetry gap;
polling resumes automatically. Same-pair pushes do not reboot.

If the stored pair cannot build a server context (for example a truncated key
or unsupported encoding), or if the loopback self-test fails (mismatched key or
unsupported TLS), the device serves only the admin listener on port 80. Its
`POST /api/tls` remains available to repair the pair; use `--force` for an
identical re-push.

## Security notes

- The entire feature is opt-in; with `tls_enabled=false` the device has no
  HTTPS listener and `/api/tls` returns 404, so a normal deployment needs no
  key material or token.
- The bearer token is compared in constant time for equal-length tokens
  (length is not secret). Valid tokens are checked before the failure window,
  so a brute-force budget cannot lock out the legitimate renewer; ten failed
  attempts in 60 seconds make further invalid attempts return `429`.
- When TLS is active, port 80 exposes only a root redirect, `308` upgrades for
  other `/api/*` paths, and a `403` for `/api/tls`; it does not expose status
  or accept certificate operations. The UI and the rest of the API are on 443.
- HTTP bootstrap **and HTTP recovery** send the private key in clear text.
  Do them on a trusted LAN, or pre-provision over USB. Renewals must use HTTPS
  with `--ca` (or `--insecure` for an expired certificate).
- The private key is stored unencrypted in LittleFS; physical access to the
  board means key compromise. This design is for a trusted home/IoT network,
  not a hostile physical environment.
- The pair is written as one file, so a power loss cannot commit a new cert
  with an old key. An oversized, malformed or encrypted key is rejected with a
  `400` and leaves the installed pair untouched.
- There is no replay/rollback protection: possession of the token allows
  installing any syntactically valid pair, including an older one. Rotate the
  token if it is exposed. TLS on :443 encrypts the link but does not
  authenticate clients; the pre-existing control API remains unauthenticated
  for LAN clients.
- Pushing a key generated outside the device is a deliberate trade-off of the
  push model. A CSR/EST/ACME flow where the key never leaves the ESP32 is the
  stronger long-term design.

## Firmware requirement

Server-side TLS in `asyncio.start_server(..., ssl=...)` requires MicroPython
1.24 or newer. `deploy.sh` pins 1.24.1. On an older image the firmware refuses
to start the TLS listener; with a stored certificate it serves only the admin
listener on port 80. Check the boot log for
`TLS requires MicroPython >= 1.24`. The same admin-only
fallback is used if the startup TLS probe fails, so the full control app is
never exposed on :443 in plaintext. A persistent `:443` failure after a
previous success keeps the `:80` admin listener stable and re-enables HTTP
recovery after repeated failures instead of reboot-looping the board.

## Exporter impact

Once TLS is active, the exporter's optional ESP32 recovery must use HTTPS:

```bash
export MARSTEK_ESP32_BASE_URL=https://esp32.lan
```

Go trusts the system CA store. For a private CA, point the container at the CA
bundle (Go honours `SSL_CERT_FILE` on Linux) or add it to
`/etc/ssl/certs/ca-certificates.crt`. The certificate must contain the exact
DNS name (or IP SAN) used in `MARSTEK_ESP32_BASE_URL`. Port 80 answers
`/api/*` with a `308` upgrade to HTTPS (or `403` if the Host does not match the
certificate CN), so switching the base URL to HTTPS is required, not optional.
