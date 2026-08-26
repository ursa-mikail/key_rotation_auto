import type { KeyBirthCert, StatusResponse, KeyRecord } from './types';

// In dev (vite) the backend runs on :8080; in the docker-compose stack
// nginx proxies /api to the backend container, so relative paths work
// in both places without a build-time env var.
const API_BASE = import.meta.env.DEV ? 'http://localhost:8080' : '';

function toBase64(bytes: Uint8Array): string {
  let binary = '';
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary);
}

/**
 * Generates the genesis key entirely client-side using the Web Crypto
 * API (crypto.getRandomValues), builds its birth certificate, and
 * returns both together. The raw key material never has to be
 * "trusted into existence" by the backend -- the backend only ever
 * receives it, the same way it will only ever derive from it later.
 */
export function generateGenesisKey(): KeyBirthCert {
  const material = new Uint8Array(32); // 256 bits, AES-256
  crypto.getRandomValues(material);
  const keyId = crypto.randomUUID();
  return {
    keyId,
    createdAt: new Date().toISOString(),
    algorithm: 'AES-256-GCM',
    generation: 0,
    parentKeyId: null,
    material: toBase64(material),
  };
}

export async function submitGenesis(cert: KeyBirthCert): Promise<{ status: string; primaryKeyId: string }> {
  const res = await fetch(`${API_BASE}/api/genesis`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      keyId: cert.keyId,
      createdAt: cert.createdAt,
      algorithm: cert.algorithm,
      material: cert.material,
    }),
  });
  if (!res.ok && res.status !== 409) {
    const body = await res.text();
    throw new Error(`genesis failed (${res.status}): ${body}`);
  }
  return res.json();
}

/**
 * Engages the rotation kill switch. Persisted server-side in
 * rotation_state (paused/paused_at/paused_reason), so it survives a
 * page reload and applies to every connected tab, not just this one.
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
