import './style.css';
import {
  generateGenesisKey,
  submitGenesis,
  fetchStatus,
  subscribeToStatus,
  pauseRotation,
  resumeRotation,
} from './api';
import type { StatusResponse, KeyBirthCert } from './types';

const app = document.querySelector<HTMLDivElement>('#app')!;

app.innerHTML = `
  <div class="wrap">
    <header>
      <h1>🔑 Key Rotation Console</h1>
      <p class="sub">TypeScript frontend &middot; Go rotation service &middot; Postgres &middot; Terraform</p>
    </header>

    <section class="card" id="genesis-card">
      <h2>Genesis Key</h2>
      <p class="muted">Generates a 256-bit key client-side (Web Crypto) and its birth certificate, then hands it to the backend to start the rotation clock.</p>
      <button id="gen-btn">Generate Genesis Key</button>
      <pre id="birth-cert" class="birth-cert" hidden></pre>
      <div id="genesis-status" class="status-line"></div>
    </section>

    <section class="card">
      <h2>Live Status</h2>
      <div class="status-grid">
        <div><span class="label">Primary key</span><span id="s-primary" class="value mono">—</span></div>
        <div><span class="label">Generation</span><span id="s-gen" class="value">—</span></div>
        <div><span class="label">Rotation interval</span><span id="s-interval" class="value">—</span></div>
        <div><span class="label">Next rotation in</span><span id="s-next" class="value countdown">—</span></div>
        <div><span class="label">Last liveness test</span><span id="s-test" class="value"><span class="badge">—</span></span></div>
      </div>
      <div class="kill-switch">
        <div>
          <span class="label">Auto-rotation</span>
          <span id="s-paused-badge" class="badge">—</span>
          <span id="s-paused-detail" class="muted"></span>
        </div>
        <button id="kill-btn" class="danger">Stop auto-rotation</button>
      </div>
      <p class="muted small">
        Stops Loop A (Go + Postgres) from starting any new rotation. Checked inside the same
        locked transaction that decides whether a rotation is due, so it's race-free and
        persists in Postgres — a backend restart or a second replica both still honor it.
        Terraform's sidecar (Loop B) is unaffected either way; it just has nothing new to apply.
      </p>
    </section>

    <section class="card">
      <h2>Key Chain</h2>
      <table id="keys-table">
        <thead>
          <tr><th>Gen</th><th>Key ID</th><th>Status</th><th>Created</th><th>Verified</th></tr>
        </thead>
        <tbody></tbody>
      </table>
    </section>

    <section class="card">
      <h2>Live files &amp; history</h2>
      <p class="muted">
        The first two tabs are <strong>snapshots</strong> — each one only ever holds the current
        state and is overwritten on every update, read fresh off the shared Docker volume below.
        Loop A (Go) writes the tfvars file after each rotation commits; Loop B (the Terraform
        sidecar) reads it, applies only if its hash changed, and writes the output file. Neither
        loop calls the other directly. <strong>History</strong> is the opposite: the real,
        <strong>append-only</strong> ledger in Postgres — nothing in it is ever overwritten or
        deleted, which is where "did key versions actually accumulate" is answered, not in the
        snapshot files. <strong>tf-config</strong> is just the static <code>main.tf</code> source
        that produces those snapshots, for reference.
      </p>
      <div class="tabs">
        <button class="tab-btn active" data-tab="tfvars">rotation.auto.tfvars.json</button>
        <button class="tab-btn" data-tab="output">terraform output</button>
        <button class="tab-btn" data-tab="tfconfig">tf-config</button>
        <button class="tab-btn" data-tab="history">history</button>
      </div>
      <div id="tab-tfvars" class="tab-panel active">
        <div class="file-meta"><span id="tfvars-path" class="mono muted"></span> <span id="tfvars-modified" class="muted"></span></div>
        <pre id="tfvars-content" class="artifact-json">—</pre>
      </div>
      <div id="tab-output" class="tab-panel">
        <div class="file-meta"><span id="output-path" class="mono muted"></span> <span id="output-modified" class="muted"></span></div>
        <pre id="output-content" class="artifact-json">—</pre>
      </div>
      <div id="tab-tfconfig" class="tab-panel">
        <div class="file-meta"><span id="tfconfig-path" class="mono muted"></span> <span id="tfconfig-modified" class="muted"></span></div>
        <pre id="tfconfig-content" class="artifact-json">—</pre>
      </div>
      <div id="tab-history" class="tab-panel">
        <p class="muted small">Every rotation attempt ever recorded, newest first. Nothing here is ever deleted or overwritten.</p>
        <table id="history-table">
          <thead><tr><th>When</th><th>Rotation ID</th><th>From → To</th><th>Result</th></tr></thead>
          <tbody></tbody>
        </table>
      </div>
      <div class="status-line">
        Sync status: <span id="sync-badge" class="badge">—</span>
      </div>
    </section>

    <section class="card">
      <h2>Genesis audit trail</h2>
      <p class="muted">
        Every <code>/api/genesis</code> request the backend has received, in order — including
        rejected and no-op ones. Logged server-side the instant the request arrives, independent
        of the "Generate Genesis Key" button's disabled state in any particular browser tab, so a
        rapid re-click (or a second tab) is traced here rather than silently disappearing.
      </p>
      <table id="audit-table">
        <thead><tr><th>Received</th><th>Key ID</th><th>Outcome</th><th>Detail</th></tr></thead>
        <tbody></tbody>
      </table>
    </section>

    <footer>
      <span id="conn-indicator" class="conn">connecting…</span>
    </footer>
  </div>
`;

const genBtn = document.querySelector<HTMLButtonElement>('#gen-btn')!;
const birthCertEl = document.querySelector<HTMLPreElement>('#birth-cert')!;
const genesisStatusEl = document.querySelector<HTMLDivElement>('#genesis-status')!;
const connIndicator = document.querySelector<HTMLSpanElement>('#conn-indicator')!;

genBtn.addEventListener('click', async () => {
  genBtn.disabled = true;
  genesisStatusEl.textContent = 'Generating key material…';
  try {
    const cert: KeyBirthCert = generateGenesisKey();
    birthCertEl.hidden = false;
    birthCertEl.textContent = JSON.stringify(
      { ...cert, material: cert.material.slice(0, 12) + '… (32 bytes, base64)' },
      null,
      2,
    );
    genesisStatusEl.textContent = 'Submitting to backend…';
    const result = await submitGenesis(cert);
    genesisStatusEl.textContent =
      result.status === 'already-initialized'
        ? `A genesis key already exists (primary: ${result.primaryKeyId.slice(0, 8)}…). Showing live state below.`
        : `Genesis key created. Rotation clock started.`;
  } catch (err) {
    genesisStatusEl.textContent = `Error: ${(err as Error).message}`;
  } finally {
    genBtn.disabled = false;
  }
});

const killBtn = document.querySelector<HTMLButtonElement>('#kill-btn')!;
let currentlyPaused = false;

killBtn.addEventListener('click', async () => {
  killBtn.disabled = true;
  try {
    if (currentlyPaused) {
      await resumeRotation();
    } else {
      await pauseRotation('stopped from the console');
    }
    // No local state flip here — the next status tick (SSE or poll)
    // is the source of truth, same as everything else in this UI.
  } catch (err) {
    genesisStatusEl.textContent = `Kill switch error: ${(err as Error).message}`;
  } finally {
    killBtn.disabled = false;
  }
});

// Tab switching for the live-file / history panels.
document.querySelectorAll<HTMLButtonElement>('.tab-btn').forEach((btn) => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.tab-btn').forEach((b) => b.classList.remove('active'));
    document.querySelectorAll('.tab-panel').forEach((p) => p.classList.remove('active'));
    btn.classList.add('active');
    document.querySelector(`#tab-${btn.dataset.tab}`)!.classList.add('active');
  });
});

function prettyJSON(raw: string | undefined): string {
  if (!raw) return '—';
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw; // show as-is if it isn't valid JSON (e.g. mid-write)
  }
}

function shortId(id: string | null): string {
  if (!id) return '—';
  return id.slice(0, 8) + '…';
}

function render(status: StatusResponse) {
  document.querySelector('#s-primary')!.textContent = shortId(status.primaryKeyId);
  document.querySelector('#s-gen')!.textContent = String(status.generation);
  document.querySelector('#s-interval')!.textContent = `${status.intervalSeconds}s`;
  document.querySelector('#s-next')!.textContent = `${Math.max(0, Math.round(status.nextRotationInSeconds))}s`;

  const testEl = document.querySelector('#s-test')!;
  if (status.lastTestPassed === undefined) {
    testEl.innerHTML = `<span class="badge">—</span>`;
  } else {
    const cls = status.lastTestPassed ? 'badge ok' : 'badge fail';
    const label = status.lastTestPassed ? 'PASSED' : 'FAILED';
    testEl.innerHTML = `<span class="${cls}">${label}</span> <span class="muted">${status.lastTestDetail ?? ''}</span>`;
  }

  currentlyPaused = status.paused;
  const pausedBadge = document.querySelector('#s-paused-badge')!;
  const pausedDetail = document.querySelector('#s-paused-detail')!;
  if (status.paused) {
    pausedBadge.className = 'badge fail';
    pausedBadge.textContent = 'STOPPED';
    pausedDetail.textContent = status.pausedReason
      ? `${status.pausedReason}${status.pausedAt ? ' · ' + new Date(status.pausedAt).toLocaleTimeString() : ''}`
      : '';
    killBtn.textContent = 'Resume auto-rotation';
  } else {
    pausedBadge.className = 'badge ok';
    pausedBadge.textContent = 'RUNNING';
    pausedDetail.textContent = '';
    killBtn.textContent = 'Stop auto-rotation';
  }

  document.querySelector('#tfvars-path')!.textContent = status.tfVars.path;
  document.querySelector('#tfvars-content')!.textContent = status.tfVars.exists
    ? prettyJSON(status.tfVars.content)
    : '(not written yet — waiting for the first rotation)';
  document.querySelector('#tfvars-modified')!.textContent = status.tfVars.lastModified
    ? `updated ${new Date(status.tfVars.lastModified).toLocaleTimeString()}`
    : '';

  document.querySelector('#output-path')!.textContent = status.terraformOutput.path;
  document.querySelector('#output-content')!.textContent = status.terraformOutput.exists
    ? prettyJSON(status.terraformOutput.content)
    : '(not written yet — waiting for the first terraform apply)';
  document.querySelector('#output-modified')!.textContent = status.terraformOutput.lastModified
    ? `updated ${new Date(status.terraformOutput.lastModified).toLocaleTimeString()}`
    : '';

  const syncBadge = document.querySelector('#sync-badge')!;
  if (status.terraformInSync === undefined) {
    syncBadge.className = 'badge';
    syncBadge.textContent = '—';
  } else if (status.terraformInSync) {
    syncBadge.className = 'badge ok';
    syncBadge.textContent = 'in sync — terraform has applied the latest rotation';
  } else {
    syncBadge.className = 'badge fail';
    syncBadge.textContent = 'pending — terraform hasn\u2019t applied the latest rotation yet';
  }

  document.querySelector('#tfconfig-path')!.textContent = status.tfConfig.path;
  document.querySelector('#tfconfig-content')!.textContent = status.tfConfig.exists
    ? status.tfConfig.content ?? '—'
    : '(main.tf not mounted — check the tf-config volume mount)';
  document.querySelector('#tfconfig-modified')!.textContent = status.tfConfig.lastModified
    ? `updated ${new Date(status.tfConfig.lastModified).toLocaleTimeString()}`
    : '';

  const historyBody = document.querySelector('#history-table tbody')!;
  historyBody.innerHTML = status.history
    .map((e) => {
      const resultCls = e.applied ? 'badge ok' : 'badge fail';
      const resultLabel = e.applied ? 'applied' : e.reason.includes('not due') || e.reason.includes('paused') || e.reason.includes('lock held') || e.reason.includes('already handled')
        ? 'skipped'
        : 'failed';
      return `
      <tr>
        <td>${new Date(e.triggeredAt).toLocaleTimeString()}</td>
        <td class="mono">${e.rotationId.slice(0, 10)}…</td>
        <td class="mono">${shortId(e.fromKeyId)} → ${shortId(e.toKeyId)}</td>
        <td><span class="${resultCls}">${resultLabel}</span> <span class="muted small">${e.reason}</span></td>
      </tr>`;
    })
    .join('');

  const auditBody = document.querySelector('#audit-table tbody')!;
  auditBody.innerHTML = status.genesisAttempts
    .map((a) => {
      const cls = a.outcome === 'created' ? 'badge ok' : a.outcome === 'already-initialized' ? 'badge' : 'badge fail';
      return `
      <tr>
        <td>${new Date(a.receivedAt).toLocaleTimeString()}</td>
        <td class="mono">${shortId(a.attemptedKeyId ?? null)}</td>
        <td><span class="${cls}">${a.outcome}</span></td>
        <td class="muted small">${a.detail ?? ''}</td>
      </tr>`;
    })
    .join('');

  const tbody = document.querySelector('#keys-table tbody')!;
  tbody.innerHTML = status.keys
    .slice()
    .reverse()
    .map(
      (k) => `
      <tr class="row-${k.status}">
        <td>${k.generation}</td>
        <td class="mono">${shortId(k.keyId)}</td>
        <td><span class="badge status-${k.status}">${k.status}</span></td>
        <td>${new Date(k.createdAt).toLocaleTimeString()}</td>
        <td>${k.verifiedAt ? new Date(k.verifiedAt).toLocaleTimeString() : '—'}</td>
      </tr>`,
    )
    .join('');
}

// Realtime updates via SSE (pushed roughly once per second by the
// backend's rotation ticker).
subscribeToStatus((status) => {
  connIndicator.textContent = 'live (SSE)';
  connIndicator.className = 'conn ok';
  render(status);
});

// Polling fallback covers the brief window before the first SSE
// message arrives, and keeps working even if EventSource is blocked.
let pollFailures = 0;
setInterval(async () => {
  try {
    const status = await fetchStatus();
    render(status);
    pollFailures = 0;
  } catch {
    pollFailures += 1;
    if (pollFailures > 2) {
      connIndicator.textContent = 'backend unreachable';
      connIndicator.className = 'conn fail';
    }
  }
}, 2000);
