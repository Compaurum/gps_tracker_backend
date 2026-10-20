package protocol

import (
	"strings"
	"testing"
)

const now = int64(1760950000)

const validFix = `{"t":1760949990,"lat":-32.9283,"lon":151.7817,"alt":12.3,"spd":4.2,"crs":270,"sat":9,"hdop":0.9}`

func TestParseTelemetryValid(t *testing.T) {
	msg, dropped, err := ParseTelemetry([]byte(`{"v":1,"seq":7,"fixes":[`+validFix+`],"bat":3.98,"rssi":-71}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 || len(msg.Fixes) != 1 || *msg.Seq != 7 || *msg.Bat != 3.98 || *msg.Rssi != -71 {
		t.Fatalf("unexpected result: %+v dropped=%d", msg, dropped)
	}
	f := msg.Fixes[0]
	if f.T != 1760949990 || f.Lat != -32.9283 || f.Lon != 151.7817 || *f.Sat != 9 {
		t.Fatalf("unexpected fix: %+v", f)
	}
}

func TestParseTelemetryDropsInvalidFixes(t *testing.T) {
	fixes := []string{
		validFix,
		`{"t":1760949990,"lat":0,"lon":0}`,                   // no-fix placeholder
		`{"t":0,"lat":-32.9,"lon":151.7}`,                    // GPS time not acquired
		`{"t":1760953600,"lat":-32.9,"lon":151.7}`,           // in the future
		`{"t":1760949990,"lat":91,"lon":151.7}`,              // out of range
		`{"t":1760949990,"lat":-32.9,"lon":151.7,"spd":"4"}`, // wrong type
		`{"t":1760949990,"lon":151.7}`,                       // missing lat
		`{"t":1760949990.5,"lat":-32.9,"lon":151.7}`,         // non-integer time
	}
	msg, dropped, err := ParseTelemetry([]byte(`{"v":1,"fixes":[`+strings.Join(fixes, ",")+`]}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Fixes) != 1 || dropped != 7 {
		t.Fatalf("got %d fixes, %d dropped", len(msg.Fixes), dropped)
	}
}

func TestParseTelemetryRejects(t *testing.T) {
	tooMany := strings.TrimSuffix(strings.Repeat(validFix+",", MaxFixesPerMessage+1), ",")
	cases := map[string]struct{ body, wantErr string }{
		"bad version":     {`{"v":2,"fixes":[` + validFix + `]}`, "version"},
		"missing version": {`{"fixes":[` + validFix + `]}`, "version"},
		"empty fixes":     {`{"v":1,"fixes":[]}`, "non-empty"},
		"too many fixes":  {`{"v":1,"fixes":[` + tooMany + `]}`, "too many"},
		"no valid fixes":  {`{"v":1,"fixes":[{"t":1760949990,"lat":0,"lon":0}]}`, "no valid fixes"},
		"bad battery":     {`{"v":1,"fixes":[` + validFix + `],"bat":"low"}`, "invalid telemetry"},
		"not json":        {`hello`, "invalid JSON"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := ParseTelemetry([]byte(tc.body), now)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestParseStatus(t *testing.T) {
	s, err := ParseStatus([]byte(`{"v":1,"ev":"boot","fw":"0.1.0","bat":4.1,"up":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Ev != EventBoot || *s.Fw != "0.1.0" || *s.Up != 3 {
		t.Fatalf("unexpected status: %+v", s)
	}

	if _, err := ParseStatus([]byte(`{"v":1,"ev":"explode"}`)); err == nil {
		t.Fatal("want error for unknown event")
	}
}
