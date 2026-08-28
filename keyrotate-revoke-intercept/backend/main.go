package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("warning: invalid %s=%q, using default %d", name, v, def)
		return def
	}
	return n
}

func envFloat(name string, def float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Printf("warning: invalid %s=%q, using default %v", name, v, def)
		return def
	}
	return f
}

func envBool(name string, def bool) bool {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("warning: invalid %s=%q, using default %v", name, v, def)
		return def
	}
	return b
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool := mustConnectDB(ctx)
	defer pool.Close()

	sharedDir := os.Getenv("SHARED_DIR")
	if sharedDir == "" {
		sharedDir = "/shared"
	}
	tfConfigPath := os.Getenv("TF_CONFIG_PATH")
	if tfConfigPath == "" {
		tfConfigPath = "/tf-config/main.tf"
	}

	// N = total keysets generated at genesis. Every one of them
	// rotates in this variant -- there is no static tier.
	keysetCount := envInt("KEYSET_COUNT", 50)
	minSeconds := envInt("ROTATION_MIN_SECONDS", 3)
	maxSeconds := envInt("ROTATION_MAX_SECONDS", 20)

	// How often the revoke interceptor ticks, and the probability it
	// actually fires (picks one random still-active keyset to revoke)
	// on any given tick. Fires roughly once every
	// interval/probability seconds on average.
	revokeInterval := time.Duration(envFloat("REVOKE_INTERCEPT_INTERVAL_SECONDS", 3)) * time.Second
	revokeProbability := envFloat("REVOKE_INTERCEPT_PROBABILITY", 0.35)

	// Initial value for the revoke-mode toggle; can be flipped live
	// afterward via POST /api/revoke-mode (see system_state.revoke_auto_rotate).
	revokeAutoRotateDefault := envBool("REVOKE_AUTO_ROTATE", true)

	hub := newSSEHub()

	loopsCtx, cancelLoops := context.WithCancel(ctx)
	defer cancelLoops()
	go runStatusBroadcastLoop(loopsCtx, pool, hub, sharedDir, tfConfigPath)
	go runRevokeInterceptorLoop(loopsCtx, pool, sharedDir, revokeInterval, revokeProbability)

	// Applied once at startup, before genesis has necessarily even
	// run, so the very first genesis (or the very first status poll)
	// already reflects the configured default -- afterward it's just
	// a live, independent toggle.
	if err := setRevokeAutoRotate(ctx, pool, revokeAutoRotateDefault); err != nil {
		log.Printf("warning: failed to set initial revoke_auto_rotate: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/healthz", withCORS(handleHealthz(pool)))
	mux.HandleFunc("/api/genesis", withCORS(handleGenesis(pool, sharedDir, keysetCount, minSeconds, maxSeconds)))
	mux.HandleFunc("/api/status", withCORS(handleStatus(pool, sharedDir, tfConfigPath)))
	mux.HandleFunc("/api/keys", withCORS(handleKeys(pool)))
	mux.HandleFunc("/api/keysets", withCORS(handleKeysets(pool)))
	mux.HandleFunc("/api/events", withCORS(hub.handle))
	mux.HandleFunc("/api/rotation/pause", withCORS(handleRotationControl(pool, true)))
	mux.HandleFunc("/api/rotation/resume", withCORS(handleRotationControl(pool, false)))
	mux.HandleFunc("/api/rotate-if-due", withCORS(handleRotateIfDue(pool, sharedDir)))
	mux.HandleFunc("/api/revoke", withCORS(handleRevoke(pool, sharedDir)))
	mux.HandleFunc("/api/revoke-mode", withCORS(handleRevokeMode(pool)))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{Addr: ":" + port, Handler: mux}

	go func() {
		log.Printf("keyrotate backend listening on :%s (N=%d keysets, all rotating, %d-%ds jitter; revoke every ~%.1fs @ p=%.2f, auto-rotate=%v)",
			port, keysetCount, minSeconds, maxSeconds, revokeInterval.Seconds(), revokeProbability, revokeAutoRotateDefault)
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
