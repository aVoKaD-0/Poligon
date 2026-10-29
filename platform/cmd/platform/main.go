package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/poligon/platform/internal/platform"
	"github.com/poligon/platform/migrations"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/oauth2"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if e := run(); e != nil {
		slog.Error("platform stopped", "error", e.Error())
		os.Exit(1)
	}
}
func run() error {
	cfg, e := platform.LoadConfig()
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownTelemetry, e := platform.SetupTelemetry(ctx)
	if e != nil {
		return e
	}
	defer func() {
		work, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTelemetry(work)
	}()
	pool, e := pgxpool.New(ctx, cfg.DatabaseURL)
	if e != nil {
		return e
	}
	defer pool.Close()
	if e = pool.Ping(ctx); e != nil {
		return errors.New("database unavailable")
	}
	app := &platform.App{Config: cfg, DB: &platform.DB{Pool: pool}}
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		return migrations.Apply(ctx, pool)
	}
	if len(os.Args) > 1 && os.Args[1] == "seed" {
		return app.SeedCatalog(ctx)
	}
	var version int
	if e = pool.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); e != nil || version != 1 {
		return errors.New("run platform migrate before serve")
	}
	if len(os.Args) == 4 && os.Args[1] == "quota" {
		return app.SetProjectQuota(ctx, os.Args[2], os.Args[3])
	}
	app.Store, e = platform.NewArtifactStore(cfg)
	if e != nil {
		return e
	}
	if len(os.Args) > 1 && os.Args[1] == "verify-artifacts" {
		return app.VerifyStorage(ctx)
	}
	if len(os.Args) == 3 && os.Args[1] == "purge-expired" && os.Args[2] == "--apply" {
		return app.PurgeExpired(ctx)
	}
	if len(os.Args) > 1 && os.Args[1] != "serve" {
		return errors.New("unknown command; use migrate, seed, quota, verify-artifacts or purge-expired --apply")
	}
	// Keep the unagreed validator wire proposal opt-in until provider IDL arrives.
	if mode := os.Getenv("VALIDATOR_WIRE_MODE"); mode != "" {
		if mode != "struct-proposal" {
			return errors.New("unsupported VALIDATOR_WIRE_MODE")
		}
		env, e := platform.ValidatorConnection(cfg, os.Getenv("ENVIRONMENT_GRPC_ADDR"))
		if e != nil {
			return e
		}
		defer env.Close()
		intel, e := platform.ValidatorConnection(cfg, os.Getenv("INTELLIGENCE_GRPC_ADDR"))
		if e != nil {
			return e
		}
		defer intel.Close()
		app.Validators = &platform.RemoteValidators{Environment: env, Intelligence: intel, Token: os.Getenv("PLATFORM_SERVICE_TOKEN")}
	}
	if cfg.OIDCIssuer != "" {
		work, cancel := context.WithTimeout(ctx, 10*time.Second)
		app.OIDC, e = oidc.NewProvider(work, cfg.OIDCIssuer)
		cancel()
		if e != nil {
			return errors.New("OIDC discovery failed")
		}
		app.OAuth = oauth2.Config{ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret, Endpoint: app.OIDC.Endpoint(), RedirectURL: cfg.Origin + "/api/v1/auth/callback", Scopes: []string{oidc.ScopeOpenID, "profile"}}
	}
	otel.SetTextMapPropagator(propagation.TraceContext{})
	grpcServer, e := app.GRPCServer()
	if e != nil {
		return e
	}
	listener, e := net.Listen("tcp", cfg.GRPCAddr)
	if e != nil {
		return e
	}
	defer listener.Close()
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	errorsCh := make(chan error, 3)
	go func() {
		if e := grpcServer.Serve(listener); e != nil {
			errorsCh <- e
		}
	}()
	go func() {
		if e := server.ListenAndServe(); e != nil && !errors.Is(e, http.ErrServerClosed) {
			errorsCh <- e
		}
	}()
	go broker(ctx, app)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				work, c := context.WithTimeout(ctx, 4*time.Second)
				e := app.Supervise(work)
				c()
				if e != nil {
					slog.Error("supervisor failed", "error_type", "database")
				}
			}
		}
	}()
	slog.Info("platform listening", "http", cfg.HTTPAddr, "grpc", cfg.GRPCAddr, "environment", cfg.Env)
	select {
	case <-ctx.Done():
	case e := <-errorsCh:
		stop()
		return e
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	done := make(chan struct{})
	go func() { grpcServer.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-shutdown.Done():
		grpcServer.Stop()
	}
	return nil
}
func broker(ctx context.Context, a *platform.App) {
	for ctx.Err() == nil {
		func() {
			opts := []nats.Option{nats.Timeout(2 * time.Second), nats.MaxReconnects(-1), nats.ReconnectWait(time.Second)}
			if a.Config.NATSUser != "" {
				opts = append(opts, nats.UserInfo(a.Config.NATSUser, a.Config.NATSPassword))
			}
			nc, e := nats.Connect(a.Config.NATSURL, opts...)
			if e != nil {
				return
			}
			defer nc.Close()
			defer a.SetBrokerConnected(false)
			js, e := nc.JetStream(nats.MaxWait(3 * time.Second))
			if e != nil {
				return
			}
			if e = platform.SetupStreams(js); e != nil {
				return
			}
			a.SetBrokerConnected(true)
			consumer := make(chan error, 1)
			go func() { consumer <- a.Consume(ctx, js) }()
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-consumer:
					return
				case <-tick.C:
					a.SetBrokerConnected(nc.IsConnected())
					work, c := context.WithTimeout(ctx, 5*time.Second)
					e = a.PublishOutbox(work, js)
					c()
					if e != nil {
						slog.Warn("outbox publication delayed")
					}
				}
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}
