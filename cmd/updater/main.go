package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTTL        = 300
	defaultRecordType = "A"

	envAuthEmail  = "CF_AUTH_EMAIL"
	envAuthMethod = "CF_AUTH_METHOD"
	envAuthKey    = "CF_AUTH_KEY"
	envZoneID     = "CF_ZONE_ID"
	envRecordName = "CF_RECORD_NAME"
	envRecordType = "CF_RECORD_TYPE"
	envTTL        = "CF_TTL"
	envProxied    = "CF_PROXIED"
	envIPServices = "CF_IP_SERVICES"
)

var (
	defaultHTTPTimeout = 15 * time.Second

	defaultIPServices = []string{
		"https://api.ipify.org",
		"https://ipv4.icanhazip.com",
		"https://ipinfo.io/ip",
	}
)

// Config contains the runtime configuration required to talk to Cloudflare and
// determine the current public IP address.
type Config struct {
	AuthEmail  string
	AuthMethod string
	AuthKey    string
	ZoneID     string
	RecordName string
	RecordType string
	TTL        int
	Proxied    bool
	IPServices []string
}

// DNSRecord captures a Cloudflare DNS record response.
type DNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type listResponse struct {
	Success bool        `json:"success"`
	Errors  []apiError  `json:"errors"`
	Result  []DNSRecord `json:"result"`
}

type updateResponse struct {
	Success bool       `json:"success"`
	Errors  []apiError `json:"errors"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	log.SetFlags(log.LstdFlags)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	client := &http.Client{Timeout: defaultHTTPTimeout}

	ip, err := discoverIP(client, cfg.IPServices)
	if err != nil {
		log.Fatalf("failed to determine public IP: %v", err)
	}
	log.Printf("detected public IP: %s", ip)

	record, err := fetchDNSRecord(client, cfg)
	if err != nil {
		log.Fatalf("failed to fetch DNS record: %v", err)
	}

	if record.Content == ip {
		log.Printf("Cloudflare record %s already up to date", record.Name)
		return
	}

	if err := updateDNSRecord(client, cfg, record.ID, ip); err != nil {
		log.Fatalf("failed to update DNS record: %v", err)
	}

	log.Printf("successfully updated %s from %s to %s", record.Name, record.Content, ip)
}

func loadConfig() (Config, error) {
	cfg := Config{
		AuthEmail:  strings.TrimSpace(os.Getenv(envAuthEmail)),
		AuthMethod: strings.ToLower(strings.TrimSpace(os.Getenv(envAuthMethod))),
		AuthKey:    strings.TrimSpace(os.Getenv(envAuthKey)),
		ZoneID:     strings.TrimSpace(os.Getenv(envZoneID)),
		RecordName: strings.TrimSpace(os.Getenv(envRecordName)),
		RecordType: strings.ToUpper(strings.TrimSpace(os.Getenv(envRecordType))),
	}

	if cfg.AuthMethod == "" {
		cfg.AuthMethod = "token"
	}

	if cfg.RecordType == "" {
		cfg.RecordType = defaultRecordType
	}

	ttlValue := strings.TrimSpace(os.Getenv(envTTL))
	if ttlValue == "" {
		cfg.TTL = defaultTTL
	} else {
		ttl, err := strconv.Atoi(ttlValue)
		if err != nil || ttl < 60 {
			return Config{}, fmt.Errorf("invalid %s value %q", envTTL, ttlValue)
		}
		cfg.TTL = ttl
	}

	proxiedValue := strings.TrimSpace(os.Getenv(envProxied))
	switch strings.ToLower(proxiedValue) {
	case "", "false":
		cfg.Proxied = false
	case "true":
		cfg.Proxied = true
	default:
		return Config{}, fmt.Errorf("invalid %s value %q", envProxied, proxiedValue)
	}

	servicesValue := strings.TrimSpace(os.Getenv(envIPServices))
	if servicesValue == "" {
		cfg.IPServices = append([]string{}, defaultIPServices...)
	} else {
		raw := strings.Split(servicesValue, ",")
		for _, svc := range raw {
			trimmed := strings.TrimSpace(svc)
			if trimmed != "" {
				cfg.IPServices = append(cfg.IPServices, trimmed)
			}
		}
		if len(cfg.IPServices) == 0 {
			cfg.IPServices = append([]string{}, defaultIPServices...)
		}
	}

	if cfg.AuthKey == "" {
		return Config{}, fmt.Errorf("%s is required", envAuthKey)
	}

	switch cfg.AuthMethod {
	case "token":
		if cfg.AuthEmail == "" {
			log.Printf("warning: %s is empty; API tokens typically do not require it", envAuthEmail)
		}
	case "global":
		if cfg.AuthEmail == "" {
			return Config{}, fmt.Errorf("%s is required when %s is 'global'", envAuthEmail, envAuthMethod)
		}
	default:
		return Config{}, fmt.Errorf("unsupported %s %q (must be 'token' or 'global')", envAuthMethod, cfg.AuthMethod)
	}

	if cfg.ZoneID == "" {
		return Config{}, fmt.Errorf("%s is required", envZoneID)
	}

	if cfg.RecordName == "" {
		return Config{}, fmt.Errorf("%s is required", envRecordName)
	}

	if cfg.RecordType != "A" {
		return Config{}, fmt.Errorf("unsupported %s %q (only A records are handled)", envRecordType, cfg.RecordType)
	}

	return cfg, nil
}

func discoverIP(client *http.Client, services []string) (string, error) {
	for _, svc := range services {
		req, err := http.NewRequest(http.MethodGet, svc, nil)
		if err != nil {
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("failed to query %s: %v", svc, err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("failed to read response from %s: %v", svc, err)
			continue
		}

		ip := strings.TrimSpace(string(body))
		parsed := net.ParseIP(ip)
		if parsed == nil {
			log.Printf("invalid IP %q from %s", ip, svc)
			continue
		}

		parsed4 := parsed.To4()
		if parsed4 == nil {
			log.Printf("non-IPv4 address %q from %s", ip, svc)
			continue
		}

		return parsed4.String(), nil
	}

	return "", errors.New("unable to discover IPv4 address from configured services")
}

func fetchDNSRecord(client *http.Client, cfg Config) (DNSRecord, error) {
	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?type=%s&name=%s", cfg.ZoneID, cfg.RecordType, cfg.RecordName)

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return DNSRecord{}, err
	}

	applyAuthHeaders(req, cfg)

	resp, err := client.Do(req)
	if err != nil {
		return DNSRecord{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return DNSRecord{}, fmt.Errorf("unexpected status %s", resp.Status)
	}

	var payload listResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return DNSRecord{}, err
	}

	if !payload.Success {
		return DNSRecord{}, fmt.Errorf("cloudflare error: %v", payload.Errors)
	}

	if len(payload.Result) == 0 {
		return DNSRecord{}, fmt.Errorf("no matching record for %s", cfg.RecordName)
	}

	return payload.Result[0], nil
}

func updateDNSRecord(client *http.Client, cfg Config, recordID, newIP string) error {
	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", cfg.ZoneID, recordID)

	body := map[string]any{
		"type":    cfg.RecordType,
		"name":    cfg.RecordName,
		"content": newIP,
		"ttl":     cfg.TTL,
		"proxied": cfg.Proxied,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPatch, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	applyAuthHeaders(req, cfg)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	var result updateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	if !result.Success {
		return fmt.Errorf("cloudflare update failed: %v", result.Errors)
	}

	return nil
}

func applyAuthHeaders(req *http.Request, cfg Config) {
	if cfg.AuthEmail != "" {
		req.Header.Set("X-Auth-Email", cfg.AuthEmail)
	}

	if cfg.AuthMethod == "global" {
		req.Header.Set("X-Auth-Key", cfg.AuthKey)
		return
	}

	req.Header.Set("Authorization", "Bearer "+cfg.AuthKey)
}
