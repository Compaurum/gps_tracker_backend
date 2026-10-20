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

	// Anything before this is a GPS that hasn't got a time fix yet (2020-01-01T00:00:00Z).
	minPlausibleTS = 1577836800
	// Tolerated device clock skew into the future, in seconds.
	maxFutureSkewS = 300
)

// Fix is a single GPS position. Optional fields are nil when the device didn't send them.
type Fix struct {
	T    int64    `json:"t" dynamodbav:"ts"`                        // UTC epoch seconds of the GPS fix
	Lat  float64  `json:"lat" dynamodbav:"lat"`                     // decimal degrees, WGS84
	Lon  float64  `json:"lon" dynamodbav:"lon"`                     // decimal degrees, WGS84
	Alt  *float64 `json:"alt,omitempty" dynamodbav:"alt,omitempty"` // metres above mean sea level
	Spd  *float64 `json:"spd,omitempty" dynamodbav:"spd,omitempty"` // km/h
	Crs  *float64 `json:"crs,omitempty" dynamodbav:"crs,omitempty"` // course over ground, degrees 0-360
	Sat  *int     `json:"sat,omitempty" dynamodbav:"sat,omitempty"` // satellites used
	Hdop *float64 `json:"hdop,omitempty" dynamodbav:"hdop,omitempty"`
}

type Telemetry struct {
	Seq   *int64
	Fixes []Fix
	Bat   *float64 // battery volts
	Rssi  *float64 // dBm
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
	Ev   StatusEvent `json:"ev"`
	Fw   *string     `json:"fw"` // firmware version
	Bat  *float64    `json:"bat"`
	Rssi *float64    `json:"rssi"`
	Up   *int64      `json:"up"` // uptime, seconds
}

type envelope struct {
	V *int `json:"v"`
}

func checkVersion(raw []byte) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if env.V == nil || *env.V != Version {
		return errors.New("unsupported protocol version")
	}
	return nil
}

// ParseTelemetry validates a telemetry message. Invalid fixes are dropped individually and
// counted in dropped; the message is rejected only if nothing valid remains.
// Unknown fields are ignored, so they never reach storage.
func ParseTelemetry(raw []byte, nowS int64) (msg Telemetry, dropped int, err error) {
	if err := checkVersion(raw); err != nil {
		return Telemetry{}, 0, err
	}
	var body struct {
		Seq   *int64            `json:"seq"`
		Fixes []json.RawMessage `json:"fixes"`
		Bat   *float64          `json:"bat"`
		Rssi  *float64          `json:"rssi"`
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

	msg = Telemetry{Seq: body.Seq, Bat: body.Bat, Rssi: body.Rssi}
	for _, rf := range body.Fixes {
		if f, ok := parseFix(rf, nowS); ok {
			msg.Fixes = append(msg.Fixes, f)
		}
	}
	if len(msg.Fixes) == 0 {
		return Telemetry{}, 0, errors.New("no valid fixes")
	}
	return msg, len(body.Fixes) - len(msg.Fixes), nil
}

func parseFix(raw json.RawMessage, nowS int64) (Fix, bool) {
	// Required fields are pointers so a missing field is distinguishable from zero.
	var rf struct {
		T   *int64   `json:"t"`
		Lat *float64 `json:"lat"`
		Lon *float64 `json:"lon"`
	}
	var f Fix
	if json.Unmarshal(raw, &rf) != nil || json.Unmarshal(raw, &f) != nil {
		return Fix{}, false
	}
	if rf.T == nil || rf.Lat == nil || rf.Lon == nil {
		return Fix{}, false
	}
	if f.T < minPlausibleTS || f.T > nowS+maxFutureSkewS {
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
func ParseStatus(raw []byte) (Status, error) {
	if err := checkVersion(raw); err != nil {
		return Status{}, err
	}
	var s Status
	if err := json.Unmarshal(raw, &s); err != nil {
		return Status{}, fmt.Errorf("invalid status: %w", err)
	}
	if !slices.Contains(statusEvents, s.Ev) {
		return Status{}, fmt.Errorf("unknown status event %q", s.Ev)
	}
	return s, nil
}
