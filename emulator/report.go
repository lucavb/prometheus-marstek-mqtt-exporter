package emulator

import (
	"crypto/aes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// reportKey is the AES-128 key used by Marstek/Hame firmware to encrypt the
// v= payload on GET /prod/api/v1/setB2500Report. It is the ASCII string
// "hamedata" repeated twice to fill 16 bytes. Discovered by scored dictionary
// attack against captures from marstek-2.pcap; as of 2026-04-21 this key has
// not been published elsewhere in any public index.
const reportKey = "hamedatahamedata"

// DecryptReport base64url-decodes v and AES-128-ECB-decrypts it using the
// fixed firmware key, then strips PKCS#7 padding and returns the plaintext.
//
// The ciphertext length must be a non-zero multiple of aes.BlockSize (16).
// Malformed base64, wrong-length ciphertext, or invalid PKCS#7 padding all
// return a non-nil error.
func DecryptReport(v string) (string, error) {
	ct, err := base64.URLEncoding.DecodeString(v)
	if err != nil {
		ct, err = base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			return "", fmt.Errorf("base64url decode: %w", err)
		}
	}

	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return "", fmt.Errorf("ciphertext length %d is not a non-zero multiple of %d", len(ct), aes.BlockSize)
	}

	block, err := aes.NewCipher([]byte(reportKey))
	if err != nil {
		// Only fails for invalid key lengths; our key is exactly 16 bytes.
		return "", fmt.Errorf("aes.NewCipher: %w", err)
	}

	// AES-ECB: decrypt each 16-byte block independently. Go's crypto/cipher
	// does not ship an ECB mode constructor (deliberately — NIST SP 800-131Ar3
	// disallows it for new uses), so we apply Block.Decrypt directly.
	pt := make([]byte, len(ct))
	for i := 0; i < len(ct); i += aes.BlockSize {
		block.Decrypt(pt[i:i+aes.BlockSize], ct[i:i+aes.BlockSize])
	}

	// Strip and validate PKCS#7 padding.
	pt, err = pkcs7Unpad(pt)
	if err != nil {
		return "", err
	}

	return string(pt), nil
}

// ParseReport URL-decodes the plaintext produced by DecryptReport and returns
// a flat key→value map. For duplicate keys (none observed in the wild) the
// first value wins.
func ParseReport(plaintext string) (map[string]string, error) {
	vals, err := url.ParseQuery(plaintext)
	if err != nil {
		return nil, fmt.Errorf("url.ParseQuery: %w", err)
	}
	m := make(map[string]string, len(vals))
	for k, vs := range vals {
		if len(vs) > 0 {
			m[k] = vs[0]
		}
	}
	return m, nil
}

// pkcs7Unpad validates and strips PKCS#7 padding from b. The padding byte
// value must be in [1, aes.BlockSize] and all padding bytes must be equal.
func pkcs7Unpad(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("pkcs7: empty input")
	}
	pad := int(b[len(b)-1])
	if pad < 1 || pad > aes.BlockSize {
		return nil, fmt.Errorf("pkcs7: invalid pad byte 0x%02x", b[len(b)-1])
	}
	if len(b) < pad {
		return nil, fmt.Errorf("pkcs7: pad %d exceeds data length %d", pad, len(b))
	}
	for _, byt := range b[len(b)-pad:] {
		if int(byt) != pad {
			return nil, fmt.Errorf("pkcs7: inconsistent padding (expected 0x%02x, got 0x%02x)", pad, byt)
		}
	}
	return b[:len(b)-pad], nil
}

// parseFloat parses s as a float64, returning 0 and false on failure.
func parseFloat(s string) (float64, bool) {
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
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

	// Per-solar-input voltage and power (inputs 1–2).
	for _, input := range []string{"1", "2"} {
		if v, ok := parseFloat(f["pv"+input+"v"]); ok {
			e.solarInputVoltage.WithLabelValues(input).Set(v)
		}
		if v, ok := parseFloat(f["pv"+input]); ok {
			e.solarInputPower.WithLabelValues(input).Set(v)
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
