# Zigbee Demo IoT App (Go)

Containerized IoT app with:

- REST API for local Zigbee device state interactions
- MQTT publish + subscribe for telemetry and commands
- Structured JSON logs to stdout
- Periodic app status log every 60 seconds

## Prerequisites

1. Follow the instructions in the App Developer Portal documentation and the Aruba Central documentation to set up your IoT Connectors, radio and Zigbee service profile configurations, Zigbee end devices, and access points (Zigbee Coordinator).  
  
2. Set up your MQTT endpoint server, topics, and related configuration (optional).

## General Code Flow

This section explains what happens from startup to shutdown in simple steps.

1. Startup entrypoint (`cmd/iot-app/main.go`)

- `main()` calls `run()`.
- `run()` loads config from defaults, optional `config.yaml`, and environment variable overrides.
- A shared JSON logger is created.
- All environment variables are logged once at startup for container troubleshooting.
 If gateway is enabled, startup performs:
 - Connectivity ping to the gateway (`/health` or equivalent).
 - If connectivity fails, remaining gateway startup substeps are skipped and logged as `gateway_startup_steps_skipped`.
 - If connectivity succeeds:
   1. `zigbee_get_devices` — lists all currently known Zigbee devices.
   2. `zigbee_get_devices_unclassified` — lists devices that have not yet been assigned a class.
   3. Build a deduplicated discovered device list from both responses.
   4. For each discovered device:
     - `UpdateDeviceClass` with `device_class=["aqara"]`.
     - `SetDeviceID` with class `aqara` and ID format `<mac>_chinmay_<random>`.
   5. If at least one device succeeds in both steps, call `zigbee_subscribe_packet_stream`.
   6. Start `appInfoReporter` goroutine.

2. Shared app state (`pkg/zigbee/device.go`)

- The app creates an in-memory device store.
- No dummy devices are preloaded by default.
- If `devices.json` already exists, previously saved devices are loaded from disk.
- Handlers and MQTT both use this same store, so API actions and MQTT commands affect the same device state.

3. MQTT setup (`pkg/mqtt/client.go`)

- If MQTT is enabled, client connects to broker.
- On connect, it subscribes to the southbound topic (default `iot/zigbee/southbound`).
- Received messages are decoded and routed to the corresponding Aruba Zigbee API call.
- Responses are published to the northbound topic (default `iot/zigbee/northbound`).

4. HTTP API setup (`internal/api/server.go`)

- HTTP routes are registered:
  - `GET /health`
  - `GET /devices`
  - `GET /devices/{id}`
  - `GET /devices/{id}/telemetry`
  - `POST /devices/{id}/command`
- Each request is logged by middleware with method, path, status, and latency.

5. Status heartbeat (`cmd/iot-app/main.go`)

- A background goroutine logs `status_report` every 60s (configurable).
- Status includes uptime, MQTT connected state, last telemetry publish time, goroutine count, and current devices.

6. Keep-running behavior

- The HTTP server runs until process shutdown.
- Even with no HTTP traffic and no MQTT messages, the app keeps running and emits periodic status logs.

7. Graceful shutdown

- On `SIGINT` or `SIGTERM`, the root context is cancelled.
- MQTT client disconnects gracefully (publishes an in-flight drain, then disconnects).
- HTTP server gets up to 10 seconds to finish in-flight requests before shutting down.
- No explicit device de-registration is performed on shutdown.

## Environment Variables

All MQTT server details are configured via environment variables. Pass them with `-e` when using Docker or export them in your shell for local runs.

### MQTT (required for broker connectivity)

| Variable | Required | Default | Description |
|---|---|---|---|
| `MQTT_SERVER_URL` | **Yes** | _(none)_ | MQTT broker address, e.g. `tcp://192.168.1.44:1883` |
| `MQTT_CLIENT_ID` | No | `zigbee-demo-app` | Client identifier sent to the broker |
| `MQTT_USERNAME` | No | _(none)_ | Broker username (if authentication is enabled) |
| `MQTT_PASSWORD` | No | _(none)_ | Broker password (if authentication is enabled) |
| `MQTT_SOUTHBOUND_TOPIC` | No | `iot/zigbee/southbound` | Topic the app **subscribes** to for incoming actions |
| `MQTT_NORTHBOUND_TOPIC` | No | `iot/zigbee/northbound` | Topic the app **publishes** responses and status to |
| `MQTT_QOS` | No | `1` | MQTT QoS level: `0`, `1`, or `2` |
| `MQTT_ENABLED` | No | `true` | Set to `false` to disable MQTT entirely |

> **Note:** If `MQTT_SERVER_URL` is not set the MQTT client is automatically disabled and no broker connection is attempted.
> **Exponential Backoff:** Connection retries to the broker use exponential backoff starting at 1 second, doubling on each retry, with a maximum interval of 30 minutes.

### Aruba IoT Gateway (required for Zigbee API proxy)

| Variable | Required | Default | Description |
|---|---|---|---|
| `APIGW_URL` | **Yes** | _(none)_ | API gateway base URL, e.g. `http://apigw.iot-gateway.svc` |
| `APIKEY` | **Yes** | _(none)_ | Bearer API key injected by the platform |
| `GATEWAY_ID` | No | _(none)_ | Gateway / data-collector unique identifier |
| `GATEWAY_IP` | No | _(none)_ | IP address of the data collector |
| `GATEWAY_NAME` | No | _(none)_ | Human-readable name of the data collector |
| `GATEWAY_REQUEST_TIMEOUT_SECONDS` | No | `30` | HTTP timeout for gateway API calls |

> **Note:** If `APIGW_URL` is provided without protocol, the app prepends `http://` automatically.
> **Exponential Backoff:** Startup connectivity to the gateway uses exponential backoff starting at 1 second, doubling on each retry, with a maximum interval of 30 minutes.

### App / Server

| Variable | Default | Description |
|---|---|---|
| `SERVER_HOST` | `0.0.0.0` | HTTP bind address |
| `SERVER_PORT` | `8080` | HTTP bind port |
| `LOG_LEVEL` | `INFO` | Log level. Accepted values: `DEBUG`, `INFO`, `WARN`, `ERROR` (invalid values default to `INFO`) |
| `APP_STATUS_INTERVAL_SECONDS` | `60` | How often the status heartbeat log is emitted |

`LOG_LEVEL` behavior:

- `DEBUG`: Includes everything.
- `INFO`: Includes `INFO`, `WARN`, and `ERROR` logs. Gateway API request/response logging is included at this level (`gateway_api_request`, `gateway_api_response`, `gateway_api_request_failed`) with URL, headers (secrets redacted), request body, response body, status, and duration.
- `WARN`: Includes `WARN` and `ERROR` logs.
- `ERROR`: Includes only `ERROR` logs.

## Run locally

```bash
go mod tidy
export MQTT_SERVER_URL=tcp://localhost:1883
go run ./cmd/iot-app
```

## Run with Docker

```bash
docker build -t zigbee-demo-app .
docker run --rm -p 8080:8080 \
  -e MQTT_SERVER_URL=tcp://host.docker.internal:1883 \
  -e MQTT_CLIENT_ID=zigbee-demo-app \
  -e MQTT_USERNAME=myuser \
  -e MQTT_PASSWORD=mypassword \
  zigbee-demo-app
```

## REST API

```bash
curl http://localhost:8080/health
curl http://localhost:8080/devices
curl http://localhost:8080/devices/<device_id>
curl http://localhost:8080/devices/<device_id>/telemetry
curl -X POST http://localhost:8080/devices/<device_id>/command \
  -H 'Content-Type: application/json' \
  -d '{"command":"read_sensor","params":{}}'
```

## MQTT Topics

The app uses exactly two MQTT topics:

| Direction | Topic (default) | Env var | Purpose |
|---|---|---|---|
| **Inbound** | `iot/zigbee/southbound` | `MQTT_SOUTHBOUND_TOPIC` | Receive action requests; every message triggers a gateway API call |
| **Outbound** | `iot/zigbee/northbound` | `MQTT_NORTHBOUND_TOPIC` | Publish action responses and periodic app status |

## MQTT Zigbee Action Payloads

Publish JSON to the southbound topic (`iot/zigbee/southbound` by default). The app decodes the message, calls the corresponding Aruba IoT Gateway API, and publishes the response to the northbound topic (`iot/zigbee/northbound` by default).

- `request_id` is optional but strongly recommended for correlating responses.
- All actions require gateway config (`APIGW_URL` and `APIKEY`) to be set.

### Action → API mapping

| `action` value | HTTP method | Gateway API path |
|---|---|---|
| `zigbee_get_devices` | `GET` | `/api/v3/zigbee/devices` |
| `zigbee_get_devices_unclassified` | `GET` | `/api/v3/zigbee/devices-unclassified` |
| `zigbee_get_device_data` | `GET` | `/api/v3/zigbee/devices/{device_id}` |
| `zigbee_update_device_data_put` | `PUT` | `/api/v3/zigbee/devices/{device_id}/data` |
| `zigbee_update_device_data_patch` | `PATCH` | `/api/v3/zigbee/devices/{device_id}/data` |
| `zigbee_set_device_class` | `PUT` | `/api/v3/zigbee/devices/{device_id}/deviceclass` |
| `zigbee_set_device_id` | `PUT` | `/api/v3/zigbee/devices/{device_id}/deviceid` |
| `zigbee_send_leave` | `PUT` | `/api/v3/zigbee/devices/{device_id}/leave` |
| `zigbee_set_offline_timeout` | `PUT` | `/api/v3/zigbee/devices/{device_id}/offline-timeout` |
| `zigbee_send_southbound_data` | `POST` | `/api/v3/zigbee/devices/{device_id}/packets` |
| `zigbee_set_timeout` | `PUT` | `/api/v3/zigbee/devices/{device_id}/timeout` |
| `zigbee_get_radios` | `GET` | `/api/v3/zigbee/radios` |
| `zigbee_get_radio_info` | `GET` | `/api/v3/zigbee/radios/{radio_mac}` |
| `zigbee_subscribe_packet_stream` | `GET` | `/api/v3/zigbee/stream/packets` |
| `app_send_info` | `POST` | `/api/v3/apps/info` |

### 1) zigbee_get_devices

```json
{
  "request_id": "req-zb-get-devices-001",
  "action": "zigbee_get_devices"
}
```

### 2) zigbee_get_devices_unclassified

```json
{
  "request_id": "req-zb-get-unclassified-001",
  "action": "zigbee_get_devices_unclassified"
}
```

### 3) zigbee_get_device_data

```json
{
  "request_id": "req-zb-get-device-data-001",
  "action": "zigbee_get_device_data",
  "device_id": "aa:bb:cc:dd:ee:ff:11:22",
  "id_type": "ID_TYPE_HARDWARE_ID",
  "params": {
    "device_class": "sensor.temperature"
  }
}
```

### 4) zigbee_update_device_data_put

```json
{
  "request_id": "req-zb-update-put-001",
  "action": "zigbee_update_device_data_put",
  "device_id": "aa:bb:cc:dd:ee:ff:11:22",
  "id_type": "ID_TYPE_HARDWARE_ID",
  "params": {
    "device_class": "sensor.temperature",
    "update_info": {
      "deviceInfo": {
        "name": "Warehouse Sensor 1",
        "location": "Zone-A"
      }
    }
  }
}
```

### 5) zigbee_update_device_data_patch

```json
{
  "request_id": "req-zb-update-patch-001",
  "action": "zigbee_update_device_data_patch",
  "device_id": "aa:bb:cc:dd:ee:ff:11:22",
  "id_type": "ID_TYPE_HARDWARE_ID",
  "params": {
    "device_class": "sensor.temperature",
    "update_info": {
      "deviceInfo": {
        "note": "calibrated"
      }
    }
  }
}
```

### 6) zigbee_set_device_class

```json
{
  "request_id": "req-zb-set-class-001",
  "action": "zigbee_set_device_class",
  "device_id": "aa:bb:cc:dd:ee:ff:11:22",
  "device_class": ["sensor.temperature"]
}
```

### 7) zigbee_set_device_id

```json
{
  "request_id": "req-zb-set-device-id-001",
  "action": "zigbee_set_device_id",
  "device_id": "aa:bb:cc:dd:ee:ff:11:22",
  "params": {
    "device_class": "sensor.temperature",
    "target_device_id": "temp-sensor-01"
  }
}
```

### 8) zigbee_send_leave

```json
{
  "request_id": "req-zb-send-leave-001",
  "action": "zigbee_send_leave",
  "device_id": "aa:bb:cc:dd:ee:ff:11:22"
}
```

### 9) zigbee_set_offline_timeout

```json
{
  "request_id": "req-zb-offline-timeout-001",
  "action": "zigbee_set_offline_timeout",
  "device_id": "aa:bb:cc:dd:ee:ff:11:22",
  "id_type": "ID_TYPE_HARDWARE_ID",
  "params": {
    "device_class": "sensor.temperature",
    "timeout_minutes": 15
  }
}
```

### 10) zigbee_send_southbound_data

```json
{
  "request_id": "req-zb-southbound-001",
  "action": "zigbee_send_southbound_data",
  "device_id": "aa:bb:cc:dd:ee:ff:11:22",
  "id_type": "ID_TYPE_HARDWARE_ID",
  "params": {
    "device_class": "sensor.temperature",
    "payload_b64": "AQIDBA==",
    "profile_id": 260,
    "cluster_id": 6
  }
}
```

### 11) zigbee_set_timeout

```json
{
  "request_id": "req-zb-set-timeout-001",
  "action": "zigbee_set_timeout",
  "device_id": "aa:bb:cc:dd:ee:ff:11:22",
  "id_type": "ID_TYPE_HARDWARE_ID",
  "params": {
    "device_class": "sensor.temperature",
    "timeout": "TIMEOUT_2MIN"
  }
}
```

### 12) zigbee_get_radios

```json
{
  "request_id": "req-zb-get-radios-001",
  "action": "zigbee_get_radios"
}
```

### 13) zigbee_get_radio_info

```json
{
  "request_id": "req-zb-get-radio-info-001",
  "action": "zigbee_get_radio_info",
  "radio_mac": "11:22:33:44:55:66"
}
```
Alternative using params:

```json
{
  "request_id": "req-zb-get-radio-info-002",
  "action": "zigbee_get_radio_info",
  "params": {
    "radio_mac": "11:22:33:44:55:66"
  }
}
```

### 14) zigbee_subscribe_packet_stream

Supported `fields` values for Zigbee packet stream are currently:

- `DeviceClass`

```json
{
  "request_id": "req-zb-subscribe-stream-001",
  "action": "zigbee_subscribe_packet_stream",
  "fields": ["DeviceClass"]
}
```
Alternative using params:

```json
{
  "request_id": "req-zb-subscribe-stream-002",
  "action": "zigbee_subscribe_packet_stream",
  "params": {
    "fields": ["DeviceClass"]
  }
}
```

### Non-Zigbee helper action: app_send_info

```json
{
  "request_id": "req-app-send-info-001",
  "action": "app_send_info",
  "params": {
    "message": "Hello from MQTT Explorer"
  }
}
```
