package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultListenAddress   = "127.0.0.1:8080"
	defaultShutdownTimeout = 10 * time.Second
)

type Options struct {
	ListenAddress string
	Logger        *slog.Logger
}

type listenFunc func(network, address string) (net.Listener, error)

func ValidateListenAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("validate listen address %q: split host and port: %w", address, err)
	}
	if host == "" {
		return fmt.Errorf("validate listen address %q: host must not be empty", address)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("validate listen address %q: parse port: %w", address, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("validate listen address %q: host must be localhost or a loopback IP address", address)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("validate listen address %q: IP address is not loopback", address)
	}

	return nil
}

func Run(ctx context.Context, options Options) error {
	if ctx == nil {
		return errors.New("run server: context must not be nil")
	}

	normalizedOptions, err := prepareOptions(options)
	if err != nil {
		return fmt.Errorf("run server: %w", err)
	}

	handler, err := newHandler(normalizedOptions.Logger)
	if err != nil {
		return fmt.Errorf("run server: construct handler: %w", err)
	}

	return run(ctx, normalizedOptions, handler, net.Listen, defaultShutdownTimeout)
}

func prepareOptions(options Options) (Options, error) {
	if options.Logger == nil {
		return Options{}, errors.New("prepare server options: logger must not be nil")
	}
	if options.ListenAddress == "" {
		options.ListenAddress = DefaultListenAddress
	}
	if err := ValidateListenAddress(options.ListenAddress); err != nil {
		return Options{}, fmt.Errorf("prepare server options: %w", err)
	}

	return options, nil
}

func newHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		WriteTimeout:      0,
	}
}

func run(
	ctx context.Context,
	options Options,
	handler http.Handler,
	listen listenFunc,
	shutdownTimeout time.Duration,
) error {
	if ctx == nil {
		return errors.New("run server: context must not be nil")
	}

	normalizedOptions, err := prepareOptions(options)
	if err != nil {
		return fmt.Errorf("run server: %w", err)
	}

	logger := normalizedOptions.Logger
	requestedAddress := normalizedOptions.ListenAddress
	logger.InfoContext(ctx, "server starting", "requested_address", requestedAddress)

	listener, err := listen("tcp", requestedAddress)
	if err != nil {
		logger.ErrorContext(ctx, "server listen failed", "address", requestedAddress, "error", err)
		return fmt.Errorf("run server: listen on %q: %w", requestedAddress, err)
	}

	effectiveAddress := listener.Addr().String()
	logger.InfoContext(
		ctx,
		"server listening",
		"requested_address", requestedAddress,
		"effective_address", effectiveAddress,
	)

	httpServer := newHTTPServer(requestedAddress, handler)
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- httpServer.Serve(listener)
	}()

	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		logger.ErrorContext(
			ctx,
			"server serve failed",
			"requested_address", requestedAddress,
			"effective_address", effectiveAddress,
			"error", err,
		)
		return fmt.Errorf("run server: serve HTTP: %w", err)
	case <-ctx.Done():
		logger.Info(
			"server shutdown started",
			"requested_address", requestedAddress,
			"effective_address", effectiveAddress,
		)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			logger.Error(
				"server shutdown failed",
				"requested_address", requestedAddress,
				"effective_address", effectiveAddress,
				"error", shutdownErr,
			)
			if closeErr := httpServer.Close(); closeErr != nil {
				logger.Error(
					"server forced close failed",
					"requested_address", requestedAddress,
					"effective_address", effectiveAddress,
					"error", closeErr,
				)
			}
			return fmt.Errorf("run server: shutdown HTTP: %w", shutdownErr)
		}

		logger.Info(
			"server shutdown complete",
			"requested_address", requestedAddress,
			"effective_address", effectiveAddress,
		)
		return nil
	}
}
