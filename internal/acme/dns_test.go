package acme

import (
	"reflect"
	"testing"
)

func TestDNSProvidersList(t *testing.T) {
	want := []string{"cloudflare", "digitalocean", "hetzner"}
	if got := DNSProviders(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DNSProviders() = %v, want %v", got, want)
	}
}

func TestNewDNSProvider(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		settings map[string]string
		wantErr  bool
	}{
		{"cloudflare ok", "cloudflare", map[string]string{"api_token": "t"}, false},
		{"cloudflare case-insensitive", " CloudFlare ", map[string]string{"api_token": "t", "zone_token": "z"}, false},
		{"cloudflare missing token", "cloudflare", nil, true},
		{"cloudflare blank token", "cloudflare", map[string]string{"api_token": "  "}, true},
		{"digitalocean ok", "digitalocean", map[string]string{"api_token": "t"}, false},
		{"digitalocean missing", "digitalocean", map[string]string{}, true},
		{"hetzner ok", "hetzner", map[string]string{"api_token": "t"}, false},
		{"hetzner missing", "hetzner", nil, true},
		{"unknown", "namecheap", map[string]string{"api_token": "t"}, true},
		{"empty", "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := newDNSProvider(tc.provider, tc.settings)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got provider %T", p)
				}
				return
			}
			if err != nil || p == nil {
				t.Fatalf("got %T, %v", p, err)
			}
		})
	}
}
