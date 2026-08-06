//go:build incus

package image

import (
	"context"
	"os"
	"testing"

	"github.com/sklarsa/kanedias/internal/config"
)

func TestLiveCreate(t *testing.T) {
	if os.Getenv("KANEDIAS_LIVE_IMAGE_CREATE") != "1" {
		t.Skip("set KANEDIAS_LIVE_IMAGE_CREATE=1 to run the live image creation smoke test")
	}

	configPath := os.Getenv("KANEDIAS_CONFIG")
	if configPath == "" {
		configPath = "./config.toml"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := Create(context.Background(), cfg, os.Stdout, os.Stderr); err != nil {
		t.Fatal(err)
	}
}
