package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/manager"
	"github.com/sklarsa/kanedias/internal/supervisor"
)

const (
	DefaultListenAddress   = "127.0.0.1:8080"
	defaultShutdownTimeout = 10 * time.Second
)

// Options configures the server process.
type Options struct {
	ListenAddress   string
	Logger          *slog.Logger
	BootstrapOutput io.Writer
	ConfigPath      string
}

// fleetManager is the interface the server uses to interact with the manager.
// It is satisfied by *manager.Manager and can be faked in tests.
type fleetManager interface {
	Start(context.Context) error
	Fleet() manager.FleetSnapshot
	Session(sessionID string) (manager.SessionState, error)
	SubscribeFleet() manager.ChangeSubscription
	SubscribeSession(sessionID string) (manager.ChangeSubscription, error)
	SpawnRoot(ctx context.Context) (string, error)
	Steer(ctx context.Context, sessionID string, message string) error
	Interrupt(ctx context.Context, sessionID string) error
	StopSession(ctx context.Context, sessionID string) error
	AnswerQuestion(ctx context.Context, sessionID string, questionID string, answer json.RawMessage) error
	SessionStats(ctx context.Context, sessionID string) (manager.SessionStats, error)
	Quiesce(context.Context) error
	Close(context.Context) error
}

type managerFactory func(manager.Options) (fleetManager, error)

type listenFunc func(network, address string) (net.Listener, error)

// handlerFactory is called after net.Listen so security can lock exact Host/Origin.
type handlerFactory func(effectiveAddress string, streamContext context.Context) (http.Handler, error)

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

	if net.ParseIP(host) == nil {
		return fmt.Errorf("validate listen address %q: host must be localhost or an IP address", address)
	}
	return nil
}

// Run is the public entry point used by cmd/server.go.
func Run(ctx context.Context, cfg config.Config, options Options) error {
	return runApplication(ctx, cfg, options, productionManagerFactory, net.Listen)
}

// productionManagerFactory wraps manager.New to satisfy managerFactory.
func productionManagerFactory(opts manager.Options) (fleetManager, error) {
	return manager.New(opts)
}

// runApplication is the testable inner entry point. It constructs the manager,
// starts it, then delegates to the existing run function.
func runApplication(
	ctx context.Context,
	cfg config.Config,
	options Options,
	newManager managerFactory,
	listen listenFunc,
) error {
	if ctx == nil {
		return errors.New("run server: context must not be nil")
	}

	normalizedOptions, err := prepareOptions(options)
	if err != nil {
		return fmt.Errorf("run server: %w", err)
	}

	resolved, err := cfg.Server.Resolve()
	if err != nil {
		return fmt.Errorf("run server: resolve server config: %w", err)
	}

	limits, err := cfg.Supervisor.Events.Limits()
	if err != nil {
		return fmt.Errorf("run server: resolve event limits: %w", err)
	}

	fleet, err := newManager(manager.Options{
		ConfigPath:        normalizedOptions.ConfigPath,
		RootSocketDir:     resolved.RootSocketDir,
		SessionLogDir:     resolved.SessionLogDir,
		SessionBinary:     resolved.SessionBinary,
		DiscoveryInterval: resolved.DiscoveryInterval,
		SnapshotInterval:  resolved.SnapshotInterval,
		SpawnTimeout:      resolved.SpawnTimeout,
		EventLimits: supervisor.EventBrokerOptions{
			MaxEvents: limits.MaxEvents,
			MaxBytes:  limits.MaxBytes,
		},
		Logger: normalizedOptions.Logger,
	})
	if err != nil {
		return fmt.Errorf("run server: create manager: %w", err)
	}

	if err := fleet.Start(ctx); err != nil {
		return errors.Join(
			fmt.Errorf("run server: start manager: %w", err),
			fleet.Close(context.Background()),
		)
	}

	bootstrapOutput := normalizedOptions.BootstrapOutput
	if bootstrapOutput == nil {
		bootstrapOutput = io.Discard
	}
	handlerFn := func(effectiveAddress string, streamCtx context.Context) (http.Handler, error) {
		return newHandlerWithOptions(normalizedOptions.Logger, effectiveAddress, bootstrapOutput, fleet, streamCtx, resolved.RequireSession)
	}

	return runWithManager(ctx, normalizedOptions, fleet, handlerFn, listen, defaultShutdownTimeout)
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

// run is the legacy test-facing entry point that does not require a manager or config.
// Kept for backward compatibility with existing server_test.go tests.
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

	return runCore(ctx, normalizedOptions, handler, listen, shutdownTimeout)
}

// runWithManager runs the server with phased manager shutdown ordering.
func runWithManager(
	ctx context.Context,
	options Options,
	fleet fleetManager,
	makeHandler handlerFactory,
	listen listenFunc,
	shutdownTimeout time.Duration,
) (resultErr error) {
	logger := options.Logger
	managerClosed := false
	defer func() {
		if managerClosed {
			return
		}
		cleanupTimeout := shutdownTimeout
		if cleanupTimeout <= 0 {
			cleanupTimeout = defaultShutdownTimeout
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cleanupCancel()
		quiesceErr := fleet.Quiesce(cleanupCtx)
		closeErr := fleet.Close(cleanupCtx)
		if quiesceErr != nil || closeErr != nil {
			logger.Error("manager failure-path cleanup failed", "error", errors.Join(quiesceErr, closeErr))
		}
		resultErr = errors.Join(resultErr, quiesceErr, closeErr)
	}()
	requestedAddress := options.ListenAddress
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

	// Stream context is separate from the manager context: we cancel streams
	// before http.Server.Shutdown, which allows SSE connections to drain.
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()

	handler, err := makeHandler(effectiveAddress, streamCtx)
	if err != nil {
		_ = listener.Close()
		streamCancel()
		return fmt.Errorf("run server: construct handler: %w", err)
	}

	httpServer := newHTTPServer(listenAddress, handler)
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- httpServer.Serve(listener)
	}()

	// Wait for context cancellation or unexpected serve error.
	var serveErr error
	if ctx.Err() == nil {
		select {
		case serveErr = <-serveResult:
			// Unexpected serve error — no shutdown sequence needed.
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
		case <-ctx.Done():
		}
	}

	// Phased shutdown: Quiesce → cancel streams → HTTP Shutdown → manager.Close
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	_ = fleet.Quiesce(shutdownCtx)
	streamCancel()

	logger.Info(
		"server shutdown started",
		"requested_address", requestedAddress,
		"effective_address", effectiveAddress,
	)
	shutdownErr := httpServer.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = httpServer.Close()
	}
	managerErr := fleet.Close(shutdownCtx)
	managerClosed = managerErr == nil
	serveErr = <-serveResult

	if shutdownErr != nil {
		logger.Error(
			"server shutdown failed",
			"requested_address", requestedAddress,
			"effective_address", effectiveAddress,
			"error", shutdownErr,
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
	if managerErr != nil {
		logger.Error(
			"manager close failed",
			"error", managerErr,
		)
	}
	if shutdownErr == nil {
		logger.Info(
			"server shutdown complete",
			"requested_address", requestedAddress,
			"effective_address", effectiveAddress,
		)
	}

	return errors.Join(
		serverRunError(true, shutdownErr, serveErr),
		managerErr,
	)
}

// runCore is the shared implementation used by the legacy run() function.
func runCore(
	ctx context.Context,
	options Options,
	handler http.Handler,
	listen listenFunc,
	shutdownTimeout time.Duration,
) error {
	logger := options.Logger
	requestedAddress := options.ListenAddress
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
