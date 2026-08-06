package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestValidateListenAddressAcceptsLocalOnlyAddresses(t *testing.T) {
	cases := []string{
		"127.0.0.1:8080",
		"127.0.0.1:0",
		"LOCALHOST:8080",
		"[::1]:8080",
		"[0:0:0:0:0:0:0:1]:0",
	}

	for _, address := range cases {
		t.Run(address, func(t *testing.T) {
			if err := ValidateListenAddress(address); err != nil {
				t.Fatalf("ValidateListenAddress(%q) returned error: %v", address, err)
			}
		})
	}
}

func TestValidateListenAddressRejectsNonLocalAddresses(t *testing.T) {
	cases := []string{
		"",
		":8080",
		"0.0.0.0:8080",
		"[::]:8080",
		"192.168.1.2:8080",
		"8.8.8.8:8080",
		"example.com:8080",
		"127.0.0.1",
		"127.0.0.1:http",
		"127.0.0.1:-1",
		"127.0.0.1:65536",
	}

	for _, address := range cases {
		t.Run(address, func(t *testing.T) {
			err := ValidateListenAddress(address)
			if err == nil {
				t.Fatalf("ValidateListenAddress(%q) returned nil error", address)
			}
			if !strings.Contains(err.Error(), address) {
				t.Fatalf("ValidateListenAddress(%q) error %q does not contain address", address, err)
			}
		})
	}
}

func TestRunRejectsNilContext(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	listenCalled := false
	listen := func(string, string) (net.Listener, error) {
		listenCalled = true
		return nil, errors.New("unexpected listen")
	}

	err := run(nil, Options{ListenAddress: "127.0.0.1:0", Logger: logger}, http.NotFoundHandler(), listen, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("run(nil, ...) error = %v, want context error", err)
	}
	if listenCalled {
		t.Fatal("run(nil, ...) called listen")
	}

	err = Run(nil, Options{ListenAddress: "127.0.0.1:0", Logger: logger})
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("Run(nil, ...) error = %v, want context error", err)
	}
}

func TestRunRejectsNilLogger(t *testing.T) {
	listenCalled := false
	listen := func(string, string) (net.Listener, error) {
		listenCalled = true
		return nil, errors.New("unexpected listen")
	}

	err := run(context.Background(), Options{ListenAddress: "127.0.0.1:0"}, http.NotFoundHandler(), listen, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "logger") {
		t.Fatalf("run with nil logger error = %v, want logger error", err)
	}
	if listenCalled {
		t.Fatal("run with nil logger called listen")
	}
}

func TestPrepareOptionsDefaultsEmptyListenAddress(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	got, err := prepareOptions(Options{Logger: logger})
	if err != nil {
		t.Fatalf("prepareOptions with empty address returned error: %v", err)
	}
	if got.ListenAddress != DefaultListenAddress {
		t.Fatalf("prepareOptions address = %q, want %q", got.ListenAddress, DefaultListenAddress)
	}
	if got.Logger != logger {
		t.Fatal("prepareOptions did not preserve logger")
	}

	const address = "[::1]:9000"
	got, err = prepareOptions(Options{ListenAddress: address, Logger: logger})
	if err != nil {
		t.Fatalf("prepareOptions with explicit address returned error: %v", err)
	}
	if got.ListenAddress != address {
		t.Fatalf("prepareOptions address = %q, want %q", got.ListenAddress, address)
	}
	if got.Logger != logger {
		t.Fatal("prepareOptions did not preserve logger")
	}
}

func TestRunRejectsUnsafeListenAddressBeforeListening(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	listenCalled := false
	listen := func(string, string) (net.Listener, error) {
		listenCalled = true
		return nil, errors.New("unexpected listen")
	}

	err := run(context.Background(), Options{ListenAddress: "0.0.0.0:8080", Logger: logger}, http.NotFoundHandler(), listen, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "0.0.0.0:8080") {
		t.Fatalf("run with unsafe address error = %v, want address validation error", err)
	}
	if listenCalled {
		t.Fatal("run with unsafe address called listen")
	}
}

func TestRunReturnsListenError(t *testing.T) {
	listenErr := errors.New("listen failed")
	logger, logs := testLogger()
	listen := func(network, address string) (net.Listener, error) {
		if network != "tcp" {
			t.Fatalf("listen network = %q, want tcp", network)
		}
		if address != "127.0.0.1:0" {
			t.Fatalf("listen address = %q, want 127.0.0.1:0", address)
		}
		return nil, listenErr
	}

	err := run(context.Background(), Options{ListenAddress: "127.0.0.1:0", Logger: logger}, http.NotFoundHandler(), listen, time.Millisecond)
	if !errors.Is(err, listenErr) {
		t.Fatalf("run error = %v, want errors.Is(listenErr)", err)
	}
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("run error = %v, want listen operation context", err)
	}
	requireStructuredLog(t, logs, `"msg":"server listen failed"`, `"address":"127.0.0.1:0"`, `"error":"listen failed"`)
}

func TestRunNormalizesLocalhostBeforeListeningAndLogsRequestedAddress(t *testing.T) {
	listenErr := errors.New("listen failed")
	logger, logs := testLogger()
	listen := func(network, address string) (net.Listener, error) {
		if network != "tcp" {
			t.Fatalf("listen network = %q, want tcp", network)
		}
		if address != "127.0.0.1:4321" {
			t.Fatalf("listen address = %q, want literal IPv4 loopback", address)
		}
		return nil, listenErr
	}

	err := run(
		context.Background(),
		Options{ListenAddress: "LOCALHOST:4321", Logger: logger},
		http.NotFoundHandler(),
		listen,
		time.Millisecond,
	)
	if !errors.Is(err, listenErr) {
		t.Fatalf("run error = %v, want errors.Is(listenErr)", err)
	}
	requireStructuredLog(t, logs, `"msg":"server starting"`, `"requested_address":"LOCALHOST:4321"`)
	requireStructuredLog(t, logs, `"msg":"server listen failed"`, `"address":"LOCALHOST:4321"`)
}

func TestRunServesAndGracefullyStopsOnContextCancellation(t *testing.T) {
	logger, logs := testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addresses := make(chan string, 1)
	listen := func(network, address string) (net.Listener, error) {
		listener, err := net.Listen(network, address)
		if err == nil {
			addresses <- listener.Addr().String()
		}
		return listener, err
	}
	requested := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	})
	result := make(chan error, 1)
	go func() {
		result <- run(ctx, Options{ListenAddress: "127.0.0.1:0", Logger: logger}, handler, listen, 250*time.Millisecond)
	}()

	var effectiveAddress string
	select {
	case effectiveAddress = <-addresses:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for listener address")
	}

	response, err := (&http.Client{Timeout: time.Second}).Get("http://" + effectiveAddress)
	if err != nil {
		t.Fatalf("GET served endpoint: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("GET status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request handler")
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("run after cancellation returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run did not stop after cancellation")
	}

	requireStructuredLog(t, logs, `"msg":"server starting"`, `"requested_address":"127.0.0.1:0"`)
	requireStructuredLog(t, logs, `"msg":"server listening"`, `"requested_address":"127.0.0.1:0"`, `"effective_address":"`+effectiveAddress+`"`)
	requireStructuredLog(t, logs, `"msg":"server shutdown started"`, `"effective_address":"`+effectiveAddress+`"`)
	requireStructuredLog(t, logs, `"msg":"server shutdown complete"`)
}

func TestRunReturnsUnexpectedServeError(t *testing.T) {
	serveErr := errors.New("serve failed")
	logger, logs := testLogger()
	listener := &acceptErrorListener{err: serveErr}

	err := run(
		context.Background(),
		Options{ListenAddress: "127.0.0.1:0", Logger: logger},
		http.NotFoundHandler(),
		func(string, string) (net.Listener, error) { return listener, nil },
		time.Millisecond,
	)
	if !errors.Is(err, serveErr) {
		t.Fatalf("run error = %v, want errors.Is(serveErr)", err)
	}
	if err == nil || !strings.Contains(err.Error(), "serve") {
		t.Fatalf("run error = %v, want serve operation context", err)
	}
	requireStructuredLog(t, logs, `"msg":"server serve failed"`, `"error":"serve failed"`)
}

func TestCoordinateServerSimultaneousCancellationJoinsUnexpectedServeResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	serveErr := errors.New("serve failed during cancellation")
	serveResult := make(chan error, 1)
	serveResult <- serveErr

	shutdownCalled := false
	shutdown := func(context.Context) error {
		shutdownCalled = true
		return nil
	}
	forceClose := func() error {
		t.Fatal("force close called after successful shutdown")
		return nil
	}

	shutdownStarted, shutdownErr, closeErr, gotServeErr := coordinateServer(
		ctx,
		serveResult,
		time.Second,
		shutdown,
		forceClose,
	)
	if !shutdownStarted || !shutdownCalled {
		t.Fatal("coordinateServer did not prioritize the ready cancellation shutdown path")
	}
	if shutdownErr != nil {
		t.Fatalf("shutdown error = %v, want nil", shutdownErr)
	}
	if closeErr != nil {
		t.Fatalf("close error = %v, want nil", closeErr)
	}
	if !errors.Is(gotServeErr, serveErr) {
		t.Fatalf("serve error = %v, want errors.Is(serveErr)", gotServeErr)
	}
	if len(serveResult) != 0 {
		t.Fatal("Serve result was not drained")
	}
}

func TestServerRunErrorClassifiesCancellationResults(t *testing.T) {
	shutdownErr := errors.New("shutdown failed")
	serveErr := errors.New("serve failed during cancellation")

	joinedErr := serverRunError(true, shutdownErr, serveErr)
	if !errors.Is(joinedErr, shutdownErr) {
		t.Fatalf("serverRunError = %v, want errors.Is(shutdownErr)", joinedErr)
	}
	if !errors.Is(joinedErr, serveErr) {
		t.Fatalf("serverRunError = %v, want errors.Is(serveErr)", joinedErr)
	}

	if err := serverRunError(true, nil, http.ErrServerClosed); err != nil {
		t.Fatalf("shutdown-caused ErrServerClosed returned error: %v", err)
	}
	if err := serverRunError(false, nil, http.ErrServerClosed); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("non-shutdown ErrServerClosed error = %v, want errors.Is(http.ErrServerClosed)", err)
	}
}

func TestRunReturnsShutdownError(t *testing.T) {
	logger, logs := testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addresses := make(chan string, 1)
	listen := func(network, address string) (net.Listener, error) {
		listener, err := net.Listen(network, address)
		if err == nil {
			addresses <- listener.Addr().String()
		}
		return listener, err
	}
	entered := make(chan struct{})
	exited := make(chan struct{})
	// Use the request's own context to prove forced Close cancels active work.
	handler := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(entered)
		<-request.Context().Done()
		close(exited)
	})

	result := make(chan error, 1)
	go func() {
		result <- run(ctx, Options{ListenAddress: "127.0.0.1:0", Logger: logger}, handler, listen, 25*time.Millisecond)
	}()

	var effectiveAddress string
	select {
	case effectiveAddress = <-addresses:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for listener address")
	}

	requestResult := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: time.Second}).Get("http://" + effectiveAddress)
		if response != nil {
			response.Body.Close()
		}
		requestResult <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active request")
	}

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("run shutdown error = %v, want context deadline exceeded", err)
		}
		if !strings.Contains(err.Error(), "shutdown") {
			t.Fatalf("run error = %v, want shutdown operation context", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run did not return after shutdown timeout")
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("active handler context was not canceled by forced Close")
	}
	select {
	case <-requestResult:
	case <-time.After(time.Second):
		t.Fatal("client request did not stop after forced Close")
	}

	requireStructuredLog(t, logs, `"msg":"server shutdown failed"`, `"error":"context deadline exceeded"`)
}

func TestServerTimeoutConfiguration(t *testing.T) {
	handler := http.NewServeMux()
	server := newHTTPServer("127.0.0.1:8080", handler)

	if server.Addr != "127.0.0.1:8080" {
		t.Fatalf("server Addr = %q, want 127.0.0.1:8080", server.Addr)
	}
	if server.Handler != handler {
		t.Fatal("server did not preserve handler")
	}
	if server.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want 5s", server.ReadHeaderTimeout)
	}
	if server.IdleTimeout != 60*time.Second {
		t.Fatalf("IdleTimeout = %v, want 60s", server.IdleTimeout)
	}
	if server.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want zero", server.WriteTimeout)
	}
	if defaultShutdownTimeout != 10*time.Second {
		t.Fatalf("defaultShutdownTimeout = %v, want 10s", defaultShutdownTimeout)
	}
}

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var logs bytes.Buffer
	return slog.New(slog.NewJSONHandler(&logs, nil)), &logs
}

func requireStructuredLog(t *testing.T, logs *bytes.Buffer, parts ...string) {
	t.Helper()
	got := logs.String()
	for _, part := range parts {
		if !strings.Contains(got, part) {
			t.Fatalf("structured logs %q do not contain %q", got, part)
		}
	}
}

type acceptErrorListener struct {
	err error
}

func (listener *acceptErrorListener) Accept() (net.Conn, error) {
	return nil, listener.err
}

func (*acceptErrorListener) Close() error {
	return nil
}

func (*acceptErrorListener) Addr() net.Addr {
	return staticAddr("127.0.0.1:43210")
}

type staticAddr string

func (staticAddr) Network() string {
	return "tcp"
}

func (address staticAddr) String() string {
	return string(address)
}
