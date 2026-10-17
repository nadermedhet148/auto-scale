// dockscale web UI: a read-only dashboard over the controller's /api/*
// endpoints. No dependencies and no build step. Every value from the API is
// inserted with textContent, never innerHTML.

const POLL_MS = 5000;
const state = { overview: null, service: null, window: '1h', tab: 'overview', profileMetric: null };

const METRICS = {
  cpu: { title: 'CPU', noun: 'CPU', unit: '% of CPU limit, per replica' },
  memory: { title: 'Memory', noun: 'memory', unit: '% of memory limit, per replica' },
  rps: { title: 'Request rate', noun: 'request rate', unit: 'requests/s per replica' },
  latency_p95: { title: 'p95 latency', noun: 'p95 latency', unit: 'milliseconds' },
  queue_depth: { title: 'Queue backlog', noun: 'queue backlog', unit: 'messages per replica' },
};

// Verdicts map to status roles. Every status renders as icon + label.
const VERDICTS = {
  effective: ['good', '✓', 'effective'],
  ok: ['good', '✓', 'ok'],
  insufficient: ['serious', '▲', 'insufficient'],
  premature: ['serious', '▲', 'premature'],
  unnecessary: ['warning', '◆', 'unnecessary'],
};

// ---------- DOM helpers ----------

const $ = (sel) => document.querySelector(sel);

function h(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') e.className = v;
    else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    e.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return e;
}

const SVGNS = 'http://www.w3.org/2000/svg';
function sv(tag, attrs, ...kids) {
  const e = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs || {})) if (v != null) e.setAttribute(k, v);
  for (const kid of kids.flat()) if (kid != null) e.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  return e;
}

function status(role, icon, label) {
  return h('span', { class: `status ${role}` }, h('span', { class: 'icon', 'aria-hidden': 'true' }, icon), label);
}

function verdictBadge(entry) {
  if (entry.decision.dry_run) return status('muted', '○', 'dry run');
  if (!entry.outcome) return status('muted', '○', 'pending');
  const [role, icon, label] = VERDICTS[entry.outcome.verdict] || ['muted', '○', entry.outcome.verdict];
  return status(role, icon, label);
}

async function getJSON(url) {
  const r = await fetch(url, { cache: 'no-store' });
  if (!r.ok) {
    const err = new Error(`${r.status} ${r.statusText} for ${url}`);
    err.status = r.status;
    throw err;
  }
  return r.json();
}

// ---------- Formatting ----------

const nf1 = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 });
function fmtNum(v) {
  if (v == null || !Number.isFinite(v)) return '–';
  const a = Math.abs(v);
  if (a >= 1e6) return nf1.format(v / 1e6) + 'M';
  if (a >= 1e4) return nf1.format(v / 1e3) + 'K';
  return nf1.format(v);
}
const fmtClock = (ms) => new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
const fmtClockSec = (ms) => new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
const fmtDateTime = (iso) => new Date(iso).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
function fmtSecs(s) {
  if (s < 60) return `${Math.round(s)}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  if (s < 86400) return `${nf1.format(s / 3600)}h`;
  return `${nf1.format(s / 86400)}d`;
}
function fmtAgo(iso, nowMs) {
  if (!iso || iso.startsWith('0001-')) return 'never';
  const s = Math.max(0, (nowMs - Date.parse(iso)) / 1000);
  return s < 5 ? 'just now' : `${fmtSecs(s)} ago`;
}
const nsToSec = (ns) => ns / 1e9;

// ---------- Scales ----------

function niceTicks(lo, hi, count = 5) {
  if (!(hi > lo)) hi = lo + 1;
  const raw = (hi - lo) / count;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw);
  const top = Math.ceil(hi / step - 1e-9) * step;
  const ticks = [];
  for (let v = Math.ceil(lo / step) * step; v <= top + step / 1e6; v += step) ticks.push(Number(v.toFixed(10)));
  return { ticks, max: top };
}

function timeTicks(a, b, maxTicks) {
  const steps = [1, 2, 5, 10, 15, 30, 60, 120, 180, 360].map((m) => m * 60000);
  const step = steps.find((s) => (b - a) / s <= maxTicks) || steps[steps.length - 1];
  const out = [];
  for (let t = Math.ceil(a / step) * step; t <= b; t += step) out.push(t);
  return out;
}

function nearestIndex(xs, x) {
  let lo = 0, hi = xs.length - 1;
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1;
    if (xs[mid] < x) lo = mid; else hi = mid;
  }
  return Math.abs(xs[lo] - x) <= Math.abs(xs[hi] - x) ? lo : hi;
}

// ---------- Line chart ----------
//
// o = {
//   title, subtitle, empty,
//   xs: [number],                        sorted x values (ms, or slot index)
//   series: [{ name, color, step, area, values: [number|null] }],   drawn in order; last on top
//   band: { color, lo: [], hi: [] },     optional shaded range (±σ)
//   refs: [{ y, label }],                horizontal reference lines (target, min, max)
//   vlines: [{ x, label }],              vertical reference lines ("now")
//   extra: [{ name, values, fmt }],      shown in tooltip and table, not drawn
//   xFmt, xTipFmt, xTicks(lo, hi, n), xLabel, yFmt, yMaxHint, height
// }
// Chart state (table open, hovered x) lives on the figure, so the 5s polling
// re-render keeps the reader's place.
function lineChart(fig, o) {
  fig.replaceChildren();
  const multi = o.series.length > 1;
  const tableOpen = fig.dataset.table === '1';
  const tableBtn = h('button', { class: 'ghost small', type: 'button', 'aria-pressed': String(tableOpen) }, 'Table');
  const head = h('div', { class: 'fig-head' },
    h('div', {}, h('h3', {}, o.title), o.subtitle ? h('p', { class: 'sub' }, o.subtitle) : null),
    h('div', { class: 'fig-tools' },
      multi ? h('ul', { class: 'legend' }, o.series.map((s) =>
        h('li', {}, h('span', { class: 'key', style: `background:var(${s.color})`, 'aria-hidden': 'true' }), s.name))) : null,
      o.xs.length ? tableBtn : null));
  const plot = h('div', { class: 'plot' });
  fig.append(head, plot);
  if (!o.xs.length) {
    plot.append(h('p', { class: 'empty' }, o.empty || 'No data yet.'));
    return;
  }
  const table = dataTable(o);
  table.hidden = !tableOpen;
  fig.append(table);
  tableBtn.addEventListener('click', () => {
    table.hidden = !table.hidden;
    fig.dataset.table = table.hidden ? '' : '1';
    tableBtn.setAttribute('aria-pressed', String(!table.hidden));
  });

  let lastW = 0;
  const draw = () => {
    const W = Math.max(280, plot.clientWidth);
    if (W === lastW) return;
    lastW = W;
    plot.replaceChildren(render(fig, o, W));
  };
  fig._ro?.disconnect();
  fig._ro = new ResizeObserver(draw);
  fig._ro.observe(plot);
  draw();
}

function render(fig, o, W) {
  const H = o.height || 220;
  const m = { l: 44, r: o.series.length <= 4 ? 84 : 12, t: 14, b: 26 };
  const iw = W - m.l - m.r, ih = H - m.t - m.b;
  const xs = o.xs, x0 = xs[0], x1 = xs[xs.length - 1];
  const X = (v) => m.l + (x1 === x0 ? iw / 2 : ((v - x0) / (x1 - x0)) * iw);

  let dataMax = 0;
  for (const s of o.series) for (const v of s.values) if (v != null && v > dataMax) dataMax = v;
  if (o.band) for (const v of o.band.hi) if (v != null && v > dataMax) dataMax = v;
  for (const r of o.refs || []) dataMax = Math.max(dataMax, r.y);
  const { ticks, max: yMax } = niceTicks(0, Math.max(dataMax * 1.08, o.yMaxHint || 0, 1));
  const Y = (v) => m.t + ih - (v / yMax) * ih;
  const yFmt = o.yFmt || fmtNum;

  const root = h('div', {});
  const svg = sv('svg', { width: W, height: H, viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': `${o.title} chart` });
  root.append(svg);

  for (const t of ticks) {
    svg.append(sv('line', { class: t === 0 ? 'axis-line' : 'grid-line', x1: m.l, x2: m.l + iw, y1: Y(t), y2: Y(t) }));
    svg.append(sv('text', { class: 'tick', x: m.l - 6, y: Y(t) + 4, 'text-anchor': 'end' }, yFmt(t)));
  }
  const xt = (o.xTicks || timeTicks)(x0, x1, Math.max(2, Math.floor(iw / 90)));
  for (const t of xt) svg.append(sv('text', { class: 'tick', x: X(t), y: H - 8, 'text-anchor': 'middle' }, (o.xFmt || fmtClock)(t)));

  if (o.band) svg.append(sv('path', { d: bandPath(xs, o.band.lo, o.band.hi, X, Y), fill: `var(${o.band.color})`, 'fill-opacity': 0.1, stroke: 'none' }));

  for (const r of o.refs || []) {
    svg.append(sv('line', { class: 'ref', x1: m.l, x2: m.l + iw, y1: Y(r.y), y2: Y(r.y) }));
    svg.append(sv('text', { class: 'ref-label', x: m.l + iw - 4, y: Y(r.y) - 4, 'text-anchor': 'end' }, r.label));
  }
  for (const v of o.vlines || []) {
    if (v.x < x0 || v.x > x1) continue;
    svg.append(sv('line', { class: 'ref', x1: X(v.x), x2: X(v.x), y1: m.t, y2: m.t + ih }));
    svg.append(sv('text', { class: 'ref-label', x: X(v.x) + 4, y: m.t + 10 }, v.label));
  }

  for (const s of o.series) {
    if (s.area) svg.append(sv('path', { d: areaPath(xs, s.values, X, Y, s.step), fill: `var(${s.color})`, 'fill-opacity': 0.1, stroke: 'none' }));
    svg.append(sv('path', {
      d: linePath(xs, s.values, X, Y, s.step), fill: 'none', stroke: `var(${s.color})`,
      'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round',
    }));
  }

  // End markers, plus direct labels when they don't collide.
  const ends = o.series.map((s) => {
    let i = s.values.length - 1;
    while (i >= 0 && s.values[i] == null) i--;
    return i < 0 ? null : { s, i, y: Y(s.values[i]) };
  }).filter(Boolean);
  const sorted = [...ends].sort((a, b) => a.y - b.y);
  const collide = sorted.some((e, k) => k > 0 && e.y - sorted[k - 1].y < 13);
  for (const e of ends) {
    svg.append(sv('circle', { cx: X(xs[e.i]), cy: e.y, r: 4, fill: `var(${e.s.color})`, stroke: 'var(--surface)', 'stroke-width': 2 }));
    if (m.r > 12 && !collide) {
      const label = o.series.length > 1 ? `${e.s.name} ${yFmt(e.s.values[e.i])}` : yFmt(e.s.values[e.i]);
      svg.append(sv('text', { class: 'end-label', x: X(xs[e.i]) + 8, y: e.y + 4 }, label));
    }
  }

  // Hover / focus layer: a crosshair that snaps to the nearest x.
  const cross = sv('line', { class: 'cross', y1: m.t, y2: m.t + ih, visibility: 'hidden' });
  const dots = sv('g', { visibility: 'hidden' });
  const hit = sv('rect', { class: 'hit', x: m.l, y: m.t, width: iw, height: ih, tabindex: 0, 'aria-label': `${o.title}: use arrow keys to read values` });
  svg.append(cross, dots, hit);
  const tip = h('div', { class: 'tip', hidden: true });
  root.append(tip);

  const show = (i) => {
    fig._hover = xs[i];
    fig._hoverIndex = i;
    const cx = X(xs[i]);
    cross.setAttribute('x1', cx); cross.setAttribute('x2', cx); cross.setAttribute('visibility', 'visible');
    dots.replaceChildren();
    const rows = [];
    for (const s of o.series) {
      const v = s.values[i];
      if (v != null) dots.append(sv('circle', { cx, cy: Y(v), r: 4, fill: `var(${s.color})`, stroke: 'var(--surface)', 'stroke-width': 2 }));
      rows.push(h('div', { class: 't-row' }, h('span', { class: 'key', style: `background:var(${s.color})` }), h('b', {}, yFmt(v)), h('span', {}, s.name)));
    }
    for (const r of o.refs || []) rows.push(h('div', { class: 't-row muted' }, h('b', {}, yFmt(r.y)), h('span', {}, r.label.replace(/\s[\d.,KM]+$/, ''))));
    for (const e of o.extra || []) rows.push(h('div', { class: 't-row muted' }, h('b', {}, (e.fmt || fmtNum)(e.values[i])), h('span', {}, e.name)));
    tip.replaceChildren(h('div', { class: 't-head' }, (o.xTipFmt || o.xFmt || fmtClockSec)(xs[i])), ...rows);
    tip.hidden = false;
    const tw = tip.offsetWidth || 160;
    tip.style.left = `${cx + 12 + tw > W ? cx - 12 - tw : cx + 12}px`;
    dots.setAttribute('visibility', 'visible');
  };
  const hide = () => {
    fig._hover = undefined;
    cross.setAttribute('visibility', 'hidden');
    dots.setAttribute('visibility', 'hidden');
    tip.hidden = true;
  };
  const fromPointer = (ev) => {
    const r = svg.getBoundingClientRect();
    const px = ev.clientX - r.left;
    return nearestIndex(xs, x0 + ((px - m.l) / iw) * (x1 - x0));
  };
  hit.addEventListener('pointermove', (ev) => show(fromPointer(ev)));
  hit.addEventListener('pointerleave', hide);
  hit.addEventListener('focus', () => show(fig._hoverIndex ?? xs.length - 1));
  hit.addEventListener('blur', hide);
  hit.addEventListener('keydown', (ev) => {
    const cur = fig._hoverIndex ?? xs.length - 1;
    if (ev.key === 'ArrowLeft') { show(Math.max(0, cur - 1)); ev.preventDefault(); }
    if (ev.key === 'ArrowRight') { show(Math.min(xs.length - 1, cur + 1)); ev.preventDefault(); }
    if (ev.key === 'Escape') hide();
  });
  if (fig._hover !== undefined) requestAnimationFrame(() => show(nearestIndex(xs, fig._hover)));
  return root;
}

function linePath(xs, ys, X, Y, step) {
  let d = '', open = false;
  for (let i = 0; i < xs.length; i++) {
    const v = ys[i];
    if (v == null) { open = false; continue; }
    const x = X(xs[i]), y = Y(v);
    if (!open) d += `M${x},${y}`;
    else if (step) d += `H${x}V${y}`;
    else d += `L${x},${y}`;
    open = true;
  }
  return d || 'M0,0';
}

// areaPath fills from a line down to zero, one closed shape per unbroken run.
function areaPath(xs, ys, X, Y, step) {
  let d = '', start = -1;
  const close = () => { if (start >= 0) d += `V${Y(0)}H${X(xs[start])}Z`; start = -1; };
  for (let i = 0; i < xs.length; i++) {
    const v = ys[i];
    if (v == null) { close(); continue; }
    const x = X(xs[i]), y = Y(v);
    if (start < 0) { d += `M${x},${Y(0)}V${y}`; start = i; }
    else if (step) d += `H${x}V${y}`;
    else d += `L${x},${y}`;
  }
  close();
  return d || 'M0,0';
}

function bandPath(xs, lo, hi, X, Y) {
  let d = '', seg = [];
  const flush = () => {
    if (seg.length > 1) {
      d += 'M' + seg.map((i) => `${X(xs[i])},${Y(hi[i])}`).join('L');
      d += 'L' + [...seg].reverse().map((i) => `${X(xs[i])},${Y(lo[i])}`).join('L') + 'Z';
    }
    seg = [];
  };
  for (let i = 0; i < xs.length; i++) {
    if (lo[i] == null || hi[i] == null) flush(); else seg.push(i);
  }
  flush();
  return d || 'M0,0';
}

// The table view: the accessible twin of every chart. Newest rows first.
function dataTable(o) {
  const cols = [...o.series.map((s) => ({ name: s.name, values: s.values, fmt: o.yFmt })), ...(o.extra || [])];
  const head = h('tr', {}, h('th', {}, o.xLabel || 'Time'), cols.map((c) => h('th', { class: 'num' }, c.name)));
  const rows = [];
  for (let i = o.xs.length - 1; i >= 0 && rows.length < 500; i--) {
    rows.push(h('tr', {}, h('td', { class: 'when' }, (o.xTipFmt || o.xFmt || fmtClockSec)(o.xs[i])),
      cols.map((c) => h('td', { class: 'num' }, (c.fmt || fmtNum)(c.values[i])))));
  }
  return h('div', { class: 'data-table' }, h('table', {}, h('thead', {}, head), h('tbody', {}, rows)));
}

// ---------- Views ----------

function currentService() {
  return state.overview?.services.find((s) => s.name === state.service);
}

function tile(label, value, note, hero) {
  return h('div', { class: 'tile' },
    h('div', { class: 'label' }, label),
    h('div', { class: hero ? 'value hero' : 'value' }, value),
    note ? h('div', { class: 'note' }, note) : null);
}

async function renderOverview() {
  const svc = currentService();
  if (!svc) return;
  const o = state.overview, st = svc.status, nowMs = Date.parse(o.now);

  const svcs = $('#services');
  svcs.hidden = o.services.length < 2;
  if (!svcs.hidden) {
    svcs.replaceChildren(h('div', { class: 'table-wrap' }, h('table', {},
      h('thead', {}, h('tr', {}, ['Service', 'Replicas', 'Desired', 'Model', 'Health'].map((c, i) => h('th', { class: i && i < 3 ? 'num' : null }, c)))),
      h('tbody', {}, o.services.map((s) => h('tr', {},
        h('td', {}, h('a', { href: '#', onclick: (ev) => { ev.preventDefault(); selectService(s.name); } }, s.name)),
        h('td', { class: 'num' }, s.status.replicas), h('td', { class: 'num' }, s.status.desired),
        h('td', {}, s.model + (s.prewarm ? ' + prewarm' : '')),
        h('td', {}, s.status.last_error ? status('critical', '✕', 'error') : status('good', '✓', 'healthy'))))))));
  }

  const health = st.last_error
    ? h('span', { title: st.last_error }, status('critical', '✕', 'error'))
    : status('good', '✓', 'healthy');
  $('#tiles').replaceChildren(
    tile('Replicas', st.replicas, `min ${svc.min} · max ${svc.max}`, true),
    tile('Desired', st.desired, `metrics recommend ${st.recommended}`),
    tile('Last scale', fmtAgo(st.last_scale, nowMs), `model ${svc.model}${svc.prewarm ? ' + prewarm' : ''}`),
    tile('Health', health, st.last_error || `updated ${fmtAgo(st.updated_at, nowMs)}`),
  );
  $('#reason').replaceChildren(h('b', {}, 'Latest reasoning'), st.reason || '—');

  const pts = await getJSON(`/api/series?service=${encodeURIComponent(svc.name)}&window=${state.window}`);
  const xs = pts.map((p) => Date.parse(p.at));
  lineChart($('#chart-replicas'), {
    title: 'Replicas',
    subtitle: 'Running replicas and the policy’s desired count',
    xs,
    series: [
      { name: 'running', color: '--series-1', step: true, area: true, values: pts.map((p) => p.replicas) },
      { name: 'desired', color: '--series-2', step: true, values: pts.map((p) => p.desired) },
    ],
    refs: [{ y: svc.max, label: `max ${svc.max}` }, { y: svc.min, label: `min ${svc.min}` }],
    yMaxHint: svc.max + 1,
    yFmt: (v) => (v == null ? '–' : String(Math.round(v))),
    empty: 'No ticks recorded yet. Charts fill in as the control loop runs.',
  });

  const grid = $('#signal-charts');
  const keep = new Set();
  for (const metric of svc.metrics) {
    const id = `sig-${metric.type}`;
    keep.add(id);
    let fig = document.getElementById(id);
    if (!fig) {
      fig = h('figure', { id, class: 'card chart' });
      grid.append(fig);
    }
    const pick = (name, field) => pts.map((p) => {
      const s = (p.signals || []).find((x) => x.metric === name);
      return s ? s[field] : null;
    });
    const hist = pick(`${metric.type}@history`, 'value');
    // Drawn in order, last on top: raw measurement as muted context, then
    // the history forecast, then the estimate the policy acts on.
    const series = [{ name: 'measured', color: '--deemph', values: pick(metric.type, 'raw') }];
    if (hist.some((v) => v != null)) series.push({ name: 'usual (history)', color: '--series-3', values: hist });
    series.push({ name: 'estimate', color: '--series-1', values: pick(metric.type, 'value') });
    const info = METRICS[metric.type] || { title: metric.type, unit: '' };
    lineChart(fig, {
      title: info.title,
      subtitle: `${info.unit}. The policy acts on the estimate.`,
      xs, series,
      refs: [{ y: metric.target, label: `target ${fmtNum(metric.target)}` }],
      empty: 'No samples yet.',
    });
  }
  for (const fig of [...grid.children]) if (!keep.has(fig.id)) fig.remove();
}

async function renderDecisions() {
  const svc = currentService();
  if (!svc) return;
  const q = `service=${encodeURIComponent(svc.name)}`;
  const [rep, hist] = await Promise.all([getJSON(`/api/report?${q}`), getJSON(`/api/history?${q}&limit=100`)]);
  const v = rep.verdicts || {};

  const count = (role, icon, label, n) => h('div', { class: 'tile' },
    h('div', { class: 'label' }, status(role, icon, label)),
    h('div', { class: 'value' }, n || 0));
  $('#verdict-tiles').replaceChildren(
    tile('Decisions', rep.decisions, `${rep.ups} up · ${rep.downs} down · ${nf1.format(rep.scale_events_per_hour)}/hour`),
    count('good', '✓', 'Scale-ups effective', v.effective),
    count('serious', '▲', 'Scale-ups insufficient', v.insufficient),
    count('warning', '◆', 'Scale-ups unnecessary', v.unnecessary),
    count('good', '✓', 'Scale-downs ok', v.ok),
    count('serious', '▲', 'Scale-downs premature', v.premature),
  );
  const advice = [h('h3', {}, 'Tuning advice'), h('ul', {}, (rep.advice || []).map((a) => h('li', {}, a)))];
  if (!state.overview.memory) advice.push(h('p', { class: 'sub' }, 'Memory is off (no memory.dir), so this covers only decisions since dockscale started.'));
  $('#advice').replaceChildren(...advice);

  const table = $('#decisions');
  if (!hist.length) {
    table.replaceChildren(h('tbody', {}, h('tr', {}, h('td', { class: 'empty' }, 'No scaling decisions yet.'))));
    return;
  }
  table.replaceChildren(
    h('thead', {}, h('tr', {}, h('th', {}, 'When'), h('th', {}, 'Change'), h('th', {}, 'Verdict'), h('th', { class: 'num' }, 'Overloaded'), h('th', {}, 'Reason'))),
    h('tbody', {}, hist.map((e) => {
      const d = e.decision;
      const arrow = d.to > d.from ? '↑' : '↓';
      const change = `${arrow} ${d.from} → ${d.to}` + (d.to !== d.wanted ? ` (wanted ${d.wanted})` : '');
      return h('tr', {},
        h('td', { class: 'when' }, fmtDateTime(d.at)),
        h('td', { class: 'when' }, change),
        h('td', {}, verdictBadge(e)),
        h('td', { class: 'num' }, e.outcome ? `${Math.round(100 * e.outcome.overload_fraction)}%` : '–'),
        h('td', { class: 'code' }, d.reason + (d.error ? ` — error: ${d.error}` : '')));
    })));
}

async function renderProfile() {
  const svc = currentService();
  if (!svc) return;
  const fig = $('#chart-profile');
  const sel = $('#profile-metric');
  let prof;
  try {
    prof = await getJSON(`/api/profile?service=${encodeURIComponent(svc.name)}`);
  } catch (err) {
    if (err.status !== 404) throw err;
    sel.replaceChildren();
    lineChart(fig, { title: 'Load profile', xs: [], series: [], empty: 'No load profile for this service yet.' });
    return;
  }
  const order = svc.metrics.map((m) => m.type);
  const metrics = Object.keys(prof.metrics).sort((a, b) => (order.indexOf(a) + 99) % 99 - (order.indexOf(b) + 99) % 99);
  if (!metrics.includes(state.profileMetric)) state.profileMetric = metrics[0];
  sel.replaceChildren(...metrics.map((m) => h('option', { value: m, selected: m === state.profileMetric }, (METRICS[m]?.title || m))));
  if (!metrics.length) {
    lineChart(fig, { title: 'Load profile', xs: [], series: [], empty: 'The profile has no completed time slots yet.' });
    return;
  }

  const slots = prof.metrics[state.profileMetric];
  const xs = slots.map((_, i) => i);
  const mean = slots.map((s) => (s.n ? s.mean : null));
  const weekly = prof.cycle_seconds >= 7 * 86400;
  const days = ['Thu', 'Fri', 'Sat', 'Sun', 'Mon', 'Tue', 'Wed']; // the Unix epoch was a Thursday
  const slotLabel = (i) => {
    const secs = i * prof.slot_seconds;
    const hh = String(Math.floor((secs % 86400) / 3600)).padStart(2, '0');
    const mm = String(Math.floor((secs % 3600) / 60)).padStart(2, '0');
    return (weekly ? `${days[Math.floor(secs / 86400) % 7]} ` : '') + `${hh}:${mm}`;
  };
  const perHour = 3600 / prof.slot_seconds;
  const noun = METRICS[state.profileMetric]?.noun || state.profileMetric;
  const visited = slots.filter((s) => s.n).length;
  lineChart(fig, {
    title: `Usual ${noun} by time of ${weekly ? 'week' : 'day'}`,
    subtitle: `Service-wide total, mean ± 1σ per ${fmtSecs(prof.slot_seconds)} slot (UTC). ${visited} of ${slots.length} slots learned.`,
    xs,
    series: [{ name: 'mean', color: '--series-1', values: mean }],
    band: {
      color: '--series-1',
      lo: slots.map((s) => (s.n ? Math.max(0, s.mean - s.stddev) : null)),
      hi: slots.map((s) => (s.n ? s.mean + s.stddev : null)),
    },
    vlines: [{ x: prof.now_slot, label: 'now' }],
    extra: [
      { name: '± σ', values: slots.map((s) => (s.n ? s.stddev : null)) },
      { name: 'visits', values: slots.map((s) => s.n), fmt: (v) => String(v ?? 0) },
    ],
    xLabel: 'Slot (UTC)',
    xFmt: slotLabel,
    xTicks: (lo, hi, n) => {
      const hours = [1, 2, 3, 4, 6, 12, 24, 48].find((hrs) => (hi - lo) / (hrs * perHour) <= n) || 24;
      const step = Math.max(1, Math.round(hours * perHour));
      const out = [];
      for (let i = 0; i <= hi; i += step) out.push(i);
      return out;
    },
    empty: 'No data for this metric.',
  });
}

// ---------- Shell ----------

function renderShell() {
  const o = state.overview;
  const banner = $('#banner');
  banner.hidden = !o.banner;
  banner.textContent = o.banner || '';
  $('#meta').replaceChildren(
    h('span', {}, o.dry_run ? 'dry run (no containers changed)' : 'live'),
    h('span', {}, `tick every ${fmtSecs(nsToSec(o.interval))}`),
    h('span', {}, o.memory ? 'memory on' : 'memory off'),
    h('span', {}, `server time ${fmtClockSec(Date.parse(o.now))}`),
  );
  const sel = $('#service');
  const names = o.services.map((s) => s.name);
  if (!names.includes(state.service)) state.service = names[0];
  if ([...sel.options].map((x) => x.value).join() !== names.join()) {
    sel.replaceChildren(...names.map((n) => h('option', { value: n }, n)));
  }
  sel.value = state.service;
  $('#range').hidden = state.tab !== 'overview';
}

// A refresh requested mid-flight (a click during a poll) runs right after the
// current one instead of being dropped, so the view never lags a click by 5s.
let inflight = false, again = false;
async function refresh() {
  if (inflight) { again = true; return; }
  inflight = true;
  const main = document.querySelector('main');
  try {
    state.overview = await getJSON('/api/overview');
    renderShell();
    if (state.tab === 'overview') await renderOverview();
    if (state.tab === 'decisions') await renderDecisions();
    if (state.tab === 'profile') await renderProfile();
    $('#alert').hidden = true;
    main.classList.remove('stale');
  } catch (err) {
    // Keep the last render, dimmed, and say why.
    main.classList.add('stale');
    const a = $('#alert');
    a.textContent = `Can't reach the dockscale API: ${err.message}. Retrying every ${POLL_MS / 1000}s.`;
    a.hidden = false;
  } finally {
    inflight = false;
    if (again) { again = false; refresh(); }
  }
}

function selectService(name) {
  state.service = name;
  save('service', name);
  refresh();
}

function selectTab(tab) {
  state.tab = tab;
  save('tab', tab);
  for (const b of document.querySelectorAll('.tabs button')) b.setAttribute('aria-selected', String(b.dataset.tab === tab));
  for (const s of document.querySelectorAll('main > section')) s.hidden = s.id !== `tab-${tab}`;
  refresh();
}

// Per-viewer conveniences only; the page works without storage.
function load(k) { try { return localStorage.getItem(`dockscale.${k}`); } catch { return null; } }
function save(k, v) { try { localStorage.setItem(`dockscale.${k}`, v); } catch { /* storage unavailable */ } }

function applyTheme(t) {
  const root = document.documentElement;
  if (t === 'auto') delete root.dataset.theme; else root.dataset.theme = t;
  const btn = $('#theme');
  btn.textContent = `Theme: ${t}`;
  btn.title = t === 'auto' ? 'Theme: follows your system' : `Theme: ${t}`;
}

function init() {
  state.service = load('service');
  state.window = load('window') || '1h';
  let theme = load('theme') || 'auto';
  applyTheme(theme);
  $('#theme').addEventListener('click', () => {
    theme = { auto: 'light', light: 'dark', dark: 'auto' }[theme];
    save('theme', theme);
    applyTheme(theme);
  });
  $('#service').addEventListener('change', (ev) => selectService(ev.target.value));
  $('#profile-metric').addEventListener('change', (ev) => { state.profileMetric = ev.target.value; refresh(); });
  for (const b of document.querySelectorAll('#range button')) {
    b.setAttribute('aria-pressed', String(b.dataset.window === state.window));
    b.addEventListener('click', () => {
      state.window = b.dataset.window;
      save('window', state.window);
      for (const x of document.querySelectorAll('#range button')) x.setAttribute('aria-pressed', String(x === b));
      refresh();
    });
  }
  for (const b of document.querySelectorAll('.tabs button')) b.addEventListener('click', () => selectTab(b.dataset.tab));
  selectTab(load('tab') || 'overview');
  setInterval(() => { if (!document.hidden) refresh(); }, POLL_MS);
}

init();
