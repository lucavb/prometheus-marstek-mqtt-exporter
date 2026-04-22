package emulator

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"
)

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
