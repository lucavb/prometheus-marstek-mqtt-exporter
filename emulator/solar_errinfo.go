package emulator

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

// Known event codes observed in marstek-6.pcap (44 events, firmware fcv=202310231502).
// Each event is a triple: <code>.<unix_timestamp_seconds>.<value>.
// All timestamps are Unix seconds (UTC); a single upload covers the previous ~24 h.
//
//   - 84  Heartbeat toggle. Value alternates between 0 and 327679 (0x4FFFF) on a
//         ~60 s cycle. The device appears to emit a pair per measurement period.
//   - 75  Signed flag bitmap. Observed values: 0, -4194302, -4259838, -4325374.
//         Adjacent values differ by exactly 0x10000, suggesting bit-field changes.
//   - 78  Mode/state change. Value always 1 in this capture.
//   - 12  Monotonic counter. Observed values: 3652, 3653 (increments by 1).
//   - 91  One-off event; semantics unknown.
//   - 92  One-off event; semantics unknown.
//   - 77  One-off event; semantics unknown.
//   - 101 One-off event; semantics unknown.
//
// This dictionary is derived from a single capture and is tentative; new codes
// should be added as they are observed in subsequent captures.

// solarErrInfoEvent represents one event triple from the device's error log:
// <code>.<unix_ts_seconds>.<value>
type solarErrInfoEvent struct {
	Code  int64
	TS    int64
	Value int64
}

// solarErrInfoParsed is the result of a best-effort parse of a
// /app/Solar/puterrinfo.php request body.
//
// The observed wire format is:
//
//	<uid>:<a>:<b>:<c>:<d>:<e>:<f>:<code>.<ts>.<val>,<code>.<ts>.<val>,...
//
// where uid is the device UID and a..f are colon-separated integers whose
// exact semantics are still being validated (in our single capture,
// field b = sw_version = 110). Header greedily captures all consecutive
// integer fields so future firmware adding or removing a field won't break
// parsing — it only changes len(Header).
//
// ParseErrors is never nil. Any field or triple that could not be parsed is
// appended here and the rest of the parse continues. An empty body produces
// a zero-value struct with ParseErrors = ["empty body"].
type solarErrInfoParsed struct {
	UID            string
	Header         []int64
	Events         []solarErrInfoEvent
	DistinctCodes  []int64 // sorted, deduplicated
	DistinctValues []int64 // sorted, deduplicated
	OldestTS       int64   // 0 if no events
	NewestTS       int64   // 0 if no events
	ParseErrors    []string
}

// parseSolarErrInfoBody performs a safe best-effort parse of the raw body.
// It never panics regardless of input.
func parseSolarErrInfoBody(raw []byte) (p solarErrInfoParsed) {
	p.ParseErrors = []string{}

	s := strings.TrimSpace(string(raw))
	if s == "" {
		p.ParseErrors = append(p.ParseErrors, "empty body")
		return
	}

	// Split into colon-separated tokens. The first token is the UID.
	// Subsequent tokens are either integers (header) or the events blob.
	tokens := strings.Split(s, ":")
	if len(tokens) == 0 {
		p.ParseErrors = append(p.ParseErrors, "no colon-separated tokens")
		return
	}

	p.UID = tokens[0]

	// Greedily consume integer tokens into Header until one fails to parse,
	// then treat everything from that token onward (joined back with ":") as
	// the events blob. This tolerates a variable number of header fields.
	var eventsBlob string
	for i := 1; i < len(tokens); i++ {
		n, err := strconv.ParseInt(strings.TrimSpace(tokens[i]), 10, 64)
		if err != nil {
			// Remaining tokens form the events blob.
			eventsBlob = strings.Join(tokens[i:], ":")
			break
		}
		p.Header = append(p.Header, n)
	}
	// If all tokens were integers and no events blob was found, there are no events.
	// If there was exactly one extra non-integer token, eventsBlob is set.

	if eventsBlob == "" {
		// All tokens parsed as integers — no events section.
		return
	}

	// Parse events blob: comma-separated triples of the form code.ts.value
	// A trailing comma (as seen in real payloads) produces an empty final
	// token which we skip.
	codeSet := make(map[int64]struct{})
	valueSet := make(map[int64]struct{})

	for _, triple := range strings.Split(eventsBlob, ",") {
		triple = strings.TrimSpace(triple)
		if triple == "" {
			continue
		}
		parts := strings.SplitN(triple, ".", 3)
		if len(parts) != 3 {
			p.ParseErrors = append(p.ParseErrors, "bad triple (expected 3 dot-separated parts): "+triple)
			continue
		}
		code, err1 := strconv.ParseInt(parts[0], 10, 64)
		ts, err2 := strconv.ParseInt(parts[1], 10, 64)
		val, err3 := strconv.ParseInt(parts[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			p.ParseErrors = append(p.ParseErrors, "bad triple (non-integer field): "+triple)
			continue
		}
		p.Events = append(p.Events, solarErrInfoEvent{Code: code, TS: ts, Value: val})
		codeSet[code] = struct{}{}
		valueSet[val] = struct{}{}

		if p.OldestTS == 0 || ts < p.OldestTS {
			p.OldestTS = ts
		}
		if ts > p.NewestTS {
			p.NewestTS = ts
		}
	}

	// Build sorted, deduplicated slices.
	for c := range codeSet {
		p.DistinctCodes = append(p.DistinctCodes, c)
	}
	sort.Slice(p.DistinctCodes, func(i, j int) bool { return p.DistinctCodes[i] < p.DistinctCodes[j] })

	for v := range valueSet {
		p.DistinctValues = append(p.DistinctValues, v)
	}
	sort.Slice(p.DistinctValues, func(i, j int) bool { return p.DistinctValues[i] < p.DistinctValues[j] })

	return
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

	// Delete any series whose index no longer exists in this upload.
	for i := len(header); i < prevLen; i++ {
		e.solarErrInfoHeaderValue.DeleteLabelValues(fmt.Sprintf("%d", i))
	}

	for i, v := range header {
		e.solarErrInfoHeaderValue.WithLabelValues(strconv.Itoa(i)).Set(float64(v))
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
// regardless of body content. The body is read, logged at info level with a
// safe best-effort parse (so operators can validate the schema hypothesis), and
// then discarded. No per-event metrics are collected in this pass.
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
		"header", parsed.Header,
		"events_count", len(parsed.Events),
		"events_oldest_ts", parsed.OldestTS,
		"events_newest_ts", parsed.NewestTS,
		"distinct_codes", parsed.DistinctCodes,
		"distinct_values", parsed.DistinctValues,
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
