package emulator

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// parseFloat parses s as a float64, returning 0 and false on failure.
func parseFloat(s string) (float64, bool) {
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

const (
	maxBodyLog  = 4096
	logCooldown = time.Minute

	endpointDateInfo     = "date_info"
	endpointReport       = "report"
	endpointSolarErrInfo = "solar_errinfo"
	endpointUnknown      = "unknown"

	pathDateInfo     = "/app/neng/getDateInfoeu.php"
	pathReport       = "/prod/api/v1/setB2500Report"
	pathSolarErrInfo = "/app/Solar/puterrinfo.php"
)

// corsHeadersBackendA are the CORS headers emitted by the direct-PHP backend
// (Backend A) that serves /app/neng/getDateInfoeu.php. This backend does NOT
// run behind Kong and sends the full explicit CORS block.
var corsHeadersBackendA = map[string]string{
	"Access-Control-Allow-Headers": "Content-Type, Authorization, Token, X-Requested-With, Origin, Accept, Accept-Language, Content-Language",
	"Access-Control-Allow-Methods": "GET, POST, PUT, DELETE, PATCH, OPTIONS",
	"Access-Control-Allow-Origin":  "*",
	"Access-Control-Max-Age":       "1728000",
}

// randomHexID returns 32 lowercase hex characters (16 random bytes), matching
// the format used by the real server for Trace-Id and X-Kong-Request-Id.
func randomHexID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// writeDateInfoHeaders sets response headers that match Backend A
// (getDateInfoeu.php): full CORS block, Trace-Id, Connection: keep-alive,
// Transfer-Encoding: chunked. Must be called before WriteHeader.
func writeDateInfoHeaders(w http.ResponseWriter, startedAt time.Time) {
	h := w.Header()
	for k, v := range corsHeadersBackendA {
		h.Set(k, v)
	}
	h.Set("Connection", "keep-alive")
	h.Set("Transfer-Encoding", "chunked")
	h.Set("Trace-Id", randomHexID())
	// Suppress Content-Length so Go does not override chunked framing.
	h.Del("Content-Length")
	_ = startedAt // retained in signature for symmetry with writeKongHeaders
}

// writeKongHeaders sets response headers that match Backend B (Kong 3.9.1 +
// PHP 8.1.2): vary, CORS credentials, Via, X-Kong-* timing/request-id, and
// optionally X-Powered-By. Must be called before WriteHeader.
//
// withPHP should be true for puterrinfo.php (PHP-generated response) and false
// for setB2500Report (JSON API response without the PHP header).
func writeKongHeaders(w http.ResponseWriter, startedAt time.Time, withPHP bool) {
	elapsed := time.Since(startedAt).Milliseconds()
	h := w.Header()
	h.Set("Vary", "Origin")
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Via", "1.1 kong/3.9.1")
	h.Set("X-Kong-Request-Id", randomHexID())
	h.Set("X-Kong-Upstream-Latency", strconv.FormatInt(elapsed, 10))
	h.Set("X-Kong-Proxy-Latency", "1")
	h.Set("Connection", "keep-alive")
	if withPHP {
		h.Set("X-Powered-By", "PHP/8.1.2")
	}
}

// Emulator emulates the eu.hamedata.com cloud server that Marstek battery
// devices contact for time-sync and telemetry reporting. It exposes Prometheus
// metrics derived from the intercepted request traffic.
type Emulator struct {
	tz *time.Location

	mu             sync.Mutex
	lastDeviceInfo deviceInfoLabels
	unknownRateMap map[string]time.Time // path → time of last warn log

	// metrics
	reportsTotal                *prometheus.CounterVec
	lastReportTimestamp         *prometheus.GaugeVec
	lastUnknownRequestTimestamp prometheus.Gauge
	reportPayloadBytes          prometheus.Gauge
	reportDecodeErrors          prometheus.Counter
	deviceInfo                  *prometheus.GaugeVec

	// cloud-report-only metrics (not available via MQTT cd=1)
	cellVoltageMillivolts  *prometheus.GaugeVec // b{n}max/min
	cellVoltageIndex       *prometheus.GaugeVec // b{n}maxn/minn
	solarInputVoltage      *prometheus.GaugeVec // pv1v/pv2v
	outputVoltage          *prometheus.GaugeVec // out1v/out2v
	cloudDeviceTimestamp   prometheus.Gauge
	wifiBTStatus           prometheus.Gauge
}

type deviceInfoLabels struct {
	uid                string
	deviceTypeReported string
	firmwareVersion    string
	swVersion          string
	subVersion         string
	modVersion         string
}

// New creates an Emulator and registers its metrics on reg.
// deviceType and deviceID are used as const labels matching the rest of the
// exporter so all metrics land in the same label namespace.
func New(reg prometheus.Registerer, deviceType, deviceID string, tz *time.Location) *Emulator {
	constLabels := prometheus.Labels{
		"device_type": deviceType,
		"device_id":   deviceID,
	}

	reportsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "marstek_cloud_reports_total",
		Help:        "Total number of HTTP requests received by the cloud emulator, by endpoint.",
		ConstLabels: constLabels,
	}, []string{"endpoint"})
	reg.MustRegister(reportsTotal)

	// Pre-initialise all known label values so the series exist even before the
	// device first calls in.
	reportsTotal.WithLabelValues(endpointDateInfo)
	reportsTotal.WithLabelValues(endpointReport)
	reportsTotal.WithLabelValues(endpointSolarErrInfo)
	reportsTotal.WithLabelValues(endpointUnknown)

	lastReportTimestamp := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_cloud_last_report_timestamp_seconds",
		Help:        "Unix timestamp of the last successful request per cloud endpoint.",
		ConstLabels: constLabels,
	}, []string{"endpoint"})
	reg.MustRegister(lastReportTimestamp)

	lastUnknownRequestTimestamp := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "marstek_cloud_last_unknown_request_timestamp_seconds",
		Help:        "Unix timestamp of the last request to an unrecognised cloud endpoint. Non-zero means a new firmware endpoint was discovered — check the logs.",
		ConstLabels: constLabels,
	})
	reg.MustRegister(lastUnknownRequestTimestamp)

	reportPayloadBytes := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "marstek_cloud_report_payload_bytes",
		Help:        "Decoded size in bytes of the latest setB2500Report payload. A change may indicate a firmware update.",
		ConstLabels: constLabels,
	})
	reg.MustRegister(reportPayloadBytes)

	reportDecodeErrors := prometheus.NewCounter(prometheus.CounterOpts{
		Name:        "marstek_cloud_report_decode_errors_total",
		Help:        "Total number of setB2500Report payloads that could not be decrypted or parsed. A non-zero value may indicate a firmware key rotation.",
		ConstLabels: constLabels,
	})
	reg.MustRegister(reportDecodeErrors)

	// marstek_device_info follows the Prometheus info-metric convention: value
	// is always 1 and the interesting data lives in the label set.
	deviceInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_device_info",
		Help:        "Device metadata parsed from the cloud time-sync request. Value is always 1; use label values for joins/alerts.",
		ConstLabels: constLabels,
	}, []string{"uid", "device_type_reported", "firmware_version", "sw_version", "sub_version", "mod_version"})
	reg.MustRegister(deviceInfo)

	cellVoltageMillivolts := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_cell_voltage_millivolts",
		Help:        "Per-pack min/max cell voltage in millivolts, from the cloud telemetry report.",
		ConstLabels: constLabels,
	}, []string{"pack", "bound"})
	reg.MustRegister(cellVoltageMillivolts)

	cellVoltageIndex := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_cell_voltage_cell_index",
		Help:        "Index of the min/max voltage cell within each pack, from the cloud telemetry report.",
		ConstLabels: constLabels,
	}, []string{"pack", "bound"})
	reg.MustRegister(cellVoltageIndex)

	solarInputVoltage := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_solar_input_voltage_millivolts",
		Help:        "Per-solar-input voltage in millivolts, from the cloud telemetry report.",
		ConstLabels: constLabels,
	}, []string{"input"})
	reg.MustRegister(solarInputVoltage)

	outputVoltage := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_output_voltage_millivolts",
		Help:        "Per-output-port voltage in millivolts, from the cloud telemetry report.",
		ConstLabels: constLabels,
	}, []string{"output"})
	reg.MustRegister(outputVoltage)

	cloudDeviceTimestamp := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "marstek_cloud_device_timestamp_seconds",
		Help:        "Device self-reported local time as a Unix timestamp, from the cloud telemetry report. Use to detect clock drift.",
		ConstLabels: constLabels,
	})
	reg.MustRegister(cloudDeviceTimestamp)

	wifiBTStatus := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "marstek_wifi_bt_status",
		Help:        "Raw wbs field from the cloud telemetry report, indicating Wi-Fi/Bluetooth connectivity state.",
		ConstLabels: constLabels,
	})
	reg.MustRegister(wifiBTStatus)

	return &Emulator{
		tz:                          tz,
		unknownRateMap:              make(map[string]time.Time),
		reportsTotal:                reportsTotal,
		lastReportTimestamp:         lastReportTimestamp,
		lastUnknownRequestTimestamp: lastUnknownRequestTimestamp,
		reportPayloadBytes:          reportPayloadBytes,
		reportDecodeErrors:          reportDecodeErrors,
		deviceInfo:                  deviceInfo,
		cellVoltageMillivolts:       cellVoltageMillivolts,
		cellVoltageIndex:            cellVoltageIndex,
		solarInputVoltage:           solarInputVoltage,
		outputVoltage:               outputVoltage,
		cloudDeviceTimestamp:        cloudDeviceTimestamp,
		wifiBTStatus:                wifiBTStatus,
	}
}

// Handler returns the http.Handler for the emulated cloud server.
func (e *Emulator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(pathDateInfo, e.handleDateInfo)
	mux.HandleFunc(pathReport, e.handleReport)
	mux.HandleFunc(pathSolarErrInfo, e.handleSolarErrInfo)
	mux.HandleFunc("/", e.handleUnknown)
	return mux
}

// handleDateInfo serves the time-sync response and populates marstek_device_info
// from query string parameters.
func (e *Emulator) handleDateInfo(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()

	slog.Debug("cloud emulator: date-info request",
		"method", r.Method,
		"path", r.URL.Path,
		"remote_addr", r.RemoteAddr,
		"user_agent", r.UserAgent(),
	)

	// Parse device metadata from query string.
	q := r.URL.Query()
	info := deviceInfoLabels{
		uid:                q.Get("uid"),
		deviceTypeReported: q.Get("aid"),
		firmwareVersion:    q.Get("fcv"),
		swVersion:          q.Get("sv"),
		subVersion:         q.Get("sbv"),
		modVersion:         q.Get("mv"),
	}
	e.updateDeviceInfo(info)

	e.reportsTotal.WithLabelValues(endpointDateInfo).Inc()
	e.lastReportTimestamp.WithLabelValues(endpointDateInfo).Set(float64(time.Now().Unix()))

	now := time.Now().In(e.tz)
	_, offsetSec := now.Zone()
	// ZZ = UTC offset in half-hours, zero-padded to two digits.
	zz := offsetSec / 1800

	writeDateInfoHeaders(w, startedAt)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	body := fmt.Sprintf("_%04d_%02d_%02d_%02d_%02d_%02d_%02d_0_0_0\n",
		now.Year(), now.Month(), now.Day(),
		now.Hour(), now.Minute(), now.Second(),
		zz,
	)
	_, _ = io.WriteString(w, body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// handleReport acknowledges a setB2500Report telemetry upload, decrypts the
// payload, and updates Prometheus metrics for the cloud-only telemetry fields.
func (e *Emulator) handleReport(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()

	slog.Debug("cloud emulator: telemetry report request",
		"method", r.Method,
		"path", r.URL.Path,
		"remote_addr", r.RemoteAddr,
		"user_agent", r.UserAgent(),
	)

	if v := r.URL.Query().Get("v"); v != "" {
		plaintext, err := DecryptReport(v)
		if err != nil {
			e.reportDecodeErrors.Inc()
			slog.Warn("cloud emulator: setB2500Report decrypt failed — firmware may have rotated the key",
				"err", err,
				"remote_addr", r.RemoteAddr,
			)
		} else {
			// Keep the payload-size canary working.
			e.reportPayloadBytes.Set(float64(len(plaintext)))

			fields, parseErr := ParseReport(plaintext)
			if parseErr != nil {
				e.reportDecodeErrors.Inc()
				slog.Warn("cloud emulator: setB2500Report parse failed", "err", parseErr)
			} else {
				slog.Debug("cloud emulator: setB2500Report decoded", "fields", fields)
				e.updateReportMetrics(fields)
			}
		}
	}

	e.reportsTotal.WithLabelValues(endpointReport).Inc()
	e.lastReportTimestamp.WithLabelValues(endpointReport).Set(float64(time.Now().Unix()))

	writeKongHeaders(w, startedAt, false)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", "21")
	w.WriteHeader(http.StatusOK)
	// Fixed 21-byte body matching the real server response observed in the pcap.
	_, _ = io.WriteString(w, `{"code":1,"msg":"ok"}`)
}

// updateReportMetrics populates the cloud-report-only Prometheus gauges from
// the parsed field map. Fields that are already exported by the MQTT collector
// (soc, bi/bo, pv, iv, bid/bod/pvd/ivd, vs/svs, b0f/b1f/b2f) are skipped to
// avoid double-counting.
func (e *Emulator) updateReportMetrics(f map[string]string) {
	// Per-pack cell voltage min/max and cell indices (packs 0–2).
	for _, pack := range []string{"0", "1", "2"} {
		if v, ok := parseFloat(f["b"+pack+"max"]); ok {
			e.cellVoltageMillivolts.WithLabelValues(pack, "max").Set(v)
		}
		if v, ok := parseFloat(f["b"+pack+"min"]); ok {
			e.cellVoltageMillivolts.WithLabelValues(pack, "min").Set(v)
		}
		if v, ok := parseFloat(f["b"+pack+"maxn"]); ok {
			e.cellVoltageIndex.WithLabelValues(pack, "max").Set(v)
		}
		if v, ok := parseFloat(f["b"+pack+"minn"]); ok {
			e.cellVoltageIndex.WithLabelValues(pack, "min").Set(v)
		}
	}

	// Per-solar-input voltage (inputs 1–2).
	for _, input := range []string{"1", "2"} {
		if v, ok := parseFloat(f["pv"+input+"v"]); ok {
			e.solarInputVoltage.WithLabelValues(input).Set(v)
		}
	}

	// Per-output-port voltage (outputs 1–2).
	for _, output := range []string{"1", "2"} {
		if v, ok := parseFloat(f["out"+output+"v"]); ok {
			e.outputVoltage.WithLabelValues(output).Set(v)
		}
	}

	// Wi-Fi/BT status.
	if v, ok := parseFloat(f["wbs"]); ok {
		e.wifiBTStatus.Set(v)
	}

	// Device self-reported timestamp: "2026-4-20 12:00:00"
	if dateStr, ok := f["date"]; ok {
		if ts, err := time.ParseInLocation("2006-1-2 15:04:05", dateStr, time.Local); err == nil {
			e.cloudDeviceTimestamp.Set(float64(ts.Unix()))
		} else {
			slog.Debug("cloud emulator: could not parse date field", "date", dateStr, "err", err)
		}
	}
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

// handleUnknown catches any path not matched above, logs it at warn level
// (with rate limiting per path) so new firmware endpoints are discoverable,
// and returns 404 mirroring upstream behaviour.
func (e *Emulator) handleUnknown(w http.ResponseWriter, r *http.Request) {
	// Read body for forensic logging.
	var bodyHex string
	bodyTruncated := false
	if r.ContentLength != 0 {
		lr := io.LimitReader(r.Body, maxBodyLog+1)
		raw, err := io.ReadAll(lr)
		if err == nil {
			if len(raw) > maxBodyLog {
				raw = raw[:maxBodyLog]
				bodyTruncated = true
			}
			if len(raw) > 0 {
				bodyHex = hex.EncodeToString(raw)
			}
		}
		_, _ = io.Copy(io.Discard, r.Body)
	}

	e.reportsTotal.WithLabelValues(endpointUnknown).Inc()
	e.lastUnknownRequestTimestamp.SetToCurrentTime()

	e.mu.Lock()
	last, seen := e.unknownRateMap[r.URL.Path]
	now := time.Now()
	if !seen || now.Sub(last) >= logCooldown {
		e.unknownRateMap[r.URL.Path] = now
		e.mu.Unlock()

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"raw_query", r.URL.RawQuery,
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent(),
			"content_type", r.Header.Get("Content-Type"),
		}
		if bodyHex != "" {
			attrs = append(attrs, "body_hex", bodyHex)
			if bodyTruncated {
				attrs = append(attrs, "body_truncated", true)
			}
		}
		slog.Warn("cloud emulator: unknown endpoint — possible new firmware request; check if this should be emulated", attrs...)
	} else {
		e.mu.Unlock()
		slog.Debug("cloud emulator: unknown endpoint (suppressed repeated warn)",
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
		)
	}

	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "404 page not found\n")
}

// updateDeviceInfo atomically replaces the marstek_device_info series so that
// a firmware upgrade doesn't leave stale label combinations in the registry.
func (e *Emulator) updateDeviceInfo(info deviceInfoLabels) {
	e.mu.Lock()
	defer e.mu.Unlock()

	prev := e.lastDeviceInfo
	if prev != (deviceInfoLabels{}) {
		e.deviceInfo.DeleteLabelValues(
			prev.uid, prev.deviceTypeReported, prev.firmwareVersion,
			prev.swVersion, prev.subVersion, prev.modVersion,
		)
	}

	e.deviceInfo.WithLabelValues(
		info.uid, info.deviceTypeReported, info.firmwareVersion,
		info.swVersion, info.subVersion, info.modVersion,
	).Set(1)
	e.lastDeviceInfo = info
}
