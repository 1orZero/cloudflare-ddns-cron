package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestLoadConfigSuccessToken(t *testing.T) {
	t.Setenv(envAuthKey, "token-value")
	t.Setenv(envZoneID, "zone-id")
	t.Setenv(envRecordName, "example.com")
	t.Setenv(envAuthMethod, "TOKEN")
	t.Setenv(envAuthEmail, "user@example.com")
	t.Setenv(envTTL, "600")
	t.Setenv(envProxied, "true")
	t.Setenv(envIPServices, "https://service.one, https://service.two")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if cfg.AuthMethod != "token" {
		t.Fatalf("expected auth method token, got %q", cfg.AuthMethod)
	}
	if cfg.TTL != 600 {
		t.Fatalf("expected TTL 600, got %d", cfg.TTL)
	}
	if !cfg.Proxied {
		t.Fatalf("expected proxied true")
	}
	expectedServices := []string{"https://service.one", "https://service.two"}
	if !reflect.DeepEqual(cfg.IPServices, expectedServices) {
		t.Fatalf("unexpected IP services: %v", cfg.IPServices)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv(envAuthKey, "token-value")
	t.Setenv(envZoneID, "zone-id")
	t.Setenv(envRecordName, "example.com")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if cfg.TTL != defaultTTL {
		t.Fatalf("expected default TTL %d, got %d", defaultTTL, cfg.TTL)
	}
	if cfg.RecordType != defaultRecordType {
		t.Fatalf("expected record type %s, got %s", defaultRecordType, cfg.RecordType)
	}
	if !reflect.DeepEqual(cfg.IPServices, defaultIPServices) {
		t.Fatalf("expected default services, got %v", cfg.IPServices)
	}
}

func TestLoadConfigMissingAuthKey(t *testing.T) {
	t.Setenv(envAuthKey, "")
	t.Setenv(envZoneID, "zone-id")
	t.Setenv(envRecordName, "example.com")

	if _, err := loadConfig(); err == nil {
		t.Fatalf("expected error when auth key missing")
	}
}

func TestDiscoverIP(t *testing.T) {
	invalidServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(invalidServer.Close)

	badIPServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not-an-ip"))
	}))
	t.Cleanup(badIPServer.Close)

	validServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("203.0.113.10"))
	}))
	t.Cleanup(validServer.Close)

	client := &http.Client{}

	ip, err := discoverIP(client, []string{invalidServer.URL, badIPServer.URL, validServer.URL})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if ip != "203.0.113.10" {
		t.Fatalf("unexpected IP %s", ip)
	}
}

func TestDiscoverIPAllFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("invalid"))
	}))
	t.Cleanup(server.Close)

	client := &http.Client{}

	if _, err := discoverIP(client, []string{server.URL}); err == nil {
		t.Fatalf("expected error when all services fail")
	}
}

func TestFetchDNSRecord(t *testing.T) {
	responsePayload := map[string]any{
		"success":  true,
		"errors":   []any{},
		"messages": []any{},
		"result": []map[string]any{
			{
				"id":          "record-id",
				"type":        "A",
				"name":        "example.com",
				"content":     "198.51.100.2",
				"proxied":     false,
				"proxiable":   true,
				"comment":     "",
				"tags":        []any{},
				"ttl":         120,
				"data":        map[string]any{},
				"priority":    0,
				"created_on":  "2024-01-01T00:00:00Z",
				"modified_on": "2024-01-01T00:00:00Z",
			},
		},
		"result_info": map[string]any{
			"page":     1,
			"per_page": 1,
		},
	}
	payload, err := json.Marshal(responsePayload)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var capturedAuth string

	httpClient := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			capturedAuth = req.Header.Get("Authorization")
			expectedPath := "/client/v4/zones/zone-id/dns_records"
			if req.URL.Path != expectedPath {
				t.Fatalf("unexpected path %s", req.URL.Path)
			}
			query := req.URL.Query()
			if query.Get("type") != "A" || query.Get("name") != "example.com" {
				t.Fatalf("unexpected query %s", req.URL.RawQuery)
			}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(payload)),
				Header:     make(http.Header),
			}
			resp.Header.Set("Content-Type", "application/json")
			return resp, nil
		}),
	}

	cfg := Config{
		AuthMethod: "token",
		AuthKey:    "token-value",
		ZoneID:     "zone-id",
		RecordName: "example.com",
		RecordType: "A",
	}

	client, err := newCloudflareClient(httpClient, cfg)
	if err != nil {
		t.Fatalf("unexpected client error: %v", err)
	}

	record, err := fetchDNSRecord(context.Background(), client, cfg)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if record.ID != "record-id" {
		t.Fatalf("unexpected record ID %s", record.ID)
	}
	if capturedAuth != "Bearer token-value" {
		t.Fatalf("unexpected auth header %s", capturedAuth)
	}
	if ip, err := extractARecordIP(record); err != nil || ip != "198.51.100.2" {
		t.Fatalf("unexpected record content: %v %s", err, ip)
	}
}

func TestUpdateDNSRecord(t *testing.T) {
	var receivedBody []byte

	httpClient := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPut {
				t.Fatalf("expected PUT, got %s", req.Method)
			}
			if req.Header.Get("X-Auth-Key") != "global-key" {
				t.Fatalf("expected global auth key header")
			}
			if req.Header.Get("X-Auth-Email") != "user@example.com" {
				t.Fatalf("expected auth email header")
			}
			var err error
			receivedBody, err = io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read body err: %v", err)
			}
			responsePayload := map[string]any{
				"success":  true,
				"errors":   []any{},
				"messages": []any{},
				"result": map[string]any{
					"id": "record-id",
				},
			}
			body, err := json.Marshal(responsePayload)
			if err != nil {
				t.Fatalf("marshal response err: %v", err)
			}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(body)),
				Header:     make(http.Header),
			}
			resp.Header.Set("Content-Type", "application/json")
			return resp, nil
		}),
	}

	cfg := Config{
		AuthMethod: "global",
		AuthKey:    "global-key",
		AuthEmail:  "user@example.com",
		ZoneID:     "zone-id",
		RecordName: "example.com",
		RecordType: "A",
		TTL:        120,
		Proxied:    true,
	}

	client, err := newCloudflareClient(httpClient, cfg)
	if err != nil {
		t.Fatalf("unexpected client error: %v", err)
	}

	if err := updateDNSRecord(context.Background(), client, cfg, "record-id", "198.51.100.3"); err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(receivedBody, &payload); err != nil {
		t.Fatalf("json unmarshal err: %v", err)
	}
	if payload["content"] != "198.51.100.3" {
		t.Fatalf("unexpected content %v", payload["content"])
	}
	if payload["proxied"] != true {
		t.Fatalf("expected proxied flag true")
	}
	if payload["ttl"] != float64(120) {
		t.Fatalf("expected ttl 120, got %v", payload["ttl"])
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
