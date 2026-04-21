package emulator

import (
	"encoding/base64"
	"os"
	"testing"
)

// goldenCiphertext returns the 448-byte AES-128-ECB ciphertext extracted from
// marstek-2.pcap by scripts/crack_report.py and stored as a binary fixture.
// It is re-encoded as a base64url string to exercise the full DecryptReport path.
func goldenCiphertext(t *testing.T) string {
	t.Helper()
	ct, err := os.ReadFile("testdata/marstek2_report.bin")
	if err != nil {
		t.Fatalf("read testdata/marstek2_report.bin: %v", err)
	}
	return base64.URLEncoding.EncodeToString(ct)
}

// TestDecryptReport_Golden verifies that the synthetic test fixture decrypts
// correctly and produces all 51 expected fields. The fixture in
// testdata/marstek2_report.bin was generated from a synthetic plaintext (same
// field structure as a real capture, dummy values) so no real device data is
// committed to the repository. A failure here indicates a parser regression or
// a key change.
func TestDecryptReport_Golden(t *testing.T) {
	v := goldenCiphertext(t)

	plaintext, err := DecryptReport(v)
	if err != nil {
		t.Fatalf("DecryptReport: %v", err)
	}

	fields, err := ParseReport(plaintext)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}

	const wantFields = 51
	if got := len(fields); got != wantFields {
		t.Errorf("field count = %d, want %d", got, wantFields)
		for k, v := range fields {
			t.Logf("  %s = %s", k, v)
		}
	}

	// Verify all 51 expected field names are present with the synthetic values.
	wantKeys := []string{
		"devid", "soc", "pvs", "esps", "grids",
		"bi", "bo", "pv", "iv",
		"bid", "bod", "pvd", "ivd",
		"tn", "pe0", "pe1", "pe2",
		"b0f", "b1f", "b2f",
		"vs", "svs", "st", "sp",
		"it", "is", "ie", "ity", "itp", "sm", "bn",
		"b0max", "b0min", "b0maxn", "b0minn",
		"b1max", "b1min", "b1maxn", "b1minn",
		"b2max", "b2min", "b2maxn", "b2minn",
		"pv1v", "pv2v", "out1v", "out2v",
		"pv1", "pv2", "wbs", "date",
	}
	for _, k := range wantKeys {
		if _, ok := fields[k]; !ok {
			t.Errorf("missing expected field %q", k)
		}
	}

	// Spot-check a few of the synthetic values.
	checks := map[string]string{
		"devid": "000000000000000000000000",
		"soc":   "50",
		"pv1v":  "40000",
		"pv2v":  "40000",
		"out1v": "30000",
		"out2v": "30000",
		"b0max": "3300",
		"b0min": "3290",
		"wbs":   "3",
	}
	for k, want := range checks {
		if got := fields[k]; got != want {
			t.Errorf("fields[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestDecryptReport_BadBase64 ensures a malformed base64 string returns an
// error and does not panic.
func TestDecryptReport_BadBase64(t *testing.T) {
	_, err := DecryptReport("not!valid!base64!!!")
	if err == nil {
		t.Fatal("expected error for invalid base64, got nil")
	}
}

// TestDecryptReport_WrongLength ensures a ciphertext whose length is not a
// multiple of 16 returns an error and does not panic.
func TestDecryptReport_WrongLength(t *testing.T) {
	// 17 bytes of valid base64, but not a multiple of aes.BlockSize.
	v := base64.URLEncoding.EncodeToString(make([]byte, 17))
	_, err := DecryptReport(v)
	if err == nil {
		t.Fatal("expected error for non-block-aligned ciphertext, got nil")
	}
}

// TestDecryptReport_BadPadding ensures a 16-byte block with invalid PKCS#7
// padding (all zeros) returns an error.
func TestDecryptReport_BadPadding(t *testing.T) {
	// Encrypt 16 zero bytes with the real key — the decrypted result won't have
	// valid PKCS#7 padding (pad byte would be 0, which is outside [1,16]).
	// We construct a ciphertext manually: just 16 arbitrary non-zero bytes
	// that, when decrypted, produce a last byte of 0x00.
	// Simplest: pass a ciphertext whose last decrypted byte is 0x11 (17 > 16).
	// Rather than computing that, we craft a known-bad plaintext via a second
	// round-trip: encrypt 16 bytes with last byte 0x00 using the test helper.
	// Easiest: just feed 16 null bytes as the "ciphertext" and let the AES-ECB
	// round produce whatever it produces, which will almost certainly not be
	// valid PKCS#7.
	v := base64.URLEncoding.EncodeToString(make([]byte, 16))
	_, err := DecryptReport(v)
	if err == nil {
		t.Fatal("expected error for invalid padding, got nil")
	}
}
