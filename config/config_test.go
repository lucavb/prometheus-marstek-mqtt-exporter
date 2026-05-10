package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testLoad(t *testing.T, args []string, env map[string]string) (*Config, error) {
	t.Helper()
	lookup := func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
	readFile := func(string) ([]byte, error) {
		return nil, errors.New("unexpected read")
	}
	hostname := func() (string, error) {
		return "test-host", nil
	}
	return load(args, lookup, readFile, hostname, 1234)
}

func TestESP32RecoveryDefaultsDisabled(t *testing.T) {
	cfg, err := testLoad(t, nil, map[string]string{
		"MARSTEK_MQTT_HOST": "mqtt.local",
		"MARSTEK_DEVICE_ID": "battery-1",
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.ESP32BaseURL != "" {
		t.Fatalf("ESP32 recovery should default to disabled, got %q", cfg.ESP32BaseURL)
	}
	if cfg.ESP32MetricsFallback {
		t.Fatal("ESP32 metrics fallback should default to disabled")
	}
	if cfg.ESP32CheckInterval != 300*time.Second {
		t.Fatalf("ESP32 check interval = %s, want 300s", cfg.ESP32CheckInterval)
	}
	if cfg.ESP32RecoveryMissedPolls != 3 {
		t.Fatalf("ESP32 missed polls = %d, want 3", cfg.ESP32RecoveryMissedPolls)
	}
	if cfg.ESP32MaxRecoveryAttempts != 3 {
		t.Fatalf("ESP32 max recovery attempts = %d, want 3", cfg.ESP32MaxRecoveryAttempts)
	}
}

func TestESP32BaseURLDoesNotRequireWiFiProvisioning(t *testing.T) {
	cfg, err := testLoad(t, nil, map[string]string{
		"MARSTEK_MQTT_HOST":      "mqtt.local",
		"MARSTEK_DEVICE_ID":      "battery-1",
		"MARSTEK_ESP32_BASE_URL": "http://esp32.local",
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.BatteryWiFiSSID != "" || cfg.BatteryWiFiPassword != "" {
		t.Fatalf("expected wifi credentials to be optional, got ssid=%q", cfg.BatteryWiFiSSID)
	}
}

func TestESP32WiFiProvisioningRequiresBothFields(t *testing.T) {
	_, err := testLoad(t, nil, map[string]string{
		"MARSTEK_MQTT_HOST":         "mqtt.local",
		"MARSTEK_DEVICE_ID":         "battery-1",
		"MARSTEK_ESP32_BASE_URL":    "http://esp32.local",
		"MARSTEK_BATTERY_WIFI_SSID": "iot",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "must be provided together") {
		t.Fatalf("error = %q, want paired wifi credential validation", err)
	}
}

func TestESP32MetricsFallbackRequiresBaseURL(t *testing.T) {
	_, err := testLoad(t, nil, map[string]string{
		"MARSTEK_MQTT_HOST":                      "mqtt.local",
		"MARSTEK_DEVICE_ID":                      "battery-1",
		"MARSTEK_ESP32_METRICS_FALLBACK_ENABLED": "true",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "esp32-base-url") {
		t.Fatalf("error = %q, want missing base url validation", err)
	}
}

func TestESP32CheckIntervalSecondsMustBePositive(t *testing.T) {
	_, err := testLoad(t, []string{"--esp32-check-interval-seconds=0"}, map[string]string{
		"MARSTEK_MQTT_HOST": "mqtt.local",
		"MARSTEK_DEVICE_ID": "battery-1",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "esp32-check-interval-seconds") {
		t.Fatalf("error = %q, want check interval validation", err)
	}
}

func TestESP32RecoveryConfig(t *testing.T) {
	cfg, err := testLoad(t, nil, map[string]string{
		"MARSTEK_MQTT_HOST":                      "mqtt.local",
		"MARSTEK_MQTT_PORT":                      "1884",
		"MARSTEK_MQTT_USERNAME":                  "mqtt-user",
		"MARSTEK_MQTT_PASSWORD":                  "mqtt-pass",
		"MARSTEK_DEVICE_ID":                      "battery-1",
		"MARSTEK_ESP32_BASE_URL":                 "http://esp32.local/",
		"MARSTEK_ESP32_METRICS_FALLBACK_ENABLED": "true",
		"MARSTEK_ESP32_CHECK_INTERVAL_SECONDS":   "60",
		"MARSTEK_ESP32_RECOVERY_MISSED_POLLS":    "4",
		"MARSTEK_ESP32_MAX_RECOVERY_ATTEMPTS":    "2",
		"MARSTEK_BATTERY_WIFI_SSID":              "iot",
		"MARSTEK_BATTERY_WIFI_PASSWORD":          "secret",
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.ESP32BaseURL != "http://esp32.local" {
		t.Fatalf("ESP32 base URL = %q", cfg.ESP32BaseURL)
	}
	if !cfg.ESP32MetricsFallback {
		t.Fatal("expected ESP32 metrics fallback to be enabled")
	}
	if cfg.ESP32CheckInterval != time.Minute {
		t.Fatalf("ESP32 check interval = %s, want 1m", cfg.ESP32CheckInterval)
	}
	if cfg.ESP32RecoveryMissedPolls != 4 {
		t.Fatalf("ESP32 missed polls = %d, want 4", cfg.ESP32RecoveryMissedPolls)
	}
	if cfg.ESP32MaxRecoveryAttempts != 2 {
		t.Fatalf("ESP32 max recovery attempts = %d, want 2", cfg.ESP32MaxRecoveryAttempts)
	}
	if cfg.BatteryWiFiSSID != "iot" || cfg.BatteryWiFiPassword != "secret" {
		t.Fatalf("battery provisioning config not loaded correctly: %+v", cfg)
	}
}
