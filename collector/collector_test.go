package collector

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const samplePayload = "pe=75,kn=1500,do=20,lv=5,bc=200,bs=150,pt=100,it=50,lmo=800,lmi=600,tc_dis=0,w1=120,w2=80,g1=200,g2=150,o1=1,o2=1,tl=25,th=35,b1=1,b2=0"

func newTestCollector(ttl time.Duration, now func() time.Time) (*Collector, *prometheus.Registry) {
	reg := prometheus.NewRegistry()
	c := New(reg, "HMJ-2", "test-device", ttl)
	c.nowFn = now
	return c, reg
}

func TestFreshMetricsEmitted(t *testing.T) {
	fixedNow := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c, reg := newTestCollector(5*time.Minute, func() time.Time { return fixedNow })

	c.Update(samplePayload)
	c.MarkUp()

	expected := `
# HELP marstek_battery_soc_percent State of charge in percent
# TYPE marstek_battery_soc_percent gauge
marstek_battery_soc_percent{device_id="test-device",device_type="HMJ-2"} 75
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_battery_soc_percent"); err != nil {
		t.Errorf("fresh gauge not emitted correctly: %v", err)
	}

	expected = `
# HELP marstek_solar_input_watts Solar input power in watts
# TYPE marstek_solar_input_watts gauge
marstek_solar_input_watts{device_id="test-device",device_type="HMJ-2",input="1"} 120
marstek_solar_input_watts{device_id="test-device",device_type="HMJ-2",input="2"} 80
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_solar_input_watts"); err != nil {
		t.Errorf("fresh vec gauge not emitted correctly: %v", err)
	}

	expected = `
# HELP marstek_surplus_feed_in_enabled 1 if surplus feed-in is enabled, 0 otherwise
# TYPE marstek_surplus_feed_in_enabled gauge
marstek_surplus_feed_in_enabled{device_id="test-device",device_type="HMJ-2"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_surplus_feed_in_enabled"); err != nil {
		t.Errorf("surplus_feed_in_enabled inversion not applied correctly: %v", err)
	}
}

func TestStaleMetricsOmitted(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base
	c, reg := newTestCollector(5*time.Minute, func() time.Time { return now })

	c.Update(samplePayload)
	c.MarkUp()

	// Advance clock past TTL.
	now = base.Add(6 * time.Minute)

	// Gauge should be gone.
	gathered, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}
	for _, mf := range gathered {
		name := mf.GetName()
		if name == "marstek_battery_soc_percent" {
			t.Errorf("expected marstek_battery_soc_percent to be absent after TTL expiry, but it was present")
		}
		if name == "marstek_solar_input_watts" {
			t.Errorf("expected marstek_solar_input_watts to be absent after TTL expiry, but it was present")
		}
	}

	// Meta metrics must still be present.
	expected := `
# HELP marstek_up 1 if the last poll received a response, 0 otherwise
# TYPE marstek_up gauge
marstek_up{device_id="test-device",device_type="HMJ-2"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_up"); err != nil {
		t.Errorf("marstek_up should still be emitted after TTL expiry: %v", err)
	}

	found := false
	for _, mf := range gathered {
		if mf.GetName() == "marstek_last_update_timestamp_seconds" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("marstek_last_update_timestamp_seconds should still be emitted after TTL expiry")
	}
}

func TestMetaMetricsAlwaysPresent(t *testing.T) {
	fixedNow := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	_, reg := newTestCollector(5*time.Minute, func() time.Time { return fixedNow })

	// No Update or MarkUp called — cold start.

	expected := `
# HELP marstek_up 1 if the last poll received a response, 0 otherwise
# TYPE marstek_up gauge
marstek_up{device_id="test-device",device_type="HMJ-2"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_up"); err != nil {
		t.Errorf("marstek_up should be present at cold start with value 0: %v", err)
	}

	expected = `
# HELP marstek_scrapes_total Total number of cd=1 polls sent to the device
# TYPE marstek_scrapes_total counter
marstek_scrapes_total{device_id="test-device",device_type="HMJ-2"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_scrapes_total"); err != nil {
		t.Errorf("marstek_scrapes_total should be present at cold start: %v", err)
	}

	expected = `
# HELP marstek_scrape_errors_total Number of polls that received no response within the timeout
# TYPE marstek_scrape_errors_total counter
marstek_scrape_errors_total{device_id="test-device",device_type="HMJ-2"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_scrape_errors_total"); err != nil {
		t.Errorf("marstek_scrape_errors_total should be present at cold start: %v", err)
	}

	// No device gauge should be present before any Update.
	gathered, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}
	for _, mf := range gathered {
		name := mf.GetName()
		if name == "marstek_battery_soc_percent" || name == "marstek_solar_input_watts" {
			t.Errorf("device gauge %q should not be present before first Update()", name)
		}
	}
}

// TestPhase0BMSAdjacentMetrics asserts that the pack-level SoC (pe/a1/a2) and
// the m0..m3 channels observed in every cd=0 capture are surfaced as gauges.
// Regressing any of these silently drops BMS-adjacent signal the exporter used
// to ignore — this is the Phase 0 "free win" anchor.
func TestPhase0BMSAdjacentMetrics(t *testing.T) {
	fixedNow := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	// Payload approximates a real cd=0 response: pe=45 aggregate, a0=a1=a2=0
	// on a single-pack device; m0..m3 present as raw channels. lv=42 is
	// copied into m3 so we have a distinct value to assert.
	payload := "pe=45,a0=45,a1=0,a2=0,m0=12,m1=34,m2=56,m3=42,lv=42"
	c, reg := newTestCollector(5*time.Minute, func() time.Time { return fixedNow })
	c.Update(payload)
	c.MarkUp()

	expected := `
# HELP marstek_battery_pack_soc_percent Per-pack state of charge in percent. pack=0 is the aggregate pe field; pack=1 and pack=2 are the a1/a2 channels.
# TYPE marstek_battery_pack_soc_percent gauge
marstek_battery_pack_soc_percent{device_id="test-device",device_type="HMJ-2",pack="0"} 45
marstek_battery_pack_soc_percent{device_id="test-device",device_type="HMJ-2",pack="1"} 0
marstek_battery_pack_soc_percent{device_id="test-device",device_type="HMJ-2",pack="2"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_battery_pack_soc_percent"); err != nil {
		t.Errorf("battery_pack_soc_percent not emitted correctly: %v", err)
	}

	expected = `
# HELP marstek_mqtt_m_channel Raw m0..m3 channels from the cd=0 MQTT telemetry. Semantics are partially decoded: m3 tracks the load watts (lv) in captured traffic; m0..m2 are unverified per-pack metrics. Use for anomaly detection until fully decoded.
# TYPE marstek_mqtt_m_channel gauge
marstek_mqtt_m_channel{channel="0",device_id="test-device",device_type="HMJ-2"} 12
marstek_mqtt_m_channel{channel="1",device_id="test-device",device_type="HMJ-2"} 34
marstek_mqtt_m_channel{channel="2",device_id="test-device",device_type="HMJ-2"} 56
marstek_mqtt_m_channel{channel="3",device_id="test-device",device_type="HMJ-2"} 42
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_mqtt_m_channel"); err != nil {
		t.Errorf("mqtt_m_channel not emitted correctly: %v", err)
	}
}

// TestPhase0PackSoCSparsePayload verifies that a payload missing a1/a2 emits
// only pack=0 (from pe), rather than publishing synthetic zeros for packs
// that were never reported.
func TestPhase0PackSoCSparsePayload(t *testing.T) {
	fixedNow := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	c, reg := newTestCollector(5*time.Minute, func() time.Time { return fixedNow })
	c.Update("pe=60")
	c.MarkUp()

	expected := `
# HELP marstek_battery_pack_soc_percent Per-pack state of charge in percent. pack=0 is the aggregate pe field; pack=1 and pack=2 are the a1/a2 channels.
# TYPE marstek_battery_pack_soc_percent gauge
marstek_battery_pack_soc_percent{device_id="test-device",device_type="HMJ-2",pack="0"} 60
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_battery_pack_soc_percent"); err != nil {
		t.Errorf("sparse pack SoC not emitted correctly: %v", err)
	}
}

func TestSurplusFeedInInversion(t *testing.T) {
	fixedNow := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	// tc_dis=1 means surplus feed-in is DISABLED (feed_in_enabled should be 0).
	payloadDisabled := "pe=50,tc_dis=1"
	c, reg := newTestCollector(5*time.Minute, func() time.Time { return fixedNow })
	c.Update(payloadDisabled)

	expected := `
# HELP marstek_surplus_feed_in_enabled 1 if surplus feed-in is enabled, 0 otherwise
# TYPE marstek_surplus_feed_in_enabled gauge
marstek_surplus_feed_in_enabled{device_id="test-device",device_type="HMJ-2"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "marstek_surplus_feed_in_enabled"); err != nil {
		t.Errorf("tc_dis=1 should yield surplus_feed_in_enabled=0: %v", err)
	}
}
