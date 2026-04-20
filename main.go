package main

import (
	"context"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/lucavb/prometheus-marstek-mqtt-exporter/collector"
	"github.com/lucavb/prometheus-marstek-mqtt-exporter/config"
	"github.com/lucavb/prometheus-marstek-mqtt-exporter/emulator"
	mqttclient "github.com/lucavb/prometheus-marstek-mqtt-exporter/mqtt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	cfg := config.Load()
	config.SetupLogger(cfg)
	config.LogConfig(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Dedicated registry excludes Go runtime / process metrics from /metrics.
	reg := prometheus.NewRegistry()
	coll := collector.New(reg, cfg.DeviceType, cfg.DeviceID)

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

	go func() {
		// Drain any stale signal before the first poll.
		select {
		case <-responseCh:
		default:
		}

		if err := runPoll(ctx, client, coll, cfg.ResponseTimeout, responseCh); err != nil {
			return
		}

		ticker := time.NewTicker(cfg.PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := runPoll(ctx, client, coll, cfg.ResponseTimeout, responseCh); err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

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

// runPoll sends one cd=1 poll and waits for a response or timeout.
// Returns a non-nil error only when ctx is cancelled, which signals the caller to stop.
func runPoll(ctx context.Context, client *mqttclient.Client, coll *collector.Collector, timeout time.Duration, responseCh <-chan struct{}) error {
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
		return nil
	}

	select {
	case <-responseCh:
		slog.Debug("poll response received")
	case <-time.After(timeout):
		slog.Warn("poll timed out waiting for device response", "timeout", timeout)
		coll.IncScrapeError()
		coll.MarkDown()
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}
