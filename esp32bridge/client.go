package esp32bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultRequestTimeout = 10 * time.Second

// Status is the subset of /api/status fields needed for recovery decisions.
type Status struct {
	Connected     bool `json:"connected"`
	WiFiConnected bool `json:"wifi_connected"`
	MQTTConnected bool `json:"mqtt_connected"`

	SOCPercent          *float64 `json:"soc_percent,omitempty"`
	RemainingCapacityWh *float64 `json:"remaining_capacity_wh,omitempty"`
	DoDPercent          *float64 `json:"dod,omitempty"`
	In1PowerWatts       *float64 `json:"in1_power_w,omitempty"`
	In2PowerWatts       *float64 `json:"in2_power_w,omitempty"`
	Out1PowerWatts      *float64 `json:"out1_power_w,omitempty"`
	Out2PowerWatts      *float64 `json:"out2_power_w,omitempty"`
	Out1Enabled         *bool    `json:"out1_enable,omitempty"`
	Out2Enabled         *bool    `json:"out2_enable,omitempty"`
	TemperatureLowC     *float64 `json:"temperature_low_c,omitempty"`
	TemperatureHighC    *float64 `json:"temperature_high_c,omitempty"`
	DailyChargeWh       *float64 `json:"daily_charge_wh,omitempty"`
	DailyDischargeWh    *float64 `json:"daily_discharge_wh,omitempty"`
	DailyLoadWh         *float64 `json:"daily_load_wh,omitempty"`
}

func (s Status) Healthy() bool {
	return s.Connected && s.WiFiConnected && s.MQTTConnected
}

func (s Status) HasRuntimeTelemetry() bool {
	return s.SOCPercent != nil ||
		s.RemainingCapacityWh != nil ||
		s.DoDPercent != nil ||
		s.In1PowerWatts != nil ||
		s.In2PowerWatts != nil ||
		s.Out1PowerWatts != nil ||
		s.Out2PowerWatts != nil ||
		s.Out1Enabled != nil ||
		s.Out2Enabled != nil ||
		s.TemperatureLowC != nil ||
		s.TemperatureHighC != nil ||
		s.DailyChargeWh != nil ||
		s.DailyDischargeWh != nil ||
		s.DailyLoadWh != nil
}

type WiFiConfig struct {
	SSID     string
	Password string
}

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: defaultRequestTimeout,
		},
	}
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var status Status
	if err := c.do(ctx, http.MethodGet, "/api/status", nil, &status); err != nil {
		return Status{}, err
	}
	return status, nil
}

func (c *Client) ConfigureWiFi(ctx context.Context, cfg WiFiConfig) error {
	body := map[string]string{
		"ssid":     cfg.SSID,
		"password": cfg.Password,
	}
	return c.do(ctx, http.MethodPost, "/api/wifi", body, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}
