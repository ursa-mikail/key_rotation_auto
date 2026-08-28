import type { GenesisBatchResult, StatusResponse, KeyRecord, KeysetSummary, RevokeResult } from './types';

const API_BASE = import.meta.env.DEV ? 'http://localhost:8080' : '';

/**
 * Triggers batch genesis: N keysets, all rotating, server-side
 * material generation. Idempotent -- calling again after the first
 * real batch just returns { status: 'already-initialized', ... }.
 */
export async function submitGenesisBatch(): Promise<GenesisBatchResult> {
  const res = await fetch(`${API_BASE}/api/genesis`, { method: 'POST' });
  if (!res.ok) {
    const body = await res.text();
    throw new Error(`genesis failed (${res.status}): ${body}`);
  }
  return res.json();
}

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

/**
 * Manually fires the same revoke path the background interceptor uses
 * on its own. Omit keysetId to let the server pick a random
 * still-active keyset itself -- handy for demoing the timer/revoke
 * collision case on demand.
 */
export async function revokeKeyset(keysetId?: string): Promise<RevokeResult> {
  const res = await fetch(`${API_BASE}/api/revoke`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(keysetId ? { keysetId } : {}),
  });
  if (!res.ok) throw new Error(`revoke failed (${res.status})`);
  return res.json();
}

/**
 * Flips the global "what happens when a revoke lands" toggle:
 * true = auto-rotate (emergency-renew and keep cycling), false = halt
 * (permanently stop that keyset). Persisted in system_state, so it
 * survives a reload and applies to the very next revoke processed,
 * whichever caller (interceptor loop or manual /api/revoke) triggers it.
 */
export async function setRevokeMode(autoRotate: boolean): Promise<{ autoRotate: boolean }> {
  const res = await fetch(`${API_BASE}/api/revoke-mode`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ autoRotate }),
  });
  if (!res.ok) throw new Error(`revoke-mode update failed (${res.status})`);
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
    // EventSource auto-reconnects on its own.
  };
  return () => source.close();
}
