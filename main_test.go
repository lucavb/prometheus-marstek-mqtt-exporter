package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lucavb/prometheus-marstek-mqtt-exporter/collector"
	"github.com/lucavb/prometheus-marstek-mqtt-exporter/esp32bridge"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakePoller struct {
	err    error
	onPoll func()
}

func (f fakePoller) Poll() error {
	if f.onPoll != nil {
		f.onPoll()
	}
	return f.err
}

type fakeESP32Status struct {
	status esp32bridge.Status
	err    error
	calls  int
}

func (f *fakeESP32Status) Status(context.Context) (esp32bridge.Status, error) {
	f.calls++
	if f.err != nil {
		return esp32bridge.Status{}, f.err
	}
	return f.status, nil
}

type fakeSupervisor struct {
	mu           sync.Mutex
	enableCalls  int
	triggerCalls int
}

func (f *fakeSupervisor) EnableRecovery() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enableCalls++
}

func (f *fakeSupervisor) TriggerCheck() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggerCalls++
}

func (f *fakeSupervisor) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enableCalls, f.triggerCalls
}

func testCollector(t *testing.T) *collector.Collector {
	t.Helper()
	return collector.New(prometheus.NewRegistry(), "HMJ-2", "test-device", time.Minute)
}

func newCollectorWithRegistry(t *testing.T) (*collector.Collector, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	return collector.New(reg, "HMJ-2", "test-device", time.Minute), reg
}

func TestRunPollResponded(t *testing.T) {
	responseCh := make(chan struct{}, 1)
	poller := fakePoller{onPoll: func() {
		responseCh <- struct{}{}
	}}

	result, err := runPoll(context.Background(), poller, testCollector(t), time.Second, 2*time.Second, responseCh, nil, false)
	if err != nil {
		t.Fatalf("runPoll: %v", err)
	}
	if result != pollResponded {
		t.Fatalf("result = %v, want pollResponded", result)
	}
}

func TestRunPollPublishFailed(t *testing.T) {
	result, err := runPoll(context.Background(), fakePoller{err: errors.New("publish failed")}, testCollector(t), time.Second, 2*time.Second, make(chan struct{}, 1), nil, false)
	if err != nil {
		t.Fatalf("runPoll: %v", err)
	}
	if result != pollPublishFailed {
		t.Fatalf("result = %v, want pollPublishFailed", result)
	}
}

func TestRunPollTimedOut(t *testing.T) {
	result, err := runPoll(context.Background(), fakePoller{}, testCollector(t), time.Millisecond, 2*time.Millisecond, make(chan struct{}, 1), nil, false)
	if err != nil {
		t.Fatalf("runPoll: %v", err)
	}
	if result != pollTimedOut {
		t.Fatalf("result = %v, want pollTimedOut", result)
	}
}

func TestRunPollUsesFreshBackgroundTelemetry(t *testing.T) {
	coll := testCollector(t)
	responseCh := make(chan struct{}, 1)

	handleDevicePayload(coll, responseCh, "pe=75")

	result, err := runPoll(context.Background(), fakePoller{}, coll, time.Millisecond, time.Second, responseCh, nil, false)
	if err != nil {
		t.Fatalf("runPoll: %v", err)
	}
	if result != pollFreshTelemetry {
		t.Fatalf("result = %v, want pollFreshTelemetry", result)
	}
}

func TestHandleDevicePayloadSignalsParseableResponse(t *testing.T) {
	coll, reg := newCollectorWithRegistry(t)
	responseCh := make(chan struct{}, 1)

	handleDevicePayload(coll, responseCh, "pe=75")

	select {
	case <-responseCh:
	default:
		t.Fatal("expected parseable payload to signal a response")
	}

	expected := `
# HELP marstek_up 1 if recent parseable MQTT telemetry is healthy, 0 otherwise
# TYPE marstek_up gauge
marstek_up{device_id="test-device",device_type="HMJ-2"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_up"); err != nil {
		t.Fatalf("expected parseable payload to mark collector up: %v", err)
	}
}

func TestHandleDevicePayloadIgnoresUnparseableResponse(t *testing.T) {
	coll, reg := newCollectorWithRegistry(t)
	responseCh := make(chan struct{}, 1)

	handleDevicePayload(coll, responseCh, "not-a-payload")

	select {
	case <-responseCh:
		t.Fatal("did not expect unparseable payload to signal a response")
	default:
	}

	expected := `
# HELP marstek_up 1 if recent parseable MQTT telemetry is healthy, 0 otherwise
# TYPE marstek_up gauge
marstek_up{device_id="test-device",device_type="HMJ-2"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_up"); err != nil {
		t.Fatalf("unparseable payload should not mark collector up: %v", err)
	}
}

func TestUpdateMissedPollsTriggersAfterThresholdAndResets(t *testing.T) {
	missed := 0
	if updateMissedPolls(&missed, pollTimedOut, 3) {
		t.Fatal("unexpected trigger after first miss")
	}
	if updateMissedPolls(&missed, pollPublishFailed, 3) {
		t.Fatal("unexpected trigger after second miss")
	}
	if !updateMissedPolls(&missed, pollTimedOut, 3) {
		t.Fatal("expected trigger after third miss")
	}
	if missed != 0 {
		t.Fatalf("missed = %d, want reset to 0", missed)
	}
}

func TestUpdateMissedPollsResponseResetsCounter(t *testing.T) {
	missed := 2
	if updateMissedPolls(&missed, pollResponded, 3) {
		t.Fatal("response should not trigger recovery")
	}
	if missed != 0 {
		t.Fatalf("missed = %d, want reset to 0", missed)
	}
}

func TestUpdateMissedPollsFreshTelemetryResetsCounter(t *testing.T) {
	missed := 2
	if updateMissedPolls(&missed, pollFreshTelemetry, 3) {
		t.Fatal("fresh telemetry should not trigger recovery")
	}
	if missed != 0 {
		t.Fatalf("missed = %d, want reset to 0", missed)
	}
}

func TestRunPollLoopEnablesRecoveryAfterFirstResponse(t *testing.T) {
	responseCh := make(chan struct{}, 1)
	supervisor := &fakeSupervisor{}
	polls := 0
	poller := fakePoller{onPoll: func() {
		polls++
		if polls == 1 {
			responseCh <- struct{}{}
		}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runPollLoop(ctx, poller, testCollector(t), time.Hour, 10*time.Millisecond, 2, supervisor, responseCh, nil, false)
	time.Sleep(30 * time.Millisecond)
	cancel()
	time.Sleep(10 * time.Millisecond)

	enableCalls, _ := supervisor.counts()
	if enableCalls != 1 {
		t.Fatalf("enable calls = %d, want 1", enableCalls)
	}
}

func TestRunPollLoopKeepsFreshTelemetryHealthy(t *testing.T) {
	coll := testCollector(t)
	supervisor := &fakeSupervisor{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case <-ticker.C:
				if !coll.Update("pe=75") {
					t.Error("expected payload refresh to parse")
					return
				}
				coll.MarkUp()
			case <-ctx.Done():
				return
			}
		}
	}()

	go runPollLoop(ctx, fakePoller{}, coll, 5*time.Millisecond, time.Millisecond, 2, supervisor, make(chan struct{}, 1), nil, false)
	time.Sleep(35 * time.Millisecond)
	cancel()
	<-done
	time.Sleep(10 * time.Millisecond)

	enableCalls, triggerCalls := supervisor.counts()
	if enableCalls == 0 {
		t.Fatal("expected recovery to be enabled after fresh telemetry")
	}
	if triggerCalls != 0 {
		t.Fatalf("trigger calls = %d, want 0 while telemetry stays fresh", triggerCalls)
	}
}

func TestRunPollLoopSkipsEarlyTriggerUntilFirstResponse(t *testing.T) {
	responseCh := make(chan struct{}, 1)
	supervisor := &fakeSupervisor{}
	polls := 0
	poller := fakePoller{onPoll: func() {
		polls++
		if polls == 4 {
			responseCh <- struct{}{}
		}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runPollLoop(ctx, poller, testCollector(t), 5*time.Millisecond, time.Millisecond, 2, supervisor, responseCh, nil, false)
	time.Sleep(45 * time.Millisecond)
	cancel()
	time.Sleep(10 * time.Millisecond)

	enableCalls, triggerCalls := supervisor.counts()
	if enableCalls == 0 {
		t.Fatal("expected recovery to be enabled after first response")
	}
	if triggerCalls == 0 {
		t.Fatal("expected trigger after post-startup missed polls")
	}
}

func TestRunPollAppliesESP32MetricsFallbackOnTimeout(t *testing.T) {
	coll, reg := newCollectorWithRegistry(t)
	esp32 := &fakeESP32Status{
		status: esp32bridge.Status{
			Connected:           true,
			SOCPercent:          ptrFloat(44.5),
			RemainingCapacityWh: ptrFloat(900),
			In1PowerWatts:       ptrFloat(110),
			In2PowerWatts:       ptrFloat(90),
			Out1PowerWatts:      ptrFloat(210),
			Out2PowerWatts:      ptrFloat(170),
			Out1Enabled:         ptrBool(true),
			Out2Enabled:         ptrBool(false),
			TemperatureLowC:     ptrFloat(21),
			TemperatureHighC:    ptrFloat(29),
			DailyChargeWh:       ptrFloat(500),
			DailyDischargeWh:    ptrFloat(450),
			DailyLoadWh:         ptrFloat(980),
		},
	}

	result, err := runPoll(context.Background(), fakePoller{}, coll, time.Millisecond, 2*time.Millisecond, make(chan struct{}, 1), esp32, true)
	if err != nil {
		t.Fatalf("runPoll: %v", err)
	}
	if result != pollTimedOut {
		t.Fatalf("result = %v, want pollTimedOut", result)
	}
	if esp32.calls != 1 {
		t.Fatalf("status calls = %d, want 1", esp32.calls)
	}

	expected := `
# HELP marstek_battery_soc_percent State of charge in percent
# TYPE marstek_battery_soc_percent gauge
marstek_battery_soc_percent{device_id="test-device",device_type="HMJ-2"} 44.5
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_battery_soc_percent"); err != nil {
		t.Fatalf("expected fallback metrics to refresh cache: %v", err)
	}
}

func TestRunPollSkipsESP32FallbackWhenDisabled(t *testing.T) {
	esp32 := &fakeESP32Status{
		status: esp32bridge.Status{
			Connected:  true,
			SOCPercent: ptrFloat(65),
		},
	}
	result, err := runPoll(context.Background(), fakePoller{}, testCollector(t), time.Millisecond, 2*time.Millisecond, make(chan struct{}, 1), esp32, false)
	if err != nil {
		t.Fatalf("runPoll: %v", err)
	}
	if result != pollTimedOut {
		t.Fatalf("result = %v, want pollTimedOut", result)
	}
	if esp32.calls != 0 {
		t.Fatalf("status calls = %d, want 0", esp32.calls)
	}
}

func ptrFloat(v float64) *float64 { return &v }

func ptrBool(v bool) *bool { return &v }
