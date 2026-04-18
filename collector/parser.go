package collector

import (
	"strconv"
	"strings"
)

// Parse parses the flat "key=value,key=value,..." payload from the Marstek device.
// Malformed tokens (missing '=') and surrounding whitespace are silently ignored.
func Parse(raw string) map[string]string {
	result := make(map[string]string)
	for _, token := range strings.Split(raw, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		idx := strings.IndexByte(token, '=')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(token[:idx])
		val := strings.TrimSpace(token[idx+1:])
		if key != "" {
			result[key] = val
		}
	}
	return result
}

func intVal(m map[string]string, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, false
	}
	return n, true
}

func floatVal(m map[string]string, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
