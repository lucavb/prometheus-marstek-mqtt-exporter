# MQTT Protocol

## Connection

- Broker: `eu.hamedata.com:1883` (plain TCP, no TLS)
- Client-side uses QoS 0, keepalive ~60s
- On firmware **226.5** (HMA/HMK) and **108.7** (HMJ) every device connects
  with the same MQTT client ID `mst_`, which breaks multi-device setups.
  Newer firmware lets you configure distinct MQTT usernames to work around it.
  (See `noone2k/hm2500pub` discussion.)

## Topic scheme

All topics follow:

```
hame_energy/<hardware_id>/<role>/<mac>/<verb>
```

For this device:

| Direction | Topic | Notes |
| --- | --- | --- |
| Device → cloud | `hame_energy/HMJ-2/device/60323bd14b6e/ctrl` | Status/telemetry payload published by the battery |
| Cloud/App → device | `hame_energy/HMJ-2/App/60323bd14b6e/ctrl` | Control commands |

Replacing `App` with your own MQTT client is enough to issue control
commands in the same format.

## Telemetry payload format

The telemetry is a flat, comma-separated `key=value` list (not JSON). Example
from a captured frame:

```
p1=1,p2=1,w1=24,w2=23,pe=45,vv=110,sv=9,cs=0,cd=0,am=0,o1=1,o2=1,
do=80,lv=220,cj=2,kn=1008,g1=110,g2=109,b1=0,b2=0,md=0,
d1=1,e1=0:0,f1=23:59,h1=221,d2=0,e2=0:0,f2=23:59,h2=80,
d3=0,e3=0:0,f3=23:59,h3=80,sg=0,sp=80,st=0,tl=19,th=20,
tc=0,tf=0,fc=202310231502,id=5,a0=45,a1=0,a2=0,
l0=1,l1=0,c0=255,c1=4,bc=1399,bs=589,pt=3091,it=2108,
m0=0,m1=0,m2=0,m3=219,d4=0,e4=0:0,f4=23:59,h4=80,
d5=0,e5=0:0,f5=23:59,h5=80,lmo=1997,lmi=1447,lmf=0,
uv=107,sm=0,bn=0,ct_t=7,tc_dis=1
```

### Observed field mapping (working set)

These are the fields we've either confirmed from capture or are consistent
with the community `hm2mqtt` project's mapping. Fields not yet understood are
kept for future investigation.

| Key | Meaning | Unit |
| --- | --- | --- |
| `p1`, `p2` | Panel 1 / Panel 2 input active | bool |
| `w1`, `w2` | Panel 1 / Panel 2 input power | W |
| `pe` | State of charge (%) | % |
| `vv` | Software version major (`sv` in HTTP) | — |
| `sv` | Software sub-version | — |
| `cs` | Charging status flag | — |
| `cd` | Discharging status flag | — |
| `am` | Auto-mode flag | — |
| `o1`, `o2` | Output 1 / Output 2 enabled | bool |
| `do` | Discharge threshold | % |
| `lv` | Load voltage / total load W | W |
| `cj` | Grid status | — |
| `kn` | Daily generation | Wh |
| `g1`, `g2` | Output 1 / Output 2 power | W |
| `b1`, `b2` | Output 1 / Output 2 bypass | bool |
| `md` | Mode | enum |
| `d1..d5` | Schedule slot enable | bool |
| `e1..e5` | Schedule slot start time (`HH:MM`) | time |
| `f1..f5` | Schedule slot end time (`HH:MM`) | time |
| `h1..h5` | Schedule slot target | W/% |
| `sg` | Smart-grid flag | bool |
| `sp` | Schedule power cap | W |
| `st` | Schedule state | — |
| `tl`, `th` | Temperature low / high | °C |
| `tc`, `tf` | Temperature flags | — |
| `fc` | Firmware compile timestamp (`YYYYMMDDHHMM`) | — |
| `id` | Some device-role id | — |
| `a0`, `a1`, `a2` | Aggregate / battery % channels | — |
| `l0`, `l1` | Load flags | — |
| `c0`, `c1` | CT / current-sensor channels | — |
| `bc` | Battery cycle count (or charging counter) | — |
| `bs` | Battery state counter | — |
| `pt` | Total PV input (Wh) | Wh |
| `it` | Input total | Wh |
| `m0..m3` | Per-pack metrics | — |
| `lmo`, `lmi`, `lmf` | Lifetime metering counters (output / input / feed) | Wh |
| `uv` | Under-voltage threshold | — |
| `sm` | Smartmeter flag | bool |
| `bn` | Battery count | — |
| `ct_t` | CT type (7 = Shelly 3EM in our capture) | enum |
| `tc_dis` | Temperature-compensation disabled | bool |

> Keys/semantics above follow the `hm2mqtt` community schema where applicable
> and should be verified against the running firmware before being trusted
> in downstream metrics.

## Control commands

The companion topic for this device is:

```
hame_energy/HMJ-2/App/60323bd14b6e/ctrl
```

A single frame with an empty body was observed during the capture — the real
app sends the same `key=value` format back with the subset of keys it wants
to change (e.g. `o1=1,o2=0,md=1`). OTA-trigger commands also flow through
this topic; the payload points at a URL from `www.hamedata.com/app/download/neng/`
(see [`firmware.md`](firmware.md)).
