package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

func withCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// handleGenesis is the entire "N=50 keysets, M=20 rotating, L=random
// emergency renewals" batch event. It is idempotent: once
// system_state.initialized is true, every subsequent call (any
// re-click of the button) is a no-op that just reports what already
// exists -- there is exactly one real batch-genesis event, ever, per
// stack, same "one genesis, ever" guarantee the base version had for
// its single key.
func handleGenesis(db *sql.DB, sharedDir string, defaultN, defaultM, minSeconds, maxSeconds int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx := r.Context()

		n, m := defaultN, defaultM
		var req GenesisRequest
		// Body is optional -- an empty POST just uses the server's
		// configured N/M defaults. A decode error on a genuinely
		// empty body is not fatal.
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&req)
		}
		if req.KeysetCount != nil {
			n = *req.KeysetCount
		}
		if req.RotatingCount != nil {
			m = *req.RotatingCount
		}

		result, err := runGenesisBatch(ctx, db, sharedDir, n, m, minSeconds, maxSeconds)
		if err != nil {
			logGenesisAttempt(ctx, db, r.RemoteAddr, "rejected", err.Error())
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		if result.Status == "already-initialized" {
			logGenesisAttempt(ctx, db, r.RemoteAddr, "already-initialized",
				"batch genesis already ran for this stack")
		} else {
			logGenesisAttempt(ctx, db, r.RemoteAddr, "created",
				"batch genesis created keysets")
		}

		status := http.StatusCreated
		if result.Status == "already-initialized" {
			status = http.StatusOK
		}
		writeJSON(w, status, result)
	}
}

// handleRotateIfDue is the entire inbound side of "Terraform triggers
// Go" in this variant: an external caller (Terraform's local-exec
// provisioner) POSTs here whenever it believes AT LEAST ONE of the N
// keysets is due. tryRotateAll never trusts that belief on its own --
// it re-derives exactly which rotating keysets are due from
// Postgres's own clock and rotates every one of them in this single
// call.
func handleRotateIfDue(db *sql.DB, sharedDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		results, err := tryRotateAll(r.Context(), db, sharedDir)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		out := make([]map[string]any, 0, len(results))
		rotatedCount := 0
		for _, res := range results {
			entry := map[string]any{
				"keysetId":   res.KeysetID,
				"rotated":    res.Rotated,
				"reason":     res.Skipped,
				"rotationId": res.RotationID,
			}
			if res.Rotated {
				rotatedCount++
				entry["fromKeyId"] = res.FromKeyID
				entry["toKeyId"] = res.ToKeyID
				entry["testPassed"] = res.TestPassed
				entry["testDetail"] = res.TestDetail
				entry["nextExpiresAt"] = res.NextExpiresAt.UTC().Format(time.RFC3339)
			}
			out = append(out, entry)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"rotatedCount": rotatedCount,
			"results":      out,
		})
	}
}

func handleStatus(db *sql.DB, sharedDir, tfConfigPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, err := loadStatus(r.Context(), db, sharedDir, tfConfigPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, status)
	}
}

// handleRotationControl is the kill switch: POST /api/rotation/pause
// and POST /api/rotation/resume both route here. Global -- stops (or
// resumes) auto-rotation for every rotating keyset at once.
func handleRotationControl(db *sql.DB, paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Reason string `json:"reason"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		reason := req.Reason
		if paused && reason == "" {
			reason = "paused via API"
		}
		if err := setPaused(r.Context(), db, paused, reason); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"paused": paused})
	}
}

func handleKeys(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys, err := listKeys(r.Context(), db)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, keys)
	}
}

func handleKeysets(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keysets, err := buildKeysetSummaries(r.Context(), db)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, keysets)
	}
}

func handleHealthz(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db unreachable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
