import './style.css';
import {
  submitGenesisBatch,
  fetchStatus,
  subscribeToStatus,
  pauseRotation,
  resumeRotation,
} from './api';
import type { StatusResponse } from './types';

const app = document.querySelector<HTMLDivElement>('#app')!;

app.innerHTML = `
  <div class="wrap">
    <header>
      <h1>🔑 Key Rotation Console</h1>
      <p class="sub">N keysets &middot; M rotating &middot; L emergency-renewed &middot; TypeScript &middot; Go &middot; Postgres &middot; Terraform</p>
    </header>

    <section class="card" id="genesis-card">
      <h2>Batch Genesis</h2>
      <p class="muted">
        Generates <strong>N = 50</strong> independent keysets server-side (crypto/rand, material
        never leaves Go), randomly selects <strong>M = 20</strong> of them to actively rotate, and
        randomly flags <strong>L</strong> (0&ndash;20) of those M as <strong>revoked</strong> &mdash;
        needing an immediate emergency renewal instead of waiting for their normal random expiry.
      </p>
      <button id="gen-btn">Generate 50 Keysets</button>
      <div id="genesis-status" class="status-line"></div>
    </section>

    <section class="card">
      <h2>Overview</h2>
      <div class="status-grid">
        <div><span class="label">Keysets (N)</span><span id="s-n" class="value">—</span></div>
        <div><span class="label">Rotating (M)</span><span id="s-m" class="value">—</span></div>
        <div><span class="label">Emergency-renewed (L)</span><span id="s-l" class="value">—</span></div>
        <div><span class="label">Static</span><span id="s-static" class="value">—</span></div>
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
        Global kill switch &mdash; stops all M rotating keysets at once. Checked inside the same
        code path that decides whether any keyset is due, so it's race-free and persists in
        Postgres. Terraform's sidecar is unaffected either way; it just has nothing new to apply.
      </p>
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
        <button class="tab-btn" data-filter="rotating">Rotating <span class="tab-count" id="count-rotating"></span></button>
        <button class="tab-btn" data-filter="revoked">Revoked <span class="tab-count" id="count-revoked"></span></button>
        <button class="tab-btn" data-filter="static">Static <span class="tab-count" id="count-static"></span></button>
      </div>
      <p class="muted small index-strip-label">
        Jump to a specific keyset (1&ndash;N) &mdash; each swatch is one keyset's own "tab":
      </p>
      <div id="keyset-index-strip" class="keyset-index-strip"></div>
      <div id="keyset-selection-bar" class="selection-bar" style="display: none;">
        Showing only <span id="keyset-selection-chip"></span>
        <button id="clear-selection-btn" class="link-btn">clear</button>
      </div>
      <table id="keysets-table">
        <thead>
          <tr><th>#</th><th>Keyset</th><th>Kind</th><th>Status</th><th>Primary key</th><th>Gen</th><th>Expires in</th></tr>
        </thead>
        <tbody></tbody>
      </table>
    </section>

    <section class="card">
      <h2>Live files &amp; history</h2>
      <p class="muted">
        The first two tabs are <strong>snapshots</strong> covering ALL keysets at once &mdash;
        overwritten on every update, read fresh off the shared Docker volume below. Loop A (Go)
        writes the tfvars file after any rotation across any keyset commits; Loop B (the Terraform
        sidecar) applies unconditionally on a timer and Terraform's own HCL decides whether
        anything is due. <strong>History</strong> is the append-only ledger in Postgres across
        every keyset &mdash; nothing in it is ever overwritten or deleted.
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
          What every keyset's resource looks like right now, with live values plugged in &mdash;
          one <code>ursa_keyset</code> block per keyset, regenerated fresh on every update below.
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
          Every rotation attempt ever recorded across every keyset, newest first. Each row's left
          edge and keyset chip are colored per-keyset (same color as the Keysets table above) so
          you can visually trace one keyset's rotations through a busy, interleaved feed.
        </p>
        <table id="history-table">
          <thead><tr><th>When</th><th>Keyset</th><th>Rotation ID</th><th>From → To</th><th>Result</th></tr></thead>
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
        Every <code>/api/genesis</code> request the backend has received, in order &mdash;
        including no-op re-clicks after the real batch genesis already ran.
      </p>
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

// Once batch genesis has run anywhere, generating again is never a
// valid action -- there is exactly one batch-genesis event, ever.
// This flag permanently locks the button; it is not reset by
// anything in this browser tab, only by render() seeing
// status.initialized become true (driven by Postgres, not by this
// tab's own click).
let genesisLocked = false;

genBtn.addEventListener('click', async () => {
  if (genesisLocked) return; // belt-and-suspenders; the button is disabled anyway
  genBtn.disabled = true;
  genesisStatusEl.textContent = 'Generating 50 keysets server-side…';
  try {
    const result = await submitGenesisBatch();
    genesisStatusEl.textContent =
      result.status === 'already-initialized'
        ? `Batch genesis already ran (N=${result.keysetCount}, M=${result.rotatingCount}, L=${result.revokedCount}). Showing live state below.`
        : `Created N=${result.keysetCount} keysets: M=${result.rotatingCount} rotating (of which L=${result.revokedCount} needed immediate emergency renewal), ${result.staticCount} static.`;
  } catch (err) {
    genesisStatusEl.textContent = `Error: ${(err as Error).message}`;
  } finally {
    // Do NOT unconditionally re-enable here -- the next status tick
    // is the authoritative path once genesis has actually happened.
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
    // No local state flip here — the next status tick (SSE or poll)
    // is the source of truth, same as everything else in this UI.
  } catch (err) {
    genesisStatusEl.textContent = `Kill switch error: ${(err as Error).message}`;
  } finally {
    killBtn.disabled = false;
  }
});

// Tab switching for the live-file / history panels (data-tab -- these
// swap which panel is visible).
document.querySelectorAll<HTMLButtonElement>('.tab-btn[data-tab]').forEach((btn) => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.tab-btn[data-tab]').forEach((b) => b.classList.remove('active'));
    document.querySelectorAll('.tab-panel').forEach((p) => p.classList.remove('active'));
    btn.classList.add('active');
    document.querySelector(`#tab-${btn.dataset.tab}`)!.classList.add('active');
  });
});

// Kind of keyset currently shown in the Keysets table AND the History
// table -- both are driven by the same category filter, so switching
// tabs narrows the whole picture consistently instead of just one
// table.
type KeysetFilter = 'all' | 'rotating' | 'revoked' | 'static';
let keysetFilter: KeysetFilter = 'all';
let lastKeysets: StatusResponse['keysets'] = [];
let lastHistory: StatusResponse['history'] = [];

// Per-keyset drill-down: clicking any colored chip (in either table)
// sets this, which overrides the category filter above and narrows
// BOTH tables down to just that one keyset -- effectively giving each
// of the 50 keysets its own on-demand "tab" without needing 50 actual
// tab buttons. Clicking the same chip again, or the "clear" link,
// resets it.
let selectedKeysetId: string | null = null;
// keysetId -> {rotating, revoked}, rebuilt from the Keysets table's
// data on every render so the History table's category filter (which
// only has a keysetId on each row, not the keyset's own metadata) can
// answer "does this event's keyset match the current tab?"
let keysetMeta = new Map<string, { rotating: boolean; revoked: boolean }>();

document.querySelectorAll<HTMLButtonElement>('.keyset-tabs .tab-btn[data-filter]').forEach((btn) => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.keyset-tabs .tab-btn').forEach((b) => b.classList.remove('active'));
    btn.classList.add('active');
    keysetFilter = btn.dataset.filter as KeysetFilter;
    // Switching category tabs clears any single-keyset drill-down --
    // the two are alternative ways of narrowing the view, not
    // additive, so there's never a confusing "Rotating tab, but also
    // pinned to one Static keyset" state.
    selectedKeysetId = null;
    updateSelectionBar();
    // Re-render immediately from the last known data instead of
    // waiting for the next status tick, so switching tabs feels
    // instant.
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

// Event delegation: chips are regenerated via innerHTML on every
// render, so listeners are attached once here, on the document, and
// matched by a data attribute rather than re-bound per chip.
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
    return raw; // show as-is if it isn't valid JSON (e.g. mid-write)
  }
}

function shortId(id: string | null | undefined): string {
  if (!id) return '—';
  return id.slice(0, 8) + '…';
}

// Stable per-keyset color: a small string hash mapped to an HSL hue.
// Same keysetId always produces the same color across the Keysets
// table and the History tab, which is what makes it possible to
// visually trace one keyset's rotations through an interleaved,
// 50-keyset feed ("colorful history tracing").
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

function statusBadge(status: string): string {
  switch (status) {
    case 'static':
      return `<span class="badge">static</span>`;
    case 'rotating':
      return `<span class="badge ok">rotating</span>`;
    case 'pending-renewal':
      return `<span class="badge warn">⚡ pending renewal</span>`;
    case 'renewed':
      return `<span class="badge ok">⚡ renewed</span>`;
    default:
      return `<span class="badge">${status}</span>`;
  }
}

function render(status: StatusResponse) {
  // Authoritative lock: driven by Postgres via status, not by "did my
  // own request succeed" -- safe against two tabs, a rapid
  // double-click, or this tab's own request still being in flight.
  if (status.initialized && !genesisLocked) {
    genesisLocked = true;
    genBtn.disabled = true;
    genBtn.textContent = 'Keysets already generated';
  }

  document.querySelector('#s-n')!.textContent = String(status.keysetCount);
  document.querySelector('#s-m')!.textContent = String(status.rotatingCount);
  document.querySelector('#s-l')!.textContent = String(status.revokedCount);
  document.querySelector('#s-static')!.textContent = String(status.staticCount);
  document.querySelector('#keysets-count-label')!.textContent = status.initialized
    ? `(${status.keysetCount} total)`
    : '';

  const rotatingKeysets = (status.keysets ?? []).filter((k) => k.rotating && k.nextRotationInSeconds !== undefined);
  const soonest = rotatingKeysets.length
    ? rotatingKeysets.reduce((min, k) => Math.min(min, k.nextRotationInSeconds!), Infinity)
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
    syncBadge.textContent = 'in sync — output file matches every keyset\'s current expiry';
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

  keysetMeta = new Map((status.keysets ?? []).map((k) => [k.keysetId, { rotating: k.rotating, revoked: k.revoked }]));

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

function applyKeysetFilter(keysets: StatusResponse['keysets'], filter: KeysetFilter): StatusResponse['keysets'] {
  switch (filter) {
    case 'rotating':
      return keysets.filter((k) => k.rotating);
    case 'revoked':
      return keysets.filter((k) => k.revoked);
    case 'static':
      return keysets.filter((k) => !k.rotating);
    default:
      return keysets;
  }
}

// Compact numbered index strip: N small colored swatches, each one
// functioning as an individual keyset's own "tab" -- click #7 and
// keyset #7 is all you see in both tables below, exactly like the
// chip-drilldown, just addressed by number instead of by finding its
// row first. Deliberately NOT N real <button> tabs in the .tabs bar
// above: at N=50 a literal tab-per-keyset bar wraps into several rows
// of unreadable text and gets worse on narrow screens, whereas a
// small square grid of numbers stays compact and scannable. Always
// shows every keyset regardless of the current category filter, so
// you can jump anywhere from anywhere.
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
      const kind = k.rotating ? (k.revoked ? 'rotating + revoked' : 'rotating') : 'static';
      return `<span
        class="keyset-chip keyset-index-chip${selected ? ' selected' : ''}"
        data-keysetid="${k.keysetId}"
        role="button" tabindex="0"
        title="${k.keysetId} — ${kind}"
        style="background:${color}22;color:${color};border:1px solid ${color}${selected ? 'ff' : '66'}"
      >${k.index}</span>`;
    })
    .join('');
}

function renderKeysetsTable(keysets: StatusResponse['keysets']) {
  lastKeysets = keysets;

  renderKeysetIndexStrip(keysets);

  document.querySelector('#count-all')!.textContent = `(${keysets.length})`;
  document.querySelector('#count-rotating')!.textContent = `(${keysets.filter((k) => k.rotating).length})`;
  document.querySelector('#count-revoked')!.textContent = `(${keysets.filter((k) => k.revoked).length})`;
  document.querySelector('#count-static')!.textContent = `(${keysets.filter((k) => !k.rotating).length})`;

  // A single-keyset drill-down (chip click) always wins over the
  // category tab -- it's a more specific request than "show me the
  // Rotating tab."
  const visible = selectedKeysetId
    ? keysets.filter((k) => k.keysetId === selectedKeysetId)
    : applyKeysetFilter(keysets, keysetFilter);

  const keysetsBody = document.querySelector('#keysets-table tbody')!;
  keysetsBody.innerHTML = visible
    .slice()
    .sort((a, b) => a.index - b.index)
    .map((k) => {
      // keysetColor() is a pure function of keysetId, so a given
      // keyset's color is identical here, in the History tab further
      // down, and across every filter tab -- filtering only changes
      // which ROWS are visible, never what color a keyset gets.
      const color = keysetColor(k.keysetId);
      const kind = k.rotating ? (k.revoked ? 'rotating + revoked' : 'rotating') : 'static';
      const expiresIn =
        k.rotating && k.nextRotationInSeconds !== undefined
          ? `${Math.max(0, Math.round(k.nextRotationInSeconds))}s`
          : '—';
      return `
      <tr style="border-left: 3px solid ${color}">
        <td>${k.index}</td>
        <td>${keysetChip(k.keysetId)}</td>
        <td class="muted small">${kind}</td>
        <td>${statusBadge(k.status)}</td>
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

// History rows respect the exact same filter state as the Keysets
// table above (category tab, or a single-keyset drill-down), using
// keysetMeta to look up each event's keyset's rotating/revoked flags
// since a rotation_events row itself only carries a keysetId.
function renderHistoryTable(history: StatusResponse['history']) {
  lastHistory = history;

  const visible = selectedKeysetId
    ? history.filter((e) => e.keysetId === selectedKeysetId)
    : history.filter((e) => {
        if (keysetFilter === 'all') return true;
        const meta = keysetMeta.get(e.keysetId);
        if (!meta) return true; // unknown keyset (e.g. metadata not loaded yet) -- don't hide it
        if (keysetFilter === 'rotating') return meta.rotating;
        if (keysetFilter === 'revoked') return meta.revoked;
        if (keysetFilter === 'static') return !meta.rotating;
        return true;
      });

  const historyBody = document.querySelector('#history-table tbody')!;
  historyBody.innerHTML = visible
    .map((e) => {
      const resultCls = e.applied ? 'badge ok' : 'badge fail';
      const resultLabel = e.applied
        ? 'applied'
        : e.reason.includes('not due') ||
            e.reason.includes('paused') ||
            e.reason.includes('lock held') ||
            e.reason.includes('already handled') ||
            e.reason.includes('no keyset due')
          ? 'skipped'
          : 'failed';
      const color = keysetColor(e.keysetId);
      return `
      <tr style="border-left: 4px solid ${color}">
        <td>${new Date(e.triggeredAt).toLocaleTimeString()}</td>
        <td>${keysetChip(e.keysetId)}</td>
        <td class="mono">${e.rotationId ? e.rotationId.slice(0, 10) + '…' : '—'}</td>
        <td class="mono">${shortId(e.fromKeyId)} → ${shortId(e.toKeyId)}</td>
        <td><span class="${resultCls}">${resultLabel}</span> <span class="muted small">${e.reason}</span></td>
      </tr>`;
    })
    .join('');

  if (visible.length === 0) {
    historyBody.innerHTML = `<tr><td colspan="5" class="muted small">No history rows match this filter yet.</td></tr>`;
  }
}


// Realtime updates via SSE (pushed roughly once per second by the
// backend's status broadcast loop).
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

// Polling fallback covers the brief window before the first SSE
// message arrives, and keeps working even if EventSource is blocked.
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
