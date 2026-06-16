package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level application configuration loaded from YAML and env vars.
type Config struct {
	Server  ServerConfig  `yaml:"server"`
	MQTT    MQTTConfig    `yaml:"mqtt"`
	App     AppConfig     `yaml:"app"`
	Gateway GatewayConfig `yaml:"gateway"`
}

// GatewayConfig holds connection details for the Aruba IoT Gateway API.
// These values are provided by the container runtime as environment variables
// (APIGW_URL, APIKEY, GATEWAY_ID, etc.) as per the Aruba IoT Operations Partner API.
type GatewayConfig struct {
	// APIGW_URL is the base URL of the API gateway, e.g. "http://apigw.iot-gateway.svc".
	APIGWURL string `yaml:"apigw_url"`
	// APIKey is the bearer API key injected by the platform (env: APIKEY).
	APIKey string `yaml:"api_key"`
	// GatewayID is the gateway/data-collector unique identifier (env: GATEWAY_ID).
	GatewayID string `yaml:"gateway_id"`
	// GatewayIP is the IP of the data collector (env: GATEWAY_IP).
	GatewayIP string `yaml:"gateway_ip"`
	// GatewayName is the human-readable name of the data collector (env: GATEWAY_NAME).
	GatewayName string `yaml:"gateway_name"`
	// RequestTimeoutSeconds is the HTTP call timeout for gateway API requests.
	RequestTimeoutSeconds int `yaml:"request_timeout_seconds"`
}

// ServerConfig controls HTTP bind address and port.
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// MQTTConfig controls broker connection and pub/sub topics.
type MQTTConfig struct {
	Enabled         bool   `yaml:"enabled"`
	BrokerURL       string `yaml:"broker_url"`
	ClientID        string `yaml:"client_id"`
	Username        string `yaml:"username"`
	Password        string `yaml:"password"`
	SouthboundTopic string `yaml:"southbound_topic"`
	NorthboundTopic string `yaml:"northbound_topic"`
	QOS             byte   `yaml:"qos"`
}

// AppConfig controls app-level behavior like logging and periodic intervals.
type AppConfig struct {
	LogLevel              string `yaml:"log_level"`
	StatusIntervalSeconds int    `yaml:"status_interval_seconds"`
	// DeviceMacAddress1..5 are optional Zigbee device MAC addresses (env: DEVICE_MAC_ADDRESS_1..5).
	// When set, the app will provision each device after connecting to the gateway by
	// calling SetDeviceID and UpdateDeviceClass with device class "Aqara".
	DeviceMacAddress1 string `yaml:"device_mac_address_1"`
	DeviceMacAddress2 string `yaml:"device_mac_address_2"`
	DeviceMacAddress3 string `yaml:"device_mac_address_3"`
	DeviceMacAddress4 string `yaml:"device_mac_address_4"`
	DeviceMacAddress5 string `yaml:"device_mac_address_5"`
}

// Default returns safe, useful defaults so the app can run with minimal setup.
// MQTT broker URL, credentials, and topics must be provided via environment variables
// (MQTT_BROKER_URL, MQTT_USERNAME, MQTT_PASSWORD, MQTT_CLIENT_ID, etc.).
// If MQTT_BROKER_URL is not set the MQTT client is disabled automatically.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
		MQTT: MQTTConfig{
			Enabled:         true,
			BrokerURL:       "", // Required: set via MQTT_SERVER_URL env var
			ClientID:        "zigbee-demo-app",
			SouthboundTopic: "iot/zigbee/southbound",
			NorthboundTopic: "iot/zigbee/northbound",
			QOS:             1,
		},
		App: AppConfig{
			LogLevel:              "INFO",
			StatusIntervalSeconds: 60,
		},
		Gateway: GatewayConfig{
			RequestTimeoutSeconds: 30,
		},
	}
}

// Load reads configuration in this order:
//  1. built-in defaults
//  2. config file values (if present)
//  3. environment variable overrides
func Load(path string) (Config, error) {
	cfg := Default()

	if path == "" {
		// Use conventional file name when caller does not provide a path.
		path = "config.yaml"
	}

	// Missing config file is allowed, because defaults + env can be enough.
	if b, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config file: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read config file: %w", err)
	}

	applyEnvOverrides(&cfg)
	normalize(&cfg)
	return cfg, nil
}

// Address returns HTTP listen address in host:port format.
func (c Config) Address() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}

// StatusInterval converts integer seconds into time.Duration.
func (c Config) StatusInterval() time.Duration {
	return time.Duration(c.App.StatusIntervalSeconds) * time.Second
}

// GatewayRequestTimeout converts integer seconds into time.Duration for gateway HTTP calls.
func (c Config) GatewayRequestTimeout() time.Duration {
	if c.Gateway.RequestTimeoutSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.Gateway.RequestTimeoutSeconds) * time.Second
}

// GatewayEnabled reports whether the gateway URL has been configured.
func (c Config) GatewayEnabled() bool {
	return strings.TrimSpace(c.Gateway.APIGWURL) != ""
}

// normalize fills invalid or empty values with defaults.
func normalize(cfg *Config) {
	if cfg.Server.Host == "" {
		cfg.Server.Host = "0.0.0.0"
	}
	if cfg.Server.Port <= 0 {
		cfg.Server.Port = 8080
	}
	if cfg.MQTT.ClientID == "" {
		cfg.MQTT.ClientID = "zigbee-demo-app"
	}
	if cfg.MQTT.SouthboundTopic == "" {
		cfg.MQTT.SouthboundTopic = "iot/zigbee/southbound"
	}
	if cfg.MQTT.NorthboundTopic == "" {
		cfg.MQTT.NorthboundTopic = "iot/zigbee/northbound"
	}
	if cfg.MQTT.QOS > 2 {
		cfg.MQTT.QOS = 1
	}
	cfg.App.LogLevel = strings.ToUpper(strings.TrimSpace(cfg.App.LogLevel))
	if cfg.App.LogLevel != "DEBUG" && cfg.App.LogLevel != "INFO" && cfg.App.LogLevel != "WARN" && cfg.App.LogLevel != "ERROR" {
		cfg.App.LogLevel = "INFO"
	}
	if cfg.App.StatusIntervalSeconds <= 0 {
		cfg.App.StatusIntervalSeconds = 60
	}
	if strings.TrimSpace(cfg.MQTT.BrokerURL) == "" {
		// Empty broker URL means MQTT cannot connect, so disable it explicitly.
		cfg.MQTT.Enabled = false
	}
}

// applyEnvOverrides maps env vars to config fields.
// If an env var is missing or invalid, existing config values stay unchanged.
func applyEnvOverrides(cfg *Config) {
	setString(&cfg.Server.Host, "SERVER_HOST")
	setInt(&cfg.Server.Port, "SERVER_PORT")

	setBool(&cfg.MQTT.Enabled, "MQTT_ENABLED")
	setString(&cfg.MQTT.BrokerURL, "MQTT_SERVER_URL")
	setString(&cfg.MQTT.ClientID, "MQTT_CLIENT_ID")
	setString(&cfg.MQTT.Username, "MQTT_USERNAME")
	setString(&cfg.MQTT.Password, "MQTT_PASSWORD")
	setString(&cfg.MQTT.SouthboundTopic, "MQTT_SOUTHBOUND_TOPIC")
	setString(&cfg.MQTT.NorthboundTopic, "MQTT_NORTHBOUND_TOPIC")
	setByte(&cfg.MQTT.QOS, "MQTT_QOS")

	setString(&cfg.App.LogLevel, "LOG_LEVEL")
	setInt(&cfg.App.StatusIntervalSeconds, "APP_STATUS_INTERVAL_SECONDS")
	setString(&cfg.App.DeviceMacAddress1, "DEVICE_MAC_ADDRESS_1")
	setString(&cfg.App.DeviceMacAddress2, "DEVICE_MAC_ADDRESS_2")
	setString(&cfg.App.DeviceMacAddress3, "DEVICE_MAC_ADDRESS_3")
	setString(&cfg.App.DeviceMacAddress4, "DEVICE_MAC_ADDRESS_4")
	setString(&cfg.App.DeviceMacAddress5, "DEVICE_MAC_ADDRESS_5")

	// Aruba IoT Gateway environment variables (injected by the container platform).
	setString(&cfg.Gateway.APIGWURL, "APIGW_URL")
	setString(&cfg.Gateway.APIKey, "APIKEY")
	setString(&cfg.Gateway.GatewayID, "GATEWAY_ID")
	setString(&cfg.Gateway.GatewayIP, "GATEWAY_IP")
	setString(&cfg.Gateway.GatewayName, "GATEWAY_NAME")
	setInt(&cfg.Gateway.RequestTimeoutSeconds, "GATEWAY_REQUEST_TIMEOUT_SECONDS")
}

// setString replaces destination with env value when present.
func setString(dest *string, key string) {
	v, ok := os.LookupEnv(key)
	if ok {
		*dest = v
	}
}

// setInt parses integer env values safely.
func setInt(dest *int, key string) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return
	}
	if parsed, err := strconv.Atoi(v); err == nil {
		*dest = parsed
	}
}

// setBool parses boolean env values safely.
func setBool(dest *bool, key string) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return
	}
	if parsed, err := strconv.ParseBool(v); err == nil {
		*dest = parsed
	}
}

// setByte parses MQTT QoS (valid range: 0..2).
func setByte(dest *byte, key string) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return
	}
	if parsed, err := strconv.Atoi(v); err == nil && parsed >= 0 && parsed <= 2 {
		*dest = byte(parsed)
	}
}
