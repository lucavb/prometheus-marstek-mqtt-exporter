package emulator

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	maxBodyLog  = 4096
	logCooldown = time.Minute

	endpointDateInfo = "date_info"
	endpointReport   = "report"
	endpointUnknown  = "unknown"

	pathDateInfo = "/app/neng/getDateInfoeu.php"
	pathReport   = "/prod/api/v1/setB2500Report"
)

// corsHeaders are the CORS headers observed in both pcap captures.
var corsHeaders = map[string]string{
	"Access-Control-Allow-Headers": "Content-Type, Authorization, Token, X-Requested-With, Origin, Accept, Accept-Language, Content-Language",
	"Access-Control-Allow-Methods": "GET, POST, PUT, DELETE, PATCH, OPTIONS",
	"Access-Control-Allow-Origin":  "*",
	"Access-Control-Max-Age":       "1728000",
}

// Emulator emulates the eu.hamedata.com cloud server that Marstek battery
// devices contact for time-sync and telemetry reporting. It exposes Prometheus
// metrics derived from the intercepted request traffic.
type Emulator struct {
	tz *time.Location

	mu              sync.Mutex
	lastDeviceInfo  deviceInfoLabels
	unknownRateMap  map[string]time.Time // path → time of last warn log

	// metrics
	reportsTotal                  *prometheus.CounterVec
	lastReportTimestamp           *prometheus.GaugeVec
	lastUnknownRequestTimestamp   prometheus.Gauge
	reportPayloadBytes            prometheus.Gauge
	deviceInfo                    *prometheus.GaugeVec
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

	// marstek_device_info follows the Prometheus info-metric convention: value
	// is always 1 and the interesting data lives in the label set.
	deviceInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_device_info",
		Help:        "Device metadata parsed from the cloud time-sync request. Value is always 1; use label values for joins/alerts.",
		ConstLabels: constLabels,
	}, []string{"uid", "device_type_reported", "firmware_version", "sw_version", "sub_version", "mod_version"})
	reg.MustRegister(deviceInfo)

	return &Emulator{
		tz:                          tz,
		unknownRateMap:              make(map[string]time.Time),
		reportsTotal:                reportsTotal,
		lastReportTimestamp:         lastReportTimestamp,
		lastUnknownRequestTimestamp: lastUnknownRequestTimestamp,
		reportPayloadBytes:          reportPayloadBytes,
		deviceInfo:                  deviceInfo,
	}
}

// Handler returns the http.Handler for the emulated cloud server.
func (e *Emulator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(pathDateInfo, e.handleDateInfo)
	mux.HandleFunc(pathReport, e.handleReport)
	mux.HandleFunc("/", e.handleUnknown)
	return mux
}

// setCORSHeaders writes the CORS headers observed in the pcap captures.
func setCORSHeaders(w http.ResponseWriter) {
	for k, v := range corsHeaders {
		w.Header().Set(k, v)
	}
}

// handleDateInfo serves the time-sync response and populates marstek_device_info
// from query string parameters.
func (e *Emulator) handleDateInfo(w http.ResponseWriter, r *http.Request) {
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

	setCORSHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "_%04d_%02d_%02d_%02d_%02d_%02d_%02d_0_0_0\n",
		now.Year(), now.Month(), now.Day(),
		now.Hour(), now.Minute(), now.Second(),
		zz,
	)
}

// handleReport acknowledges a setB2500Report telemetry upload and records
// the payload size for observability.
func (e *Emulator) handleReport(w http.ResponseWriter, r *http.Request) {
	slog.Debug("cloud emulator: telemetry report request",
		"method", r.Method,
		"path", r.URL.Path,
		"remote_addr", r.RemoteAddr,
		"user_agent", r.UserAgent(),
	)

	// The v= parameter is a base64url-encoded encrypted blob. We don't decrypt
	// it, but we record its decoded size as a cheap change signal.
	if v := r.URL.Query().Get("v"); v != "" {
		if decoded, err := base64.URLEncoding.DecodeString(v); err == nil {
			e.reportPayloadBytes.Set(float64(len(decoded)))
		} else if decoded, err := base64.RawURLEncoding.DecodeString(v); err == nil {
			e.reportPayloadBytes.Set(float64(len(decoded)))
		}
	}

	e.reportsTotal.WithLabelValues(endpointReport).Inc()
	e.lastReportTimestamp.WithLabelValues(endpointReport).Set(float64(time.Now().Unix()))

	setCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", "21")
	w.WriteHeader(http.StatusOK)
	// Fixed 21-byte body matching the real server response observed in the pcap.
	_, _ = io.WriteString(w, `{"code":1,"msg":"ok"}`)
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

	setCORSHeaders(w)
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
