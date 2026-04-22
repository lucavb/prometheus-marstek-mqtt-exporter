package emulator

import "github.com/prometheus/client_golang/prometheus"

// registerMetrics creates and registers all Prometheus metrics for the
// Emulator on reg, then returns the initialised metric handles.
// Pre-initialises the known endpoint label values so series exist before
// the device first calls in.
func registerMetrics(reg prometheus.Registerer, constLabels prometheus.Labels) (
	reportsTotal *prometheus.CounterVec,
	lastReportTimestamp *prometheus.GaugeVec,
	lastUnknownRequestTimestamp prometheus.Gauge,
	reportPayloadBytes prometheus.Gauge,
	reportDecodeErrors prometheus.Counter,
	deviceInfo *prometheus.GaugeVec,
	cellVoltageMillivolts *prometheus.GaugeVec,
	cellVoltageIndex *prometheus.GaugeVec,
	solarInputVoltage *prometheus.GaugeVec,
	solarInputPower *prometheus.GaugeVec,
	outputVoltage *prometheus.GaugeVec,
	cloudDeviceTimestamp prometheus.Gauge,
	wifiBTStatus prometheus.Gauge,
	solarErrInfoHeaderValue *prometheus.GaugeVec,
	// Named puterrinfo metrics.
	solarErrInfoReportType *prometheus.GaugeVec,
	solarErrInfoSwVersion *prometheus.GaugeVec,
	solarErrInfoField2 *prometheus.GaugeVec,
	solarErrInfoField3 *prometheus.GaugeVec,
	solarErrInfoField4 *prometheus.GaugeVec,
	solarErrInfoField5 *prometheus.GaugeVec,
	solarErrInfoEventTotal *prometheus.CounterVec,
	solarErrInfoLastEventTS *prometheus.GaugeVec,
) {
	reportsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "marstek_cloud_reports_total",
		Help:        "Total number of HTTP requests received by the cloud emulator, by endpoint.",
		ConstLabels: constLabels,
	}, []string{"endpoint"})
	reg.MustRegister(reportsTotal)

	// Pre-initialise all known label values so the series exist even before the
	// device first calls in.
	reportsTotal.WithLabelValues(endpointDateInfo)
	reportsTotal.WithLabelValues(endpointReport)
	reportsTotal.WithLabelValues(endpointSolarErrInfo)
	reportsTotal.WithLabelValues(endpointUnknown)

	lastReportTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_cloud_last_report_timestamp_seconds",
		Help:        "Unix timestamp of the last successful request per cloud endpoint.",
		ConstLabels: constLabels,
	}, []string{"endpoint"})
	reg.MustRegister(lastReportTimestamp)

	lastUnknownRequestTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "marstek_cloud_last_unknown_request_timestamp_seconds",
		Help:        "Unix timestamp of the last request to an unrecognised cloud endpoint. Non-zero means a new firmware endpoint was discovered — check the logs.",
		ConstLabels: constLabels,
	})
	reg.MustRegister(lastUnknownRequestTimestamp)

	reportPayloadBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "marstek_cloud_report_payload_bytes",
		Help:        "Decoded size in bytes of the latest setB2500Report payload. A change may indicate a firmware update.",
		ConstLabels: constLabels,
	})
	reg.MustRegister(reportPayloadBytes)

	reportDecodeErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        "marstek_cloud_report_decode_errors_total",
		Help:        "Total number of setB2500Report payloads that could not be decrypted or parsed. A non-zero value may indicate a firmware key rotation.",
		ConstLabels: constLabels,
	})
	reg.MustRegister(reportDecodeErrors)

	// marstek_device_info follows the Prometheus info-metric convention: value
	// is always 1 and the interesting data lives in the label set.
	deviceInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_device_info",
		Help:        "Device metadata parsed from the cloud time-sync request. Value is always 1; use label values for joins/alerts.",
		ConstLabels: constLabels,
	}, []string{"uid", "device_type_reported", "firmware_version", "sw_version", "sub_version", "mod_version"})
	reg.MustRegister(deviceInfo)

	cellVoltageMillivolts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_cell_voltage_millivolts",
		Help:        "Per-pack min/max cell voltage in millivolts, from the cloud telemetry report.",
		ConstLabels: constLabels,
	}, []string{"pack", "bound"})
	reg.MustRegister(cellVoltageMillivolts)

	cellVoltageIndex = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_cell_voltage_cell_index",
		Help:        "Index of the min/max voltage cell within each pack, from the cloud telemetry report.",
		ConstLabels: constLabels,
	}, []string{"pack", "bound"})
	reg.MustRegister(cellVoltageIndex)

	solarInputVoltage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_solar_input_voltage_millivolts",
		Help:        "Per-solar-input voltage in millivolts, from the cloud telemetry report.",
		ConstLabels: constLabels,
	}, []string{"input"})
	reg.MustRegister(solarInputVoltage)

	solarInputPower = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_solar_input_power_watts",
		Help:        "Per-solar-input power in watts, from the cloud telemetry report (pv1/pv2 fields). The sum equals the aggregate pv field.",
		ConstLabels: constLabels,
	}, []string{"input"})
	reg.MustRegister(solarInputPower)

	outputVoltage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_output_voltage_millivolts",
		Help:        "Per-output-port voltage in millivolts, from the cloud telemetry report.",
		ConstLabels: constLabels,
	}, []string{"output"})
	reg.MustRegister(outputVoltage)

	cloudDeviceTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "marstek_cloud_device_timestamp_seconds",
		Help:        "Device self-reported local time as a Unix timestamp, from the cloud telemetry report. Use to detect clock drift.",
		ConstLabels: constLabels,
	})
	reg.MustRegister(cloudDeviceTimestamp)

	wifiBTStatus = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "marstek_wifi_bt_status",
		Help:        "Raw wbs field from the cloud telemetry report, indicating Wi-Fi/Bluetooth connectivity state.",
		ConstLabels: constLabels,
	})
	reg.MustRegister(wifiBTStatus)

	// Positional header gauge — kept for backward compatibility and to surface
	// unexpected field additions from future firmware versions automatically.
	solarErrInfoHeaderValue = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_cloud_solar_errinfo_header_value",
		Help:        "Integer values from the puterrinfo request header, keyed by zero-based positional index. index=0 is report_type; index=1 is sw_version; index=2..5 are status/flag fields. See emulator/solar_errinfo.go for full field map.",
		ConstLabels: constLabels,
	}, []string{"index"})
	reg.MustRegister(solarErrInfoHeaderValue)

	// Named header gauges — derived from Ghidra analysis of puterrinfo_state_machine.
	solarErrInfoReportType = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_solar_errinfo_report_type",
		Help:        "Report type from the puterrinfo header: 0=battery slot 0 (triples), 1=slot 1 (quintuples), 2=slot 2 (quintuples).",
		ConstLabels: constLabels,
	}, []string{"uid", "battery"})
	reg.MustRegister(solarErrInfoReportType)

	solarErrInfoSwVersion = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_solar_errinfo_sw_version",
		Help:        "Firmware software version number from the puterrinfo header (header[1]).",
		ConstLabels: constLabels,
	}, []string{"uid", "battery"})
	reg.MustRegister(solarErrInfoSwVersion)

	solarErrInfoField2 = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_solar_errinfo_field2",
		Help:        "puterrinfo header field 2 (header[2]); likely SoC % or a battery voltage field. Exact semantics TBC against a live capture.",
		ConstLabels: constLabels,
	}, []string{"uid", "battery"})
	reg.MustRegister(solarErrInfoField2)

	solarErrInfoField3 = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_solar_errinfo_field3",
		Help:        "puterrinfo header field 3 (header[3]); status flags byte at battery_state+0x4d/0xde/0x16f.",
		ConstLabels: constLabels,
	}, []string{"uid", "battery"})
	reg.MustRegister(solarErrInfoField3)

	solarErrInfoField4 = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_solar_errinfo_field4",
		Help:        "puterrinfo header field 4 (header[4]); status flags byte at battery_state+0x4e/0xdf/0x170.",
		ConstLabels: constLabels,
	}, []string{"uid", "battery"})
	reg.MustRegister(solarErrInfoField4)

	solarErrInfoField5 = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_solar_errinfo_field5",
		Help:        "puterrinfo header field 5 (header[5]); status flags byte at battery_state+0x4f/0xe0/0x171.",
		ConstLabels: constLabels,
	}, []string{"uid", "battery"})
	reg.MustRegister(solarErrInfoField5)

	// Per-event counters and timestamps — labelled with the human-readable code
	// name so Grafana panels are self-documenting.
	solarErrInfoEventTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "marstek_solar_errinfo_event_total",
		Help:        "Total puterrinfo events received, labelled by uid, battery slot, numeric code, and human-readable name from the Ghidra-derived dictionary.",
		ConstLabels: constLabels,
	}, []string{"uid", "battery", "code", "name"})
	reg.MustRegister(solarErrInfoEventTotal)

	solarErrInfoLastEventTS = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "marstek_solar_errinfo_last_event_ts_seconds",
		Help:        "Unix timestamp of the most recently received puterrinfo event for each (uid, battery, code, name) combination.",
		ConstLabels: constLabels,
	}, []string{"uid", "battery", "code", "name"})
	reg.MustRegister(solarErrInfoLastEventTS)

	return
}
