package esp32bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/status" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"connected": true,
			"wifi_connected": true,
			"mqtt_connected": false,
			"soc_percent": 67.5,
			"remaining_capacity_wh": 1280,
			"dod": 80,
			"in1_power_w": 310,
			"in2_power_w": 205,
			"out1_power_w": 450,
			"out2_power_w": 325,
			"out1_enable": true,
			"out2_enable": false,
			"temperature_low_c": 24,
			"temperature_high_c": 31,
			"daily_charge_wh": 2500,
			"daily_discharge_wh": 2100,
			"daily_load_wh": 4100
		}`))
	}))
	defer server.Close()

	status, err := New(server.URL).Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !status.Connected || !status.WiFiConnected || status.MQTTConnected {
		t.Fatalf("unexpected status: %+v", status)
	}
	if !status.HasRuntimeTelemetry() {
		t.Fatal("expected runtime telemetry fields to be available")
	}
	if status.SOCPercent == nil || *status.SOCPercent != 67.5 {
		t.Fatalf("unexpected soc: %+v", status.SOCPercent)
	}
	if status.Out2Enabled == nil || *status.Out2Enabled {
		t.Fatalf("unexpected out2 enabled state: %+v", status.Out2Enabled)
	}
}

func TestClientConfigureWiFi(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/wifi" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode %s: %v", r.URL.Path, err)
		}
		request = body
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := New(server.URL)
	if err := client.ConfigureWiFi(context.Background(), WiFiConfig{SSID: "iot", Password: "wifi-pass"}); err != nil {
		t.Fatalf("configure wifi: %v", err)
	}

	if got := request["ssid"]; got != "iot" {
		t.Fatalf("wifi ssid = %v", got)
	}
	if got := request["password"]; got != "wifi-pass" {
		t.Fatalf("wifi password = %v", got)
	}
}

func TestClientNon2xxError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"not connected"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	if _, err := New(server.URL).Status(context.Background()); err == nil {
		t.Fatal("expected non-2xx error")
	}
}
