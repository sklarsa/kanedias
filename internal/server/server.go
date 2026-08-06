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

type shutdownFunc func(context.Context) error

type closeFunc func() error

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
	listenAddress := requestedAddress
	if host, port, splitErr := net.SplitHostPort(requestedAddress); splitErr == nil && strings.EqualFold(host, "localhost") {
		listenAddress = net.JoinHostPort("127.0.0.1", port)
	}
	logger.InfoContext(ctx, "server starting", "requested_address", requestedAddress)

	listener, err := listen("tcp", listenAddress)
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

	httpServer := newHTTPServer(listenAddress, handler)
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- httpServer.Serve(listener)
	}()

	shutdownStarted, shutdownErr, closeErr, serveErr := coordinateServer(
		ctx,
		serveResult,
		shutdownTimeout,
		func(shutdownCtx context.Context) error {
			logger.Info(
				"server shutdown started",
				"requested_address", requestedAddress,
				"effective_address", effectiveAddress,
			)
			return httpServer.Shutdown(shutdownCtx)
		},
		httpServer.Close,
	)

	if !shutdownStarted {
		if serveErr != nil {
			logger.ErrorContext(
				ctx,
				"server serve failed",
				"requested_address", requestedAddress,
				"effective_address", effectiveAddress,
				"error", serveErr,
			)
		}
		return serverRunError(false, nil, serveErr)
	}

	if shutdownErr != nil {
		logger.Error(
			"server shutdown failed",
			"requested_address", requestedAddress,
			"effective_address", effectiveAddress,
			"error", shutdownErr,
		)
	}
	if closeErr != nil {
		logger.Error(
			"server forced close failed",
			"requested_address", requestedAddress,
			"effective_address", effectiveAddress,
			"error", closeErr,
		)
	}

	unexpectedServeErr := serveErr
	if errors.Is(serveErr, http.ErrServerClosed) {
		unexpectedServeErr = nil
	}
	if unexpectedServeErr != nil {
		logger.Error(
			"server serve failed",
			"requested_address", requestedAddress,
			"effective_address", effectiveAddress,
			"error", unexpectedServeErr,
		)
	}
	if shutdownErr == nil {
		logger.Info(
			"server shutdown complete",
			"requested_address", requestedAddress,
			"effective_address", effectiveAddress,
		)
	}

	return serverRunError(true, shutdownErr, serveErr)
}

func serverRunError(shutdownStarted bool, shutdownErr, serveErr error) error {
	var result []error
	if shutdownErr != nil {
		result = append(result, fmt.Errorf("run server: shutdown HTTP: %w", shutdownErr))
	}
	if serveErr != nil && (!shutdownStarted || !errors.Is(serveErr, http.ErrServerClosed)) {
		result = append(result, fmt.Errorf("run server: serve HTTP: %w", serveErr))
	}
	return errors.Join(result...)
}

func coordinateServer(
	ctx context.Context,
	serveResult <-chan error,
	shutdownTimeout time.Duration,
	shutdown shutdownFunc,
	forceClose closeFunc,
) (shutdownStarted bool, shutdownErr, closeErr, serveErr error) {
	if ctx.Err() == nil {
		select {
		case serveErr = <-serveResult:
			return false, nil, nil, serveErr
		case <-ctx.Done():
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	shutdownErr = shutdown(shutdownCtx)
	cancel()
	if shutdownErr != nil {
		closeErr = forceClose()
	}
	serveErr = <-serveResult
	return true, shutdownErr, closeErr, serveErr
}
