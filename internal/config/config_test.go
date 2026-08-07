package config

import (
	"os"
	"path/filepath"
	"reflect"
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

func TestLoadLifecycleConfig(t *testing.T) {
	path := writeConfig(t, `[network]
ipv4 = "10.76.111.1/24"
ipv6 = "fd42:28e2:2375:7000::1/64"
[base_image]
name = "sandbox"
source = "https://images.linuxcontainers.org"
image = "debian/13"
authorized_hosts = ["github.com", "gitlab.com"]
[workspace]
pool = "default"
volume = "workspace"
repos = ["owner/repo", "other/project"]
[workspace.incus]
volume = "nested-state"
images = ["images:debian/13", "images:ubuntu/24.04"]
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := cfg.BaseImage, (BaseImage{
		Name:            "sandbox",
		Source:          "https://images.linuxcontainers.org",
		Image:           "debian/13",
		AuthorizedHosts: []string{"github.com", "gitlab.com"},
	}); !reflect.DeepEqual(got, want) {
		t.Errorf("BaseImage = %#v, want %#v", got, want)
	}
	if got, want := cfg.Workspace, (Workspace{
		Pool:   "default",
		Volume: "workspace",
		Repos:  []string{"owner/repo", "other/project"},
		Incus: IncusWorkspace{
			Volume: "nested-state",
			Images: []string{"images:debian/13", "images:ubuntu/24.04"},
		},
	}); !reflect.DeepEqual(got, want) {
		t.Errorf("Workspace = %#v, want %#v", got, want)
	}
	if got := cfg.Workspace.Incus.Volume; got != "nested-state" {
		t.Errorf("Workspace.Incus.Volume = %q, want nested-state", got)
	}
}

func TestLoadLifecycleDefaultsAndPaths(t *testing.T) {
	t.Chdir(t.TempDir())
	path := filepath.Join("config", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(`[network]
ipv4 = "10.76.111.1/24"
[base_image]
name = "sandbox"
source = "https://images.linuxcontainers.org"
image = "debian/13"
[workspace]
repos = []
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.Volume != DefaultWorkspaceVolume {
		t.Fatalf("Workspace.Volume = %q, want %q", cfg.Workspace.Volume, DefaultWorkspaceVolume)
	}
	if got := cfg.Workspace.Incus.Volume; got != DefaultIncusWorkspaceVolume {
		t.Fatalf("Workspace.Incus.Volume = %q, want %q", got, DefaultIncusWorkspaceVolume)
	}
	if cfg.Workspace.Incus.Images != nil {
		t.Fatalf("Workspace.Incus.Images = %#v, want nil", cfg.Workspace.Incus.Images)
	}
	wantDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		t.Fatalf("resolve config directory: %v", err)
	}
	if cfg.Dir != wantDir {
		t.Errorf("Dir = %q, want %q", cfg.Dir, wantDir)
	}
	if got := cfg.AssetPath("tmux.conf"); got != filepath.Join(wantDir, "assets", "tmux.conf") {
		t.Errorf("AssetPath() = %q, want %q", got, filepath.Join(wantDir, "assets", "tmux.conf"))
	}
	if err := cfg.ValidateLifecycle(); err != nil {
		t.Fatalf("ValidateLifecycle() error = %v", err)
	}
}

func TestValidateLifecycleRequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name: "missing name",
			cfg: Config{BaseImage: BaseImage{
				Source: "https://images.linuxcontainers.org",
				Image:  "debian/13",
			}},
			wantErr: "base_image.name is required",
		},
		{
			name: "missing source",
			cfg: Config{BaseImage: BaseImage{
				Name:  "sandbox",
				Image: "debian/13",
			}},
			wantErr: "base_image.source is required",
		},
		{
			name: "missing image",
			cfg: Config{BaseImage: BaseImage{
				Name:   "sandbox",
				Source: "https://images.linuxcontainers.org",
			}},
			wantErr: "base_image.image is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.ValidateLifecycle(); err == nil || err.Error() != tt.wantErr {
				t.Fatalf("ValidateLifecycle() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateLifecycleRejectsIdenticalWorkspaceSeeds(t *testing.T) {
	cfg := Config{
		BaseImage: BaseImage{Name: "sandbox", Source: "images:", Image: "debian/13"},
		Workspace: Workspace{
			Volume: "shared-seed",
			Incus:  IncusWorkspace{Volume: "shared-seed"},
		},
	}
	if err := cfg.ValidateLifecycle(); err == nil || !strings.Contains(err.Error(), "must be different") {
		t.Fatalf("ValidateLifecycle() error = %v, want distinct seed validation", err)
	}
}

func TestValidateLifecycleAllowsEmptyLists(t *testing.T) {
	cfg := Config{
		BaseImage: BaseImage{
			Name:            "sandbox",
			Source:          "https://images.linuxcontainers.org",
			Image:           "debian/13",
			AuthorizedHosts: []string{},
		},
		Workspace: Workspace{Repos: []string{}},
	}

	if err := cfg.ValidateLifecycle(); err != nil {
		t.Fatalf("ValidateLifecycle() error = %v", err)
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

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
