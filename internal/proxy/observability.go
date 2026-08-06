package proxy

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type privacySafeProxyLogger struct {
	logger *slog.Logger
}

func (l privacySafeProxyLogger) Printf(string, ...any) {
	l.logger.LogAttrs(context.Background(), slog.LevelWarn, "proxy internal warning",
		slog.String("error_class", "internal"))
}

type proxyObserver struct {
	logger         *slog.Logger
	requestLogging bool
	metrics        *proxyMetrics
	now            func() time.Time
}

type proxyRequestObservation struct {
	start    time.Time
	route    string
	method   string
	host     string
	path     string
	finished bool
}

func newProxyObserver(logger *slog.Logger, requestLogging bool, metrics *proxyMetrics) *proxyObserver {
	if logger == nil {
		logger = slog.Default()
	}
	return &proxyObserver{
		logger:         logger,
		requestLogging: requestLogging,
		metrics:        metrics,
		now:            time.Now,
	}
}

func (o *proxyObserver) requestStarted(req *http.Request, ctx *goproxy.ProxyCtx, upstream http.RoundTripper) {
	if o == nil || req.Method == "PRI" {
		return
	}
	host := req.URL.Hostname()
	if host == "" {
		host = req.Host
		if parsedHost, _, err := net.SplitHostPort(host); err == nil {
			host = parsedHost
		}
	}
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	state := &proxyRequestObservation{
		start:  o.now(),
		route:  proxyRouteForHost(host),
		method: proxyMethod(req.Method),
		host:   host,
		path:   path,
	}
	ctx.UserData = state
	if o.metrics != nil {
		o.metrics.requestStarted(state.route)
	}
	ctx.RoundTripper = goproxy.RoundTripperFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Response, error) {
		resp, err := upstream.RoundTrip(req)
		if err != nil {
			o.requestFinished(ctx, nil, err)
		}
		return resp, err
	})
}

func (o *proxyObserver) requestFinished(ctx *goproxy.ProxyCtx, resp *http.Response, requestErr error) {
	if o == nil {
		return
	}
	state, ok := ctx.UserData.(*proxyRequestObservation)
	if !ok || state.finished {
		return
	}
	state.finished = true
	status := 0
	outcome := "response"
	level := slog.LevelInfo
	if resp != nil {
		status = resp.StatusCode
	}
	if requestErr != nil {
		outcome = "upstream_error"
		level = slog.LevelWarn
		if o.metrics != nil {
			o.metrics.upstreamError(state.route)
		}
	}
	duration := o.now().Sub(state.start)
	if o.metrics != nil {
		o.metrics.requestFinished(state.route, state.method, status, outcome, duration)
	}
	if o.requestLogging {
		attrs := []slog.Attr{
			slog.Int64("session", ctx.Session),
			slog.String("route", state.route),
			slog.String("method", state.method),
			slog.String("host", state.host),
			slog.String("path", state.path),
			slog.Int("status", status),
			slog.String("outcome", outcome),
			slog.Duration("duration", duration),
		}
		if requestErr != nil {
			attrs = append(attrs, slog.String("error_class", "round_trip"))
		}
		o.logger.LogAttrs(context.Background(), level, "proxy request", attrs...)
	}
}

func (o *proxyObserver) connectDecision(ctx *goproxy.ProxyCtx, target, route, action string) {
	if o == nil {
		return
	}
	if o.metrics != nil {
		o.metrics.connectDecision(route, action)
	}
	if o.requestLogging {
		o.logger.LogAttrs(context.Background(), slog.LevelInfo, "proxy connect",
			slog.Int64("session", ctx.Session),
			slog.String("target", target),
			slog.String("route", route),
			slog.String("action", action))
	}
}

func (o *proxyObserver) credentialResult(provider, result string) {
	if o != nil && o.metrics != nil {
		o.metrics.credentialResult(provider, result)
	}
}

type proxyMetrics struct {
	requests        *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	inFlight        *prometheus.GaugeVec
	connects        *prometheus.CounterVec
	upstreamErrors  *prometheus.CounterVec
	credentials     *prometheus.CounterVec
}

func newMetricsHandler(registry *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	return mux
}

func newProxyMetrics() (*proxyMetrics, *prometheus.Registry) {
	registry := prometheus.NewRegistry()
	metrics := &proxyMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "kanedias",
			Subsystem: "proxy",
			Name:      "requests_total",
			Help:      "Completed proxy HTTP requests.",
		}, []string{"route", "method", "status_class", "outcome"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "kanedias",
			Subsystem: "proxy",
			Name:      "request_duration_seconds",
			Help:      "Time from receiving a proxy HTTP request to upstream response headers.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"route", "method"}),
		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "kanedias",
			Subsystem: "proxy",
			Name:      "requests_in_flight",
			Help:      "Proxy HTTP requests currently awaiting response headers.",
		}, []string{"route"}),
		connects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "kanedias",
			Subsystem: "proxy",
			Name:      "connect_total",
			Help:      "Proxy CONNECT decisions.",
		}, []string{"route", "action"}),
		upstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "kanedias",
			Subsystem: "proxy",
			Name:      "upstream_errors_total",
			Help:      "Proxy upstream round-trip errors.",
		}, []string{"route"}),
		credentials: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "kanedias",
			Subsystem: "proxy",
			Name:      "credentials_total",
			Help:      "Proxy credential injection outcomes.",
		}, []string{"provider", "result"}),
	}

	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		metrics.requests,
		metrics.requestDuration,
		metrics.inFlight,
		metrics.connects,
		metrics.upstreamErrors,
		metrics.credentials,
	)
	return metrics, registry
}

func (m *proxyMetrics) requestStarted(route string) {
	m.inFlight.WithLabelValues(route).Inc()
}

func (m *proxyMetrics) requestFinished(route, method string, status int, outcome string, duration time.Duration) {
	method = proxyMethod(method)
	m.inFlight.WithLabelValues(route).Dec()
	m.requests.WithLabelValues(route, method, proxyStatusClass(status), outcome).Inc()
	m.requestDuration.WithLabelValues(route, method).Observe(duration.Seconds())
}

func (m *proxyMetrics) connectDecision(route, action string) {
	m.connects.WithLabelValues(route, action).Inc()
}

func (m *proxyMetrics) upstreamError(route string) {
	m.upstreamErrors.WithLabelValues(route).Inc()
}

func (m *proxyMetrics) credentialResult(provider, result string) {
	m.credentials.WithLabelValues(provider, result).Inc()
}

func proxyRouteForHost(host string) string {
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	switch host {
	case "api.github.com", "uploads.github.com", "github.com":
		return "github"
	case "api.anthropic.com":
		return "anthropic"
	case "api.openai.com":
		return "openai"
	case "chatgpt.com":
		return "openai_codex"
	default:
		return "passthrough"
	}
}

func proxyMethod(method string) string {
	method = strings.ToUpper(method)
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "CONNECT", "TRACE":
		return method
	default:
		return "OTHER"
	}
}

func proxyStatusClass(status int) string {
	if status == 0 {
		return "none"
	}
	if status < 100 || status >= 600 {
		return "other"
	}
	return string(rune('0'+status/100)) + "xx"
}
