package platform

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func SetupTelemetry(ctx context.Context) (func(context.Context) error, error) {
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(resource.NewWithAttributes("", attribute.String("service.name", "poligon-platform"))), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1)))}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		exporter, e := otlptracehttp.New(ctx)
		if e != nil {
			return nil, e
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	provider := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

type metrics struct {
	requests, errors, latencyMicros, sse atomic.Int64
	broker                               atomic.Bool
}
type observedWriter struct {
	http.ResponseWriter
	status int
}

func (w *observedWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *observedWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedWriter) Flush()                      { _ = w.FlushError() }
func (w *observedWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}
func (a *App) observe(w http.ResponseWriter, r *http.Request, fn func(http.ResponseWriter, *http.Request)) {
	start := time.Now()
	out := &observedWriter{ResponseWriter: w}
	defer func() {
		elapsed := time.Since(start)
		m, _ := r.Context().Value(ctxKey("http_trace")).(*httpTrace)
		if m == nil {
			m = &httpTrace{route: r.Pattern}
		}
		a.metrics.requests.Add(1)
		a.metrics.latencyMicros.Add(elapsed.Microseconds())
		if out.status >= 400 {
			a.metrics.errors.Add(1)
		}
		slog.Info("http request", "request_id", r.Context().Value(ctxKey("request_id")), "trace_id", trace.SpanContextFromContext(r.Context()).TraceID().String(), "method", r.Method, "route", m.route, "status", out.status, "duration_ms", elapsed.Milliseconds(), "project_id", m.project, "run_id", m.run, "session_id", m.session)
	}()
	fn(out, r)
}
func (a *App) metricsHandler(w http.ResponseWriter, r *http.Request) {
	token := os.Getenv("METRICS_TOKEN")
	if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
		w.WriteHeader(403)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "platform_http_requests_total %d\nplatform_http_errors_total %d\nplatform_http_duration_seconds_sum %g\nplatform_sse_connections %d\n", a.metrics.requests.Load(), a.metrics.errors.Load(), float64(a.metrics.latencyMicros.Load())/1e6, a.metrics.sse.Load())
	for name, sql := range map[string]string{"outbox_pending": "SELECT count(*) FROM outbox WHERE published_at IS NULL", "outbox_lag_seconds": "SELECT coalesce(extract(epoch FROM now()-min(created_at)),0)::bigint FROM outbox WHERE published_at IS NULL", "sessions_queued": "SELECT count(*) FROM sessions WHERE status='queued'", "sessions_running": "SELECT count(*) FROM sessions WHERE status='running'", "execution_retries": "SELECT count(*) FROM execution_attempts WHERE attempt_no>1", "expired_leases": "SELECT count(*) FROM execution_attempts WHERE status='expired'", "stale_results_rejected": "SELECT count(*) FROM stale_results", "queue_oldest_seconds": "SELECT coalesce(extract(epoch FROM now()-min(a.created_at)),0)::bigint FROM execution_attempts a JOIN sessions s ON s.id=a.session_id WHERE a.status='queued' AND a.attempt_no=s.current_attempt_no",
		"quarantine": "SELECT count(*) FROM quarantine"} {
		var n int64
		if e := a.DB.Pool.QueryRow(r.Context(), sql).Scan(&n); e == nil {
			fmt.Fprintf(w, "platform_%s %d\n", name, n)
		}
	}
}

func (a *App) SetBrokerConnected(connected bool) { a.metrics.broker.Store(connected) }
