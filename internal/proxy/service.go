package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Options configures the proxy service.
type Options struct {
	ListenAddress         string
	MetricsListenAddress  string
	RequestLog            bool
	CACertPath            string
	CAKeyPath             string
	ClaudeCredentialsPath string
	OpenAICodexAuthPath   string
	Logger                *slog.Logger
}

// DefaultOptions returns proxy options using the current user's config and home directories.
func DefaultOptions() (Options, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return Options{}, fmt.Errorf("resolve user config directory: %w", err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return Options{}, fmt.Errorf("resolve user home directory: %w", err)
	}
	paths := defaultOAuthPaths(configDir, homeDir)
	defaultDir := filepath.Join(configDir, "kanedias-proxy")
	return Options{
		ListenAddress:         "127.0.0.1:3128",
		CACertPath:            filepath.Join(defaultDir, "ca.crt"),
		CAKeyPath:             filepath.Join(defaultDir, "ca.key"),
		ClaudeCredentialsPath: paths.claude,
		OpenAICodexAuthPath:   paths.openAICodex,
	}, nil
}

// InitCA creates the proxy certificate authority unless it already exists.
func InitCA(certPath, keyPath string) error {
	_, _, err := loadOrCreateCA(certPath, keyPath)
	return err
}

// LoginOpenAICodex performs an OpenAI Codex device login and saves the resulting credential.
func LoginOpenAICodex(ctx context.Context, authPath string, out io.Writer) error {
	return newOpenAICodexOAuthSource(authPath).Login(ctx, out)
}

// Run starts the proxy and optional metrics servers and blocks until either server fails.
func Run(options Options) error {
	return RunContext(context.Background(), options)
}

// RunContext starts the proxy and optional metrics servers until either a server fails or ctx is canceled.
func RunContext(ctx context.Context, options Options) error {
	if strings.TrimSpace(options.ListenAddress) == "" {
		return errors.New("proxy listen address must not be empty")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	ca, _, err := loadOrCreateCA(options.CACertPath, options.CAKeyPath)
	if err != nil {
		return fmt.Errorf("initialize proxy CA: %w", err)
	}

	var metrics *proxyMetrics
	var metricsHandler http.Handler
	if options.MetricsListenAddress != "" {
		var registry *prometheus.Registry
		metrics, registry = newProxyMetrics()
		metricsHandler = newMetricsHandler(registry)
	}
	var observer *proxyObserver
	if options.RequestLog || metrics != nil {
		observer = newProxyObserver(logger, options.RequestLog, metrics)
	}
	creds := loadCredentials(options.ClaudeCredentialsPath, options.OpenAICodexAuthPath)
	proxy := newProxyWithObserver(ca, creds, observer)

	serverErrors := make(chan error, 2)
	proxyServer := &http.Server{
		Addr:              options.ListenAddress,
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		serverErrors <- fmt.Errorf("proxy listener: %w", proxyServer.ListenAndServe())
	}()
	logger.Info("proxy listening", "address", options.ListenAddress, "ca_certificate", options.CACertPath, "request_logging", options.RequestLog)

	var metricsServer *http.Server
	if metricsHandler != nil {
		metricsServer = &http.Server{
			Addr:              options.MetricsListenAddress,
			Handler:           metricsHandler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			serverErrors <- fmt.Errorf("metrics listener: %w", metricsServer.ListenAndServe())
		}()
		logger.Info("Prometheus metrics listening", "address", options.MetricsListenAddress, "path", "/metrics")
	}

	select {
	case err = <-serverErrors:
		logger.Error("proxy stopped", "error", err)
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr := proxyServer.Shutdown(shutdownCtx)
		if metricsServer != nil {
			shutdownErr = errors.Join(shutdownErr, metricsServer.Shutdown(shutdownCtx))
		}
		return errors.Join(ctx.Err(), shutdownErr)
	}
}
