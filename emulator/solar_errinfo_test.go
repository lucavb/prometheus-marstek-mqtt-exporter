package emulator

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// realSample is the single observed payload from the device, used as the
// happy-path input for both parser and handler tests.
const realSample = `3601115030374d33300f1365:0:110:41:0:0:67:84.1776748256.0,84.1776748262.327679,84.1776748316.0,84.1776748322.327679,84.1776748376.0,84.1776748382.327679,84.1776748436.0,84.1776748442.327679,84.1776748496.0,84.1776748502.327679,84.1776748556.0,84.1776748562.327679,84.1776748616.0,84.1776748622.327679,84.1776748676.0,84.1776748747.327679,84.1776748969.0,84.1776748980.327679,84.1776749145.0,84.1776749156.327679,84.1776749299.0,84.1776749310.327679,84.1776749475.0,84.1776749486.327679,84.1776749629.0,84.1776749640.327679,84.1776749716.0,84.1776749727.327679,84.1776749893.0,84.1776749969.327679,84.1776750259.0,84.1776750270.327679,84.1776750469.0,84.1776750480.327679,84.1776750556.0,84.1776750567.327679,84.1776750765.0,84.1776750776.327679,84.1776750885.0,84.1776750896.327679,84.1776751039.0,84.1776751139.327679,`

// ---- parser unit tests (table-driven) ----

func TestParseSolarErrInfoBody(t *testing.T) {
	t.Run("happy path real sample", func(t *testing.T) {
		p := parseSolarErrInfoBody([]byte(realSample))

		if p.UID != "3601115030374d33300f1365" {
			t.Errorf("uid = %q, want 3601115030374d33300f1365", p.UID)
		}
		wantHeader := []int64{0, 110, 41, 0, 0, 67}
		if len(p.Header) != len(wantHeader) {
			t.Fatalf("header len = %d, want %d; got %v", len(p.Header), len(wantHeader), p.Header)
		}
		for i, v := range wantHeader {
			if p.Header[i] != v {
				t.Errorf("header[%d] = %d, want %d", i, p.Header[i], v)
			}
		}
		if len(p.Events) != 42 {
			t.Errorf("events count = %d, want 42", len(p.Events))
		}
		if len(p.DistinctCodes) != 1 || p.DistinctCodes[0] != 84 {
			t.Errorf("distinct_codes = %v, want [84]", p.DistinctCodes)
		}
		if len(p.DistinctValues) != 2 || p.DistinctValues[0] != 0 || p.DistinctValues[1] != 327679 {
			t.Errorf("distinct_values = %v, want [0 327679]", p.DistinctValues)
		}
		if p.OldestTS != 1776748256 {
			t.Errorf("oldest_ts = %d, want 1776748256", p.OldestTS)
		}
		if p.NewestTS != 1776751139 {
			t.Errorf("newest_ts = %d, want 1776751139", p.NewestTS)
		}
		if len(p.ParseErrors) != 0 {
			t.Errorf("unexpected parse_errors: %v", p.ParseErrors)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		p := parseSolarErrInfoBody([]byte{})
		if len(p.ParseErrors) == 0 || p.ParseErrors[0] != "empty body" {
			t.Errorf("parse_errors = %v, want [\"empty body\"]", p.ParseErrors)
		}
		if p.UID != "" || len(p.Events) != 0 {
			t.Errorf("expected zero-value struct for empty body, got uid=%q events=%d", p.UID, len(p.Events))
		}
	})

	t.Run("whitespace-only body", func(t *testing.T) {
		p := parseSolarErrInfoBody([]byte("   \t\n"))
		if len(p.ParseErrors) == 0 || p.ParseErrors[0] != "empty body" {
			t.Errorf("parse_errors = %v, want [\"empty body\"]", p.ParseErrors)
		}
	})

	t.Run("triple with non-numeric value", func(t *testing.T) {
		body := "uid123:0:110:0:0:0:1:84.1776748256.notanumber,84.1776748262.327679,"
		p := parseSolarErrInfoBody([]byte(body))
		if len(p.ParseErrors) == 0 {
			t.Error("expected parse error for non-numeric triple value")
		}
		// The second valid triple should still be parsed.
		if len(p.Events) != 1 {
			t.Errorf("events count = %d, want 1 (valid triple only)", len(p.Events))
		}
		if p.Events[0].Value != 327679 {
			t.Errorf("events[0].value = %d, want 327679", p.Events[0].Value)
		}
	})

	t.Run("truncated trailing triple", func(t *testing.T) {
		// Two dot-separated parts only — should be skipped.
		body := "uid123:0:110:0:0:0:1:84.1776748256.0,84.1776"
		p := parseSolarErrInfoBody([]byte(body))
		if len(p.ParseErrors) == 0 {
			t.Error("expected parse error for truncated triple")
		}
		if len(p.Events) != 1 {
			t.Errorf("events count = %d, want 1 (only the complete triple)", len(p.Events))
		}
	})

	t.Run("future firmware with 7 header ints", func(t *testing.T) {
		body := "uid123:0:110:41:0:0:67:99:84.1776748256.0,"
		p := parseSolarErrInfoBody([]byte(body))
		if len(p.Header) != 7 {
			t.Errorf("header len = %d, want 7; got %v", len(p.Header), p.Header)
		}
		if len(p.Events) != 1 {
			t.Errorf("events count = %d, want 1", len(p.Events))
		}
	})

	t.Run("no events section — all tokens are integers", func(t *testing.T) {
		body := "uid123:0:110:41:0:0:67"
		p := parseSolarErrInfoBody([]byte(body))
		if len(p.ParseErrors) != 0 {
			t.Errorf("unexpected parse errors: %v", p.ParseErrors)
		}
		if len(p.Events) != 0 {
			t.Errorf("events count = %d, want 0", len(p.Events))
		}
		if p.UID != "uid123" {
			t.Errorf("uid = %q, want uid123", p.UID)
		}
	})

	t.Run("marstek-6 header 0:110:23:0:0:131", func(t *testing.T) {
		// Header observed in the marstek-6.pcap capture: 6 integers where the
		// 6th element (index 5, value 131) was not present in earlier captures.
		body := "3601115030374d33300f1365:0:110:23:0:0:131:84.1776750480.327679,"
		p := parseSolarErrInfoBody([]byte(body))
		if len(p.ParseErrors) != 0 {
			t.Errorf("unexpected parse errors: %v", p.ParseErrors)
		}
		wantHeader := []int64{0, 110, 23, 0, 0, 131}
		if len(p.Header) != len(wantHeader) {
			t.Fatalf("header len = %d, want %d; got %v", len(p.Header), len(wantHeader), p.Header)
		}
		for i, want := range wantHeader {
			if p.Header[i] != want {
				t.Errorf("header[%d] = %d, want %d", i, p.Header[i], want)
			}
		}
		if len(p.Events) != 1 {
			t.Errorf("events count = %d, want 1", len(p.Events))
		}
	})
}

// ---- handler tests ----

func TestSolarErrInfoResponse(t *testing.T) {
	em, _ := newTestEmulator(t, time.UTC)
	h := em.Handler()

	req := httptest.NewRequest(http.MethodPost, pathSolarErrInfo,
		strings.NewReader(realSample))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ContentLength = int64(len(realSample))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if body := rr.Body.String(); body != "_1" {
		t.Errorf("body = %q, want \"_1\"", body)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/html; charset=UTF-8" {
		t.Errorf("Content-Type = %q, want \"text/html; charset=UTF-8\"", ct)
	}
}

func TestSolarErrInfoEmptyBody(t *testing.T) {
	em, _ := newTestEmulator(t, time.UTC)
	h := em.Handler()

	req := httptest.NewRequest(http.MethodPost, pathSolarErrInfo, http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if body := rr.Body.String(); body != "_1" {
		t.Errorf("body = %q, want \"_1\"", body)
	}
}

func TestSolarErrInfoCounterIncrements(t *testing.T) {
	em, reg := newTestEmulator(t, time.UTC)
	h := em.Handler()

	req := httptest.NewRequest(http.MethodPost, pathSolarErrInfo, http.NoBody)
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if err := testutil.CollectAndCompare(reg, strings.NewReader(`
# HELP marstek_cloud_reports_total Total number of HTTP requests received by the cloud emulator, by endpoint.
# TYPE marstek_cloud_reports_total counter
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="date_info"} 0
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="report"} 0
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="solar_errinfo"} 2
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="unknown"} 0
`), "marstek_cloud_reports_total"); err != nil {
		t.Errorf("counter mismatch: %v", err)
	}
}

func TestSolarErrInfoNotMarkedUnknown(t *testing.T) {
	em, reg := newTestEmulator(t, time.UTC)
	h := em.Handler()

	req := httptest.NewRequest(http.MethodPost, pathSolarErrInfo,
		strings.NewReader(realSample))
	req.ContentLength = int64(len(realSample))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if err := testutil.CollectAndCompare(reg, strings.NewReader(`
# HELP marstek_cloud_reports_total Total number of HTTP requests received by the cloud emulator, by endpoint.
# TYPE marstek_cloud_reports_total counter
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="date_info"} 0
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="report"} 0
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="solar_errinfo"} 1
marstek_cloud_reports_total{device_id="testdeviceid",device_type="TEST-TYPE",endpoint="unknown"} 0
`), "marstek_cloud_reports_total"); err != nil {
		t.Errorf("counter mismatch: %v", err)
	}

	if ts := testutil.ToFloat64(em.lastUnknownRequestTimestamp); ts != 0 {
		t.Errorf("lastUnknownRequestTimestamp = %v, want 0", ts)
	}
}

func TestSolarErrInfoHeaderGauge(t *testing.T) {
	em, reg := newTestEmulator(t, time.UTC)
	h := em.Handler()

	// First upload: marstek-6 header with 6 fields (including the new index=5).
	body6 := "3601115030374d33300f1365:0:110:23:0:0:131:84.1776750480.0,"
	req := httptest.NewRequest(http.MethodPost, pathSolarErrInfo, strings.NewReader(body6))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ContentLength = int64(len(body6))
	h.ServeHTTP(httptest.NewRecorder(), req)

	// All six indices must be set.
	if c := testutil.CollectAndCount(reg, "marstek_cloud_solar_errinfo_header_value"); c != 6 {
		t.Fatalf("expected 6 header series after first upload, got %d", c)
	}

	// Spot-check known mappings: index=1 → sw_version=110, index=5 → 131.
	wantValues := map[string]float64{
		"0": 0, "1": 110, "2": 23, "3": 0, "4": 0, "5": 131,
	}
	gathered, err := reg.Gather()
	if err != nil {
		t.Fatalf("registry.Gather: %v", err)
	}
	for _, mf := range gathered {
		if mf.GetName() != "marstek_cloud_solar_errinfo_header_value" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var idx string
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "index" {
					idx = lp.GetValue()
				}
			}
			want, ok := wantValues[idx]
			if !ok {
				t.Errorf("unexpected index label %q", idx)
				continue
			}
			if got := m.GetGauge().GetValue(); got != want {
				t.Errorf("index=%s: got %v, want %v", idx, got, want)
			}
			delete(wantValues, idx)
		}
	}
	for missing := range wantValues {
		t.Errorf("header gauge index=%s not found in metrics", missing)
	}

	// Second upload: shorter header with only 5 fields — index=5 must disappear.
	body5 := "3601115030374d33300f1365:0:110:23:0:0:84.1776750480.0,"
	req2 := httptest.NewRequest(http.MethodPost, pathSolarErrInfo, strings.NewReader(body5))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.ContentLength = int64(len(body5))
	h.ServeHTTP(httptest.NewRecorder(), req2)

	if c := testutil.CollectAndCount(reg, "marstek_cloud_solar_errinfo_header_value"); c != 5 {
		t.Errorf("expected 5 header series after shrinkage, got %d", c)
	}
}

func TestSolarErrInfoLogsParsed(t *testing.T) {
	em, _ := newTestEmulator(t, time.UTC)
	h := em.Handler()

	var loggedAttrs []slog.Attr
	handler := &captureHandler{fn: func(r slog.Record) {
		r.Attrs(func(a slog.Attr) bool {
			loggedAttrs = append(loggedAttrs, a)
			return true
		})
	}}
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	req := httptest.NewRequest(http.MethodPost, pathSolarErrInfo,
		strings.NewReader(realSample))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ContentLength = int64(len(realSample))
	h.ServeHTTP(httptest.NewRecorder(), req)

	wantKeys := []string{"body_raw", "uid", "events_count", "distinct_codes"}
	found := make(map[string]bool)
	for _, a := range loggedAttrs {
		found[a.Key] = true
	}
	for _, k := range wantKeys {
		if !found[k] {
			t.Errorf("expected log attr %q to be present; got attrs: %v", k, loggedAttrs)
		}
	}
}
