package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/lib/pq"
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

func handleGenesis(db *sql.DB, sharedDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx := r.Context()

		var req GenesisRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			logGenesisAttempt(ctx, db, "", "", r.RemoteAddr, "invalid", "invalid json: "+err.Error())
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
			return
		}
		// Every attempt is logged from here on, keyed to whatever keyId
		// the request claims -- this is the audit row that exists no
		// matter what happens next, including a rapid-fire second click
		// that arrives before the first request's response does.
		if req.KeyID == "" || req.Material == "" {
			logGenesisAttempt(ctx, db, req.KeyID, req.CreatedAt, r.RemoteAddr, "invalid", "keyId and material are required")
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "keyId and material are required"})
			return
		}
		material, err := base64.StdEncoding.DecodeString(req.Material)
		if err != nil {
			logGenesisAttempt(ctx, db, req.KeyID, req.CreatedAt, r.RemoteAddr, "invalid", "material must be base64")
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "material must be base64"})
			return
		}
		if len(material) != 32 {
			logGenesisAttempt(ctx, db, req.KeyID, req.CreatedAt, r.RemoteAddr, "invalid", "material must decode to 32 bytes (AES-256)")
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "material must decode to 32 bytes (AES-256)"})
			return
		}
		createdAt := time.Now().UTC()
		if req.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, req.CreatedAt); err == nil {
				createdAt = t
			}
		}

		// If a primary key already exists, genesis is a no-op (this
		// endpoint is idempotent, not "create another genesis key").
		// This check plus the DB's own one_primary_key unique index
		// below is what makes a re-click safe: at most one of any
		// number of concurrent genesis requests can ever win. The
		// audit row below is what makes every OTHER one visible
		// instead of silently vanishing.
		var existing string
		err = db.QueryRowContext(ctx, `SELECT key_id FROM keys WHERE status = 'primary'`).Scan(&existing)
		if err == nil {
			logGenesisAttempt(ctx, db, req.KeyID, req.CreatedAt, r.RemoteAddr, "already-initialized", "primary key already exists: "+existing)
			writeJSON(w, http.StatusOK, map[string]string{
				"status": "already-initialized", "primaryKeyId": existing,
			})
			return
		}

		ciphertext, nonce, err := sealTestVector(material)
		if err != nil {
			logGenesisAttempt(ctx, db, req.KeyID, req.CreatedAt, r.RemoteAddr, "rejected", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			logGenesisAttempt(ctx, db, req.KeyID, req.CreatedAt, r.RemoteAddr, "rejected", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer tx.Rollback()

		_, err = tx.ExecContext(ctx,
			`INSERT INTO keys (key_id, generation, parent_key_id, created_at, material, status)
			 VALUES ($1, 0, NULL, $2, $3, 'primary')`,
			req.KeyID, createdAt, material,
		)
		if err != nil {
			if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
				// Lost the race to another concurrent genesis request that
				// committed first -- e.g. two clicks that both passed the
				// SELECT above before either one's INSERT ran. This is the
				// gap the frontend's disabled-button alone can't close
				// (it only prevents a second click from the SAME tab,
				// not from a second tab, a slow first response, etc.);
				// the unique index closes it for real, and the audit row
				// below makes the rejected attempt visible instead of a
				// client-side error that just disappears.
				logGenesisAttempt(ctx, db, req.KeyID, req.CreatedAt, r.RemoteAddr, "rejected", "lost race: a primary key already exists (23505)")
				writeJSON(w, http.StatusConflict, map[string]string{"error": "a primary key already exists"})
				return
			}
			logGenesisAttempt(ctx, db, req.KeyID, req.CreatedAt, r.RemoteAddr, "rejected", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO test_vectors (key_id, plaintext, ciphertext, nonce) VALUES ($1, $2, $3, $4)`,
			req.KeyID, testPlaintext, ciphertext, nonce,
		)
		if err != nil {
			logGenesisAttempt(ctx, db, req.KeyID, req.CreatedAt, r.RemoteAddr, "rejected", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		// Start the rotation clock now, so the first rotation happens
		// one full interval after genesis rather than on the next tick.
		_, err = tx.ExecContext(ctx, `UPDATE rotation_state SET last_rotated_at = $1 WHERE id = 1`, createdAt)
		if err != nil {
			logGenesisAttempt(ctx, db, req.KeyID, req.CreatedAt, r.RemoteAddr, "rejected", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if err := tx.Commit(); err != nil {
			logGenesisAttempt(ctx, db, req.KeyID, req.CreatedAt, r.RemoteAddr, "rejected", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		logGenesisAttempt(ctx, db, req.KeyID, req.CreatedAt, r.RemoteAddr, "created", "")

		// Same infra side-effect tryRotate performs after a rotation
		// commits, done here too so the tf-config/tfvars tabs show a
		// one-entry keyset immediately after genesis instead of sitting
		// on "not written yet" for a full rotation interval. Same
		// strictly-after-commit ordering, same full-keyset-not-just-
		// the-new-key read, same writer.
		if keys, err := listKeys(ctx, db); err != nil {
			log.Printf("warning: failed to list keys for terraform vars after genesis: %v", err)
		} else if err := writeTerraformVars(sharedDir, keys); err != nil {
			log.Printf("warning: failed to write terraform vars after genesis: %v", err)
		}

		writeJSON(w, http.StatusCreated, map[string]string{"status": "created", "primaryKeyId": req.KeyID})
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
// and POST /api/rotation/resume both route here (mux path decides
// which). It's deliberately a separate, tiny endpoint rather than a
// field on some bigger "settings" object, so it's obvious in the API
// surface and in access logs exactly when auto-rotation was stopped
// or restarted, and by extension shows up as an unambiguous SSE state
// change (`paused: true/false`) to every connected browser tab.
func handleRotationControl(db *sql.DB, paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Reason string `json:"reason"`
		}
		// Body is optional -- decode errors (including an empty body)
		// are not fatal, just means no reason was given.
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
