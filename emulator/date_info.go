package emulator

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

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
