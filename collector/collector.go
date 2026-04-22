package collector

import (
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "marstek"

// cachedMetric holds a single pre-built const metric ready to send to Prometheus.
type cachedMetric struct {
	desc        *prometheus.Desc
	value       float64
	labelValues []string // nil for scalar gauges
}

// Collector implements prometheus.Collector. It caches the last device payload
// and emits gauge metrics only while the cache is fresh (age <= ttl). The meta
// metrics (up, last_update_timestamp_seconds) and the poll counters are always
// emitted regardless of freshness.
//
// This follows the Prometheus "Writing Exporters" guidance:
//   - Use MustNewConstMetric in Collect() so stale label values are not exported.
//   - Expire cached push-source values after a configurable TTL.
type Collector struct {
	mu    sync.RWMutex
	ttl   time.Duration
	nowFn func() time.Time // injectable for testing; defaults to time.Now

	// Descriptors owned by this collector (sent in Describe).
	// Counter descs are NOT listed here — those belong to the separately
	// registered prometheus.Counter instances below.
	upDesc         *prometheus.Desc
	lastUpdateDesc *prometheus.Desc
	allGaugeDescs  []*prometheus.Desc

	// Lookup map used inside Update() to resolve desc by metric name.
	// Key is the metric name suffix (e.g. "battery_soc_percent").
	gaugeDescMap map[string]*prometheus.Desc

	// Always-live state — emitted on every Collect regardless of freshness.
	up              float64
	lastSuccessUnix float64

	// Device value cache — rebuilt wholesale on every Update().
	cached       []cachedMetric
	lastUpdateAt time.Time

	// Direct-instrumentation counters (separately registered; always accumulate).
	scrapesTotal      prometheus.Counter
	scrapeErrorsTotal prometheus.Counter
}

func New(reg prometheus.Registerer, deviceType, deviceID string, ttl time.Duration) *Collector {
	constLabels := prometheus.Labels{
		"device_type": deviceType,
		"device_id":   deviceID,
	}

	newDesc := func(name, help string, variableLabels ...string) *prometheus.Desc {
		return prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", name),
			help,
			variableLabels,
			constLabels,
		)
	}

	upDesc := newDesc("up", "1 if the last poll received a response, 0 otherwise")
	lastUpdateDesc := newDesc("last_update_timestamp_seconds", "Unix timestamp of the last successful device update")

	type namedDesc struct {
		name string
		desc *prometheus.Desc
	}

	named := []namedDesc{
		{"battery_soc_percent", newDesc("battery_soc_percent", "State of charge in percent")},
		{"battery_remaining_wh", newDesc("battery_remaining_wh", "Remaining battery capacity in Wh")},
		{"battery_dod_percent", newDesc("battery_dod_percent", "Depth of discharge setting in percent")},
		{"output_threshold_watts", newDesc("output_threshold_watts", "Minimum load to engage output in watts")},
		{"daily_battery_charge_wh", newDesc("daily_battery_charge_wh", "Daily battery charge energy in Wh (resets at midnight)")},
		{"daily_battery_discharge_wh", newDesc("daily_battery_discharge_wh", "Daily battery discharge energy in Wh (resets at midnight)")},
		{"daily_load_charge_wh", newDesc("daily_load_charge_wh", "Daily load charge energy in Wh")},
		{"daily_load_discharge_wh", newDesc("daily_load_discharge_wh", "Daily load discharge energy in Wh")},
		{"rated_output_watts", newDesc("rated_output_watts", "Rated output power in watts")},
		{"rated_input_watts", newDesc("rated_input_watts", "Rated input power in watts")},
		{"surplus_feed_in_enabled", newDesc("surplus_feed_in_enabled", "1 if surplus feed-in is enabled, 0 otherwise")},
		{"solar_input_watts", newDesc("solar_input_watts", "Solar input power in watts", "input")},
		{"output_watts", newDesc("output_watts", "Output power in watts", "output")},
		{"output_enabled", newDesc("output_enabled", "Output enabled state (1=on, 0=off)", "output")},
		{"temperature_celsius", newDesc("temperature_celsius", "Device temperature in Celsius", "sensor")},
		{"extra_pack_connected", newDesc("extra_pack_connected", "Extra battery pack connected (1=yes, 0=no)", "pack")},
		// Per-pack state of charge derived from the cd=0 telemetry. Pack 0 comes
		// from the aggregate `pe` field (identical to `a0` in every capture we
		// have, hence a0 is not exposed separately); packs 1–2 come from the
		// `a1`/`a2` channels. On a single-pack device, packs 1 and 2 report 0.
		{"battery_pack_soc_percent", newDesc("battery_pack_soc_percent", "Per-pack state of charge in percent. pack=0 is the aggregate pe field; pack=1 and pack=2 are the a1/a2 channels.", "pack")},
		// m0..m3 are "per-pack metric" fields in cd=0. The semantics have not
		// been fully decoded yet — m3 tracks lv (load watts) in the captures
		// but the roles of m0/m1/m2 are unverified. Exposing them as raw
		// numbers is still useful for anomaly detection and dashboarding;
		// a future phase will split them once we know what they mean.
		{"mqtt_m_channel", newDesc("mqtt_m_channel", "Raw m0..m3 channels from the cd=0 MQTT telemetry. Semantics are partially decoded: m3 tracks the load watts (lv) in captured traffic; m0..m2 are unverified per-pack metrics. Use for anomaly detection until fully decoded.", "channel")},
	}

	gaugeDescs := make([]*prometheus.Desc, len(named))
	gaugeDescMap := make(map[string]*prometheus.Desc, len(named))
	for i, n := range named {
		gaugeDescs[i] = n.desc
		gaugeDescMap[n.name] = n.desc
	}

	scrapesTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name:        prometheus.BuildFQName(namespace, "", "scrapes_total"),
		Help:        "Total number of cd=1 polls sent to the device",
		ConstLabels: constLabels,
	})
	scrapeErrorsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name:        prometheus.BuildFQName(namespace, "", "scrape_errors_total"),
		Help:        "Number of polls that received no response within the timeout",
		ConstLabels: constLabels,
	})

	c := &Collector{
		ttl:               ttl,
		nowFn:             time.Now,
		upDesc:            upDesc,
		lastUpdateDesc:    lastUpdateDesc,
		allGaugeDescs:     gaugeDescs,
		gaugeDescMap:      gaugeDescMap,
		scrapesTotal:      scrapesTotal,
		scrapeErrorsTotal: scrapeErrorsTotal,
	}

	// Register the custom collector and the two direct-instrumentation counters.
	reg.MustRegister(c, scrapesTotal, scrapeErrorsTotal)
	return c
}

// Describe implements prometheus.Collector.
// We enumerate descriptors explicitly rather than using DescribeByCollect because
// Collect conditionally omits stale gauge metrics — DescribeByCollect is only
// correct when Collect always emits the same set.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.upDesc
	ch <- c.lastUpdateDesc
	for _, d := range c.allGaugeDescs {
		ch <- d
	}
}

// Collect implements prometheus.Collector. It is safe for concurrent use.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	fresh := !c.lastUpdateAt.IsZero() && c.nowFn().Sub(c.lastUpdateAt) <= c.ttl

	// Always emit meta metrics regardless of device state.
	ch <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, c.up)
	ch <- prometheus.MustNewConstMetric(c.lastUpdateDesc, prometheus.GaugeValue, c.lastSuccessUnix)

	if !fresh {
		return
	}
	for _, m := range c.cached {
		ch <- prometheus.MustNewConstMetric(m.desc, prometheus.GaugeValue, m.value, m.labelValues...)
	}
}

// Update parses a device payload and rebuilds the metric cache. Safe for concurrent use.
func (c *Collector) Update(payload string) {
	m := Parse(payload)
	if len(m) == 0 {
		slog.Warn("received empty or unparseable payload", "payload", payload)
		return
	}

	var metrics []cachedMetric

	add := func(name string, v float64, labels ...string) {
		d, ok := c.gaugeDescMap[name]
		if !ok {
			return
		}
		metrics = append(metrics, cachedMetric{desc: d, value: v, labelValues: labels})
	}

	setIfPresent := func(name, key string) {
		if v, ok := floatVal(m, key); ok {
			add(name, v)
		}
	}

	setIfPresent("battery_soc_percent", "pe")
	setIfPresent("battery_remaining_wh", "kn")
	setIfPresent("battery_dod_percent", "do")
	setIfPresent("output_threshold_watts", "lv")
	setIfPresent("daily_battery_charge_wh", "bc")
	setIfPresent("daily_battery_discharge_wh", "bs")
	setIfPresent("daily_load_charge_wh", "pt")
	setIfPresent("daily_load_discharge_wh", "it")
	setIfPresent("rated_output_watts", "lmo")
	setIfPresent("rated_input_watts", "lmi")

	// tc_dis=0 means surplus feed-in is enabled (inverted flag in device protocol).
	if v, ok := intVal(m, "tc_dis"); ok {
		if v == 0 {
			add("surplus_feed_in_enabled", 1)
		} else {
			add("surplus_feed_in_enabled", 0)
		}
	}

	if v, ok := floatVal(m, "w1"); ok {
		add("solar_input_watts", v, "1")
	}
	if v, ok := floatVal(m, "w2"); ok {
		add("solar_input_watts", v, "2")
	}

	if v, ok := floatVal(m, "g1"); ok {
		add("output_watts", v, "1")
	}
	if v, ok := floatVal(m, "g2"); ok {
		add("output_watts", v, "2")
	}

	if v, ok := floatVal(m, "o1"); ok {
		add("output_enabled", v, "1")
	}
	if v, ok := floatVal(m, "o2"); ok {
		add("output_enabled", v, "2")
	}

	if v, ok := floatVal(m, "tl"); ok {
		add("temperature_celsius", v, "min")
	}
	if v, ok := floatVal(m, "th"); ok {
		add("temperature_celsius", v, "max")
	}

	if v, ok := floatVal(m, "b1"); ok {
		add("extra_pack_connected", v, "1")
	}
	if v, ok := floatVal(m, "b2"); ok {
		add("extra_pack_connected", v, "2")
	}

	if v, ok := floatVal(m, "pe"); ok {
		add("battery_pack_soc_percent", v, "0")
	}
	if v, ok := floatVal(m, "a1"); ok {
		add("battery_pack_soc_percent", v, "1")
	}
	if v, ok := floatVal(m, "a2"); ok {
		add("battery_pack_soc_percent", v, "2")
	}

	for _, ch := range []string{"0", "1", "2", "3"} {
		if v, ok := floatVal(m, "m"+ch); ok {
			add("mqtt_m_channel", v, ch)
		}
	}

	c.mu.Lock()
	c.cached = metrics
	c.lastUpdateAt = c.nowFn()
	c.mu.Unlock()

	slog.Debug("metrics updated from payload", "fields_parsed", len(m))
}

// MarkUp records a successful device response.
func (c *Collector) MarkUp() {
	c.mu.Lock()
	c.up = 1
	c.lastSuccessUnix = float64(c.nowFn().Unix())
	c.mu.Unlock()
}

// MarkDown records a failed device response.
func (c *Collector) MarkDown() {
	c.mu.Lock()
	c.up = 0
	c.mu.Unlock()
}

func (c *Collector) IncScrape() {
	c.scrapesTotal.Inc()
}

func (c *Collector) IncScrapeError() {
	c.scrapeErrorsTotal.Inc()
}
