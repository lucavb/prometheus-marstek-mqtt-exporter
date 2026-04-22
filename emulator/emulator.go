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

	mu                   sync.Mutex
	lastDeviceInfo       deviceInfoLabels
	unknownRateMap       map[string]time.Time // path → time of last warn log
	lastErrInfoHeaderLen int                  // length of the header slice from the previous puterrinfo upload

	// metrics
	reportsTotal                *prometheus.CounterVec
	lastReportTimestamp         *prometheus.GaugeVec
	lastUnknownRequestTimestamp prometheus.Gauge
	reportPayloadBytes          prometheus.Gauge
	reportDecodeErrors          prometheus.Counter
	deviceInfo                  *prometheus.GaugeVec

	// cloud-report-only metrics (not available via MQTT cd=1)
	cellVoltageMillivolts  *prometheus.GaugeVec // b{n}max/min
	cellVoltageIndex       *prometheus.GaugeVec // b{n}maxn/minn
	solarInputVoltage      *prometheus.GaugeVec // pv1v/pv2v
	solarInputPower        *prometheus.GaugeVec // pv1/pv2 (watts)
	outputVoltage          *prometheus.GaugeVec // out1v/out2v
	cloudDeviceTimestamp   prometheus.Gauge
	wifiBTStatus           prometheus.Gauge
	cloudBatteryPackSoC    *prometheus.GaugeVec // pe0/pe1/pe2
	batteryPackChargeDirection *prometheus.GaugeVec // b0f/b1f/b2f
	cloudReportSequence       prometheus.Gauge     // tn
	cellVoltageSpread         *prometheus.GaugeVec // b{n}max - b{n}min

	// puterrinfo positional header — kept for backward compat and firmware-drift detection
	solarErrInfoHeaderValue *prometheus.GaugeVec

	// puterrinfo named header gauges (Ghidra-derived field names)
	solarErrInfoReportType *prometheus.GaugeVec
	solarErrInfoSwVersion  *prometheus.GaugeVec
	solarErrInfoField2     *prometheus.GaugeVec
	solarErrInfoField3     *prometheus.GaugeVec
	solarErrInfoField4     *prometheus.GaugeVec
	solarErrInfoField5     *prometheus.GaugeVec

	// puterrinfo per-event metrics labelled with human-readable code names
	solarErrInfoEventTotal  *prometheus.CounterVec
	solarErrInfoLastEventTS *prometheus.GaugeVec
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
		cloudBatteryPackSoC, batteryPackChargeDirection, cloudReportSequence,
		cellVoltageSpread,
		solarErrInfoHeaderValue,
		solarErrInfoReportType, solarErrInfoSwVersion,
		solarErrInfoField2, solarErrInfoField3, solarErrInfoField4, solarErrInfoField5,
		solarErrInfoEventTotal, solarErrInfoLastEventTS := registerMetrics(reg, constLabels)

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
		cloudBatteryPackSoC:         cloudBatteryPackSoC,
		batteryPackChargeDirection:  batteryPackChargeDirection,
		cloudReportSequence:        cloudReportSequence,
		cellVoltageSpread:          cellVoltageSpread,
		solarErrInfoHeaderValue:     solarErrInfoHeaderValue,
		solarErrInfoReportType:      solarErrInfoReportType,
		solarErrInfoSwVersion:       solarErrInfoSwVersion,
		solarErrInfoField2:          solarErrInfoField2,
		solarErrInfoField3:          solarErrInfoField3,
		solarErrInfoField4:          solarErrInfoField4,
		solarErrInfoField5:          solarErrInfoField5,
		solarErrInfoEventTotal:      solarErrInfoEventTotal,
		solarErrInfoLastEventTS:     solarErrInfoLastEventTS,
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
