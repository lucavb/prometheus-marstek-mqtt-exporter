package config

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MQTTHost        string
	MQTTPort        int
	MQTTUsername    string
	MQTTPassword    string
	MQTTClientID    string
	DeviceType      string
	DeviceID        string
	PollInterval    time.Duration
	ResponseTimeout time.Duration
	MetricTTL       time.Duration
	ListenAddr      string
	LogLevel        string
	LogFormat       string
	LogSource       bool

	// ESP32 BLE bridge recovery (optional). Empty ESP32BaseURL disables it.
	ESP32BaseURL             string
	ESP32CheckInterval       time.Duration
	ESP32RecoveryMissedPolls int
	ESP32MaxRecoveryAttempts int
	BatteryWiFiSSID          string
	BatteryWiFiPassword      string

	// Cloud emulator (optional). Empty EmulatorListenAddr disables the feature.
	EmulatorListenAddr string
	EmulatorTZ         string
	EmulatorLocation   *time.Location
}

// Load applies defaults → env vars → CLI flags (flags win).
// Exits with code 2 on any invalid value.
func Load() *Config {
	cfg, err := load(os.Args[1:], os.LookupEnv, os.ReadFile, os.Hostname, os.Getpid())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	return cfg
}

func load(args []string, lookupEnv func(string) (string, bool), readFile func(string) ([]byte, error), hostname func() (string, error), pid int) (*Config, error) {
	cfg := &Config{}

	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	env := func(key, fallback string) string { return envOr(lookupEnv, key, fallback) }
	envInt := func(key string, fallback int) int { return envOrInt(lookupEnv, key, fallback) }
	envBool := func(key string, fallback bool) bool { return envOrBool(lookupEnv, key, fallback) }

	mqttHost := fs.String("mqtt-host", env("MARSTEK_MQTT_HOST", ""), "Broker host (env: MARSTEK_MQTT_HOST)")
	mqttPort := fs.Int("mqtt-port", envInt("MARSTEK_MQTT_PORT", 1883), "Broker port (env: MARSTEK_MQTT_PORT)")
	mqttUsername := fs.String("mqtt-username", env("MARSTEK_MQTT_USERNAME", ""), "Optional broker username, empty = anonymous (env: MARSTEK_MQTT_USERNAME)")
	mqttPassword := fs.String("mqtt-password", env("MARSTEK_MQTT_PASSWORD", ""), "Optional broker password (env: MARSTEK_MQTT_PASSWORD)")
	mqttPasswordFile := fs.String("mqtt-password-file", env("MARSTEK_MQTT_PASSWORD_FILE", ""), "Path to file containing broker password; overrides --mqtt-password (env: MARSTEK_MQTT_PASSWORD_FILE)")
	mqttClientID := fs.String("mqtt-client-id", env("MARSTEK_MQTT_CLIENT_ID", ""), "MQTT client ID; auto-generated if empty (env: MARSTEK_MQTT_CLIENT_ID)")
	deviceType := fs.String("device-type", env("MARSTEK_DEVICE_TYPE", "HMJ-2"), "MQTT topic device type segment (env: MARSTEK_DEVICE_TYPE)")
	deviceID := fs.String("device-id", env("MARSTEK_DEVICE_ID", ""), "MQTT topic device ID segment (env: MARSTEK_DEVICE_ID)")
	pollInterval := fs.String("poll-interval", env("MARSTEK_POLL_INTERVAL", "30s"), "How often to send cd=1 (env: MARSTEK_POLL_INTERVAL)")
	responseTimeout := fs.String("response-timeout", env("MARSTEK_RESPONSE_TIMEOUT", "8s"), "Max wait for device response (env: MARSTEK_RESPONSE_TIMEOUT)")
	metricTTL := fs.String("metric-ttl", env("MARSTEK_METRIC_TTL", ""), "How long to keep device gauge values after the last successful update before dropping them from /metrics; empty = 3×poll-interval (env: MARSTEK_METRIC_TTL)")
	listenAddr := fs.String("listen-addr", env("MARSTEK_LISTEN_ADDR", ":9734"), "HTTP metrics listen address (env: MARSTEK_LISTEN_ADDR)")
	logLevel := fs.String("log-level", env("MARSTEK_LOG_LEVEL", "info"), "Log level: debug, info, warn, error (env: MARSTEK_LOG_LEVEL)")
	logFormat := fs.String("log-format", env("MARSTEK_LOG_FORMAT", "text"), "Log format: text or json (env: MARSTEK_LOG_FORMAT)")
	logSource := fs.Bool("log-source", envBool("MARSTEK_LOG_SOURCE", false), "Add source file/line to log records (env: MARSTEK_LOG_SOURCE)")
	esp32BaseURL := fs.String("esp32-base-url", env("MARSTEK_ESP32_BASE_URL", ""), "Base URL for optional ESP32 BLE bridge recovery; empty = disabled (env: MARSTEK_ESP32_BASE_URL)")
	esp32CheckIntervalSeconds := fs.Int("esp32-check-interval-seconds", envInt("MARSTEK_ESP32_CHECK_INTERVAL_SECONDS", 300), "How often to check ESP32 bridge status, in seconds (env: MARSTEK_ESP32_CHECK_INTERVAL_SECONDS)")
	esp32RecoveryMissedPolls := fs.Int("esp32-recovery-missed-polls", envInt("MARSTEK_ESP32_RECOVERY_MISSED_POLLS", 3), "Consecutive missed MQTT polls before an early ESP32 status check (env: MARSTEK_ESP32_RECOVERY_MISSED_POLLS)")
	esp32MaxRecoveryAttempts := fs.Int("esp32-max-recovery-attempts", envInt("MARSTEK_ESP32_MAX_RECOVERY_ATTEMPTS", 3), "Full ESP32 recovery attempts per continuous outage before human intervention is required (env: MARSTEK_ESP32_MAX_RECOVERY_ATTEMPTS)")
	batteryWiFiSSID := fs.String("battery-wifi-ssid", env("MARSTEK_BATTERY_WIFI_SSID", ""), "Battery WiFi SSID to provision through the ESP32 bridge (env: MARSTEK_BATTERY_WIFI_SSID)")
	batteryWiFiPassword := fs.String("battery-wifi-password", env("MARSTEK_BATTERY_WIFI_PASSWORD", ""), "Battery WiFi password to provision through the ESP32 bridge (env: MARSTEK_BATTERY_WIFI_PASSWORD)")
	emulatorListenAddr := fs.String("emulator-listen-addr", env("MARSTEK_EMULATOR_LISTEN_ADDR", ""), "Listen address for the cloud emulator server; empty = disabled (env: MARSTEK_EMULATOR_LISTEN_ADDR)")
	emulatorTZ := fs.String("emulator-tz", env("MARSTEK_EMULATOR_TZ", ""), "Timezone for the cloud emulator time-sync response (e.g. Europe/Berlin); empty = system timezone (env: MARSTEK_EMULATOR_TZ)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if strings.TrimSpace(*mqttHost) == "" {
		return nil, fmt.Errorf("--mqtt-host (or MARSTEK_MQTT_HOST) is required")
	}
	if strings.TrimSpace(*deviceID) == "" {
		return nil, fmt.Errorf("--device-id (or MARSTEK_DEVICE_ID) is required")
	}

	pi, err := time.ParseDuration(*pollInterval)
	if err != nil || pi <= 0 {
		return nil, fmt.Errorf("invalid --poll-interval %q: must be a positive duration", *pollInterval)
	}
	rt, err := time.ParseDuration(*responseTimeout)
	if err != nil || rt <= 0 {
		return nil, fmt.Errorf("invalid --response-timeout %q: must be a positive duration", *responseTimeout)
	}

	var ttl time.Duration
	if *metricTTL == "" {
		ttl = 3 * pi
	} else {
		ttl, err = time.ParseDuration(*metricTTL)
		if err != nil || ttl <= 0 {
			return nil, fmt.Errorf("invalid --metric-ttl %q: must be a positive duration", *metricTTL)
		}
	}
	if *esp32CheckIntervalSeconds <= 0 {
		return nil, fmt.Errorf("invalid --esp32-check-interval-seconds %d: must be positive", *esp32CheckIntervalSeconds)
	}
	if *esp32RecoveryMissedPolls <= 0 {
		return nil, fmt.Errorf("invalid --esp32-recovery-missed-polls %d: must be positive", *esp32RecoveryMissedPolls)
	}
	if *esp32MaxRecoveryAttempts <= 0 {
		return nil, fmt.Errorf("invalid --esp32-max-recovery-attempts %d: must be positive", *esp32MaxRecoveryAttempts)
	}
	esp32Enabled := strings.TrimSpace(*esp32BaseURL) != ""
	if esp32Enabled {
		if strings.TrimSpace(*batteryWiFiSSID) == "" {
			return nil, fmt.Errorf("--battery-wifi-ssid (or MARSTEK_BATTERY_WIFI_SSID) is required when ESP32 recovery is enabled")
		}
		if strings.TrimSpace(*batteryWiFiPassword) == "" {
			return nil, fmt.Errorf("--battery-wifi-password (or MARSTEK_BATTERY_WIFI_PASSWORD) is required when ESP32 recovery is enabled")
		}
	}

	// File overrides inline password value (docker/k8s secret pattern).
	password := *mqttPassword
	if *mqttPasswordFile != "" {
		data, err := readFile(*mqttPasswordFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read --mqtt-password-file %q: %w", *mqttPasswordFile, err)
		}
		password = strings.TrimRight(string(data), "\r\n")
	}

	clientID := *mqttClientID
	if clientID == "" {
		host, _ := hostname()
		clientID = fmt.Sprintf("marstek-exporter-%s-%d", host, pid)
	}

	switch strings.ToLower(*logLevel) {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("invalid --log-level %q: must be debug, info, warn, or error", *logLevel)
	}
	switch strings.ToLower(*logFormat) {
	case "text", "json":
	default:
		return nil, fmt.Errorf("invalid --log-format %q: must be text or json", *logFormat)
	}

	// Resolve emulator timezone; empty string means system timezone.
	var emulatorLoc *time.Location
	if *emulatorTZ == "" {
		emulatorLoc = time.Local
	} else {
		var err error
		emulatorLoc, err = time.LoadLocation(*emulatorTZ)
		if err != nil {
			return nil, fmt.Errorf("invalid --emulator-tz %q: %w", *emulatorTZ, err)
		}
	}

	cfg.MQTTHost = *mqttHost
	cfg.MQTTPort = *mqttPort
	cfg.MQTTUsername = *mqttUsername
	cfg.MQTTPassword = password
	cfg.MQTTClientID = clientID
	cfg.DeviceType = *deviceType
	cfg.DeviceID = *deviceID
	cfg.PollInterval = pi
	cfg.ResponseTimeout = rt
	cfg.MetricTTL = ttl
	cfg.ListenAddr = *listenAddr
	cfg.LogLevel = strings.ToLower(*logLevel)
	cfg.LogFormat = strings.ToLower(*logFormat)
	cfg.LogSource = *logSource
	cfg.ESP32BaseURL = strings.TrimRight(strings.TrimSpace(*esp32BaseURL), "/")
	cfg.ESP32CheckInterval = time.Duration(*esp32CheckIntervalSeconds) * time.Second
	cfg.ESP32RecoveryMissedPolls = *esp32RecoveryMissedPolls
	cfg.ESP32MaxRecoveryAttempts = *esp32MaxRecoveryAttempts
	cfg.BatteryWiFiSSID = *batteryWiFiSSID
	cfg.BatteryWiFiPassword = *batteryWiFiPassword
	cfg.EmulatorListenAddr = *emulatorListenAddr
	cfg.EmulatorTZ = *emulatorTZ
	cfg.EmulatorLocation = emulatorLoc

	return cfg, nil
}

func SetupLogger(cfg *Config) {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: cfg.LogSource,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// Loki detected_level expects lowercase; slog emits "INFO", "WARN", etc.
			if a.Key == slog.LevelKey {
				if lv, ok := a.Value.Any().(slog.Level); ok {
					a.Value = slog.StringValue(strings.ToLower(lv.String()))
				}
			}
			return a
		},
	}

	var handler slog.Handler
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	slog.SetDefault(slog.New(handler))
}

func LogConfig(cfg *Config) {
	passwordMask := ""
	if cfg.MQTTPassword != "" {
		passwordMask = "***"
	}
	batteryWiFiPasswordMask := ""
	if cfg.BatteryWiFiPassword != "" {
		batteryWiFiPasswordMask = "***"
	}

	emulatorTZName := cfg.EmulatorLocation.String()

	slog.Info("config loaded",
		"mqtt_host", cfg.MQTTHost,
		"mqtt_port", cfg.MQTTPort,
		"mqtt_username", cfg.MQTTUsername,
		"mqtt_password", passwordMask,
		"mqtt_client_id", cfg.MQTTClientID,
		"device_type", cfg.DeviceType,
		"device_id", cfg.DeviceID,
		"poll_interval", cfg.PollInterval.String(),
		"response_timeout", cfg.ResponseTimeout.String(),
		"metric_ttl", cfg.MetricTTL.String(),
		"listen_addr", cfg.ListenAddr,
		"log_level", cfg.LogLevel,
		"log_format", cfg.LogFormat,
		"log_source", cfg.LogSource,
		"esp32_base_url", cfg.ESP32BaseURL,
		"esp32_check_interval", cfg.ESP32CheckInterval.String(),
		"esp32_recovery_missed_polls", cfg.ESP32RecoveryMissedPolls,
		"esp32_max_recovery_attempts", cfg.ESP32MaxRecoveryAttempts,
		"battery_wifi_ssid", cfg.BatteryWiFiSSID,
		"battery_wifi_password", batteryWiFiPasswordMask,
		"emulator_listen_addr", cfg.EmulatorListenAddr,
		"emulator_tz", emulatorTZName,
	)
}

func envOr(lookupEnv func(string) (string, bool), key, fallback string) string {
	if v, ok := lookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envOrInt(lookupEnv func(string) (string, bool), key string, fallback int) int {
	if v, ok := lookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envOrBool(lookupEnv func(string) (string, bool), key string, fallback bool) bool {
	if v, ok := lookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}
