package acme

import (
	"fmt"
	"sort"
	"strings"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"github.com/libdns/digitalocean"
	"github.com/libdns/hetzner"
)

// dnsProviderFactory builds a DNS-01 provider from its settings map.
type dnsProviderFactory func(settings map[string]string) (certmagic.DNSProvider, error)

// dnsProviders maps a provider name (as used in config [tls.dns] provider)
// to its factory. Settings keys per provider:
//
//	cloudflare:   api_token (required), zone_token (optional)
//	digitalocean: api_token (required)
//	hetzner:      api_token (required)
var dnsProviders = map[string]dnsProviderFactory{
	"cloudflare": func(s map[string]string) (certmagic.DNSProvider, error) {
		token, err := requireSetting("cloudflare", s, "api_token")
		if err != nil {
			return nil, err
		}
		return &cloudflare.Provider{APIToken: token, ZoneToken: s["zone_token"]}, nil
	},
	"digitalocean": func(s map[string]string) (certmagic.DNSProvider, error) {
		token, err := requireSetting("digitalocean", s, "api_token")
		if err != nil {
			return nil, err
		}
		return &digitalocean.Provider{APIToken: token}, nil
	},
	"hetzner": func(s map[string]string) (certmagic.DNSProvider, error) {
		token, err := requireSetting("hetzner", s, "api_token")
		if err != nil {
			return nil, err
		}
		return &hetzner.Provider{AuthAPIToken: token}, nil
	},
}

func requireSetting(provider string, s map[string]string, key string) (string, error) {
	v := strings.TrimSpace(s[key])
	if v == "" {
		return "", fmt.Errorf("acme: dns provider %q requires setting %q", provider, key)
	}
	return v, nil
}

// DNSProviders returns the names of the supported DNS-01 providers, sorted.
func DNSProviders() []string {
	names := make([]string, 0, len(dnsProviders))
	for n := range dnsProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// newDNSProvider builds the named provider; name is case-insensitive.
func newDNSProvider(name string, settings map[string]string) (certmagic.DNSProvider, error) {
	f, ok := dnsProviders[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return nil, fmt.Errorf("acme: unknown dns provider %q (supported: %s)", name, strings.Join(DNSProviders(), ", "))
	}
	return f(settings)
}
