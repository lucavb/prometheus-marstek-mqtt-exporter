"""HTTP server for the Marstek BLE bridge — microdot 2.x."""
import struct
import ujson
import marstek_ble as ble
from microdot import Microdot, Response

Response.default_content_type = "application/json"

app = Microdot()


# ---------------------------------------------------------------------------
# Parsers
# ---------------------------------------------------------------------------

def _parse_runtime_info(data: bytes) -> dict | None:
    if len(data) < 39 or data[0] != 0x73 or data[2] != 0x23 or data[3] != 0x03:
        return None

    def u16(o): return data[o] | (data[o+1] << 8)
    def s16(o):
        v = u16(o)
        return v - 65536 if v >= 32768 else v
    def u32(o): return data[o] | (data[o+1]<<8) | (data[o+2]<<16) | (data[o+3]<<24)

    dev_version = data[12]
    out1_cfg_enable = bool(data[14] & 0x01)
    out2_cfg_enable = bool(data[14] & 0x02)
    out1_active = data[16]
    out2_active = data[17]
    out1_power_w = u16(24)
    out2_power_w = u16(26)

    # FW 116+ changed output-state semantics: config bits can remain false while
    # runtime active/power clearly show outputs delivering power.
    if dev_version >= 116:
        out1_enable = bool(out1_active) or out1_power_w > 0
        out2_enable = bool(out2_active) or out2_power_w > 0
    else:
        out1_enable = out1_cfg_enable
        out2_enable = out2_cfg_enable

    r = {
        "in1_active":             bool(data[4] & 0x01),
        "in2_active":             bool(data[5] & 0x01),
        "in1_power_w":            u16(6),
        "in2_power_w":            u16(8),
        "soc_percent":            u16(10) / 10,
        "dev_version":            dev_version,
        "wifi_connected":         bool(data[15] & 0x01),
        "mqtt_connected":         bool(data[15] & 0x02),
        "out1_enable":            out1_enable,
        "out2_enable":            out2_enable,
        "out1_cfg_enable":        out1_cfg_enable,
        "out2_cfg_enable":        out2_cfg_enable,
        "out1_active":            out1_active,
        "out2_active":            out2_active,
        "dod":                    data[18],
        "remaining_capacity_wh":  u16(22),
        "out1_power_w":           out1_power_w,
        "out2_power_w":           out2_power_w,
        "time_hour":              data[31],
        "time_minute":            data[32],
        "temperature_low_c":      s16(33),
        "temperature_high_c":     s16(35),
    }
    if len(data) >= 45: r["daily_charge_wh"]    = u32(40)
    if len(data) >= 49: r["daily_discharge_wh"] = u32(44)
    if len(data) >= 53: r["daily_load_wh"]      = u32(48)
    return r


def _parse_ascii_payload(data: bytes, opcode: int) -> str | None:
    if len(data) < 5 or data[0] != 0x73 or data[2] != 0x23 or data[3] != opcode:
        return None
    payload = data[4:-1]
    text = payload.decode("ascii", "ignore")
    # Some payloads include leading non-printable bytes before ASCII text.
    text = "".join(ch for ch in text if 32 <= ord(ch) <= 126).strip()
    return text or None


def _parse_history(data: bytes) -> list:
    """Parse 9-byte event records from a query_history (0x30) data notification."""
    if len(data) < 5 or data[0] != 0x73 or data[2] != 0x23 or data[3] != 0x30:
        return []
    body = data[4:-1]
    records = []
    i = 0
    while i + 9 <= len(body):
        rec = body[i:i+9]
        ts = struct.unpack(">I", rec[2:6])[0]   # big-endian epoch
        val = struct.unpack(">h", rec[6:8])[0]  # big-endian signed int16
        records.append({
            "type": rec[0], "sub": rec[1],
            "ts": ts, "value": val, "flags": rec[8],
        })
        i += 9
    return records


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _err(msg: str, status: int = 503, code: str | None = None, hint: str | None = None, details: dict | None = None):
    body = {"error": msg}
    if code:
        body["code"] = code
    if hint:
        body["hint"] = hint
    if details:
        body["details"] = details
    return body, status


async def _cmd(opcode: int, payload: bytes = b"", opname: str = "command"):
    if not ble.connected:
        return _err(
            "not connected",
            503,
            code="ble_disconnected",
            hint="Wait for BLE status to become connected, then retry.",
            details={"opcode": opcode, "op": opname},
        )
    resp = await ble.send_command(opcode, payload)
    if resp is None:
        if not ble.connected:
            return _err(
                "BLE link dropped before response",
                502,
                code="ble_dropped",
                hint="Battery or bridge disconnected during the command. Reconnect and retry.",
                details={"opcode": opcode, "op": opname},
            )
        return _err(
            "no response from battery",
            502,
            code="battery_no_response",
            hint="Battery may be busy or restarting. Retry in a few seconds.",
            details={"opcode": opcode, "op": opname},
        )
    return {"ok": True, "raw": resp.hex(), "opcode": opcode, "op": opname}


# ---------------------------------------------------------------------------
# Status / device info
# ---------------------------------------------------------------------------

@app.get("/api/status")
async def api_status(req):
    status = {
        "connected": ble.connected,
        "ble_state": ble.state,
        "mtu": ble.negotiated_mtu,
    }
    if ble.last_error:
        status["ble_last_error"] = ble.last_error
    if ble.connected:
        resp = await ble.send_command(0x03)
        if resp:
            parsed = _parse_runtime_info(resp)
            if parsed:
                status.update(parsed)
            else:
                status["raw"] = resp.hex()
    body = "{" + ",".join('"' + k + '":' + ujson.dumps(status[k]) for k in sorted(status)) + "}"
    return body, 200, {"Content-Type": "application/json"}


@app.get("/api/device")
async def api_device(req):
    if not ble.connected:
        return _err("not connected")

    devname_resp = await ble.send_command(0x09)
    serial_resp = await ble.send_command(0x23)

    if devname_resp is None and serial_resp is None:
        return _err("no response from battery", 502)

    out = {}

    if devname_resp is not None:
        device_name = _parse_ascii_payload(devname_resp, 0x09)
        if device_name:
            out["device_name"] = device_name
        out["raw_name"] = devname_resp.hex()

    if serial_resp is not None:
        serial = _parse_ascii_payload(serial_resp, 0x23)
        if serial:
            out["serial"] = serial
        out["raw_serial"] = serial_resp.hex()

    return out if out else _err("unable to parse device info", 502)


# ---------------------------------------------------------------------------
# Schedule (0x12) — 5 × 7-byte slots
# ---------------------------------------------------------------------------

@app.post("/api/schedules")
async def api_set_schedules(req):
    body = req.json or {}
    slots = body.get("slots", [])
    if len(slots) != 5:
        return _err("exactly 5 slots required", 400)
    payload = b""
    for slot in slots:
        en = 1 if slot.get("enabled") else 0
        sh = max(0, min(23, int(slot.get("sh", 0))))
        sm = max(0, min(59, int(slot.get("sm", 0))))
        eh = max(0, min(23, int(slot.get("eh", 23))))
        em = max(0, min(59, int(slot.get("em", 59))))
        pw = max(0, min(9999, int(slot.get("power_w", 0))))
        payload += bytes([en, sh, sm, eh, em, pw & 0xFF, (pw >> 8) & 0xFF])
    return await _cmd(0x12, payload)


# ---------------------------------------------------------------------------
# Time sync (0x14)
# ---------------------------------------------------------------------------

@app.post("/api/time")
async def api_set_time(req):
    body = req.json or {}
    yr = max(0, min(255, int(body.get("year", 2026)) - 1900))
    mo = max(0, min(11, int(body.get("month", 1)) - 1))   # 0-indexed on wire
    dy = max(1, min(31, int(body.get("day", 1))))
    hr = max(0, min(23, int(body.get("hour", 0))))
    mn = max(0, min(59, int(body.get("minute", 0))))
    sc = max(0, min(59, int(body.get("second", 0))))
    tz = int(body.get("tz_minutes", 0)) & 0xFFFF
    payload = bytes([yr, mo, dy, hr, mn, sc, tz & 0xFF, (tz >> 8) & 0xFF])
    return await _cmd(0x14, payload)


# ---------------------------------------------------------------------------
# Surplus PV feed-in (0x35) — wire: 0x00 = enable, 0x01 = disable
# ---------------------------------------------------------------------------

@app.post("/api/surplus-feed")
async def api_surplus_feed(req):
    body = req.json or {}
    enabled = bool(body.get("enabled", True))
    flag = 0 if enabled else 1
    return await _cmd(0x35, bytes([flag]))


# ---------------------------------------------------------------------------
# Inverter config (0x2C) — 7-byte type/id block + max_power_W LE16
# ---------------------------------------------------------------------------

@app.post("/api/inverter")
async def api_inverter(req):
    body = req.json or {}
    id_hex = body.get("inverter_id", "ef0368011e0000").replace(" ", "").lower()
    max_w = max(0, min(9999, int(body.get("max_w", 800))))
    if len(id_hex) != 14:
        return _err("inverter_id must be 14 hex chars (7 bytes)", 400)
    try:
        id_bytes = bytes(int(id_hex[i:i+2], 16) for i in range(0, 14, 2))
    except Exception:
        return _err("invalid inverter_id hex", 400)
    payload = id_bytes + bytes([max_w & 0xFF, (max_w >> 8) & 0xFF])
    return await _cmd(0x2C, payload)


# ---------------------------------------------------------------------------
# Event history (0x30) — returns ack + data notification
# ---------------------------------------------------------------------------

@app.get("/api/history")
async def api_history(req):
    if not ble.connected:
        return _err("not connected")
    results = await ble.send_command_multi(0x30, b"\x01", count=2)
    if not results:
        return _err("no response", 502)
    records = []
    for r in results:
        if r and len(r) > 10 and r[3] == 0x30:
            records.extend(_parse_history(r))
    return {"records": records}


# ---------------------------------------------------------------------------
# WiFi / MQTT
# ---------------------------------------------------------------------------

@app.post("/api/wifi")
async def api_set_wifi(req):
    body = req.json or {}
    ssid = body.get("ssid", "")
    password = body.get("password", "")
    if not ssid:
        return _err("ssid required", 400)
    payload = (ssid + "<.,.>" + password).encode()
    return await _cmd(0x05, payload, "set WiFi")


@app.post("/api/mqtt")
async def api_set_mqtt(req):
    body = req.json or {}
    host = body.get("host", "")
    port = str(body.get("port", 1883))
    ssl  = "1" if body.get("ssl") else "0"
    user = body.get("user", "")
    pw   = body.get("password", "")
    if not host:
        return _err("host required", 400)
    payload = (ssl + "<.,.>" + host + "<.,.>" + port + "<.,.>" + user + "<.,.>" + pw + "<.,.>").encode()
    return await _cmd(0x20, payload, "set MQTT")


@app.post("/api/mqtt/reset")
async def api_mqtt_reset(req):
    return await _cmd(0x21, b"", "reset MQTT")


# ---------------------------------------------------------------------------
# DoD (0x0B)
# ---------------------------------------------------------------------------

@app.post("/api/dod")
async def api_set_dod(req):
    body = req.json or {}
    depth = body.get("depth")
    if not isinstance(depth, int) or not (0 <= depth <= 100):
        return _err("depth must be an integer 0–100", 400)
    return await _cmd(0x0B, bytes([depth]))


# ---------------------------------------------------------------------------
# Restart (0x25)
# ---------------------------------------------------------------------------

@app.post("/api/restart")
async def api_restart(req):
    if not ble.connected:
        return _err("not connected")
    await ble.send_command(0x25, b"\x01")
    return {"ok": True}


# ---------------------------------------------------------------------------
# Factory reset (0x26) — no ack; device reboots ~5 s later
# ---------------------------------------------------------------------------

@app.post("/api/factory-reset")
async def api_factory_reset(req):
    if not ble.connected:
        return _err("not connected")
    await ble.send_command(0x26, b"\x01")
    return {"ok": True}


# ---------------------------------------------------------------------------
# Hardware reset (FF06 aa-protocol)
# ---------------------------------------------------------------------------

@app.post("/api/hardware-reset")
async def api_hardware_reset(req):
    if not ble.connected:
        return _err("not connected")
    ok = await ble.send_ff06_reset()
    if not ok:
        return _err("hardware reset failed — FF06 characteristic unavailable", 502)
    return {"ok": True}


# ---------------------------------------------------------------------------
# HTML UI
# ---------------------------------------------------------------------------

_HTML = """\
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Marstek B2500</title>
<style>
:root{
  --bg:#0f172a;--surf:#1e293b;--surf2:#263448;--border:#334155;
  --muted:#475569;--sub:#94a3b8;--text:#f1f5f9;
  --primary:#3b82f6;--primary-d:#2563eb;
  --success:#22c55e;--warn:#f59e0b;--danger:#ef4444;
  --r:10px;
}
*{box-sizing:border-box;margin:0;padding:0}
body{
  background:var(--bg);color:var(--text);
  font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;
  font-size:14px;line-height:1.5;
  max-width:560px;margin:0 auto;padding:0 12px 80px;
}
header{
  display:flex;align-items:center;justify-content:space-between;
  padding:16px 4px 10px;position:sticky;top:0;
  background:var(--bg);z-index:10;
  border-bottom:1px solid var(--border);margin-bottom:12px;
}
header h1{font-size:1.05rem;font-weight:700;letter-spacing:-.3px}
.badge{
  display:flex;align-items:center;gap:5px;font-size:.72rem;font-weight:600;
  padding:4px 10px;border-radius:20px;background:var(--surf);
  border:1px solid var(--border);transition:.3s;
}
.badge .dot{width:7px;height:7px;border-radius:50%;background:var(--muted);transition:.3s}
.badge.ok{border-color:rgba(34,197,94,.3)}
.badge.ok .dot{background:var(--success)}
section,details{
  background:var(--surf);border:1px solid var(--border);
  border-radius:var(--r);padding:16px;margin-bottom:10px;
}
.ct{
  font-size:.68rem;font-weight:700;text-transform:uppercase;
  letter-spacing:.08em;color:var(--sub);margin-bottom:14px;
}
details>summary{
  cursor:pointer;list-style:none;display:flex;
  align-items:center;justify-content:space-between;user-select:none;
}
details>summary::-webkit-details-marker{display:none}
details>summary::after{content:'▾';color:var(--sub);font-size:.9rem;transition:.2s}
details[open]>summary::after{transform:rotate(180deg)}
details>summary .ct{margin-bottom:0}
details[open]>summary{margin-bottom:14px}
/* ── Status ── */
.conn-row{display:flex;gap:5px;margin-bottom:12px}
.cc{
  display:flex;align-items:center;gap:4px;font-size:.72rem;
  padding:2px 8px;border-radius:12px;border:1px solid var(--border);background:var(--surf2);
}
.cc .d{width:6px;height:6px;border-radius:50%;background:var(--muted)}
.cc.on .d{background:var(--success)}
.cc.on{border-color:rgba(34,197,94,.3)}
.soc-row{display:flex;align-items:center;gap:14px;margin-bottom:14px}
.soc-num{font-size:2.8rem;font-weight:800;line-height:1}
.soc-num .u{font-size:1.1rem;font-weight:400;color:var(--sub)}
.soc-col{flex:1}
.soc-track{height:10px;background:var(--border);border-radius:6px;overflow:hidden;margin-bottom:4px}
.soc-fill{height:100%;border-radius:6px;background:var(--success);transition:width .5s,background .3s}
.soc-meta{display:flex;justify-content:space-between;font-size:.73rem;color:var(--sub)}
.stat-grid{display:grid;grid-template-columns:repeat(3,1fr);gap:8px;margin-bottom:12px}
.stat-box{
  background:var(--surf2);border-radius:8px;padding:10px;
  border:1px solid var(--border);
}
.stat-box .lbl{font-size:.66rem;text-transform:uppercase;letter-spacing:.06em;color:var(--sub);margin-bottom:4px}
.stat-box .val{font-size:1.25rem;font-weight:700;line-height:1}
.stat-box .val .u{font-size:.72rem;font-weight:400;color:var(--sub)}
.stat-box .sv{font-size:.75rem;color:var(--sub);margin-top:2px}
.c-solar{color:var(--warn)}
.c-out{color:var(--success)}
.ch-row{display:flex;align-items:baseline;gap:4px;margin-top:5px}
.ch-n{font-size:.65rem;color:var(--muted);min-width:8px;flex-shrink:0}
.ch-v{font-size:1.2rem;font-weight:700;line-height:1}
.ch-u{font-size:.72rem;color:var(--sub)}
.chips{display:flex;flex-wrap:wrap;gap:6px}
.chip{
  font-size:.73rem;padding:3px 10px;border-radius:20px;
  background:var(--surf2);border:1px solid var(--border);color:var(--sub);
}
.chip b{color:var(--text)}
#ph{display:flex;align-items:center;gap:10px;color:var(--sub);padding:6px 0}
.spin{
  width:18px;height:18px;border-radius:50%;
  border:2px solid var(--border);border-top-color:var(--primary);
  animation:spin .7s linear infinite;flex-shrink:0;
}
@keyframes spin{to{transform:rotate(360deg)}}
/* ── Schedule ── */
.st{width:100%;border-collapse:collapse}
.st th{
  font-size:.66rem;text-transform:uppercase;letter-spacing:.06em;
  color:var(--sub);text-align:left;padding-bottom:8px;font-weight:600;
}
.st td{padding:5px 3px;vertical-align:middle;transition:background .2s}
.st tr+tr td{border-top:1px solid var(--border)}
.st tr.son td{background:rgba(34,197,94,.06)}
.sn{width:20px;text-align:center;color:var(--sub);font-size:.85rem;font-weight:600}
.stog{
  width:42px;padding:4px 0;border-radius:6px;border:1px solid var(--border);
  font-size:.7rem;font-weight:700;cursor:pointer;transition:.2s;
  background:var(--surf2);color:var(--muted);text-align:center;
}
.stog.on{background:rgba(34,197,94,.18);color:var(--success);border-color:rgba(34,197,94,.4)}
/* ── Toggle ── */
.tgl{position:relative;width:42px;height:23px;flex-shrink:0}
.tgl input{opacity:0;width:0;height:0}
.tsl{
  position:absolute;top:0;left:0;right:0;bottom:0;
  background:var(--border);border-radius:23px;cursor:pointer;transition:.25s;
}
.tsl:before{
  content:"";position:absolute;width:17px;height:17px;
  left:3px;top:3px;background:white;border-radius:50%;transition:.25s;
}
.tgl input:checked+.tsl{background:var(--success)}
.tgl input:checked+.tsl:before{transform:translateX(19px)}
/* ── Inputs ── */
input[type=time],input[type=number],input[type=text],input[type=password]{
  background:var(--surf2);color:var(--text);
  border:1px solid var(--border);border-radius:6px;
  padding:5px 8px;font-size:.82rem;width:100%;
}
input[type=number]{-moz-appearance:textfield}
input[type=number]::-webkit-inner-spin-button{opacity:1}
input[type=time]{width:84px}
input.w64{width:64px}
input.w72{width:72px}
input:focus{outline:none;border-color:var(--primary)}
input[type=range]{accent-color:var(--primary);width:100%}
/* ── Buttons ── */
.btn{
  display:inline-flex;align-items:center;justify-content:center;
  gap:5px;padding:8px 16px;border:none;border-radius:8px;
  cursor:pointer;font-size:.85rem;font-weight:600;
  transition:opacity .15s,transform .1s;
}
.btn:active{transform:scale(.97)}
.btn:disabled{opacity:.4;cursor:default}
.btn-p{background:var(--primary);color:#fff}
.btn-p:hover:not(:disabled){background:var(--primary-d)}
.btn-w{background:var(--warn);color:#000}
.btn-d{background:var(--danger);color:#fff}
.btn-g{background:transparent;color:var(--sub);border:1px solid var(--border)}
.btn-g:hover:not(:disabled){color:var(--text);border-color:var(--sub)}
.btn-sm{padding:5px 12px;font-size:.78rem}
.btn-row{display:flex;gap:8px;flex-wrap:wrap;margin-top:12px}
.btn-row .btn{flex:1;min-width:80px}
hr.div{border:none;border-top:1px solid var(--border);margin:14px 0}
/* ── Field ── */
.fld{margin-bottom:10px}
.fld label{display:block;font-size:.73rem;font-weight:600;color:var(--sub);margin-bottom:4px}
.frow{display:grid;grid-template-columns:1fr 1fr;gap:10px}
/* ── Result ── */
.res{
  margin-top:8px;font-size:.78rem;padding:6px 10px;
  border-radius:6px;display:none;
}
.res.ok{background:rgba(34,197,94,.12);color:var(--success);border:1px solid rgba(34,197,94,.25)}
.res.er{background:rgba(239,68,68,.12);color:var(--danger);border:1px solid rgba(239,68,68,.25)}
/* ── Danger ── */
.danger{border-color:rgba(239,68,68,.25)}
.danger .ct{color:var(--danger)}
.dbtn{display:flex;gap:8px;flex-wrap:wrap}
.dbtn .btn{flex:1;min-width:110px}
/* ── Toasts ── */
#toasts{
  position:fixed;bottom:16px;left:50%;transform:translateX(-50%);
  display:flex;flex-direction:column;gap:7px;z-index:999;
  width:min(420px,calc(100vw - 24px));pointer-events:none;
}
.toast{
  padding:10px 16px;border-radius:8px;font-size:.84rem;font-weight:500;
  pointer-events:auto;box-shadow:0 4px 20px rgba(0,0,0,.4);
  animation:su .2s ease;
}
.toast.ok{background:#166534;color:#bbf7d0;border:1px solid #15803d}
.toast.er{background:#7f1d1d;color:#fecaca;border:1px solid #b91c1c}
.toast.info{background:#1e3a5f;color:#bfdbfe;border:1px solid #2563eb}
@keyframes su{from{opacity:0;transform:translateY(10px)}to{opacity:1;transform:translateY(0)}}
/* ── Modal ── */
#mbg{
  display:none;position:fixed;inset:0;background:rgba(0,0,0,.65);
  z-index:100;align-items:center;justify-content:center;
}
#mbg.open{display:flex}
#mbox{
  background:var(--surf);border:1px solid var(--border);
  border-radius:14px;padding:24px;max-width:320px;width:90%;
}
#mbox h3{font-size:1rem;margin-bottom:8px}
#mbox p{color:var(--sub);font-size:.85rem;margin-bottom:20px;line-height:1.6}
#mbox .btns{display:flex;gap:8px;justify-content:flex-end}
/* ── History table ── */
#htable{overflow-x:auto;margin-top:12px;display:none}
#htable table{width:100%;border-collapse:collapse;font-size:.77rem}
#htable th{
  text-align:left;padding:4px 8px;color:var(--sub);
  border-bottom:1px solid var(--border);font-weight:600;
}
#htable td{padding:5px 8px;border-bottom:1px solid var(--border)}
#htable tr:last-child td{border-bottom:none}
</style>
</head>
<body>

<header>
  <h1>Marstek B2500</h1>
  <div class="badge" id="hbadge"><div class="dot"></div><span id="hstat">Connecting…</span></div>
</header>

<!-- Live Status -->
<section>
  <div class="ct">Live Status</div>
  <div id="ph"><div class="spin"></div><span>Scanning for battery…</span></div>
  <div id="sc" style="display:none">
    <div class="conn-row">
      <div class="cc" id="cc-ble"><div class="d"></div>BLE</div>
      <div class="cc" id="cc-wifi"><div class="d"></div>WiFi</div>
      <div class="cc" id="cc-mqtt"><div class="d"></div>MQTT</div>
    </div>
    <div class="soc-row">
      <div><div class="soc-num" id="soc-n">–<span class="u">%</span></div></div>
      <div class="soc-col">
        <div class="soc-track"><div class="soc-fill" id="soc-f" style="width:0%"></div></div>
        <div class="soc-meta"><span id="cap">–</span><span id="dod-v">DoD –</span></div>
      </div>
    </div>
    <div class="stat-grid">
      <div class="stat-box">
        <div class="lbl">Solar In</div>
        <div class="ch-row"><span class="ch-n">1</span><span class="ch-v c-solar" id="in1">–</span><span class="ch-u">W</span></div>
        <div class="ch-row"><span class="ch-n">2</span><span class="ch-v c-solar" id="in2">–</span><span class="ch-u">W</span></div>
      </div>
      <div class="stat-box">
        <div class="lbl">Output</div>
        <div class="ch-row"><span class="ch-n">1</span><span class="ch-v c-out" id="o1">–</span><span class="ch-u">W</span></div>
        <div class="ch-row"><span class="ch-n">2</span><span class="ch-v c-out" id="o2">–</span><span class="ch-u">W</span></div>
      </div>
      <div class="stat-box">
        <div class="lbl">Temperature</div>
        <div class="val" id="temp" style="font-size:1rem">–</div>
        <div class="sv" id="daily">–</div>
      </div>
    </div>
    <div class="chips" id="chips"></div>
  </div>
</section>

<!-- Output Schedule -->
<section>
  <div class="ct">Output Schedule</div>
  <table class="st">
    <thead><tr>
      <th style="width:22px">#</th>
      <th style="width:50px">On</th>
      <th>Start</th>
      <th>End</th>
      <th>Watts</th>
    </tr></thead>
    <tbody id="sb"></tbody>
  </table>
  <div class="btn-row">
    <button class="btn btn-p" id="b-sched">Apply Schedule</button>
    <button class="btn btn-g btn-sm" id="b-sreset">Reset</button>
  </div>
  <div class="res" id="r-sched"></div>
</section>

<!-- Battery Settings -->
<section>
  <div class="ct">Battery Settings</div>
  <div class="fld">
    <label>Depth of Discharge (%)</label>
    <div style="display:flex;align-items:center;gap:10px;margin-top:2px">
      <input type="range" id="dod-r" min="20" max="100" value="80" style="flex:1">
      <input type="number" id="dod-i" min="20" max="100" value="80" class="w64">
      <span style="color:var(--sub);font-size:.8rem">%</span>
    </div>
  </div>
  <button class="btn btn-p btn-sm" id="b-dod">Apply DoD</button>
  <div class="res" id="r-dod"></div>

  <hr class="div">

  <div style="display:flex;align-items:center;gap:12px">
    <label class="tgl">
      <input type="checkbox" id="surplus" checked>
      <div class="tsl"></div>
    </label>
    <div style="flex:1">
      <div style="font-size:.9rem;font-weight:600">Surplus PV Feed-in</div>
      <div style="font-size:.73rem;color:var(--sub)">Feed excess solar back to the grid</div>
    </div>
    <button class="btn btn-g btn-sm" id="b-surplus" style="flex-shrink:0">Apply</button>
  </div>
  <div class="res" id="r-surplus"></div>
</section>

<!-- Inverter -->
<section>
  <div class="ct">Inverter Config</div>
  <div class="frow">
    <div class="fld">
      <label>Type ID (7 bytes hex)</label>
      <input type="text" id="inv-id" value="ef0368011e0000" maxlength="14" style="font-family:monospace">
    </div>
    <div class="fld">
      <label>Max Power (W)</label>
      <input type="number" id="inv-w" value="800" min="0" max="9999">
    </div>
  </div>
  <p style="font-size:.71rem;color:var(--sub);margin-bottom:10px">
    Hoymiles HM-800 → <code style="color:var(--primary)">ef0368011e0000</code> @ 800 W
  </p>
  <button class="btn btn-p btn-sm" id="b-inv">Apply Inverter</button>
  <div class="res" id="r-inv"></div>
</section>

<!-- Clock -->
<section>
  <div class="ct">Battery Clock</div>
  <p style="font-size:.82rem;color:var(--sub);margin-bottom:12px">
    Syncs the battery’s RTC from your browser’s current time and timezone.
  </p>
  <div style="display:flex;align-items:flex-end;gap:10px">
    <div class="fld" style="margin-bottom:0">
      <label>Timezone offset (minutes from UTC)</label>
      <input type="number" id="tz" value="0" class="w72" style="margin-top:4px">
    </div>
    <button class="btn btn-p" id="b-time">Sync Now</button>
  </div>
  <div class="res" id="r-time"></div>
</section>

<!-- History -->
<section>
  <div class="ct">Event History</div>
  <p style="font-size:.82rem;color:var(--sub);margin-bottom:12px">Fetch stored event records from the battery’s log.</p>
  <button class="btn btn-g" id="b-hist">Fetch History</button>
  <div id="htable">
    <table>
      <thead><tr><th>Time</th><th>Type</th><th style="text-align:right">Value</th></tr></thead>
      <tbody id="hbody"></tbody>
    </table>
  </div>
  <div class="res" id="r-hist"></div>
</section>

<!-- Network (collapsible) -->
<details>
  <summary><div class="ct">Network</div></summary>
  <div style="font-size:.82rem;font-weight:600;margin-bottom:8px">WiFi</div>
  <div class="frow">
    <div class="fld"><label>SSID</label><input type="text" id="w-ssid"></div>
    <div class="fld"><label>Password</label><input type="password" id="w-pw"></div>
  </div>
  <button class="btn btn-p btn-sm" id="b-wifi">Apply WiFi</button>
  <div class="res" id="r-wifi"></div>

  <hr class="div">

  <div style="font-size:.82rem;font-weight:600;margin-bottom:8px">MQTT</div>
  <div class="frow">
    <div class="fld"><label>Host</label><input type="text" id="m-host"></div>
    <div class="fld"><label>Port</label><input type="number" id="m-port" value="1883"></div>
  </div>
  <div class="frow">
    <div class="fld"><label>Username</label><input type="text" id="m-user"></div>
    <div class="fld"><label>Password</label><input type="password" id="m-pw"></div>
  </div>
  <div class="fld">
    <label style="display:flex;align-items:center;gap:6px;cursor:pointer">
      <input type="checkbox" id="m-ssl"> SSL / TLS
    </label>
  </div>
  <div style="display:flex;gap:8px">
    <button class="btn btn-p btn-sm" id="b-mqtt">Apply MQTT</button>
    <button class="btn btn-g btn-sm" id="b-mreset">Reset to Cloud</button>
  </div>
  <div class="res" id="r-mqtt"></div>
</details>

<!-- Device / Danger Zone -->
<section class="danger">
  <div class="ct">Device Control</div>
  <div class="dbtn">
    <button class="btn btn-g" id="b-restart">Restart</button>
    <button class="btn btn-w" id="b-hwreset">Hardware Reset</button>
    <button class="btn btn-d" id="b-factory">Factory Reset</button>
  </div>
  <div class="res" id="r-dev"></div>
</section>

<!-- Confirm modal -->
<div id="mbg">
  <div id="mbox">
    <h3 id="mt">Confirm</h3>
    <p id="mm">Are you sure?</p>
    <div class="btns">
      <button class="btn btn-g btn-sm" id="m-no">Cancel</button>
      <button class="btn btn-d btn-sm" id="m-yes">Confirm</button>
    </div>
  </div>
</div>

<div id="toasts"></div>

<script>
// ── helpers ──────────────────────────────────────────────────────────────────
function p2(n){return String(n).padStart(2,'0')}

async function api(method,url,body){
  const o={method};
  if(body!==undefined){o.headers={'Content-Type':'application/json'};o.body=JSON.stringify(body)}
  try{const r=await fetch(url,o);return await r.json()}
  catch(e){return{error:String(e)}}
}
const GET=url=>api('GET',url);
const POST=(url,b)=>api('POST',url,b||{});

function toast(msg,type,dur){
  dur=dur||3000;
  const t=document.createElement('div');
  t.className='toast '+type;t.textContent=msg;
  document.getElementById('toasts').appendChild(t);
  setTimeout(()=>t.remove(),dur);
}
function fmtErr(d){
  if(!d||!d.error)return 'Error';
  let msg=d.error;
  if(d.hint)msg+=' — '+d.hint;
  if(d.code)msg+=' ['+d.code+']';
  return msg;
}
function res(id,d){
  const el=document.getElementById(id);
  if(!el)return;
  el.style.display='block';
  if(d.ok){
    const label=d.op?('✓ '+d.op+' sent'):'✓ Done';
    el.className='res ok';el.textContent=label;
    toast(label,'ok');setTimeout(()=>el.style.display='none',3500);
  }else{
    const msg=fmtErr(d);
    el.className='res er';el.textContent='✗ '+msg;
    toast(msg,'er',6500);
    if(d && (d.code==='ble_disconnected' || d.code==='ble_dropped')){
      poll();
    }
  }
}

// ── modal ────────────────────────────────────────────────────────────────────
let _mr=null;
function confirm(title,msg){
  document.getElementById('mt').textContent=title;
  document.getElementById('mm').textContent=msg;
  document.getElementById('mbg').classList.add('open');
  return new Promise(r=>{_mr=r});
}
document.getElementById('m-yes').onclick=()=>{
  document.getElementById('mbg').classList.remove('open');
  if(_mr){_mr(true);_mr=null}
};
document.getElementById('m-no').onclick=()=>{
  document.getElementById('mbg').classList.remove('open');
  if(_mr){_mr(false);_mr=null}
};
document.getElementById('mbg').onclick=e=>{
  if(e.target===document.getElementById('mbg'))document.getElementById('m-no').click();
};

// ── schedule ─────────────────────────────────────────────────────────────────
const LS='b2500_sched_v1';
const DSCHED=[
  {enabled:false,sh:0,sm:0,eh:23,em:59,power_w:800},
  {enabled:false,sh:0,sm:0,eh:23,em:59,power_w:800},
  {enabled:false,sh:0,sm:0,eh:23,em:59,power_w:800},
  {enabled:false,sh:0,sm:0,eh:23,em:59,power_w:800},
  {enabled:false,sh:0,sm:0,eh:23,em:59,power_w:800},
];
function ldSched(){try{return JSON.parse(localStorage.getItem(LS))||DSCHED}catch{return DSCHED}}
function svSched(s){localStorage.setItem(LS,JSON.stringify(s))}

function buildSched(slots){
  const tb=document.getElementById('sb');tb.innerHTML='';
  slots.forEach((s,i)=>{
    const tr=document.createElement('tr');
    if(s.enabled)tr.classList.add('son');
    tr.innerHTML=
      '<td class="sn">'+(i+1)+'</td>'+
      '<td><button class="stog'+(s.enabled?' on':'')+'" data-i="'+i+'">'+(s.enabled?'ON':'OFF')+'</button></td>'+
      '<td><input type="time" class="ss" data-i="'+i+'" value="'+p2(s.sh)+':'+p2(s.sm)+'"></td>'+
      '<td><input type="time" class="se2" data-i="'+i+'" value="'+p2(s.eh)+':'+p2(s.em)+'"></td>'+
      '<td><input type="number" class="sw w72" data-i="'+i+'" value="'+s.power_w+'" min="0" max="9999"></td>';
    tb.appendChild(tr);
    tr.querySelector('.stog').onclick=function(){
      const on=this.classList.toggle('on');
      this.textContent=on?'ON':'OFF';
      tr.classList.toggle('son',on);
    };
  });
}
function rdSched(){
  return Array.from({length:5},(_,i)=>{
    const btn=document.querySelector('.stog[data-i="'+i+'"]');
    const st=document.querySelector('.ss[data-i="'+i+'"]');
    const ed=document.querySelector('.se2[data-i="'+i+'"]');
    const pw=document.querySelector('.sw[data-i="'+i+'"]');
    const [sh,sm]=(st?st.value:'00:00').split(':').map(Number);
    const [eh,em]=(ed?ed.value:'23:59').split(':').map(Number);
    return{enabled:btn?btn.classList.contains('on'):false,sh,sm,eh,em,power_w:pw?parseInt(pw.value)||0:0};
  });
}
buildSched(ldSched());
document.getElementById('b-sched').onclick=async()=>{
  const slots=rdSched();svSched(slots);
  res('r-sched',await POST('/api/schedules',{slots}));
};
document.getElementById('b-sreset').onclick=()=>{
  buildSched(DSCHED);document.getElementById('r-sched').style.display='none';
};

// ── DoD ──────────────────────────────────────────────────────────────────────
const DR=document.getElementById('dod-r'),DI=document.getElementById('dod-i');
DR.oninput=()=>DI.value=DR.value;
DI.oninput=()=>{const v=Math.min(100,Math.max(20,parseInt(DI.value)||20));DR.value=v};
document.getElementById('b-dod').onclick=async()=>{
  res('r-dod',await POST('/api/dod',{depth:parseInt(DI.value)}));
};

// ── Surplus ──────────────────────────────────────────────────────────────────
document.getElementById('b-surplus').onclick=async()=>{
  res('r-surplus',await POST('/api/surplus-feed',{enabled:document.getElementById('surplus').checked}));
};

// ── Inverter ─────────────────────────────────────────────────────────────────
document.getElementById('b-inv').onclick=async()=>{
  const id=document.getElementById('inv-id').value.replace(/\\s/g,'').toLowerCase();
  const w=parseInt(document.getElementById('inv-w').value)||800;
  res('r-inv',await POST('/api/inverter',{inverter_id:id,max_w:w}));
};

// ── Time sync ─────────────────────────────────────────────────────────────────
document.getElementById('tz').value=-new Date().getTimezoneOffset();
document.getElementById('b-time').onclick=async()=>{
  const n=new Date();
  const d=await POST('/api/time',{
    year:n.getUTCFullYear(),month:n.getUTCMonth()+1,day:n.getUTCDate(),
    hour:n.getUTCHours(),minute:n.getUTCMinutes(),second:n.getUTCSeconds(),
    tz_minutes:parseInt(document.getElementById('tz').value)||0,
  });
  res('r-time',d);
};

// ── History ───────────────────────────────────────────────────────────────────
document.getElementById('b-hist').onclick=async()=>{
  const btn=document.getElementById('b-hist');
  btn.disabled=true;btn.textContent='Fetching…';
  const d=await GET('/api/history');
  btn.disabled=false;btn.textContent='Fetch History';
  if(d.error){res('r-hist',d);return}
  const recs=d.records||[];
  if(!recs.length){toast('No records returned','info');return}
  const tb=document.getElementById('hbody');tb.innerHTML='';
  recs.forEach(r=>{
    const tr=document.createElement('tr');
    tr.innerHTML=
      '<td>'+new Date(r.ts*1000).toLocaleString()+'</td>'+
      '<td style="color:var(--sub);font-family:monospace">'+
        '0x'+r.type.toString(16).padStart(2,'0')+':'+r.sub.toString(16).padStart(2,'0')+
      '</td>'+
      '<td style="text-align:right">'+r.value+'</td>';
    tb.appendChild(tr);
  });
  document.getElementById('htable').style.display='block';
  document.getElementById('r-hist').style.display='none';
};

// ── Network ───────────────────────────────────────────────────────────────────
document.getElementById('b-wifi').onclick=async()=>{
  const btn=document.getElementById('b-wifi');
  const s=document.getElementById('w-ssid').value;
  if(!s){toast('SSID required','er');return}
  btn.disabled=true;const t=btn.textContent;btn.textContent='Sending…';
  const d=await POST('/api/wifi',{ssid:s,password:document.getElementById('w-pw').value});
  btn.disabled=false;btn.textContent=t;
  res('r-wifi',d);
};
document.getElementById('b-mqtt').onclick=async()=>{
  const btn=document.getElementById('b-mqtt');
  const h=document.getElementById('m-host').value;
  if(!h){toast('Host required','er');return}
  btn.disabled=true;const t=btn.textContent;btn.textContent='Applying…';
  const d=await POST('/api/mqtt',{
    host:h,port:parseInt(document.getElementById('m-port').value)||1883,
    ssl:document.getElementById('m-ssl').checked,
    user:document.getElementById('m-user').value,
    password:document.getElementById('m-pw').value,
  });
  btn.disabled=false;btn.textContent=t;
  res('r-mqtt',d);
};
document.getElementById('b-mreset').onclick=async()=>{
  const btn=document.getElementById('b-mreset');
  btn.disabled=true;const t=btn.textContent;btn.textContent='Resetting…';
  const d=await POST('/api/mqtt/reset');
  btn.disabled=false;btn.textContent=t;
  res('r-mqtt',d);
};

// ── Device controls ───────────────────────────────────────────────────────────
document.getElementById('b-restart').onclick=async()=>{
  if(!await confirm('Restart Battery','The battery will reboot. BLE will reconnect automatically.'))return;
  res('r-dev',await POST('/api/restart'));
};
document.getElementById('b-hwreset').onclick=async()=>{
  if(!await confirm('Hardware Reset','Triggers a deep hardware reset via the FF06 channel. The battery will reboot.'))return;
  res('r-dev',await POST('/api/hardware-reset'));
};
document.getElementById('b-factory').onclick=async()=>{
  if(!await confirm('Factory Reset ⚠','WARNING: This wipes all settings — schedules, WiFi, MQTT. The device will reboot. This cannot be undone.'))return;
  res('r-dev',await POST('/api/factory-reset'));
};

// ── Status polling ────────────────────────────────────────────────────────────
function socClr(p){return p>50?'var(--success)':p>20?'var(--warn)':'var(--danger)'}
function cc(id,on){document.getElementById(id).classList.toggle('on',!!on)}

async function poll(){
  const d=await GET('/api/status');
  const ph=document.getElementById('ph'),sc=document.getElementById('sc');
  const badge=document.getElementById('hbadge'),hs=document.getElementById('hstat');
  if(!d.connected){
    ph.style.display='flex';sc.style.display='none';
    badge.classList.remove('ok');hs.textContent='Disconnected';
    return;
  }
  ph.style.display='none';sc.style.display='block';
  badge.classList.add('ok');hs.textContent='Connected';

  cc('cc-ble',true);cc('cc-wifi',d.wifi_connected);cc('cc-mqtt',d.mqtt_connected);

  const soc=typeof d.soc_percent==='number'?d.soc_percent:0;
  document.getElementById('soc-n').innerHTML=soc.toFixed(1)+'<span class="u">%</span>';
  const f=document.getElementById('soc-f');
  f.style.width=soc+'%';f.style.background=socClr(soc);

  document.getElementById('cap').textContent=d.remaining_capacity_wh!=null?d.remaining_capacity_wh+' Wh rem':'';
  document.getElementById('dod-v').textContent='DoD '+(d.dod!=null?d.dod:'–')+'%';

  document.getElementById('in1').textContent=d.in1_power_w??'–';
  document.getElementById('in2').textContent=d.in2_power_w??'–';
  document.getElementById('o1').textContent=d.out1_power_w??'–';
  document.getElementById('o2').textContent=d.out2_power_w??'–';

  const tlo=d.temperature_low_c,thi=d.temperature_high_c;
  document.getElementById('temp').innerHTML=
    tlo!=null?tlo+'°<span class="u">–</span>'+thi+'°<span class="u">C</span>':'–';

  const ch=d.daily_charge_wh,dis=d.daily_discharge_wh;
  document.getElementById('daily').textContent=
    ch!=null?'↑'+(ch/1000).toFixed(2)+'kWh ↓'+((dis||0)/1000).toFixed(2)+'kWh':'';

  const chips=[];
  if(d.out1_enable!=null)chips.push('Out1 <b>'+(d.out1_enable?'ON':'off')+'</b>');
  if(d.out2_enable!=null)chips.push('Out2 <b>'+(d.out2_enable?'ON':'off')+'</b>');
  if(d.time_hour!=null)chips.push('Clock <b>'+p2(d.time_hour)+':'+p2(d.time_minute)+'</b>');
  document.getElementById('chips').innerHTML=chips.map(c=>'<div class="chip">'+c+'</div>').join('');

  if(d.dod!=null){DR.value=d.dod;DI.value=d.dod}
}
setInterval(poll,5000);
poll();
</script>
</body>
</html>
"""


@app.get("/")
async def index(req):
    return _HTML, 200, {"Content-Type": "text/html"}
