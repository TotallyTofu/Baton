// The dashboard's core: polling the daemon, the header and alerts, tabs and
// panels, engine cards, the games drawer and the engine log drawer.
'use strict';
const $ = s => document.querySelector(s);
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const gib = mib => (mib / 1024).toFixed(mib >= 10240 ? 1 : 2);
const ago = t => { const s = Math.max(0, (Date.now() - new Date(t)) / 1000); if (s < 60) return Math.round(s) + 's'; if (s < 3600) return Math.round(s / 60) + 'm'; return (s / 3600).toFixed(1) + 'h'; };
// 54m05s, 2h03m: how long, the way the departures board shows it.
const span = s => { s = Math.max(0, Math.round(s)); if (s < 60) return s + 's'; if (s < 3600) return Math.floor(s / 60) + 'm' + String(s % 60).padStart(2, '0') + 's'; if (s < 86400) return Math.floor(s / 3600) + 'h' + String(Math.floor(s % 3600 / 60)).padStart(2, '0') + 'm'; return Math.floor(s / 86400) + 'd' + String(Math.floor(s % 86400 / 3600)).padStart(2, '0') + 'h'; };
const since = t => t && !String(t).startsWith('0001') ? span((Date.now() - Date.parse(t)) / 1000) : '';
const tok = n => n >= 1e6 ? (n / 1e6).toFixed(n >= 1e7 ? 0 : 1) + 'M' : n >= 1e3 ? (n / 1e3).toFixed(n >= 1e4 ? 0 : 1) + 'k' : String(Math.round(n || 0));
const clock = t => new Date(t).toLocaleTimeString([], {hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false});
const store = {get(k, d) { try { const v = localStorage.getItem('baton.' + k); return v === null ? d : JSON.parse(v); } catch (e) { return d; } }, set(k, v) { try { localStorage.setItem('baton.' + k, JSON.stringify(v)); } catch (e) {} }};

const KIND_COLOR = {tabby: 'var(--c-tabby)', unsloth: 'var(--c-unsloth)', comfyui: 'var(--c-comfyui)'};
const FIXED_COLOR = {game: 'var(--c-game)', stream: 'var(--c-stream)', guest: 'var(--c-guest)', hold: 'var(--c-hold)', system: 'var(--c-system)', apps: 'var(--c-other)', driver: 'var(--faint)', other: 'var(--c-system)'};
const MODES = {normal: ['Normal', 'var(--green)'], stream: ['Streaming', 'var(--c-stream)'], game: ['Game', 'var(--c-game)'], hold: ['Hold', 'var(--c-hold)']};
let view = null, kinds = {}, models = [], lastModels = 0, evSeq = 0, evList = [];

function colorOf(key) {
  if (kinds[key]) return KIND_COLOR[kinds[key]] || 'var(--cyan)';
  return KIND_COLOR[key] || FIXED_COLOR[key] || 'var(--c-other)';
}
function labelOf(key) {
  return {apps: 'other apps', driver: 'driver / unattributed', system: 'Windows', other: 'everything else', guest: 'guests'}[key] || key;
}

async function post(path, body) {
  try {
    const r = await fetch(path, {method: 'POST', headers: {'X-Baton': 'dashboard', 'Content-Type': 'application/json'}, body: JSON.stringify(body || {})});
    if (!r.ok) { const j = await r.json().catch(() => ({})); toast(j.error || r.statusText, {err: true}); }
  } catch (e) { toast(String(e), {err: true}); }
  setTimeout(poll, 150);
}

// --- tabs, section rail, folding panels ---
const PAGES = ['harbor', 'engines'];
let page = new URLSearchParams(location.search).get('tab') || store.get('tab', 'harbor');
function showPage(p, focus) {
  if (!PAGES.includes(p)) p = 'harbor';
  page = p;
  store.set('tab', p);
  for (const x of PAGES) {
    $('#tab-' + x).setAttribute('aria-selected', String(x === p));
    $('#page-' + x).hidden = x !== p;
  }
  if (focus) $('#tab-' + p).focus();
  window.scrollTo({top: 0});
  if (p === 'harbor') pollHarbor(); else pollRequests();
  if (view) renderAll(view);
}
for (const p of PAGES) $('#tab-' + p).onclick = () => showPage(p);
$('.tabs').addEventListener('keydown', ev => {
  if (ev.key !== 'ArrowRight' && ev.key !== 'ArrowLeft') return;
  showPage(PAGES[(PAGES.indexOf(page) + (ev.key === 'ArrowRight' ? 1 : PAGES.length - 1)) % PAGES.length], true);
});

const folded = new Set(store.get('folded', []));
const CHEV = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg>';
document.querySelectorAll('.panel').forEach(p => {
  const head = p.querySelector('.panel-head');
  const b = document.createElement('button');
  b.className = 'fold'; b.type = 'button'; b.innerHTML = CHEV;
  b.setAttribute('aria-label', 'Fold ' + (p.getAttribute('aria-label') || 'panel'));
  head.appendChild(b);
  if (folded.has(p.id)) p.classList.add('folded');
  b.setAttribute('aria-expanded', String(!folded.has(p.id)));
  b.onclick = () => {
    const f = p.classList.toggle('folded');
    f ? folded.add(p.id) : folded.delete(p.id);
    b.setAttribute('aria-expanded', String(!f));
    store.set('folded', [...folded]);
    if (!f) setTimeout(() => view && renderAll(view), 0);
  };
});
document.querySelectorAll('.subnav').forEach(nav => nav.addEventListener('click', ev => {
  const a = ev.target.closest('a');
  if (!a) return;
  ev.preventDefault();
  const t = document.querySelector(a.getAttribute('href'));
  if (t) t.scrollIntoView({behavior: reduceMotion ? 'auto' : 'smooth', block: 'start'});
}));
// Mark the section in view on the rail.
const spy = new IntersectionObserver(es => {
  for (const e of es) if (e.isIntersecting) {
    const nav = e.target.closest('[role=tabpanel]').querySelector('.subnav');
    nav.querySelectorAll('a').forEach(a => a.classList.toggle('on', a.getAttribute('href') === '#' + e.target.id));
  }
}, {rootMargin: '-35% 0px -60% 0px'});
document.querySelectorAll('main .panel').forEach(p => spy.observe(p));
const setTopH = () => document.documentElement.style.setProperty('--top-h', $('#top').offsetHeight + 'px');
new ResizeObserver(setTopH).observe($('#top'));

// --- header and alerts ---
function renderHeader(v) {
  const [name, color] = MODES[v.mode] || [v.mode, 'var(--muted)'];
  $('#modePill').style.color = color;
  $('#modeName').textContent = name;
  let why = v.mode_why ? v.mode_why : (v.holder ? v.holder + ' holds the GPUs' : 'no engine holds the GPUs');
  if (v.mode !== 'normal') why += ' · ' + ago(v.mode_since);
  if (v.guest) why += ' · guest ' + v.guest.why + ' has the GPUs';
  if (v.restore_to) why += ' · restores ' + v.restore_to + ' after';
  $('#modeWhy').textContent = why;
  $('#modeWhy').title = why;
  const holdBtn = v.mode === 'hold'
    ? `<button class="warn" onclick="post('/api/unhold')">Release hold</button>`
    : `<button onclick="hold()">Hold GPUs</button>`;
  const html = `<button class="btn-icon" onclick="openGames()">${ICON.pad}Games</button>` + holdBtn + `<button onclick="if(confirm('Release every engine from both GPUs?'))post('/api/free')">Free all</button>`;
  if ($('#topActions').innerHTML !== html) $('#topActions').innerHTML = html;
  $('#nEngines').textContent = (v.engines || []).filter(e => e.up).length;
  // On the board: changes under way or queued, and whatever is on the cards.
  $('#nHarbor').textContent = (v.op ? 1 : 0) + (v.queue || []).length + (v.hits || []).length + (v.hold ? 1 : 0)
    + (v.engines || []).filter(e => e.loaded || e.loading || e.aground).length;
}

function hold() {
  const r = prompt('Reserve both GPUs. Reason?', 'gaming');
  if (r === null) return;
  post('/api/hold', {reason: r});
}

function renderAlerts(v) {
  const out = [];
  if (v.op) out.push(`<div class="alert op"><span class="spin"></span><span class="tag">#${String(v.op.no || 0).padStart(4, '0')}</span><span class="msg"><b>${esc(v.op.name)}</b>${v.op.stage ? ' · ' + esc(v.op.stage) : ''}${v.queue && v.queue.length ? ` · ${v.queue.length} queued` : ''}</span><span class="when">${ago(v.op.since)}</span></div>`);
  for (const e of v.engines || []) if (e.aground) {
    out.push(`<div class="alert err"><span class="tag">AGROUND</span><span class="msg">${aground(e.name, e.aground)}</span><span class="actions"><button class="warn" data-n="${esc(e.name)}" onclick="reclaim(this.dataset.n)">Force stop</button></span><span class="when">since ${clock(e.aground.since)}</span></div>`);
  }
  if (v.mode === 'game' || v.mode === 'hold') out.push(`<div class="alert warn"><span class="tag">${v.mode.toUpperCase()}</span><span class="msg">${esc(v.mode_why || '')}: every engine is off the cards${v.restore_to ? '; ' + esc(v.restore_to) + ' comes back after' : ''}.</span><span class="when">since ${clock(v.mode_since)}</span></div>`);
  else if (v.guest) out.push(`<div class="alert warn"><span class="tag">GUEST</span><span class="msg"><b>${esc(v.guest.why)}</b> is using the cards: the LLMs stay off until it is done${v.restore_to ? '; ' + esc(v.restore_to) + ' comes back after' : ''}.</span><span class="when">since ${clock(v.guest.since)}</span></div>`);
  if (v.error) out.push(`<div class="alert err"><span class="tag">ERROR</span><span class="msg">${esc(v.error)}</span></div>`);
  const html = out.join('');
  if ($('#alerts').innerHTML !== html) $('#alerts').innerHTML = html;
}

// Memory an engine kept after its release: nothing else starts on those GPUs
// until it lets go or is force-stopped.
function aground(name, a) {
  const per = Object.entries(a.mib || {}).map(([g, m]) => `${gib(m)} GiB on GPU ${esc(g)}`).join(', ') || gib(a.total_mib) + ' GiB';
  return `<b>${esc(name)}</b> still holds ${per} after its release; nothing else starts there until it lets go.`;
}

function reclaim(name) {
  if (confirm(`Force stop ${name}? Its server is ended, whatever its release mode, so the memory it kept is freed.`)) post(`/api/engines/${encodeURIComponent(name)}/reclaim`);
}

// --- engines ---
function engState(e) {
  if (e.aground) return ['Aground', 'st-err'];
  if (e.error && !e.up && !e.loading) return ['Error', 'st-err'];
  if (e.loading) return ['Loading', 'st-load'];
  if (e.holder && (e.loaded || e.up)) return ['Holds GPUs', 'st-hold'];
  if (e.loaded) return ['Loaded', 'st-up'];
  if (e.up) return [e.llm ? 'Up, no model' : 'Running', 'st-up'];
  return ['Stopped', 'st-off'];
}

// One line on an engine card about TabbyAPI's disk KV cache, and its warning.
function diskLine(dk) {
  const bits = [];
  if (dk.active) bits.push(`${tok(dk.reusable_tokens)} reusable at load` + (dk.restored_tokens ? ` · ${tok(dk.restored_tokens)} restored` : ''));
  else if (dk.active === false) bits.push('not used by this load, takes effect on the next start');
  if (dk.used_mib !== undefined) bits.push(`${gib(dk.used_mib)}${dk.budget_mib ? ' / ' + gib(dk.budget_mib) : ''} GiB`);
  const f = dk.last_flush;
  if (f) bits.push(f.timed_out ? `last save cut off after ${f.secs}s` : f.files ? `last save ${Math.round(f.wrote_mib)} MB in ${f.secs}s` : 'nothing left to save at the last stop');
  return (bits.length ? `<div class="sub" title="${esc(dk.dir || '')}">disk KV cache · ${bits.join(' · ')}</div>` : '')
    + (dk.note ? `<div class="sub warn">${esc(dk.note)}</div>` : '');
}

function renderEngines(v) {
  const busyOp = !!v.op;
  const html = (v.engines || []).map(e => {
    const [st, cls] = engState(e);
    const vram = (Object.entries(e.vram_mib || {}).filter(([, m]) => m > 0).map(([g, m]) => `GPU${g} ${gib(m)} GiB`).join(' · ') || 'no VRAM')
      + (e.ram_mib >= 64 ? ` · RAM ${gib(e.ram_mib)} GiB` : '');
    const bits = [];
    if (e.busy) bits.push('busy: ' + esc(e.busy_why || 'working'));
    if (e.inflight) bits.push(e.inflight + ' gateway request(s)');
    if (e.util >= 1) bits.push('load ' + Math.round(e.util) + '%');
    if (e.kind === 'comfyui' && (v.gate.waiting || v.gate.running)) bits.push(`gate: ${v.gate.running} running, ${v.gate.waiting} waiting`);
    if (e.restore) bits.push('comes back after the ' + (v.mode === 'normal' ? 'current job' : v.mode));
    const m = e.extra && e.extra.last;
    const rates = m ? `<div class="rates" title="Last request: ${esc(m.request)} at ${esc(m.at)}"><div><span>DECODE</span><b>${m.decode_tps || '—'}<small>t/s</small></b></div><div><span>PREFILL</span><b>${m.prompt_tps ? Math.round(m.prompt_tps).toLocaleString() : '—'}<small>t/s</small></b></div></div>
      <div class="sub">last request ${esc(m.at)} · ${m.tokens.toLocaleString()} out · ${m.prompt_tokens.toLocaleString()} in, ${m.cached_pct}% cached${m.draft_total ? ` · draft ${m.draft_pct}%` : ''}</div>` : '';
    const activeLabel = e.kind === 'comfyui' ? 'Give GPUs' : 'Use';
    return `<div class="eng ${e.holder ? 'holder' : ''} ${e.aground ? 'aground' : ''}">
      <div class="eng-h"><span class="sw" style="background:${KIND_COLOR[e.kind] || 'var(--cyan)'}"></span><b>${esc(e.name)}</b><span class="kind">${esc(e.kind)} · GPU ${esc((e.gpus || []).join(','))}</span><span class="pill ${cls}"><span class="d"></span>${st}</span></div>
      <div class="model" title="${esc(e.model)}">${esc(e.model || '—')}</div>
      ${rates}
      <div class="sub">${vram}${e.detail ? ' · ' + esc(e.detail) : ''}</div>
      ${e.extra && e.extra.disk ? diskLine(e.extra.disk) : ''}
      ${bits.length ? `<div class="sub">${bits.join(' · ')}</div>` : ''}
      ${e.aground ? `<div class="errtxt">${aground(e.name, e.aground).replace(/<\/?b>/g, '')}</div>` : ''}
      ${e.error ? `<div class="errtxt">${esc(e.error)}</div>` : ''}
      <div class="actions">
        <button class="primary" ${busyOp || (e.holder && e.loaded) ? 'disabled' : ''} onclick="post('/api/engines/${encodeURIComponent(e.name)}/activate')">${activeLabel}</button>
        <button ${!(e.loaded || e.loading || (e.pids || []).length) ? 'disabled' : ''} onclick="post('/api/engines/${encodeURIComponent(e.name)}/release')">Release</button>
        ${e.aground ? `<button class="warn" data-n="${esc(e.name)}" onclick="reclaim(this.dataset.n)">Force stop</button>` : ''}
        ${e.has_log || e.kind === 'tabby' ? `<button data-n="${esc(e.name)}" onclick="openLog(this.dataset.n)">Log</button>` : ''}
      </div>
    </div>`;
  }).join('') || '<div class="empty-note">No engines configured.</div>';
  if ($('#engines').innerHTML !== html) $('#engines').innerHTML = html;
  const up = (v.engines || []).filter(e => e.up).length;
  $('#engMeta').textContent = `${(v.engines || []).length} engines · ${up} up · what they report about themselves`;
}

function renderGateway(v) {
  const gw = v.gateway || {};
  const url = gw.enabled ? `http://${gw.listen}/v1` : 'disabled';
  const ms = models.map(m => `<code>${esc(m.id)}</code>`).join(' ') || '<span class="empty">none</span>';
  const p = v.policy || {};
  const rm = v.remote || {};
  let phone;
  if (!rm.enabled) phone = '<span class="empty">off · set <code>[remote] tailscale = true</code> in baton.toml</span>';
  else if (!rm.ip) phone = `<span class="empty">waiting for Tailscale${rm.error ? ' · ' + esc(rm.error) : ''}</span>`;
  else phone = `<code>${esc(rm.dashboard)}</code>` + (rm.gateway ? `<br><span class="empty">gateway</span> <code>${esc(rm.gateway)}</code>` : '');
  const html = `<div class="kv">
    <span>Base URL</span><span><code>${esc(url)}</code></span>
    <span>On your phone</span><span>${phone}</span>
    <span>API key</span><span><code>baton key</code> prints it</span>
    <span>Models</span><span>${ms}</span>
    <span>Game</span><span>frees every GPU (running jobs get ${esc(p.game_drain)}), ${p.restore ? 'restores after' : 'no restore'}</span>
    <span>Stream</span><span>${p.stream_hold_comfyui ? 'ComfyUI held, ' : ''}LLM kept while GPU ${esc(p.stream_encode_gpu)} ≥ ${esc(p.stream_min_free_mib)} MiB free</span>
  </div>`;
  if ($('#gw').innerHTML !== html) $('#gw').innerHTML = html;
}

// --- engine log drawer ---
let logName = null, logNext = -1, logTimer = null, logPrevCls = '', logLines = 0;

function logLine(l) {
  let cls = '';
  if (/^\s{10,}\S/.test(l) && (logPrevCls === 'metric' || logPrevCls === 'err' || logPrevCls === 'warn')) cls = logPrevCls;
  else if (/^=====/.test(l)) cls = 'sep';
  else if (/\bERROR\b|Traceback \(most recent|^\w*(Error|Exception)\b/.test(l)) cls = 'err';
  else if (/\bWARNING\b/.test(l)) cls = 'warn';
  else if (/tokens generated at/.test(l)) cls = 'metric';
  logPrevCls = cls;
  const m = l.match(/^(\d\d:\d\d:\d\d(?:\.\d+)?)(\s+)([A-Z]+:)(.*)$/);
  const body = m ? `<span class="ts">${m[1]}</span>${m[2]}<span class="lvl">${m[3]}</span>${esc(m[4])}` : (esc(l) || ' ');
  return `<span class="ln ${cls}">${body}</span>`;
}

function renderStats(m) {
  const box = $('#logStats');
  if (!m) { box.innerHTML = ''; return; }
  const tile = (k, v, unit, sub) => `<div class="tile"><div class="k">${k}</div><div class="v">${v}<small>${unit}</small></div><div class="s">${sub}</div></div>`;
  box.innerHTML =
    tile('Decode', m.decode_tps || '—', m.decode_tps ? 'T/s' : '', `${m.tokens.toLocaleString()} tokens`) +
    tile('Prompt', m.prompt_tps ? Math.round(m.prompt_tps).toLocaleString() : '—', m.prompt_tps ? 'T/s' : '', `${m.prompt_tokens.toLocaleString()} tokens · ${m.cached_pct}% cached`) +
    tile('Draft accepted', m.draft_total ? m.draft_pct : '—', m.draft_total ? '%' : '', m.draft_total ? `${m.draft_accepted} of ${m.draft_total} (MTP)` : 'no drafting') +
    tile('First token', m.first_token_s ? m.first_token_s : '—', m.first_token_s ? 's' : '', m.total_s ? `total ${m.total_s} s` : '') +
    `<div class="strip-cap">Last request: ${esc(m.request)} at ${esc(m.at)}</div>`;
}

async function pollLog() {
  if (!logName) return;
  const pre = $('#logPre');
  try {
    const r = await (await fetch(`/api/engines/${encodeURIComponent(logName)}/log?from=${logNext}`, {cache: 'no-store'})).json();
    if (!r.exists) {
      pre.innerHTML = `<div class="none">No log yet. Baton saves ${esc(logName)}'s output here whenever it starts ${esc(logName)} (the Use button, the gateway, or after a game). If you start it yourself, or set <code>show_window = true</code> in baton.toml, the output stays in its own console window.</div>`;
      $('#logSub').textContent = r.path || '';
      renderStats(null);
      return;
    }
    if (logNext >= 0 && r.size < logNext) { logNext = -1; pre.innerHTML = ''; logLines = 0; return pollLog(); }
    $('#logSub').textContent = r.path;
    if (r.last !== undefined) renderStats(r.last);
    if (r.text) {
      const atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
      if (logNext < 0) { pre.innerHTML = ''; logLines = 0; logPrevCls = ''; }
      const lines = r.text.replace(/\n$/, '').split('\n');
      pre.insertAdjacentHTML('beforeend', lines.map(logLine).join(''));
      logLines += lines.length;
      while (logLines > 6000 && pre.firstChild) { pre.firstChild.remove(); logLines--; }
      if (atBottom || logNext < 0) pre.scrollTop = pre.scrollHeight; else $('#logJump').hidden = false;
    }
    logNext = r.next;
  } catch (e) {}
}

function openLog(name) {
  closeGames();
  logName = name; logNext = -1; logLines = 0; logPrevCls = '';
  $('#logTitle').textContent = name + ' log';
  $('#logPre').innerHTML = '';
  $('#logStats').innerHTML = '';
  $('#logJump').hidden = true;
  drawerOpener = document.activeElement;
  $('#scrim').hidden = $('#logDrawer').hidden = false;
  requestAnimationFrame(() => { $('#scrim').classList.add('on'); $('#logDrawer').classList.add('on'); });
  document.body.style.overflow = 'hidden';
  pollLog();
  logTimer = setInterval(pollLog, 1000);
  setTimeout(() => $('#logClose').focus(), 60);
}

function closeLog() {
  if (!logName) return;
  logName = null;
  clearInterval(logTimer);
  $('#scrim').classList.remove('on'); $('#logDrawer').classList.remove('on');
  setTimeout(() => { if (!logName && !drawerOpen) $('#scrim').hidden = true; if (!logName) $('#logDrawer').hidden = true; }, 260);
  document.body.style.overflow = '';
  if (drawerOpener && drawerOpener.focus) drawerOpener.focus();
}

$('#logClose').onclick = closeLog;
$('#logJump').onclick = () => { const p = $('#logPre'); p.scrollTop = p.scrollHeight; $('#logJump').hidden = true; };
$('#logPre').addEventListener('scroll', () => { const p = $('#logPre'); if (p.scrollHeight - p.scrollTop - p.clientHeight < 40) $('#logJump').hidden = true; });
$('#logNotepad').onclick = async () => {
  try { await gapi(`/api/engines/${encodeURIComponent(logName)}/log/open`); } catch (e) { toast(e.message, {err: true}); }
};
document.addEventListener('keydown', ev => { if (logName && ev.key === 'Escape') closeLog(); });
if (location.hash.startsWith('#log=')) setTimeout(() => openLog(decodeURIComponent(location.hash.slice(5))), 0);

// --- games drawer ---
const ICON = {
  pad: '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 11h4M8 9v4M15 12h.01M18 10h.01"/><path d="M17.3 5H6.7a4 4 0 0 0-3.96 3.43l-.73 5.1A3 3 0 0 0 7.2 16.1L9 14h6l1.8 2.1a3 3 0 0 0 5.2-2.57l-.74-5.1A4 4 0 0 0 17.3 5z"/></svg>',
  folder: '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round" aria-hidden="true"><path d="M3 7.5A2.5 2.5 0 0 1 5.5 5H9l2 2.2h7.5A2.5 2.5 0 0 1 21 9.7v7.8a2.5 2.5 0 0 1-2.5 2.5h-13A2.5 2.5 0 0 1 3 17.5z"/></svg>',
  app: '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4.5" width="18" height="15" rx="2.5"/><path d="M3 9h18M6.5 6.8h.01M9 6.8h.01"/></svg>',
  ban: '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" aria-hidden="true"><circle cx="12" cy="12" r="8.5"/><path d="M6 18L18 6"/></svg>',
  x: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>',
  plus: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>',
};
const PRETTY = {steam: 'Steam', epic: 'Epic Games', gog: 'GOG', xbox: 'Xbox', ea: 'EA app', ubisoft: 'Ubisoft Connect', emulator: 'Emulators'};
const LIST_NAME = {folder: 'library folders', game: 'games', ignore: 'never count'};
let drawerOpen = false, drawerOpener = null, gamesTimer = null, gamesSig = '';

function splitPath(p) {
  const i = p.lastIndexOf('\\');
  if (i < 0 || p.length <= 3) return [p, ''];
  return [p.slice(i + 1), p.slice(0, i + 1)];
}
const gRow = (icon, name, sub, act) => `<div class="row"><span class="ic">${icon}</span><div class="tx"><div class="nm">${name}</div>${sub ? `<div class="pt" title="${esc(sub)}"><bdi>${esc(sub)}</bdi></div>` : ''}</div><div class="act">${act || ''}</div></div>`;
// A readable name for a running game: its folder under the library or
// launcher, not the shipping exe ("Stellar Blade", not "SB-Win64-Shipping").
function gameTitle(h) {
  const p = h.path || '';
  if (h.match && p.toLowerCase().startsWith(h.match.toLowerCase() + '\\')) {
    const t = p.slice(h.match.length + 1).split('\\')[0];
    if (t && !/\.exe$/i.test(t)) return t;
  }
  const m = p.match(/\\(?:steamapps\\common|Epic Games|XboxGames|GOG Games|GOG Galaxy\\Games|EA Games)\\([^\\]+)/i);
  if (m) return m[1];
  return (h.exe || '').replace(/\.exe$/i, '');
}
const liveTag = t => `<span class="livetag"><i></i>${t}</span>`;

function listRows(entries, kind, icon, empty) {
  if (!entries.length) return `<div class="empty-row">${empty}</div>`;
  return '<div class="rows">' + entries.map(e => {
    const [n, parent] = splitPath(e.path);
    const name = esc(kind === 'game' ? n.replace(/\.exe$/i, '') : n) + (e.running ? liveTag('running') : '');
    return gRow(icon, name, parent || e.path, `<button class="icon-btn rm" title="Remove" aria-label="Remove ${esc(n)}" data-rm="${kind}" data-path="${esc(e.path)}">${ICON.x}</button>`);
  }).join('') + '</div>';
}

function section(title, count, desc, body) {
  return `<section class="sec"><div class="sec-h"><h4>${title}</h4>${count ? `<span class="count">${count}</span>` : ''}</div>${desc ? `<p class="sec-d">${desc}</p>` : ''}${body}</section>`;
}

function renderGames(g) {
  const sig = JSON.stringify(g);
  if (sig === gamesSig) return;
  gamesSig = sig;
  const input = $('#drBody input[name=p]');
  const keep = input ? {v: input.value, f: document.activeElement === input, s: input.selectionStart} : null;
  let h = '';
  if (g.running.length) h += section('Playing now', 0, '', '<div class="rows">' + g.running.map(r =>
    gRow(ICON.pad, esc(gameTitle(r)), r.exe, liveTag('GPUs free'))).join('') + '</div>');
  if (g.suggestions.length) h += section('Using your GPU', 0, 'Holding VRAM, but not a game Baton knows. Is one of these a game?',
    '<div class="sugg"><div class="rows">' + g.suggestions.map(s => gRow(ICON.app, esc(s.name), s.path,
      `<span class="tag-chip">${gib(s.mib)} GiB</span><button class="mini primary" data-add="game" data-path="${esc(s.path)}">Add game</button><button class="mini" data-add="ignore" data-path="${esc(s.path)}">Ignore</button>`)).join('') + '</div></div>');
  h += section('Library folders', g.folders.length, 'Every program inside counts as a game.',
    listRows(g.folders, 'folder', ICON.folder, 'No folders yet.') +
    `<div class="adders"><button class="ghost btn-icon" data-pick="folder">${ICON.plus}Add folder…</button></div>
     <form class="paste" data-paste><input name="p" placeholder="or paste a folder or .exe path" autocomplete="off" spellcheck="false" aria-label="Folder or program path"><button>Add</button></form>`);
  h += section('Single games', g.games.length, 'One program, wherever it is installed.',
    listRows(g.games, 'game', ICON.app, 'None yet.') +
    `<div class="adders"><button class="ghost btn-icon" data-pick="game">${ICON.plus}Add game…</button></div>`);
  h += section('Never count', g.ignore.length, 'Launchers and tools that live next to your games.',
    listRows(g.ignore, 'ignore', ICON.ban, 'Nothing excluded.') +
    `<div class="adders"><button class="ghost btn-icon" data-pick="ignore-folder">${ICON.plus}Folder…</button><button class="ghost btn-icon" data-pick="ignore-game">${ICON.plus}Program…</button></div>`);
  h += `<section class="sec"><div class="sec-h"><h4>Built-in launchers</h4><button class="switch" role="switch" aria-checked="${g.builtin.on}" aria-label="Find games installed by launchers" data-builtin></button></div>
    <p class="sec-d">Games installed by these are found on their own.</p>
    <div class="chips ${g.builtin.on ? '' : 'off'}">${g.builtin.names.map(n => `<span class="chip">${esc(PRETTY[n] || n)}</span>`).join('')}</div></section>`;
  $('#drBody').innerHTML = h;
  $('#drFoot').innerHTML = `Saved in <code>${esc(splitPath(g.file)[0])}</code> next to baton.toml · changes apply right away`;
  if (keep) {
    const i = $('#drBody input[name=p]');
    i.value = keep.v;
    if (keep.f) { i.focus(); try { i.setSelectionRange(keep.s, keep.s); } catch (e) {} }
  }
}

async function loadGames() {
  try { renderGames(await (await fetch('/api/games', {cache: 'no-store'})).json()); } catch (e) {}
}

async function gapi(path, body) {
  const r = await fetch(path, {method: 'POST', headers: {'X-Baton': 'dashboard', 'Content-Type': 'application/json'}, body: JSON.stringify(body || {})});
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}

let toastTimer;
function toast(msg, opts = {}) {
  const t = $('#toast');
  t.className = 'toast on' + (opts.err ? ' err' : '');
  t.innerHTML = `<span>${esc(msg)}</span>` + (opts.undo ? '<button type="button">Undo</button>' : '');
  if (opts.undo) t.querySelector('button').onclick = () => { t.className = 'toast'; opts.undo(); };
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.className = 'toast'; }, opts.undo ? 6000 : 3500);
}

async function changed() { gamesSig = ''; await loadGames(); setTimeout(poll, 100); }

function short(p) { const [n] = splitPath(p); return n.replace(/\.exe$/i, ''); }

$('#drBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button');
  if (!b) return;
  try {
    if (b.dataset.pick) {
      b.disabled = true;
      const foot = $('#drFoot'), was = foot.innerHTML;
      foot.innerHTML = '<span style="display:inline-flex;gap:8px;align-items:center"><span class="spin"></span>Choose in the Windows dialog… (it may open behind this window)</span>';
      try {
        const r = await gapi('/api/games/pick', {kind: b.dataset.pick});
        if (!r.cancelled) toast(`Added ${short(r.path)} to ${LIST_NAME[r.kind]}`);
      } finally { foot.innerHTML = was; b.disabled = false; }
      await changed();
    } else if (b.dataset.add) {
      const kind = b.dataset.add, path = b.dataset.path;
      await gapi('/api/games/add', {kind, path});
      toast(kind === 'game' ? `${short(path)} now counts as a game` : `Baton will ignore ${short(path)}`, {undo: async () => { await gapi('/api/games/remove', {kind, path}); changed(); }});
      await changed();
    } else if (b.dataset.rm) {
      const kind = b.dataset.rm, path = b.dataset.path;
      await gapi('/api/games/remove', {kind, path});
      toast(`Removed ${short(path)}`, {undo: async () => { await gapi('/api/games/add', {kind, path}); changed(); }});
      await changed();
    } else if (b.dataset.builtin !== undefined) {
      const on = b.getAttribute('aria-checked') !== 'true';
      b.setAttribute('aria-checked', on);
      await gapi('/api/games/builtin', {on});
      await changed();
    }
  } catch (e) { toast(e.message, {err: true}); await changed(); }
});

$('#drBody').addEventListener('submit', async ev => {
  ev.preventDefault();
  const input = ev.target.querySelector('input');
  const path = input.value.trim();
  if (!path) return;
  try {
    const r = await gapi('/api/games/add', {kind: 'auto', path});
    input.value = '';
    toast(`Added ${short(r.path)} to ${LIST_NAME[r.kind]}`);
    await changed();
  } catch (e) { toast(e.message, {err: true}); }
});

function openGames() {
  if (drawerOpen) return;
  drawerOpen = true;
  drawerOpener = document.activeElement;
  $('#scrim').hidden = $('#drawer').hidden = false;
  requestAnimationFrame(() => { $('#scrim').classList.add('on'); $('#drawer').classList.add('on'); });
  document.body.style.overflow = 'hidden';
  if (location.hash !== '#games') history.replaceState(null, '', '#games');
  gamesSig = '';
  loadGames();
  gamesTimer = setInterval(loadGames, 3000);
  setTimeout(() => $('#drClose').focus(), 60);
}

function closeGames() {
  if (!drawerOpen) return;
  drawerOpen = false;
  $('#scrim').classList.remove('on'); $('#drawer').classList.remove('on');
  setTimeout(() => { if (!drawerOpen) $('#scrim').hidden = $('#drawer').hidden = true; }, 260);
  document.body.style.overflow = '';
  clearInterval(gamesTimer);
  if (location.hash === '#games') history.replaceState(null, '', location.pathname);
  if (drawerOpener && drawerOpener.focus) drawerOpener.focus();
}

$('#scrim').onclick = () => { closeGames(); closeLog(); };
$('#drClose').onclick = closeGames;
document.addEventListener('keydown', ev => {
  if (!drawerOpen) return;
  if (ev.key === 'Escape') { closeGames(); return; }
  if (ev.key === 'Tab') { // keep focus inside the drawer
    const f = [...$('#drawer').querySelectorAll('button:not([disabled]),input')].filter(x => x.offsetParent);
    if (!f.length) return;
    if (ev.shiftKey && document.activeElement === f[0]) { ev.preventDefault(); f[f.length - 1].focus(); }
    else if (!ev.shiftKey && document.activeElement === f[f.length - 1]) { ev.preventDefault(); f[0].focus(); }
  }
});
window.addEventListener('hashchange', () => location.hash === '#games' ? openGames() : closeGames());
if (location.hash === '#games') openGames();

// --- polling ---
function renderAll(v) {
  renderHeader(v); renderAlerts(v);
  if (page === 'harbor') { renderGPUs(v); renderMem(v); renderHits(v); renderStow(v); renderEvents(v); }
  else { renderEngines(v); renderPools(v); renderGateway(v); }
}

async function poll() {
  clearTimeout(poll.t);
  try {
    const r = await fetch('/api/status?since=' + evSeq, {cache: 'no-store'});
    view = await r.json();
    kinds = {};
    for (const e of view.engines || []) kinds[e.name] = e.kind;
    for (const e of view.events || []) if (e.seq > evSeq) { evList.push(e); evSeq = e.seq; }
    if (evList.length > 400) evList = evList.slice(-400);
    if (Date.now() - lastModels > 10000) {
      lastModels = Date.now();
      models = await (await fetch('/api/models')).json() || [];
    }
    renderAll(view);
    $('#conn').className = 'conn';
    $('#conn span').textContent = 'live';
  } catch (e) {
    $('#conn').className = 'conn off';
    $('#conn span').textContent = 'daemon offline';
  }
  poll.t = setTimeout(poll, 1500);
}
let resizeT;
window.addEventListener('resize', () => { clearTimeout(resizeT); resizeT = setTimeout(() => { if (view) renderAll(view); redrawCharts(); }, 150); });
