package main

import (
	"context"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // embed IANA timezone database so named zones work in minimal containers

	"github.com/lucavb/prometheus-marstek-mqtt-exporter/collector"
	"github.com/lucavb/prometheus-marstek-mqtt-exporter/config"
	"github.com/lucavb/prometheus-marstek-mqtt-exporter/emulator"
	"github.com/lucavb/prometheus-marstek-mqtt-exporter/esp32bridge"
	mqttclient "github.com/lucavb/prometheus-marstek-mqtt-exporter/mqtt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type pollResult int

const (
	pollResponded pollResult = iota
	pollFreshTelemetry
	pollPublishFailed
	pollTimedOut
)

type poller interface {
	Poll() error
}

type esp32StatusProvider interface {
	Status(context.Context) (esp32bridge.Status, error)
}

type recoverySupervisor interface {
	EnableRecovery()
	TriggerCheck()
}

func main() {
	cfg := config.Load()
	config.SetupLogger(cfg)
	config.LogConfig(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Dedicated registry excludes Go runtime / process metrics from /metrics.
	reg := prometheus.NewRegistry()
	coll := collector.New(reg, cfg.DeviceType, cfg.DeviceID, cfg.MetricTTL)

	client := mqttclient.New(cfg)
	if err := client.Connect(ctx); err != nil {
		slog.Error("failed to connect to MQTT broker", "error", err)
		return
	}
	defer client.Close()

	// responseCh signals the poll loop that a status message has arrived.
	responseCh := make(chan struct{}, 1)

	if err := client.Subscribe(func(payload string) {
		handleDevicePayload(coll, responseCh, payload)
	}); err != nil {
		slog.Error("failed to subscribe to device topic", "error", err)
		return
	}

	var bridge *esp32bridge.Client
	var supervisor *esp32bridge.Supervisor
	if cfg.ESP32BaseURL != "" {
		bridge = esp32bridge.New(cfg.ESP32BaseURL)
		metrics := esp32bridge.NewMetrics(reg, cfg.DeviceType, cfg.DeviceID)
		supervisor = esp32bridge.NewSupervisor(bridge, esp32bridge.SupervisorConfig{
			CheckInterval:       cfg.ESP32CheckInterval,
			MaxRecoveryAttempts: cfg.ESP32MaxRecoveryAttempts,
			WiFi: esp32bridge.WiFiConfig{
				SSID:     cfg.BatteryWiFiSSID,
				Password: cfg.BatteryWiFiPassword,
			},
			Metrics: metrics,
		})
		go supervisor.Run(ctx)
		slog.Info("ESP32 recovery supervisor started", "base_url", cfg.ESP32BaseURL, "check_interval", cfg.ESP32CheckInterval.String())
	}

	go runPollLoop(ctx, client, coll, cfg.PollInterval, cfg.ResponseTimeout, cfg.ESP32RecoveryMissedPolls, supervisor, responseCh, bridge, cfg.ESP32MetricsFallback)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!DOCTYPE html>
<html>
<head><title>Marstek Exporter</title></head>
<body>
<h1>Marstek Exporter</h1>
<ul>
  <li><a href="/metrics">Metrics</a></li>
  <li><a href="/health">Health</a></li>
</ul>
</body>
</html>
`))
	})

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	go func() {
		slog.Info("http server started", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "error", err)
		}
	}()

	var emulatorSrv *http.Server
	if cfg.EmulatorListenAddr != "" {
		em := emulator.New(reg, cfg.DeviceType, cfg.DeviceID, cfg.EmulatorLocation)
		emulatorSrv = &http.Server{
			Addr:    cfg.EmulatorListenAddr,
			Handler: em.Handler(),
		}
		go func() {
			slog.Info("cloud emulator started", "addr", cfg.EmulatorListenAddr)
			if err := emulatorSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("cloud emulator error", "error", err)
			}
		}()
	}

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if emulatorSrv != nil {
		_ = emulatorSrv.Shutdown(shutdownCtx)
	}
}

func runPollLoop(
	ctx context.Context,
	client poller,
	coll *collector.Collector,
	interval, timeout time.Duration,
	recoveryMissedPolls int,
	supervisor recoverySupervisor,
	responseCh <-chan struct{},
	esp32 esp32StatusProvider,
	esp32MetricsFallbackEnabled bool,
) {
	// Drain any stale signal before the first poll.
	select {
	case <-responseCh:
	default:
	}

	missedPolls := 0
	seenSuccessfulResponse := false
	freshTelemetryWindow := interval + timeout
	handleResult := func(result pollResult) {
		if isHealthyPollResult(result) {
			seenSuccessfulResponse = true
			if supervisor != nil {
				supervisor.EnableRecovery()
			}
		}
		if supervisor != nil && seenSuccessfulResponse && updateMissedPolls(&missedPolls, result, recoveryMissedPolls) {
			slog.Warn("triggering early ESP32 status check after missed MQTT polls", "missed_polls", recoveryMissedPolls)
			supervisor.TriggerCheck()
		}
	}

	result, err := runPoll(ctx, client, coll, timeout, freshTelemetryWindow, responseCh, esp32, esp32MetricsFallbackEnabled)
	if err != nil {
		return
	}
	handleResult(result)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			result, err := runPoll(ctx, client, coll, timeout, freshTelemetryWindow, responseCh, esp32, esp32MetricsFallbackEnabled)
			if err != nil {
				return
			}
			handleResult(result)
		case <-ctx.Done():
			return
		}
	}
}

func updateMissedPolls(missedPolls *int, result pollResult, threshold int) bool {
	if isHealthyPollResult(result) {
		*missedPolls = 0
		return false
	}
	*missedPolls++
	if *missedPolls < threshold {
		return false
	}
	*missedPolls = 0
	return true
}

func isHealthyPollResult(result pollResult) bool {
	return result == pollResponded || result == pollFreshTelemetry
}

func handleDevicePayload(coll *collector.Collector, responseCh chan<- struct{}, payload string) {
	if !coll.Update(payload) {
		return
	}
	coll.MarkUp()
	select {
	case responseCh <- struct{}{}:
	default:
	}
}

// runPoll sends one cd=1 poll and waits for either an in-window response or a
// timeout, after which it falls back to recent telemetry freshness.
// Returns a non-nil error only when ctx is cancelled, which signals the caller to stop.
func runPoll(
	ctx context.Context,
	client poller,
	coll *collector.Collector,
	timeout, freshTelemetryWindow time.Duration,
	responseCh <-chan struct{},
	esp32 esp32StatusProvider,
	esp32MetricsFallbackEnabled bool,
) (pollResult, error) {
	// Drain any stale response from the previous round.
	select {
	case <-responseCh:
	default:
	}

	coll.IncScrape()
	if err := client.Poll(); err != nil {
		slog.Warn("poll publish failed", "error", err)
		coll.IncPollPublishError()
		coll.MarkDown()
		return pollPublishFailed, nil
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-responseCh:
		slog.Debug("poll response received")
		return pollResponded, nil
	case <-timer.C:
		if coll.HasFreshPayload(freshTelemetryWindow) {
			slog.Debug("poll deadline passed but recent telemetry is still fresh", "fresh_telemetry_window", freshTelemetryWindow)
			return pollFreshTelemetry, nil
		}
		if esp32MetricsFallbackEnabled && esp32 != nil {
			if fallbackApplied, err := applyESP32MetricsFallback(ctx, esp32, coll); err != nil {
				slog.Warn("ESP32 metrics fallback failed", "error", err)
			} else if fallbackApplied {
				coll.IncESP32FallbackUpdate()
				slog.Warn("poll timed out; refreshed gauges from ESP32 status fallback")
			}
		}
		slog.Warn("poll timed out and no recent device telemetry was available", "timeout", timeout, "fresh_telemetry_window", freshTelemetryWindow)
		coll.IncPollTimeout()
		coll.MarkDown()
		return pollTimedOut, nil
	case <-ctx.Done():
		return pollTimedOut, ctx.Err()
	}
}

func applyESP32MetricsFallback(ctx context.Context, esp32 esp32StatusProvider, coll *collector.Collector) (bool, error) {
	status, err := esp32.Status(ctx)
	if err != nil {
		return false, err
	}
	if !status.Connected {
		return false, nil
	}

	values := metricValuesFromESP32Status(status)
	if len(values) == 0 {
		return false, nil
	}
	return coll.UpdateMetrics(values), nil
}

func metricValuesFromESP32Status(status esp32bridge.Status) []collector.MetricValue {
	values := make([]collector.MetricValue, 0, 12)
	add := func(name string, value float64, labels ...string) {
		values = append(values, collector.MetricValue{Name: name, Value: value, Labels: labels})
	}
	addIfFloat := func(name string, value *float64, labels ...string) {
		if value != nil {
			add(name, *value, labels...)
		}
	}
	addIfBool := func(name string, value *bool, labels ...string) {
		if value == nil {
			return
		}
		if *value {
			add(name, 1, labels...)
		} else {
			add(name, 0, labels...)
		}
	}

	addIfFloat("battery_soc_percent", status.SOCPercent)
	addIfFloat("battery_pack_soc_percent", status.SOCPercent, "0")
	addIfFloat("battery_remaining_wh", status.RemainingCapacityWh)
	addIfFloat("battery_dod_percent", status.DoDPercent)
	addIfFloat("solar_input_watts", status.In1PowerWatts, "1")
	addIfFloat("solar_input_watts", status.In2PowerWatts, "2")
	addIfFloat("output_watts", status.Out1PowerWatts, "1")
	addIfFloat("output_watts", status.Out2PowerWatts, "2")
	addIfBool("output_enabled", status.Out1Enabled, "1")
	addIfBool("output_enabled", status.Out2Enabled, "2")
	addIfFloat("temperature_celsius", status.TemperatureLowC, "min")
	addIfFloat("temperature_celsius", status.TemperatureHighC, "max")
	addIfFloat("daily_battery_charge_wh", status.DailyChargeWh)
	addIfFloat("daily_battery_discharge_wh", status.DailyDischargeWh)
	// /api/status only exposes one daily load energy value; map it to the
	// charge-side gauge to keep a single comparable daily-load trend.
	addIfFloat("daily_load_charge_wh", status.DailyLoadWh)

	return values
}
