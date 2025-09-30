package main

import (
	"context"
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

	"github.com/cloudflare/cloudflare-go/v2"
	"github.com/cloudflare/cloudflare-go/v2/dns"
	"github.com/cloudflare/cloudflare-go/v2/option"
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

func main() {
	log.SetFlags(log.LstdFlags)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	httpClient := &http.Client{Timeout: defaultHTTPTimeout}

	ip, err := discoverIP(httpClient, cfg.IPServices)
	if err != nil {
		log.Fatalf("failed to determine public IP: %v", err)
	}
	log.Printf("detected public IP: %s", ip)

	cfClient, err := newCloudflareClient(httpClient, cfg)
	if err != nil {
		log.Fatalf("failed to configure Cloudflare client: %v", err)
	}

	ctx := context.Background()

	record, err := fetchDNSRecord(ctx, cfClient, cfg)
	if err != nil {
		log.Fatalf("failed to fetch DNS record: %v", err)
	}

	currentIP, err := extractARecordIP(record)
	if err != nil {
		log.Fatalf("unexpected DNS record content: %v", err)
	}

	if currentIP == ip {
		log.Printf("Cloudflare record %s already up to date", record.Name)
		return
	}

	if err := updateDNSRecord(ctx, cfClient, cfg, record.ID, ip); err != nil {
		log.Fatalf("failed to update DNS record: %v", err)
	}

	log.Printf("successfully updated %s from %s to %s", record.Name, currentIP, ip)
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

func newCloudflareClient(httpClient *http.Client, cfg Config) (*cloudflare.Client, error) {
	options := []option.RequestOption{option.WithHTTPClient(httpClient)}

	switch cfg.AuthMethod {
	case "token":
		options = append(options, option.WithAPIToken(cfg.AuthKey))
	case "global":
		options = append(options, option.WithAPIKey(cfg.AuthKey), option.WithAPIEmail(cfg.AuthEmail))
	default:
		return nil, fmt.Errorf("unsupported auth method %q", cfg.AuthMethod)
	}

	return cloudflare.NewClient(options...), nil
}

func fetchDNSRecord(ctx context.Context, client *cloudflare.Client, cfg Config) (dns.Record, error) {
	params := dns.RecordListParams{
		ZoneID: cloudflare.String(cfg.ZoneID),
		Name:   cloudflare.String(cfg.RecordName),
		Type:   cloudflare.F(dns.RecordListParamsType(cfg.RecordType)),
	}

	page, err := client.DNS.Records.List(ctx, params)
	if err != nil {
		return dns.Record{}, err
	}

	if len(page.Result) == 0 {
		return dns.Record{}, fmt.Errorf("no matching record for %s", cfg.RecordName)
	}

	return page.Result[0], nil
}

func extractARecordIP(record dns.Record) (string, error) {
	union := record.AsUnion()
	aRecord, ok := union.(dns.ARecord)
	if !ok {
		return "", fmt.Errorf("record type %q is not supported", record.Type)
	}

	return strings.TrimSpace(aRecord.Content), nil
}

func updateDNSRecord(ctx context.Context, client *cloudflare.Client, cfg Config, recordID, newIP string) error {
	params := dns.RecordUpdateParams{
		ZoneID: cloudflare.String(cfg.ZoneID),
		Record: dns.ARecordParam{
			Name:    cloudflare.String(cfg.RecordName),
			Content: cloudflare.String(newIP),
			Type:    cloudflare.F(dns.ARecordTypeA),
			TTL:     cloudflare.F(dns.TTL(float64(cfg.TTL))),
			Proxied: cloudflare.F(cfg.Proxied),
		},
	}

	_, err := client.DNS.Records.Update(ctx, recordID, params)
	return err
}
