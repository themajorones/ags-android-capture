package bridge

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type streamRequest struct {
	serial  string
	bitrate int
	fps     int
	maxSize int
}

func parseStreamRequest(values url.Values) (streamRequest, error) {
	serial := strings.TrimSpace(values.Get("serial"))
	if !validSerial(serial) {
		return streamRequest{}, fmt.Errorf("serial must be an ADB device id or host:port")
	}

	bitrate, err := queryInt(values, "bitrate", 4_000_000, 100_000, 25_000_000)
	if err != nil {
		return streamRequest{}, err
	}
	fps, err := queryInt(values, "fps", 30, 1, 60)
	if err != nil {
		return streamRequest{}, err
	}
	maxSize, err := queryInt(values, "maxSize", 0, 0, 4096)
	if err != nil {
		return streamRequest{}, err
	}
	return streamRequest{serial: serial, bitrate: bitrate, fps: fps, maxSize: maxSize}, nil
}

func queryInt(values url.Values, key string, fallback, min, max int) (int, error) {
	raw := values.Get(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("%s must be between %d and %d", key, min, max)
	}
	return value, nil
}

func validSerial(serial string) bool {
	if serial == "" || len(serial) > 255 {
		return false
	}
	for _, r := range serial {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("._:-[]", r)) {
			return false
		}
	}
	if strings.Contains(serial, ":") {
		part := serial[strings.LastIndex(serial, ":")+1:]
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
		port, err := strconv.Atoi(part)
		return err == nil && port > 0 && port <= 65535
	}
	return true
}
