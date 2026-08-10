package manager

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

// mustLaunchConfiguration builds the launch catalog from a fixture, failing the
// test if construction fails.
func mustLaunchConfiguration(t *testing.T, cfg config.Config) LaunchConfiguration {
	t.Helper()
	lc, err := NewLaunchConfiguration(cfg)
	if err != nil {
		t.Fatalf("NewLaunchConfiguration: %v", err)
	}
	return lc
}

// assertInvalidRequest verifies err is a contract.Error with the invalid
// request code.
func assertInvalidRequest(t *testing.T, err error) {
	t.Helper()
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorInvalidRequest {
		t.Fatalf("error = %v, want *contract.Error with code %q", err, contract.ErrorInvalidRequest)
	}
}

func TestRepositoryLaunchOptionsNameGPT56SolCorrectly(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	options := mustLaunchConfiguration(t, cfg).LaunchOptions()
	for _, model := range options.Models {
		if model.ModelType == "gpt-5-6-sol" {
			if model.Label != "GPT-5.6 Sol" {
				t.Fatalf("GPT-5.6 Sol launch label = %q", model.Label)
			}
			return
		}
	}
	t.Fatal("gpt-5-6-sol is missing from repository launch options")
}

func TestLaunchConfigurationValidCustomRequest(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	request := SessionLaunchRequest{
		Root: ModelSelection{ModelType: "gpt-5-6-sol", ThinkingLevel: "high"},
		Workers: []WorkerModelSelection{
			{WorkerType: "reviewer", ModelType: "gpt-5-6-sol", ThinkingLevel: "xhigh"},
			{WorkerType: "worker", ModelType: "gpt-5-6-sol", ThinkingLevel: "low"},
		},
	}
	resolved, err := launch.Resolve(request)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	policy := resolved.Policy
	if policy.Root != (config.ModelProfile{Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevel: "high"}) {
		t.Fatalf("root = %#v", policy.Root)
	}
	reviewer, err := policy.ResolveWorker("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if reviewer != (config.WorkerProfile{
		Description: "Review code and designs without modifying files.",
		Provider:    "openai-codex", Model: "gpt-5.6-sol", ThinkingLevel: "xhigh",
	}) {
		t.Fatalf("reviewer = %#v", reviewer)
	}
	worker, err := policy.ResolveWorker("worker")
	if err != nil {
		t.Fatal(err)
	}
	if worker.ThinkingLevel != "low" {
		t.Fatalf("worker thinking = %q, want low (custom, not default)", worker.ThinkingLevel)
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("resolved policy Validate: %v", err)
	}
}

func TestLaunchConfigurationRequiresEveryWorkerExactlyOnce(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	request := launch.DefaultRequest()
	request.Workers = append(request.Workers, request.Workers[0])
	_, err := launch.Resolve(request)
	assertInvalidRequest(t, err)
}

func TestLaunchConfigurationRejectsMissingWorker(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	request := launch.DefaultRequest()
	request.Workers = request.Workers[:len(request.Workers)-1]
	_, err := launch.Resolve(request)
	assertInvalidRequest(t, err)
}

func TestLaunchConfigurationRejectsEqualLengthDuplicateWorkerRoles(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	request := launch.DefaultRequest()
	request.Workers[1].WorkerType = request.Workers[0].WorkerType
	_, err := launch.Resolve(request)
	assertInvalidRequest(t, err)
}

func TestLaunchConfigurationRejectsUnknownWorker(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	request := launch.DefaultRequest()
	request.Workers[0].WorkerType = "ghost"
	_, err := launch.Resolve(request)
	assertInvalidRequest(t, err)
}

func TestLaunchConfigurationRejectsEmptyRootModelType(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	request := launch.DefaultRequest()
	request.Root.ModelType = ""
	_, err := launch.Resolve(request)
	assertInvalidRequest(t, err)
}

func TestLaunchConfigurationRejectsUnknownModel(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	request := launch.DefaultRequest()
	request.Root.ModelType = "bogus"
	_, err := launch.Resolve(request)
	assertInvalidRequest(t, err)

	request = launch.DefaultRequest()
	request.Workers[0].ModelType = "bogus"
	_, err = launch.Resolve(request)
	assertInvalidRequest(t, err)
}

func TestLaunchConfigurationRejectsUnsupportedThinking(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	request := launch.DefaultRequest()
	request.Root.ModelType = "local-qwen"
	request.Root.ThinkingLevel = "high" // local-qwen only supports "off"
	_, err := launch.Resolve(request)
	assertInvalidRequest(t, err)
}

func TestLaunchConfigurationDefaultThinkingFallsBack(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	request := launch.DefaultRequest()
	request.Root.ModelType = "gpt-5-6-sol"
	request.Root.ThinkingLevel = ""
	policy, err := launch.Resolve(request)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if policy.Policy.Root.ThinkingLevel != "high" {
		t.Fatalf("root thinking = %q, want model default high", policy.Policy.Root.ThinkingLevel)
	}
}

func TestLaunchConfigurationResolveReturnsIndependentPolicy(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	first, err := launch.Resolve(launch.DefaultRequest())
	if err != nil {
		t.Fatal(err)
	}
	first.Policy.Workers["reviewer"] = config.WorkerProfile{Description: "changed", Provider: "x", Model: "y", ThinkingLevel: "off"}
	second, err := launch.Resolve(launch.DefaultRequest())
	if err != nil {
		t.Fatal(err)
	}
	if second.Policy.Workers["reviewer"].Provider == "x" {
		t.Fatal("launch policy aliased prior result")
	}
}

func TestLaunchOptionsDeterministicOrderingAndDefaults(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	opts := launch.LaunchOptions()

	wantModels := []string{"gpt-5-6-sol", "local-qwen"}
	gotModels := make([]string, 0, len(opts.Models))
	for _, m := range opts.Models {
		gotModels = append(gotModels, m.ModelType)
	}
	if !reflect.DeepEqual(gotModels, wantModels) {
		t.Fatalf("model order = %#v, want %#v", gotModels, wantModels)
	}

	wantWorkers := []string{"reviewer", "worker"}
	gotWorkers := make([]string, 0, len(opts.Workers))
	for _, w := range opts.Workers {
		gotWorkers = append(gotWorkers, w.WorkerType)
	}
	if !reflect.DeepEqual(gotWorkers, wantWorkers) {
		t.Fatalf("worker order = %#v, want %#v", gotWorkers, wantWorkers)
	}

	if opts.Root != (ModelSelection{ModelType: "local-qwen", ThinkingLevel: "off"}) {
		t.Fatalf("default root = %#v", opts.Root)
	}

	// Worker rows expose descriptions and configured defaults only.
	byType := make(map[string]WorkerLaunchOption, len(opts.Workers))
	for _, w := range opts.Workers {
		byType[w.WorkerType] = w
	}
	if w := byType["reviewer"]; w.Description == "" || w.ModelType != "gpt-5-6-sol" || w.ThinkingLevel != "xhigh" {
		t.Fatalf("reviewer option = %#v", w)
	}
	if w := byType["worker"]; w.ModelType != "gpt-5-6-sol" || w.ThinkingLevel != "high" {
		t.Fatalf("worker option = %#v", w)
	}

	// Model options carry the model type ID, label, and thinking choices only.
	byModel := make(map[string]ModelLaunchOption, len(opts.Models))
	for _, m := range opts.Models {
		byModel[m.ModelType] = m
	}
	gpt := byModel["gpt-5-6-sol"]
	if gpt.Label == "" || gpt.DefaultThinkingLevel != "high" {
		t.Fatalf("gpt option = %#v", gpt)
	}
	if len(gpt.ThinkingLevels) == 0 || gpt.ThinkingLevels[0] != "minimal" {
		t.Fatalf("gpt thinking levels = %#v", gpt.ThinkingLevels)
	}
}

func TestLaunchConfigurationDoesNotAliasInputThinkingLevels(t *testing.T) {
	cfg := modelConfigFixture()
	launch := mustLaunchConfiguration(t, cfg)
	definition := cfg.Models["local-qwen"]
	definition.ThinkingLevels[0] = "high"
	cfg.Models["local-qwen"] = definition

	options := launch.LaunchOptions()
	var local ModelLaunchOption
	for _, option := range options.Models {
		if option.ModelType == "local-qwen" {
			local = option
		}
	}
	if !reflect.DeepEqual(local.ThinkingLevels, []string{"off"}) {
		t.Fatalf("launch thinking levels changed through input alias: %v", local.ThinkingLevels)
	}
	request := launch.DefaultRequest()
	request.Root.ThinkingLevel = "high"
	if _, err := launch.Resolve(request); err == nil {
		t.Fatal("mutating source config changed launch resolution")
	}
}

func TestLaunchOptionsReturnsCopiedSlices(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	first := launch.LaunchOptions()
	first.Models[0].ThinkingLevels[0] = "mutated"
	first.Workers[0].WorkerType = "mutated"

	second := launch.LaunchOptions()
	if second.Models[0].ThinkingLevels[0] == "mutated" {
		t.Fatal("LaunchOptions aliased model thinking slices")
	}
	if second.Workers[0].WorkerType == "mutated" {
		t.Fatal("LaunchOptions aliased worker slice")
	}
}

func TestDefaultRequestReturnsCopiedSlices(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	first := launch.DefaultRequest()
	first.Workers[0].WorkerType = "mutated"
	first.Root.ModelType = "mutated"

	second := launch.DefaultRequest()
	if second.Workers[0].WorkerType == "mutated" || second.Root.ModelType == "mutated" {
		t.Fatal("DefaultRequest aliased prior request")
	}
}

func TestNewManagerRequiresLaunchConfiguration(t *testing.T) {
	dir, logDir := shortTempDirs(t)
	_, err := New(Options{
		RootSocketDir: dir,
		SessionLogDir: logDir,
		EventLimits:   supervisor.EventBrokerOptions{MaxEvents: 100},
		Logger:        discardLogger(),
		SpawnTimeout:  5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "launch configuration") {
		t.Fatalf("New with zero launch error = %v, want launch configuration error", err)
	}

	m, err := New(Options{
		RootSocketDir: dir,
		SessionLogDir: logDir,
		EventLimits:   supervisor.EventBrokerOptions{MaxEvents: 100},
		Logger:        discardLogger(),
		Launch:        managerTestLaunch(),
		SpawnTimeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New with valid launch: %v", err)
	}
	if len(m.LaunchOptions().Models) == 0 {
		t.Fatal("LaunchOptions() on manager returned no model options")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestManagerLaunchOptionsExposesView(t *testing.T) {
	m := fakeManager(nil)
	opts := m.LaunchOptions()
	if len(opts.Models) != 2 || len(opts.Workers) != 2 {
		t.Fatalf("manager LaunchOptions models/workers = %d/%d, want 2/2", len(opts.Models), len(opts.Workers))
	}
	if len(opts.Repositories) != 3 {
		t.Fatalf("manager LaunchOptions repositories = %d, want 3", len(opts.Repositories))
	}
}

func TestLaunchConfigurationResolvesNameRepositoryAndPolicy(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	request := launch.DefaultRequest()
	request.Name = "  release triage  "
	request.Repository = "owner/repo"
	got, err := launch.Resolve(request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "release triage" {
		t.Fatalf("name = %q", got.Name)
	}
	if got.Workspace != (config.WorkspaceStart{Repository: "owner/repo", Checkout: "repo"}) {
		t.Fatalf("workspace = %#v", got.Workspace)
	}
	if got.Policy.Root.Provider == "" {
		t.Fatalf("policy not resolved: %#v", got.Policy)
	}
}

func TestLaunchConfigurationResolveEmptyNameAndRepository(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	request := launch.DefaultRequest()
	request.Name = "   "
	request.Repository = ""
	got, err := launch.Resolve(request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "" {
		t.Fatalf("empty name = %q, want empty", got.Name)
	}
	if got.Workspace != (config.WorkspaceStart{}) {
		t.Fatalf("empty workspace = %#v, want zero value", got.Workspace)
	}
}

func TestLaunchConfigurationRejectsUnknownRepository(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	request := launch.DefaultRequest()
	request.Repository = "owner/not-configured"
	_, err := launch.Resolve(request)
	assertInvalidRequest(t, err)
}

func TestNewLaunchConfigurationRejectsInvalidConfiguredRepository(t *testing.T) {
	cfg := modelConfigFixture()
	cfg.Workspace.Repos = []string{"https://github.com/owner/repo"}
	if _, err := NewLaunchConfiguration(cfg); err == nil {
		t.Fatal("invalid configured repository accepted")
	}
}

func TestLaunchConfigurationAcceptsDuplicateNames(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	a := launch.DefaultRequest()
	a.Name = "triage"
	b := launch.DefaultRequest()
	b.Name = "triage"
	ra, err := launch.Resolve(a)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := launch.Resolve(b)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Name != "triage" || rb.Name != "triage" {
		t.Fatalf("duplicate names not accepted: %q vs %q", ra.Name, rb.Name)
	}
}

func TestLaunchConfigurationRejectsNameOver80CodePoints(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	request := launch.DefaultRequest()
	request.Name = strings.Repeat("界", 81)
	if _, err := launch.Resolve(request); err == nil {
		t.Fatal("81 code-point name accepted")
	} else {
		assertInvalidRequest(t, err)
	}
	request.Name = strings.Repeat("界", 80)
	if _, err := launch.Resolve(request); err != nil {
		t.Fatalf("80 code-point name rejected: %v", err)
	}
}

func TestLaunchConfigurationRejectsNameWithControlCharacter(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	for _, name := range []string{"triage\n", "tri\tage", "\x00triage"} {
		request := launch.DefaultRequest()
		request.Name = name
		if _, err := launch.Resolve(request); err == nil {
			t.Errorf("name %q with control character accepted", name)
		} else {
			assertInvalidRequest(t, err)
		}
	}
}

func TestLaunchOptionsExposeRepositorySlugsOnlySortedAndCopied(t *testing.T) {
	launch := mustLaunchConfiguration(t, modelConfigFixture())
	opts := launch.LaunchOptions()

	want := []string{"one/alpha", "owner/repo", "two/beta"}
	got := make([]string, 0, len(opts.Repositories))
	for _, repo := range opts.Repositories {
		got = append(got, repo.Slug)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("repository order = %#v, want %#v", got, want)
	}

	// Slugs only: no URL or absolute path leak.
	for _, repo := range opts.Repositories {
		if strings.HasPrefix(repo.Slug, "/") || strings.HasPrefix(repo.Slug, "http") || strings.HasPrefix(repo.Slug, "_") || strings.HasPrefix(repo.Slug, ".") {
			t.Fatalf("repository option leaked URL/path: %q", repo.Slug)
		}
	}

	// Copied slice: mutating one view must not alias the catalog.
	first := launch.LaunchOptions()
	first.Repositories[0].Slug = "mutated"
	second := launch.LaunchOptions()
	if second.Repositories[0].Slug == "mutated" {
		t.Fatal("LaunchOptions aliased repository slice")
	}
}
