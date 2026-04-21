package emulator

import (
	"crypto/aes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
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
