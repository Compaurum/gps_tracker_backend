// Package protocol implements wire protocol v1 between trackers and the backend.
// See README.md "Device protocol".
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

const (
	Version            = 1
	MaxFixesPerMessage = 100

	// Anything before this is a device whose clock isn't set yet (2020-01-01T00:00:00Z), epoch ms.
	minPlausibleTimestamp = 1577836800000
	// Tolerated device clock skew into the future, ms.
	maxFutureSkew = 5 * 60 * 1000
)

// Fix is a single GPS position. Optional fields are nil when the device didn't send them.
type Fix struct {
	Timestamp  int64    `json:"ts" dynamodbav:"ts"`                       // UTC epoch ms of the GPS fix
	Lat        float64  `json:"lat" dynamodbav:"lat"`                     // decimal degrees, WGS84
	Lon        float64  `json:"lon" dynamodbav:"lon"`                     // decimal degrees, WGS84
	Altitude   *float64 `json:"alt,omitempty" dynamodbav:"alt,omitempty"` // metres above mean sea level
	Speed      *float64 `json:"spd,omitempty" dynamodbav:"spd,omitempty"` // ground speed, km/h
	Course     *float64 `json:"crs,omitempty" dynamodbav:"crs,omitempty"` // course over ground, degrees 0-360
	Satellites *int     `json:"sat,omitempty" dynamodbav:"sat,omitempty"` // satellites used in the fix
	// Horizontal dilution of precision: how much satellite geometry amplifies position error.
	// Lower is better; horizontal error is roughly hdop × 3–5 m (<2 excellent, 2–5 good, >5 jumpy).
	// Stored as-is, not filtered on, so low-precision fixes can be judged later.
	Hdop *float64 `json:"hdop,omitempty" dynamodbav:"hdop,omitempty"`
}

type Telemetry struct {
	Timestamp      int64    `json:"ts"`   // device time the message was created, UTC epoch ms
	Sequence       *int64   `json:"seq"`  // message counter, for spotting gaps
	Fixes          []Fix    `json:"-"`    // parsed one by one, see ParseTelemetry
	BatteryVoltage *float64 `json:"batV"` // volts
	Rssi           *float64 `json:"rssi"` // cellular signal strength, dBm
}

type StatusEvent string

const (
	EventBoot       StatusEvent = "boot"
	EventHeartbeat  StatusEvent = "heartbeat"
	EventLowBattery StatusEvent = "low_battery"
	EventShutdown   StatusEvent = "shutdown"
)

var statusEvents = []StatusEvent{EventBoot, EventHeartbeat, EventLowBattery, EventShutdown}

type Status struct {
	Timestamp       int64       `json:"ts"` // device time of the event, UTC epoch ms
	Event           StatusEvent `json:"event"`
	FirmwareVersion *string     `json:"fw"`
	BatteryVoltage  *float64    `json:"batV"`   // volts
	Rssi            *float64    `json:"rssi"`   // cellular signal strength, dBm
	Uptime          *int64      `json:"uptime"` // seconds since boot
}

// envelope holds the fields every message carries. Pointers tell a missing field from zero.
type envelope struct {
	Version   *int   `json:"v"`
	Timestamp *int64 `json:"ts"`
}

func checkEnvelope(raw []byte, nowMs int64) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if env.Version == nil || *env.Version != Version {
		return errors.New("unsupported protocol version")
	}
	if env.Timestamp == nil || !plausibleTimestamp(*env.Timestamp, nowMs) {
		return errors.New("ts missing or implausible (device clock not set?)")
	}
	return nil
}

func plausibleTimestamp(ms, nowMs int64) bool {
	return ms >= minPlausibleTimestamp && ms <= nowMs+maxFutureSkew
}

// ParseTelemetry validates a telemetry message. Invalid fixes are dropped individually and
// counted in dropped; the message is rejected only if nothing valid remains.
// Unknown fields are ignored, so they never reach storage.
func ParseTelemetry(raw []byte, nowMs int64) (msg Telemetry, dropped int, err error) {
	if err := checkEnvelope(raw, nowMs); err != nil {
		return Telemetry{}, 0, err
	}
	var body struct {
		Telemetry
		Fixes []json.RawMessage `json:"fixes"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return Telemetry{}, 0, fmt.Errorf("invalid telemetry: %w", err)
	}
	if len(body.Fixes) == 0 {
		return Telemetry{}, 0, errors.New("fixes must be a non-empty array")
	}
	if len(body.Fixes) > MaxFixesPerMessage {
		return Telemetry{}, 0, fmt.Errorf("too many fixes (max %d)", MaxFixesPerMessage)
	}

	msg = body.Telemetry
	for _, rf := range body.Fixes {
		if f, ok := parseFix(rf, nowMs); ok {
			msg.Fixes = append(msg.Fixes, f)
		}
	}
	if len(msg.Fixes) == 0 {
		return Telemetry{}, 0, errors.New("no valid fixes")
	}
	return msg, len(body.Fixes) - len(msg.Fixes), nil
}

func parseFix(raw json.RawMessage, nowMs int64) (Fix, bool) {
	// Required fields are pointers so a missing field is distinguishable from zero.
	var required struct {
		Timestamp *int64   `json:"ts"`
		Lat       *float64 `json:"lat"`
		Lon       *float64 `json:"lon"`
	}
	var f Fix
	if json.Unmarshal(raw, &required) != nil || json.Unmarshal(raw, &f) != nil {
		return Fix{}, false
	}
	if required.Timestamp == nil || required.Lat == nil || required.Lon == nil {
		return Fix{}, false
	}
	if !plausibleTimestamp(f.Timestamp, nowMs) {
		return Fix{}, false
	}
	if f.Lat < -90 || f.Lat > 90 || f.Lon < -180 || f.Lon > 180 {
		return Fix{}, false
	}
	// 0,0 is what most receivers report before they have a fix.
	if f.Lat == 0 && f.Lon == 0 {
		return Fix{}, false
	}
	return f, true
}

// ParseStatus validates a status message.
func ParseStatus(raw []byte, nowMs int64) (Status, error) {
	if err := checkEnvelope(raw, nowMs); err != nil {
		return Status{}, err
	}
	var s Status
	if err := json.Unmarshal(raw, &s); err != nil {
		return Status{}, fmt.Errorf("invalid status: %w", err)
	}
	if !slices.Contains(statusEvents, s.Event) {
		return Status{}, fmt.Errorf("unknown status event %q", s.Event)
	}
	return s, nil
}
