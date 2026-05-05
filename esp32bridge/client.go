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
}

func (s Status) Healthy() bool {
	return s.Connected && s.WiFiConnected && s.MQTTConnected
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
