package esp32bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultRecoveryWaitTimeout = 3 * time.Minute
	defaultRecoveryPollPeriod  = 5 * time.Second
)

type Bridge interface {
	Status(context.Context) (Status, error)
	ConfigureWiFi(context.Context, WiFiConfig) error
}

type SupervisorConfig struct {
	CheckInterval       time.Duration
	MaxRecoveryAttempts int
	WiFi                WiFiConfig
	WaitTimeout         time.Duration
	PollPeriod          time.Duration
	Metrics             *Metrics
}

type Supervisor struct {
	bridge Bridge
	cfg    SupervisorConfig

	triggerCh chan struct{}

	mu                 sync.Mutex
	recoveryAttempts   int
	recoveryEnabled    bool
	recoveryInProgress bool
	manualIntervention bool
}

func NewSupervisor(bridge Bridge, cfg SupervisorConfig) *Supervisor {
	if cfg.WaitTimeout <= 0 {
		cfg.WaitTimeout = defaultRecoveryWaitTimeout
	}
	if cfg.PollPeriod <= 0 {
		cfg.PollPeriod = defaultRecoveryPollPeriod
	}
	return &Supervisor{
		bridge:    bridge,
		cfg:       cfg,
		triggerCh: make(chan struct{}, 1),
	}
}

func (s *Supervisor) TriggerCheck() {
	select {
	case s.triggerCh <- struct{}{}:
	default:
	}
}

func (s *Supervisor) EnableRecovery() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoveryEnabled = true
}

func (s *Supervisor) Run(ctx context.Context) {
	if s.cfg.CheckInterval <= 0 {
		slog.Warn("ESP32 recovery supervisor disabled: invalid check interval", "interval", s.cfg.CheckInterval)
		return
	}

	ticker := time.NewTicker(s.cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.Check(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("ESP32 status check failed", "error", err)
			}
		case <-s.triggerCh:
			if err := s.Check(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("early ESP32 status check failed", "error", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Supervisor) Check(ctx context.Context) error {
	if !s.beginCheck() {
		return nil
	}
	defer s.endCheck()

	status, err := s.bridge.Status(ctx)
	if err != nil {
		s.cfg.Metrics.IncStatusCheckError()
		return err
	}
	s.cfg.Metrics.ObserveStatus(status)
	if status.Healthy() {
		s.resetAttempts()
		slog.Debug("ESP32 bridge reports battery healthy")
		return nil
	}
	if !status.Connected {
		slog.Warn("ESP32 bridge is not connected to battery over BLE")
		return nil
	}
	if status.WiFiConnected && status.MQTTConnected {
		return nil
	}
	if status.WiFiConnected {
		slog.Warn("ESP32 bridge reports battery MQTT disconnected; no automatic remediation configured")
		return nil
	}
	if !s.recoveryAllowed() {
		slog.Info("ESP32 bridge reports battery WiFi disconnected during cold start; waiting for first successful MQTT response before remediation")
		return nil
	}
	if !s.reserveRecoveryAttempt() {
		slog.Error("ESP32 automatic recovery requires human intervention", "max_attempts", s.cfg.MaxRecoveryAttempts)
		return nil
	}
	s.cfg.Metrics.IncRecoveryAttempt()

	attempt := s.currentAttempts()
	slog.Warn("starting ESP32 battery recovery", "attempt", attempt, "max_attempts", s.cfg.MaxRecoveryAttempts, "wifi_connected", status.WiFiConnected, "mqtt_connected", status.MQTTConnected)
	if err := s.recoverWiFi(ctx); err != nil {
		s.cfg.Metrics.IncRecoveryFailure()
		slog.Warn("ESP32 battery recovery failed", "attempt", attempt, "max_attempts", s.cfg.MaxRecoveryAttempts, "error", err)
		if attempt >= s.cfg.MaxRecoveryAttempts {
			s.markManualIntervention()
			slog.Error("ESP32 automatic recovery exhausted attempts; human intervention required", "max_attempts", s.cfg.MaxRecoveryAttempts)
		}
		return err
	}

	s.resetAttempts()
	s.cfg.Metrics.ObserveRecoverySuccess()
	slog.Info("ESP32 battery recovery completed")
	return nil
}

func (s *Supervisor) recoverWiFi(ctx context.Context) error {
	if err := s.bridge.ConfigureWiFi(ctx, s.cfg.WiFi); err != nil {
		return fmt.Errorf("configure wifi: %w", err)
	}
	if _, err := s.waitFor(ctx, func(status Status) bool {
		return status.Connected && status.WiFiConnected
	}); err != nil {
		return fmt.Errorf("wait for wifi connection: %w", err)
	}
	return nil
}

func (s *Supervisor) waitFor(ctx context.Context, ready func(Status) bool) (Status, error) {
	deadline := time.NewTimer(s.cfg.WaitTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(s.cfg.PollPeriod)
	defer ticker.Stop()

	for {
		status, err := s.bridge.Status(ctx)
		if err != nil {
			s.cfg.Metrics.IncStatusCheckError()
		} else {
			s.cfg.Metrics.ObserveStatus(status)
		}
		if err == nil && ready(status) {
			return status, nil
		}

		select {
		case <-ticker.C:
		case <-deadline.C:
			if err != nil {
				return Status{}, err
			}
			return status, fmt.Errorf("timed out after %s", s.cfg.WaitTimeout)
		case <-ctx.Done():
			return Status{}, ctx.Err()
		}
	}
}

func (s *Supervisor) beginCheck() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoveryInProgress {
		return false
	}
	s.recoveryInProgress = true
	return true
}

func (s *Supervisor) recoveryAllowed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recoveryEnabled
}

func (s *Supervisor) endCheck() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoveryInProgress = false
}

func (s *Supervisor) reserveRecoveryAttempt() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manualIntervention || s.recoveryAttempts >= s.cfg.MaxRecoveryAttempts {
		s.manualIntervention = true
		s.cfg.Metrics.SetManualInterventionRequired(true)
		return false
	}
	s.recoveryAttempts++
	return true
}

func (s *Supervisor) currentAttempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recoveryAttempts
}

func (s *Supervisor) markManualIntervention() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manualIntervention = true
	s.cfg.Metrics.SetManualInterventionRequired(true)
}

func (s *Supervisor) resetAttempts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoveryAttempts = 0
	s.manualIntervention = false
	s.cfg.Metrics.SetManualInterventionRequired(false)
}
