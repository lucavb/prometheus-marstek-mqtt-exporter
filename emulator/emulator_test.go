package emulator

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newTestEmulator creates an Emulator backed by a fresh registry and the given
// timezone for use in tests.
func newTestEmulator(t *testing.T, tz *time.Location) (*Emulator, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	em := New(reg, "TEST-TYPE", "testdeviceid", tz)
	return em, reg
}

func TestDateInfoBody(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	em, _ := newTestEmulator(t, loc)
	h := em.Handler()

	req := httptest.NewRequest(http.MethodGet,
		"/app/neng/getDateInfoeu.php?uid=deadbeef&aid=XYZ-0&fcv=000000000000&sv=1&sbv=2&mv=3",
		nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := strings.TrimSpace(rr.Body.String())
	// Body must start with underscore and contain the right number of segments.
	if !strings.HasPrefix(body, "_") {
		t.Fatalf("body does not start with underscore: %q", body)
	}
	parts := strings.Split(body, "_")
	// Split on "_" of "_YYYY_MM_DD_HH_MM_SS_ZZ_0_0_0" → ["", "YYYY","MM","DD","HH","MM","SS","ZZ","0","0","0"]
	if len(parts) != 11 {
		t.Fatalf("expected 11 parts after split, got %d: %q", len(parts), body)
	}
}

func TestDateInfoBodyUTC(t *testing.T) {
	em, _ := newTestEmulator(t, time.UTC)
	h := em.Handler()

	req := httptest.NewRequest(http.MethodGet, "/app/neng/getDateInfoeu.php", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	body := strings.TrimSpace(rr.Body.String())
	parts := strings.Split(body, "_")
	// Format: _YYYY_MM_DD_HH_MM_SS_ZZ_0_0_0
	// Split gives: ["","YYYY","MM","DD","HH","MM","SS","ZZ","0","0","0"]
	// ZZ is at index 7.
	if parts[7] != "00" {
		t.Errorf("expected ZZ=00 for UTC, got %q (body=%q)", parts[7], body)
	}
}

func TestDateInfoZZOffset(t *testing.T) {
	cases := []struct {
		tzName string
		wantZZ string // two-digit expected value
	}{
		{"UTC", "00"},
		{"Europe/Berlin", "02"}, // UTC+1 in winter / UTC+2 in summer; we check dynamic
		{"America/Los_Angeles", ""}, // negative offset — just check it's not "00"
	}

	for _, tc := range cases {
		loc, err := time.LoadLocation(tc.tzName)
		if err != nil {
			t.Skipf("timezone %s not available: %v", tc.tzName, err)
		}
		em, _ := newTestEmulator(t, loc)
		h := em.Handler()

		req := httptest.NewRequest(http.MethodGet, "/app/neng/getDateInfoeu.php", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		body := strings.TrimSpace(rr.Body.String())
		// Format: _YYYY_MM_DD_HH_MM_SS_ZZ_0_0_0 → split gives ZZ at index 7.
		parts := strings.Split(body, "_")
		zz := parts[7]

		// For all timezones, validate ZZ dynamically against the actual offset.
		_, offsetSec := time.Now().In(loc).Zone()
		wantZZ := fmt.Sprintf("%02d", offsetSec/1800)
		if zz != wantZZ {
			t.Errorf("%s: expected ZZ=%s, got %s (body=%q)", tc.tzName, wantZZ, zz, body)
		}
	}
}

func TestDateInfoHeaders(t *testing.T) {
	em, _ := newTestEmulator(t, time.UTC)
	h := em.Handler()

	req := httptest.NewRequest(http.MethodGet, "/app/neng/getDateInfoeu.php", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if ct := rr.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("unexpected Content-Type: %q", ct)
	}
	if origin := rr.Header().Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Errorf("unexpected CORS origin: %q", origin)
	}
	if conn := rr.Header().Get("Connection"); conn != "keep-alive" {
		t.Errorf("unexpected Connection: %q", conn)
	}
	if te := rr.Header().Get("Transfer-Encoding"); te != "chunked" {
		t.Errorf("unexpected Transfer-Encoding: %q", te)
	}
	traceID := rr.Header().Get("Trace-Id")
	if !isHex32(traceID) {
		t.Errorf("Trace-Id %q is not 32 lowercase hex chars", traceID)
	}
	// Backend A does NOT use Kong — these headers must be absent.
	if v := rr.Header().Get("Via"); v != "" {
		t.Errorf("unexpected Via header on date-info endpoint: %q", v)
	}
	if v := rr.Header().Get("X-Kong-Request-Id"); v != "" {
		t.Errorf("unexpected X-Kong-Request-Id on date-info endpoint: %q", v)
	}
}

func TestDateInfoDeviceInfoMetric(t *testing.T) {
	em, reg := newTestEmulator(t, time.UTC)
	h := em.Handler()

	req := httptest.NewRequest(http.MethodGet,
		"/app/neng/getDateInfoeu.php?uid=deadbeef&aid=XYZ-0&fcv=000000000000&sv=1&sbv=2&mv=3",
		nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	count := testutil.CollectAndCount(reg, "marstek_device_info")
	if count != 1 {
		t.Fatalf("expected 1 marstek_device_info series, got %d", count)
	}
}

func TestDateInfoDeviceInfoRotation(t *testing.T) {
	em, reg := newTestEmulator(t, time.UTC)
	h := em.Handler()

	// First call with firmware v1.
	req1 := httptest.NewRequest(http.MethodGet,
		"/app/neng/getDateInfoeu.php?uid=deadbeef&aid=XYZ-0&fcv=000000000001&sv=1&sbv=0&mv=0",
		nil)
	h.ServeHTTP(httptest.NewRecorder(), req1)

	// Second call with firmware v2 — old series must be removed.
	req2 := httptest.NewRequest(http.MethodGet,
		"/app/neng/getDateInfoeu.php?uid=deadbeef&aid=XYZ-0&fcv=000000000002&sv=1&sbv=0&mv=0",
		nil)
	h.ServeHTTP(httptest.NewRecorder(), req2)

	count := testutil.CollectAndCount(reg, "marstek_device_info")
	if count != 1 {
		t.Fatalf("expected exactly 1 marstek_device_info series after rotation, got %d", count)
	}
}

func TestReportResponse(t *testing.T) {
	em, _ := newTestEmulator(t, time.UTC)
	h := em.Handler()

	req := httptest.NewRequest(http.MethodGet,
		"/prod/api/v1/setB2500Report?v=aGVsbG8=", // base64 "hello" (5 bytes)
		nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if body := rr.Body.String(); body != `{"code":1,"msg":"ok"}` {
		t.Errorf("unexpected report body: %q", body)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("unexpected Content-Type: %q", ct)
	}
	if cl := rr.Header().Get("Content-Length"); cl != "21" {
		t.Errorf("unexpected Content-Length: %q", cl)
	}

	// Backend B (Kong) headers must be present.
	if via := rr.Header().Get("Via"); via != "1.1 kong/3.9.1" {
		t.Errorf("unexpected Via: %q", via)
	}
	if cred := rr.Header().Get("Access-Control-Allow-Credentials"); cred != "true" {
		t.Errorf("unexpected Access-Control-Allow-Credentials: %q", cred)
	}
	if vary := rr.Header().Get("Vary"); vary != "Origin" {
		t.Errorf("unexpected Vary: %q", vary)
	}
	if proxy := rr.Header().Get("X-Kong-Proxy-Latency"); proxy != "1" {
		t.Errorf("unexpected X-Kong-Proxy-Latency: %q", proxy)
	}
	if _, err := strconv.Atoi(rr.Header().Get("X-Kong-Upstream-Latency")); err != nil {
		t.Errorf("X-Kong-Upstream-Latency is not numeric: %q", rr.Header().Get("X-Kong-Upstream-Latency"))
	}
	if reqID := rr.Header().Get("X-Kong-Request-Id"); !isHex32(reqID) {
		t.Errorf("X-Kong-Request-Id %q is not 32 lowercase hex chars", reqID)
	}

	// Backend A (full CORS block) must be absent on this endpoint.
	if v := rr.Header().Get("Access-Control-Allow-Origin"); v != "" {
		t.Errorf("unexpected Access-Control-Allow-Origin on report endpoint: %q", v)
	}
	if v := rr.Header().Get("Trace-Id"); v != "" {
		t.Errorf("unexpected Trace-Id on report endpoint: %q", v)
	}
	// setB2500Report is not PHP-generated — no X-Powered-By.
	if v := rr.Header().Get("X-Powered-By"); v != "" {
		t.Errorf("unexpected X-Powered-By on report endpoint: %q", v)
	}
}

func TestReportPayloadBytes(t *testing.T) {
	em, reg := newTestEmulator(t, time.UTC)
	h := em.Handler()

	// Send the synthetic golden fixture so decryption succeeds and the
	// payload-bytes gauge is set to the plaintext length.
	v := goldenCiphertext(t)
	req := httptest.NewRequest(http.MethodGet,
		"/prod/api/v1/setB2500Report?v="+v,
		nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	gauges := testutil.CollectAndCount(reg, "marstek_cloud_report_payload_bytes")
	if gauges != 1 {
		t.Fatalf("expected payload_bytes metric, got %d series", gauges)
	}
}

func TestReportCloudMetrics(t *testing.T) {
	em, reg := newTestEmulator(t, time.UTC)
	h := em.Handler()

	v := goldenCiphertext(t)
	req := httptest.NewRequest(http.MethodGet,
		"/prod/api/v1/setB2500Report?v="+v,
		nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	// All cloud-report-only metric families must be present and non-empty.
	for _, name := range []string{
		"marstek_cell_voltage_millivolts",
		"marstek_cell_voltage_cell_index",
		"marstek_solar_input_voltage_millivolts",
		"marstek_output_voltage_millivolts",
		"marstek_cloud_device_timestamp_seconds",
		"marstek_wifi_bt_status",
	} {
		if c := testutil.CollectAndCount(reg, name); c == 0 {
			t.Errorf("metric %q has no series after a valid report", name)
		}
	}

	// Spot-check specific label combinations against the synthetic fixture values.
	metricChecks := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"marstek_cell_voltage_millivolts", map[string]string{"pack": "0", "bound": "max"}, 3300},
		{"marstek_cell_voltage_millivolts", map[string]string{"pack": "0", "bound": "min"}, 3290},
		{"marstek_cell_voltage_cell_index", map[string]string{"pack": "0", "bound": "max"}, 1},
		{"marstek_cell_voltage_cell_index", map[string]string{"pack": "0", "bound": "min"}, 2},
		{"marstek_solar_input_voltage_millivolts", map[string]string{"input": "1"}, 40000},
		{"marstek_solar_input_voltage_millivolts", map[string]string{"input": "2"}, 40000},
		{"marstek_output_voltage_millivolts", map[string]string{"output": "1"}, 30000},
		{"marstek_output_voltage_millivolts", map[string]string{"output": "2"}, 30000},
		{"marstek_wifi_bt_status", nil, 3},
	}
	for _, tc := range metricChecks {
		gathered, err := reg.Gather()
		if err != nil {
			t.Fatalf("registry.Gather: %v", err)
		}
		found := false
		for _, mf := range gathered {
			if mf.GetName() != "marstek_"+strings.TrimPrefix(tc.name, "marstek_") && mf.GetName() != tc.name {
				continue
			}
			for _, m := range mf.GetMetric() {
				match := true
				for wk, wv := range tc.labels {
					lmatch := false
					for _, lp := range m.GetLabel() {
						if lp.GetName() == wk && lp.GetValue() == wv {
							lmatch = true
							break
						}
					}
					if !lmatch {
						match = false
						break
					}
				}
				if match {
					got := m.GetGauge().GetValue()
					if got != tc.want {
						t.Errorf("%s%v = %v, want %v", tc.name, tc.labels, got, tc.want)
					}
					found = true
				}
			}
		}
		if !found {
			t.Errorf("metric %s with labels %v not found", tc.name, tc.labels)
		}
	}
}

func TestReportCounterIncrements(t *testing.T) {
	em, reg := newTestEmulator(t, time.UTC)
	h := em.Handler()

	req := httptest.NewRequest(http.MethodGet, "/prod/api/v1/setB2500Report", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if err := testutil.CollectAndCompare(reg, strings.NewReader(`
# HELP marstek_cloud_reports_total Total number of HTTP requests received by the cloud emulator, by endpoint.
# TYPE marstek_cloud_reports_total counter
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="date_info"} 0
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="report"} 2
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="solar_errinfo"} 0
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="unknown"} 0
`), "marstek_cloud_reports_total"); err != nil {
		t.Errorf("counter mismatch: %v", err)
	}
}

func TestUnknownEndpoint404(t *testing.T) {
	em, reg := newTestEmulator(t, time.UTC)
	h := em.Handler()

	req := httptest.NewRequest(http.MethodGet, "/some/new/firmware/path?foo=bar", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}

	// Counter for unknown must be 1.
	if err := testutil.CollectAndCompare(reg, strings.NewReader(`
# HELP marstek_cloud_reports_total Total number of HTTP requests received by the cloud emulator, by endpoint.
# TYPE marstek_cloud_reports_total counter
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="date_info"} 0
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="report"} 0
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="solar_errinfo"} 0
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="unknown"} 1
`), "marstek_cloud_reports_total"); err != nil {
		t.Errorf("counter mismatch: %v", err)
	}

	// Gauge must be set (non-zero).
	count := testutil.CollectAndCount(reg, "marstek_cloud_last_unknown_request_timestamp_seconds")
	if count != 1 {
		t.Fatalf("expected 1 unknown_timestamp series, got %d", count)
	}
}

func TestUnknownEndpointBodyCapture(t *testing.T) {
	em, _ := newTestEmulator(t, time.UTC)
	h := em.Handler()

	// Intercept slog output to verify body_hex is present.
	var loggedAttrs []slog.Attr
	handler := &captureHandler{fn: func(r slog.Record) {
		r.Attrs(func(a slog.Attr) bool {
			loggedAttrs = append(loggedAttrs, a)
			return true
		})
	}}
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	body := strings.NewReader("binarydata")
	req := httptest.NewRequest(http.MethodPost, "/unknown/path", body)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len("binarydata"))
	h.ServeHTTP(httptest.NewRecorder(), req)

	found := false
	for _, a := range loggedAttrs {
		if a.Key == "body_hex" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected body_hex in log attrs for unknown endpoint with body")
	}
}

func TestUnknownEndpointRateLimit(t *testing.T) {
	em, _ := newTestEmulator(t, time.UTC)
	h := em.Handler()

	var warnCount int
	handler := &captureHandler{fn: func(r slog.Record) {
		if r.Level == slog.LevelWarn {
			warnCount++
		}
	}}
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	req := httptest.NewRequest(http.MethodGet, "/brand/new/path", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if warnCount != 1 {
		t.Errorf("expected exactly 1 warn log for rapid duplicate unknown path, got %d", warnCount)
	}
}

func TestSolarErrInfoHeaders(t *testing.T) {
	em, _ := newTestEmulator(t, time.UTC)
	h := em.Handler()

	body := strings.NewReader("3601115030374d33300f1365:0:110:22:0:0:131:84.1776750259.0,")
	req := httptest.NewRequest(http.MethodPost, "/app/Solar/puterrinfo.php", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ContentLength = int64(body.Len())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if b := rr.Body.String(); b != "_1" {
		t.Errorf("unexpected body: %q", b)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/html; charset=UTF-8" {
		t.Errorf("unexpected Content-Type: %q", ct)
	}
	if te := rr.Header().Get("Transfer-Encoding"); te != "chunked" {
		t.Errorf("unexpected Transfer-Encoding: %q", te)
	}
	if via := rr.Header().Get("Via"); via != "1.1 kong/3.9.1" {
		t.Errorf("unexpected Via: %q", via)
	}
	if cred := rr.Header().Get("Access-Control-Allow-Credentials"); cred != "true" {
		t.Errorf("unexpected Access-Control-Allow-Credentials: %q", cred)
	}
	if vary := rr.Header().Get("Vary"); vary != "Origin" {
		t.Errorf("unexpected Vary: %q", vary)
	}
	if php := rr.Header().Get("X-Powered-By"); php != "PHP/8.1.2" {
		t.Errorf("unexpected X-Powered-By: %q", php)
	}
	if proxy := rr.Header().Get("X-Kong-Proxy-Latency"); proxy != "1" {
		t.Errorf("unexpected X-Kong-Proxy-Latency: %q", proxy)
	}
	if _, err := strconv.Atoi(rr.Header().Get("X-Kong-Upstream-Latency")); err != nil {
		t.Errorf("X-Kong-Upstream-Latency is not numeric: %q", rr.Header().Get("X-Kong-Upstream-Latency"))
	}
	if reqID := rr.Header().Get("X-Kong-Request-Id"); !isHex32(reqID) {
		t.Errorf("X-Kong-Request-Id %q is not 32 lowercase hex chars", reqID)
	}
	// Backend A CORS headers must be absent on this Kong-fronted endpoint.
	if v := rr.Header().Get("Access-Control-Allow-Origin"); v != "" {
		t.Errorf("unexpected Access-Control-Allow-Origin on solar-errinfo endpoint: %q", v)
	}
	if v := rr.Header().Get("Trace-Id"); v != "" {
		t.Errorf("unexpected Trace-Id on solar-errinfo endpoint: %q", v)
	}
}

func TestRandomHexID(t *testing.T) {
	for i := 0; i < 10; i++ {
		id := randomHexID()
		if !isHex32(id) {
			t.Errorf("randomHexID() = %q, want 32 lowercase hex chars", id)
		}
	}
	// Two calls must not return the same value (collision probability 2^-128).
	a, b := randomHexID(), randomHexID()
	if a == b {
		t.Errorf("randomHexID() returned identical values on consecutive calls: %q", a)
	}
}

// isHex32 returns true if s consists of exactly 32 lowercase hexadecimal chars.
func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// captureHandler is a minimal slog.Handler that calls fn for each record.
type captureHandler struct {
	fn func(slog.Record)
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.fn(r)
	return nil
}
func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler  { return h }
func (h *captureHandler) WithGroup(name string) slog.Handler        { return h }
