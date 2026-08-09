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
build_scripts_dir = "image-build.d"
[workspace]
pool = "default"
volume = "workspace"
repos = ["owner/repo", "other/project"]
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
		BuildScriptsDir: "image-build.d",
	}); !reflect.DeepEqual(got, want) {
		t.Errorf("BaseImage = %#v, want %#v", got, want)
	}
	if got, want := cfg.Workspace, (Workspace{
		Pool:   "default",
		Volume: "workspace",
		Repos:  []string{"owner/repo", "other/project"},
	}); !reflect.DeepEqual(got, want) {
		t.Errorf("Workspace = %#v, want %#v", got, want)
	}
}

func TestBuildScriptsPath(t *testing.T) {
	dir := t.TempDir()
	absolute := filepath.Join(t.TempDir(), "scripts")
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "omitted", cfg: Config{Dir: dir}, want: ""},
		{name: "relative", cfg: Config{Dir: dir, BaseImage: BaseImage{BuildScriptsDir: "image-build.d"}}, want: filepath.Join(dir, "image-build.d")},
		{name: "absolute", cfg: Config{Dir: dir, BaseImage: BaseImage{BuildScriptsDir: absolute}}, want: absolute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.BuildScriptsPath(); got != tt.want {
				t.Fatalf("BuildScriptsPath() = %q, want %q", got, tt.want)
			}
		})
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

func TestLoadWorkers(t *testing.T) {
	path := writeConfig(t, `[network]
ipv4 = "10.76.111.1/24"

[models.local-qwen]
label = "Local Qwen"
provider = "local-executor"
model = "Qwen3.6-27B-GGUF"
thinking_levels = ["off"]
default_thinking_level = "off"

[models.gpt-5-6-sol]
label = "GPT-5.6 Solver"
provider = "openai-codex"
model = "gpt-5.6-sol"
thinking_levels = ["low", "high", "xhigh"]
default_thinking_level = "high"

[session]
model_type = "local-qwen"
thinking_level = "off"

[workers.reviewer]
description = "Review code and designs without modifying files."
model_type = "gpt-5-6-sol"
thinking_level = "xhigh"

[workers.worker]
description = "Implement changes and hand off pushed Git refs."
model_type = "gpt-5-6-sol"
thinking_level = "high"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.WorkerNames(), []string{"reviewer", "worker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("WorkerNames() = %#v, want %#v", got, want)
	}
	if got, want := cfg.Workers["reviewer"], (WorkerDefaults{
		Description:   "Review code and designs without modifying files.",
		ModelType:     "gpt-5-6-sol",
		ThinkingLevel: "xhigh",
	}); got != want {
		t.Fatalf("Workers[reviewer] = %#v, want %#v", got, want)
	}
}

func TestResolveWorker(t *testing.T) {
	cfg := modelConfigFixture()
	want := WorkerProfile{
		Description:   "Implement changes and hand off pushed Git refs.",
		Provider:      "openai-codex",
		Model:         "gpt-5.6-sol",
		ThinkingLevel: "high",
	}

	got, err := cfg.ResolveWorker("worker")
	if err != nil {
		t.Fatalf("ResolveWorker(worker) error = %v", err)
	}
	if got != want {
		t.Fatalf("ResolveWorker(worker) = %#v, want %#v", got, want)
	}
	if _, err := cfg.ResolveWorker("missing"); err == nil || !strings.Contains(err.Error(), `unknown worker type "missing"`) {
		t.Fatalf("ResolveWorker(missing) error = %v, want unknown-worker error", err)
	}
}

func TestValidateSupervisorWorkers(t *testing.T) {
	tests := []struct {
		name    string
		workers map[string]WorkerDefaults
		wantErr string
	}{
		{name: "valid", workers: map[string]WorkerDefaults{"reviewer": {Description: "d", ModelType: "gpt-5-6-sol", ThinkingLevel: "xhigh"}}},
		{name: "no workers", workers: map[string]WorkerDefaults{}, wantErr: "at least one worker is required"},
		{name: "empty worker name", workers: map[string]WorkerDefaults{"": {Description: "d", ModelType: "gpt-5-6-sol"}}, wantErr: "worker name is required"},
		{name: "whitespace worker name", workers: map[string]WorkerDefaults{"  ": {Description: "d", ModelType: "gpt-5-6-sol"}}, wantErr: "worker name is required"},
		{name: "empty description", workers: map[string]WorkerDefaults{"reviewer": {ModelType: "gpt-5-6-sol"}}, wantErr: "description is required"},
		{name: "unknown model type", workers: map[string]WorkerDefaults{"reviewer": {Description: "d", ModelType: "bogus"}}, wantErr: "unknown model type"},
		{name: "unsupported thinking level", workers: map[string]WorkerDefaults{"reviewer": {Description: "d", ModelType: "local-qwen", ThinkingLevel: "high"}}, wantErr: "does not support thinking level"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := modelConfigFixture()
			cfg.Workers = tt.workers
			err := cfg.ValidateSupervisor()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateSupervisor() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateSupervisor() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateSupervisorModelCatalog(t *testing.T) {
	base := modelConfigFixture()
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid fixture", mutate: func(*Config) {}},
		{
			name:    "invalid model slug",
			mutate:  func(c *Config) { c.Models["Bad_Model"] = base.Models["gpt-5-6-sol"] },
			wantErr: "invalid name",
		},
		{
			name:    "empty model slug",
			mutate:  func(c *Config) { c.Models[""] = base.Models["gpt-5-6-sol"] },
			wantErr: "invalid name",
		},
		{
			name: "duplicate provider/model pair",
			mutate: func(c *Config) {
				c.Models["gpt-clone"] = ModelDefinition{
					Label: "Clone", Provider: "openai-codex", Model: "gpt-5.6-sol",
					ThinkingLevels: []string{"high"}, DefaultThinkingLevel: "high",
				}
			},
			wantErr: "duplicate provider/model",
		},
		{
			name: "empty label",
			mutate: func(c *Config) {
				m := c.Models["gpt-5-6-sol"]
				m.Label = ""
				c.Models["gpt-5-6-sol"] = m
			},
			wantErr: "label is required",
		},
		{
			name: "whitespace label",
			mutate: func(c *Config) {
				m := c.Models["gpt-5-6-sol"]
				m.Label = "  \t"
				c.Models["gpt-5-6-sol"] = m
			},
			wantErr: "label is required",
		},
		{
			name: "empty provider",
			mutate: func(c *Config) {
				c.Models["gpt-5-6-sol"] = ModelDefinition{Label: "GPT", Model: "gpt-5.6-sol", ThinkingLevels: []string{"high"}, DefaultThinkingLevel: "high"}
			},
			wantErr: "provider is required",
		},
		{
			name: "empty model",
			mutate: func(c *Config) {
				c.Models["gpt-5-6-sol"] = ModelDefinition{Label: "GPT", Provider: "openai-codex", ThinkingLevels: []string{"high"}, DefaultThinkingLevel: "high"}
			},
			wantErr: "model is required",
		},
		{
			name: "invalid thinking level in model",
			mutate: func(c *Config) {
				m := c.Models["gpt-5-6-sol"]
				m.ThinkingLevels = []string{"high", "extreme"}
				c.Models["gpt-5-6-sol"] = m
			},
			wantErr: "invalid thinking level",
		},
		{
			name: "duplicate thinking level",
			mutate: func(c *Config) {
				m := c.Models["gpt-5-6-sol"]
				m.ThinkingLevels = []string{"high", "high"}
				c.Models["gpt-5-6-sol"] = m
			},
			wantErr: "duplicate thinking level",
		},
		{
			name: "empty thinking levels",
			mutate: func(c *Config) {
				m := c.Models["gpt-5-6-sol"]
				m.ThinkingLevels = nil
				c.Models["gpt-5-6-sol"] = m
			},
			wantErr: "at least one thinking level",
		},
		{
			name: "default thinking not in set",
			mutate: func(c *Config) {
				m := c.Models["gpt-5-6-sol"]
				m.ThinkingLevels = []string{"high", "xhigh"}
				m.DefaultThinkingLevel = "medium"
				c.Models["gpt-5-6-sol"] = m
			},
			wantErr: "default thinking level",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := modelConfigFixture()
			tt.mutate(&cfg)
			err := cfg.ValidateSupervisor()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateSupervisor() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateSupervisor() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateSupervisorResolvesSessionAndWorkerDefaults(t *testing.T) {
	cfg := modelConfigFixture()
	if err := cfg.ValidateSupervisor(); err != nil {
		t.Fatalf("ValidateSupervisor() error = %v", err)
	}

	session := modelConfigFixture()
	session.Session = SessionDefaults{ModelType: "bogus", ThinkingLevel: "high"}
	if err := session.ValidateSupervisor(); err == nil || !strings.Contains(err.Error(), "unknown model type") {
		t.Fatalf("ValidateSupervisor() error = %v, want unknown model type", err)
	}

	unsupported := modelConfigFixture()
	unsupported.Session = SessionDefaults{ModelType: "local-qwen", ThinkingLevel: "high"}
	if err := unsupported.ValidateSupervisor(); err == nil || !strings.Contains(err.Error(), "does not support thinking level") {
		t.Fatalf("ValidateSupervisor() error = %v, want unsupported thinking level", err)
	}
}

func TestValidateSupervisorValidThinkingLevels(t *testing.T) {
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
		t.Run(level, func(t *testing.T) {
			cfg := modelConfigFixture()
			cfg.Models = map[string]ModelDefinition{"m": {
				Label: "Model", Provider: "provider", Model: "model",
				ThinkingLevels: []string{level}, DefaultThinkingLevel: level,
			}}
			cfg.Session = SessionDefaults{ModelType: "m", ThinkingLevel: level}
			cfg.Workers = map[string]WorkerDefaults{"worker": {
				Description: "Does work.", ModelType: "m", ThinkingLevel: level,
			}}
			if err := cfg.ValidateSupervisor(); err != nil {
				t.Fatalf("ValidateSupervisor() error = %v", err)
			}
		})
	}
}

func TestValidateSupervisorIncludesLifecycleValidation(t *testing.T) {
	cfg := modelConfigFixture()
	cfg.BaseImage = BaseImage{}
	if err := cfg.ValidateSupervisor(); err == nil || err.Error() != "base_image.name is required" {
		t.Fatalf("ValidateSupervisor() error = %v, want lifecycle validation error", err)
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

func modelConfigFixture() Config {
	return Config{
		BaseImage: BaseImage{Name: "sandbox", Source: "https://images.linuxcontainers.org", Image: "debian/13"},
		Models: map[string]ModelDefinition{
			"local-qwen": {
				Label: "Local Qwen", Provider: "local-executor", Model: "Qwen3.6-27B-GGUF",
				ThinkingLevels: []string{"off"}, DefaultThinkingLevel: "off",
			},
			"gpt-5-6-sol": {
				Label: "GPT-5.6 Solver", Provider: "openai-codex", Model: "gpt-5.6-sol",
				ThinkingLevels:       []string{"minimal", "low", "medium", "high", "xhigh", "max"},
				DefaultThinkingLevel: "high",
			},
		},
		Session: SessionDefaults{ModelType: "local-qwen", ThinkingLevel: "off"},
		Workers: map[string]WorkerDefaults{
			"reviewer": {Description: "Review code and designs without modifying files.", ModelType: "gpt-5-6-sol", ThinkingLevel: "xhigh"},
			"worker":   {Description: "Implement changes and hand off pushed Git refs.", ModelType: "gpt-5-6-sol", ThinkingLevel: "high"},
		},
	}
}

func TestDefaultSessionModelPolicyResolvesCatalogReferences(t *testing.T) {
	cfg := modelConfigFixture()
	policy, err := cfg.DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Root != (ModelProfile{Provider: "local-executor", Model: "Qwen3.6-27B-GGUF", ThinkingLevel: "off"}) {
		t.Fatalf("root = %#v", policy.Root)
	}
	reviewer, err := policy.ResolveWorker("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if reviewer.Model != "gpt-5.6-sol" || reviewer.ThinkingLevel != "xhigh" {
		t.Fatalf("reviewer = %#v", reviewer)
	}
}

func TestDefaultSessionModelPolicyFallsBackToModelDefaultThinking(t *testing.T) {
	cfg := modelConfigFixture()
	cfg.Session = SessionDefaults{ModelType: "gpt-5-6-sol"}
	policy, err := cfg.DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Root != (ModelProfile{Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevel: "high"}) {
		t.Fatalf("root = %#v", policy.Root)
	}

	workerCfg := modelConfigFixture()
	workerCfg.Workers = map[string]WorkerDefaults{"worker": {Description: "Does work.", ModelType: "gpt-5-6-sol"}}
	policy, err = workerCfg.DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.Workers["worker"]; got.ThinkingLevel != "high" {
		t.Fatalf("worker thinking = %q, want high default", got.ThinkingLevel)
	}
}

func TestSessionModelPolicyCloneDoesNotAliasWorkers(t *testing.T) {
	original, err := modelConfigFixture().DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	cloned := original.Clone()
	worker := cloned.Workers["reviewer"]
	worker.Model = "mutated"
	cloned.Workers["reviewer"] = worker
	if original.Workers["reviewer"].Model == "mutated" {
		t.Fatal("Clone retained the workers map")
	}
}

func TestSessionModelPolicyValidate(t *testing.T) {
	base := func() SessionModelPolicy {
		policy, err := modelConfigFixture().DefaultSessionModelPolicy()
		if err != nil {
			t.Fatal(err)
		}
		return policy
	}
	tests := []struct {
		name    string
		mutate  func(*SessionModelPolicy)
		wantErr string
	}{
		{name: "valid", mutate: func(*SessionModelPolicy) {}},
		{name: "root missing provider", mutate: func(p *SessionModelPolicy) { p.Root.Provider = "" }, wantErr: "provider"},
		{name: "root invalid thinking", mutate: func(p *SessionModelPolicy) { p.Root.ThinkingLevel = "extreme" }, wantErr: "thinking level"},
		{name: "no workers", mutate: func(p *SessionModelPolicy) { p.Workers = map[string]WorkerProfile{} }, wantErr: "at least one worker"},
		{name: "worker empty description", mutate: func(p *SessionModelPolicy) {
			w := p.Workers["reviewer"]
			w.Description = ""
			p.Workers["reviewer"] = w
		}, wantErr: "description"},
		{name: "worker invalid thinking", mutate: func(p *SessionModelPolicy) {
			w := p.Workers["reviewer"]
			w.ThinkingLevel = "bogus"
			p.Workers["reviewer"] = w
		}, wantErr: "thinking level"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := base()
			tt.mutate(&policy)
			err := policy.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestWorkerNamesDeterministic(t *testing.T) {
	cfg := modelConfigFixture()
	if got, want := cfg.WorkerNames(), []string{"reviewer", "worker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Config.WorkerNames() = %#v, want %#v", got, want)
	}
	policy, err := cfg.DefaultSessionModelPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := policy.WorkerNames(), []string{"reviewer", "worker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SessionModelPolicy.WorkerNames() = %#v, want %#v", got, want)
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
