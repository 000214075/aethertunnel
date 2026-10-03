package config

import (
	"strings"
	"testing"
)

// TestLocationsValidation covers the round-ten keys: locations split a
// hostname's paths on the http/https listeners, and host_header_rewrite
// renames the Host the local service sees.
func TestLocationsValidation(t *testing.T) {
	t.Run("a location must start with a slash", func(t *testing.T) {
		path := writeConfig(t, `
[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "api"
type = "http"
local_port = 8080
domains = ["split.example.com"]
locations = ["api"]
`)
		_, err := LoadClient(path)
		if err == nil || !strings.Contains(err.Error(), `location "api" must start with a slash`) {
			t.Fatalf("err = %v, want a leading-slash error", err)
		}
	})

	t.Run("locations on a tcp tunnel draw a warning", func(t *testing.T) {
		path := writeConfig(t, `
[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "tcp"
type = "tcp"
local_port = 8080
remote_port = 6000
locations = ["/api"]
`)
		cfg, err := LoadClient(path)
		if err != nil {
			t.Fatalf("LoadClient: %v", err)
		}
		if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "a tcp tunnel never parses HTTP and ignores them") {
			t.Fatalf("expected a locations warning, got %v", cfg.Warnings)
		}
	})

	t.Run("locations with tls_passthrough draw a warning", func(t *testing.T) {
		path := writeConfig(t, `
[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "site"
type = "https"
local_port = 8443
domains = ["site.example.com"]
locations = ["/api"]
tls_passthrough = true
`)
		cfg, err := LoadClient(path)
		if err != nil {
			t.Fatalf("LoadClient: %v", err)
		}
		if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "locations are ignored with tls_passthrough") {
			t.Fatalf("expected a passthrough warning, got %v", cfg.Warnings)
		}
	})

	t.Run("route_by_http_user on a tcp tunnel draws a warning", func(t *testing.T) {
		path := writeConfig(t, `
[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "tcp"
type = "tcp"
local_port = 8080
remote_port = 6000
route_by_http_user = "alice"
`)
		cfg, err := LoadClient(path)
		if err != nil {
			t.Fatalf("LoadClient: %v", err)
		}
		if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "a tcp tunnel never parses HTTP and ignores it") {
			t.Fatalf("expected a route_by_http_user warning, got %v", cfg.Warnings)
		}
	})

	t.Run("route_by_http_user round-trips", func(t *testing.T) {
		path := writeConfig(t, `
[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "api"
type = "http"
local_port = 8080
domains = ["split.example.com"]
route_by_http_user = "alice"
`)
		cfg, err := LoadClient(path)
		if err != nil {
			t.Fatalf("LoadClient: %v", err)
		}
		if cfg.Proxies[0].RouteByHTTPUser != "alice" {
			t.Fatalf("route_by_http_user round-tripped as %q", cfg.Proxies[0].RouteByHTTPUser)
		}
	})

	t.Run("locations and host_header_rewrite round-trip", func(t *testing.T) {
		path := writeConfig(t, `
[client]
server_addr = "127.0.0.1:7000"
auth_token = "0123456789abcdef0123456789abcdef"

[[proxies]]
name = "api"
type = "http"
local_port = 8080
domains = ["split.example.com"]
locations = ["/api", "/v2"]
host_header_rewrite = "api.internal"
`)
		cfg, err := LoadClient(path)
		if err != nil {
			t.Fatalf("LoadClient: %v", err)
		}
		proxy := cfg.Proxies[0]
		if len(proxy.Locations) != 2 || proxy.Locations[1] != "/v2" {
			t.Fatalf("locations round-tripped as %v", proxy.Locations)
		}
		if proxy.HostHeaderRewrite != "api.internal" {
			t.Fatalf("host_header_rewrite round-tripped as %q", proxy.HostHeaderRewrite)
		}
	})
}
