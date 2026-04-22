package emulator

import (
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"time"
)

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
