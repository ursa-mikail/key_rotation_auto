import type { GenesisBatchResult, StatusResponse, KeyRecord, KeysetSummary } from './types';

// In dev (vite) the backend runs on :8080; in the docker-compose stack
// nginx proxies /api to the backend container, so relative paths work
// in both places without a build-time env var.
const API_BASE = import.meta.env.DEV ? 'http://localhost:8080' : '';

/**
 * Triggers batch genesis: the backend generates N keysets server-side
 * (crypto/rand, never leaves Go), randomly selects M of them to
 * rotate, and randomly flags L (0..M) of those for immediate/emergency
 * renewal. Idempotent -- calling this again after the first real
 * batch just returns { status: 'already-initialized', ... }.
 */
export async function submitGenesisBatch(): Promise<GenesisBatchResult> {
  const res = await fetch(`${API_BASE}/api/genesis`, { method: 'POST' });
  if (!res.ok) {
    const body = await res.text();
    throw new Error(`genesis failed (${res.status}): ${body}`);
  }
  return res.json();
}

/**
 * Engages the rotation kill switch. Persisted server-side in
 * system_state (paused/paused_at/paused_reason), so it survives a
 * page reload and applies to every connected tab, not just this one.
 * Global: stops every rotating keyset at once.
 */
export async function pauseRotation(reason?: string): Promise<{ paused: boolean }> {
  const res = await fetch(`${API_BASE}/api/rotation/pause`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ reason: reason ?? '' }),
  });
  if (!res.ok) throw new Error(`pause failed (${res.status})`);
  return res.json();
}

export async function resumeRotation(): Promise<{ paused: boolean }> {
  const res = await fetch(`${API_BASE}/api/rotation/resume`, { method: 'POST' });
  if (!res.ok) throw new Error(`resume failed (${res.status})`);
  return res.json();
}

export async function fetchStatus(): Promise<StatusResponse> {
  const res = await fetch(`${API_BASE}/api/status`);
  if (!res.ok) throw new Error(`status fetch failed (${res.status})`);
  return res.json();
}

export async function fetchKeys(): Promise<KeyRecord[]> {
  const res = await fetch(`${API_BASE}/api/keys`);
  if (!res.ok) throw new Error(`keys fetch failed (${res.status})`);
  return res.json();
}

export async function fetchKeysets(): Promise<KeysetSummary[]> {
  const res = await fetch(`${API_BASE}/api/keysets`);
  if (!res.ok) throw new Error(`keysets fetch failed (${res.status})`);
  return res.json();
}

/**
 * Opens the SSE stream for realtime status updates (roughly once per
 * second, driven by the backend's rotation ticker). Returns a function
 * to close the connection.
 */
export function subscribeToStatus(onUpdate: (status: StatusResponse) => void): () => void {
  const source = new EventSource(`${API_BASE}/api/events`);
  source.onmessage = (event) => {
    try {
      onUpdate(JSON.parse(event.data) as StatusResponse);
    } catch (err) {
      console.error('failed to parse SSE payload', err);
    }
  };
  source.onerror = () => {
    // EventSource auto-reconnects on its own; nothing to do here
    // besides let it retry. Polling fallback in main.ts covers the
    // gap while it does.
  };
  return () => source.close();
}
