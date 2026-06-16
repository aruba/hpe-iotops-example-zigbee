package mqtt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"zigbee-demo-app/pkg/config"
	"zigbee-demo-app/pkg/retry"
	"zigbee-demo-app/pkg/zigbee"
)

// Client wraps the Paho MQTT client and coordinates southbound/northbound behavior.
type Client struct {
	logger      *slog.Logger
	gateway     *zigbee.GatewayClient
	cfg         config.MQTTConfig
	client      paho.Client
	lastPublish atomic.Int64
}

// commandMessage describes the JSON format expected from subscribed command topics.
type commandMessage struct {
	RequestID   string         `json:"request_id,omitempty"`
	Action      string         `json:"action,omitempty"`
	DeviceID    string         `json:"device_id,omitempty"`
	RadioMAC    string         `json:"radio_mac,omitempty"`
	Command     string         `json:"command,omitempty"`
	Params      map[string]any `json:"params,omitempty"`
	DeviceClass []string       `json:"device_class,omitempty"`
	IDType      string         `json:"id_type,omitempty"`
	Fields      []string       `json:"fields,omitempty"`
}

type commandResponse struct {
	RequestID string `json:"request_id,omitempty"`
	Action    string `json:"action,omitempty"`
	Status    string `json:"status"`
	Result    any    `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
}

// NewClient creates an MQTT integration instance.
func NewClient(cfg config.MQTTConfig, logger *slog.Logger, gateway *zigbee.GatewayClient) *Client {
	return &Client{cfg: cfg, logger: logger, gateway: gateway}
}

// Start connects to the broker and subscribes to the southbound topic.
// If the broker is not reachable at startup the app continues running and a background
// goroutine retries the connection with exponential backoff until it succeeds.
func (c *Client) Start(ctx context.Context) error {
	if !c.cfg.Enabled {
		c.logger.Info("mqtt_disabled", "reason", "MQTT disabled by config or missing broker URL")
		return nil
	}

	// Configure reconnect behavior so temporary broker/network issues self-heal.
	// AutoReconnect handles mid-session drops; we handle the initial connect ourselves.
	opts := paho.NewClientOptions().
		AddBroker(c.cfg.BrokerURL).
		SetClientID(c.cfg.ClientID).
		SetAutoReconnect(true).
		SetConnectRetry(false) // we manage startup retries explicitly below

	if c.cfg.Username != "" {
		opts.SetUsername(c.cfg.Username)
		opts.SetPassword(c.cfg.Password)
	}

	// On every (re)connect subscribe to the southbound topic.
	opts.SetOnConnectHandler(func(cli paho.Client) {
		c.logger.Info("mqtt_connected", "broker", c.cfg.BrokerURL)
		if topic := strings.TrimSpace(c.cfg.SouthboundTopic); topic != "" {
			wrapped := c.wrapInboundHandler(c.onSouthboundMessage)
			if token := cli.Subscribe(topic, c.cfg.QOS, wrapped); token.Wait() && token.Error() != nil {
				c.logger.Error("mqtt_subscribe_failed", "topic", topic, "error", token.Error())
				return
			}
			c.logger.Info("mqtt_subscribed", "topic", topic)
		}
	})

	// ConnectionLost gives visibility into transient outages.
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		c.logger.Warn("mqtt_connection_lost", "error", err)
	})

	c.client = paho.NewClient(opts)

	// Attempt initial connection with a 5-second timeout.
	c.logger.Info("mqtt_connecting", "broker", c.cfg.BrokerURL)
	if err := c.connectOnce(); err != nil {
		c.logger.Warn("mqtt_initial_connect_failed",
			"broker", c.cfg.BrokerURL,
			"error", err,
			"retry_strategy", "exponential_backoff",
		)
		// Retry in background with exponential backoff; app (HTTP API) remains available.
		go c.retryLoop(ctx)
		return nil
	}
	return nil
}

func (c *Client) wrapInboundHandler(next paho.MessageHandler) paho.MessageHandler {
	return func(cli paho.Client, msg paho.Message) {
		c.logInboundMessage(msg)
		if next != nil {
			next(cli, msg)
		}
	}
}

func (c *Client) logInboundMessage(msg paho.Message) {
	payload := msg.Payload()
	payloadPreview := string(payload)
	if len(payloadPreview) > 512 {
		payloadPreview = payloadPreview[:512] + "..."
	}

	c.logger.Info("mqtt_message_received",
		"topic", msg.Topic(),
		"qos", msg.Qos(),
		"retained", msg.Retained(),
		"duplicate", msg.Duplicate(),
		"payload_size", len(payload),
		"payload_preview", payloadPreview,
	)
}

// connectOnce attempts one MQTT connection with a 5-second timeout.
func (c *Client) connectOnce() error {
	token := c.client.Connect()
	if !token.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("connection timed out after 5s")
	}
	return token.Error()
}

// retryLoop keeps trying to connect to the MQTT broker using exponential backoff
// until the context is cancelled or a connection is established.
// Initial delay is 1 second, doubled on each retry, capped at 30 minutes.
func (c *Client) retryLoop(ctx context.Context) {
	backoff := retry.NewExponentialBackoff(1*time.Second, 2.0, 30*time.Minute)

	for {
		// If auto-reconnect already succeeded, nothing more to do.
		if c.IsConnected() {
			c.logger.Info("mqtt_retry_not_needed", "broker", c.cfg.BrokerURL, "status", "already connected")
			return
		}

		delay := backoff.NextDelay()
		c.logger.Info("mqtt_retry_scheduling",
			"broker", c.cfg.BrokerURL,
			"attempt", backoff.Attempts(),
			"next_retry_in", delay.String(),
		)

		// Wait for the calculated delay or context cancellation
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		// Attempt connection
		c.logger.Info("mqtt_retry_connecting", "broker", c.cfg.BrokerURL, "attempt", backoff.Attempts())
		if err := c.connectOnce(); err != nil {
			c.logger.Warn("mqtt_retry_connect_failed",
				"broker", c.cfg.BrokerURL,
				"error", err,
				"attempt", backoff.Attempts(),
			)
			continue
		}

		c.logger.Info("mqtt_retry_connected", "broker", c.cfg.BrokerURL, "attempts", backoff.Attempts())
		return
	}
}

// Stop disconnects cleanly from the broker.
func (c *Client) Stop() {
	if c.client != nil && c.client.IsConnected() {
		c.client.Disconnect(250)
	}
}

// IsConnected reports current MQTT connection state.
func (c *Client) IsConnected() bool {
	return c.client != nil && c.client.IsConnected()
}

// LastPublish returns the last successful telemetry publish timestamp.
func (c *Client) LastPublish() time.Time {
	unix := c.lastPublish.Load()
	if unix == 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0).UTC()
}

func (c *Client) handleGatewayAction(cmd commandMessage) {
	c.logger.Info("mqtt_action_dispatching",
		"request_id", cmd.RequestID,
		"action", cmd.Action,
		"device_id", cmd.DeviceID,
	)
	if c.gateway == nil {
		c.logger.Warn("mqtt_action_gateway_not_configured",
			"request_id", cmd.RequestID,
			"action", cmd.Action,
			"reason", "APIGW_URL and APIKEY are not set",
		)
		c.publishCommandResponse(commandResponse{
			RequestID: cmd.RequestID,
			Action:    cmd.Action,
			Status:    "error",
			Error:     "gateway client is not configured (set APIGW_URL and APIKEY)",
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	action := strings.ToLower(strings.TrimSpace(cmd.Action))
	switch action {
	case "zigbee_get_devices":
		result, err := c.gateway.GetDeviceList(ctx)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "zigbee_get_devices_unclassified":
		result, err := c.gateway.GetDevicesUnclassified(ctx)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "zigbee_get_device_data":
		if strings.TrimSpace(cmd.DeviceID) == "" {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "device_id is required"})
			return
		}
		deviceClass := paramString(cmd.Params, "device_class", "")
		result, err := c.gateway.GetDeviceData(ctx, cmd.DeviceID, cmd.IDType, deviceClass)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "zigbee_update_device_data_put":
		if strings.TrimSpace(cmd.DeviceID) == "" {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "device_id is required"})
			return
		}
		var body zigbee.DeviceDataUpdateInfo
		if err := decodeParamsObject(cmd.Params, "update_info", &body); err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		deviceClass := paramString(cmd.Params, "device_class", "")
		result, err := c.gateway.UpdateDeviceDataPut(ctx, cmd.DeviceID, cmd.IDType, deviceClass, body)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "zigbee_update_device_data_patch":
		if strings.TrimSpace(cmd.DeviceID) == "" {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "device_id is required"})
			return
		}
		var body zigbee.DeviceDataUpdateInfo
		if err := decodeParamsObject(cmd.Params, "update_info", &body); err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		deviceClass := paramString(cmd.Params, "device_class", "")
		result, err := c.gateway.UpdateDeviceDataPatch(ctx, cmd.DeviceID, cmd.IDType, deviceClass, body)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "zigbee_set_device_class":
		if strings.TrimSpace(cmd.DeviceID) == "" {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "device_id is required"})
			return
		}
		if len(cmd.DeviceClass) == 0 {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "device_class must include at least one value"})
			return
		}
		body := zigbee.DeviceClassArray{DeviceClass: cmd.DeviceClass}
		result, err := c.gateway.UpdateDeviceClass(ctx, cmd.DeviceID, body)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "zigbee_set_device_id":
		if strings.TrimSpace(cmd.DeviceID) == "" {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "device_id is required"})
			return
		}
		body := zigbee.DeviceClassScopedData{
			DeviceClass: paramString(cmd.Params, "device_class", ""),
			DeviceID:    paramString(cmd.Params, "target_device_id", ""),
		}
		if strings.TrimSpace(body.DeviceClass) == "" {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "params.device_class is required"})
			return
		}
		result, err := c.gateway.SetDeviceID(ctx, cmd.DeviceID, body.DeviceClass, body)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "zigbee_send_leave":
		if strings.TrimSpace(cmd.DeviceID) == "" {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "device_id is required"})
			return
		}
		result, err := c.gateway.SendLeave(ctx, cmd.DeviceID)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "zigbee_set_offline_timeout":
		if strings.TrimSpace(cmd.DeviceID) == "" {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "device_id is required"})
			return
		}
		minutes := paramInt(cmd.Params, "timeout_minutes", 0)
		if minutes < 1 || minutes > 1440 {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "params.timeout_minutes must be between 1 and 1440"})
			return
		}
		deviceClass := paramString(cmd.Params, "device_class", "")
		body := zigbee.OfflineTimeoutData{TimeoutMinutes: minutes}
		result, err := c.gateway.SetOfflineTimeout(ctx, cmd.DeviceID, cmd.IDType, deviceClass, body)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "zigbee_send_southbound_data":
		if strings.TrimSpace(cmd.DeviceID) == "" {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "device_id is required"})
			return
		}
		payloadB64 := paramString(cmd.Params, "payload_b64", "")
		if payloadB64 == "" {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "params.payload_b64 is required"})
			return
		}
		payloadRaw, err := base64.StdEncoding.DecodeString(payloadB64)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "params.payload_b64 must be valid base64"})
			return
		}
		body := zigbee.ZigbeeSouthboundData{
			Payload:   payloadRaw,
			ProfileID: paramInt(cmd.Params, "profile_id", 0),
			ClusterID: paramInt(cmd.Params, "cluster_id", 0),
		}
		deviceClass := paramString(cmd.Params, "device_class", "")
		result, err := c.gateway.SendSouthboundData(ctx, cmd.DeviceID, cmd.IDType, deviceClass, body)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "zigbee_set_timeout":
		if strings.TrimSpace(cmd.DeviceID) == "" {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "device_id is required"})
			return
		}
		timeoutValue := paramString(cmd.Params, "timeout", "")
		if timeoutValue == "" {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "params.timeout is required"})
			return
		}
		deviceClass := paramString(cmd.Params, "device_class", "")
		body := zigbee.TimeoutValueData{Timeout: timeoutValue}
		result, err := c.gateway.SetTimeoutValue(ctx, cmd.DeviceID, cmd.IDType, deviceClass, body)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "zigbee_get_radios":
		result, err := c.gateway.GetRadios(ctx)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "zigbee_get_radio_info":
		radioMAC := strings.TrimSpace(cmd.RadioMAC)
		if radioMAC == "" {
			radioMAC = paramString(cmd.Params, "radio_mac", "")
		}
		if radioMAC == "" {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: "radio_mac is required"})
			return
		}
		result, err := c.gateway.GetRadioInfo(ctx, radioMAC)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "zigbee_subscribe_packet_stream":
		fields := cmd.Fields
		if len(fields) == 0 {
			fields = paramStringSlice(cmd.Params, "fields")
		}
		result, err := c.gateway.SubscribePacketStream(ctx, fields)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	case "app_send_info":
		message := paramString(cmd.Params, "message", "")
		if message == "" {
			message = "Hello app is running as of timestamp " + strconv.FormatInt(time.Now().Unix(), 10)
		}
		result, err := c.gateway.SendAppInfo(ctx, message)
		if err != nil {
			c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "error", Error: err.Error()})
			return
		}
		c.publishCommandResponse(commandResponse{RequestID: cmd.RequestID, Action: action, Status: "ok", Result: result})

	default:
		c.publishCommandResponse(commandResponse{
			RequestID: cmd.RequestID,
			Action:    action,
			Status:    "error",
			Error:     "unsupported action: " + action,
		})
	}
}

func paramString(params map[string]any, key, defaultValue string) string {
	if params == nil {
		return defaultValue
	}
	v, ok := params[key]
	if !ok {
		return defaultValue
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return defaultValue
}

func paramInt(params map[string]any, key string, defaultValue int) int {
	if params == nil {
		return defaultValue
	}
	v, ok := params[key]
	if !ok {
		return defaultValue
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(n))
		if err == nil {
			return parsed
		}
	}
	return defaultValue
}

func paramStringSlice(params map[string]any, key string) []string {
	if params == nil {
		return nil
	}
	v, ok := params[key]
	if !ok {
		return nil
	}

	if arr, ok := v.([]any); ok {
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}

	if arr, ok := v.([]string); ok {
		return arr
	}

	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return []string{strings.TrimSpace(s)}
	}

	return nil
}

func decodeParamsObject(params map[string]any, key string, dest any) error {
	if params == nil {
		return fmt.Errorf("params.%s is required", key)
	}
	v, ok := params[key]
	if !ok {
		return fmt.Errorf("params.%s is required", key)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal params.%s: %w", key, err)
	}
	if err := json.Unmarshal(b, dest); err != nil {
		return fmt.Errorf("decode params.%s: %w", key, err)
	}
	return nil
}

func (c *Client) publishCommandResponse(resp commandResponse) {
	if c.client == nil || !c.client.IsConnected() {
		return
	}

	if strings.TrimSpace(resp.RequestID) == "" {
		resp.RequestID = strconv.FormatInt(time.Now().UnixNano(), 10)
	}

	payload, err := json.Marshal(resp)
	if err != nil {
		c.logger.Warn("mqtt_command_response_marshal_failed", "error", err)
		return
	}

	topic := strings.TrimSpace(c.cfg.NorthboundTopic)
	if topic == "" {
		topic = "iot/zigbee/northbound"
	}
	token := c.client.Publish(topic, c.cfg.QOS, false, payload)
	if token.Wait() && token.Error() != nil {
		c.logger.Warn("mqtt_command_response_publish_failed", "topic", topic, "error", token.Error())
		return
	}
	c.lastPublish.Store(time.Now().UTC().Unix())
	c.logger.Info("mqtt_command_response_published",
		"topic", topic,
		"request_id", resp.RequestID,
		"action", resp.Action,
		"status", resp.Status,
		"error", resp.Error,
	)
}

// onSouthboundMessage handles incoming MQTT southbound messages.
// Every message is decoded as a commandMessage, the full JSON is logged, then the
// action field is dispatched to handleGatewayAction — identical routing to the
// commands topic so any action (e.g. zigbee_set_device_id) triggers the correct API call.
func (c *Client) onSouthboundMessage(_ paho.Client, msg paho.Message) {
	var cmd commandMessage
	if err := json.Unmarshal(msg.Payload(), &cmd); err != nil {
		c.logger.Warn("mqtt_southbound_decode_failed",
			"topic", msg.Topic(),
			"error", err,
			"raw_payload", string(msg.Payload()),
		)
		return
	}

	// Log the full decoded message so every southbound receive is visible in logs.
	c.logger.Info("mqtt_southbound_message_decoded",
		"topic", msg.Topic(),
		"request_id", cmd.RequestID,
		"action", cmd.Action,
		"device_id", cmd.DeviceID,
		"id_type", cmd.IDType,
		"device_class", cmd.DeviceClass,
		"command", cmd.Command,
		"params", cmd.Params,
	)

	if strings.TrimSpace(cmd.Action) == "" {
		c.logger.Warn("mqtt_southbound_no_action", "topic", msg.Topic(), "reason", "action field is empty; no API call made")
		return
	}

	// Dispatch through the same gateway action router used by the commands topic.
	c.handleGatewayAction(cmd)
}

// PublishNorthboundStatus publishes app status to the northbound MQTT topic.
// The message is a JSON object containing app status information.
func (c *Client) PublishNorthboundStatus(status map[string]any) error {
	if c.client == nil || !c.client.IsConnected() {
		return fmt.Errorf("mqtt client not connected")
	}
	if c.cfg.NorthboundTopic == "" {
		return fmt.Errorf("northbound topic not configured")
	}

	// Marshal status to JSON
	payload, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal northbound status: %w", err)
	}

	// Publish to northbound topic
	token := c.client.Publish(c.cfg.NorthboundTopic, c.cfg.QOS, false, payload)
	if token.Wait() && token.Error() != nil {
		return fmt.Errorf("publish northbound status: %w", token.Error())
	}

	c.lastPublish.Store(time.Now().UTC().Unix())
	c.logger.Info("mqtt_northbound_status_published", "topic", c.cfg.NorthboundTopic)
	return nil
}
