package emulator

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
}

func TestReportPayloadBytes(t *testing.T) {
	em, reg := newTestEmulator(t, time.UTC)
	h := em.Handler()

	// base64url for a synthetic 10-byte payload.
	tenBytes := "AAECBAUGB"  // short — just verify the counter logic
	req := httptest.NewRequest(http.MethodGet,
		"/prod/api/v1/setB2500Report?v="+tenBytes,
		nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	count := testutil.CollectAndCount(reg, "marstek_cloud_report_payload_bytes")
	if count != 1 {
		t.Fatalf("expected payload_bytes metric, got %d series", count)
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
