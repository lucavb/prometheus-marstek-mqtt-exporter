package collector

import (
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "marstek"

type Collector struct {
	mu sync.Mutex

	batterySoC           prometheus.Gauge
	batteryRemainingWh   prometheus.Gauge
	batteryDoDPercent    prometheus.Gauge
	outputThresholdWatts prometheus.Gauge
	dailyBatteryChargeWh prometheus.Gauge
	dailyBatteryDischWh  prometheus.Gauge
	dailyLoadChargeWh    prometheus.Gauge
	dailyLoadDischWh     prometheus.Gauge
	ratedOutputWatts     prometheus.Gauge
	ratedInputWatts      prometheus.Gauge
	surplusFeedInEnabled prometheus.Gauge
	up                   prometheus.Gauge
	lastUpdateTimestamp  prometheus.Gauge

	solarInputWatts    *prometheus.GaugeVec
	outputWatts        *prometheus.GaugeVec
	outputEnabled      *prometheus.GaugeVec
	temperatureCelsius *prometheus.GaugeVec
	extraPackConnected *prometheus.GaugeVec

	scrapesTotal      prometheus.Counter
	scrapeErrorsTotal prometheus.Counter
}

func New(reg prometheus.Registerer, deviceType, deviceID string) *Collector {
	constLabels := prometheus.Labels{
		"device_type": deviceType,
		"device_id":   deviceID,
	}

	newGauge := func(name, help string) prometheus.Gauge {
		g := prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        prometheus.BuildFQName(namespace, "", name),
			Help:        help,
			ConstLabels: constLabels,
		})
		reg.MustRegister(g)
		return g
	}

	newGaugeVec := func(name, help string, labelNames []string) *prometheus.GaugeVec {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name:        prometheus.BuildFQName(namespace, "", name),
			Help:        help,
			ConstLabels: constLabels,
		}, labelNames)
		reg.MustRegister(g)
		return g
	}

	newCounter := func(name, help string) prometheus.Counter {
		c := prometheus.NewCounter(prometheus.CounterOpts{
			Name:        prometheus.BuildFQName(namespace, "", name),
			Help:        help,
			ConstLabels: constLabels,
		})
		reg.MustRegister(c)
		return c
	}

	return &Collector{
		batterySoC:           newGauge("battery_soc_percent", "State of charge in percent"),
		batteryRemainingWh:   newGauge("battery_remaining_wh", "Remaining battery capacity in Wh"),
		batteryDoDPercent:    newGauge("battery_dod_percent", "Depth of discharge setting in percent"),
		outputThresholdWatts: newGauge("output_threshold_watts", "Minimum load to engage output in watts"),
		dailyBatteryChargeWh: newGauge("daily_battery_charge_wh", "Daily battery charge energy in Wh (resets at midnight)"),
		dailyBatteryDischWh:  newGauge("daily_battery_discharge_wh", "Daily battery discharge energy in Wh (resets at midnight)"),
		dailyLoadChargeWh:    newGauge("daily_load_charge_wh", "Daily load charge energy in Wh"),
		dailyLoadDischWh:     newGauge("daily_load_discharge_wh", "Daily load discharge energy in Wh"),
		ratedOutputWatts:     newGauge("rated_output_watts", "Rated output power in watts"),
		ratedInputWatts:      newGauge("rated_input_watts", "Rated input power in watts"),
		surplusFeedInEnabled: newGauge("surplus_feed_in_enabled", "1 if surplus feed-in is enabled, 0 otherwise"),
		up:                   newGauge("up", "1 if the last poll received a response, 0 otherwise"),
		lastUpdateTimestamp:  newGauge("last_update_timestamp_seconds", "Unix timestamp of the last successful device update"),

		solarInputWatts:    newGaugeVec("solar_input_watts", "Solar input power in watts", []string{"input"}),
		outputWatts:        newGaugeVec("output_watts", "Output power in watts", []string{"output"}),
		outputEnabled:      newGaugeVec("output_enabled", "Output enabled state (1=on, 0=off)", []string{"output"}),
		temperatureCelsius: newGaugeVec("temperature_celsius", "Device temperature in Celsius", []string{"sensor"}),
		extraPackConnected: newGaugeVec("extra_pack_connected", "Extra battery pack connected (1=yes, 0=no)", []string{"pack"}),

		scrapesTotal:      newCounter("scrapes_total", "Total number of cd=1 polls sent to the device"),
		scrapeErrorsTotal: newCounter("scrape_errors_total", "Number of polls that received no response within the timeout"),
	}
}

// Update is safe to call concurrently; the mutex serializes gauge writes.
func (c *Collector) Update(payload string) {
	m := Parse(payload)
	if len(m) == 0 {
		slog.Warn("received empty or unparseable payload", "payload", payload)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	setIfPresent := func(g prometheus.Gauge, key string) {
		if v, ok := floatVal(m, key); ok {
			g.Set(v)
		}
	}

	setIfPresent(c.batterySoC, "pe")
	setIfPresent(c.batteryRemainingWh, "kn")
	setIfPresent(c.batteryDoDPercent, "do")
	setIfPresent(c.outputThresholdWatts, "lv")
	setIfPresent(c.dailyBatteryChargeWh, "bc")
	setIfPresent(c.dailyBatteryDischWh, "bs")
	setIfPresent(c.dailyLoadChargeWh, "pt")
	setIfPresent(c.dailyLoadDischWh, "it")
	setIfPresent(c.ratedOutputWatts, "lmo")
	setIfPresent(c.ratedInputWatts, "lmi")

	// tc_dis=0 means surplus feed-in is enabled (inverted flag in device protocol).
	if v, ok := intVal(m, "tc_dis"); ok {
		if v == 0 {
			c.surplusFeedInEnabled.Set(1)
		} else {
			c.surplusFeedInEnabled.Set(0)
		}
	}

	if v, ok := floatVal(m, "w1"); ok {
		c.solarInputWatts.WithLabelValues("1").Set(v)
	}
	if v, ok := floatVal(m, "w2"); ok {
		c.solarInputWatts.WithLabelValues("2").Set(v)
	}

	if v, ok := floatVal(m, "g1"); ok {
		c.outputWatts.WithLabelValues("1").Set(v)
	}
	if v, ok := floatVal(m, "g2"); ok {
		c.outputWatts.WithLabelValues("2").Set(v)
	}
	if v, ok := floatVal(m, "o1"); ok {
		c.outputEnabled.WithLabelValues("1").Set(v)
	}
	if v, ok := floatVal(m, "o2"); ok {
		c.outputEnabled.WithLabelValues("2").Set(v)
	}

	if v, ok := floatVal(m, "tl"); ok {
		c.temperatureCelsius.WithLabelValues("min").Set(v)
	}
	if v, ok := floatVal(m, "th"); ok {
		c.temperatureCelsius.WithLabelValues("max").Set(v)
	}

	if v, ok := floatVal(m, "b1"); ok {
		c.extraPackConnected.WithLabelValues("1").Set(v)
	}
	if v, ok := floatVal(m, "b2"); ok {
		c.extraPackConnected.WithLabelValues("2").Set(v)
	}

	slog.Debug("metrics updated from payload", "fields_parsed", len(m))
}

func (c *Collector) MarkUp() {
	c.up.Set(1)
	c.lastUpdateTimestamp.Set(float64(time.Now().Unix()))
}

func (c *Collector) MarkDown() {
	c.up.Set(0)
}

func (c *Collector) IncScrape() {
	c.scrapesTotal.Inc()
}

func (c *Collector) IncScrapeError() {
	c.scrapeErrorsTotal.Inc()
}
