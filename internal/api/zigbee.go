package api

// zigbee.go contains HTTP handlers for all Aruba IoT Gateway Zigbee endpoints
// reachable under /api/v3/zigbee/.
//
// How the routing works:
//   - The mux pattern "/api/v3/zigbee/" matches everything that starts with that prefix.
//   - routeZigbee() splits the remaining path segments and dispatches to the right handler.
//   - Each handler decodes its request, calls the GatewayClient, and forwards the response.
//
// Reference: apidefinitionsiot.yaml — paths starting with "/api/v3/zigbee/"

import (
	"encoding/json"
	"net/http"
	"strings"

	"zigbee-demo-app/pkg/zigbee"
)

// routeZigbee dispatches all /api/v3/zigbee/* requests.
// URL structure after stripping the prefix:
//
//	""                          → list devices          (GET)
//	"devices-unclassified"      → unclassified devices  (GET)
//	"radios"                    → list radios            (GET)
//	"radios/{mac}"              → radio detail           (GET)
//	"stream/packets"            → packet stream          (GET)
//	"devices/{id}"              → device data            (GET)
//	"devices/{id}/data"         → update data            (PUT/PATCH)
//	"devices/{id}/deviceclass"  → update class           (PUT)
//	"devices/{id}/deviceid"     → set device ID          (PUT)
//	"devices/{id}/leave"        → send leave             (PUT)
//	"devices/{id}/offline-timeout" → set offline timeout (PUT)
//	"devices/{id}/packets"      → send southbound data   (POST)
//	"devices/{id}/timeout"      → set timeout            (PUT)
func (s *Server) routeZigbee(w http.ResponseWriter, r *http.Request) {
	// Strip the base prefix to get the relative sub-path.
	rel := strings.TrimPrefix(r.URL.Path, "/api/v3/zigbee")
	rel = strings.TrimPrefix(rel, "/")
	// parts[0] is the first segment after "/api/v3/zigbee/".
	parts := splitPath(rel)

	// The gateway client must be configured for any of these calls to succeed.
	if s.gateway == nil {
		respondError(w, http.StatusServiceUnavailable, "zigbee gateway is not configured (set APIGW_URL and APIKEY)")
		return
	}

	switch {
	// GET /api/v3/zigbee/devices
	case len(parts) == 1 && parts[0] == "devices" && r.Method == http.MethodGet:
		s.handleZigbeeGetDeviceList(w, r)

	// GET /api/v3/zigbee/devices-unclassified
	case len(parts) == 1 && parts[0] == "devices-unclassified" && r.Method == http.MethodGet:
		s.handleZigbeeGetDevicesUnclassified(w, r)

	// GET /api/v3/zigbee/radios
	case len(parts) == 1 && parts[0] == "radios" && r.Method == http.MethodGet:
		s.handleZigbeeGetRadios(w, r)

	// GET /api/v3/zigbee/radios/{radioMac}
	case len(parts) == 2 && parts[0] == "radios" && r.Method == http.MethodGet:
		s.handleZigbeeGetRadioInfo(w, r, parts[1])

	// GET /api/v3/zigbee/stream/packets
	case len(parts) == 2 && parts[0] == "stream" && parts[1] == "packets" && r.Method == http.MethodGet:
		s.handleZigbeeSubscribePacketStream(w, r)

	// /api/v3/zigbee/devices/{id} and sub-routes
	case len(parts) >= 2 && parts[0] == "devices":
		s.routeZigbeeDevice(w, r, parts[1:])

	default:
		respondError(w, http.StatusNotFound, "route not found")
	}
}

// routeZigbeeDevice handles routes under /api/v3/zigbee/devices/{id}/...
// parts[0] is the device id; parts[1] (if present) is the sub-resource name.
func (s *Server) routeZigbeeDevice(w http.ResponseWriter, r *http.Request, parts []string) {
	id := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	switch sub {
	case "":
		// GET /api/v3/zigbee/devices/{id}
		if r.Method != http.MethodGet {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleZigbeeGetDeviceData(w, r, id)

	case "data":
		// PUT or PATCH /api/v3/zigbee/devices/{id}/data
		switch r.Method {
		case http.MethodPut:
			s.handleZigbeeUpdateDeviceDataPut(w, r, id)
		case http.MethodPatch:
			s.handleZigbeeUpdateDeviceDataPatch(w, r, id)
		default:
			respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	case "deviceclass":
		// PUT /api/v3/zigbee/devices/{id}/deviceclass
		if r.Method != http.MethodPut {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleZigbeeUpdateDeviceClass(w, r, id)

	case "deviceid":
		// PUT /api/v3/zigbee/devices/{id}/deviceid
		if r.Method != http.MethodPut {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleZigbeeSetDeviceID(w, r, id)

	case "leave":
		// PUT /api/v3/zigbee/devices/{id}/leave
		if r.Method != http.MethodPut {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleZigbeeSendLeave(w, r, id)

	case "offline-timeout":
		// PUT /api/v3/zigbee/devices/{id}/offline-timeout
		if r.Method != http.MethodPut {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleZigbeeSetOfflineTimeout(w, r, id)

	case "packets":
		// POST /api/v3/zigbee/devices/{id}/packets
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleZigbeeSendSouthboundData(w, r, id)

	case "timeout":
		// PUT /api/v3/zigbee/devices/{id}/timeout
		if r.Method != http.MethodPut {
			respondError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleZigbeeSetTimeoutValue(w, r, id)

	default:
		respondError(w, http.StatusNotFound, "route not found")
	}
}

// ────────────────────────────────────────────────────────────────
// Individual handlers
// ────────────────────────────────────────────────────────────────

// handleZigbeeGetDeviceList handles GET /api/v3/zigbee/devices
func (s *Server) handleZigbeeGetDeviceList(w http.ResponseWriter, r *http.Request) {
	result, err := s.gateway.GetDeviceList(r.Context())
	if err != nil {
		s.logger.Warn("zigbee_get_device_list_failed", "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// handleZigbeeGetDevicesUnclassified handles GET /api/v3/zigbee/devices-unclassified
func (s *Server) handleZigbeeGetDevicesUnclassified(w http.ResponseWriter, r *http.Request) {
	result, err := s.gateway.GetDevicesUnclassified(r.Context())
	if err != nil {
		s.logger.Warn("zigbee_get_devices_unclassified_failed", "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// handleZigbeeGetDeviceData handles GET /api/v3/zigbee/devices/{id}
func (s *Server) handleZigbeeGetDeviceData(w http.ResponseWriter, r *http.Request, id string) {
	q := r.URL.Query()
	result, err := s.gateway.GetDeviceData(r.Context(), id, q.Get("idType"), q.Get("deviceClass"))
	if err != nil {
		s.logger.Warn("zigbee_get_device_data_failed", "id", id, "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// handleZigbeeUpdateDeviceDataPut handles PUT /api/v3/zigbee/devices/{id}/data
func (s *Server) handleZigbeeUpdateDeviceDataPut(w http.ResponseWriter, r *http.Request, id string) {
	var body zigbee.DeviceDataUpdateInfo
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	q := r.URL.Query()
	result, err := s.gateway.UpdateDeviceDataPut(r.Context(), id, q.Get("idType"), q.Get("deviceClass"), body)
	if err != nil {
		s.logger.Warn("zigbee_update_device_data_put_failed", "id", id, "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// handleZigbeeUpdateDeviceDataPatch handles PATCH /api/v3/zigbee/devices/{id}/data
func (s *Server) handleZigbeeUpdateDeviceDataPatch(w http.ResponseWriter, r *http.Request, id string) {
	var body zigbee.DeviceDataUpdateInfo
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	q := r.URL.Query()
	result, err := s.gateway.UpdateDeviceDataPatch(r.Context(), id, q.Get("idType"), q.Get("deviceClass"), body)
	if err != nil {
		s.logger.Warn("zigbee_update_device_data_patch_failed", "id", id, "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// handleZigbeeUpdateDeviceClass handles PUT /api/v3/zigbee/devices/{id}/deviceclass
func (s *Server) handleZigbeeUpdateDeviceClass(w http.ResponseWriter, r *http.Request, id string) {
	var body zigbee.DeviceClassArray
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	result, err := s.gateway.UpdateDeviceClass(r.Context(), id, body)
	if err != nil {
		s.logger.Warn("zigbee_update_device_class_failed", "id", id, "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// handleZigbeeSetDeviceID handles PUT /api/v3/zigbee/devices/{id}/deviceid
func (s *Server) handleZigbeeSetDeviceID(w http.ResponseWriter, r *http.Request, id string) {
	var body zigbee.DeviceClassScopedData
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	result, err := s.gateway.SetDeviceID(r.Context(), id, r.URL.Query().Get("deviceClass"), body)
	if err != nil {
		s.logger.Warn("zigbee_set_device_id_failed", "id", id, "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// handleZigbeeSendLeave handles PUT /api/v3/zigbee/devices/{id}/leave
func (s *Server) handleZigbeeSendLeave(w http.ResponseWriter, r *http.Request, id string) {
	result, err := s.gateway.SendLeave(r.Context(), id)
	if err != nil {
		s.logger.Warn("zigbee_send_leave_failed", "id", id, "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// handleZigbeeSetOfflineTimeout handles PUT /api/v3/zigbee/devices/{id}/offline-timeout
func (s *Server) handleZigbeeSetOfflineTimeout(w http.ResponseWriter, r *http.Request, id string) {
	var body zigbee.OfflineTimeoutData
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	if body.TimeoutMinutes < 1 || body.TimeoutMinutes > 1440 {
		respondError(w, http.StatusBadRequest, "timeoutMinutes must be between 1 and 1440")
		return
	}
	q := r.URL.Query()
	result, err := s.gateway.SetOfflineTimeout(r.Context(), id, q.Get("idType"), q.Get("deviceClass"), body)
	if err != nil {
		s.logger.Warn("zigbee_set_offline_timeout_failed", "id", id, "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// handleZigbeeSendSouthboundData handles POST /api/v3/zigbee/devices/{id}/packets
func (s *Server) handleZigbeeSendSouthboundData(w http.ResponseWriter, r *http.Request, id string) {
	var body zigbee.ZigbeeSouthboundData
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	q := r.URL.Query()
	result, err := s.gateway.SendSouthboundData(r.Context(), id, q.Get("idType"), q.Get("deviceClass"), body)
	if err != nil {
		s.logger.Warn("zigbee_send_southbound_data_failed", "id", id, "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// handleZigbeeSetTimeoutValue handles PUT /api/v3/zigbee/devices/{id}/timeout
func (s *Server) handleZigbeeSetTimeoutValue(w http.ResponseWriter, r *http.Request, id string) {
	var body zigbee.TimeoutValueData
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	q := r.URL.Query()
	result, err := s.gateway.SetTimeoutValue(r.Context(), id, q.Get("idType"), q.Get("deviceClass"), body)
	if err != nil {
		s.logger.Warn("zigbee_set_timeout_value_failed", "id", id, "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// handleZigbeeGetRadios handles GET /api/v3/zigbee/radios
func (s *Server) handleZigbeeGetRadios(w http.ResponseWriter, r *http.Request) {
	result, err := s.gateway.GetRadios(r.Context())
	if err != nil {
		s.logger.Warn("zigbee_get_radios_failed", "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// handleZigbeeGetRadioInfo handles GET /api/v3/zigbee/radios/{radioMac}
func (s *Server) handleZigbeeGetRadioInfo(w http.ResponseWriter, r *http.Request, radioMac string) {
	result, err := s.gateway.GetRadioInfo(r.Context(), radioMac)
	if err != nil {
		s.logger.Warn("zigbee_get_radio_info_failed", "radioMac", radioMac, "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// handleZigbeeSubscribePacketStream handles GET /api/v3/zigbee/stream/packets
func (s *Server) handleZigbeeSubscribePacketStream(w http.ResponseWriter, r *http.Request) {
	// The "fields" query parameter can appear multiple times (multi-value).
	fields := r.URL.Query()["fields"]
	result, err := s.gateway.SubscribePacketStream(r.Context(), fields)
	if err != nil {
		s.logger.Warn("zigbee_subscribe_packet_stream_failed", "error", err)
		respondGatewayError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// ────────────────────────────────────────────────────────────────
// Helpers
// ────────────────────────────────────────────────────────────────

// splitPath splits a URL path by "/" and removes empty segments.
func splitPath(p string) []string {
	raw := strings.Split(p, "/")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// respondGatewayError inspects the error string to forward the correct HTTP status.
// The Aruba gateway includes the upstream status code in the error message returned
// by GatewayClient.do. We surface the same code to our caller for transparency.
func respondGatewayError(w http.ResponseWriter, err error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "400"):
		respondError(w, http.StatusBadRequest, msg)
	case strings.Contains(msg, "401"):
		respondError(w, http.StatusUnauthorized, msg)
	case strings.Contains(msg, "403"):
		respondError(w, http.StatusForbidden, msg)
	case strings.Contains(msg, "404"):
		respondError(w, http.StatusNotFound, msg)
	case strings.Contains(msg, "409"):
		respondError(w, http.StatusConflict, msg)
	case strings.Contains(msg, "503"):
		respondError(w, http.StatusServiceUnavailable, msg)
	case strings.Contains(msg, "504"):
		respondError(w, http.StatusGatewayTimeout, msg)
	default:
		respondError(w, http.StatusInternalServerError, msg)
	}
}
