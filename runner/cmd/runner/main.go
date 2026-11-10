package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/poligon/runner/contracts"
	pb "github.com/poligon/runner/contracts/controlv1"
	"github.com/poligon/runner/internal/config"
	"github.com/poligon/runner/internal/execution"
	"github.com/poligon/runner/internal/platform"
	"github.com/poligon/runner/internal/transport"
	"github.com/poligon/runner/internal/worker"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdk "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/connectivity"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, log); err != nil {
		log.Error("runner stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	schemas, err := contracts.Load()
	if err != nil {
		return errors.New("load Platform schemas failed")
	}
	eURL, err := transport.Endpoint(cfg.Environment, cfg.Local)
	if err != nil || eURL.RawQuery != "" || eURL.ForceQuery {
		return errors.New("invalid RUNNER_ENVIRONMENT_URL")
	}
	if err = os.MkdirAll(cfg.TempDir, 0700); err != nil {
		return errors.New("cannot access Runner temporary directory")
	}
	conn, err := platform.Dial(cfg.Control, cfg.Local, platform.TLSFiles{
		CA: cfg.CA, Certificate: cfg.Cert, Key: cfg.Key, ServerName: cfg.ServerName,
	}, cfg.PlanLimit)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.Connect()

	brokerURL, err := url.Parse(cfg.NATSURL)
	if err != nil || brokerURL.Hostname() == "" || brokerURL.User != nil || brokerURL.Path != "" ||
		brokerURL.RawQuery != "" || brokerURL.Fragment != "" ||
		(brokerURL.Scheme != "tls" && !(cfg.Local && brokerURL.Scheme == "nats")) {
		return errors.New("invalid RUNNER_NATS_URL")
	}
	opts := []nats.Option{
		nats.Name("poligon-runner"), nats.Timeout(2 * time.Second), nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1), nats.ReconnectWait(2 * time.Second), nats.ReconnectBufSize(-1),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, _ error) { log.Warn("broker asynchronous error") }),
	}
	if cfg.NATSUser != "" {
		opts = append(opts, nats.UserInfo(cfg.NATSUser, cfg.NATSPassword))
	}
	if brokerURL.Scheme == "tls" {
		opts = append(opts, nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12}))
	}
	nc, err := nats.Connect(cfg.NATSURL, opts...)
	if err != nil {
		return errors.New("NATS configuration or connection failed")
	}
	defer nc.Close()
	js, err := nc.JetStream(nats.MaxWait(3 * time.Second))
	if err != nil {
		return errors.New("JetStream setup failed")
	}
	broker := worker.Broker{JS: js}
	control := &platform.Control{RPC: pb.NewControlServiceClient(conn), Token: cfg.ControlToken, Timeout: cfg.RPC}
	internalHTTP := transport.HTTPClient(cfg.Connections, cfg.Local, false)
	defer internalHTTP.CloseIdleConnections()
	agentHTTP := transport.HTTPClient(cfg.Connections, cfg.Local, true)
	defer agentHTTP.CloseIdleConnections()
	metrics := new(execution.Metrics)
	engine := &execution.Engine{
		Control: control, Publisher: broker,
		Evidence:    &platform.Artifacts{Control: control, Client: internalHTTP, Hosts: cfg.UploadHosts, Local: cfg.Local, PutTimeout: cfg.Upload},
		Agents:      &transport.Agents{Client: agentHTTP, Hosts: cfg.AgentHosts, Local: cfg.Local, EnableHTTP: cfg.ProposedHTTP, Timeout: cfg.Request, Limit: cfg.MessageLimit},
		Environment: &transport.Environment{Client: internalHTTP, BaseURL: strings.TrimRight(eURL.String(), "/"), Timeout: cfg.Tool, Limit: min(cfg.MessageLimit, 24<<10)},
		Schemas:     schemas, Metrics: metrics, Log: log, TempDir: cfg.TempDir, EvidenceLimit: cfg.EvidenceLimit, PlanLimit: cfg.PlanLimit,
		HeartbeatInterval: 10 * time.Second, CloseTimeout: 25 * time.Second,
	}
	// No provider-owned Intelligence simulator contract is published by the pinned refs.
	// Keep production consumers idle until an actual provider can be wired here.
	accept := cfg.Local || engine.Simulators != nil
	if engine.Simulators == nil {
		log.Warn("Intelligence simulator unavailable", "accepting_jobs", accept)
	}
	manager := &worker.Manager{Broker: broker, Durable: cfg.Durable, Slots: cfg.Slots, Pending: cfg.Pending,
		AckWait: 45 * time.Second, Progress: 10 * time.Second, Drain: cfg.Drain, Log: log, Execute: engine.Run}
	provider, err := tracing(ctx)
	if err != nil {
		return errors.New("OTLP exporter setup failed")
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = provider.Shutdown(flush)
	}()

	diagnostics, err := diagnosticServer(cfg, metrics, func() (infra, executionReady bool) {
		infra = ctx.Err() == nil && nc.IsConnected() && conn.GetState() == connectivity.Ready &&
			(manager.Ready() || !accept)
		return infra, infra && engine.Simulators != nil
	})
	if err != nil {
		return err
	}
	listen, err := net.Listen("tcp", diagnostics.Addr)
	if err != nil {
		return errors.New("diagnostics listener failed")
	}
	defer listen.Close()
	work, stop := context.WithCancel(ctx)
	defer stop()
	serving := make(chan error, 1)
	go func() { serving <- diagnostics.Serve(listen) }()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		if accept {
			manager.Run(work)
		}
	}()
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serving:
		if !errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = errors.New("diagnostics server failed")
		}
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = diagnostics.Shutdown(shutdown)
	<-drained
	return serveErr
}

func tracing(ctx context.Context) (*sdk.TracerProvider, error) {
	options := []sdk.TracerProviderOption{
		sdk.WithResource(resource.NewSchemaless(attribute.String("service.name", "poligon-runner"))),
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		options = append(options, sdk.WithBatcher(exporter))
	}
	p := sdk.NewTracerProvider(options...)
	otel.SetTracerProvider(p)
	return p, nil
}

func diagnosticServer(cfg config.Config, metrics *execution.Metrics, ready func() (bool, bool)) (*http.Server, error) {
	host, _, err := net.SplitHostPort(cfg.Listen)
	ip := net.ParseIP(host)
	if err != nil || (cfg.Profiles && (ip == nil || !ip.IsLoopback())) {
		return nil, errors.New("invalid diagnostics address or pprof listener is not loopback")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		infra, executionReady := ready()
		if !executionReady {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		if executionReady {
			_, _ = io.WriteString(w, "ready\n")
		} else if infra {
			_, _ = io.WriteString(w, "Intelligence simulator unavailable\n")
		} else {
			_, _ = io.WriteString(w, "dependencies unavailable\n")
		}
	})
	want := sha256.Sum256([]byte("Bearer " + cfg.MetricsToken))
	protect := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
			if cfg.MetricsToken != "" && subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	if cfg.MetricsToken != "" || ip != nil && ip.IsLoopback() {
		mux.HandleFunc("GET /metrics", protect(func(w http.ResponseWriter, _ *http.Request) { metrics.Write(w) }))
	}
	if cfg.Profiles {
		runtime.SetBlockProfileRate(int(time.Millisecond))
		runtime.SetMutexProfileFraction(10)
		mux.HandleFunc("GET /debug/pprof/", protect(pprof.Index))
		mux.HandleFunc("GET /debug/pprof/profile", protect(pprof.Profile))
		mux.HandleFunc("GET /debug/pprof/trace", protect(pprof.Trace))
	}
	return &http.Server{Addr: cfg.Listen, Handler: mux, ReadTimeout: 3 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}, nil
}
