package esp32bridge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeBridge struct {
	mu       sync.Mutex
	statuses []Status
	events   []string

	wifiErr error
}

func (f *fakeBridge) Status(context.Context) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.statuses) == 0 {
		return Status{}, errors.New("no status queued")
	}
	status := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	f.events = append(f.events, "status")
	return status, nil
}

func (f *fakeBridge) ConfigureWiFi(context.Context, WiFiConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "wifi")
	return f.wifiErr
}

func (f *fakeBridge) eventCount(event string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, got := range f.events {
		if got == event {
			count++
		}
	}
	return count
}

func TestSupervisorRecoverySequence(t *testing.T) {
	bridge := &fakeBridge{statuses: []Status{
		{Connected: true, WiFiConnected: false, MQTTConnected: false},
		{Connected: true, WiFiConnected: true, MQTTConnected: false},
	}}
	supervisor := NewSupervisor(bridge, SupervisorConfig{
		MaxRecoveryAttempts: 3,
		WiFi:                WiFiConfig{SSID: "iot", Password: "wifi-pass"},
		WaitTimeout:         50 * time.Millisecond,
		PollPeriod:          time.Millisecond,
	})
	supervisor.EnableRecovery()

	if err := supervisor.Check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}

	want := []string{"status", "wifi", "status"}
	if len(bridge.events) != len(want) {
		t.Fatalf("events = %v, want %v", bridge.events, want)
	}
	for i := range want {
		if bridge.events[i] != want[i] {
			t.Fatalf("events = %v, want %v", bridge.events, want)
		}
	}
}

func TestSupervisorIgnoresMQTTOnlyDisconnect(t *testing.T) {
	bridge := &fakeBridge{statuses: []Status{
		{Connected: true, WiFiConnected: true, MQTTConnected: false},
	}}
	supervisor := NewSupervisor(bridge, SupervisorConfig{
		MaxRecoveryAttempts: 3,
		WaitTimeout:         50 * time.Millisecond,
		PollPeriod:          time.Millisecond,
	})
	supervisor.EnableRecovery()

	if err := supervisor.Check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if got := bridge.eventCount("wifi"); got != 0 {
		t.Fatalf("wifi count = %d, want 0", got)
	}
}

func TestSupervisorSkipsColdStartRecoveryUntilEnabled(t *testing.T) {
	bridge := &fakeBridge{
		statuses: []Status{
			{Connected: true, WiFiConnected: false, MQTTConnected: false},
			{Connected: true, WiFiConnected: false, MQTTConnected: false},
			{Connected: true, WiFiConnected: true, MQTTConnected: false},
		},
	}
	supervisor := NewSupervisor(bridge, SupervisorConfig{
		MaxRecoveryAttempts: 3,
		WaitTimeout:         50 * time.Millisecond,
		PollPeriod:          time.Millisecond,
	})

	if err := supervisor.Check(context.Background()); err != nil {
		t.Fatalf("cold start check: %v", err)
	}
	if got := bridge.eventCount("wifi"); got != 0 {
		t.Fatalf("wifi count before enable = %d, want 0", got)
	}

	supervisor.EnableRecovery()
	if err := supervisor.Check(context.Background()); err != nil {
		t.Fatalf("enabled check: %v", err)
	}
	if got := bridge.eventCount("wifi"); got != 1 {
		t.Fatalf("wifi count after enable = %d, want 1", got)
	}
}

func TestSupervisorStopsAfterMaxRecoveryAttempts(t *testing.T) {
	bridge := &fakeBridge{
		statuses: []Status{
			{Connected: true, WiFiConnected: false, MQTTConnected: false},
		},
		wifiErr: errors.New("wifi failed"),
	}
	supervisor := NewSupervisor(bridge, SupervisorConfig{
		MaxRecoveryAttempts: 3,
		WaitTimeout:         50 * time.Millisecond,
		PollPeriod:          time.Millisecond,
	})
	supervisor.EnableRecovery()

	for i := 0; i < 4; i++ {
		_ = supervisor.Check(context.Background())
	}
	if got := bridge.eventCount("wifi"); got != 3 {
		t.Fatalf("wifi count = %d, want 3", got)
	}
}

func TestSupervisorHealthyStatusResetsRecoveryAttempts(t *testing.T) {
	bridge := &fakeBridge{statuses: []Status{
		{Connected: true, WiFiConnected: false, MQTTConnected: false},
		{Connected: true, WiFiConnected: true, MQTTConnected: true},
		{Connected: true, WiFiConnected: false, MQTTConnected: false},
	}}
	supervisor := NewSupervisor(bridge, SupervisorConfig{
		MaxRecoveryAttempts: 1,
		WaitTimeout:         50 * time.Millisecond,
		PollPeriod:          time.Millisecond,
	})
	supervisor.EnableRecovery()

	_ = supervisor.Check(context.Background())
	_ = supervisor.Check(context.Background())
	_ = supervisor.Check(context.Background())

	if got := bridge.eventCount("wifi"); got != 2 {
		t.Fatalf("wifi count = %d, want 2 after healthy reset", got)
	}
}

func TestSupervisorRecordsRecoveryMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg, "HMJ-2", "test-device")
	bridge := &fakeBridge{statuses: []Status{
		{Connected: true, WiFiConnected: false, MQTTConnected: false},
		{Connected: true, WiFiConnected: true, MQTTConnected: false},
	}}
	supervisor := NewSupervisor(bridge, SupervisorConfig{
		MaxRecoveryAttempts: 3,
		WiFi:                WiFiConfig{SSID: "iot", Password: "wifi-pass"},
		WaitTimeout:         50 * time.Millisecond,
		PollPeriod:          time.Millisecond,
		Metrics:             metrics,
	})
	supervisor.EnableRecovery()

	if err := supervisor.Check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	expected := `
# HELP marstek_esp32_battery_ble_connected 1 if the ESP32 bridge is connected to the battery over BLE, 0 otherwise
# TYPE marstek_esp32_battery_ble_connected gauge
marstek_esp32_battery_ble_connected{device_id="test-device",device_type="HMJ-2"} 1
# HELP marstek_esp32_battery_mqtt_connected 1 if the battery reports MQTT connected through the ESP32 bridge, 0 otherwise
# TYPE marstek_esp32_battery_mqtt_connected gauge
marstek_esp32_battery_mqtt_connected{device_id="test-device",device_type="HMJ-2"} 0
# HELP marstek_esp32_battery_wifi_connected 1 if the battery reports WiFi connected through the ESP32 bridge, 0 otherwise
# TYPE marstek_esp32_battery_wifi_connected gauge
marstek_esp32_battery_wifi_connected{device_id="test-device",device_type="HMJ-2"} 1
# HELP marstek_esp32_recovery_attempts_total Total number of ESP32 bridge recovery attempts started
# TYPE marstek_esp32_recovery_attempts_total counter
marstek_esp32_recovery_attempts_total{device_id="test-device",device_type="HMJ-2"} 1
# HELP marstek_esp32_recovery_failures_total Total number of ESP32 bridge recovery attempts that failed
# TYPE marstek_esp32_recovery_failures_total counter
marstek_esp32_recovery_failures_total{device_id="test-device",device_type="HMJ-2"} 0
# HELP marstek_esp32_recovery_manual_intervention_required 1 if automatic ESP32 recovery is exhausted and human intervention is required, 0 otherwise
# TYPE marstek_esp32_recovery_manual_intervention_required gauge
marstek_esp32_recovery_manual_intervention_required{device_id="test-device",device_type="HMJ-2"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"marstek_esp32_battery_ble_connected",
		"marstek_esp32_battery_wifi_connected",
		"marstek_esp32_battery_mqtt_connected",
		"marstek_esp32_recovery_attempts_total",
		"marstek_esp32_recovery_failures_total",
		"marstek_esp32_recovery_manual_intervention_required",
	); err != nil {
		t.Fatalf("metrics mismatch: %v", err)
	}
}

func TestSupervisorRecordsManualInterventionMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg, "HMJ-2", "test-device")
	bridge := &fakeBridge{
		statuses: []Status{
			{Connected: true, WiFiConnected: false, MQTTConnected: false},
		},
		wifiErr: errors.New("wifi failed"),
	}
	supervisor := NewSupervisor(bridge, SupervisorConfig{
		MaxRecoveryAttempts: 1,
		WaitTimeout:         50 * time.Millisecond,
		PollPeriod:          time.Millisecond,
		Metrics:             metrics,
	})
	supervisor.EnableRecovery()

	_ = supervisor.Check(context.Background())
	_ = supervisor.Check(context.Background())

	expected := `
# HELP marstek_esp32_recovery_attempts_total Total number of ESP32 bridge recovery attempts started
# TYPE marstek_esp32_recovery_attempts_total counter
marstek_esp32_recovery_attempts_total{device_id="test-device",device_type="HMJ-2"} 1
# HELP marstek_esp32_recovery_failures_total Total number of ESP32 bridge recovery attempts that failed
# TYPE marstek_esp32_recovery_failures_total counter
marstek_esp32_recovery_failures_total{device_id="test-device",device_type="HMJ-2"} 1
# HELP marstek_esp32_recovery_manual_intervention_required 1 if automatic ESP32 recovery is exhausted and human intervention is required, 0 otherwise
# TYPE marstek_esp32_recovery_manual_intervention_required gauge
marstek_esp32_recovery_manual_intervention_required{device_id="test-device",device_type="HMJ-2"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"marstek_esp32_recovery_attempts_total",
		"marstek_esp32_recovery_failures_total",
		"marstek_esp32_recovery_manual_intervention_required",
	); err != nil {
		t.Fatalf("metrics mismatch: %v", err)
	}
}
