package profiles

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/config"
)

func TestRenderSandboxUsesConfiguredIPv4(t *testing.T) {
	cfg := config.Config{Network: config.Network{IPv4: "10.76.111.1/24"}}
	var output bytes.Buffer
	if err := Render(&output, "sandbox", cfg); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"environment.HTTP_PROXY", "environment.HTTPS_PROXY",
		"environment.http_proxy", "environment.https_proxy",
	} {
		want := key + ": \"http://10.76.111.1:3128\""
		if !strings.Contains(output.String(), want) {
			t.Errorf("rendered sandbox missing %q", want)
		}
	}
	if strings.Contains(output.String(), "10.75.177.1") {
		t.Fatal("rendered sandbox retained the old hard-coded endpoint")
	}
}

func TestRenderProfiles(t *testing.T) {
	tests := []struct {
		name        string
		description string
	}{
		{name: "image-build", description: "Unprivileged container with nesting for Docker and kind"},
		{name: "lemonade", description: "Expose the host Lemonade GPU service inside the container"},
		{name: "sandbox", description: "Kanedias sandbox with persistent workspace and credential proxy"},
	}

	cfg := config.Config{Network: config.Network{IPv4: "10.76.111.1/24"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := Render(&output, tt.name, cfg); err != nil {
				t.Fatal(err)
			}
			if output.Len() == 0 {
				t.Fatal("Render() produced empty output")
			}
			if !strings.HasSuffix(output.String(), "\n") {
				t.Fatal("Render() output does not end with a newline")
			}
			if !strings.Contains(output.String(), "description: "+tt.description) {
				t.Errorf("Render() output missing description %q", tt.description)
			}
		})
	}
}

func TestRenderUnknownType(t *testing.T) {
	const badName = "unknown"
	var output bytes.Buffer
	err := Render(&output, badName, config.Config{})
	if err == nil {
		t.Fatal("Render() returned nil error for unknown type")
	}
	for _, want := range []string{badName, "image-build", "lemonade", "sandbox"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Render() error %q does not contain %q", err, want)
		}
	}
}

func TestRenderSandboxRejectsInvalidIPv4(t *testing.T) {
	cfg := config.Config{Network: config.Network{IPv4: "not-a-prefix"}}
	var output bytes.Buffer
	err := Render(&output, "sandbox", cfg)
	if err == nil {
		t.Fatal("Render() returned nil error for invalid sandbox IPv4")
	}
	if !strings.Contains(err.Error(), "network.ipv4") {
		t.Errorf("Render() error %q does not contain network.ipv4", err)
	}
}

func TestTypes(t *testing.T) {
	want := []string{"image-build", "lemonade", "sandbox"}
	got := Types()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Types() = %v, want %v", got, want)
	}

	got[0] = "changed"
	if next := Types(); !reflect.DeepEqual(next, want) {
		t.Fatalf("Types() returned shared package state: %v", next)
	}
}
