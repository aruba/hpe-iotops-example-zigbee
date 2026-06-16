package zigbee

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SensorData is the telemetry payload exposed by the sample Aqara W100 device.
type SensorData struct {
	Temperature float64 `json:"temperature"`
	Humidity    float64 `json:"humidity"`
	Pressure    float64 `json:"pressure"`
}

// HueLightData holds the state of a Philips Hue smart light.
// On/off, brightness, color temperature, and RGB color are all controllable.
type HueLightData struct {
	On         bool   `json:"on"`
	Brightness int    `json:"brightness"` // 0–100 %
	ColorTemp  int    `json:"color_temp"` // Mired scale (153–500)
	Color      string `json:"color"`      // hex RGB, e.g. "#FFFFFF"
}

// Device is the in-memory representation of one Zigbee device.
type Device struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Model       string        `json:"model"`
	Type        string        `json:"type"`
	Status      string        `json:"status"`
	Data        SensorData    `json:"data,omitempty"`
	HueData     *HueLightData `json:"hue_data,omitempty"`
	LastUpdated time.Time     `json:"last_updated"`
}

// Command is the API/MQTT command contract for device control actions.
type Command struct {
	Command string         `json:"command"`
	Params  map[string]any `json:"params"`
}

// Store provides thread-safe access to device state persisted in /home/app/data/devices.json.
// Every mutation (command execution, state change) is immediately written to disk.
// On startup, devices are loaded from the file or initialized with sensible defaults.
type Store struct {
	mu       sync.RWMutex
	rndMu    sync.Mutex
	devices  map[string]*Device
	filePath string
}

// NewStore loads or creates the device store in the given data directory.
// If devices.json exists, it loads the saved state. Otherwise, it initializes
// with sample devices (Aqara W100 sensor + Philips Hue smart light).
func NewStore(dataDir string) (*Store, error) {
	// Ensure the data directory exists.
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	filePath := filepath.Join(dataDir, "devices.json")

	// Try to load existing devices from the file.
	if data, err := os.ReadFile(filePath); err == nil {
		// File exists; parse it.
		var devices map[string]*Device
		if err := json.Unmarshal(data, &devices); err != nil {
			return nil, fmt.Errorf("parse devices.json: %w", err)
		}
		return &Store{devices: devices, filePath: filePath}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// File exists but couldn't be read.
		return nil, fmt.Errorf("read devices.json: %w", err)
	}

	// File does not exist; create with defaults.
	defaultDevices := map[string]*Device{
		// "aqara_w100_01": {
		// 	ID:          "aqara_w100_01",
		// 	Name:        "Aqara W100 Living Room",
		// 	Model:       "Aqara W100",
		// 	Type:        "temperature_humidity_pressure_sensor",
		// 	Status:      "online",
		// 	Data:        SensorData{Temperature: 24.1, Humidity: 41.2, Pressure: 1012.8},
		// 	LastUpdated: now,
		// },
		// "philips_hue_color_01": {
		// 	ID:          "philips_hue_color_01",
		// 	Name:        "Hue Color Bulb Office",
		// 	Model:       "Philips Hue White and Color Ambiance",
		// 	Type:        "smart_light",
		// 	Status:      "online",
		// 	HueData:     &HueLightData{On: true, Brightness: 80, ColorTemp: 300, Color: "#FFFFFF"},
		// 	LastUpdated: now,
		// },
	}

	store := &Store{devices: defaultDevices, filePath: filePath}
	if err := store.save(); err != nil {
		return nil, fmt.Errorf("write initial devices.json: %w", err)
	}
	return store, nil
}

// save writes all devices to the JSON file. Called after every mutation.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.devices, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal devices: %w", err)
	}
	// Use atomic write: write to temp file, then rename.
	tmpPath := s.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.filePath); err != nil {
		return fmt.Errorf("rename file: %w", err)
	}
	return nil
}

// List returns a copy of all devices.
// We return copies to prevent external callers from mutating shared state.
func (s *Store) List() []Device {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, *d)
	}
	return out
}

// Get returns one device by id.
func (s *Store) Get(id string) (Device, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	d, ok := s.devices[id]
	if !ok {
		return Device{}, false
	}
	return *d, true
}

// UpdateSensorData applies a small random drift to sensor values to simulate
// real-world periodic changes, then persists the updated device to disk.
func (s *Store) UpdateSensorData(id string) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.devices[id]
	if !ok {
		return Device{}, fmt.Errorf("device %s not found", id)
	}

	// Keep values in realistic bounds for demos/tests.
	s.rndMu.Lock()
	// Simulate natural variation for the Aqara W100 sample payload.
	d.Data.Temperature = clamp(d.Data.Temperature+randDelta(0.4), 18.0, 32.0)
	d.Data.Humidity = clamp(d.Data.Humidity+randDelta(1.5), 25.0, 85.0)
	d.Data.Pressure = clamp(d.Data.Pressure+randDelta(0.9), 980.0, 1050.0)
	s.rndMu.Unlock()

	d.LastUpdated = time.Now().UTC()
	if err := s.save(); err != nil {
		return *d, fmt.Errorf("persist device: %w", err)
	}
	return *d, nil
}

// ExecuteCommand runs one supported command against a device.
func (s *Store) ExecuteCommand(id string, cmd Command) (Device, error) {
	if cmd.Command == "" {
		return Device{}, errors.New("missing command")
	}

	switch cmd.Command {
	case "read_sensor":
		return s.UpdateSensorData(id)
	case "set_status":
		return s.setStatus(id, cmd.Params)

	// ── Philips Hue light commands ────────────────────────────────
	case "turn_on":
		return s.setHueOn(id, true)
	case "turn_off":
		return s.setHueOn(id, false)
	case "set_brightness":
		return s.setHueBrightness(id, cmd.Params)
	case "set_color_temp":
		return s.setHueColorTemp(id, cmd.Params)
	case "set_color":
		return s.setHueColor(id, cmd.Params)
	default:
		return Device{}, fmt.Errorf("unsupported command: %s", cmd.Command)
	}
}

// setHueOn turns the Hue light on or off and persists the change.
func (s *Store) setHueOn(id string, on bool) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return Device{}, fmt.Errorf("device %s not found", id)
	}
	if d.HueData == nil {
		return Device{}, fmt.Errorf("device %s is not a Hue light", id)
	}
	d.HueData.On = on
	d.LastUpdated = time.Now().UTC()
	if err := s.save(); err != nil {
		return *d, fmt.Errorf("persist device: %w", err)
	}
	return *d, nil
}

// setHueBrightness sets the Hue light brightness (0–100) and persists the change.
func (s *Store) setHueBrightness(id string, params map[string]any) (Device, error) {
	v, ok := params["brightness"]
	if !ok {
		return Device{}, errors.New("brightness param is required")
	}
	// JSON numbers unmarshal as float64.
	bval, ok := v.(float64)
	if !ok {
		return Device{}, errors.New("brightness must be a number")
	}
	bint := int(bval)
	if bint < 0 || bint > 100 {
		return Device{}, errors.New("brightness must be between 0 and 100")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return Device{}, fmt.Errorf("device %s not found", id)
	}
	if d.HueData == nil {
		return Device{}, fmt.Errorf("device %s is not a Hue light", id)
	}
	d.HueData.Brightness = bint
	d.LastUpdated = time.Now().UTC()
	if err := s.save(); err != nil {
		return *d, fmt.Errorf("persist device: %w", err)
	}
	return *d, nil
}

// setHueColorTemp sets the Hue light color temperature in the Mired scale (153–500) and persists.
func (s *Store) setHueColorTemp(id string, params map[string]any) (Device, error) {
	v, ok := params["color_temp"]
	if !ok {
		return Device{}, errors.New("color_temp param is required (mired scale 153-500)")
	}
	ct, ok := v.(float64)
	if !ok {
		return Device{}, errors.New("color_temp must be a number")
	}
	ctint := int(ct)
	if ctint < 153 || ctint > 500 {
		return Device{}, errors.New("color_temp must be between 153 and 500 (mireds)")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return Device{}, fmt.Errorf("device %s not found", id)
	}
	if d.HueData == nil {
		return Device{}, fmt.Errorf("device %s is not a Hue light", id)
	}
	d.HueData.ColorTemp = ctint
	d.LastUpdated = time.Now().UTC()
	if err := s.save(); err != nil {
		return *d, fmt.Errorf("persist device: %w", err)
	}
	return *d, nil
}

// setHueColor sets the Hue light color as a hex RGB string, e.g. "#FF5500" and persists.
func (s *Store) setHueColor(id string, params map[string]any) (Device, error) {
	v, ok := params["color"]
	if !ok {
		return Device{}, errors.New("color param is required (hex RGB, e.g. #FF5500)")
	}
	color, ok := v.(string)
	if !ok || color == "" {
		return Device{}, errors.New("color must be a non-empty hex string")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return Device{}, fmt.Errorf("device %s not found", id)
	}
	if d.HueData == nil {
		return Device{}, fmt.Errorf("device %s is not a Hue light", id)
	}
	d.HueData.Color = color
	d.LastUpdated = time.Now().UTC()
	if err := s.save(); err != nil {
		return *d, fmt.Errorf("persist device: %w", err)
	}
	return *d, nil
}

// setStatus is a helper command implementation for online/offline state toggling.
func (s *Store) setStatus(id string, params map[string]any) (Device, error) {
	v, ok := params["status"]
	if !ok {
		return Device{}, errors.New("status param is required")
	}

	status, ok := v.(string)
	if !ok {
		return Device{}, errors.New("status param must be string")
	}
	if status != "online" && status != "offline" {
		return Device{}, errors.New("status must be online or offline")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return Device{}, fmt.Errorf("device %s not found", id)
	}

	d.Status = status
	d.LastUpdated = time.Now().UTC()
	if err := s.save(); err != nil {
		return *d, fmt.Errorf("persist device: %w", err)
	}
	return *d, nil
}

// randDelta returns a random value in the range [-spread, +spread].
func randDelta(spread float64) float64 {
	return (rand.Float64() * spread * 2) - spread
}

// clamp bounds v to [min, max].
func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
