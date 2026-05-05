"""HTTP server for the Marstek BLE bridge — microdot 2.x."""
import ujson
import marstek_ble as ble
from microdot import Microdot, Response

Response.default_content_type = "application/json"

app = Microdot()


# ---------------------------------------------------------------------------
# RUNTIME_INFO parser (ported from ble_probe.py)
# ---------------------------------------------------------------------------

def _parse_runtime_info(data: bytes) -> dict | None:
    if len(data) < 39 or data[0] != 0x73 or data[2] != 0x23 or data[3] != 0x03:
        return None

    def u16(o): return data[o] | (data[o+1] << 8)
    def s16(o):
        v = u16(o)
        return v - 65536 if v >= 32768 else v
    def u32(o): return data[o] | (data[o+1]<<8) | (data[o+2]<<16) | (data[o+3]<<24)

    r = {
        "in1_active":             bool(data[4] & 0x01),
        "in2_active":             bool(data[5] & 0x01),
        "in1_power_w":            u16(6),
        "in2_power_w":            u16(8),
        "soc_percent":            u16(10) / 10,
        "wifi_connected":         bool(data[15] & 0x01),
        "mqtt_connected":         bool(data[15] & 0x02),
        "out1_enable":            bool(data[14] & 0x01),
        "out2_enable":            bool(data[14] & 0x02),
        "out1_active":            data[16],
        "out2_active":            data[17],
        "dod":                    data[18],
        "remaining_capacity_wh":  u16(22),
        "out1_power_w":           u16(24),
        "out2_power_w":           u16(26),
        "time_hour":              data[31],
        "time_minute":            data[32],
        "temperature_low_c":      s16(33),
        "temperature_high_c":     s16(35),
    }
    if len(data) >= 45: r["daily_charge_wh"]    = u32(40)
    if len(data) >= 49: r["daily_discharge_wh"] = u32(44)
    if len(data) >= 53: r["daily_load_wh"]      = u32(48)
    return r

# ---------------------------------------------------------------------------
# DEVICE_INFO parser (opcode 0x04)
# ---------------------------------------------------------------------------

def _parse_device_info(data: bytes) -> dict | None:
    # Frame: [0x73][len][0x23][0x04][payload...][checksum]
    if len(data) < 5 or data[0] != 0x73 or data[2] != 0x23 or data[3] != 0x04:
        return None
    payload = data[4:-1].decode("ascii", "ignore")
    result = {}
    for part in payload.split(","):
        if "=" in part:
            k, _, v = part.partition("=")
            result[k.strip()] = v.strip()
    return result if result else None


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _err(msg: str, status: int = 503):
    return {"error": msg}, status


async def _cmd(opcode: int, payload: bytes = b""):
    if not ble.connected:
        return _err("not connected")
    resp = await ble.send_command(opcode, payload)
    if resp is None:
        return _err("no response from battery", 502)
    return {"ok": True, "raw": resp.hex()}


# ---------------------------------------------------------------------------
# API endpoints
# ---------------------------------------------------------------------------

@app.get("/api/status")
async def api_status(req):
    status = {"connected": ble.connected}
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
    resp = await ble.send_command(0x04)
    if resp is None:
        return _err("no response from battery", 502)
    parsed = _parse_device_info(resp)
    if parsed:
        return parsed
    return {"raw": resp.hex()}


@app.post("/api/wifi")
async def api_set_wifi(req):
    body = req.json or {}
    ssid = body.get("ssid", "")
    password = body.get("password", "")
    if not ssid:
        return _err("ssid required", 400)
    payload = (ssid + "<.,.>" + password).encode()
    return await _cmd(0x05, payload)


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
    return await _cmd(0x20, payload)


@app.post("/api/mqtt/reset")
async def api_mqtt_reset(req):
    return await _cmd(0x21)


@app.post("/api/restart")
async def api_restart(req):
    if not ble.connected:
        return _err("not connected")
    # Send 0x25 [0x01] — device acks then reboots; connection will drop
    resp = await ble.send_command(0x25, b"\x01")
    # resp may be None if connection dropped before we read the ack — that's fine
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
<title>Marstek Bridge</title>
<style>
  body{font-family:sans-serif;max-width:480px;margin:2rem auto;padding:0 1rem}
  h1{font-size:1.3rem;margin-bottom:1.5rem}
  h2{font-size:1rem;border-bottom:1px solid #ccc;padding-bottom:4px;margin-top:1.5rem}
  label{display:block;margin:.4rem 0 .1rem;font-size:.85rem;color:#555}
  input[type=text],input[type=password],input[type=number]{width:100%;box-sizing:border-box;padding:6px;margin-bottom:.5rem}
  button{padding:8px 16px;cursor:pointer}
  .danger{background:#d9534f;color:#fff;border:none}
  #status{background:#f5f5f5;padding:.8rem;border-radius:4px;font-size:.85rem;white-space:pre}
  .ok{color:green} .err{color:red}
</style>
</head>
<body>
<h1>Marstek B2500 Bridge</h1>

<h2>Status</h2>
<div id="status">loading…</div>

<h2>Set WiFi</h2>
<form id="fwifi">
  <label>SSID</label><input name="ssid" type="text" required>
  <label>Password</label><input name="password" type="password">
  <button type="submit">Apply</button>
</form>
<div id="rwifi"></div>

<h2>Set MQTT</h2>
<form id="fmqtt">
  <label>Host</label><input name="host" type="text" required>
  <label>Port</label><input name="port" type="number" value="1883">
  <label><input name="ssl" type="checkbox"> SSL</label>
  <label>Username</label><input name="user" type="text">
  <label>Password</label><input name="password" type="password">
  <button type="submit">Apply</button>
</form>
<div id="rmqtt"></div>

<h2>Reset MQTT</h2>
<p style="font-size:.85rem">Return to Marstek cloud defaults.</p>
<button id="breset">Reset MQTT</button>
<div id="rreset"></div>

<h2>Restart Battery</h2>
<p style="font-size:.85rem">Triggers a full MCU reboot via BLE opcode 0x25.</p>
<button class="danger" id="brestart">Restart Battery</button>
<div id="rrestart"></div>

<script>
async function post(url, body){
  const r=await fetch(url,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
  return r.json();
}
async function postNoBody(url){
  const r=await fetch(url,{method:'POST'});
  return r.json();
}
function show(id, data){
  document.getElementById(id).innerHTML =
    data.ok ? '<span class="ok">OK</span>' :
              '<span class="err">'+(data.error||JSON.stringify(data))+'</span>';
}

async function refreshStatus(){
  try{
    const d=await fetch('/api/status').then(r=>r.json());
    document.getElementById('status').textContent=JSON.stringify(d,null,2);
  }catch(e){document.getElementById('status').textContent='error: '+e;}
}
setInterval(refreshStatus,5000);
refreshStatus();

document.getElementById('fwifi').onsubmit=async e=>{
  e.preventDefault();
  const f=new FormData(e.target);
  show('rwifi', await post('/api/wifi',{ssid:f.get('ssid'),password:f.get('password')}));
};

document.getElementById('fmqtt').onsubmit=async e=>{
  e.preventDefault();
  const f=new FormData(e.target);
  show('rmqtt', await post('/api/mqtt',{
    host:f.get('host'),port:parseInt(f.get('port')),
    ssl:f.get('ssl')==='on',user:f.get('user'),password:f.get('password')
  }));
};

document.getElementById('breset').onclick=async()=>{
  show('rreset', await postNoBody('/api/mqtt/reset'));
};

document.getElementById('brestart').onclick=async()=>{
  if(!confirm('Restart the battery now?')) return;
  show('rrestart', await postNoBody('/api/restart'));
};
</script>
</body>
</html>
"""


@app.get("/")
async def index(req):
    return _HTML, 200, {"Content-Type": "text/html"}
