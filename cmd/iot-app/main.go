package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"zigbee-demo-app/internal/api"
	"zigbee-demo-app/pkg/config"
	"zigbee-demo-app/pkg/logger"
	"zigbee-demo-app/pkg/mqtt"
	"zigbee-demo-app/pkg/retry"
	"zigbee-demo-app/pkg/zigbee"
)

// main is the real OS process entrypoint.
// We delegate to run() so we can return an exit code in one place.
func main() {
	os.Exit(run())
}

// run wires all components together and keeps the app alive until shutdown.
// Return value convention:
//   - 0 means success
//   - 1 means startup or shutdown failure
func run() int {
	// CONFIG_FILE lets containers point to a mounted config file location.
	configPath := os.Getenv("CONFIG_FILE")
	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("failed to load configuration", "error", err)
		return 1
	}

	// Create one shared structured logger used by all packages.
	log := logger.New(cfg.App.LogLevel)

	// Log every environment variable at startup for container debugging.
	envs := os.Environ()
	sort.Strings(envs)
	for _, kv := range envs {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			log.Info("app_env", "key", parts[0], "value", parts[1])
		} else {
			log.Info("app_env", "key", parts[0], "value", "")
		}
	}

	// Detect the hardware platform this container is running on and log it once at startup.
	platform := platformLabel()
	log.Info("app_starting", "address", cfg.Address(), "platform", platform)

	// Context cancellation is triggered when SIGINT/SIGTERM is received.
	// Every long-running goroutine should listen to this context.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Store loads or creates device state from /home/app/data/devices.json.
	// The data directory can be overridden via DATA_DIR environment variable (useful for testing).
	// On first run, it seeds two sample devices: Aqara W100 sensor + Philips Hue light.
	// On subsequent runs, it loads saved state from the file.
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/home/app/data"
	}
	store, err := zigbee.NewStore(dataDir)
	if err != nil {
		fallbackDataDir := "/tmp/zigbee-demo-app/data"
		log.Warn("device_store_init_failed_primary",
			"data_dir", dataDir,
			"error", err,
			"fallback_data_dir", fallbackDataDir,
		)

		store, err = zigbee.NewStore(fallbackDataDir)
		if err != nil {
			log.Error("failed_to_initialize_device_store",
				"primary_data_dir", dataDir,
				"fallback_data_dir", fallbackDataDir,
				"error", err,
			)
			return 1
		}

		log.Warn("device_store_fallback_enabled",
			"active_data_dir", fallbackDataDir,
			"reason", "primary data directory is unavailable",
		)
	}

	// Build the Aruba IoT Gateway Zigbee API client when APIGW_URL is configured.
	// When not configured (e.g. local dev), the gateway client is nil and Zigbee
	// proxy routes return 503 so the rest of the app still works.
	var gwClient *zigbee.GatewayClient
	if cfg.GatewayEnabled() {
		gwClient = zigbee.NewGatewayClient(cfg.Gateway, cfg.GatewayRequestTimeout(), log)
		log.Info("zigbee_gateway_configured", "url", cfg.Gateway.APIGWURL, "gateway_id", cfg.Gateway.GatewayID)

		// Run gateway startup substeps asynchronously so HTTP startup is not blocked
		// by gateway reachability/provisioning latency.
		go func() {
			// Ensure gateway is reachable with exponential backoff before attempting API calls.
			// Bound the wait so this background task does not retry forever.
			connectivityCtx, connectivityCancel := context.WithTimeout(ctx, 45*time.Second)
			defer connectivityCancel()
			if err := ensureGatewayConnectivity(connectivityCtx, log, gwClient, cfg.Gateway.APIGWURL); err != nil {
				log.Warn("gateway_connectivity_failed", "url", cfg.Gateway.APIGWURL, "error", err)
				log.Warn("gateway_startup_steps_skipped",
					"reason", "connectivity check failed",
					"max_wait_seconds", 45,
					"skipped_steps", []string{"get_devices", "get_unclassified_devices", "start_app_info_reporter"},
				)
				return
			}

			log.Info("gateway_connectivity_ok", "url", cfg.Gateway.APIGWURL)

			// Initial gateway read calls so operators can confirm API access at startup.
			startupAPICtx, startupAPICancel := context.WithTimeout(ctx, 20*time.Second)
			devicesResp, devicesErr := gwClient.GetDeviceList(startupAPICtx)
			if devicesErr != nil {
				log.Warn("gateway_get_devices_failed", "error", devicesErr)
			} else {
				log.Info("gateway_get_devices_ok", "count", len(devicesResp.Devices))
			}

			unclassifiedResp, unclassifiedErr := gwClient.GetDevicesUnclassified(startupAPICtx)
			if unclassifiedErr != nil {
				log.Warn("gateway_get_unclassified_devices_failed", "error", unclassifiedErr)
			} else {
				log.Info("gateway_get_unclassified_devices_ok", "count", len(unclassifiedResp.Devices))
			}
			startupAPICancel()

			discoveredSet := make(map[string]struct{})
			discoveredMACs := make([]string, 0)

			if devicesErr == nil && devicesResp != nil {
				for _, d := range devicesResp.Devices {
					mac := strings.TrimSpace(d.Mac)
					if mac == "" {
						continue
					}
					k := strings.ToLower(mac)
					if _, exists := discoveredSet[k]; exists {
						continue
					}
					discoveredSet[k] = struct{}{}
					discoveredMACs = append(discoveredMACs, mac)
				}
			}

			if unclassifiedErr == nil && unclassifiedResp != nil {
				for _, d := range unclassifiedResp.Devices {
					mac := strings.TrimSpace(d.Mac)
					if mac == "" {
						continue
					}
					k := strings.ToLower(mac)
					if _, exists := discoveredSet[k]; exists {
						continue
					}
					discoveredSet[k] = struct{}{}
					discoveredMACs = append(discoveredMACs, mac)
				}
			}

			if len(discoveredMACs) == 0 {
				log.Info("gateway_discovered_devices_empty",
					"source_calls", []string{"zigbee_get_devices", "zigbee_get_devices_unclassified"},
					"result", "no devices to provision",
				)
			} else {
				log.Info("gateway_discovered_devices_ready_for_provisioning",
					"count", len(discoveredMACs),
					"devices", discoveredMACs,
				)
				go provisionDiscoveredDevices(ctx, log, gwClient, append([]string(nil), discoveredMACs...))
			}

			// Report app liveness to the gateway every minute using /api/v3/apps/info.
			go appInfoReporter(ctx, log, gwClient)
		}()
	} else {
		log.Info("zigbee_gateway_disabled", "reason", "APIGW_URL not set; set APIGW_URL and APIKEY to enable")
	}

	// Start MQTT integration. Initial connection failure is non-fatal — a retry
	// loop will reconnect every minute. HTTP API remains available in the meantime.
	mqttClient := mqtt.NewClient(cfg.MQTT, log, gwClient)
	if err := mqttClient.Start(ctx); err != nil {
		log.Warn("mqtt_start_failed", "error", err)
	}
	defer mqttClient.Stop()

	// Publish app status to MQTT northbound topic every minute.
	go northboundStatusPublisher(ctx, log, mqttClient, store, time.Now().UTC())

	// Periodic heartbeat to stdout so operators can see liveness and current state.
	go statusReporter(ctx, log, store, mqttClient, time.Now().UTC(), cfg.StatusInterval())

	// HTTP server is the primary foreground workload for this container.
	server := api.NewServer(cfg.Server, log, store, gwClient)
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Start()
	}()

	// Wait for either:
	// 1) OS shutdown signal, or
	// 2) unexpected HTTP server termination.
	select {
	case <-ctx.Done():
		log.Info("shutdown_signal_received")
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http_server_failed", "error", err)
			return 1
		}
	}

	// Give in-flight HTTP requests up to 10 seconds to finish cleanly.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("http_server_shutdown_failed", "error", err)
		return 1
	}

	log.Info("app_stopped")
	return 0
}

// mqttStatus is a narrow interface so statusReporter depends on behavior,
// not a concrete MQTT implementation.
type mqttStatus interface {
	IsConnected() bool
	LastPublish() time.Time
}

// platformLabel returns a human-readable description of the CPU architecture
// this binary is running on.
//
//	"amd64"       → "Running on IoT Connector VM"
//	"arm"/"arm64" → "Running on AP IoT Connector"
func platformLabel() string {
	switch runtime.GOARCH {
	case "amd64":
		return "Running on IoT Connector VM"
	case "arm", "arm64":
		return "Running on AP IoT Connector"
	default:
		return "Running on unknown platform (" + runtime.GOARCH + ")"
	}
}

// statusReporter logs a periodic operational snapshot.
// This log is useful for early debugging and production monitoring.
func statusReporter(ctx context.Context, log *slog.Logger, store *zigbee.Store, mqttClient mqttStatus, startTime time.Time, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Read current in-memory device state.
			devices := store.List()
			// Last publish is optional; if never published we make that explicit.
			lastPublish := mqttClient.LastPublish()
			lastPublishText := "never"
			if !lastPublish.IsZero() {
				lastPublishText = lastPublish.Format(time.RFC3339)
			}

			// One structured log event contains the full periodic status snapshot.
			log.Info("status_report",
				"platform", platformLabel(),
				"uptime_seconds", int(time.Since(startTime).Seconds()),
				"devices_count", len(devices),
				"mqtt_connected", mqttClient.IsConnected(),
				"last_telemetry_publish", lastPublishText,
				"goroutines", runtime.NumGoroutine(),
				"devices", devices,
			)
		}
	}
}

// appInfoReporter posts app status to /api/v3/apps/info at 1-minute intervals.
// Message format is fixed as requested:
// "Hello app is running as of timestamp <epochtime>"
func appInfoReporter(ctx context.Context, log *slog.Logger, gw *zigbee.GatewayClient) {
	report := func() {
		epoch := time.Now().Unix()
		msg := platformLabel() + " | Hello app is running as of timestamp " + strconv.FormatInt(epoch, 10)
		if _, err := gw.SendAppInfo(ctx, msg); err != nil {
			log.Warn("send_app_info_failed", "error", err)
			return
		}
		log.Info("app_info_reported", "message", msg)
	}

	// Send one report immediately at startup, then every minute.
	report()

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			report()
		}
	}
}

// northboundStatusPublisher publishes app status to MQTT northbound topic every minute.
// This allows external systems to monitor app health via MQTT messaging.
type northboundPublisher interface {
	PublishNorthboundStatus(status map[string]any) error
	IsConnected() bool
}

func northboundStatusPublisher(ctx context.Context, log *slog.Logger, mqttClient northboundPublisher, store *zigbee.Store, startTime time.Time) {
	publish := func() {
		epoch := time.Now().Unix()
		status := map[string]any{
			"timestamp":      epoch,
			"uptime_seconds": int(time.Since(startTime).Seconds()),
			"devices_count":  len(store.List()),
			"mqtt_connected": mqttClient.IsConnected(),
			"platform":       platformLabel(),
			"goroutines":     runtime.NumGoroutine(),
			"message":        platformLabel() + " | Hello app is running as of timestamp " + strconv.FormatInt(epoch, 10),
		}

		if err := mqttClient.PublishNorthboundStatus(status); err != nil {
			log.Warn("publish_northbound_status_failed", "error", err)
			return
		}
		log.Info("northbound_status_published", "status", status)
	}

	// Send one status immediately at startup, then every minute.
	publish()

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}

// provisionDiscoveredDevices applies startup provisioning to devices discovered
// from gateway get-calls.
// For each MAC address it attempts:
//  1. UpdateDeviceClass to ["aqara"]
//  2. SetDeviceID with class "aqara" and id format "<mac>_chinmay_<random>"
//
// After all devices are processed, it subscribes to packet stream only if at
// least one device completed both steps.
func provisionDiscoveredDevices(ctx context.Context, log *slog.Logger, gw *zigbee.GatewayClient, macs []string) {
	const deviceClass = "aqara"
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	total := len(macs)
	classSuccess := 0
	deviceIDSuccess := 0

	log.Info("startup_discovered_provisioning_started",
		"total_devices", total,
		"device_class", deviceClass,
	)

	for i, mac := range macs {
		mac = strings.TrimSpace(mac)
		if mac == "" {
			log.Warn("startup_provision_device_skipped",
				"index", i,
				"reason", "empty_mac",
			)
			continue
		}

		deviceID := mac + "_chinmay_" + strconv.Itoa(rng.Intn(1_000_000))
		log.Info("startup_provision_device_attempt",
			"index", i,
			"mac", mac,
			"target_device_id", deviceID,
			"target_device_class", deviceClass,
		)

		classCtx, classCancel := context.WithTimeout(ctx, 15*time.Second)
		_, classErr := gw.UpdateDeviceClass(classCtx, mac, zigbee.DeviceClassArray{DeviceClass: []string{deviceClass}})
		classCancel()
		if classErr != nil {
			log.Warn("startup_provision_set_device_class_failed",
				"index", i,
				"mac", mac,
				"device_class", deviceClass,
				"error", classErr,
			)
			continue
		}
		classSuccess++
		log.Info("startup_provision_set_device_class_ok",
			"index", i,
			"mac", mac,
			"device_class", deviceClass,
		)

		idCtx, idCancel := context.WithTimeout(ctx, 15*time.Second)
		_, idErr := gw.SetDeviceID(idCtx, mac, deviceClass, zigbee.DeviceClassScopedData{
			DeviceClass: deviceClass,
			DeviceID:    deviceID,
		})
		idCancel()
		if idErr != nil {
			log.Warn("startup_provision_set_device_id_failed",
				"index", i,
				"mac", mac,
				"device_class", deviceClass,
				"device_id", deviceID,
				"error", idErr,
			)
			continue
		}
		deviceIDSuccess++
		log.Info("startup_provision_set_device_id_ok",
			"index", i,
			"mac", mac,
			"device_class", deviceClass,
			"device_id", deviceID,
		)
	}

	log.Info("startup_discovered_provisioning_completed",
		"total_devices", total,
		"set_device_class_success", classSuccess,
		"set_device_id_success", deviceIDSuccess,
	)

	if deviceIDSuccess == 0 {
		log.Warn("startup_subscribe_packet_stream_skipped",
			"reason", "no device completed class+device_id setup",
			"total_devices", total,
		)
		return
	}

	streamCtx, streamCancel := context.WithTimeout(ctx, 15*time.Second)
	streamResp, streamErr := gw.SubscribePacketStream(streamCtx, []string{"DeviceClass", "DeviceInfo"})
	streamCancel()
	if streamErr != nil {
		log.Warn("startup_subscribe_packet_stream_failed",
			"provisioned_devices", deviceIDSuccess,
			"fields", []string{"DeviceClass", "DeviceInfo"},
			"error", streamErr,
		)
		return
	}

	packetsBuffered := 0
	if streamResp != nil {
		packetsBuffered = len(streamResp.Packets)
	}
	log.Info("startup_subscribe_packet_stream_ok",
		"provisioned_devices", deviceIDSuccess,
		"fields", []string{"DeviceClass", "DeviceInfo"},
		"packets_buffered", packetsBuffered,
	)
}

// ensureGatewayConnectivity attempts to reach the gateway with exponential backoff.
// Initial delay is 1 second, doubled on each retry, capped at 30 minutes.
// This ensures the app can recover from gateway startup delays or network issues.
func ensureGatewayConnectivity(ctx context.Context, log *slog.Logger, gw *zigbee.GatewayClient, url string) error {
	backoff := retry.NewExponentialBackoff(1*time.Second, 2.0, 30*time.Minute)

	for {
		// Attempt ping with a 5-second timeout
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		statusCode, err := gw.Ping(pingCtx)
		cancel()

		if err == nil {
			log.Info("gateway_connectivity_established",
				"url", url,
				"http_status", statusCode,
				"attempts", backoff.Attempts(),
			)
			return nil
		}

		// Check if we should continue retrying
		delay := backoff.NextDelay()
		log.Warn("gateway_connectivity_attempt_failed",
			"url", url,
			"error", err,
			"attempt", backoff.Attempts(),
			"next_retry_in", delay.String(),
		)

		// Wait for the calculated delay or context cancellation
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled after %d attempts: %w", backoff.Attempts(), err)
		case <-time.After(delay):
		}
	}
}
