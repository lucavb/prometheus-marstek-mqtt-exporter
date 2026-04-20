package config

import (
	"flag"
	"fmt"
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
	ListenAddr      string
	LogLevel        string
	LogFormat       string
	LogSource       bool

	// Cloud emulator (optional). Empty EmulatorListenAddr disables the feature.
	EmulatorListenAddr string
	EmulatorTZ         string
	EmulatorLocation   *time.Location
}

// Load applies defaults → env vars → CLI flags (flags win).
// Exits with code 2 on any invalid value.
func Load() *Config {
	cfg := &Config{}

	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	mqttHost := fs.String("mqtt-host", envOr("MARSTEK_MQTT_HOST", "10.1.1.5"), "Broker host (env: MARSTEK_MQTT_HOST)")
	mqttPort := fs.Int("mqtt-port", envOrInt("MARSTEK_MQTT_PORT", 1883), "Broker port (env: MARSTEK_MQTT_PORT)")
	mqttUsername := fs.String("mqtt-username", envOr("MARSTEK_MQTT_USERNAME", ""), "Optional broker username, empty = anonymous (env: MARSTEK_MQTT_USERNAME)")
	mqttPassword := fs.String("mqtt-password", envOr("MARSTEK_MQTT_PASSWORD", ""), "Optional broker password (env: MARSTEK_MQTT_PASSWORD)")
	mqttPasswordFile := fs.String("mqtt-password-file", envOr("MARSTEK_MQTT_PASSWORD_FILE", ""), "Path to file containing broker password; overrides --mqtt-password (env: MARSTEK_MQTT_PASSWORD_FILE)")
	mqttClientID := fs.String("mqtt-client-id", envOr("MARSTEK_MQTT_CLIENT_ID", ""), "MQTT client ID; auto-generated if empty (env: MARSTEK_MQTT_CLIENT_ID)")
	deviceType := fs.String("device-type", envOr("MARSTEK_DEVICE_TYPE", "HMJ-2"), "MQTT topic device type segment (env: MARSTEK_DEVICE_TYPE)")
	deviceID := fs.String("device-id", envOr("MARSTEK_DEVICE_ID", "60323bd14b6e"), "MQTT topic device ID segment (env: MARSTEK_DEVICE_ID)")
	pollInterval := fs.String("poll-interval", envOr("MARSTEK_POLL_INTERVAL", "30s"), "How often to send cd=1 (env: MARSTEK_POLL_INTERVAL)")
	responseTimeout := fs.String("response-timeout", envOr("MARSTEK_RESPONSE_TIMEOUT", "8s"), "Max wait for device response (env: MARSTEK_RESPONSE_TIMEOUT)")
	listenAddr := fs.String("listen-addr", envOr("MARSTEK_LISTEN_ADDR", ":9734"), "HTTP metrics listen address (env: MARSTEK_LISTEN_ADDR)")
	logLevel := fs.String("log-level", envOr("MARSTEK_LOG_LEVEL", "info"), "Log level: debug, info, warn, error (env: MARSTEK_LOG_LEVEL)")
	logFormat := fs.String("log-format", envOr("MARSTEK_LOG_FORMAT", "text"), "Log format: text or json (env: MARSTEK_LOG_FORMAT)")
	logSource := fs.Bool("log-source", envOrBool("MARSTEK_LOG_SOURCE", false), "Add source file/line to log records (env: MARSTEK_LOG_SOURCE)")
	emulatorListenAddr := fs.String("emulator-listen-addr", envOr("MARSTEK_EMULATOR_LISTEN_ADDR", ""), "Listen address for the cloud emulator server; empty = disabled (env: MARSTEK_EMULATOR_LISTEN_ADDR)")
	emulatorTZ := fs.String("emulator-tz", envOr("MARSTEK_EMULATOR_TZ", ""), "Timezone for the cloud emulator time-sync response (e.g. Europe/Berlin); empty = system timezone (env: MARSTEK_EMULATOR_TZ)")

	_ = fs.Parse(os.Args[1:])

	pi, err := time.ParseDuration(*pollInterval)
	if err != nil || pi <= 0 {
		fmt.Fprintf(os.Stderr, "error: invalid --poll-interval %q: must be a positive duration\n", *pollInterval)
		os.Exit(2)
	}
	rt, err := time.ParseDuration(*responseTimeout)
	if err != nil || rt <= 0 {
		fmt.Fprintf(os.Stderr, "error: invalid --response-timeout %q: must be a positive duration\n", *responseTimeout)
		os.Exit(2)
	}

	// File overrides inline password value (docker/k8s secret pattern).
	password := *mqttPassword
	if *mqttPasswordFile != "" {
		data, err := os.ReadFile(*mqttPasswordFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot read --mqtt-password-file %q: %v\n", *mqttPasswordFile, err)
			os.Exit(2)
		}
		password = strings.TrimRight(string(data), "\r\n")
	}

	clientID := *mqttClientID
	if clientID == "" {
		hostname, _ := os.Hostname()
		clientID = fmt.Sprintf("marstek-exporter-%s-%d", hostname, os.Getpid())
	}

	switch strings.ToLower(*logLevel) {
	case "debug", "info", "warn", "error":
	default:
		fmt.Fprintf(os.Stderr, "error: invalid --log-level %q: must be debug, info, warn, or error\n", *logLevel)
		os.Exit(2)
	}
	switch strings.ToLower(*logFormat) {
	case "text", "json":
	default:
		fmt.Fprintf(os.Stderr, "error: invalid --log-format %q: must be text or json\n", *logFormat)
		os.Exit(2)
	}

	// Resolve emulator timezone; empty string means system timezone.
	var emulatorLoc *time.Location
	if *emulatorTZ == "" {
		emulatorLoc = time.Local
	} else {
		var err error
		emulatorLoc, err = time.LoadLocation(*emulatorTZ)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid --emulator-tz %q: %v\n", *emulatorTZ, err)
			os.Exit(2)
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
	cfg.ListenAddr = *listenAddr
	cfg.LogLevel = strings.ToLower(*logLevel)
	cfg.LogFormat = strings.ToLower(*logFormat)
	cfg.LogSource = *logSource
	cfg.EmulatorListenAddr = *emulatorListenAddr
	cfg.EmulatorTZ = *emulatorTZ
	cfg.EmulatorLocation = emulatorLoc

	return cfg
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
		"listen_addr", cfg.ListenAddr,
		"log_level", cfg.LogLevel,
		"log_format", cfg.LogFormat,
		"log_source", cfg.LogSource,
		"emulator_listen_addr", cfg.EmulatorListenAddr,
		"emulator_tz", emulatorTZName,
	)
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envOrInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envOrBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}
