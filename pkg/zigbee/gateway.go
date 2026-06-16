package zigbee

// gateway.go implements an HTTP client that calls the Aruba IoT Operations Partner API
// for all Zigbee endpoints defined under /api/v3/zigbee/.
//
// How it works (for Go beginners):
//  1. GatewayClient holds a shared http.Client and the base URL + API key from config.
//  2. Every method builds a request URL, optionally attaches a JSON body, adds
//     the "apikey" header, sends the request and decodes the JSON response.
//  3. The caller (HTTP handler) just calls the method and gets back a typed result.
//
// Reference: apidefinitionsiot.yaml — tag "Zigbee"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zigbee-demo-app/pkg/config"
)

// ────────────────────────────────────────────────────────────────
// Request / Response types (derived from apidefinitionsiot.yaml)
// ────────────────────────────────────────────────────────────────

// ZigbeeDeviceSummary is one entry in the device list response.
type ZigbeeDeviceSummary struct {
	Mac         string `json:"mac"`
	DeviceID    string `json:"deviceId,omitempty"`
	DeviceClass []any  `json:"deviceClass,omitempty"`
}

// GetZigbeeDeviceListResponse maps to v3GetZigbeeDeviceListResponse.
type GetZigbeeDeviceListResponse struct {
	Devices []ZigbeeDeviceSummary `json:"devices,omitempty"`
}

// GetZigbeeDevicesUnclassifiedResponse maps to v3GetZigbeeDevicesUnclassifiedResponse.
type GetZigbeeDevicesUnclassifiedResponse struct {
	Devices []ZigbeeDeviceSummary `json:"devices,omitempty"`
}

// GetZigbeeDeviceDataResponse maps to apiv3GetZigbeeDeviceDataResponse.
// The Aruba API returns full device data including sensor readings and metadata.
type GetZigbeeDeviceDataResponse struct {
	Mac         string         `json:"mac,omitempty"`
	DeviceID    string         `json:"deviceId,omitempty"`
	Name        string         `json:"name,omitempty"`
	HwModel     string         `json:"hwModel,omitempty"`
	DeviceClass []string       `json:"deviceClass,omitempty"`
	Sensor      map[string]any `json:"sensor,omitempty"`
	DeviceInfo  map[string]any `json:"deviceInfo,omitempty"`
}

// DeviceDataUpdateInfo maps to apiv3DeviceDataUpdateInfo.
type DeviceDataUpdateInfo struct {
	DeviceInfo map[string]any `json:"deviceInfo,omitempty"`
}

// UpdateZigbeeDeviceDataResponse maps to apiv3UpdateZigbeeDeviceDataResponse.
type UpdateZigbeeDeviceDataResponse struct{}

// DeviceClassArray maps to v3UpdateZigbeeDeviceClassRequestDeviceClassArray.
type DeviceClassArray struct {
	DeviceClass []string `json:"deviceClass"`
}

// UpdateZigbeeDeviceClassResponse maps to apiv3UpdateZigbeeDeviceClassResponse.
type UpdateZigbeeDeviceClassResponse struct{}

// DeviceClassScopedData maps to v1DeviceClassScopedData.
type DeviceClassScopedData struct {
	DeviceClass string `json:"deviceClass"`
	DeviceID    string `json:"deviceId,omitempty"`
}

// SetZigbeeDeviceIdResponse maps to v3SetZigbeeDeviceIdResponse.
type SetZigbeeDeviceIdResponse struct{}

// SendZigbeeLeaveResponse maps to v3SendZigbeeLeaveResponse.
type SendZigbeeLeaveResponse struct{}

// OfflineTimeoutData is the body for the offline-timeout endpoint.
type OfflineTimeoutData struct {
	// TimeoutMinutes is in range [1, 1440] (1 minute to 24 hours).
	TimeoutMinutes int `json:"timeoutMinutes"`
}

// SetZigbeeOfflineTimeoutValueResponse maps to v3SetZigbeeOfflineTimeoutValueResponse.
type SetZigbeeOfflineTimeoutValueResponse struct{}

// ZigbeeSouthboundData maps to v3SendZigbeeSouthboundDataRequestSouthboundData.
type ZigbeeSouthboundData struct {
	// Payload is the base64-encoded bytes to transmit to the device.
	Payload []byte `json:"payload"`
	// ProfileID is the Zigbee application profile ID (e.g. 0x0104 for Home Automation).
	ProfileID int `json:"profileId,omitempty"`
	// ClusterID is the Zigbee cluster to address.
	ClusterID int `json:"clusterId,omitempty"`
}

// SendZigbeeSouthboundDataResponse maps to v3SendZigbeeSouthboundDataResponse.
type SendZigbeeSouthboundDataResponse struct{}

// TimeoutValueData maps to v3SetZigbeeTimeoutValueRequestTimeoutData.
type TimeoutValueData struct {
	// Timeout is one of the TIMEOUT_* enum strings defined in the API YAML.
	// Examples: "TIMEOUT_INFINITE", "TIMEOUT_10SEC", "TIMEOUT_2MIN".
	Timeout string `json:"timeout"`
}

// SetZigbeeTimeoutValueResponse maps to v3SetZigbeeTimeoutValueResponse.
type SetZigbeeTimeoutValueResponse struct{}

// ZigbeeRadioSummary is one entry in the radio list.
type ZigbeeRadioSummary struct {
	RadioMac string `json:"radioMac,omitempty"`
	ApMac    string `json:"apMac,omitempty"`
}

// GetZigbeeRadiosResponse maps to v3GetZigbeeRadiosResponse.
type GetZigbeeRadiosResponse struct {
	Radios []ZigbeeRadioSummary `json:"radios,omitempty"`
}

// GetZigbeeRadioInfoResponse maps to v3GetZigbeeRadioInfoResponse.
type GetZigbeeRadioInfoResponse struct {
	RadioMac   string         `json:"radioMac,omitempty"`
	ApMac      string         `json:"apMac,omitempty"`
	RadioState map[string]any `json:"radioState,omitempty"`
}

// ZigbeePacket is one northbound packet in the stream.
type ZigbeePacket struct {
	HardwareID string `json:"hardwareId,omitempty"`
	Payload    []byte `json:"payload,omitempty"`
	Timestamp  string `json:"timestamp,omitempty"`
}

// SubscribeZigbeePacketStreamResponse maps to v3SubscribeZigbeePacketStreamResponse.
// NOTE: The real API uses JSON streaming (chunked HTTP). Our implementation reads
// all available buffered packets and returns them as a slice. For production streaming
// use the SSE/long-poll pattern instead.
type SubscribeZigbeePacketStreamResponse struct {
	Packets []ZigbeePacket `json:"packets,omitempty"`
}

// SendAppInfoRequest maps to apiv3SendAppInfoRequest.
// For this app we send a single human-readable status message.
type SendAppInfoRequest struct {
	Message string `json:"message"`
}

// SendAppInfoResponse maps to apiv3SendAppInfoResponse.
type SendAppInfoResponse struct{}

// ────────────────────────────────────────────────────────────────
// GatewayClient
// ────────────────────────────────────────────────────────────────

// GatewayClient is an HTTP client for the Aruba IoT Operations Partner API.
// All methods proxy to the real gateway identified by cfg.APIGWURL.
type GatewayClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
	logger  *slog.Logger
}

// NewGatewayClient builds a GatewayClient from the supplied configuration.
// A dedicated http.Client is created with the configured timeout so that
// slow gateway responses never block the container indefinitely.
func NewGatewayClient(cfg config.GatewayConfig, timeout time.Duration, logger *slog.Logger) *GatewayClient {
	baseURL := strings.TrimSpace(cfg.APIGWURL)
	if baseURL != "" && !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &GatewayClient{
		// Trim trailing slash so we can always append paths with a leading slash.
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  cfg.APIKey,
		http:    &http.Client{Timeout: timeout},
		logger:  logger,
	}
}

// ────────────────────────────────────────────────────────────────
// Internal helpers
// ────────────────────────────────────────────────────────────────

// do is the single point where every request is signed with the API key and sent.
// method  – HTTP method string (GET, POST, PUT, PATCH, DELETE)
// path    – full path including query string, e.g. "/api/v3/zigbee/devices?fields=DeviceClass"
// body    – optional JSON payload; pass nil for requests with no body
// out     – pointer to the struct to decode the JSON response into; pass nil to skip decoding
func (c *GatewayClient) do(ctx context.Context, method, path string, body any, out any) error {
	requestBody := ""
	var bodyReader io.Reader
	if body != nil {
		// Encode the Go struct to JSON before sending it as the request body.
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		requestBody = string(b)
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	// The Aruba IoT Gateway authenticates via an "apikey" header.
	req.Header.Set("apikey", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	requestHeaders := sanitizeHeaders(req.Header)
	c.logger.Info("gateway_api_request",
		"method", method,
		"url", req.URL.String(),
		"headers", requestHeaders,
		"body", requestBody,
	)

	started := time.Now()

	resp, err := c.http.Do(req)
	if err != nil {
		c.logger.Info("gateway_api_request_failed",
			"method", method,
			"url", req.URL.String(),
			"headers", requestHeaders,
			"body", requestBody,
			"duration_ms", time.Since(started).Milliseconds(),
			"error", err,
		)
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	// Read the response body once, so it can be used for both error inspection
	// and JSON decoding.
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	c.logger.Info("gateway_api_response",
		"method", method,
		"url", req.URL.String(),
		"status_code", resp.StatusCode,
		"headers", sanitizeHeaders(resp.Header),
		"body", strings.TrimSpace(string(respBytes)),
		"duration_ms", time.Since(started).Milliseconds(),
	)

	// Treat any non-2xx status as an error and surface the raw body so callers
	// can inspect the Aruba error message.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gateway returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}

	// If the caller passed a destination struct, decode the JSON into it.
	if out != nil && len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func sanitizeHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, values := range h {
		key := strings.ToLower(k)
		if key == "apikey" || key == "authorization" {
			out[k] = "[REDACTED]"
			continue
		}
		out[k] = strings.Join(values, ",")
	}
	return out
}

// escapePathSegment encodes a URL path segment while preserving colons.
// Go's url.PathEscape encodes ':' as '%3A', but colons are valid unencoded in
// path segments (RFC 3986 §3.3) and the Aruba gateway matches them literally.
// Zigbee MAC addresses (e.g. "aa:bb:cc:dd:ee:ff:11:22") and logical device IDs
// that contain colons would otherwise be unresolvable.
func escapePathSegment(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "%3A", ":")
}

// buildPath constructs a URL path with optional query parameters.
// params entries should come in key, value pairs. Missing values are skipped.
func buildPath(base string, params ...string) string {
	if len(params) == 0 {
		return base
	}

	q := url.Values{}
	for i := 0; i+1 < len(params); i += 2 {
		if params[i+1] != "" {
			q.Set(params[i], params[i+1])
		}
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

// ────────────────────────────────────────────────────────────────
// Zigbee API methods — one per endpoint in /api/v3/zigbee/
// ────────────────────────────────────────────────────────────────

// GetDeviceList calls GET /api/v3/zigbee/devices and returns all known Zigbee devices.
// Required scope: context; permission: readonly.
func (c *GatewayClient) GetDeviceList(ctx context.Context) (*GetZigbeeDeviceListResponse, error) {
	var out GetZigbeeDeviceListResponse
	if err := c.do(ctx, http.MethodGet, "/api/v3/zigbee/devices", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDevicesUnclassified calls GET /api/v3/zigbee/devices-unclassified.
// Returns Zigbee devices that have not yet been assigned a device class.
// Required scope: context; permission: readWrite.
func (c *GatewayClient) GetDevicesUnclassified(ctx context.Context) (*GetZigbeeDevicesUnclassifiedResponse, error) {
	var out GetZigbeeDevicesUnclassifiedResponse
	if err := c.do(ctx, http.MethodGet, "/api/v3/zigbee/devices-unclassified", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDeviceData calls GET /api/v3/zigbee/devices/{id}.
// id      – Zigbee 8-byte MAC (hardware ID) or device ID when idType is ID_TYPE_DEVICE_ID.
// idType  – "" (use hardware ID default) or "ID_TYPE_DEVICE_ID".
// devClass – optional device class filter, used together with idType=ID_TYPE_DEVICE_ID.
// Required scope: context; permission: readonly.
func (c *GatewayClient) GetDeviceData(ctx context.Context, id, idType, devClass string) (*GetZigbeeDeviceDataResponse, error) {
	path := buildPath("/api/v3/zigbee/devices/"+escapePathSegment(id), "idType", idType, "deviceClass", devClass)
	var out GetZigbeeDeviceDataResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateDeviceDataPut calls PUT /api/v3/zigbee/devices/{id}/data.
// Replaces the device info. Required scope: context; permission: readwrite.
func (c *GatewayClient) UpdateDeviceDataPut(ctx context.Context, id, idType, devClass string, update DeviceDataUpdateInfo) (*UpdateZigbeeDeviceDataResponse, error) {
	path := buildPath("/api/v3/zigbee/devices/"+escapePathSegment(id)+"/data", "idType", idType, "deviceClass", devClass)
	var out UpdateZigbeeDeviceDataResponse
	if err := c.do(ctx, http.MethodPut, path, update, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateDeviceDataPatch calls PATCH /api/v3/zigbee/devices/{id}/data.
// Partially updates the device info. Required scope: context; permission: readwrite.
func (c *GatewayClient) UpdateDeviceDataPatch(ctx context.Context, id, idType, devClass string, update DeviceDataUpdateInfo) (*UpdateZigbeeDeviceDataResponse, error) {
	path := buildPath("/api/v3/zigbee/devices/"+escapePathSegment(id)+"/data", "idType", idType, "deviceClass", devClass)
	var out UpdateZigbeeDeviceDataResponse
	if err := c.do(ctx, http.MethodPatch, path, update, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateDeviceClass calls PUT /api/v3/zigbee/devices/{id}/deviceclass.
// Assigns or updates the device class labels for a Zigbee device.
// Required scope: context; permission: classify.
func (c *GatewayClient) UpdateDeviceClass(ctx context.Context, id string, body DeviceClassArray) (*UpdateZigbeeDeviceClassResponse, error) {
	path := "/api/v3/zigbee/devices/" + escapePathSegment(id) + "/deviceclass"
	var out UpdateZigbeeDeviceClassResponse
	if err := c.do(ctx, http.MethodPut, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetDeviceID calls PUT /api/v3/zigbee/devices/{id}/deviceid.
// Assigns a logical device ID scoped to a device class.
// Required scope: context; permission: readwrite.
func (c *GatewayClient) SetDeviceID(ctx context.Context, id, devClass string, body DeviceClassScopedData) (*SetZigbeeDeviceIdResponse, error) {
	path := buildPath("/api/v3/zigbee/devices/"+escapePathSegment(id)+"/deviceid", "deviceClass", devClass)
	var out SetZigbeeDeviceIdResponse
	if err := c.do(ctx, http.MethodPut, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendLeave calls PUT /api/v3/zigbee/devices/{id}/leave.
// Sends a Zigbee "leave" command to unpair the device from the network.
// Required scope: zigbee; permission: readwrite.
func (c *GatewayClient) SendLeave(ctx context.Context, id string) (*SendZigbeeLeaveResponse, error) {
	path := "/api/v3/zigbee/devices/" + escapePathSegment(id) + "/leave"
	var out SendZigbeeLeaveResponse
	// The leave endpoint takes no request body.
	if err := c.do(ctx, http.MethodPut, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetOfflineTimeout calls PUT /api/v3/zigbee/devices/{id}/offline-timeout.
// Sets how many minutes the gateway waits before marking a device offline (range: 1–1440).
func (c *GatewayClient) SetOfflineTimeout(ctx context.Context, id, idType, devClass string, body OfflineTimeoutData) (*SetZigbeeOfflineTimeoutValueResponse, error) {
	path := buildPath(
		"/api/v3/zigbee/devices/"+escapePathSegment(id)+"/offline-timeout",
		"idType", idType, "deviceClass", devClass,
	)
	var out SetZigbeeOfflineTimeoutValueResponse
	if err := c.do(ctx, http.MethodPut, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendSouthboundData calls POST /api/v3/zigbee/devices/{id}/packets.
// Sends raw downlink (southbound) data to the Zigbee device.
// Required scope: zigbee; permission: readwrite.
func (c *GatewayClient) SendSouthboundData(ctx context.Context, id, idType, devClass string, body ZigbeeSouthboundData) (*SendZigbeeSouthboundDataResponse, error) {
	path := buildPath(
		"/api/v3/zigbee/devices/"+escapePathSegment(id)+"/packets",
		"idType", idType, "deviceClass", devClass,
	)
	var out SendZigbeeSouthboundDataResponse
	if err := c.do(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetTimeoutValue calls PUT /api/v3/zigbee/devices/{id}/timeout.
// Sets the Zigbee device keep-alive timeout using one of the TIMEOUT_* enum values.
func (c *GatewayClient) SetTimeoutValue(ctx context.Context, id, idType, devClass string, body TimeoutValueData) (*SetZigbeeTimeoutValueResponse, error) {
	path := buildPath(
		"/api/v3/zigbee/devices/"+escapePathSegment(id)+"/timeout",
		"idType", idType, "deviceClass", devClass,
	)
	var out SetZigbeeTimeoutValueResponse
	if err := c.do(ctx, http.MethodPut, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRadios calls GET /api/v3/zigbee/radios.
// Returns all Zigbee radios that have at least one associated device.
// Required scope: context; permission: readonly.
func (c *GatewayClient) GetRadios(ctx context.Context) (*GetZigbeeRadiosResponse, error) {
	var out GetZigbeeRadiosResponse
	if err := c.do(ctx, http.MethodGet, "/api/v3/zigbee/radios", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRadioInfo calls GET /api/v3/zigbee/radios/{radioMac}.
// Returns detailed state for a single Zigbee radio.
// Required scope: context; permission: readonly.
func (c *GatewayClient) GetRadioInfo(ctx context.Context, radioMac string) (*GetZigbeeRadioInfoResponse, error) {
	path := "/api/v3/zigbee/radios/" + escapePathSegment(radioMac)
	var out GetZigbeeRadioInfoResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SubscribePacketStream calls GET /api/v3/zigbee/stream/packets.
// The real endpoint is a JSON streaming (chunked transfer) response.
// This implementation buffers one batch of available data and returns it.
// fields is an optional list of extra field names to include (e.g. "DeviceClass").
func (c *GatewayClient) SubscribePacketStream(ctx context.Context, fields []string) (*SubscribeZigbeePacketStreamResponse, error) {
	q := url.Values{}
	for _, f := range fields {
		if f != "" {
			q.Add("fields", f)
		}
	}

	path := "/api/v3/zigbee/stream/packets"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	// This endpoint is a long-lived/chunked stream. Avoid io.ReadAll-style buffering
	// to prevent unbounded memory growth and OOM kills.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("apikey", c.apiKey)
	req.Header.Set("Accept", "application/json")

	requestHeaders := sanitizeHeaders(req.Header)
	c.logger.Info("gateway_api_request",
		"method", http.MethodGet,
		"url", req.URL.String(),
		"headers", requestHeaders,
		"body", "",
	)

	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.logger.Info("gateway_api_request_failed",
			"method", http.MethodGet,
			"url", req.URL.String(),
			"headers", requestHeaders,
			"body", "",
			"duration_ms", time.Since(started).Milliseconds(),
			"error", err,
		)
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		errBody := strings.TrimSpace(string(errBodyBytes))
		c.logger.Info("gateway_api_response",
			"method", http.MethodGet,
			"url", req.URL.String(),
			"status_code", resp.StatusCode,
			"headers", sanitizeHeaders(resp.Header),
			"body", errBody,
			"duration_ms", time.Since(started).Milliseconds(),
		)
		return nil, fmt.Errorf("gateway returned %d: %s", resp.StatusCode, errBody)
	}

	c.logger.Info("gateway_api_response",
		"method", http.MethodGet,
		"url", req.URL.String(),
		"status_code", resp.StatusCode,
		"headers", sanitizeHeaders(resp.Header),
		"body", "[stream body omitted to avoid buffering]",
		"duration_ms", time.Since(started).Milliseconds(),
	)

	// We intentionally do not read/parse the stream body here.
	return &SubscribeZigbeePacketStreamResponse{Packets: nil}, nil
}

// SendAppInfo calls POST /api/v3/apps/info.
// It reports app status metadata to the Aruba IoT Gateway.
// Required scope: context; permission: readonly.
func (c *GatewayClient) SendAppInfo(ctx context.Context, message string) (*SendAppInfoResponse, error) {
	body := SendAppInfoRequest{Message: message}
	var out SendAppInfoResponse
	if err := c.do(ctx, http.MethodPost, "/api/v3/apps/info", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Ping does a lightweight HTTP GET to the gateway base URL to check reachability.
// It returns the HTTP status code and any transport-level error.
// A response of any status code (even 401/403) means the gateway is reachable.
func (c *GatewayClient) Ping(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return 0, fmt.Errorf("build ping request: %w", err)
	}
	req.Header.Set("apikey", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("gateway unreachable: %w", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}
