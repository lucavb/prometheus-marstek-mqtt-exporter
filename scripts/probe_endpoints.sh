#!/usr/bin/env bash
# Probe every URL the B2500-D firmware and APK know about, bypassing local
# DNS mocks via curl --resolve. Uses blatantly-fake device identifiers so
# the requests cannot be confused with any real device.
#
# Usage:  scripts/probe_endpoints.sh
#
# Only GETs / read-only probes + one intentionally malformed POST body and
# one correctly-encrypted (but faked-content) setB2500Report body, to show
# the error shape and write behaviour of the telemetry endpoint.

set -u

EU_IP="${EU_IP:-3.122.27.237}"      # eu.hamedata.com (AWS eu-central-1)
WWW_IP="${WWW_IP:-120.25.59.188}"   # www.hamedata.com (Alibaba CN)
US_IP="${US_IP:-35.172.49.157}"     # us.hamedata.com

# -- fake placeholders --------------------------------------------------------
UID_FAKE="00000000000000000000AAAA"     # 24-hex cloud uid
MAC_FAKE="aabbccddeeff"                  # fake mac, locally-administered bit set
AID_FAKE="HMJ-2"                         # public hardware id
SV_FAKE="110"
SBV_FAKE="9"
MV_FAKE="105"
FCV_FAKE="202310231502"
TYPE_FAKE="2"

CURL_OPTS=(
  --silent --show-error
  --max-time 10
  --connect-timeout 5
  --output /dev/stdout
  --write-out "\n--- HTTP %{http_code} | %{size_download}B | %{time_total}s ---\n"
)

probe() {
  local title="$1" ; shift
  local host="$1"  ; shift
  local ip="$1"    ; shift
  local url="$1"   ; shift
  printf '\n============================================================\n'
  printf '  %s\n' "$title"
  printf '  %s\n' "$url"
  printf '============================================================\n'
  curl "${CURL_OPTS[@]}" \
    --resolve "${host}:80:${ip}" \
    --resolve "${host}:443:${ip}" \
    "$@" \
    "$url" | head -c 1400
  echo
}

# 1) Time sync / heartbeat
probe "1. getDateInfo  (time sync / heartbeat)" \
  "eu.hamedata.com" "$EU_IP" \
  "http://eu.hamedata.com/app/neng/getDateInfoeu.php?uid=${UID_FAKE}&fcv=${FCV_FAKE}&aid=${AID_FAKE}&sv=${SV_FAKE}&sbv=${SBV_FAKE}&mv=${MV_FAKE}"

# 2) Error-info upload (legacy PHP, hosted on www.hamedata.com)
probe "2. puterrinfo  (empty body -- expect reject/echo)" \
  "www.hamedata.com" "$WWW_IP" \
  "http://www.hamedata.com/app/Solar/puterrinfo.php" \
  -X POST --data ""

# 3) getRealtimeSoc
probe "3. getRealtimeSoc" \
  "eu.hamedata.com" "$EU_IP" \
  "http://eu.hamedata.com/ems/api/v1/getRealtimeSoc?devid=${MAC_FAKE}&type=${TYPE_FAKE}"

# 4) getDeviceFire  (the only hardcoded-region URL)
probe "4. getDeviceFire  (hardcoded eu.)" \
  "eu.hamedata.com" "$EU_IP" \
  "http://eu.hamedata.com/ems/api/v1/getDeviceFire?devid=${MAC_FAKE}"

# 5a) setB2500Report -- garbage v= baseline
probe "5a. setB2500Report  (garbage v=, baseline)" \
  "eu.hamedata.com" "$EU_IP" \
  "http://eu.hamedata.com/prod/api/v1/setB2500Report?v=DEADBEEF_not_real_ciphertext"

# 5b) setB2500Report -- properly AES-128-ECB/PKCS#7 encrypted with the
#     recovered key. Fake fields only. If the cloud decrypts and stores,
#     we expect a different response body vs 5a.
ENC_V="$(uv run --quiet scripts/encrypt_report.py \
    cd=1 \
    mac=${MAC_FAKE} \
    devid=${MAC_FAKE} \
    type=2 \
    version=110.9 \
    probe_only=1 2>/dev/null || echo ENCRYPT_FAILED)"
probe "5b. setB2500Report  (valid ciphertext, fake fields)" \
  "eu.hamedata.com" "$EU_IP" \
  "http://eu.hamedata.com/prod/api/v1/setB2500Report?v=${ENC_V}"

# 6) HTTPS fetch of the firmware manifest (HEAD only, read-only)
probe "6. firmware CDN   (HEAD only)" \
  "www.hamedata.com" "$WWW_IP" \
  "https://www.hamedata.com/app/download/neng/B2500_All_HMJ.bin" \
  -I -L

# 7) Same time-sync on us. region to see if it behaves the same
probe "7. getDateInfo  (us region, different shard)" \
  "us.hamedata.com" "$US_IP" \
  "http://us.hamedata.com/app/neng/getDateInfous.php?uid=${UID_FAKE}&fcv=${FCV_FAKE}&aid=${AID_FAKE}&sv=${SV_FAKE}&sbv=${SBV_FAKE}&mv=${MV_FAKE}"

# -- APK-extracted endpoints we previously never probed ----------------------
# These are documented in docs/network.md as "extracted from the APK".
# `mConfig` is the most interesting one: it's described as "app/device
# runtime config" and is the single most likely place for the AWS IoT
# endpoint + broker credentials to be returned.

# 8) mConfig (app runtime config -- prime candidate for cloud provisioning)
probe "8. mConfig  (app/device runtime config)" \
  "eu.hamedata.com" "$EU_IP" \
  "https://eu.hamedata.com/ems/api/v3/mConfig"

# 9) Same, HTTP; some gateways only route on one
probe "9. mConfig over HTTP" \
  "eu.hamedata.com" "$EU_IP" \
  "http://eu.hamedata.com/ems/api/v3/mConfig"

# 10) getAppCodeUrl -- "get app code URL" (likely app update channel)
probe "10. getAppCodeUrl?app_name=Mars" \
  "eu.hamedata.com" "$EU_IP" \
  "https://eu.hamedata.com/ems/api/v1/getAppCodeUrl?app_name=Mars"

# 11) getAppRomSystem -- "get app rom system" (possibly OTA manifest)
probe "11. getAppRomSystem?app_name=Mars" \
  "eu.hamedata.com" "$EU_IP" \
  "https://eu.hamedata.com/ems/api/v1/getAppRomSystem?app_name=Mars"

# 12) Legacy /app/Solar/get_device.php -- account-scoped, we have no session,
#     so we only want to see the error shape (does it demand a token? echo
#     back the fake uid?).
probe "12. get_device.php  (legacy PHP, no auth)" \
  "www.hamedata.com" "$WWW_IP" \
  "http://www.hamedata.com/app/Solar/get_device.php?uid=${UID_FAKE}" \
  -X POST

# 13) get_deviceinfo.php
probe "13. get_deviceinfo.php  (legacy PHP)" \
  "www.hamedata.com" "$WWW_IP" \
  "http://www.hamedata.com/app/Solar/get_deviceinfo.php?uid=${UID_FAKE}&devid=${MAC_FAKE}" \
  -X POST

# 14) Unknown route sanity check -- confirms the Kong default-response
#     fingerprint we noted in network.md.
probe "14. Kong default  (unknown route)" \
  "eu.hamedata.com" "$EU_IP" \
  "https://eu.hamedata.com/this/route/does/not/exist"
