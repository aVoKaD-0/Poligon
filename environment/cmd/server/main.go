package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "poligon/environment/gen/controlv1"
	"poligon/environment/internal/handler"
	"poligon/environment/internal/integration"
	"poligon/environment/internal/service"
	"poligon/environment/internal/store"
)

func setting(name, fallback string) string {
	if s := os.Getenv(name); s != "" {
		return s
	}
	return fallback
}

func run(ctx context.Context, port string) error {
	manager := store.NewManager()
	h := handler.NewHandler(manager, service.NewService(manager))
	httpHandler := handler.NewRouter(h)
	addr := ":" + port
	var rpc *grpc.Server
	errs := make(chan error, 2)
	switch mode := setting("ENVIRONMENT_MODE", "standalone"); mode {
	case "standalone":
	case "integration":
		runner, platform, token := os.Getenv("RUNNER_SERVICE_TOKEN"), os.Getenv("PLATFORM_SERVICE_TOKEN"), os.Getenv("ENVIRONMENT_CONTROL_TOKEN")
		for _, t := range []string{runner, platform, token} {
			if strings.TrimSpace(t) == "" || strings.TrimSpace(t) != t {
				return errors.New("integration requires nonempty service tokens without surrounding whitespace")
			}
		}
		if runner == platform || platform == token || runner == token {
			return errors.New("integration requires distinct service tokens")
		}
		// The integration profile uses private local transport, as does Platform APP_ENV=local.
		conn, err := grpc.NewClient(setting("ENVIRONMENT_CONTROL_ADDR", "127.0.0.1:9090"), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return err
		}
		defer conn.Close()
		s := integration.New(manager, runner, platform, pb.NewControlServiceClient(conn), token)
		httpHandler = s.HTTP(h)
		rpc = s.GRPC()
		l, err := net.Listen("tcp", setting("ENVIRONMENT_GRPC_ADDR", "127.0.0.1:9091"))
		if err != nil {
			return err
		}
		defer rpc.Stop()
		go func() { errs <- rpc.Serve(l) }()
		log.Printf("Environment gRPC listening on %s", l.Addr())
		addr = setting("ENVIRONMENT_HTTP_ADDR", net.JoinHostPort("127.0.0.1", port))
	default:
		return fmt.Errorf("unknown ENVIRONMENT_MODE %q", mode)
	}
	srv := &http.Server{Addr: addr, Handler: httpHandler, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("Environment HTTP listening on %s", ln.Addr())
	go func() { errs <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err = <-errs:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if stopErr := srv.Shutdown(shutdown); stopErr != nil {
		return stopErr
	}
	if rpc != nil {
		done := make(chan struct{})
		go func() { rpc.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-shutdown.Done():
			rpc.Stop()
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func main() {
	port := flag.String("port", "", "HTTP port (default PORT or 8081)")
	flag.Parse()
	if *port == "" {
		*port = setting("PORT", "8081")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *port); err != nil {
		log.Fatal(err)
	}
}
