// The engines and requests page: the KV pool (live, from the status), and
// throughput, the token ledger, the leaderboard, recent requests and context
// residency from the request history.
'use strict';
let rv = null, rRange = store.get('rrange', '24h'), lbSort = store.get('lbsort', 'requests');

async function pollRequests() {
  clearTimeout(pollRequests.t);
  if (page === 'engines' && !document.hidden) {
    try {
      rv = await (await fetch('/api/requests?range=' + rRange, {cache: 'no-store'})).json();
      renderRequests(rv);
    } catch (e) {}
  }
  pollRequests.t = setTimeout(pollRequests, 5000);
}
pressRange($('#rRange'), rRange);
$('#rRange').onclick = ev => {
  const b = ev.target.closest('button');
  if (!b) return;
  rRange = b.dataset.r; store.set('rrange', rRange); pressRange($('#rRange'), rRange); pollRequests();
};

function renderRequests(r) {
  renderThroughput(r); renderLedger(r); renderLeaderboard(r); renderRecent(r); renderResidency(r);
}

// --- KV pool ---

// TabbyAPI's disk KV cache, under its pool: how full the directory is, what
// the running TabbyAPI found there at load and has restored and saved since
// (from its log), and how the last save before a stop went.
function diskBlock(dk) {
  if (!dk) return '';
  const has = dk.used_mib !== undefined;
  const pct = has && dk.budget_mib ? Math.min(100, dk.used_mib / dk.budget_mib * 100) : 0;
  const f = dk.last_flush;
  const last = f ? (f.timed_out ? `cut at ${f.secs}s` : f.files ? `${Math.round(f.wrote_mib).toLocaleString()} MB` : 'none due') : '—';
  const lastTip = f ? `${f.timed_out ? `gave up after ${f.secs}s with ${f.writing} write(s) open` : `${f.files} file(s), ${f.wrote_mib} MB written; waited ${f.secs}s`} · ${new Date(f.at).toLocaleString()}` : 'no stop has waited for it yet';
  const figs = dk.active
    ? [[tok(dk.reusable_tokens), 'reusable at load', 'prompt tokens TabbyAPI found on disk when it loaded'],
       [tok(dk.restored_tokens || 0), 'restored', `read back from disk since it started (${dk.restored_checkpoints || 0} checkpoints)`],
       [tok(dk.saved_tokens || 0), 'saved', `${dk.saved_mib || 0} MB written since it started`],
       [last, 'last save', lastTip]]
    : [[has ? gib(dk.used_mib) + ' GiB' : '—', 'on disk', dk.dir || ''],
       [dk.budget_mib ? gib(dk.budget_mib) + ' GiB' : '—', 'budget', 'disk_cache_size'],
       [last, 'last save', lastTip]];
  const note = dk.active ? 'from TabbyAPI\'s log since it started; restored tokens skip prefill'
    : dk.active === false ? 'set in config.yml, but this TabbyAPI loaded without it: it takes effect on the next start'
    : 'kept on disk for the next start';
  return `<div class="disk-tier">
    <div class="pool-h"><span class="lbl">DISK TIER</span><span class="r" title="${esc(dk.dir || '')}">${has ? `${gib(dk.used_mib)}${dk.budget_mib ? ' / ' + gib(dk.budget_mib) : ''} GiB` : esc(dk.dir || '')}</span></div>
    ${dk.budget_mib && has ? `<div class="kvbar thin" role="img" aria-label="${gib(dk.used_mib)} of ${gib(dk.budget_mib)} GiB of the disk cache budget used"><span class="disk" style="width:${pct.toFixed(1)}%"></span></div>` : ''}
    <div class="figs">${figs.map(([b, s, t]) => `<div title="${esc(t)}"><b>${esc(b)}</b><span>${s}</span></div>`).join('')}</div>
    <div class="note">${note}</div>${dk.note ? `<div class="note warn">${esc(dk.note)}</div>` : ''}
  </div>`;
}

function renderPools(v) {
  const llms = (v.engines || []).filter(e => e.llm);
  let slots = 0, reporting = 0;
  const html = llms.map(e => {
    const x = e.extra || {}, kv = x.kv, sl = x.slots, dk = diskBlock(x.disk);
    const head = `<div class="pool-h"><span class="sw" style="display:inline-block;width:9px;height:9px;border-radius:2px;background:${colorOf(e.name)}"></span><b>${esc(e.name)}</b><span>${esc(e.model || (e.up ? 'no model' : 'not running'))}</span>`;
    if (!e.loaded) return `<div class="pool">${head}</div><div class="note">${e.up ? 'No model loaded: no cache to show.' : 'Not running.'}</div>${dk}</div>`;
    if (sl && sl.length) {
      reporting++; slots += sl.length;
      const cells = sl.map(s => {
        const f = s.window ? Math.min(1, s.used / s.window) : 0;
        return `<div class="slot ${s.busy ? 'busy' : ''}" title="slot ${s.id}: ${s.used.toLocaleString()} of ${s.window.toLocaleString()} tokens${s.cached ? `, ${s.cached.toLocaleString()} cached` : ''}${s.busy ? ', working' : ''}"><div class="cell"><i style="height:${(f * 100).toFixed(1)}%"></i></div>${Math.round(f * 100)}%<br>s${s.id}</div>`;
      }).join('');
      return `<div class="pool">${head}<span class="r">${sl.length} slot${sl.length === 1 ? '' : 's'} · ${tok(sl[0].window)} window</span></div><div class="slots">${cells}</div><div class="note">llama.cpp /slots: prompt tokens in each slot against its window</div></div>`;
    }
    if (kv && (kv.pool || kv.window)) {
      reporting++;
      const pool = kv.pool || kv.window, fl = kv.inflight || [];
      const used = fl.reduce((a, f) => a + f.prompt, 0);
      const segs = fl.map(f => `<span title="#${esc(f.tag)}: ${f.prompt.toLocaleString()} prompt tokens, ${span(f.age_s)} in flight" style="width:${Math.max(.4, f.prompt / pool * 100)}%"></span>`).join('');
      const ticks = kv.window && kv.pool && kv.window < kv.pool ? `<i class="tick" style="left:${kv.window / kv.pool * 100}%" title="one request's window: ${kv.window.toLocaleString()}"></i>` : '';
      return `<div class="pool">${head}<span class="r">${fl.length ? `${fl.length} in flight` : 'idle'}</span></div>
        <div class="kvbar" role="img" aria-label="${used.toLocaleString()} of ${pool.toLocaleString()} cache tokens in prompts of requests in flight">${segs}${ticks}</div>
        <div class="figs"><div><b>${tok(pool)}</b><span>cache pool</span></div><div><b>${kv.window ? tok(kv.window) : '—'}</b><span>window</span></div><div><b>${fl.length ? tok(used) : '0'}</b><span>prompts in flight</span></div><div><b>${esc(kv.cache_mode || '—')}</b><span>cache mode</span></div></div>
        ${fl.length ? `<div class="flights">${fl.map(f => `<div><span>#${esc(f.tag)}</span><span>${f.prompt.toLocaleString()} tokens · ${span(f.age_s)}</span></div>`).join('')}</div>` : ''}
        <div class="note">${kv.inflight ? 'from TabbyAPI\'s log: prompt tokens of each request in flight; the tokens they generate and the prefix cache are not reported' : 'from TabbyAPI: window and pool of the loaded model; requests in flight are read from its log when baton started it'}</div>${dk}</div>`;
    }
    const ctx = typeof x.context === 'number' ? x.context : 0;
    return `<div class="pool">${head}${ctx ? `<span class="r">${tok(ctx)} window</span>` : ''}</div><div class="note">No live slot data: ${esc(e.name)} reports ${ctx ? 'only its context length' : 'nothing about its cache'}.</div>${dk}</div>`;
  }).join('') || '<div class="empty-note">No LLM engines configured.</div>';
  if ($('#pools').innerHTML !== html) $('#pools').innerHTML = html;
  $('#poolMeta').textContent = reporting ? `${reporting} engine${reporting === 1 ? ' reports its' : 's report their'} cache${slots ? ` · ${slots} live slots` : ''}` : 'no engine reports its cache right now';
}

// --- throughput ---
function renderThroughput(r) {
  if ($('#sec-tput').classList.contains('folded')) return;
  const opts = {now: Date.parse(r.at) / 1000, range: RANGE_S[r.range] || 86400, color: colorOf};
  drawRate($('#tputDecode'), r.throughput || [], 'decode', {...opts, title: 'DECODE t/s', empty: 'No decode rates in this range yet.'});
  drawRate($('#tputPrompt'), r.throughput || [], 'prompt', {...opts, title: 'PREFILL t/s', empty: 'No prefill rates in this range yet.'});
  const engs = [...new Set((r.throughput || []).map(p => p.engine))];
  $('#tputLegend').innerHTML = engs.map(e => `<span><i style="background:${colorOf(e)}"></i>${esc(e)}</span>`).join('');
  $('#tputMeta').textContent = `tokens per second of each request, last ${r.range} · ${(r.throughput || []).length} requests`;
}

// --- token ledger ---
function renderLedger(r) {
  $('#ledger').innerHTML = (r.ledger || []).map(c => {
    const sum = c.generated + c.fresh + c.cached;
    const seg = (v, cls) => sum ? `<span class="${cls}" style="width:${v / sum * 100}%"></span>` : '';
    return `<div class="lcol"><div class="h"><b>${esc(c.label)}</b><span>${c.requests.toLocaleString()} request${c.requests === 1 ? '' : 's'}</span></div>
      <div class="lbar">${seg(c.generated, 'c-gen')}${seg(c.fresh, 'c-fresh')}${seg(c.cached, 'c-cached')}</div>
      <div class="ln"><span><i class="c-gen"></i>${tok(c.generated)}</span><span><i class="c-fresh"></i>${tok(c.fresh)}</span><span><i class="c-cached"></i>${tok(c.cached)}</span></div></div>`;
  }).join('');
}

// --- leaderboard ---
const LB_COLS = [
  ['model', 'Model', false], ['engine', 'Engine', false], ['requests', 'Requests', true], ['prompt', 'Prompt', true], ['output', 'Output', true],
  ['decode_med', 'Decode med', true], ['first_token_med', 'First token', true], ['cache_pct', 'Cache', true], ['draft_pct', 'Draft acc', true], ['errors', 'Errors', true],
];
function renderLeaderboard(r) {
  const rows = (r.leaderboard || []).slice();
  const key = lbSort;
  rows.sort((a, b) => typeof a[key] === 'string' ? String(a[key]).localeCompare(b[key]) : (b[key] ?? -1) - (a[key] ?? -1));
  const cell = (row, k) => {
    const v = row[k];
    if (k === 'model') return `<td class="m" title="${esc(v)}">${esc(v || '—')}</td>`;
    if (k === 'engine') return `<td><span style="color:${colorOf(v)}">${esc(v)}</span></td>`;
    if (v === null || v === undefined) return '<td class="r" style="color:var(--faint)">—</td>';
    if (k === 'prompt' || k === 'output') return `<td class="r">${tok(v)}</td>`;
    if (k.endsWith('_pct')) return `<td class="r">${v.toFixed(0)}%</td>`;
    if (k === 'first_token_med') return `<td class="r">${v.toFixed(2)}s</td>`;
    if (k === 'decode_med') return `<td class="r">${v.toFixed(1)}</td>`;
    return `<td class="r">${v.toLocaleString()}</td>`;
  };
  $('#leaderboard').innerHTML = rows.length ? `<table class="data"><thead><tr>${LB_COLS.map(([k, l, num]) => `<th class="sortable ${num ? 'r' : ''} ${k === key ? 'on' : ''}" data-k="${k}" aria-sort="${k === key ? 'descending' : 'none'}">${l}</th>`).join('')}</tr></thead>
    <tbody>${rows.map(row => `<tr>${LB_COLS.map(([k]) => cell(row, k)).join('')}</tr>`).join('')}</tbody></table>`
    : '<div class="empty-note" style="padding:14px">No requests in this range yet.</div>';
  $('#lbMeta').textContent = `last ${r.range} · click a column to sort`;
}
$('#leaderboard').onclick = ev => {
  const th = ev.target.closest('th[data-k]');
  if (!th) return;
  lbSort = th.dataset.k; store.set('lbsort', lbSort);
  if (rv) renderLeaderboard(rv);
};

// --- recent requests ---
function renderRecent(r) {
  const rows = r.recent || [];
  $('#recent').innerHTML = rows.length ? `<table class="data"><thead><tr><th>Time</th><th>Engine</th><th>Model</th><th>Status</th><th class="r">Prompt</th><th class="r">Cached</th><th class="r">Output</th><th class="r">Prefill t/s</th><th class="r">Decode t/s</th><th class="r">First token</th><th class="r">Total</th><th>Seen by</th></tr></thead><tbody>${rows.map(q => {
    const ok = q.status === 0 || (q.status >= 200 && q.status < 300);
    const n = (v, f) => v ? f(v) : '<span style="color:var(--faint)">—</span>';
    return `<tr class="${ok ? '' : 'bad'}" title="${esc(q.route || '')}${q.error ? ' · ' + esc(q.error) : ''}"><td>${clock(q.at)}</td><td><span style="color:${colorOf(q.engine)}">${esc(q.engine)}</span></td><td class="m" title="${esc(q.model)}">${esc(q.model || '—')}</td><td>${q.status || 200}${q.error ? ' ' + esc(q.error) : ''}</td>
      <td class="r">${n(q.prompt, v => v.toLocaleString())}</td><td class="r">${n(q.cached, v => v.toLocaleString())}</td><td class="r">${n(q.output, v => v.toLocaleString())}</td>
      <td class="r">${n(q.prompt_tps, v => Math.round(v).toLocaleString())}</td><td class="r">${n(q.decode_tps, v => v.toFixed(1))}</td><td class="r">${n(q.first_token_s, v => v.toFixed(2) + 's')}</td><td class="r">${n(q.total_s, v => v.toFixed(1) + 's')}</td>
      <td><span class="src">${esc(q.source)}</span></td></tr>`;
  }).join('')}</tbody></table>` : '<div class="empty-note" style="padding:14px">No requests recorded yet. TabbyAPI\'s are read from its log when baton starts it; other engines\' are seen through the gateway.</div>';
  $('#recentMeta').textContent = `newest first · ${rows.length} shown`;
  $('#sources').innerHTML = Object.entries(r.sources || {}).map(([e, s]) => `<span><span style="color:${colorOf(e)}">${esc(e)}</span>: ${esc(s)}</span>`).join('');
}

// --- context residency ---
function renderResidency(r) {
  if ($('#sec-res').classList.contains('folded')) return;
  const reqs = r.residency || [];
  drawResidency($('#residency'), reqs, {tok});
  const live = [];
  for (const e of (view && view.engines) || []) {
    const kv = e.extra && e.extra.kv;
    for (const f of (kv && kv.inflight) || []) live.push({engine: e.name, tag: f.tag, used: f.prompt, window: kv.window, age: f.age_s});
    for (const s of (e.extra && e.extra.slots) || []) if (s.used) live.push({engine: e.name, tag: 's' + s.id, used: s.used, window: s.window});
  }
  $('#sessions').innerHTML = live.length ? '<div class="sub-h">LIVE NOW</div>' + live.map(l => `<div class="mrow"><span class="k" style="color:${colorOf(l.engine)}">${esc(l.engine)} ${esc(l.tag)}</span><span class="track"><span style="width:${l.window ? Math.min(100, l.used / l.window * 100) : 0}%;background:${colorOf(l.engine)}"></span></span><span class="v">${tok(l.used)}${l.window ? ' / ' + tok(l.window) : ''}</span></div>`).join('') : '';
  $('#resMeta').textContent = `last ${reqs.length} served request${reqs.length === 1 ? '' : 's'} against their model's window${live.length ? ` · ${live.length} live now` : ''}`;
}

// --- start ---
showPage(page);
poll();
