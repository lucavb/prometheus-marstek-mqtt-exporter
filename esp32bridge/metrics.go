package esp32bridge

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "marstek"

type Metrics struct {
	statusCheckErrorsTotal       prometheus.Counter
	recoveryAttemptsTotal        prometheus.Counter
	recoveryFailuresTotal        prometheus.Counter
	manualInterventionRequired   prometheus.Gauge
	batteryBLEConnected          prometheus.Gauge
	batteryWiFiConnected         prometheus.Gauge
	batteryMQTTConnected         prometheus.Gauge
	lastStatusCheckTimestampUnix prometheus.Gauge
	lastRecoveryTimestampUnix    prometheus.Gauge
}

func NewMetrics(reg prometheus.Registerer, deviceType, deviceID string) *Metrics {
	constLabels := prometheus.Labels{
		"device_type": deviceType,
		"device_id":   deviceID,
	}

	m := &Metrics{
		statusCheckErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        prometheus.BuildFQName(namespace, "esp32", "status_check_errors_total"),
			Help:        "Number of ESP32 bridge status checks that failed",
			ConstLabels: constLabels,
		}),
		recoveryAttemptsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        prometheus.BuildFQName(namespace, "esp32", "recovery_attempts_total"),
			Help:        "Total number of ESP32 bridge recovery attempts started",
			ConstLabels: constLabels,
		}),
		recoveryFailuresTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        prometheus.BuildFQName(namespace, "esp32", "recovery_failures_total"),
			Help:        "Total number of ESP32 bridge recovery attempts that failed",
			ConstLabels: constLabels,
		}),
		manualInterventionRequired: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        prometheus.BuildFQName(namespace, "esp32", "recovery_manual_intervention_required"),
			Help:        "1 if automatic ESP32 recovery is exhausted and human intervention is required, 0 otherwise",
			ConstLabels: constLabels,
		}),
		batteryBLEConnected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        prometheus.BuildFQName(namespace, "esp32", "battery_ble_connected"),
			Help:        "1 if the ESP32 bridge is connected to the battery over BLE, 0 otherwise",
			ConstLabels: constLabels,
		}),
		batteryWiFiConnected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        prometheus.BuildFQName(namespace, "esp32", "battery_wifi_connected"),
			Help:        "1 if the battery reports WiFi connected through the ESP32 bridge, 0 otherwise",
			ConstLabels: constLabels,
		}),
		batteryMQTTConnected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        prometheus.BuildFQName(namespace, "esp32", "battery_mqtt_connected"),
			Help:        "1 if the battery reports MQTT connected through the ESP32 bridge, 0 otherwise",
			ConstLabels: constLabels,
		}),
		lastStatusCheckTimestampUnix: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        prometheus.BuildFQName(namespace, "esp32", "last_status_check_timestamp_seconds"),
			Help:        "Unix timestamp of the last successful ESP32 bridge status check",
			ConstLabels: constLabels,
		}),
		lastRecoveryTimestampUnix: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        prometheus.BuildFQName(namespace, "esp32", "last_recovery_timestamp_seconds"),
			Help:        "Unix timestamp of the last successful ESP32 bridge recovery",
			ConstLabels: constLabels,
		}),
	}

	reg.MustRegister(
		m.statusCheckErrorsTotal,
		m.recoveryAttemptsTotal,
		m.recoveryFailuresTotal,
		m.manualInterventionRequired,
		m.batteryBLEConnected,
		m.batteryWiFiConnected,
		m.batteryMQTTConnected,
		m.lastStatusCheckTimestampUnix,
		m.lastRecoveryTimestampUnix,
	)
	return m
}

func (m *Metrics) ObserveStatus(status Status) {
	if m == nil {
		return
	}
	m.batteryBLEConnected.Set(boolToFloat(status.Connected))
	m.batteryWiFiConnected.Set(boolToFloat(status.WiFiConnected))
	m.batteryMQTTConnected.Set(boolToFloat(status.MQTTConnected))
	m.lastStatusCheckTimestampUnix.Set(float64(time.Now().Unix()))
}

func (m *Metrics) IncStatusCheckError() {
	if m != nil {
		m.statusCheckErrorsTotal.Inc()
	}
}

func (m *Metrics) IncRecoveryAttempt() {
	if m != nil {
		m.recoveryAttemptsTotal.Inc()
	}
}

func (m *Metrics) IncRecoveryFailure() {
	if m != nil {
		m.recoveryFailuresTotal.Inc()
	}
}

func (m *Metrics) SetManualInterventionRequired(required bool) {
	if m != nil {
		m.manualInterventionRequired.Set(boolToFloat(required))
	}
}

func (m *Metrics) ObserveRecoverySuccess() {
	if m != nil {
		m.lastRecoveryTimestampUnix.Set(float64(time.Now().Unix()))
	}
}

func boolToFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
