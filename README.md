# Cloudflare DDNS Updater

This repository ships a small Go binary that keeps a single Cloudflare DNS A record in sync with the machine's current public IPv4 address. It discovers the IP through a configurable list of HTTPS endpoints, then uses the official Cloudflare Go SDK to update the record only when the address changes.

## Requirements

- Go 1.25.1 to build the binary locally (a prebuilt binary can be used on any platform supported by Go).
- A Cloudflare API *token* with **Zone → DNS → Edit** permission scoped to the zone that owns the record you want to manage (global API keys are not required or recommended).
- Outbound HTTPS access to the public-IP services you configure and to `api.cloudflare.com`.

## Environment Variables

Set these before running the binary (for example in your shell profile, systemd unit, or scheduler configuration):

```
CF_AUTH_METHOD=token                # optional but recommended; defaults to "token"
CF_AUTH_KEY=<cloudflare_api_token>  # required
CF_ZONE_ID=<zone_id>                # required
CF_RECORD_NAME=<fqdn>               # required (e.g. explorator.veraze.io)
CF_TTL=<seconds>                    # optional, defaults to 300; must be >= 60
CF_PROXIED=true|false               # optional, defaults to false when unset
CF_IP_SERVICES=url1,url2,...        # optional comma-separated list; defaults to
                                    #   https://api.ipify.org,
                                    #   https://ipv4.icanhazip.com,
                                    #   https://ipinfo.io/ip
```

With token authentication, `CF_AUTH_EMAIL` is not required. The overall configuration logic lives in `cmd/updater/main.go` if you need deeper detail.

## Build

```
go build -o bin/updater ./cmd/updater
```

## Run

Once the environment variables are in place:

```
bin/updater
```

The program logs the discovered public IP, fetches the current Cloudflare record, and updates it only when the content differs. A successful run exits cleanly; any configuration or API errors abort with a descriptive message.

## Automating

- **cron / launchd / systemd**: export the environment variables inside the job definition or point the service to an `EnvironmentFile` containing the lines above.
- **Containers / CI**: inject the same variables via runtime configuration (`docker run -e ...`) or your CI secret manager.
- **Secret managers**: for better hygiene, resolve the token from macOS Keychain, AWS Secrets Manager, Vault, etc., and export it just-in-time before executing the binary.

Schedule the binary at whatever cadence matches your ISP’s lease behavior (for example every 5–10 minutes). Each run is idempotent: if the public IP hasn’t changed, the updater exits after logging that the record is already up to date.
