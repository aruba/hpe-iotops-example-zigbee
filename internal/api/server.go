package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"zigbee-demo-app/pkg/config"
	"zigbee-demo-app/pkg/zigbee"
)

// Server owns HTTP routes and links them to the device store and Zigbee gateway client.
type Server struct {
	logger  *slog.Logger
	store   *zigbee.Store
	gateway *zigbee.GatewayClient // nil when gateway is not configured
	http    *http.Server
}

// statusWriter wraps the default response writer so middleware can capture
// which status code was returned by a handler.
type statusWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader stores the response status and forwards the call to the real writer.
func (s *statusWriter) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

// NewServer configures routes and returns a ready-to-start HTTP server instance.
// gateway may be nil when APIGW_URL is not set; Zigbee proxy routes will return 503.
func NewServer(cfg config.ServerConfig, logger *slog.Logger, store *zigbee.Store, gateway *zigbee.GatewayClient) *Server {
	s := &Server{logger: logger, store: store, gateway: gateway}

	// Standard library ServeMux is enough for this API size and keeps dependencies low.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/devices", s.handleDevices)
	mux.HandleFunc("/devices/", s.handleDeviceRoutes)
	// All Aruba IoT Gateway Zigbee API endpoints are routed here.
	mux.HandleFunc("/api/v3/zigbee/", s.routeZigbee)
	mux.HandleFunc("/api/v3/zigbee/devices", s.routeZigbee)

	s.http = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           s.loggingMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	return s
}

// Start begins listening and serving HTTP requests.
func (s *Server) Start() error {
	return s.http.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// loggingMiddleware records one log event per HTTP request.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.logger.Info("http_request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// handleHealth is used by load balancers and container orchestrators.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	respondJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleDevices returns all known devices.
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"devices": s.store.List()})
}

// handleDeviceRoutes dispatches dynamic device routes like:
//   - /devices/{id}
//   - /devices/{id}/command
//   - /devices/{id}/telemetry
func (s *Server) handleDeviceRoutes(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/devices/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 || parts[0] == "" {
		respondError(w, http.StatusNotFound, "device id is required")
		return
	}

	deviceID := parts[0]
	if len(parts) == 1 {
		s.handleGetDevice(w, r, deviceID)
		return
	}

	if len(parts) == 2 && parts[1] == "command" {
		s.handleDeviceCommand(w, r, deviceID)
		return
	}

	if len(parts) == 2 && parts[1] == "telemetry" {
		s.handleDeviceTelemetry(w, r, deviceID)
		return
	}

	respondError(w, http.StatusNotFound, "route not found")
}

// handleGetDevice returns one device by ID.
func (s *Server) handleGetDevice(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	d, ok := s.store.Get(deviceID)
	if !ok {
		respondError(w, http.StatusNotFound, "device not found")
		return
	}
	respondJSON(w, http.StatusOK, d)
}

// handleDeviceTelemetry returns only telemetry-relevant fields.
func (s *Server) handleDeviceTelemetry(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	d, ok := s.store.Get(deviceID)
	if !ok {
		respondError(w, http.StatusNotFound, "device not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"device_id":    d.ID,
		"data":         d.Data,
		"last_updated": d.LastUpdated,
	})
}

// handleDeviceCommand decodes JSON command input and applies it to one device.
func (s *Server) handleDeviceCommand(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var cmd zigbee.Command
	// Decode body into strongly-typed struct to validate expected fields.
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	if cmd.Params == nil {
		cmd.Params = map[string]any{}
	}

	d, err := s.store.ExecuteCommand(deviceID, cmd)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, d)
}

// respondJSON writes a JSON payload with the given HTTP status code.
func respondJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// respondError is a small helper for JSON error responses.
func respondError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, map[string]any{
		"error": message,
	})
}
