package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want4   string
		want6   string
		wantErr string
	}{
		{name: "ipv4 only", content: "[network]\nipv4 = \"10.76.111.1/24\"\n", want4: "10.76.111.1/24"},
		{name: "dual stack", content: "[network]\nipv4 = \"10.76.111.1/24\"\nipv6 = \"fd42:28e2:2375:7000::1/64\"\n", want4: "10.76.111.1/24", want6: "fd42:28e2:2375:7000::1/64"},
		{name: "missing ipv4", content: "[network]\n", wantErr: "network.ipv4 is required"},
		{name: "invalid ipv4", content: "[network]\nipv4 = \"bad\"\n", wantErr: "network.ipv4"},
		{name: "wrong ipv4 family", content: "[network]\nipv4 = \"fd42::1/64\"\n", wantErr: "must be IPv4"},
		{name: "invalid ipv6", content: "[network]\nipv4 = \"10.76.111.1/24\"\nipv6 = \"bad\"\n", wantErr: "network.ipv6"},
		{name: "wrong ipv6 family", content: "[network]\nipv4 = \"10.76.111.1/24\"\nipv6 = \"10.0.0.1/24\"\n", wantErr: "must be IPv6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			cfg, err := Load(path)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Load() error = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %q, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}

			prefix4, err := cfg.Network.IPv4Prefix()
			if err != nil {
				t.Fatalf("IPv4Prefix() error = %v", err)
			}
			if got := prefix4.String(); got != tt.want4 {
				t.Errorf("IPv4Prefix() = %q, want %q", got, tt.want4)
			}

			prefix6, present, err := cfg.Network.IPv6Prefix()
			if err != nil {
				t.Fatalf("IPv6Prefix() error = %v", err)
			}
			if tt.want6 == "" {
				if present {
					t.Errorf("IPv6Prefix() present = true, want false")
				}
				return
			}
			if !present {
				t.Fatalf("IPv6Prefix() present = false, want true")
			}
			if got := prefix6.String(); got != tt.want6 {
				t.Errorf("IPv6Prefix() = %q, want %q", got, tt.want6)
			}
		})
	}
}

func TestLoadReadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.toml")

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want read error")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Fatalf("Load() error = %q, want error identifying config read", err)
	}
}

func TestLoadDecodeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[network\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want decode error")
	}
	if !strings.Contains(err.Error(), "decode config") {
		t.Fatalf("Load() error = %q, want error identifying config decode", err)
	}
}
