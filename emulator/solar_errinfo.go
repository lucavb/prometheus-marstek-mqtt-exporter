package emulator

// puterrinfo.php — Marstek B2500 error/event log upload
//
// Derived from Ghidra static analysis of B2500_All_HMJ.bin (HMJ firmware 110,
// ARM Cortex-M, loaded at 0x08000000). See docs/firmware.md for full notes.
//
// # Wire format
//
// Three report types, selected by flags at offsets +0x588/+0x589/+0x58a in the
// device-state struct (g_device_state, 0x08012178):
//
//	Type 0 — battery slot 0, triple events:
//	  <uid>:<0>:<sw_ver>:<field2>:<field3>:<field4>:<field5>:<code>.<ts>.<val>,...
//
//	Type 1 — battery slot 1, quintuple events (ring at g_device_state+0x200):
//	  <uid>:<1>:<sw_ver>:<field2>:<field3>:<field4>:<field5>:<a>.<b>.<c>.<d>.<val>,...
//
//	Type 2 — battery slot 2, quintuple events (ring at g_device_state+0x3c8):
//	  <uid>:<2>:<sw_ver>:<field2>:<field3>:<field4>:<field5>:<a>.<b>.<c>.<d>.<val>,...
//
// The format string in firmware is "%s:%d:%d:%d:%d:%d:%d:" (1 string + 6 ints).
// Header integer layout (zero-based after UID):
//
//	[0] report_type    — 0, 1, or 2
//	[1] sw_version     — firmware version number (observed: 110)
//	[2] field2         — SoC % or battery voltage field (TBC against a live Type 1/2 capture)
//	[3] field3         — status flags byte at battery_state+0x4d / +0xde / +0x16f
//	[4] field4         — status flags byte at battery_state+0x4e / +0xdf / +0x170
//	[5] field5         — status flags byte at battery_state+0x4f / +0xe0 / +0x171
//
// Triple event format (type 0):   <code>.<unix_ts_seconds>.<value>
// Quintuple event format (types 1/2): <a>.<b>.<c>.<d>.<value>
//   where a=code, b/c/d = sub-fields (likely cell index, phase, severity)
//   and value is the raw u32 measurement.
//
// The device ring holds 42 events (enqueue_event, 0x0802152c). Dedup: if
// (last_code, last_value) matches the incoming call, the event is silently
// dropped. Overflow: FIFO — oldest entry evicted, newest appended.
//
// # Event code dictionary
//
// 49 distinct codes confirmed from 95 enqueue_event call sites. Full table:
//
//	 0  startup_init                   periodic battery poll initialisation
//	12  soc_threshold_crossed          SoC counter threshold or 100% HTTP parse
//	15  cell_overvoltage_charge        cell voltage exceeded two-level charge limit
//	18  cell_voltage_high              cell voltage > high threshold (debounced)
//	24  discharge_status_flag          discharge status bit changed
//	25  bms_probe_no_response          BMS probe returned non-zero
//	33  shelly_ct_meter_status         Shelly CT meter state byte changed
//	35  undervoltage_discharge         battery < undervoltage threshold (debounced)
//	36  soc_zero_or_overvoltage_low    SoC==0 or voltage < low limit
//	38  charge_voltage_limit           charge voltage crossing boundary
//	39  overcurrent_charge             charge current > threshold
//	42  discharge_protection_flag      discharge protection bit set
//	43  battery_capacity_low           capacity low on poll timer
//	50  thermal_discharge_high         max temp > discharge high limit
//	51  thermal_discharge_low          min temp > discharge high limit
//	52  thermal_charge_high            max temp > charge high limit
//	53  thermal_charge_low             min temp > charge high limit
//	62  ble_energy_accumulated         BLE GATT energy accumulation complete
//	64  battery_poll_status            battery poll FSM state changed
//	65  reboot_pending                 MCU reset imminent
//	66  ble_soc_state_changed          BLE SoC state transition
//	73  bms_comm_watchdog              BMS watchdog triggered
//	74  mqtt_ext_conn_failed           aux MQTT client AT+QMTCONN rejected (4 retries)
//	75  fault_flags_bitmap             BMS fault flags word changed (or wifi scan giveup)
//	77  soc_below_threshold            SoC < charge threshold
//	78  mqtt_ext_session_up            aux MQTT client subscribed (session established)
//	80  setreport_response_parsed      setreport JSON alt-key branch parsed
//	81  battery_pack_init_no_response  BMS init probe returned 0 or 0xFF
//	82  battery_pack_init_cell_fault   init-time cell overvoltage/imbalance flag
//	84  heartbeat                      60s timer or HTTP OK; value 0 or 0x4FFFF
//	85  mqtt_ext_subscribe_failed      aux MQTT client AT+QMTSUB rejected (4 retries)
//	86  tls_cert_inventory_missing     AT+QFLST did not list all 3 TLS files; re-provision
//	87  tls_cert_slots_cleared         AT+QSSLCERT="CA/User_Cert/User_Key",0 all OK
//	88  tls_user_key_written           TLS User Key payload flashed to modem cert store
//	89  charge_current_high_ch0        ch0 > 16A for 60s
//	90  charge_current_high_ch1        ch1 > 16A for 60s
//	91  http_post_failed               puterrinfo POST returned non-200
//	92  cert_slot_selected             TLS cert slot chosen
//	93  cert_activated                 TLS cert activated
//	95  cell_fault_flags_packed        packed pack cell-fault flags, every 50% SoC
//	96  mqtt_auth_max_retries          MQTT connect auth exhausted
//	98  subsystem_state_event          subsystem state transition
//	99  cert_change_triggered          TLS cert rotation
//	100 charge_setpoint_exceeded       charge setpoint voltage exceeded
//	101 passthrough_mode_changed       pass-through mode enabled/disabled
//	103 ac_coupling_event              AC coupling fault/timeout
//	104 battery_pack_reset_complete    battery pack reset sequence completed
//	105 ble_adv_retry_exhausted        BLE adv restart counter > 3
//	106 wifi_disconnect                supplicant reason 1/2 reported 3x
//
// See emulator/solar_errinfo_codes.go for the full dictionary with descriptions.

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// solarErrInfoEvent represents one triple event from a Type 0 upload:
// <code>.<unix_ts_seconds>.<value>
type solarErrInfoEvent struct {
	Code  int64
	TS    int64
	Value int64
	// Name is the human-readable label from errInfoCodeName, or "unknown_N".
	Name string
}

// solarErrInfoQuintuple represents one 5-field entry from a Type 1/2 upload:
// <a>.<b>.<c>.<d>.<value>
// Field a is the event code; b, c, d are sub-fields (likely cell index, phase,
// severity — exact semantics TBC against a live Type 1/2 capture).
type solarErrInfoQuintuple struct {
	Code  int64 // field a — event code
	SubB  int64 // field b
	SubC  int64 // field c
	SubD  int64 // field d
	Value int64 // field e — raw u32 measurement
	// Name is the human-readable label from errInfoCodeName, or "unknown_N".
	Name string
}

// solarErrInfoHeader holds the six named header integer fields that follow the
// UID and report_type on the wire. Populated from Header[1..5] when Header has
// at least 6 elements; zero otherwise.
//
// Field naming is derived from Ghidra analysis of the per-battery state struct
// passed to snprintf_append in puterrinfo_state_machine, case 5.
type solarErrInfoHeader struct {
	SoftwareVersion int64 // Header[1] — firmware version number (e.g. 110)
	Field2          int64 // Header[2] — SoC % or voltage field (TBC)
	Field3          int64 // Header[3] — status flags at battery_state+0x4d/0xde/0x16f
	Field4          int64 // Header[4] — status flags at battery_state+0x4e/0xdf/0x170
	Field5          int64 // Header[5] — status flags at battery_state+0x4f/0xe0/0x171
}

// solarErrInfoParsed is the result of a best-effort parse of a
// /app/Solar/puterrinfo.php request body.
//
// Header retains the raw positional integer slice for backward compatibility
// with metrics and tests; ParsedHeader holds the same values with named fields
// for human-readable logging.
//
// ParseErrors is never nil. Any field or entry that could not be parsed is
// appended here and the rest of the parse continues.
type solarErrInfoParsed struct {
	UID          string
	ReportType   int    // 0, 1, 2 (battery slot), or -1 if unknown/unset
	Header       []int64
	ParsedHeader solarErrInfoHeader
	// Events is populated for Type 0 uploads (triples).
	Events []solarErrInfoEvent
	// Quintuples is populated for Type 1/2 uploads.
	Quintuples     []solarErrInfoQuintuple
	DistinctCodes  []int64 // sorted, deduplicated
	DistinctValues []int64 // sorted, deduplicated (type 0 only)
	OldestTS       int64   // unix seconds; 0 if no type-0 events
	NewestTS       int64   // unix seconds; 0 if no type-0 events
	ParseErrors    []string
}

// parseSolarErrInfoBody performs a safe best-effort parse of the raw body.
// It never panics regardless of input.
func parseSolarErrInfoBody(raw []byte) (p solarErrInfoParsed) {
	p.ParseErrors = []string{}
	p.ReportType = -1

	s := strings.TrimSpace(string(raw))
	if s == "" {
		p.ParseErrors = append(p.ParseErrors, "empty body")
		return
	}

	// Split on colons. First token is the UID. Subsequent integer tokens build
	// the header. The first non-integer token starts the events blob (which may
	// itself contain colons if it is malformed, so we join the remainder).
	tokens := strings.Split(s, ":")
	if len(tokens) == 0 {
		p.ParseErrors = append(p.ParseErrors, "no colon-separated tokens")
		return
	}

	p.UID = tokens[0]

	var eventsBlob string
	for i := 1; i < len(tokens); i++ {
		n, err := strconv.ParseInt(strings.TrimSpace(tokens[i]), 10, 64)
		if err != nil {
			eventsBlob = strings.Join(tokens[i:], ":")
			break
		}
		p.Header = append(p.Header, n)
	}

	// Derive report type and named header fields from the integer header.
	if len(p.Header) >= 1 {
		p.ReportType = int(p.Header[0])
	}
	if len(p.Header) >= 6 {
		p.ParsedHeader = solarErrInfoHeader{
			SoftwareVersion: p.Header[1],
			Field2:          p.Header[2],
			Field3:          p.Header[3],
			Field4:          p.Header[4],
			Field5:          p.Header[5],
		}
	} else if len(p.Header) >= 2 {
		p.ParsedHeader.SoftwareVersion = p.Header[1]
	}

	if eventsBlob == "" {
		return
	}

	codeSet := make(map[int64]struct{})

	switch p.ReportType {
	case 1, 2:
		p.Quintuples = parseQuintuples(eventsBlob, &p.ParseErrors, codeSet)
	default:
		// Type 0 or unknown — attempt triple parse.
		valueSet := make(map[int64]struct{})
		p.Events = parseTriples(eventsBlob, &p.ParseErrors, codeSet, valueSet, &p.OldestTS, &p.NewestTS)
		for v := range valueSet {
			p.DistinctValues = append(p.DistinctValues, v)
		}
		sort.Slice(p.DistinctValues, func(i, j int) bool { return p.DistinctValues[i] < p.DistinctValues[j] })
	}

	for c := range codeSet {
		p.DistinctCodes = append(p.DistinctCodes, c)
	}
	sort.Slice(p.DistinctCodes, func(i, j int) bool { return p.DistinctCodes[i] < p.DistinctCodes[j] })

	return
}

// parseTriples parses a comma-separated list of "code.ts.value" entries.
func parseTriples(blob string, errs *[]string, codeSet map[int64]struct{}, valueSet map[int64]struct{}, oldestTS, newestTS *int64) []solarErrInfoEvent {
	var events []solarErrInfoEvent
	for _, entry := range strings.Split(blob, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ".", 3)
		if len(parts) != 3 {
			*errs = append(*errs, "bad triple (expected 3 dot-separated parts): "+entry)
			continue
		}
		code, err1 := strconv.ParseInt(parts[0], 10, 64)
		ts, err2 := strconv.ParseInt(parts[1], 10, 64)
		val, err3 := strconv.ParseInt(parts[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			*errs = append(*errs, "bad triple (non-integer field): "+entry)
			continue
		}
		events = append(events, solarErrInfoEvent{
			Code:  code,
			TS:    ts,
			Value: val,
			Name:  errInfoCodeLabel(code),
		})
		codeSet[code] = struct{}{}
		valueSet[val] = struct{}{}
		if *oldestTS == 0 || ts < *oldestTS {
			*oldestTS = ts
		}
		if ts > *newestTS {
			*newestTS = ts
		}
	}
	return events
}

// parseQuintuples parses a comma-separated list of "a.b.c.d.value" entries
// used in Type 1 and Type 2 reports.
func parseQuintuples(blob string, errs *[]string, codeSet map[int64]struct{}) []solarErrInfoQuintuple {
	var qs []solarErrInfoQuintuple
	for _, entry := range strings.Split(blob, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ".", 5)
		if len(parts) != 5 {
			*errs = append(*errs, "bad quintuple (expected 5 dot-separated parts): "+entry)
			continue
		}
		a, err1 := strconv.ParseInt(parts[0], 10, 64)
		b, err2 := strconv.ParseInt(parts[1], 10, 64)
		c, err3 := strconv.ParseInt(parts[2], 10, 64)
		d, err4 := strconv.ParseInt(parts[3], 10, 64)
		val, err5 := strconv.ParseInt(parts[4], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
			*errs = append(*errs, "bad quintuple (non-integer field): "+entry)
			continue
		}
		qs = append(qs, solarErrInfoQuintuple{
			Code:  a,
			SubB:  b,
			SubC:  c,
			SubD:  d,
			Value: val,
			Name:  errInfoCodeLabel(a),
		})
		codeSet[a] = struct{}{}
	}
	return qs
}

// updateSolarErrInfoHeader publishes each element of the puterrinfo header
// slice as a labelled gauge series. If the new header is shorter than the
// previous one, the now-absent higher-indexed series are deleted so they don't
// linger with stale values after a firmware downgrade.
func (e *Emulator) updateSolarErrInfoHeader(header []int64) {
	e.mu.Lock()
	prevLen := e.lastErrInfoHeaderLen
	e.lastErrInfoHeaderLen = len(header)
	e.mu.Unlock()

	for i := len(header); i < prevLen; i++ {
		e.solarErrInfoHeaderValue.DeleteLabelValues(fmt.Sprintf("%d", i))
	}

	for i, v := range header {
		e.solarErrInfoHeaderValue.WithLabelValues(strconv.Itoa(i)).Set(float64(v))
	}
}

// updateErrInfoNamedMetrics publishes the named header gauges and per-event
// counters for a successfully parsed puterrinfo upload.
func (e *Emulator) updateErrInfoNamedMetrics(p *solarErrInfoParsed) {
	uid := p.UID
	battery := strconv.Itoa(p.ReportType)

	if len(p.Header) >= 1 {
		e.solarErrInfoReportType.WithLabelValues(uid, battery).Set(float64(p.ReportType))
	}
	if len(p.Header) >= 2 {
		e.solarErrInfoSwVersion.WithLabelValues(uid, battery).Set(float64(p.ParsedHeader.SoftwareVersion))
	}
	if len(p.Header) >= 6 {
		e.solarErrInfoField2.WithLabelValues(uid, battery).Set(float64(p.ParsedHeader.Field2))
		e.solarErrInfoField3.WithLabelValues(uid, battery).Set(float64(p.ParsedHeader.Field3))
		e.solarErrInfoField4.WithLabelValues(uid, battery).Set(float64(p.ParsedHeader.Field4))
		e.solarErrInfoField5.WithLabelValues(uid, battery).Set(float64(p.ParsedHeader.Field5))
	}

	for _, ev := range p.Events {
		code := strconv.FormatInt(ev.Code, 10)
		e.solarErrInfoEventTotal.WithLabelValues(uid, battery, code, ev.Name).Inc()
		e.solarErrInfoLastEventTS.WithLabelValues(uid, battery, code, ev.Name).Set(float64(ev.TS))
	}
	for _, q := range p.Quintuples {
		code := strconv.FormatInt(q.Code, 10)
		e.solarErrInfoEventTotal.WithLabelValues(uid, battery, code, q.Name).Inc()
	}
}

// bodyToString returns body as a UTF-8 string if it is valid UTF-8, otherwise
// as a hex string prefixed with "hex:" so log consumers can identify the encoding.
func bodyToString(raw []byte) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	const hextable = "0123456789abcdef"
	buf := make([]byte, 4+len(raw)*2)
	copy(buf, "hex:")
	for i, b := range raw {
		buf[4+i*2] = hextable[b>>4]
		buf[4+i*2+1] = hextable[b&0x0f]
	}
	return string(buf)
}

// handleSolarErrInfo handles POST /app/Solar/puterrinfo.php — a buffered
// error/event log upload from the device. The real cloud always returns "_1"
// regardless of body content.
func (e *Emulator) handleSolarErrInfo(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()

	var (
		raw           []byte
		bodyTruncated bool
	)
	if r.ContentLength != 0 {
		lr := io.LimitReader(r.Body, maxBodyLog+1)
		var err error
		raw, err = io.ReadAll(lr)
		if err == nil && len(raw) > maxBodyLog {
			raw = raw[:maxBodyLog]
			bodyTruncated = true
		}
		_, _ = io.Copy(io.Discard, r.Body)
	}

	parsed := parseSolarErrInfoBody(raw)

	e.reportsTotal.WithLabelValues(endpointSolarErrInfo).Inc()
	e.lastReportTimestamp.WithLabelValues(endpointSolarErrInfo).Set(float64(time.Now().Unix()))

	e.updateSolarErrInfoHeader(parsed.Header)
	e.updateErrInfoNamedMetrics(&parsed)

	// Build a concise list of event summaries for the log.
	eventSummaries := make([]string, 0, len(parsed.Events)+len(parsed.Quintuples))
	for _, ev := range parsed.Events {
		tsHuman := time.Unix(ev.TS, 0).UTC().Format(time.RFC3339)
		desc := errInfoCodeDescription(ev.Code)
		if desc != "" {
			eventSummaries = append(eventSummaries,
				fmt.Sprintf("{t=%s code=%d(%s) desc=%q value=%d}", tsHuman, ev.Code, ev.Name, desc, ev.Value))
		} else {
			eventSummaries = append(eventSummaries,
				fmt.Sprintf("{t=%s code=%d(%s) value=%d}", tsHuman, ev.Code, ev.Name, ev.Value))
		}
	}
	for _, q := range parsed.Quintuples {
		desc := errInfoCodeDescription(q.Code)
		if desc != "" {
			eventSummaries = append(eventSummaries,
				fmt.Sprintf("{code=%d(%s) desc=%q b=%d c=%d d=%d value=%d}", q.Code, q.Name, desc, q.SubB, q.SubC, q.SubD, q.Value))
		} else {
			eventSummaries = append(eventSummaries,
				fmt.Sprintf("{code=%d(%s) b=%d c=%d d=%d value=%d}", q.Code, q.Name, q.SubB, q.SubC, q.SubD, q.Value))
		}
	}

	attrs := []any{
		"method", r.Method,
		"path", r.URL.Path,
		"remote_addr", r.RemoteAddr,
		"user_agent", r.UserAgent(),
		"content_type", r.Header.Get("Content-Type"),
		"content_length", r.ContentLength,
		"body_raw", bodyToString(raw),
		"body_truncated", bodyTruncated,
		"uid", parsed.UID,
		"report_type", parsed.ReportType,
		"sw_version", parsed.ParsedHeader.SoftwareVersion,
		"header", parsed.Header,
		"events_count", len(parsed.Events) + len(parsed.Quintuples),
		"events_oldest_ts", parsed.OldestTS,
		"events_newest_ts", parsed.NewestTS,
		"distinct_codes", parsed.DistinctCodes,
		"distinct_values", parsed.DistinctValues,
		"events", eventSummaries,
	}
	if len(parsed.ParseErrors) > 0 {
		attrs = append(attrs, "parse_errors", parsed.ParseErrors)
	}
	slog.Info("cloud emulator: solar errinfo upload", attrs...)

	writeKongHeaders(w, startedAt, true)
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "_1")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
