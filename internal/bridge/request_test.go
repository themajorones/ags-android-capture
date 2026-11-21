package bridge

import (
	"net/url"
	"testing"
)

func TestParseStreamRequest(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantSerial string
		wantFPS    int
		wantRate   int
		wantErr    bool
	}{
		{"defaults", "serial=device-1", "device-1", 30, 4_000_000, false},
		{"requested stream settings", "serial=192.168.1.4%3A5555&fps=24&bitrate=1500000&maxSize=1080", "192.168.1.4:5555", 24, 1_500_000, false},
		{"invalid device id", "serial=bad%3Bcommand", "", 0, 0, true},
		{"invalid port", "serial=host%3A70000", "", 0, 0, true},
		{"fps too high", "serial=host%3A5555&fps=61", "", 0, 0, true},
		{"bitrate too low", "serial=host%3A5555&bitrate=99", "", 0, 0, true},
		{"max size too high", "serial=host%3A5555&maxSize=10000", "", 0, 0, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := parseStreamRequest(parseQuery(test.query))
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
			if err == nil && (request.serial != test.wantSerial || request.fps != test.wantFPS || request.bitrate != test.wantRate) {
				t.Fatalf("request = %#v", request)
			}
		})
	}
}

func TestValidSerial(t *testing.T) {
	valid := []string{"device", "emulator-5554", "host.local:5555", "[2001:db8::10]:5555"}
	for _, serial := range valid {
		if !validSerial(serial) {
			t.Errorf("validSerial(%q) = false", serial)
		}
	}
	invalid := []string{"", "host:", "host:abc", "host:0", "host:65536", "host;command", "host/path", "host name"}
	for _, serial := range invalid {
		if validSerial(serial) {
			t.Errorf("validSerial(%q) = true", serial)
		}
	}
}

func parseQuery(raw string) url.Values {
	values, err := url.ParseQuery(raw)
	if err != nil {
		panic(err)
	}
	return values
}
