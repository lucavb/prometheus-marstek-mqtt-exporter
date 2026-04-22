package emulator

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Emulator emulates the eu.hamedata.com cloud server that Marstek battery
// devices contact for time-sync and telemetry reporting. It exposes Prometheus
// metrics derived from the intercepted request traffic.
type Emulator struct {
	tz *time.Location

	mu                    sync.Mutex
	lastDeviceInfo        deviceInfoLabels
	unknownRateMap        map[string]time.Time // path → time of last warn log
	lastErrInfoHeaderLen  int                  // length of the header slice from the previous puterrinfo upload

	// metrics
	reportsTotal                *prometheus.CounterVec
	lastReportTimestamp         *prometheus.GaugeVec
	lastUnknownRequestTimestamp prometheus.Gauge
	reportPayloadBytes          prometheus.Gauge
	reportDecodeErrors          prometheus.Counter
	deviceInfo                  *prometheus.GaugeVec

	// cloud-report-only metrics (not available via MQTT cd=1)
	cellVoltageMillivolts    *prometheus.GaugeVec // b{n}max/min
	cellVoltageIndex         *prometheus.GaugeVec // b{n}maxn/minn
	solarInputVoltage        *prometheus.GaugeVec // pv1v/pv2v
	solarInputPower          *prometheus.GaugeVec // pv1/pv2 (watts)
	outputVoltage            *prometheus.GaugeVec // out1v/out2v
	cloudDeviceTimestamp     prometheus.Gauge
	wifiBTStatus             prometheus.Gauge
	solarErrInfoHeaderValue  *prometheus.GaugeVec // puterrinfo header integers by positional index
}

type deviceInfoLabels struct {
	uid                string
	deviceTypeReported string
	firmwareVersion    string
	swVersion          string
	subVersion         string
	modVersion         string
}

// New creates an Emulator and registers its metrics on reg.
// deviceType and deviceID are used as const labels matching the rest of the
// exporter so all metrics land in the same label namespace.
func New(reg prometheus.Registerer, deviceType, deviceID string, tz *time.Location) *Emulator {
	constLabels := prometheus.Labels{
		"device_type": deviceType,
		"device_id":   deviceID,
	}

	reportsTotal, lastReportTimestamp, lastUnknownRequestTimestamp,
		reportPayloadBytes, reportDecodeErrors, deviceInfo,
		cellVoltageMillivolts, cellVoltageIndex, solarInputVoltage, solarInputPower,
		outputVoltage, cloudDeviceTimestamp, wifiBTStatus,
		solarErrInfoHeaderValue := registerMetrics(reg, constLabels)

	return &Emulator{
		tz:                          tz,
		unknownRateMap:              make(map[string]time.Time),
		reportsTotal:                reportsTotal,
		lastReportTimestamp:         lastReportTimestamp,
		lastUnknownRequestTimestamp: lastUnknownRequestTimestamp,
		reportPayloadBytes:          reportPayloadBytes,
		reportDecodeErrors:          reportDecodeErrors,
		deviceInfo:                  deviceInfo,
		cellVoltageMillivolts:       cellVoltageMillivolts,
		cellVoltageIndex:            cellVoltageIndex,
		solarInputVoltage:           solarInputVoltage,
		solarInputPower:             solarInputPower,
		outputVoltage:               outputVoltage,
		cloudDeviceTimestamp:        cloudDeviceTimestamp,
		wifiBTStatus:                wifiBTStatus,
		solarErrInfoHeaderValue:     solarErrInfoHeaderValue,
	}
}

// Handler returns the http.Handler for the emulated cloud server.
func (e *Emulator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(pathDateInfo, e.handleDateInfo)
	mux.HandleFunc(pathReport, e.handleReport)
	mux.HandleFunc(pathSolarErrInfo, e.handleSolarErrInfo)
	mux.HandleFunc("/", e.handleUnknown)
	return mux
}
