package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/lib/pq"
)

func mustConnectDB(ctx context.Context) *sql.DB {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://keyrotate:keyrotate@postgres:5432/keyrotate?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)

	// Postgres may still be initializing when this container starts,
	// so retry connect instead of failing fast.
	for attempt := 1; attempt <= 30; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
		if err == nil {
			log.Printf("connected to database (attempt %d)", attempt)
			return db
		}
		log.Printf("database not ready yet (attempt %d/30): %v", attempt, err)
		time.Sleep(2 * time.Second)
	}

	log.Fatalf("could not connect to database: %v", err)
	return nil // unreachable
}

func fmtErr(prefix string, err error) error {
	return fmt.Errorf("%s: %w", prefix, err)
}
