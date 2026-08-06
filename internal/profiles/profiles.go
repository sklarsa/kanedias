package profiles

import (
	"embed"
	"fmt"
	"io"
	"net"
	"strings"
	"text/template"

	"github.com/sklarsa/kanedias/internal/config"
)

type Type string

const (
	ImageBuild Type = "image-build"
	Lemonade   Type = "lemonade"
	Sandbox    Type = "sandbox"
)

//go:embed *.yaml
var profileFiles embed.FS

var profilePaths = map[string]string{
	string(ImageBuild): "image-build.yaml",
	string(Lemonade):   "lemonade.yaml",
	string(Sandbox):    "sandbox.yaml",
}

type templateData struct {
	ProxyURL string
}

func Types() []string {
	return []string{string(ImageBuild), string(Lemonade), string(Sandbox)}
}

func Render(w io.Writer, name string, cfg config.Config) error {
	path, ok := profilePaths[name]
	if !ok {
		return fmt.Errorf("unknown profile type %q (supported: %s)", name, strings.Join(Types(), ", "))
	}

	data := templateData{}
	if name == string(Sandbox) {
		prefix, err := cfg.Network.IPv4Prefix()
		if err != nil {
			return fmt.Errorf("render profile %q: %w", name, err)
		}
		data.ProxyURL = "http://" + net.JoinHostPort(prefix.Addr().String(), "3128")
	}

	contents, err := profileFiles.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read profile %q: %w", name, err)
	}

	tmpl, err := template.New(name).Option("missingkey=error").Parse(string(contents))
	if err != nil {
		return fmt.Errorf("parse profile %q: %w", name, err)
	}
	if err := tmpl.Execute(w, data); err != nil {
		return fmt.Errorf("execute profile %q: %w", name, err)
	}
	return nil
}
