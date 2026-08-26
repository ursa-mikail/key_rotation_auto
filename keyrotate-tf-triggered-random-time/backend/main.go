package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool := mustConnectDB(ctx)
	defer pool.Close()

	sharedDir := os.Getenv("SHARED_DIR")
	if sharedDir == "" {
		sharedDir = "/shared"
	}
	// The actual terraform/main.tf source, mounted read-only into this
	// container purely for display -- the backend never reads it for
	// logic, only so the UI can show the .tf config next to the
	// tfvars/output json it produces. See docker-compose.yml.
	tfConfigPath := os.Getenv("TF_CONFIG_PATH")
	if tfConfigPath == "" {
		tfConfigPath = "/tf-config/main.tf"
	}

	hub := newSSEHub()

	rotationCtx, cancelRotation := context.WithCancel(ctx)
	defer cancelRotation()
	// This variant's broadcast loop only reads and publishes state; it
	// never decides to rotate. Rotation only happens via
	// POST /api/rotate-if-due, called from outside Go entirely.
	go runStatusBroadcastLoop(rotationCtx, pool, hub, sharedDir, tfConfigPath)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/healthz", withCORS(handleHealthz(pool)))
	mux.HandleFunc("/api/genesis", withCORS(handleGenesis(pool, sharedDir)))
	mux.HandleFunc("/api/status", withCORS(handleStatus(pool, sharedDir, tfConfigPath)))
	mux.HandleFunc("/api/keys", withCORS(handleKeys(pool)))
	mux.HandleFunc("/api/events", withCORS(hub.handle))
	mux.HandleFunc("/api/rotation/pause", withCORS(handleRotationControl(pool, true)))
	mux.HandleFunc("/api/rotation/resume", withCORS(handleRotationControl(pool, false)))
	mux.HandleFunc("/api/rotate-if-due", withCORS(handleRotateIfDue(pool, sharedDir)))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{Addr: ":" + port, Handler: mux}

	go func() {
		log.Printf("keyrotate backend listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}
