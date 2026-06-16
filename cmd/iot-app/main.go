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
					"skipped_steps", []string{"get_devices", "get_unclassified_devices", "provision_discovered_devices", "provision_configured_macs", "start_app_info_reporter"},
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
			discoveredOrdered := make([]string, 0)
			if devicesErr == nil && devicesResp != nil {
				for _, d := range devicesResp.Devices {
					mac := strings.TrimSpace(d.Mac)
					if mac == "" {
						continue
					}
					key := strings.ToLower(mac)
					if _, exists := discoveredSet[key]; !exists {
						discoveredSet[key] = struct{}{}
						discoveredOrdered = append(discoveredOrdered, mac)
					}
				}
			}
			if unclassifiedErr == nil && unclassifiedResp != nil {
				for _, d := range unclassifiedResp.Devices {
					mac := strings.TrimSpace(d.Mac)
					if mac == "" {
						continue
					}
					key := strings.ToLower(mac)
					if _, exists := discoveredSet[key]; !exists {
						discoveredSet[key] = struct{}{}
						discoveredOrdered = append(discoveredOrdered, mac)
					}
				}
			}

			configuredMACs := []string{
				cfg.App.DeviceMacAddress1,
				cfg.App.DeviceMacAddress2,
				cfg.App.DeviceMacAddress3,
				cfg.App.DeviceMacAddress4,
				cfg.App.DeviceMacAddress5,
			}

			// Report app liveness to the gateway every minute using /api/v3/apps/info.
			go appInfoReporter(ctx, log, gwClient)

			// Provisioning can involve many network calls; run it asynchronously so
			// HTTP startup is never delayed enough to trip Kubernetes probes.
			go func(discovered, configured []string) {
				// First provision discovered devices.
				if len(discovered) == 0 {
					log.Warn("provision_discovered_devices_skipped",
						"reason", "no devices returned by get_devices/get_unclassified_devices",
					)
				} else {
					provisionMacDevices(ctx, log, gwClient, discovered, "discovered_devices")
				}

				// Then provision explicitly configured MAC addresses (if present).
				hasConfiguredMAC := false
				for _, m := range configured {
					if strings.TrimSpace(m) != "" {
						hasConfiguredMAC = true
						break
					}
				}
				if hasConfiguredMAC {
					provisionMacDevices(ctx, log, gwClient, configured, "configured_env")
				} else {
					log.Info("provision_no_mac_addresses_configured",
						"reason", "none of DEVICE_MAC_ADDRESS_1..5 are set; skipping configured MAC provisioning")
				}
			}(append([]string(nil), discoveredOrdered...), append([]string(nil), configuredMACs...))
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

// provisionMacDevices iterates over a list of device MAC addresses.
// For each non-empty MAC it:
//  1. Calls UpdateDeviceClass to assign class ["Aqara"] to that device.
//  2. On success, calls SetDeviceID with device class "Aqara" and deviceId "<mac>_chinmay_<random>".
//  3. On success, calls SetOfflineTimeout to 1440 minutes for that device.
//  4. On success, calls SetTimeoutValue to TIMEOUT_INFINITE for that device.
//
// SubscribePacketStream is called once after all devices are processed, but only
// if at least one device had both its class and device ID set successfully.
func provisionMacDevices(ctx context.Context, log *slog.Logger, gw *zigbee.GatewayClient, macs []string, source string) {
	fullyProvisioned := 0

	for i, mac := range macs {
		mac = strings.TrimSpace(mac)
		if mac == "" {
			continue
		}
		targetLabel := source + "_index_" + strconv.Itoa(i+1)
		if source == "configured_env" {
			targetLabel = "DEVICE_MAC_ADDRESS_" + strconv.Itoa(i+1)
		}
		deviceID := mac + "_chinmay_" + strconv.Itoa(rand.Intn(1_000_000))

		provCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := gw.UpdateDeviceClass(provCtx, mac, zigbee.DeviceClassArray{DeviceClass: []string{"Aqara"}})
		cancel()
		if err != nil {
			log.Warn("provision_set_device_class_failed",
				"target", targetLabel,
				"source", source,
				"mac", mac,
				"error", err,
			)
			continue
		}
		log.Info("provision_set_device_class_ok",
			"target", targetLabel,
			"source", source,
			"mac", mac,
			"device_class", "Aqara",
		)

		provCtx2, cancel2 := context.WithTimeout(ctx, 15*time.Second)
		_, err = gw.SetDeviceID(provCtx2, mac, "Aqara", zigbee.DeviceClassScopedData{
			DeviceClass: "Aqara",
			DeviceID:    deviceID,
		})
		cancel2()
		if err != nil {
			log.Warn("provision_set_device_id_failed",
				"target", targetLabel,
				"source", source,
				"mac", mac,
				"device_id", deviceID,
				"error", err,
			)
			continue
		}
		log.Info("provision_set_device_id_ok",
			"target", targetLabel,
			"source", source,
			"mac", mac,
			"device_id", deviceID,
		)

		// Both class and device ID succeeded — count this device as fully provisioned.
		fullyProvisioned++

		// Set offline timeout to the maximum allowed value (1440 minutes = 24 hours).
		provCtx3, cancel3 := context.WithTimeout(ctx, 15*time.Second)
		_, err = gw.SetOfflineTimeout(provCtx3, mac, "", "Aqara", zigbee.OfflineTimeoutData{TimeoutMinutes: 1440})
		cancel3()
		if err != nil {
			log.Warn("provision_set_offline_timeout_failed",
				"target", targetLabel,
				"source", source,
				"mac", mac,
				"timeout_minutes", 1440,
				"error", err,
			)
		} else {
			log.Info("provision_set_offline_timeout_ok",
				"target", targetLabel,
				"source", source,
				"mac", mac,
				"timeout_minutes", 1440,
			)
		}

		// Set keep-alive timeout to TIMEOUT_INFINITE so the device is never expired.
		provCtx4, cancel4 := context.WithTimeout(ctx, 15*time.Second)
		_, err = gw.SetTimeoutValue(provCtx4, mac, "", "Aqara", zigbee.TimeoutValueData{Timeout: "TIMEOUT_INFINITE"})
		cancel4()
		if err != nil {
			log.Warn("provision_set_timeout_value_failed",
				"target", targetLabel,
				"source", source,
				"mac", mac,
				"timeout", "TIMEOUT_INFINITE",
				"error", err,
			)
		} else {
			log.Info("provision_set_timeout_value_ok",
				"target", targetLabel,
				"source", source,
				"mac", mac,
				"timeout", "TIMEOUT_INFINITE",
			)
		}
	}

	// Only subscribe to the packet stream if at least one device was fully provisioned
	// (both class and device ID set successfully).
	if fullyProvisioned == 0 {
		log.Info("provision_subscribe_packet_stream_skipped",
			"reason", "no devices were fully provisioned (class + device ID)",
		)
		return
	}

	streamCtx, streamCancel := context.WithTimeout(ctx, 15*time.Second)
	streamResp, err := gw.SubscribePacketStream(streamCtx, nil)
	streamCancel()
	if err != nil {
		log.Warn("provision_subscribe_packet_stream_failed", "error", err)
	} else {
		log.Info("provision_subscribe_packet_stream_ok",
			"fully_provisioned_devices", fullyProvisioned,
			"packets_buffered", len(streamResp.Packets),
		)
	}
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
