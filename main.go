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
	pollPublishFailed
	pollTimedOut
)

type poller interface {
	Poll() error
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
		coll.Update(payload)
		coll.MarkUp()
		select {
		case responseCh <- struct{}{}:
		default:
		}
	}); err != nil {
		slog.Error("failed to subscribe to device topic", "error", err)
		return
	}

	var supervisor *esp32bridge.Supervisor
	if cfg.ESP32BaseURL != "" {
		bridge := esp32bridge.New(cfg.ESP32BaseURL)
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

	go runPollLoop(ctx, client, coll, cfg.PollInterval, cfg.ResponseTimeout, cfg.ESP32RecoveryMissedPolls, supervisor, responseCh)

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

func runPollLoop(ctx context.Context, client poller, coll *collector.Collector, interval, timeout time.Duration, recoveryMissedPolls int, supervisor recoverySupervisor, responseCh <-chan struct{}) {
	// Drain any stale signal before the first poll.
	select {
	case <-responseCh:
	default:
	}

	missedPolls := 0
	seenSuccessfulResponse := false
	handleResult := func(result pollResult) {
		if result == pollResponded {
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

	result, err := runPoll(ctx, client, coll, timeout, responseCh)
	if err != nil {
		return
	}
	handleResult(result)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			result, err := runPoll(ctx, client, coll, timeout, responseCh)
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
	if result == pollResponded {
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

// runPoll sends one cd=1 poll and waits for a response or timeout.
// Returns a non-nil error only when ctx is cancelled, which signals the caller to stop.
func runPoll(ctx context.Context, client poller, coll *collector.Collector, timeout time.Duration, responseCh <-chan struct{}) (pollResult, error) {
	// Drain any stale response from the previous round.
	select {
	case <-responseCh:
	default:
	}

	coll.IncScrape()
	if err := client.Poll(); err != nil {
		slog.Warn("poll publish failed", "error", err)
		coll.IncScrapeError()
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
		slog.Warn("poll timed out waiting for device response", "timeout", timeout)
		coll.IncScrapeError()
		coll.MarkDown()
		return pollTimedOut, nil
	case <-ctx.Done():
		return pollTimedOut, ctx.Err()
	}
}
