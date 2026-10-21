// The harbor page: the ticket counter, the tide, the model timeline, the
// departures board, the harbor log, and the berths (cards), the machine and
// the stowaways read from the live status.
'use strict';
let hv = null, hRange = store.get('hrange', '1h'), tideKey = store.get('tide', 'gpu0');
const RANGE_S = {'1h': 3600, '6h': 21600, '24h': 86400, '7d': 604800};

async function pollHarbor() {
  clearTimeout(pollHarbor.t);
  if (page === 'harbor' && !document.hidden) {
    try {
      hv = await (await fetch('/api/harbor?range=' + hRange, {cache: 'no-store'})).json();
      renderCounter(hv); renderTide(hv); renderTimeline(hv); renderBoard(hv);
    } catch (e) {}
  }
  pollHarbor.t = setTimeout(pollHarbor, 5000);
}

function pressRange(group, val) {
  group.querySelectorAll('button').forEach(b => b.setAttribute('aria-pressed', String(b.dataset.r === val || b.dataset.k === val)));
}
pressRange($('#hRange'), hRange);
$('#hRange').onclick = ev => {
  const b = ev.target.closest('button');
  if (!b) return;
  hRange = b.dataset.r; store.set('hrange', hRange); pressRange($('#hRange'), hRange); pollHarbor();
};

// --- the ticket counter ---
const TILES = [
  ['handed_mib', 'VRAM released', c => c.handed_mib >= 1048576 ? (c.handed_mib / 1048576).toFixed(1) : gib(c.handed_mib), c => c.handed_mib >= 1048576 ? 'TiB' : 'GiB', 'memory baton\'s releases gave back, as the driver measured it'],
  ['switches', 'Switches', c => c.switches, '', 'an engine took the cards'],
  ['drains', 'Drains', c => c.drains, '', 'releases that first waited for work in flight'],
  ['aground', 'Aground caught', c => c.aground, '', 'releases whose memory stayed, caught before anything else started there'],
  ['claims', 'Outside loads', c => c.claims, '', 'loads outside baton settled by releasing whatever shared the card'],
  ['refused', 'Refused', c => c.refused, '', 'switches turned down: cards reserved, or an engine aground'],
  ['yields', 'Games & streams', c => c.yields, '', 'games, holds, streams and guests the cards were cleared for'],
  ['gate_held', 'ComfyUI held', c => c.gate_held, '', 'ComfyUI jobs that waited at the gate for the cards'],
];
const KPI_DEF = {
  decode: ['Decode', 't/s', v => v.toFixed(1), 'median decode rate, last hour', 'var(--cyan)'],
  first_token: ['First token', 's', v => v.toFixed(2), 'median time to first token, last hour', 'var(--amber)'],
  p95_total: ['P95', 's', v => v.toFixed(1), '95th percentile request time, last hour', 'var(--violet)'],
  req_min: ['Req/min', '', v => v.toFixed(1), 'requests per minute, last hour; one bar per minute', 'var(--cyan)'],
  cache: ['Cache', '%', v => Math.round(v), 'share of prompt tokens served from cache, last hour', 'var(--green)'],
  draft: ['Draft accept', '%', v => Math.round(v), 'share of drafted tokens accepted, last hour', 'var(--green)'],
};

function odometer(n) {
  const box = $('#odometer');
  const s = String(Math.max(0, n)).padStart(6, '0');
  if (box.children.length !== s.length) {
    box.innerHTML = s.split('').map(() => `<span class="digit"><span class="reel">${'0123456789'.split('').map(d => `<span>${d}</span>`).join('')}</span></span>`).join('');
  }
  const lead = s.search(/[1-9]/);
  [...box.children].forEach((d, i) => {
    d.classList.toggle('lead', lead < 0 || i < lead);
    d.firstChild.style.transform = `translateY(${-Number(s[i]) * 10}%)`;
  });
  box.setAttribute('aria-label', `${n} collisions avoided`);
}

function renderCounter(h) {
  const c = h.counters;
  odometer(c.handoffs);
  $('#counterMeta').innerHTML = `<span class="live"><i></i>live</span> counts what baton caught since ${new Date(c.since).toLocaleDateString([], {month: 'short', day: 'numeric', year: 'numeric'})}`;
  $('#ctiles').innerHTML = TILES.map(([k, label, f, unit, title]) =>
    `<div class="ctile ${k === 'aground' && c.aground && view && view.engines.some(e => e.aground) ? 'hot' : ''}" title="${esc(title)}"><div class="k">${label}</div><div class="v">${f(c)}${unit ? `<small>${typeof unit === 'function' ? unit(c) : unit}</small>` : ''}</div></div>`).join('');
  const kp = {};
  for (const k of h.kpis || []) kp[k.key] = k;
  const tile = (key) => {
    const k = kp[key] || {}, [label, unit, fmt, title, color] = KPI_DEF[key];
    const has = k.value !== null && k.value !== undefined;
    const spark = key === 'cache' || key === 'draft' ? '' : sparkSVG(k.spark, color, key === 'req_min');
    return `<div class="kpi" title="${esc(title)}${has ? ` (${k.n} requests)` : ''}"><div class="k">${label}<small>${unit}</small></div><div class="v ${has ? '' : 'none'}">${has ? fmt(k.value) : '—'}</div>${spark}</div>`;
  };
  const q = h.queue || 0;
  $('#kpis').innerHTML = tile('decode') + tile('first_token') + tile('p95_total') + tile('req_min')
    + `<div class="kpi ${q ? 'alarm' : ''}" title="waiting now: changes queued for the cards, ComfyUI jobs at the gate, gateway requests in flight"><div class="k">${q ? '▲ ' : ''}Queue<small></small></div><div class="v">${q}</div></div>`
    + tile('cache') + tile('draft');
}

// --- the tide ---
const TIDE_ORDER = ['game', 'stream', 'guest', 'apps', 'system', 'other', 'driver'];
function tideOrder(keys) {
  const eng = Object.keys(kinds).filter(k => keys.includes(k));
  return eng.concat(TIDE_ORDER.filter(k => keys.includes(k)), keys.filter(k => !eng.includes(k) && !TIDE_ORDER.includes(k)));
}

function renderTide(h) {
  const series = h.tide || [];
  if (!series.find(s => s.key === tideKey) && series.length) tideKey = series[0].key;
  const keysHTML = series.map(s => `<button data-k="${esc(s.key)}" aria-pressed="${s.key === tideKey}">${esc(s.key.toUpperCase())}</button>`).join('');
  if ($('#tideKeys').innerHTML !== keysHTML) $('#tideKeys').innerHTML = keysHTML;
  const s = series.find(x => x.key === tideKey);
  if (!s) return emptyChart($('#tide'), 'No readings yet.', 120);
  if (page !== 'harbor' || $('#sec-tide').classList.contains('folded')) return;
  const keys = drawTide($('#tide'), s, {now: Date.parse(h.at) / 1000, range: RANGE_S[h.range] || 3600, order: tideOrder, color: colorOf, label: labelOf}) || [];
  $('#tideLegend').innerHTML = keys.map(k => `<span><i class="${k === 'driver' ? 'hatch' : ''}" style="${k === 'driver' ? '' : 'background:' + colorOf(k)}"></i>${esc(labelOf(k))}</span>`).join('')
    + `<span><i style="border:1px dashed var(--faint);background:transparent"></i>total</span>`;
  const last = s.points[s.points.length - 1];
  $('#tideMeta').innerHTML = `<span class="live"><i></i>live</span> ${esc(s.name)} memory, last ${h.range}${last ? ` · ${gib(last.used)} / ${gib(s.total)} GiB now` : ''} · ${s.step_s}s steps`;
}
$('#tideKeys').onclick = ev => {
  const b = ev.target.closest('button');
  if (!b) return;
  tideKey = b.dataset.k; store.set('tide', tideKey); pressRange($('#tideKeys'), tideKey);
  if (hv) renderTide(hv);
};

// --- the model timeline ---
function renderTimeline(h) {
  if (page !== 'harbor' || $('#sec-timeline').classList.contains('folded')) return;
  const sp = h.timeline || [];
  drawTimeline($('#timeline'), sp, {
    now: Date.parse(h.at) / 1000, range: RANGE_S[h.range] || 3600, dur: span,
    color: (kind, owner) => kinds[owner] ? colorOf(owner) : colorOf(kind),
  });
  const rows = new Set(sp.map(s => s.owner + '\x00' + s.label)).size;
  $('#tlMeta').textContent = `what held the cards, last ${h.range}${rows ? ` · ${rows} row${rows === 1 ? '' : 's'}` : ''}`;
}

// --- departures ---
const STATUS_WORD = {docked: 'DOCKED', moored: 'MOORED', boarding: 'BOARDING', 'casting-off': 'CASTING OFF', aground: 'AGROUND', queued: 'QUEUED', reserved: 'RESERVED'};
const STATUS_TITLE = {
  docked: 'holds the cards', moored: 'loaded beside the holder, on cards it shares with no one',
  boarding: 'coming onto the cards', 'casting-off': 'being released; the driver is checked before anyone else starts',
  aground: 'kept its memory after its release; nothing else starts on its cards', queued: 'waiting its turn', reserved: 'the cards are held for you',
};
const flaps = w => `<span class="flaps" aria-label="${esc(w)}">${w.split('').map(ch => ch === ' ' ? '<b class="sp"></b>' : `<b>${esc(ch)}</b>`).join('')}</span>`;
const RESULT_WORD = {ok: 'ARRIVED', refused: 'REFUSED', aground: 'AGROUND', failed: 'FAILED', cancelled: 'CANCELLED'};

function renderBoard(h) {
  const rows = h.board || [];
  if (!rows.length) {
    $('#board').innerHTML = '<div class="empty-note">The harbor is quiet: nothing holds the cards and nothing is waiting.</div>';
  } else {
    $('#board').innerHTML = `<div class="board-wrap"><table class="board"><thead><tr><th class="c-tno">TICKET</th><th>VESSEL</th><th class="c-berth">BERTH</th><th class="c-cargo">CARGO</th><th>HOLD</th><th>STATUS</th><th>TIME</th></tr></thead><tbody>${rows.map(d => `
      <tr title="${esc(d.detail || STATUS_TITLE[d.status] || '')}">
        <td class="tno c-tno">${d.no ? String(d.no).padStart(4, '0') : '····'}</td>
        <td class="vessel" style="color:${d.kind === 'op' ? 'var(--text)' : (kinds[d.vessel] ? colorOf(d.vessel) : colorOf(d.kind))}">${esc(d.vessel)}</td>
        <td class="c-berth">${d.gpus && d.gpus.length ? 'GPU' + d.gpus.join('+') : d.kind === 'game' || d.kind === 'hold' ? 'ALL' : '—'}</td>
        <td class="cargo c-cargo">${esc(d.cargo || (d.kind === 'op' ? d.detail || '' : ''))}</td>
        <td class="num">${d.hold_mib ? gib(d.hold_mib) + 'G' : '—'}</td>
        <td><span class="st ${esc(d.status)}">${flaps(STATUS_WORD[d.status] || d.status.toUpperCase())}</span></td>
        <td class="num">${since(d.since) || '—'}</td>
      </tr>`).join('')}</tbody></table></div><div class="board-foot">${rows.length} on the board · ops by ticket number, then what is on the cards</div>`;
  }
  $('#boardMeta').textContent = `${rows.length} voyage${rows.length === 1 ? '' : 's'}`;
  const vs = h.voyages || [];
  $('#voyages').innerHTML = vs.map(v => `<div class="voyage" title="${esc(v.detail || '')}"><span class="tno">#${String(v.no).padStart(4, '0')}</span><span class="nm">${esc(v.name)}${v.freed_mib ? ` <span style="color:var(--muted)">· ${gib(v.freed_mib)} GiB freed</span>` : ''}</span><span class="res ${esc(v.result)}">${RESULT_WORD[v.result] || v.result.toUpperCase()}</span><span class="dur">${v.dur_s.toFixed(1)}s · ${clock(v.start)}</span></div>`).join('')
    || '<div class="empty-note">No changes to the cards yet. Each switch, release and hand-over is listed here with how long it took.</div>';
}

// --- the harbor log ---
let evKind = 'all', evPaused = false, evText = '';
$('#evFilter').addEventListener('input', ev => { evText = ev.target.value.toLowerCase(); renderEvents(view, true); });
$('#evPause').onclick = () => { evPaused = !evPaused; $('#evPause').setAttribute('aria-pressed', String(evPaused)); $('#evPause').textContent = evPaused ? 'Resume' : 'Pause'; if (!evPaused) renderEvents(view, true); };
$('#evChips').onclick = ev => { const b = ev.target.closest('button'); if (b) { evKind = b.dataset.k; renderEvents(view, true); } };
let evShown = -1;

function renderEvents(v, force) {
  if (!v || (evPaused && !force)) return;
  if (!force && evShown === evSeq) return;
  evShown = evSeq;
  const count = {all: evList.length, warn: 0};
  for (const e of evList) { count[e.kind] = (count[e.kind] || 0) + 1; if (e.level !== 'info') count.warn++; }
  const kindsShown = ['all'].concat(Object.keys(count).filter(k => k !== 'all' && k !== 'warn').sort((a, b) => count[b] - count[a]).slice(0, 5), count.warn ? ['warn'] : []);
  $('#evChips').innerHTML = kindsShown.map(k => `<button class="chip" data-k="${esc(k)}" aria-pressed="${evKind === k}">${esc(k === 'warn' ? 'alerts' : k)}<span class="c">${count[k]}</span></button>`).join('');
  const list = evList.filter(e => (evKind === 'all' || (evKind === 'warn' ? e.level !== 'info' : e.kind === evKind)) && (!evText || (e.kind + ' ' + (e.engine || '') + ' ' + e.msg).toLowerCase().includes(evText)));
  $('#events').innerHTML = list.slice().reverse().map(e => `<div class="ev ${esc(e.level)}"><span class="t" title="${esc(new Date(e.at).toLocaleString())}">${clock(e.at)}</span><span class="k">${esc(e.kind)}</span><span class="m">${e.engine ? `<em>${esc(e.engine)}</em> ` : ''}${esc(e.msg)}</span></div>`).join('') || '<div class="empty-note">No events match.</div>';
  $('#logMeta').textContent = `${list.length} shown${evPaused ? ' · paused' : ''}`;
}

// --- berths: the cards ---
const WORD = {system: 'SYSTEM', game: 'GAME', stream: 'STREAM', guest: 'GUEST'};
function holderWord(h) {
  if (kinds[h.tag]) {
    const e = (view.engines || []).find(x => x.name === h.tag);
    return e && e.aground ? ['AGROUND', 'var(--red)'] : e && e.holder ? ['DOCKED', colorOf(h.tag)] : ['MOORED', colorOf(h.tag)];
  }
  if (WORD[h.tag]) return [WORD[h.tag], colorOf(h.tag)];
  return ['STOWAWAY', 'var(--red)'];
}

function renderGPUs(v) {
  const html = (v.gpus || []).map(g => {
    const total = g.mem_total / 1048576, used = g.mem_used / 1048576, free = g.mem_free / 1048576;
    const groups = {};
    let attributed = 0;
    for (const h of g.holders || []) {
      const key = h.tag || 'apps';
      groups[key] = (groups[key] || 0) + h.mib;
      attributed += h.mib;
    }
    const order = tideOrder(Object.keys(groups));
    let segs = order.map(k => `<span title="${esc(labelOf(k))}: ${gib(groups[k])} GiB" style="width:${groups[k] / total * 100}%;background:${colorOf(k)}"></span>`).join('');
    const un = Math.max(0, used - attributed);
    if (un > 64) segs += `<span class="hatch" title="driver / unattributed: ${gib(un)} GiB" style="width:${un / total * 100}%"></span>`;
    const enc = (g.encoders || []).length ? `<span class="tag-chip enc">NVENC ×${g.encoders.length}</span>` : '';
    const holder = (v.engines || []).find(e => e.holder && (e.vram_mib || {})[g.index] > 0);
    const guest = v.guest && (g.holders || []).some(h => h.tag === 'guest');
    const state = v.mode === 'game' || v.mode === 'hold' ? [v.mode.toUpperCase(), 'var(--c-hold)'] : holder ? ['HELD · ' + holder.name.toUpperCase(), colorOf(holder.name)] : guest ? ['GUEST · ' + v.guest.why.toUpperCase(), colorOf('guest')] : used < 1024 ? ['FREE', 'var(--green)'] : ['IN USE', 'var(--muted)'];
    const temp = g.temp_c >= 83 ? 'crit' : g.temp_c >= 75 ? 'hot' : '';
    const holders = (g.holders || []).slice(0, 7).map(h => {
      const [w, c] = holderWord(h);
      return `<li><span class="w" style="color:${c}">${w}</span><span class="hn" title="${esc(h.path || h.name)} (pid ${h.pid})">${esc(h.tag && kinds[h.tag] ? h.tag + ' · ' + h.name : h.name)}${h.util >= 1 ? ` <span style="color:var(--faint)">${Math.round(h.util)}%</span>` : ''}</span><span class="hm">${gib(h.mib)} GiB</span></li>`;
    }).join('');
    return `<div class="berth">
      <div class="berth-h"><b>gpu${g.index}</b><span class="nm">${esc(g.name.replace('NVIDIA GeForce ', ''))}</span>${enc}${g.probe_error ? `<span class="tag-chip">${esc(g.probe_error)}</span>` : ''}<span class="state" style="color:${state[1]}">${esc(state[0])}</span></div>
      <div class="bar">${segs}</div>
      <div class="figs">
        <div><b>${gib(used)} / ${gib(total)}</b><span>GiB used</span></div><div><b>${gib(free)}</b><span>GiB free</span></div>
        <div><b>${g.util}%</b><span>load</span></div><div><b class="${temp}">${g.temp_c}°C</b><span>temp</span></div>
        <div><b>${Math.round(g.power_w)}/${Math.round(g.power_max_w)}</b><span>watts</span></div><div><b>${g.fan_pct}%</b><span>fan</span></div>
        <div><b>${g.clock_mhz}</b><span>MHz core</span></div><div><b>${g.mem_clock_mhz || '—'}</b><span>MHz mem</span></div>
      </div>
      <ul class="holders">${holders || '<li class="empty">nothing on this card</li>'}</ul>
    </div>`;
  }).join('') || '<div class="empty-note">No NVIDIA GPU found.</div>';
  if ($('#gpus').innerHTML !== html) $('#gpus').innerHTML = html;
}

// --- the machine ---
function renderMem(v) {
  const m = v.memory;
  if (!m || !m.total) { $('#mem').innerHTML = '<div class="empty-note">No memory reading.</div>'; return; }
  const M = 1048576, total = m.total / M, used = m.used / M;
  const hs = m.holders || [];
  const segs = [];
  let shown = 0;
  for (const h of hs) if (h.tag && h.tag !== 'system') { segs.push([h.tag, h.mib, colorOf(h.tag)]); shown += h.mib; }
  const apps = hs.filter(h => !h.tag).reduce((a, h) => a + h.mib, 0);
  const sys = hs.filter(h => h.tag === 'system').reduce((a, h) => a + h.mib, 0);
  if (apps) { segs.push(['apps', apps, 'var(--c-other)']); shown += apps; }
  if (sys) { segs.push(['Windows', sys, 'var(--c-system)']); shown += sys; }
  const rest = Math.max(0, used - shown);
  const track = segs.map(([k, mib, c]) => `<span title="${esc(k)}: ${gib(mib)} GiB" style="width:${mib / total * 100}%;background:${c}"></span>`).join('')
    + (rest > 64 ? `<span class="hatch" style="width:${rest / total * 100}%;background:repeating-linear-gradient(135deg,var(--faint) 0 2px,transparent 2px 5px)" title="services, drivers and kernel: ${gib(rest)} GiB"></span>` : '');
  const load = m.load, color = load >= 95 ? 'var(--red)' : load >= 85 ? 'var(--amber)' : 'var(--cyan)';
  const bar = (k, frac, c, val, title, vc) => `<div class="mrow" title="${esc(title || '')}"><span class="k">${k}</span><span class="track"><span style="width:${Math.min(100, frac * 100)}%;background:${c}"></span></span><span class="v"${vc ? ` style="color:${vc}"` : ''}>${val}</span></div>`;
  // Windows refuses new memory once commit reaches its limit, however much RAM is free.
  const commit = m.commit_limit ? m.commit_total / m.commit_limit : 0, commitHigh = commit > 0.9;
  const rows = hs.slice(0, 7).map(h => `<li><span class="w" style="color:${colorOf(h.tag || 'apps')}">${esc((h.tag && h.tag !== 'system' ? (kinds[h.tag] ? 'ENGINE' : h.tag) : h.tag === 'system' ? 'SYSTEM' : 'APP').toUpperCase())}</span><span class="hn">${esc(h.tag && kinds[h.tag] ? h.tag : h.name)}${h.count > 1 ? ` <span style="color:var(--faint)">×${h.count}</span>` : ''}</span><span class="hm">${gib(h.mib)} GiB</span></li>`).join('');
  const html = `<div class="mrow"><span class="k">RAM</span><span class="track">${track}</span><span class="v" style="color:${color}">${gib(used)} / ${gib(total)} GiB</span></div>`
    + bar('COMMIT', commit, commitHigh ? 'var(--amber)' : 'var(--violet)', `${gib(m.commit_total / M)} / ${gib(m.commit_limit / M)} GiB`,
      `Commit ${Math.round(commit * 100)}% of limit (RAM + page file).` + (commitHigh ? " Windows refuses new memory when it's full." : ''), commitHigh ? 'var(--amber)' : '')
    + bar('CACHED', m.cached / m.total, 'var(--c-system)', `${gib(m.cached / M)} GiB`, 'file cache Windows hands back when asked')
    + bar('KERNEL', m.kernel / m.total, 'var(--c-system)', `${gib(m.kernel / M)} GiB`)
    + `<ul class="holders">${rows}</ul>`;
  if ($('#mem').innerHTML !== html) $('#mem').innerHTML = html;
  $('#machineMeta').textContent = `${load}% in use · ${gib(m.available / M)} GiB free`;
}

// --- games & streams ---
function renderHits(v) {
  const hits = v.hits || [];
  let html = hits.map(h => `<div class="hit"><span class="tag-chip" style="color:${colorOf(h.kind)};border-color:currentColor">${esc(h.kind.toUpperCase())}</span><b>${esc(h.kind === 'game' ? gameTitle(h) : h.exe)}</b><span class="s">${esc(h.match ? h.rule + ' · ' + h.match : h.kind === 'guest' ? h.rule + ' · the LLMs wait' : h.rule)}${h.pid ? ' · pid ' + h.pid : ''}</span></div>`).join('');
  if (v.hold) html = `<div class="hit"><span class="tag-chip" style="color:var(--c-hold);border-color:currentColor">HOLD</span><b>${esc(v.hold.reason)}</b><span class="s">${v.hold.until && !v.hold.until.startsWith('0001') ? 'until ' + new Date(v.hold.until).toLocaleTimeString() : 'until released'}</span></div>` + html;
  html = (html || `<div class="empty-note">No game or stream detected. Games free both GPUs; a stream holds ComfyUI and keeps the LLM while GPU ${esc(v.policy.stream_encode_gpu)} has ${esc(v.policy.stream_min_free_mib)} MiB free.</div>`)
    + '<div style="margin-top:12px"><button class="linkbtn" onclick="openGames()">Manage games →</button></div>';
  if ($('#hits').innerHTML !== html) $('#hits').innerHTML = html;
  $('#hitsMeta').textContent = hits.length || v.hold ? `${hits.length + (v.hold ? 1 : 0)} active` : 'none';
}

// --- stowaways: memory on the cards that belongs to no engine, game or stream ---
function renderStow(v) {
  const list = [];
  for (const g of v.gpus || []) for (const h of g.holders || []) if (!h.tag && h.mib >= 128) list.push({...h, gpu: g.index});
  list.sort((a, b) => b.mib - a.mib);
  const html = list.map(s => `<div class="stow"><div class="t"><b>${esc(s.name)}</b><span>pid ${s.pid}</span></div><div class="s">${gib(s.mib)} GiB on gpu${s.gpu}${s.util >= 1 ? ` · ${Math.round(s.util)}% load` : ''}</div></div>`).join('')
    || '<div class="empty-note">No stowaways: every process holding real memory on the cards belongs to an engine, a game, a stream, a guest or Windows.</div>';
  if ($('#stow').innerHTML !== html) $('#stow').innerHTML = html;
  $('#stowMeta').textContent = list.length ? `${list.length} on the cards` : 'none';
}

function redrawCharts() {
  if (hv && page === 'harbor') { renderTide(hv); renderTimeline(hv); }
  if (typeof rv !== 'undefined' && rv && page === 'engines') renderRequests(rv);
}
document.addEventListener('visibilitychange', () => { if (!document.hidden) { pollHarbor(); pollRequests(); } });
