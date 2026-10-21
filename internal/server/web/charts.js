// Hand-drawn SVG charts for the dashboard: the tide, the model timeline,
// throughput, context residency and the small sparklines. No library.
'use strict';
const SVGNS = 'http://www.w3.org/2000/svg';
const reduceMotion = matchMedia('(prefers-reduced-motion: reduce)').matches;

function S(tag, attrs, parent) {
  const n = document.createElementNS(SVGNS, tag);
  for (const k in attrs || {}) n.setAttribute(k, attrs[k]);
  if (parent) parent.appendChild(n);
  return n;
}

const escT = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));

// A step of about max/n rounded to 1, 2 or 5 times a power of ten.
function niceStep(max, n) {
  const raw = max / Math.max(n, 1);
  const p = Math.pow(10, Math.floor(Math.log10(raw || 1)));
  for (const m of [1, 2, 2.5, 5, 10]) if (raw <= m * p) return m * p;
  return 10 * p;
}

const TIME_STEPS = [60, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400];
function timeTicks(t0, t1, n) {
  const step = TIME_STEPS.find(s => (t1 - t0) / s <= n) || 86400;
  const off = new Date().getTimezoneOffset() * 60;
  const out = [];
  for (let t = Math.ceil((t0 - off) / step) * step + off; t <= t1; t += step) out.push(t);
  return out;
}
function clockLabel(t, span) {
  const d = new Date(t * 1000);
  if (span > 2 * 86400) return d.toLocaleDateString([], {month: 'short', day: 'numeric'});
  return d.toLocaleTimeString([], {hour: '2-digit', minute: '2-digit', hour12: false});
}
function fullClock(t) {
  return new Date(t * 1000).toLocaleString([], {month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false});
}

// A floating tooltip inside a chart container.
function tipFor(box) {
  let tip = box.querySelector(':scope > .tip');
  if (!tip) { tip = document.createElement('div'); tip.className = 'tip'; tip.hidden = true; box.appendChild(tip); }
  return {
    show(html, x, y) {
      tip.innerHTML = html; tip.hidden = false;
      const w = tip.offsetWidth, bw = box.clientWidth;
      tip.style.left = Math.max(0, Math.min(bw - w, x + 14 > bw - w ? x - w - 14 : x + 14)) + 'px';
      tip.style.top = Math.max(0, y - 10) + 'px';
    },
    hide() { tip.hidden = true; },
  };
}

function frame(box, h) {
  box.querySelectorAll(':scope > svg, :scope > .empty-note').forEach(n => n.remove());
  const w = Math.max(box.clientWidth, 280);
  const svg = S('svg', {viewBox: `0 0 ${w} ${h}`, height: h, role: 'img'});
  box.insertBefore(svg, box.firstChild);
  return [svg, w];
}

function emptyChart(box, text, h) {
  box.querySelectorAll(':scope > svg, :scope > .empty-note').forEach(n => n.remove());
  const d = document.createElement('div');
  d.className = 'empty-note';
  d.style.minHeight = (h || 60) + 'px';
  d.textContent = text;
  box.insertBefore(d, box.firstChild);
}

function axes(svg, x0, x1, y0, y1, t0, t1, yTicks, yFmt) {
  const g = S('g', {class: 'grid'}, svg), a = S('g', {class: 'axis'}, svg);
  for (const v of yTicks) {
    const y = v.y;
    S('line', {x1: x0, x2: x1, y1: y, y2: y}, g);
    const t = S('text', {x: x0 - 6, y: y + 3, 'text-anchor': 'end'}, a); t.textContent = yFmt(v.v);
  }
  const ticks = timeTicks(t0, t1, Math.max(2, Math.floor((x1 - x0) / 110)));
  for (const t of ticks) {
    const x = x0 + (t - t0) / (t1 - t0) * (x1 - x0);
    const tx = S('text', {x, y: y1 + 15, 'text-anchor': 'middle'}, a); tx.textContent = clockLabel(t, t1 - t0);
  }
}

// --- sparklines ---

function sparkSVG(vals, color, bars) {
  if (!vals || vals.length < 2) return '<svg viewBox="0 0 100 26" preserveAspectRatio="none" aria-hidden="true"></svg>';
  const max = Math.max(...vals) || 1, n = vals.length;
  if (bars) {
    const w = 100 / n;
    return `<svg viewBox="0 0 100 26" preserveAspectRatio="none" aria-hidden="true">${vals.map((v, i) => `<rect x="${(i * w + w * .15).toFixed(2)}" y="${(26 - v / max * 24).toFixed(2)}" width="${(w * .7).toFixed(2)}" height="${(v / max * 24).toFixed(2)}" fill="${color}" opacity=".75"/>`).join('')}</svg>`;
  }
  const pts = vals.map((v, i) => `${(i / (n - 1) * 100).toFixed(2)},${(25 - v / max * 22).toFixed(2)}`);
  return `<svg viewBox="0 0 100 26" preserveAspectRatio="none" aria-hidden="true"><polygon points="0,26 ${pts.join(' ')} 100,26" fill="${color}" opacity=".14"/><polyline points="${pts.join(' ')}" fill="none" stroke="${color}" stroke-width="1.4" vector-effect="non-scaling-stroke"/></svg>`;
}

// --- the tide: a card's memory over time, stacked by holder ---

function drawTide(box, s, opts) {
  const h = box.clientWidth < 500 ? 190 : 250;
  const now = opts.now, t0 = now - opts.range, t1 = now;
  const pts = (s.points || []).filter(p => p.t >= t0);
  if (!pts.length) return emptyChart(box, 'No readings in this range yet: the tide fills in as baton runs.', h);
  const [svg, w] = frame(box, h);
  const X0 = 46, X1 = w - 10, Y0 = 20, Y1 = h - 22;
  const total = Math.max(s.total, ...pts.map(p => p.used)) || 1;
  const x = t => X0 + (t - t0) / (t1 - t0) * (X1 - X0);
  const y = v => Y1 - v / total * (Y1 - Y0);
  const step = niceStep(total / 1024, 4);
  const yt = [];
  for (let v = 0; v <= total / 1024 + 1e-9; v += step) yt.push({v, y: y(v * 1024)});
  axes(svg, X0, X1, Y0, Y1, t0, t1, yt, v => (v % 1 ? v.toFixed(1) : v) + 'G');
  S('line', {class: 'total', x1: X0, x2: X1, y1: y(s.total), y2: y(s.total)}, svg);
  const tl = S('text', {x: X0 + 4, y: y(s.total) - 5, class: 'tl-label'}, svg); tl.textContent = 'TOTAL ' + (s.total / 1024).toFixed(1) + ' GiB';

  // Layers bottom to top in a fixed order, so colours keep their place.
  const keys = opts.order(Object.keys(pts.reduce((a, p) => Object.assign(a, p.by), {})));
  // Runs of readings: a gap (baton was not running) breaks the areas.
  const gap = Math.max(s.step_s * 3, 30);
  const runs = [];
  let run = [];
  for (const p of pts) {
    if (run.length && p.t - run[run.length - 1].t > gap) { runs.push(run); run = []; }
    run.push(p);
  }
  runs.push(run);
  const defs = S('defs', {}, svg);
  const pat = S('pattern', {id: 'wv-' + s.key, width: 26, height: 8, patternUnits: 'userSpaceOnUse'}, defs);
  S('path', {d: 'M0 5 Q 6.5 1 13 5 T 26 5', fill: 'none', stroke: 'currentColor', 'stroke-width': 1, opacity: .18}, pat);
  if (!reduceMotion) S('animateTransform', {attributeName: 'patternTransform', type: 'translate', from: '0 0', to: '26 0', dur: '7s', repeatCount: 'indefinite'}, pat);
  for (const r of runs) {
    let base = r.map(() => 0);
    for (const k of keys) {
      const top = r.map((p, i) => base[i] + ((p.by || {})[k] || 0));
      if (top.every((v, i) => v === base[i])) continue;
      const fwd = r.map((p, i) => `${x(p.t).toFixed(1)},${y(top[i]).toFixed(1)}`);
      const back = r.map((p, i) => `${x(p.t).toFixed(1)},${y(base[i]).toFixed(1)}`).reverse();
      const c = opts.color(k);
      if (r.length === 1) { // a single reading: a thin bar
        S('rect', {x: x(r[0].t) - 1, y: y(top[0]), width: 2, height: y(base[0]) - y(top[0]), fill: c, opacity: .8}, svg);
      } else {
        S('polygon', {points: fwd.concat(back).join(' '), fill: k === 'driver' ? 'url(#hatch-' + s.key + ')' : c, opacity: k === 'driver' ? .7 : .42}, svg);
        S('polyline', {points: fwd.join(' '), fill: 'none', stroke: c, 'stroke-width': 1.3, opacity: .9}, svg);
      }
      base = top;
    }
    if (r.length > 1) { // the wave texture over everything in use
      const fwd = r.map((p, i) => `${x(p.t).toFixed(1)},${y(base[i]).toFixed(1)}`);
      const poly = S('polygon', {points: fwd.concat([`${x(r[r.length - 1].t).toFixed(1)},${Y1}`, `${x(r[0].t).toFixed(1)},${Y1}`]).join(' '), fill: `url(#wv-${s.key})`}, svg);
      poly.style.color = 'var(--text)';
    }
  }
  const hp = S('pattern', {id: 'hatch-' + s.key, width: 5, height: 5, patternUnits: 'userSpaceOnUse', patternTransform: 'rotate(45)'}, defs);
  S('rect', {width: 2, height: 5, fill: 'var(--faint)'}, hp);
  S('line', {class: 'now', x1: X1, x2: X1, y1: Y0, y2: Y1}, svg);

  const cross = S('line', {x1: 0, x2: 0, y1: Y0, y2: Y1, stroke: 'var(--muted)', 'stroke-dasharray': '2 3', visibility: 'hidden'}, svg);
  const tip = tipFor(box);
  svg.onpointermove = ev => {
    const r = svg.getBoundingClientRect();
    const px = (ev.clientX - r.left) / r.width * w;
    const t = t0 + (px - X0) / (X1 - X0) * (t1 - t0);
    let best = pts[0];
    for (const p of pts) if (Math.abs(p.t - t) < Math.abs(best.t - t)) best = p;
    if (Math.abs(best.t - t) > gap) { tip.hide(); cross.setAttribute('visibility', 'hidden'); return; }
    cross.setAttribute('x1', x(best.t)); cross.setAttribute('x2', x(best.t)); cross.setAttribute('visibility', 'visible');
    const rows = keys.filter(k => (best.by || {})[k]).reverse().map(k => `<div class="r"><span><i style="background:${opts.color(k)}"></i>${escT(opts.label(k))}</span><span>${(best.by[k] / 1024).toFixed(2)} GiB</span></div>`).join('');
    tip.show(`<div class="h">${fullClock(best.t)}</div>${rows}<div class="r" style="margin-top:4px"><span><b>in use</b></span><b>${(best.used / 1024).toFixed(2)} / ${(s.total / 1024).toFixed(1)} GiB</b></div>`, (ev.clientX - box.getBoundingClientRect().left), ev.clientY - box.getBoundingClientRect().top);
  };
  svg.onpointerleave = () => { tip.hide(); cross.setAttribute('visibility', 'hidden'); };
  return keys;
}

// --- the model timeline: one row per stay owner and label ---

function drawTimeline(box, spans, opts) {
  const now = opts.now, t0 = now - opts.range, t1 = now;
  if (!spans.length) return emptyChart(box, 'Nothing held the cards in this range.', 50);
  const rows = [];
  const idx = {};
  for (const s of spans) {
    const k = s.owner + '\x00' + s.label;
    if (!(k in idx)) { idx[k] = rows.length; rows.push({owner: s.owner, label: s.label, kind: s.kind, spans: []}); }
    rows[idx[k]].spans.push(s);
  }
  const narrow = box.clientWidth < 560;
  const RH = 30, LW = narrow ? 112 : 190;
  const h = rows.length * RH + 30;
  const [svg, w] = frame(box, h);
  const X0 = LW, X1 = w - 10, Y1 = h - 22;
  const x = t => X0 + (Math.max(t, t0) - t0) / (t1 - t0) * (X1 - X0);
  axes(svg, X0, X1, 4, Y1, t0, t1, [], () => '');
  for (const t of timeTicks(t0, t1, Math.max(2, Math.floor((X1 - X0) / 110)))) S('line', {x1: x(t), x2: x(t), y1: 4, y2: Y1, stroke: 'var(--line)'}, svg);
  const tip = tipFor(box);
  rows.forEach((r, i) => {
    const yc = 6 + i * RH;
    const clip = s => s.length > (narrow ? 15 : 26) ? s.slice(0, narrow ? 14 : 25) + '…' : s;
    const a = S('text', {x: LW - 10, y: yc + 10, 'text-anchor': 'end', class: 'tl-label'}, svg); a.textContent = clip(r.owner);
    const b = S('text', {x: LW - 10, y: yc + 22, 'text-anchor': 'end', class: 'tl-label b'}, svg); b.textContent = clip(r.label);
    for (const s of r.spans) {
      const st = Date.parse(s.start) / 1000, en = s.end ? Date.parse(s.end) / 1000 : now;
      if (en < t0) continue;
      const xa = x(st), xb = Math.max(x(en), xa + 3);
      const bar = S('rect', {x: xa, y: yc + 9, width: xb - xa, height: 10, rx: 2, fill: opts.color(r.kind, r.owner), opacity: s.end ? .75 : .95}, svg);
      bar.onpointermove = ev => {
        const br = box.getBoundingClientRect();
        tip.show(`<div class="h">${escT(r.owner)}</div><b>${escT(r.label)}</b><div>${fullClock(st)} → ${s.end ? fullClock(en) : 'now'}</div><div>${opts.dur(en - st)}${s.gpus && s.gpus.length ? ' · GPU ' + s.gpus.join('+') : ''}</div>`, ev.clientX - br.left, ev.clientY - br.top);
      };
      bar.onpointerleave = () => tip.hide();
    }
  });
  S('line', {class: 'now', x1: X1, x2: X1, y1: 4, y2: Y1}, svg);
}

// --- throughput: tokens per second per request, per engine ---

function drawRate(box, pts, field, opts) {
  const h = box.clientWidth < 500 ? 150 : 180;
  const now = opts.now, t0 = now - opts.range, t1 = now;
  const data = pts.filter(p => p[field] > 0 && p.t >= t0);
  if (!data.length) return emptyChart(box, opts.empty, 50);
  const [svg, w] = frame(box, h);
  const X0 = 46, X1 = w - 10, Y0 = 18, Y1 = h - 22;
  const max = Math.max(...data.map(p => p[field])) * 1.1;
  const step = niceStep(max, 4);
  const top = Math.ceil(max / step) * step;
  const x = t => X0 + (t - t0) / (t1 - t0) * (X1 - X0);
  const y = v => Y1 - v / top * (Y1 - Y0);
  const yt = [];
  for (let v = 0; v <= top + 1e-9; v += step) yt.push({v, y: y(v)});
  axes(svg, X0, X1, Y0, Y1, t0, t1, yt, v => v >= 1000 ? (v / 1000) + 'k' : String(v));
  const lab = S('text', {x: X0 + 4, y: Y0 - 6, class: 'tl-label'}, svg); lab.textContent = opts.title;
  const by = {};
  for (const p of data) (by[p.engine] = by[p.engine] || []).push(p);
  for (const [eng, ps] of Object.entries(by)) {
    const c = opts.color(eng);
    if (ps.length > 1) S('polyline', {points: ps.map(p => `${x(p.t).toFixed(1)},${y(p[field]).toFixed(1)}`).join(' '), fill: 'none', stroke: c, 'stroke-width': 1.2, opacity: .45}, svg);
    for (const p of ps) S('circle', {cx: x(p.t), cy: y(p[field]), r: 2.6, fill: c}, svg);
  }
  const tip = tipFor(box);
  svg.onpointermove = ev => {
    const r = svg.getBoundingClientRect();
    const px = (ev.clientX - r.left) / r.width * w, py = (ev.clientY - r.top) / r.height * h;
    let best = null, bd = 1e9;
    for (const p of data) { const d = Math.hypot(x(p.t) - px, y(p[field]) - py); if (d < bd) { bd = d; best = p; } }
    if (!best || bd > 40) return tip.hide();
    const br = box.getBoundingClientRect();
    tip.show(`<div class="h">${fullClock(best.t)}</div><div class="r"><span><i style="background:${opts.color(best.engine)}"></i>${escT(best.engine)}</span><b>${best[field].toLocaleString()} t/s</b></div>`, ev.clientX - br.left, ev.clientY - br.top);
  };
  svg.onpointerleave = () => tip.hide();
}

// --- context residency: each request's tokens against its window ---

function drawResidency(box, reqs, opts) {
  const h = box.clientWidth < 500 ? 150 : 170;
  if (!reqs.length) return emptyChart(box, 'No served requests with token counts yet.', 50);
  const [svg, w] = frame(box, h);
  const X0 = 46, X1 = w - 6, Y0 = 14, Y1 = h - 18;
  const tot = q => q.prompt + q.output;
  // Scaled to the requests; a window far above them would flatten every bar,
  // so it is drawn only when it fits and named otherwise.
  const most = Math.max(...reqs.map(tot)) || 1;
  const wins = reqs.map(q => q.window || 0).filter(Boolean);
  const fits = wins.filter(v => v <= most * 1.6);
  const max = Math.max(most, ...fits);
  const step = niceStep(max, 3);
  const top = Math.ceil(max / step) * step;
  const y = v => Y1 - v / top * (Y1 - Y0);
  const yt = [];
  for (let v = 0; v <= top + 1e-9; v += step) yt.push({v, y: y(v)});
  const g = S('g', {class: 'grid'}, svg), a = S('g', {class: 'axis'}, svg);
  for (const t of yt) { S('line', {x1: X0, x2: X1, y1: t.y, y2: t.y}, g); const tx = S('text', {x: X0 - 6, y: t.y + 3, 'text-anchor': 'end'}, a); tx.textContent = opts.tok(t.v); }
  const above = [...new Set(wins.filter(v => v > max))];
  if (above.length) { const n = S('text', {x: X1, y: Y0 - 3, 'text-anchor': 'end', class: 'tl-label'}, svg); n.textContent = 'window ' + above.map(opts.tok).join(' / ') + ', above the scale'; }
  const cw = (X1 - X0) / Math.max(reqs.length, 8);
  const tip = tipFor(box);
  reqs.forEach((q, i) => {
    const xa = X0 + i * cw + cw * .12, bw = cw * .76;
    const cached = Math.min(q.cached, q.prompt), fresh = Math.max(q.prompt - cached, 0);
    let base = 0;
    for (const [v, c] of [[cached, 'color-mix(in srgb,var(--green) 70%,var(--panel2))'], [fresh, 'var(--amber)'], [q.output, 'var(--cyan)']]) {
      if (v > 0) S('rect', {x: xa, y: y(base + v), width: bw, height: Math.max(y(base) - y(base + v), .8), fill: c, opacity: .85}, svg);
      base += v;
    }
    if (q.window && q.window <= max) S('line', {x1: xa - cw * .1, x2: xa + bw + cw * .1, y1: y(q.window), y2: y(q.window), stroke: 'var(--faint)', 'stroke-dasharray': '3 2'}, svg);
    const hit = S('rect', {x: X0 + i * cw, y: Y0, width: cw, height: Y1 - Y0, fill: 'transparent'}, svg);
    hit.onpointermove = ev => {
      const br = box.getBoundingClientRect();
      const pct = q.window ? ` · ${(tot(q) / q.window * 100).toFixed(1)}% of ${opts.tok(q.window)}` : '';
      tip.show(`<div class="h">${fullClock(Date.parse(q.at) / 1000)} · ${escT(q.engine)}</div><b>${escT(q.model || '')}</b>
        <div class="r"><span><i style="background:color-mix(in srgb,var(--green) 70%,var(--panel2))"></i>cached</span><span>${cached.toLocaleString()}</span></div>
        <div class="r"><span><i style="background:var(--amber)"></i>fresh</span><span>${fresh.toLocaleString()}</span></div>
        <div class="r"><span><i style="background:var(--cyan)"></i>generated</span><span>${q.output.toLocaleString()}</span></div>
        <div style="margin-top:4px">${tot(q).toLocaleString()} tokens${pct}</div>`, ev.clientX - br.left, ev.clientY - br.top);
    };
    hit.onpointerleave = () => tip.hide();
  });
}
