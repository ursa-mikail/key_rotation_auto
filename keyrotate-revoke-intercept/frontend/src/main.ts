import './style.css';
import {
  submitGenesisBatch,
  fetchStatus,
  subscribeToStatus,
  pauseRotation,
  resumeRotation,
  revokeKeyset,
  setRevokeMode,
} from './api';
import type { StatusResponse, RotationOutcome } from './types';

const app = document.querySelector<HTMLDivElement>('#app')!;

app.innerHTML = `
  <div class="wrap">
    <header>
      <h1>🔑 Key Rotation Console</h1>
      <p class="sub">All N keysets rotate &middot; async revoke interceptor &middot; TypeScript &middot; Go &middot; Postgres &middot; Terraform</p>
    </header>

    <section class="card" id="genesis-card">
      <h2>Batch Genesis</h2>
      <p class="muted">
        Generates <strong>N = 50</strong> independent keysets server-side (crypto/rand, material
        never leaves Go). Every one of them rotates on its own random 3&ndash;20s interval &mdash;
        there's no static tier in this version. Separately, a background <strong>revoke
        interceptor</strong> randomly picks a still-active keyset and revokes it at any moment,
        independent of that keyset's own timer.
      </p>
      <button id="gen-btn">Generate 50 Keysets</button>
      <div id="genesis-status" class="status-line"></div>
    </section>

    <section class="card">
      <h2>Overview</h2>
      <div class="status-grid">
        <div><span class="label">Keysets (N)</span><span id="s-n" class="value">—</span></div>
        <div><span class="label">Active</span><span id="s-active" class="value">—</span></div>
        <div><span class="label">Terminated</span><span id="s-terminated" class="value">—</span></div>
        <div><span class="label">Soonest rotation</span><span id="s-next" class="value countdown">—</span></div>
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
        Global kill switch &mdash; stops the timer loop AND the revoke interceptor at once.
      </p>

      <div class="revoke-controls">
        <div class="revoke-mode-toggle">
          <span class="label">When a revoke lands</span>
          <div class="segmented" id="revoke-mode-segmented">
            <button data-mode="auto" class="seg-btn">⚡ Auto-rotate</button>
            <button data-mode="halt" class="seg-btn">⛔ Halt</button>
          </div>
        </div>
        <p class="muted small">
          <strong>Auto-rotate</strong>: a revoked keyset is immediately emergency-rotated and
          resumes normal cycling. <strong>Halt</strong>: a revoked keyset is permanently stopped
          &mdash; no further rotation, ever. Read fresh, live, at the moment each revoke is
          actually processed &mdash; flip this any time and it applies to the very next one.
        </p>
        <button id="revoke-now-btn">🎲 Revoke a random keyset now</button>
        <span id="revoke-now-status" class="status-line"></span>
      </div>
    </section>

    <section class="card">
      <h2>Keysets <span class="muted small" id="keysets-count-label"></span></h2>
      <p class="muted small">
        Colored chip is a stable per-keyset color — it never changes, no matter which tab you're
        on. Click any chip (here, in the numbered strip below, or in the history feed further
        down) to drill down to just that one keyset in both tables; click it again (or "clear")
        to go back.
      </p>
      <div class="tabs keyset-tabs">
        <button class="tab-btn active" data-filter="all">All <span class="tab-count" id="count-all"></span></button>
        <button class="tab-btn" data-filter="active">Active <span class="tab-count" id="count-active"></span></button>
        <button class="tab-btn" data-filter="terminated">Terminated <span class="tab-count" id="count-terminated"></span></button>
      </div>
      <p class="muted small index-strip-label">
        Jump to a specific keyset (1&ndash;N) &mdash; each swatch is one keyset's own "tab":
      </p>
      <div id="keyset-index-strip" class="keyset-index-strip"></div>
      <div id="keyset-selection-bar" class="selection-bar" style="display: none;">
        Showing only <span id="keyset-selection-chip"></span>
        <button id="revoke-selected-btn" class="link-btn">revoke this one</button>
        <button id="clear-selection-btn" class="link-btn">clear</button>
      </div>
      <table id="keysets-table">
        <thead>
          <tr><th>#</th><th>Keyset</th><th>Status</th><th>Last event</th><th>Primary key</th><th>Gen</th><th>Expires in</th></tr>
        </thead>
        <tbody></tbody>
      </table>
    </section>

    <section class="card">
      <h2>Live files &amp; history</h2>
      <p class="muted">
        The first two tabs are <strong>snapshots</strong> covering ALL keysets at once, including
        the live <code>terminated</code> / <code>last_outcome</code> / <code>last_trigger</code>
        status fields — overwritten on every update, read fresh off the shared Docker volume.
        <strong>History</strong> is the append-only ledger in Postgres — every timer rotation and
        every revoke, across every keyset, nothing ever deleted.
      </p>
      <div class="tabs">
        <button class="tab-btn active" data-tab="resource">live resource (HCL)</button>
        <button class="tab-btn" data-tab="tfvars">rotation.auto.tfvars.json</button>
        <button class="tab-btn" data-tab="output">terraform output</button>
        <button class="tab-btn" data-tab="tfconfig">tf-config</button>
        <button class="tab-btn" data-tab="history">history</button>
      </div>
      <div id="tab-resource" class="tab-panel active">
        <div class="file-meta"><span id="resource-path" class="mono muted"></span></div>
        <p class="muted small">
          Every keyset's resource block, live values plugged in, including
          <code>terminated</code> / <code>last_outcome</code> where applicable.
        </p>
        <pre id="resource-content" class="artifact-json">—</pre>
      </div>
      <div id="tab-tfvars" class="tab-panel">
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
        <p class="muted small">
          Every rotation-related event ever recorded, newest first, across both triggers (⏱ timer,
          🎲 revoke). Left edge + keyset chip colors match the Keysets table above, and respect the
          same filter tab / drill-down.
        </p>
        <table id="history-table">
          <thead><tr><th>When</th><th>Keyset</th><th>Trigger</th><th>From → To</th><th>Result</th></tr></thead>
          <tbody></tbody>
        </table>
      </div>
      <div class="status-line">
        Sync status: <span id="sync-badge" class="badge">—</span>
      </div>
    </section>

    <section class="card">
      <h2>Genesis audit trail</h2>
      <table id="audit-table">
        <thead><tr><th>Received</th><th>Outcome</th><th>Detail</th></tr></thead>
        <tbody></tbody>
      </table>
    </section>

    <footer>
      <span id="conn-indicator" class="conn">connecting…</span>
    </footer>
  </div>
`;

const genBtn = document.querySelector<HTMLButtonElement>('#gen-btn')!;
const genesisStatusEl = document.querySelector<HTMLDivElement>('#genesis-status')!;
const connIndicator = document.querySelector<HTMLSpanElement>('#conn-indicator')!;

let genesisLocked = false;

genBtn.addEventListener('click', async () => {
  if (genesisLocked) return;
  genBtn.disabled = true;
  genesisStatusEl.textContent = 'Generating 50 keysets server-side…';
  try {
    const result = await submitGenesisBatch();
    genesisStatusEl.textContent =
      result.status === 'already-initialized'
        ? `Batch genesis already ran (N=${result.keysetCount}). Showing live state below.`
        : `Created N=${result.keysetCount} keysets, all rotating on independent random intervals.`;
  } catch (err) {
    genesisStatusEl.textContent = `Error: ${(err as Error).message}`;
  } finally {
    if (!genesisLocked) genBtn.disabled = false;
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
  } catch (err) {
    genesisStatusEl.textContent = `Kill switch error: ${(err as Error).message}`;
  } finally {
    killBtn.disabled = false;
  }
});

// Revoke-mode toggle (auto-rotate vs halt). currentAutoRotate mirrors
// the last status snapshot; clicking a segment optimistically disables
// both buttons until the next status tick confirms the change.
let currentAutoRotate = true;
document.querySelectorAll<HTMLButtonElement>('#revoke-mode-segmented .seg-btn').forEach((btn) => {
  btn.addEventListener('click', async () => {
    const wantsAuto = btn.dataset.mode === 'auto';
    if (wantsAuto === currentAutoRotate) return;
    document.querySelectorAll<HTMLButtonElement>('#revoke-mode-segmented .seg-btn').forEach((b) => (b.disabled = true));
    try {
      await setRevokeMode(wantsAuto);
    } catch (err) {
      genesisStatusEl.textContent = `Revoke-mode error: ${(err as Error).message}`;
    } finally {
      document.querySelectorAll<HTMLButtonElement>('#revoke-mode-segmented .seg-btn').forEach((b) => (b.disabled = false));
    }
  });
});

const revokeNowBtn = document.querySelector<HTMLButtonElement>('#revoke-now-btn')!;
const revokeNowStatus = document.querySelector<HTMLSpanElement>('#revoke-now-status')!;
revokeNowBtn.addEventListener('click', async () => {
  revokeNowBtn.disabled = true;
  revokeNowStatus.textContent = 'Revoking a random active keyset…';
  try {
    const res = await revokeKeyset();
    revokeNowStatus.textContent = res.terminated
      ? `${res.keysetId} revoked and terminated.`
      : res.rotated
        ? `${res.keysetId} revoked and emergency-rotated.`
        : `${res.keysetId}: ${res.skipped ?? 'no change'}`;
  } catch (err) {
    revokeNowStatus.textContent = `Error: ${(err as Error).message}`;
  } finally {
    revokeNowBtn.disabled = false;
  }
});

document.querySelector('#revoke-selected-btn')!.addEventListener('click', async () => {
  if (!selectedKeysetId) return;
  const id = selectedKeysetId;
  revokeNowStatus.textContent = `Revoking ${id}…`;
  try {
    const res = await revokeKeyset(id);
    revokeNowStatus.textContent = res.terminated
      ? `${res.keysetId} revoked and terminated.`
      : res.rotated
        ? `${res.keysetId} revoked and emergency-rotated.`
        : `${res.keysetId}: ${res.skipped ?? 'no change'}`;
  } catch (err) {
    revokeNowStatus.textContent = `Error: ${(err as Error).message}`;
  }
});

// Tab switching for the live-file / history panels (data-tab).
document.querySelectorAll<HTMLButtonElement>('.tab-btn[data-tab]').forEach((btn) => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.tab-btn[data-tab]').forEach((b) => b.classList.remove('active'));
    document.querySelectorAll('.tab-panel').forEach((p) => p.classList.remove('active'));
    btn.classList.add('active');
    document.querySelector(`#tab-${btn.dataset.tab}`)!.classList.add('active');
  });
});

type KeysetFilter = 'all' | 'active' | 'terminated';
let keysetFilter: KeysetFilter = 'all';
let lastKeysets: StatusResponse['keysets'] = [];
let lastHistory: StatusResponse['history'] = [];
let selectedKeysetId: string | null = null;
let keysetMeta = new Map<string, { terminated: boolean }>();

document.querySelectorAll<HTMLButtonElement>('.keyset-tabs .tab-btn[data-filter]').forEach((btn) => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.keyset-tabs .tab-btn').forEach((b) => b.classList.remove('active'));
    btn.classList.add('active');
    keysetFilter = btn.dataset.filter as KeysetFilter;
    selectedKeysetId = null;
    updateSelectionBar();
    renderKeysetsTable(lastKeysets);
    renderHistoryTable(lastHistory);
  });
});

document.querySelector('#clear-selection-btn')!.addEventListener('click', () => {
  selectedKeysetId = null;
  updateSelectionBar();
  renderKeysetsTable(lastKeysets);
  renderHistoryTable(lastHistory);
});

document.addEventListener('click', (event) => {
  const chip = (event.target as HTMLElement).closest<HTMLElement>('.keyset-chip[data-keysetid]');
  if (!chip) return;
  const id = chip.dataset.keysetid!;
  selectedKeysetId = selectedKeysetId === id ? null : id;
  updateSelectionBar();
  renderKeysetsTable(lastKeysets);
  renderHistoryTable(lastHistory);
});

function updateSelectionBar() {
  const bar = document.querySelector<HTMLElement>('#keyset-selection-bar')!;
  const chipSlot = document.querySelector('#keyset-selection-chip')!;
  if (selectedKeysetId) {
    bar.style.display = '';
    chipSlot.innerHTML = keysetChip(selectedKeysetId);
  } else {
    bar.style.display = 'none';
    chipSlot.innerHTML = '';
  }
}

function prettyJSON(raw: string | undefined): string {
  if (!raw) return '—';
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

function shortId(id: string | null | undefined): string {
  if (!id) return '—';
  return id.slice(0, 8) + '…';
}

function keysetColor(keysetId: string): string {
  let hash = 0;
  for (let i = 0; i < keysetId.length; i++) {
    hash = (hash * 31 + keysetId.charCodeAt(i)) | 0;
  }
  const hue = Math.abs(hash) % 360;
  return `hsl(${hue}, 68%, 58%)`;
}

function keysetChip(keysetId: string): string {
  const color = keysetColor(keysetId);
  const label = keysetId.replace('_keyset', '');
  return `<span class="keyset-chip" data-keysetid="${keysetId}" role="button" tabindex="0" title="Click to filter to just ${label}" style="background:${color}22;color:${color};border:1px solid ${color}66">${label}</span>`;
}

function statusBadge(status: 'rotating' | 'terminated'): string {
  return status === 'terminated' ? `<span class="badge fail">⛔ terminated</span>` : `<span class="badge ok">rotating</span>`;
}

function outcomeBadge(outcome: RotationOutcome | undefined, trigger: string | undefined): string {
  if (!outcome) return `<span class="muted small">—</span>`;
  const triggerIcon = trigger === 'revoke' ? '🎲' : '⏱';
  switch (outcome) {
    case 'rotated':
      return `<span class="badge ok">${triggerIcon} rotated</span>`;
    case 'revoked_rotated':
      return `<span class="badge warn">${triggerIcon} revoked → rotated</span>`;
    case 'revoked_terminated':
      return `<span class="badge fail">${triggerIcon} revoked → terminated</span>`;
    case 'failed':
      return `<span class="badge fail">${triggerIcon} failed</span>`;
    case 'skipped':
      return `<span class="badge">${triggerIcon} skipped</span>`;
    default:
      return `<span class="badge">${triggerIcon} ${outcome}</span>`;
  }
}

function render(status: StatusResponse) {
  if (status.initialized && !genesisLocked) {
    genesisLocked = true;
    genBtn.disabled = true;
    genBtn.textContent = 'Keysets already generated';
  }

  document.querySelector('#s-n')!.textContent = String(status.keysetCount);
  const terminatedCount = (status.keysets ?? []).filter((k) => k.terminated).length;
  document.querySelector('#s-active')!.textContent = String(status.keysetCount - terminatedCount);
  document.querySelector('#s-terminated')!.textContent = String(terminatedCount);
  document.querySelector('#keysets-count-label')!.textContent = status.initialized ? `(${status.keysetCount} total)` : '';

  const activeKeysets = (status.keysets ?? []).filter((k) => !k.terminated && k.nextRotationInSeconds !== undefined);
  const soonest = activeKeysets.length
    ? activeKeysets.reduce((min, k) => Math.min(min, k.nextRotationInSeconds!), Infinity)
    : null;
  document.querySelector('#s-next')!.textContent = soonest === null ? '—' : `${Math.max(0, Math.round(soonest))}s`;

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

  currentAutoRotate = status.revokeAutoRotate;
  document.querySelectorAll<HTMLButtonElement>('#revoke-mode-segmented .seg-btn').forEach((btn) => {
    const isActive = (btn.dataset.mode === 'auto') === currentAutoRotate;
    btn.classList.toggle('active', isActive);
  });

  document.querySelector('#resource-path')!.textContent = status.renderedResource.path;
  document.querySelector('#resource-content')!.textContent = status.renderedResource.exists
    ? status.renderedResource.content ?? '—'
    : '(no keysets yet)';

  document.querySelector('#tfvars-path')!.textContent = status.tfVars.path;
  document.querySelector('#tfvars-content')!.textContent = status.tfVars.exists
    ? prettyJSON(status.tfVars.content)
    : '(not written yet — waiting for batch genesis)';
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
    syncBadge.textContent = "in sync — output file matches every keyset's current expiry";
  } else {
    syncBadge.className = 'badge';
    syncBadge.textContent = 'catching up — one apply cycle behind (expected, see README)';
  }

  document.querySelector('#tfconfig-path')!.textContent = status.tfConfig.path;
  document.querySelector('#tfconfig-content')!.textContent = status.tfConfig.exists
    ? status.tfConfig.content ?? '—'
    : '(main.tf not mounted — check the tf-config volume mount)';
  document.querySelector('#tfconfig-modified')!.textContent = status.tfConfig.lastModified
    ? `updated ${new Date(status.tfConfig.lastModified).toLocaleTimeString()}`
    : '';

  keysetMeta = new Map((status.keysets ?? []).map((k) => [k.keysetId, { terminated: k.terminated }]));

  lastHistory = status.history ?? [];
  renderHistoryTable(lastHistory);

  const auditBody = document.querySelector('#audit-table tbody')!;
  auditBody.innerHTML = (status.genesisAttempts ?? [])
    .map((a) => {
      const cls = a.outcome === 'created' ? 'badge ok' : a.outcome === 'already-initialized' ? 'badge' : 'badge fail';
      return `
      <tr>
        <td>${new Date(a.receivedAt).toLocaleTimeString()}</td>
        <td><span class="${cls}">${a.outcome}</span></td>
        <td class="muted small">${a.detail ?? ''}</td>
      </tr>`;
    })
    .join('');

  renderKeysetsTable(status.keysets ?? []);
}

function renderKeysetIndexStrip(keysets: StatusResponse['keysets']) {
  const strip = document.querySelector('#keyset-index-strip')!;
  if (keysets.length === 0) {
    strip.innerHTML = '';
    return;
  }
  strip.innerHTML = keysets
    .slice()
    .sort((a, b) => a.index - b.index)
    .map((k) => {
      const color = keysetColor(k.keysetId);
      const selected = k.keysetId === selectedKeysetId;
      const kind = k.terminated ? 'terminated' : 'rotating';
      return `<span
        class="keyset-chip keyset-index-chip${selected ? ' selected' : ''}${k.terminated ? ' terminated' : ''}"
        data-keysetid="${k.keysetId}"
        role="button" tabindex="0"
        title="${k.keysetId} — ${kind}"
        style="background:${color}22;color:${color};border:1px solid ${color}${selected ? 'ff' : '66'}"
      >${k.index}</span>`;
    })
    .join('');
}

function applyKeysetFilter(keysets: StatusResponse['keysets'], filter: KeysetFilter): StatusResponse['keysets'] {
  switch (filter) {
    case 'active':
      return keysets.filter((k) => !k.terminated);
    case 'terminated':
      return keysets.filter((k) => k.terminated);
    default:
      return keysets;
  }
}

function renderKeysetsTable(keysets: StatusResponse['keysets']) {
  lastKeysets = keysets;

  renderKeysetIndexStrip(keysets);

  document.querySelector('#count-all')!.textContent = `(${keysets.length})`;
  document.querySelector('#count-active')!.textContent = `(${keysets.filter((k) => !k.terminated).length})`;
  document.querySelector('#count-terminated')!.textContent = `(${keysets.filter((k) => k.terminated).length})`;

  const visible = selectedKeysetId
    ? keysets.filter((k) => k.keysetId === selectedKeysetId)
    : applyKeysetFilter(keysets, keysetFilter);

  const keysetsBody = document.querySelector('#keysets-table tbody')!;
  keysetsBody.innerHTML = visible
    .slice()
    .sort((a, b) => a.index - b.index)
    .map((k) => {
      const color = keysetColor(k.keysetId);
      const expiresIn =
        !k.terminated && k.nextRotationInSeconds !== undefined
          ? `${Math.max(0, Math.round(k.nextRotationInSeconds))}s`
          : '—';
      return `
      <tr style="border-left: 3px solid ${color}">
        <td>${k.index}</td>
        <td>${keysetChip(k.keysetId)}</td>
        <td>${statusBadge(k.status)}</td>
        <td>${outcomeBadge(k.lastEventOutcome, k.lastEventTrigger)}</td>
        <td class="mono">${shortId(k.primaryKeyId)}</td>
        <td>${k.generation}</td>
        <td class="countdown">${expiresIn}</td>
      </tr>`;
    })
    .join('');

  if (visible.length === 0) {
    keysetsBody.innerHTML = `<tr><td colspan="7" class="muted small">No keysets match this filter yet.</td></tr>`;
  }
}

function renderHistoryTable(history: StatusResponse['history']) {
  lastHistory = history;

  const visible = selectedKeysetId
    ? history.filter((e) => e.keysetId === selectedKeysetId)
    : history.filter((e) => {
        if (keysetFilter === 'all') return true;
        const meta = keysetMeta.get(e.keysetId);
        if (!meta) return true;
        if (keysetFilter === 'active') return !meta.terminated;
        if (keysetFilter === 'terminated') return meta.terminated;
        return true;
      });

  const historyBody = document.querySelector('#history-table tbody')!;
  historyBody.innerHTML = visible
    .map((e) => {
      const color = keysetColor(e.keysetId);
      const triggerLabel = e.trigger === 'revoke' ? '🎲 revoke' : '⏱ timer';
      return `
      <tr style="border-left: 4px solid ${color}">
        <td>${new Date(e.triggeredAt).toLocaleTimeString()}</td>
        <td>${keysetChip(e.keysetId)}</td>
        <td class="muted small">${triggerLabel}</td>
        <td class="mono">${shortId(e.fromKeyId)} → ${shortId(e.toKeyId)}</td>
        <td>${outcomeBadge(e.outcome, e.trigger)} <span class="muted small">${e.reason}</span></td>
      </tr>`;
    })
    .join('');

  if (visible.length === 0) {
    historyBody.innerHTML = `<tr><td colspan="5" class="muted small">No history rows match this filter yet.</td></tr>`;
  }
}

subscribeToStatus((status) => {
  connIndicator.textContent = 'live (SSE)';
  connIndicator.className = 'conn ok';
  try {
    render(status);
  } catch (err) {
    console.error('render() failed on a valid SSE status message', err);
    connIndicator.textContent = 'connected, but failed to render — see console';
    connIndicator.className = 'conn fail';
  }
});

let pollFailures = 0;
setInterval(async () => {
  let status;
  try {
    status = await fetchStatus();
    pollFailures = 0;
  } catch {
    pollFailures += 1;
    if (pollFailures > 2) {
      connIndicator.textContent = 'backend unreachable';
      connIndicator.className = 'conn fail';
    }
    return;
  }
  try {
    render(status);
  } catch (err) {
    console.error('render() failed on a valid status response', err);
    connIndicator.textContent = 'connected, but failed to render — see console';
    connIndicator.className = 'conn fail';
  }
}, 2000);
