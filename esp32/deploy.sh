#!/usr/bin/env bash
# Deploy MicroPython + Marstek bridge firmware to ESP32-S3.
#
# First-time setup (flash MicroPython):
#   bash deploy.sh --flash
#
# Subsequent deploys (update app files only):
#   cp config.example.json config.json   # fill in credentials once
#   bash deploy.sh

set -euo pipefail

FLASH=0
DEVICE=""

for arg in "$@"; do
    case "$arg" in
        --flash) FLASH=1 ;;
        *)       DEVICE="$arg" ;;
    esac
done

# Auto-detect device if not specified
if [ -z "$DEVICE" ]; then
    candidates=(/dev/cu.usbmodem*)
    if [ -e "${candidates[0]}" ]; then
        DEVICE="${candidates[0]}"
    else
        echo "ERROR: No ESP32 found. Plug in the device and retry, or pass the port:"
        echo "  bash deploy.sh /dev/cu.usbserial-XXXX"
        exit 1
    fi
fi
echo "Device: $DEVICE"

# ── Step 1: flash MicroPython (only with --flash) ───────────────────────────
if [ "$FLASH" -eq 1 ]; then
    FW="micropython-esp32s3.bin"
    if [ ! -f "$FW" ]; then
        echo "Downloading MicroPython for ESP32-S3…"
        # Latest stable generic S3 build (SPIRAM variant works on all S3 boards)
        curl -fsSL -o "$FW" \
            "https://micropython.org/resources/firmware/ESP32_GENERIC_S3-20241129-v1.24.1.bin"
    fi
    echo "Erasing flash…"
    uv run --with esptool esptool --chip esp32s3 --port "$DEVICE" erase-flash
    echo "Flashing MicroPython…"
    uv run --with esptool esptool --chip esp32s3 --port "$DEVICE" \
        write-flash -z 0 "$FW"
    echo "MicroPython flashed. Waiting for USB re-enumeration…"
    # ESP32-S3 USB-JTAG disappears then reappears after reset; poll until ready
    for i in $(seq 1 20); do
        sleep 1
        if [ -e "$DEVICE" ]; then
            echo "Device back on $DEVICE (${i}s)"
            break
        fi
        if [ "$i" -eq 20 ]; then
            echo "ERROR: device did not reappear at $DEVICE after 20s"
            exit 1
        fi
    done
    # Give MicroPython a few more seconds to finish booting
    sleep 4
fi

# ── Step 2: download microdot if needed ─────────────────────────────────────
if [ ! -f microdot.py ]; then
    echo "Downloading microdot.py…"
    curl -fsSL -o microdot.py \
        "https://raw.githubusercontent.com/miguelgrinberg/microdot/refs/heads/main/src/microdot/microdot.py"
fi

# ── Step 2b: download aioble if needed ───────────────────────────────────────
AIOBLE_BASE="https://raw.githubusercontent.com/micropython/micropython-lib/master/micropython/bluetooth/aioble/aioble"
AIOBLE_FILES="__init__.py central.py client.py core.py device.py peripheral.py security.py server.py"
if [ ! -d aioble ]; then
    echo "Downloading aioble…"
    mkdir -p aioble
    for f in $AIOBLE_FILES; do
        curl -fsSL -o "aioble/$f" "$AIOBLE_BASE/$f"
    done
fi

# ── Step 3: check config ─────────────────────────────────────────────────────
if [ ! -f config.json ]; then
    echo "ERROR: config.json not found. Copy config.example.json and fill in your credentials."
    exit 1
fi

# ── Step 4: upload app files (retry until REPL is ready) ────────────────────
echo "Uploading files…"
for attempt in 1 2 3 4 5; do
    if uv run --with mpremote mpremote connect "$DEVICE" \
        exec "import os
try: os.mkdir('aioble')
except: pass" \
        + cp aioble/__init__.py :aioble/__init__.py \
        + cp aioble/central.py :aioble/central.py \
        + cp aioble/client.py :aioble/client.py \
        + cp aioble/core.py :aioble/core.py \
        + cp aioble/device.py :aioble/device.py \
        + cp aioble/peripheral.py :aioble/peripheral.py \
        + cp aioble/security.py :aioble/security.py \
        + cp aioble/server.py :aioble/server.py \
        + cp microdot.py :microdot.py \
        + cp marstek_ble.py :marstek_ble.py \
        + cp webserver.py :webserver.py \
        + cp main.py :main.py \
        + cp config.json :config.json \
        + reset; then
        break
    fi
    if [ "$attempt" -eq 5 ]; then
        echo "ERROR: mpremote failed after 5 attempts"
        exit 1
    fi
    echo "REPL not ready yet, retrying in 3s… (attempt $attempt/5)"
    sleep 3
done

echo ""
echo "Done. Watch serial output with:"
echo "  uv run --with mpremote mpremote connect $DEVICE repl"
