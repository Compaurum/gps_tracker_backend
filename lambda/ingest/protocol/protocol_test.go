package protocol

import (
	"strings"
	"testing"
)

const now = int64(1760950000000)

const validFix = `{"ts":1760949990000,"lat":-32.9283,"lon":151.7817,"alt":12.3,"spd":4.2,"crs":270,"sat":9,"hdop":0.9}`

// telemetry wraps fixes in an otherwise valid telemetry envelope; extra is spliced in verbatim.
func telemetry(fixes, extra string) []byte {
	return []byte(`{"v":1,"ts":1760949995000,"fixes":[` + fixes + `]` + extra + `}`)
}

func TestParseTelemetryValid(t *testing.T) {
	msg, dropped, err := ParseTelemetry(telemetry(validFix, `,"seq":7,"batV":3.98,"rssi":-71`), now)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 || len(msg.Fixes) != 1 || msg.Timestamp != 1760949995000 || *msg.Sequence != 7 || *msg.BatteryVoltage != 3.98 || *msg.Rssi != -71 {
		t.Fatalf("unexpected result: %+v dropped=%d", msg, dropped)
	}
	f := msg.Fixes[0]
	if f.Timestamp != 1760949990000 || f.Lat != -32.9283 || f.Lon != 151.7817 || *f.Speed != 4.2 || *f.Satellites != 9 {
		t.Fatalf("unexpected fix: %+v", f)
	}
}

func TestParseTelemetryDropsInvalidFixes(t *testing.T) {
	fixes := []string{
		validFix,
		`{"ts":1760949990000,"lat":0,"lon":0}`,                   // no-fix placeholder
		`{"ts":0,"lat":-32.9,"lon":151.7}`,                       // clock not set
		`{"ts":1760953600000,"lat":-32.9,"lon":151.7}`,           // in the future
		`{"ts":1760949990000,"lat":91,"lon":151.7}`,              // out of range
		`{"ts":1760949990000,"lat":-32.9,"lon":151.7,"spd":"4"}`, // wrong type
		`{"ts":1760949990000,"lon":151.7}`,                       // missing lat
		`{"ts":1760949990000.5,"lat":-32.9,"lon":151.7}`,         // non-integer time
		`{"lat":-32.9,"lon":151.7}`,                              // missing time
	}
	msg, dropped, err := ParseTelemetry(telemetry(strings.Join(fixes, ","), ""), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Fixes) != 1 || dropped != 8 {
		t.Fatalf("got %d fixes, %d dropped", len(msg.Fixes), dropped)
	}
}

func TestParseTelemetryRejects(t *testing.T) {
	tooMany := strings.TrimSuffix(strings.Repeat(validFix+",", MaxFixesPerMessage+1), ",")
	cases := map[string]struct {
		body    []byte
		wantErr string
	}{
		"bad version":         {[]byte(`{"v":2,"ts":1760949995000,"fixes":[` + validFix + `]}`), "protocol version"},
		"missing version":     {[]byte(`{"ts":1760949995000,"fixes":[` + validFix + `]}`), "protocol version"},
		"missing ts":          {[]byte(`{"v":1,"fixes":[` + validFix + `]}`), "ts missing or implausible"},
		"clock not set":       {[]byte(`{"v":1,"ts":5000,"fixes":[` + validFix + `]}`), "ts missing or implausible"},
		"empty fixes":         {telemetry("", ""), "non-empty"},
		"too many fixes":      {telemetry(tooMany, ""), "too many"},
		"no valid fixes":      {telemetry(`{"ts":1760949990000,"lat":0,"lon":0}`, ""), "no valid fixes"},
		"bad battery voltage": {telemetry(validFix, `,"batV":"low"`), "invalid telemetry"},
		"not json":            {[]byte(`hello`), "invalid JSON"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := ParseTelemetry(tc.body, now)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestParseStatus(t *testing.T) {
	s, err := ParseStatus([]byte(`{"v":1,"ts":1760949995000,"event":"boot","fw":"0.1.0","batV":4.1,"uptime":3}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Timestamp != 1760949995000 || s.Event != EventBoot || *s.FirmwareVersion != "0.1.0" || *s.BatteryVoltage != 4.1 || *s.Uptime != 3 {
		t.Fatalf("unexpected status: %+v", s)
	}

	for name, body := range map[string]string{
		"unknown event": `{"v":1,"ts":1760949995000,"event":"explode"}`,
		"missing ts":    `{"v":1,"event":"boot"}`,
	} {
		if _, err := ParseStatus([]byte(body), now); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
