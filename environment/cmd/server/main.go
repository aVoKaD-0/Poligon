package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"poligon/environment/internal/handler"
	"poligon/environment/internal/service"
	"poligon/environment/internal/store"
)

func main() {
	var port string
	flag.StringVar(&port, "port", "", "Server port (default: from PORT env or 8081)")
	flag.Parse()

	if port == "" {
		port = os.Getenv("PORT")
		if port == "" {
			port = "8081"
		}
	}

	addr := ":" + port

	// Initialize components
	manager := store.NewManager()
	svc := service.NewService(manager)
	h := handler.NewHandler(manager, svc)
	router := handler.NewRouter(h)

	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown channel
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("[Poligon Environment] Service starting on http://localhost%s\n", addr)
		log.Printf("[Poligon Environment] Ready to accept isolated test sessions\n")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[Poligon Environment] Server listen failed: %v\n", err)
		}
	}()

	<-stop
	log.Println("[Poligon Environment] Shutting down gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[Poligon Environment] Server forced shutdown: %v\n", err)
	}

	fmt.Println("[Poligon Environment] Stopped.")
}
