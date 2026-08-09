package image

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
)

func TestInstallerIncludesUninitializedContainerOnlyIncus(t *testing.T) {
	script := string(installer)
	const startMarker = "apt-get install -y --no-install-recommends \\\n"
	start := strings.Index(script, startMarker)
	if start < 0 {
		t.Fatal("installer initial package batch not found")
	}
	packageBlock := script[start+len(startMarker):]
	end := strings.Index(packageBlock, "\n\nrun_as_managed_user()")
	if end < 0 {
		t.Fatal("installer initial package batch terminator not found")
	}
	packages := strings.Fields(strings.ReplaceAll(packageBlock[:end], "\\\n", " "))

	if !slices.Contains(packages, "incus-base") {
		t.Error("initial package batch does not include incus-base")
	}
	if slices.Contains(packages, "incus") {
		t.Error("initial package batch includes VM-oriented incus metapackage")
	}
	if !strings.Contains(script, `usermod --append --groups sudo,incus-admin "$managed_user"`) {
		t.Error("managed user is not added to incus-admin")
	}
	if !strings.Contains(script, `[[ $(id -u "$managed_user") != 1000 || $(id -g "$managed_user") != 1000 ]]`) {
		t.Error("managed user numeric UID/GID 1000 is not asserted before image publication")
	}
	if strings.Contains(script, "incus admin init") {
		t.Error("installer initializes Incus")
	}

	allowedIncusLines := map[string]bool{
		`incus-base \`: true,
		`usermod --append --groups sudo,incus-admin "$managed_user"`: true,
	}
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "incus") && !allowedIncusLines[line] {
			t.Errorf("installer contains unexpected Incus operation %q", line)
		}
	}
}

func TestInstallerActivatesOnlyKanediasDelegationExtensionAndSkills(t *testing.T) {
	script := string(installer)
	for _, required := range []string{
		`install -d -m 0755 /opt/kanedias/pi-extension`,
		`npm ci --omit=dev --ignore-scripts`,
		`/usr/lib/tmpfiles.d/kanedias.conf`,
		`d /run/kanedias 0700 kanedias kanedias -`,
		`$managed_home/.pi/agent/skills/delegate-session`,
		`$managed_home/.pi/agent/skills/writer-handoff`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("installer missing extension activation behavior %q", required)
		}
	}
	settings, err := os.ReadFile(filepath.Join("..", "..", "assets", "pi-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "pi-subagents") || strings.Contains(string(settings), "pi-subagents") {
		t.Error("pi-subagents remains installed or configured")
	}
}

func TestPiEnvironmentBridgeWritesAllowlistedEnvironment(t *testing.T) {
	dir := t.TempDir()
	bridgePath := filepath.Join(dir, "kanedias-pi-env")
	if err := os.WriteFile(bridgePath, piEnvironmentBridge, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(dir, "pid1-environ")
	destination := filepath.Join(dir, "pi.env")
	entries := []string{
		"FORBIDDEN_SENTINEL=must-not-escape",
		"KANEDIAS_E2E_RUN_ID=e2e-run",
		"KANEDIAS_SUPERVISOR_SOCKET=/run/kanedias/supervisor.sock",
		"KANEDIAS_PI_SESSION_FILE=",
		"KANEDIAS_PI_THINKING=xhigh",
		"KANEDIAS_PI_MODEL=model",
		"KANEDIAS_PI_PROVIDER=provider",
		`KANEDIAS_WORKER_TYPE=writer "quoted"\path`,
		"KANEDIAS_SESSION_KIND=write",
		"KANEDIAS_SESSION_ID=session-123",
		"SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
		"NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/kanedias-proxy.crt",
		"no_proxy=localhost,127.0.0.1,::1",
		"NO_PROXY=localhost,127.0.0.1,::1",
		"GH_TOKEN=container-dummy",
		"https_proxy=http://proxy.example:3128",
		"http_proxy=http://proxy.example:3128",
		"HTTPS_PROXY=http://proxy.example:3128",
		"HTTP_PROXY=http://proxy.example:3128",
	}
	if err := os.WriteFile(sourcePath, append([]byte(strings.Join(entries, "\x00")), 0), 0o600); err != nil {
		t.Fatal(err)
	}

	if output, err := exec.Command(bridgePath, sourcePath, destination).CombinedOutput(); err != nil {
		t.Fatalf("bridge failed: %v\n%s", err, output)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		`HTTP_PROXY="http://proxy.example:3128"`,
		`HTTPS_PROXY="http://proxy.example:3128"`,
		`http_proxy="http://proxy.example:3128"`,
		`https_proxy="http://proxy.example:3128"`,
		`GH_TOKEN="container-dummy"`,
		`NO_PROXY="localhost,127.0.0.1,::1"`,
		`no_proxy="localhost,127.0.0.1,::1"`,
		`NODE_EXTRA_CA_CERTS="/usr/local/share/ca-certificates/kanedias-proxy.crt"`,
		`SSL_CERT_FILE="/etc/ssl/certs/ca-certificates.crt"`,
		`KANEDIAS_SESSION_ID="session-123"`,
		`KANEDIAS_SESSION_KIND="write"`,
		`KANEDIAS_WORKER_TYPE="writer \"quoted\"\\path"`,
		`KANEDIAS_PI_PROVIDER="provider"`,
		`KANEDIAS_PI_MODEL="model"`,
		`KANEDIAS_PI_THINKING="xhigh"`,
		`KANEDIAS_PI_SESSION_FILE=""`,
		`KANEDIAS_SUPERVISOR_SOCKET="/run/kanedias/supervisor.sock"`,
		`KANEDIAS_E2E_RUN_ID="e2e-run"`,
	}, "\n") + "\n"
	if string(got) != want {
		t.Fatalf("environment file = %q, want %q", got, want)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Errorf("environment file mode = %#o, want 0600", gotMode)
	}
}

func TestPiEnvironmentBridgePrivilegedInvocationIgnoresStaleEnvironment(t *testing.T) {
	const privilegedInvocation = "ExecStartPre=+/usr/bin/env -i /usr/bin/bash --noprofile --norc /usr/local/libexec/kanedias-pi-env"
	if service := string(piRPCService); !strings.Contains(service, privilegedInvocation) {
		t.Fatalf("service does not sanitize the privileged bridge environment with fixed executables:\n%s", service)
	}

	dir := t.TempDir()
	bridgePath := filepath.Join(dir, "kanedias-pi-env")
	if err := os.WriteFile(bridgePath, piEnvironmentBridge, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "attacker-code-ran")
	bashEnv := filepath.Join(dir, "bash-env")
	if err := os.WriteFile(bashEnv, []byte("printf pwned > "+marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	attackerBin := filepath.Join(dir, "attacker-bin")
	if err := os.Mkdir(attackerBin, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeBash := filepath.Join(attackerBin, "bash")
	if err := os.WriteFile(fakeBash, []byte("#!/bin/sh\nprintf pwned > "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(dir, "pid1-environ")
	destination := filepath.Join(dir, "pi.env")
	entries := []string{
		"KANEDIAS_SESSION_ID=session-123",
		"KANEDIAS_SESSION_KIND=root",
		"KANEDIAS_SUPERVISOR_SOCKET=/run/kanedias/supervisor.sock",
		"HTTP_PROXY=http://proxy.example:3128",
		"HTTPS_PROXY=http://proxy.example:3128",
		"GH_TOKEN=container-dummy",
		"SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
		"NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/kanedias-proxy.crt",
	}
	if err := os.WriteFile(sourcePath, append([]byte(strings.Join(entries, "\x00")), 0), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("/usr/bin/env", "-i", "/usr/bin/bash", "--noprofile", "--norc", bridgePath, sourcePath, destination)
	command.Env = []string{"PATH=" + attackerBin, "BASH_ENV=" + bashEnv}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sanitized privileged invocation failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("attacker code marker stat error = %v, want not exist", err)
	}
}

func TestPiEnvironmentBridgeProductionRuntimeIsRootControlled(t *testing.T) {
	service := string(piRPCService)
	if !strings.Contains(service, "EnvironmentFile=-/run/kanedias-pi/pi.env") {
		t.Fatalf("service environment file is not in the protected runtime directory:\n%s", service)
	}
	if !strings.Contains(string(installer), "d /run/kanedias-pi 0700 root root -") {
		t.Fatal("installer does not create the Pi environment runtime directory as root-only")
	}

	bridge := string(piEnvironmentBridge)
	for _, want := range []string{
		`destination=${2:-/run/kanedias-pi/pi.env}`,
		`temporary=$(/usr/bin/mktemp -- "$destination_dir/.pi.env.XXXXXX")`,
		`exec {output_fd}> "$temporary"`,
		`/usr/bin/mv -fT -- "$temporary" "$destination"`,
	} {
		if !strings.Contains(bridge, want) {
			t.Errorf("environment bridge missing protected staging behavior %q", want)
		}
	}
	for _, forbidden := range []string{
		`destination=${2:-/run/kanedias/pi.env}`,
		`chmod 0600 "$temporary"`,
		`>> "$temporary"`,
	} {
		if strings.Contains(bridge, forbidden) {
			t.Errorf("environment bridge retains unsafe staging behavior %q", forbidden)
		}
	}
}

func TestPiEnvironmentBridgeRejectsInvalidInputWithoutReplacingDestination(t *testing.T) {
	required := []string{
		"KANEDIAS_SESSION_ID=session-123",
		"KANEDIAS_SESSION_KIND=root",
		"KANEDIAS_SUPERVISOR_SOCKET=/run/kanedias/supervisor.sock",
		"HTTP_PROXY=http://proxy.example:3128",
		"HTTPS_PROXY=http://proxy.example:3128",
		"GH_TOKEN=container-dummy",
		"SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
		"NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/kanedias-proxy.crt",
	}
	tests := []struct {
		name    string
		entries []string
	}{
		{name: "missing required value", entries: required[1:]},
		{name: "newline in value", entries: append(append([]string(nil), required...), "KANEDIAS_WORKER_TYPE=bad\nvalue")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			bridgePath := filepath.Join(dir, "kanedias-pi-env")
			if err := os.WriteFile(bridgePath, piEnvironmentBridge, 0o700); err != nil {
				t.Fatal(err)
			}
			sourcePath := filepath.Join(dir, "pid1-environ")
			destination := filepath.Join(dir, "pi.env")
			if err := os.WriteFile(sourcePath, append([]byte(strings.Join(tt.entries, "\x00")), 0), 0o600); err != nil {
				t.Fatal(err)
			}
			const original = "existing-destination\n"
			if err := os.WriteFile(destination, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}

			if output, err := exec.Command(bridgePath, sourcePath, destination).CombinedOutput(); err == nil {
				t.Fatalf("bridge succeeded, want failure; output: %s", output)
			}
			got, err := os.ReadFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != original {
				t.Fatalf("destination = %q, want unchanged %q", got, original)
			}
		})
	}
}

func TestPiRPCLauncherBuildsFreshAndForkArgumentsWithoutEval(t *testing.T) {
	dir := t.TempDir()
	launcher := strings.Replace(string(piRPCLauncher), `source "$NVM_DIR/nvm.sh"`, ":", 1)
	launcherPath := filepath.Join(dir, "launcher")
	if err := os.WriteFile(launcherPath, []byte(launcher), 0o700); err != nil {
		t.Fatal(err)
	}
	piPath := filepath.Join(dir, "pi")
	if err := os.WriteFile(piPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{name: "root fresh", env: []string{"KANEDIAS_SESSION_ID=root-1", "KANEDIAS_SESSION_KIND=root"}, want: []string{"--mode", "rpc", "-e", "/opt/kanedias/pi-extension/src/index.ts"}},
		{name: "child fresh", env: []string{"KANEDIAS_SESSION_ID=child-1", "KANEDIAS_SESSION_KIND=read", "KANEDIAS_PI_PROVIDER=provider", "KANEDIAS_PI_MODEL=model", "KANEDIAS_PI_THINKING=high"}, want: []string{"--mode", "rpc", "-e", "/opt/kanedias/pi-extension/src/index.ts", "--provider", "provider", "--model", "model", "--thinking", "high"}},
		{name: "child fork", env: []string{"KANEDIAS_SESSION_ID=child-2", "KANEDIAS_SESSION_KIND=read", "KANEDIAS_PI_SESSION_FILE=/sessions/branch.jsonl", "KANEDIAS_PI_PROVIDER=provider", "KANEDIAS_PI_MODEL=model", "KANEDIAS_PI_THINKING=xhigh"}, want: []string{"--mode", "rpc", "--session", "/sessions/branch.jsonl", "-e", "/opt/kanedias/pi-extension/src/index.ts", "--provider", "provider", "--model", "model", "--thinking", "xhigh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := exec.Command(launcherPath)
			command.Env = append([]string{"PATH=" + dir + ":/usr/bin:/bin"}, tt.env...)
			output, err := command.Output()
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Fields(string(output))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("args = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestPiRPCLauncherRequiresSessionIDBeforeInvokingPi(t *testing.T) {
	dir := t.TempDir()
	launcher := strings.Replace(string(piRPCLauncher), `source "$NVM_DIR/nvm.sh"`, ":", 1)
	launcherPath := filepath.Join(dir, "launcher")
	if err := os.WriteFile(launcherPath, []byte(launcher), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "pi-invoked")
	piPath := filepath.Join(dir, "pi")
	if err := os.WriteFile(piPath, []byte("#!/bin/sh\ntouch \"$PI_INVOKED_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(launcherPath)
	command.Env = []string{
		"PATH=" + dir + ":/usr/bin:/bin",
		"KANEDIAS_SESSION_KIND=root",
		"PI_INVOKED_MARKER=" + marker,
	}
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("launcher succeeded without a session ID; output: %s", output)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Pi invocation marker stat error = %v, want not exist", err)
	}
}

func TestCreateRunsImageWorkflowInOrder(t *testing.T) {
	cfg := imageConfig(t, []string{"github.com", "gitlab.com"})
	client := &recordingClient{files: make(map[string]uploadedFile)}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := create(context.Background(), cfg, &stdout, &stderr, func(context.Context) (imageClient, error) {
		return client, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	wantCalls := []string{
		"resolve-pool",
		"get-network kanedias",
		"create-network kanedias",
		"ensure-profile image-build",
		"create-instance",
		"push /root/install.sh",
		"exec install -d /root/assets",
		"push /root/assets/authorized_hosts",
		"push /root/assets/pi-settings.json",
		"push /root/assets/pi-auth.json",
		"push /root/assets/pi-models.json",
		"push /root/assets/cobalt-ember.json",
		"push /root/assets/tmux.conf",
		"push /root/assets/kanedias-pi.socket",
		"push /root/assets/kanedias-pi@.service",
		"push /root/assets/kanedias-pi-env",
		"push /root/assets/kanedias-pi-rpc",
		"exec install -d /root/assets/pi-extension/skills/delegate-session /root/assets/pi-extension/skills/writer-handoff /root/assets/pi-extension/src",
		"push /root/assets/pi-extension/package-lock.json",
		"push /root/assets/pi-extension/package.json",
		"push /root/assets/pi-extension/skills/delegate-session/SKILL.md",
		"push /root/assets/pi-extension/skills/writer-handoff/SKILL.md",
		"push /root/assets/pi-extension/src/fork.ts",
		"push /root/assets/pi-extension/src/git-handoff.ts",
		"push /root/assets/pi-extension/src/index.ts",
		"push /root/assets/pi-extension/src/schemas.ts",
		"push /root/assets/pi-extension/src/supervisor-client.ts",
		"push /root/assets/pi-extension/src/types.ts",
		"exec bash /root/install.sh",
		"exec test -d /opt/kanedias/pi-extension/node_modules/typebox",
		"stop",
		"publish",
		"cleanup-delete-instance",
	}
	if !reflect.DeepEqual(client.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", client.calls, wantCalls)
	}
	if client.profileDefinition == nil {
		t.Fatal("image-build profile definition was not supplied")
	}
	for _, want := range []string{"eth0:", "network: kanedias", "type: nic"} {
		if !strings.Contains(string(client.profileDefinition), want) {
			t.Errorf("image-build profile missing %q:\n%s", want, client.profileDefinition)
		}
	}

	request := client.createRequest
	if !strings.HasPrefix(request.Name, "image-build-") {
		t.Errorf("instance name = %q, want image-build prefix", request.Name)
	}
	if request.Type != api.InstanceTypeContainer || !request.Start {
		t.Errorf("instance type/start = %q/%v, want container/true", request.Type, request.Start)
	}
	if got, want := request.Profiles, []string{"default", "image-build"}; !reflect.DeepEqual(got, want) {
		t.Errorf("profiles = %#v, want %#v", got, want)
	}
	wantRoot := map[string]string{"type": "disk", "pool": "pool1", "path": "/"}
	if got := request.Devices["root"]; !reflect.DeepEqual(got, wantRoot) {
		t.Errorf("root device = %#v, want %#v", got, wantRoot)
	}
	if request.Source.Type != "image" || request.Source.Server != cfg.BaseImage.Source || request.Source.Protocol != "simplestreams" || request.Source.Alias != cfg.BaseImage.Image {
		t.Errorf("instance source = %#v", request.Source)
	}
	if got := string(client.files["/root/assets/authorized_hosts"].content); got != "github.com\ngitlab.com" {
		t.Errorf("authorized_hosts = %q, want newline-joined hosts", got)
	}
	socket := string(client.files["/root/assets/kanedias-pi.socket"].content)
	if !strings.Contains(socket, "ListenStream=0.0.0.0:7777") ||
		!strings.Contains(socket, "Accept=yes") ||
		!strings.Contains(socket, "MaxConnections=1") {
		t.Fatalf("socket unit = %q", socket)
	}
	service := string(client.files["/root/assets/kanedias-pi@.service"].content)
	for _, want := range []string{
		"User=kanedias",
		"Group=kanedias",
		"EnvironmentFile=-/run/kanedias-pi/pi.env",
		"ExecStartPre=+/usr/bin/env -i /usr/bin/bash --noprofile --norc /usr/local/libexec/kanedias-pi-env",
		"WorkingDirectory=/workspace",
		"StandardInput=socket",
		"StandardOutput=inherit",
		"StandardError=journal",
	} {
		if !strings.Contains(service, want) {
			t.Errorf("service unit missing %q", want)
		}
	}
	auth := client.files["/root/assets/pi-auth.json"]
	if got := string(auth.content); got != "auth" {
		t.Errorf("pi auth = %q, want test asset", got)
	}
	if auth.mode != 0o600 {
		t.Errorf("pi auth mode = %#o, want 0600", auth.mode)
	}
	bridge := client.files["/root/assets/kanedias-pi-env"]
	if bridge.mode != 0o700 {
		t.Errorf("environment bridge mode = %#o, want 0700", bridge.mode)
	}
	launcher := client.files["/root/assets/kanedias-pi-rpc"]
	if !strings.Contains(string(launcher.content), `exec pi "${args[@]}"`) || !strings.Contains(string(launcher.content), "/opt/kanedias/pi-extension/src/index.ts") {
		t.Fatalf("launcher = %q", launcher.content)
	}
	if launcher.mode != 0o700 {
		t.Errorf("launcher mode = %#o, want 0700", launcher.mode)
	}
	for _, want := range []string{
		`"$pi_environment_bridge_file" /usr/local/libexec/kanedias-pi-env`,
		`"$pi_rpc_launcher_file" /usr/local/libexec/kanedias-pi-rpc`,
	} {
		if !strings.Contains(string(installer), want) {
			t.Errorf("installer missing Pi RPC install behavior %q", want)
		}
	}
	for _, path := range []string{
		"/root/assets/kanedias-pi.socket",
		"/root/assets/kanedias-pi@.service",
	} {
		if client.files[path].mode != 0o644 {
			t.Errorf("%s mode = %#o, want 0644", path, client.files[path].mode)
		}
	}
	for path, file := range client.files {
		if !strings.HasPrefix(path, "/root/assets/pi-extension/") {
			continue
		}
		if file.mode != 0o644 {
			t.Errorf("%s mode = %#o, want 0644", path, file.mode)
		}
		contents := string(file.content)
		for _, forbidden := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "api-key=", "Bearer sk-"} {
			if strings.Contains(contents, forbidden) {
				t.Errorf("%s contains credential marker %q", path, forbidden)
			}
		}
	}
	if _, present := client.files["/root/assets/pi-extension/node_modules/typebox"]; present {
		t.Error("development node_modules was uploaded instead of installed in the image")
	}
	if got, want := client.publishAlias, cfg.BaseImage.Name; got != want {
		t.Errorf("published alias = %q, want %q", got, want)
	}
	if got, want := client.publishDescription, "kanedias sandbox from https://images.linuxcontainers.org/debian/13"; got != want {
		t.Errorf("published description = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "installer output") {
		t.Errorf("stdout = %q, want streamed installer output", got)
	}
}

func TestCreateUploadsEmptyAuthorizedHosts(t *testing.T) {
	cfg := imageConfig(t, nil)
	client := &recordingClient{files: make(map[string]uploadedFile)}

	if err := create(context.Background(), cfg, io.Discard, io.Discard, func(context.Context) (imageClient, error) {
		return client, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := client.files["/root/assets/authorized_hosts"].content; len(got) != 0 {
		t.Errorf("authorized_hosts = %q, want empty file", got)
	}
}

func TestCreateUsesRequestDerivedNonCanceledBoundedContextForCleanup(t *testing.T) {
	cfg := imageConfig(t, nil)
	const sentinel = "request-value"
	ctx := context.WithValue(context.Background(), imageRequestContextKey{}, sentinel)
	ctx, cancel := context.WithCancel(ctx)
	client := &recordingClient{
		files: make(map[string]uploadedFile),
		exec: func() error {
			cancel()
			return context.Canceled
		},
	}

	err := create(ctx, cfg, io.Discard, io.Discard, func(context.Context) (imageClient, error) {
		return client, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("create() error = %v, want context cancellation", err)
	}
	if got, want := client.calls[len(client.calls)-2:], []string{"cleanup-stop", "cleanup-delete-instance"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup calls = %#v, want %#v", got, want)
	}
	if len(client.cleanupContexts) != 2 {
		t.Fatalf("cleanup contexts = %d, want 2", len(client.cleanupContexts))
	}
	for _, observed := range client.cleanupContexts {
		if observed.err != nil {
			t.Errorf("cleanup context error = %v, want non-canceled context", observed.err)
		}
		if observed.deadlineRemaining <= 0 || observed.deadlineRemaining > 30*time.Second {
			t.Errorf("cleanup deadline remaining = %v, want bounded by 30s", observed.deadlineRemaining)
		}
		if observed.value != sentinel {
			t.Errorf("cleanup context value = %v, want %q", observed.value, sentinel)
		}
	}
}

func TestCreateJoinsPrimaryAndRunningInstanceCleanupErrors(t *testing.T) {
	cfg := imageConfig(t, nil)
	primaryErr := errors.New("installer failed")
	stopErr := errors.New("cleanup stop failed")
	client := &recordingClient{
		files:   make(map[string]uploadedFile),
		exec:    func() error { return primaryErr },
		stopErr: stopErr,
	}

	err := create(context.Background(), cfg, io.Discard, io.Discard, func(context.Context) (imageClient, error) {
		return client, nil
	})
	for _, want := range []error{primaryErr, stopErr, errDeleteRunningInstance} {
		if !errors.Is(err, want) {
			t.Errorf("create() error = %v, want errors.Is(_, %v)", err, want)
		}
	}
	if got, want := client.calls[len(client.calls)-2:], []string{"cleanup-stop", "cleanup-delete-instance"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup calls = %#v, want %#v", got, want)
	}
}

func TestCreateReadsAssetsBeforeConnecting(t *testing.T) {
	cfg := imageConfig(t, nil)
	if err := os.Remove(cfg.AssetPath("tmux.conf")); err != nil {
		t.Fatal(err)
	}
	connected := false

	err := create(context.Background(), cfg, io.Discard, io.Discard, func(context.Context) (imageClient, error) {
		connected = true
		return &recordingClient{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "tmux.conf") {
		t.Fatalf("create() error = %v, want missing asset error", err)
	}
	if connected {
		t.Fatal("connected to Incus before all assets were read")
	}
}

func TestCreateValidatesBeforeConnecting(t *testing.T) {
	cfg := imageConfig(t, nil)
	cfg.BaseImage.Name = ""
	connected := false

	err := create(context.Background(), cfg, io.Discard, io.Discard, func(context.Context) (imageClient, error) {
		connected = true
		return &recordingClient{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "base_image.name is required") {
		t.Fatalf("create() error = %v, want validation error", err)
	}
	if connected {
		t.Fatal("connected to Incus before validating lifecycle config")
	}
}

func imageConfig(t *testing.T, hosts []string) config.Config {
	t.Helper()
	dir := t.TempDir()
	assetDir := filepath.Join(dir, "assets")
	if err := os.Mkdir(assetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"pi-settings.json":  "settings",
		"pi-auth.json":      "auth",
		"pi-models.json":    "models",
		"cobalt-ember.json": "theme",
		"tmux.conf":         "tmux",
	} {
		if err := os.WriteFile(filepath.Join(assetDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return config.Config{
		Dir:     dir,
		Network: config.Network{IPv4: "10.76.111.1/24"},
		BaseImage: config.BaseImage{
			Name:            "sandbox",
			Source:          "https://images.linuxcontainers.org",
			Image:           "debian/13",
			AuthorizedHosts: hosts,
		},
	}
}

type imageRequestContextKey struct{}

type cleanupContextObservation struct {
	err               error
	deadlineRemaining time.Duration
	value             any
}

var errDeleteRunningInstance = errors.New("cannot delete running instance")

type uploadedFile struct {
	content []byte
	mode    int
}

type recordingClient struct {
	calls              []string
	files              map[string]uploadedFile
	profileDefinition  []byte
	createRequest      api.InstancesPost
	publishAlias       string
	publishDescription string
	exec               func() error
	running            bool
	stopErr            error
	cleanupContexts    []cleanupContextObservation
}

func (c *recordingClient) ResolvePool(context.Context, string) (string, error) {
	c.calls = append(c.calls, "resolve-pool")
	return "pool1", nil
}

func (c *recordingClient) GetNetwork(context.Context, string) (*api.Network, error) {
	c.calls = append(c.calls, "get-network kanedias")
	return nil, api.StatusErrorf(404, "missing")
}

func (c *recordingClient) CreateNetwork(_ context.Context, request api.NetworksPost) error {
	c.calls = append(c.calls, "create-network "+request.Name)
	return nil
}

func (c *recordingClient) EnsureProfile(_ context.Context, name string, definition []byte) error {
	c.calls = append(c.calls, "ensure-profile "+name)
	c.profileDefinition = append([]byte(nil), definition...)
	return nil
}

func (c *recordingClient) CreateInstance(_ context.Context, request api.InstancesPost) error {
	c.calls = append(c.calls, "create-instance")
	c.createRequest = request
	c.running = request.Start
	return nil
}

func (c *recordingClient) PushFile(_ context.Context, _ string, path string, content []byte, mode int) error {
	c.calls = append(c.calls, "push "+path)
	c.files[path] = uploadedFile{content: append([]byte(nil), content...), mode: mode}
	return nil
}

func (c *recordingClient) Exec(_ context.Context, _ string, request incusclient.ExecRequest) (string, string, error) {
	c.calls = append(c.calls, "exec "+strings.Join(request.Command, " "))
	if c.exec != nil {
		return "", "", c.exec()
	}
	const output = "installer output"
	// Mirror the real Incus layer: output is streamed to the caller's writer as
	// it arrives, not only returned after the command completes.
	if request.Stdout != nil {
		_, _ = io.WriteString(request.Stdout, output)
	}
	return output, "", nil
}

func (c *recordingClient) StopInstance(ctx context.Context, _ string, force bool) error {
	if _, cleanup := ctx.Deadline(); cleanup {
		c.calls = append(c.calls, "cleanup-stop")
		c.observeCleanupContext(ctx)
		if !force {
			return errors.New("cleanup stop was not forced")
		}
	} else {
		c.calls = append(c.calls, "stop")
	}
	if c.stopErr != nil {
		return c.stopErr
	}
	c.running = false
	return nil
}

func (c *recordingClient) PublishInstance(_ context.Context, _ string, alias, description string) error {
	c.calls = append(c.calls, "publish")
	c.publishAlias = alias
	c.publishDescription = description
	return nil
}

func (c *recordingClient) DeleteInstance(ctx context.Context, _ string) error {
	c.calls = append(c.calls, "cleanup-delete-instance")
	c.observeCleanupContext(ctx)
	if c.running {
		return errDeleteRunningInstance
	}
	return nil
}

func (c *recordingClient) observeCleanupContext(ctx context.Context) {
	deadline, _ := ctx.Deadline()
	c.cleanupContexts = append(c.cleanupContexts, cleanupContextObservation{
		err:               ctx.Err(),
		deadlineRemaining: time.Until(deadline),
		value:             ctx.Value(imageRequestContextKey{}),
	})
}

func (c *recordingClient) Disconnect() {}
