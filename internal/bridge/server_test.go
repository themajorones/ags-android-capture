package bridge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamChecksOriginAndRequestBeforeUpgrading(t *testing.T) {
	server := New(Config{AllowedOrigins: []string{"http://ags.example:7530"}})
	handler := http.HandlerFunc(server.Stream)

	request := httptest.NewRequest(http.MethodGet, "/v1/stream?serial=device-1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing Origin returned %d, want %d", response.Code, http.StatusForbidden)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/stream?serial=device-1", nil)
	request.Header.Set("Origin", "http://evil.example")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unlisted Origin returned %d, want %d", response.Code, http.StatusForbidden)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/stream?serial=bad%3Bcommand", nil)
	request.Header.Set("Origin", "http://ags.example:7530")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid request returned %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestHealthReportsIdleBridge(t *testing.T) {
	server := New(Config{})
	response := httptest.NewRecorder()
	server.Health(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Body.String(); got == "" || !strings.Contains(got, `"status":"ok"`) || !strings.Contains(got, `"streamActive":false`) {
		t.Fatalf("unexpected health response: %s", got)
	}
}
