// Ting explorer: a single page over the node's /v1 API. Every value shown
// comes from that API; nothing here is trusted beyond display, and storage
// proofs are checked in the browser against the state root.
'use strict';
(() => {
  const RESOURCES = [
    { key: 'C', name: 'Compute' },
    { key: 'S', name: 'State access' },
    { key: 'D', name: 'Data' },
    { key: 'G', name: 'Growth' },
  ];
  // Receipt statuses: a status color always comes with an icon and a label.
  const STATUS = {
    Success: ['good', '✓', 'Success'],
    Revert: ['serious', '↺', 'Reverted'],
    Trap: ['critical', '!', 'Trapped'],
    Compute: ['warning', 'C', 'Out of compute'],
    Access: ['warning', 'S', 'Out of state access'],
    Data: ['warning', 'D', 'Out of data'],
    Growth: ['warning', 'G', 'Out of growth'],
    Structure: ['critical', '!', 'Structure limit'],
    Missing: ['critical', '?', 'Target missing'],
    InvalidModule: ['critical', '!', 'Invalid module'],
    Exists: ['critical', '=', 'Already exists'],
    ReadOnly: ['critical', '!', 'Read-only write'],
  };
  const VOTE = ['hold', 'increase', 'decrease'];

  // ---- DOM ----------------------------------------------------------------
  const $ = (sel, root = document) => root.querySelector(sel);
  function append(node, kids) {
    for (const k of kids.flat(Infinity)) {
      if (k == null || k === false) continue;
      node.append(k instanceof Node ? k : document.createTextNode(String(k)));
    }
    return node;
  }
  function el(tag, props, ...kids) {
    const e = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (v == null || v === false) continue;
      if (k === 'class') e.className = v;
      else if (k === 'text') e.textContent = v;
      else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
      else e.setAttribute(k, v === true ? '' : v);
    }
    return append(e, kids);
  }
  const SVGNS = 'http://www.w3.org/2000/svg';
  function sv(tag, attrs, ...kids) {
    const e = document.createElementNS(SVGNS, tag);
    for (const [k, v] of Object.entries(attrs || {})) if (v != null) e.setAttribute(k, v);
    return append(e, kids);
  }

  // ---- Formatting ---------------------------------------------------------
  const nf = new Intl.NumberFormat('en-US');
  const fmt = (n) => nf.format(n);
  const trimZeros = (x) => x.replace(/\.0+$/, '').replace(/(\.\d*?[1-9])0+$/, '$1');
  function compact(n, digits = 1) {
    const a = Math.abs(n);
    for (const [v, u] of [[1e18, 'E'], [1e15, 'P'], [1e12, 'T'], [1e9, 'G'], [1e6, 'M'], [1e3, 'K']]) {
      if (a >= v) return trimZeros((n / v).toFixed(a / v >= 100 ? 0 : digits)) + u;
    }
    return Number.isInteger(n) ? String(n) : trimZeros(n.toFixed(digits));
  }
  function bytes(n) {
    if (n < 1024) return `${n} B`;
    if (n < 1048576) return `${trimZeros((n / 1024).toFixed(1))} KiB`;
    return `${trimZeros((n / 1048576).toFixed(1))} MiB`;
  }
  function dur(sec) {
    sec = Math.round(sec);
    if (sec < 0) return `−${dur(-sec)}`;
    if (sec < 60) return `${sec} s`;
    const m = Math.floor(sec / 60);
    if (m < 60) return `${m} min ${String(sec % 60).padStart(2, '0')} s`;
    const h = Math.floor(m / 60);
    if (h < 48) return `${h} h ${m % 60} min`;
    return `${Math.floor(h / 24)} d ${h % 24} h`;
  }
  function ago(ts) {
    const d = Date.now() / 1000 - ts;
    if (d < -5) return `in ${dur(-d)}`;
    if (d < 5) return 'just now';
    return `${dur(d)} ago`;
  }
  const when = (ts) => new Date(ts * 1000).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'medium' });
  const short = (h, a = 8, b = 6) => (h.length <= a + b + 1 ? h : `${h.slice(0, a)}…${h.slice(-b)}`);
  const pct = (x, d = 1) => `${trimZeros((x * 100).toFixed(d))}%`;
  // A share with as many decimals as a small value needs.
  function share(x) {
    if (x <= 0) return '0%';
    if (x < 0.0001) return '<0.01%';
    return pct(x, x < 0.01 ? 2 : 1);
  }
  // A short duration for axis ticks: 45s, 2m, 1.5h.
  function shortDur(v) {
    const a = Math.abs(v);
    if (a < 60) return `${trimZeros(v.toFixed(0))}s`;
    if (a < 3600) return `${trimZeros((v / 60).toFixed(1))}m`;
    return `${trimZeros((v / 3600).toFixed(1))}h`;
  }
  const rate = (h) => `${compact(h)} H/s`;
  const plural = (n, one, many) => `${fmt(n)} ${n === 1 ? one : many}`;
  function hexBytes(h) {
    const b = new Uint8Array(h.length / 2);
    for (let i = 0; i < b.length; i++) b[i] = parseInt(h.substr(2 * i, 2), 16);
    return b;
  }
  const toHex = (b) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
  function asText(hex) {
    if (!hex) return null;
    const b = hexBytes(hex);
    if (!b.length || b.some((c) => c < 32 || c > 126)) return null;
    return new TextDecoder().decode(b);
  }
  function asU64(hex) {
    if (!hex || hex.length !== 16) return null;
    let v = 0n;
    for (let i = 7; i >= 0; i--) v = (v << 8n) | BigInt(parseInt(hex.substr(2 * i, 2), 16));
    return v;
  }
  function blockWork(targetHex) {
    return Number((1n << 256n) / (BigInt(`0x${targetHex}`) + 1n));
  }

  // ---- Small components ---------------------------------------------------
  function copyButton(text) {
    const b = el('button', { class: 'copy', type: 'button', title: 'Copy', 'aria-label': 'Copy to clipboard', text: '⧉' });
    b.addEventListener('click', async (e) => {
      e.preventDefault();
      try {
        await navigator.clipboard.writeText(text);
        b.textContent = '✓';
        b.classList.add('done');
        setTimeout(() => { b.textContent = '⧉'; b.classList.remove('done'); }, 1200);
      } catch { /* clipboard refused: nothing to do */ }
    });
    return b;
  }
  function hashLink(kind, id, full) {
    return el('span', null,
      el('a', { class: full ? 'full-hash' : 'hash', href: `/${kind}/${id}`, title: id, text: full ? id : short(id) }),
      copyButton(id));
  }
  const fullHash = (id) => el('span', null, el('span', { class: 'full-hash', text: id }), copyButton(id));
  const blockLink = (b) => el('a', { href: `/blocks/${b}`, text: typeof b === 'number' ? fmt(b) : short(b) });
  function statusBadge(name) {
    const [cls, icon, label] = STATUS[name] || ['neutral', '?', name];
    return el('span', { class: `badge ${cls}` }, el('span', { class: 'icon', 'aria-hidden': 'true', text: icon }), label);
  }
  function card(title, note, ...body) {
    const c = el('section', { class: 'card' });
    if (title) c.append(el('div', { class: 'card-head' }, el('h2', { text: title }), note ? el('span', { class: 'note' }, note) : null));
    return append(c, body);
  }
  function tile(label, value, detail) {
    return el('div', { class: 'card tile' }, el('div', { class: 'label', text: label }),
      el('div', { class: 'value' }, value), detail ? el('div', { class: 'detail' }, detail) : null);
  }
  function facts(rows) {
    const dl = el('dl', { class: 'facts' });
    for (const [k, v] of rows) if (v != null && v !== false) dl.append(el('dt', { text: k }), el('dd', null, v));
    return dl;
  }
  function table(cols, rows) {
    const t = el('table');
    t.append(el('thead', null, el('tr', null, cols.map((c) => el('th', { class: c.num ? 'num' : null, scope: 'col', text: c.name })))));
    const body = el('tbody');
    for (const r of rows) body.append(el('tr', null, r.map((v, i) => el('td', { class: cols[i].num ? 'num' : cols[i].cls || null }, v))));
    t.append(body);
    return el('div', { class: 'table-wrap' }, t);
  }
  function tableView(cols, rows) {
    return el('details', { class: 'table-view' }, el('summary', { text: 'Show the data as a table' }), table(cols, rows));
  }
  function meter(name, used, limit, opts = {}) {
    const f = limit ? Math.min(1, used / limit) : 0;
    const bar = el('div', {
      class: `meter${used ? '' : ' zero'}`, role: 'meter', 'aria-label': name,
      'aria-valuemin': '0', 'aria-valuemax': String(limit), 'aria-valuenow': String(used),
    });
    const fill = el('div', { class: `fill${opts.alt ? ' alt' : ''}${!opts.alt && f > 0.95 ? ' warn' : ''}` });
    fill.style.width = `${f * 100}%`;
    bar.append(fill);
    for (const t of opts.ticks || []) {
      const tick = el('div', { class: 'tick', title: t.title });
      tick.style.left = `calc(${t.at * 100}% - 1px)`;
      bar.append(tick);
    }
    return el('div', { class: 'meter-row' }, el('span', { class: 'name', text: name }), bar,
      el('span', { class: 'val', text: opts.text || `${compact(used)} / ${compact(limit)} · ${share(f)}` }));
  }
  function hbars(rows, opts = {}) {
    const max = Math.max(1, ...rows.map((r) => r.value));
    const box = el('div', { class: 'hbars' });
    for (const r of rows) {
      const bar = el('div', { class: 'hbar' });
      bar.style.width = `${(r.value / max) * 62}%`;
      box.append(el('div', { class: 'hbar-row' }, el('span', { class: 'name' }, r.label),
        el('div', { class: 'hbar-track' }, bar, el('span', { class: 'hbar-val', text: opts.format ? opts.format(r) : fmt(r.value) }))));
    }
    return box;
  }

  // ---- Tooltip ------------------------------------------------------------
  const tooltip = () => $('#tooltip');
  function showTip(x, y, title, rows) {
    const t = tooltip();
    t.replaceChildren();
    if (title) t.append(el('div', { class: 'tt-title', text: title }));
    for (const r of rows) {
      const label = el('span', { class: 'tt-label' });
      if (r.key) label.append(el('span', { class: `tt-key${r.key === 'alt' ? ' alt' : ''}` }));
      label.append(r.label);
      t.append(el('div', { class: 'tt-row' }, el('strong', { text: r.value }), label));
    }
    t.hidden = false;
    const w = t.offsetWidth;
    const hgt = t.offsetHeight;
    let left = x + 14;
    if (left + w > window.innerWidth - 8) left = x - w - 14;
    let top = y - hgt - 12;
    if (top < 8) top = y + 16;
    t.style.left = `${Math.max(8, left)}px`;
    t.style.top = `${top}px`;
  }
  const hideTip = () => { tooltip().hidden = true; };

  // ---- Charts -------------------------------------------------------------
  const charts = new Set();
  function chartBox(label, draw) {
    const box = el('div', { class: 'chart', role: 'figure', 'aria-label': label });
    const entry = { box, draw };
    charts.add(entry);
    requestAnimationFrame(() => redraw(entry));
    return box;
  }
  function redraw(entry) {
    if (!entry.box.isConnected) { charts.delete(entry); return; }
    const w = entry.box.clientWidth;
    if (w) entry.box.replaceChildren(entry.draw(w));
  }
  let resizeTimer = 0;
  window.addEventListener('resize', () => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => charts.forEach(redraw), 120);
  });
  function niceTicks(min, max, count = 3) {
    if (max <= min) max = min + 1;
    const raw = (max - min) / count;
    const mag = 10 ** Math.floor(Math.log10(raw));
    const r = raw / mag;
    const step = (r <= 1 ? 1 : r <= 2 ? 2 : r <= 2.5 ? 2.5 : r <= 5 ? 5 : 10) * mag;
    const lo = Math.floor(min / step) * step;
    const hi = Math.ceil(max / step) * step;
    const out = [];
    for (let v = lo; v <= hi + step / 2; v += step) out.push(Number(v.toFixed(12)));
    return out;
  }
  // Ticks for a duration in seconds: whole minutes once the span reaches a
  // few of them.
  function durationTicks(min, max, count = 3) {
    if (max - min >= 180) return niceTicks(min / 60, max / 60, count).map((v) => v * 60);
    return niceTicks(min, max, count);
  }
  function columnPath(x, y, w, h) {
    if (h <= 0) return '';
    const r = Math.min(4, w / 2, h);
    return `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}Z`;
  }
  function axisY(svg, ticks, scale, left, right, format) {
    for (const t of ticks) {
      const y = scale(t);
      svg.append(sv('line', { class: t === 0 ? 'baseline' : 'gridline', x1: left, x2: right, y1: y, y2: y }),
        sv('text', { class: 'tick-label', x: left - 8, y: y + 4, 'text-anchor': 'end' }, format(t)));
    }
  }
  function axisX(svg, labels, y) {
    for (const [x, text, anchor] of labels) svg.append(sv('text', { class: 'tick-label', x, y, 'text-anchor': anchor || 'middle' }, text));
  }

  // Columns: one series, one mark per item; each column is its own hover
  // target, and a click follows its link.
  function columnChart(o) {
    return chartBox(o.label, (w) => {
      const H = o.height || 190;
      const pl = o.padLeft || 46, pr = 6, pt = 10, pb = o.noX ? 8 : 24;
      const iw = w - pl - pr, ih = H - pt - pb;
      const max = Math.max(o.max || 0, ...o.data.map((d) => d.value), 0);
      const ticks = o.ticks || niceTicks(0, max || 1, o.tickCount || 3);
      const top = ticks[ticks.length - 1];
      const y = (v) => pt + ih - (v / top) * ih;
      const svg = sv('svg', { width: w, height: H, viewBox: `0 0 ${w} ${H}` });
      axisY(svg, ticks, y, pl, w - pr, o.yFormat || compact);
      const n = o.data.length;
      const band = iw / Math.max(n, 1);
      const bw = Math.max(1, Math.min(24, band - 2));
      o.data.forEach((d, i) => {
        const x0 = pl + i * band;
        const hgt = top ? (Math.max(0, d.value) / top) * ih : 0;
        const bar = sv('path', { class: `bar${o.alt ? ' alt' : ''}`, d: columnPath(x0 + (band - bw) / 2, pt + ih - hgt, bw, hgt) });
        const hit = sv('rect', { class: d.href ? 'hit' : 'hit-area', x: x0, y: pt, width: Math.max(band, 1), height: ih });
        hit.addEventListener('pointermove', (e) => {
          bar.classList.add('hover');
          showTip(e.clientX, e.clientY, d.title, [{ value: (o.format || fmt)(d.value), label: o.unit || '' }].concat(d.extra || []));
        });
        hit.addEventListener('pointerleave', () => { bar.classList.remove('hover'); hideTip(); });
        if (d.href) hit.addEventListener('click', () => { hideTip(); go(d.href); });
        svg.append(bar, hit);
      });
      if (!o.noX && n) {
        const labels = [[pl + band / 2, o.data[0].x, 'start']];
        if (n > 2) labels.push([pl + Math.floor(n / 2) * band + band / 2, o.data[Math.floor(n / 2)].x]);
        if (n > 1) labels.push([pl + (n - 1) * band + band / 2, o.data[n - 1].x, 'end']);
        axisX(svg, labels, H - 6);
      }
      return svg;
    });
  }

  // Line with a wash: a crosshair snaps to the nearest point; an optional
  // reference line is labeled directly.
  function lineChart(o) {
    return chartBox(o.label, (w) => {
      const H = o.height || 190;
      const pl = o.padLeft || 46, pr = o.refLabel ? 70 : 10, pt = 12, pb = 24;
      const iw = w - pl - pr, ih = H - pt - pb;
      const vals = o.data.map((d) => d.value);
      const lo = Math.min(0, ...vals);
      const ticks = (o.timeAxis ? durationTicks : niceTicks)(lo, Math.max(...vals, o.ref || 0, lo + 1), 3);
      const t0 = ticks[0], t1 = ticks[ticks.length - 1];
      const y = (v) => pt + ih - ((v - t0) / (t1 - t0)) * ih;
      const n = o.data.length;
      const x = (i) => pl + (n === 1 ? iw / 2 : (i / (n - 1)) * iw);
      const svg = sv('svg', { width: w, height: H, viewBox: `0 0 ${w} ${H}` });
      axisY(svg, ticks, y, pl, w - pr, o.yFormat || compact);
      if (n) {
        const pts = o.data.map((d, i) => `${x(i).toFixed(1)},${y(d.value).toFixed(1)}`);
        svg.append(sv('path', { class: 'area', d: `M${x(0)},${y(Math.max(0, t0))}L${pts.join('L')}L${x(n - 1)},${y(Math.max(0, t0))}Z` }),
          sv('path', { class: 'line', d: `M${pts.join('L')}` }));
      }
      if (o.ref != null) {
        const ry = y(o.ref);
        svg.append(sv('line', { class: 'ref', x1: pl, x2: w - pr, y1: ry, y2: ry }),
          sv('text', { class: 'direct-label', x: w - pr + 6, y: ry + 4 }, o.refLabel));
      }
      if (n) {
        const last = sv('circle', { class: 'dot', cx: x(n - 1), cy: y(o.data[n - 1].value), r: 4 });
        svg.append(last);
        axisX(svg, [[x(0), o.data[0].x, 'start'], [x(n - 1), o.data[n - 1].x, 'end']], H - 6);
      }
      const cross = sv('line', { class: 'crosshair', y1: pt, y2: pt + ih, visibility: 'hidden' });
      const dot = sv('circle', { class: 'dot', r: 4, visibility: 'hidden' });
      const area = sv('rect', { class: o.hrefOf ? 'hit' : 'hit-area', x: pl, y: pt, width: iw, height: ih });
      let at = -1;
      area.addEventListener('pointermove', (e) => {
        const r = svg.getBoundingClientRect();
        const px = e.clientX - r.left;
        at = Math.max(0, Math.min(n - 1, Math.round(((px - pl) / iw) * (n - 1))));
        const d = o.data[at];
        cross.setAttribute('x1', x(at)); cross.setAttribute('x2', x(at)); cross.setAttribute('visibility', 'visible');
        dot.setAttribute('cx', x(at)); dot.setAttribute('cy', y(d.value)); dot.setAttribute('visibility', 'visible');
        showTip(e.clientX, e.clientY, d.title, [{ key: 'line', value: (o.format || fmt)(d.value), label: o.unit || '' }]);
      });
      area.addEventListener('pointerleave', () => {
        cross.setAttribute('visibility', 'hidden'); dot.setAttribute('visibility', 'hidden'); hideTip();
      });
      if (o.hrefOf) area.addEventListener('click', () => { if (at >= 0) { hideTip(); go(o.hrefOf(o.data[at])); } });
      svg.append(cross, dot, area);
      return svg;
    });
  }

  // ---- API ----------------------------------------------------------------
  async function api(path) {
    const r = await fetch(path, { headers: { Accept: 'application/json' } });
    let j = null;
    try { j = await r.json(); } catch { /* not JSON */ }
    if (!r.ok) {
      const e = new Error((j && j.error) || `${r.status} ${r.statusText}`);
      e.status = r.status;
      throw e;
    }
    return j;
  }
  // Blocks from..to, in 100-block pages fetched side by side; a page that
  // stops short of 16 MiB is continued.
  async function blockRange(from, to) {
    if (to < from) return [];
    const page = async (f, count) => {
      const items = [];
      while (items.length < count) {
        const p = await api(`/v1/blocks?from=${f}&limit=${Math.min(100, count - items.length)}`);
        if (!p.items.length) break;
        items.push(...p.items);
        f = p.next;
      }
      return items;
    };
    const pages = [];
    for (let f = from; f <= to; f += 100) pages.push(page(f, Math.min(100, to - f + 1)));
    return (await Promise.all(pages)).flat();
  }
  let lastStatus = null;
  async function status() {
    lastStatus = await api('/v1/status');
    $('#foot-node').textContent = `${lastStatus.name || 'Ting node'} · ${lastStatus.network} · goting ${lastStatus.version}`;
    return lastStatus;
  }

  // Derived figures over a run of consecutive blocks.
  function summarize(blocks) {
    const n = blocks.length;
    const span = n > 1 ? blocks[n - 1].timestamp - blocks[0].timestamp : 0;
    let work = 0, txs = 0, events = 0;
    blocks.forEach((b, i) => {
      if (i > 0) work += blockWork(b.target);
      txs += b.transactions.length;
      for (const r of b.receipts) events += r.events.length;
    });
    return {
      n, span, txs, events,
      avgTime: n > 1 ? span / (n - 1) : null,
      hashRate: span > 0 ? work / span : null,
    };
  }
  const eventsOf = (b) => b.receipts.reduce((a, r) => a + r.events.length, 0);

  // ---- Views --------------------------------------------------------------
  async function viewOverview() {
    const st = await status();
    const h = st.height;
    const blocks = await blockRange(Math.max(1, h - 99), h);
    const sum = summarize(blocks);
    const tip = blocks[blocks.length - 1];
    document.title = 'Ting explorer';

    const hero = el('div', { class: 'hero' },
      el('div', { class: 'card hero-figure' },
        el('div', null, el('div', { class: 'kicker', text: 'Block height' }),
          el('div', { class: 'hero-value', text: fmt(h) })),
        el('div', { class: 'hero-meta' },
          tip ? el('div', null, 'Tip ', hashLink('blocks', tip.id)) : null,
          tip ? el('div', { title: when(tip.timestamp) }, `Found ${ago(tip.timestamp)}`) : el('div', { text: 'At genesis' }),
          el('div', { class: 'muted', text: `${st.network} · chain work ${compact(Number(st.work))}` }))),
      el('div', { class: 'tiles' },
        tile('Average block time', sum.avgTime != null ? dur(sum.avgTime) : '—', `over the last ${plural(sum.n, 'block', 'blocks')}`),
        tile('Transactions', fmt(sum.txs), sum.n ? `${trimZeros((sum.txs / sum.n).toFixed(2))} per block, last ${fmt(sum.n)}` : null),
        tile('Estimated hash rate', sum.hashRate ? rate(sum.hashRate) : '—', 'work per second, last blocks'),
        tile('Mempool', fmt(st.mempool_count), `${bytes(st.mempool_bytes)} · ${plural(st.deferred_count, 'deferred', 'deferred')}`),
        tile('State records', compact(st.state_records), `${fmt(st.state_records)} records`),
        tile('Block capacity', `${compact(st.limits[0])} C`, `${bytes(st.limits[2])} data per block`)));

    const intervals = blocks.slice(1).map((b, i) => ({
      x: fmt(b.height), value: b.timestamp - blocks[i].timestamp, title: `Block ${fmt(b.height)}`, href: `/blocks/${b.height}`,
    }));
    const perBlock = blocks.map((b) => ({
      x: fmt(b.height), value: b.transactions.length, title: `Block ${fmt(b.height)}`, href: `/blocks/${b.height}`,
      extra: [{ value: fmt(eventsOf(b)), label: 'events' }],
    }));
    const charts = el('div', { class: 'grid two section' },
      card('Block time', sum.avgTime != null ? `average ${dur(sum.avgTime)}` : null,
        intervals.length ? lineChart({ label: 'Seconds between blocks', data: intervals, unit: 'seconds since the previous block',
          format: (v) => dur(v), yFormat: shortDur, timeAxis: true, ref: sum.avgTime, refLabel: 'average', hrefOf: (d) => d.href })
          : el('div', { class: 'empty', text: 'Not enough blocks yet' }),
        tableView([{ name: 'Height', num: true }, { name: 'Seconds', num: true }], intervals.map((d) => [d.x, fmt(d.value)]))),
      card('Transactions per block', `${fmt(sum.txs)} in ${plural(sum.n, 'block', 'blocks')}`,
        columnChart({ label: 'Transactions per block', data: perBlock, unit: 'transactions' }),
        tableView([{ name: 'Height', num: true }, { name: 'Transactions', num: true }], perBlock.map((d) => [d.x, fmt(d.value)]))));

    const fullness = el('div', { class: 'multiples' }, RESOURCES.map((res, k) => {
      const data = blocks.map((b) => ({
        x: fmt(b.height), value: b.limits ? b.used[k] / b.limits[k] : 0, title: `Block ${fmt(b.height)}`, href: `/blocks/${b.height}`,
        extra: [{ value: `${fmt(b.used[k])} of ${fmt(b.limits ? b.limits[k] : 0)}`, label: res.key }],
      }));
      const avg = data.reduce((a, d) => a + d.value, 0) / Math.max(1, data.length);
      const peak = Math.max(0, ...data.map((d) => d.value));
      return el('div', { class: 'multiple' },
        el('h3', { text: `${res.name} (${res.key})` }),
        el('div', { class: 'stat' }, share(avg), el('span', { class: 'unit', text: `average · peak ${share(peak)}` })),
        columnChart({ label: `${res.name} used per block, as a share of capacity`, data, height: 96, noX: true, padLeft: 40,
          ticks: niceTicks(0, Math.max(peak, 0.01), 2), yFormat: share, format: share, unit: 'of capacity' }));
    }));
    const full = card('How full blocks are', 'each resource as a share of its capacity, last blocks', fullness);

    const latest = blocks.slice(-12).reverse();
    const recent = card('Latest blocks', el('a', { href: '/blocks', text: 'All blocks →' }),
      table([{ name: 'Height' }, { name: 'Age' }, { name: 'Transactions', num: true }, { name: 'Events', num: true },
        { name: 'Size', num: true }, { name: 'Compute used', num: true }, { name: 'Block id' }],
      latest.map((b) => [blockLink(b.height), el('span', { title: when(b.timestamp), text: ago(b.timestamp) }),
        fmt(b.transactions.length), fmt(eventsOf(b)), bytes(b.bytes), b.limits ? share(b.used[0] / b.limits[0]) : '—',
        hashLink('blocks', b.id)])));

    return el('div', null, hero, charts, el('div', { class: 'section' }, full), el('div', { class: 'section' }, recent));
  }

  async function viewBlocks(_, query) {
    const st = await status();
    const per = 50;
    const top = Math.max(1, Math.min(st.height, Number(query.get('top')) || st.height));
    const from = Math.max(1, top - per + 1);
    const blocks = (await blockRange(from, top)).reverse();
    document.title = 'Blocks · Ting explorer';
    const newer = Math.min(st.height, top + per);
    const pager = el('div', { class: 'pager' },
      el('a', { class: 'btn', href: top < st.height ? `/blocks?top=${newer}` : '/blocks', 'aria-disabled': top >= st.height ? 'true' : null, text: '← Newer' }),
      el('span', { class: 'muted', text: `Heights ${fmt(from)}–${fmt(top)} of ${fmt(st.height)}` }),
      el('a', { class: 'btn', href: `/blocks?top=${Math.max(1, from - 1)}`, 'aria-disabled': from <= 1 ? 'true' : null, text: 'Older →' }));
    return el('div', null,
      el('div', { class: 'page-head' }, el('div', null, el('h1', { text: 'Blocks' }),
        el('div', { class: 'sub', text: 'The canonical chain, newest first' }))),
      card(null, null, table([{ name: 'Height' }, { name: 'Time' }, { name: 'Transactions', num: true }, { name: 'Events', num: true },
        { name: 'Size', num: true }, { name: 'Compute', num: true }, { name: 'Data', num: true }, { name: 'Block id' }],
      blocks.map((b) => [blockLink(b.height), el('span', { title: when(b.timestamp), text: ago(b.timestamp) }),
        fmt(b.transactions.length), fmt(eventsOf(b)), bytes(b.bytes),
        b.limits ? share(b.used[0] / b.limits[0]) : '—', b.limits ? share(b.used[2] / b.limits[2]) : '—',
        hashLink('blocks', b.id)])), pager));
  }

  function votesPills(votes) {
    return el('span', null, RESOURCES.map((r, i) => el('span', { class: 'pill', title: `${r.name}: ${VOTE[votes[i]]}`, text: `${r.key} ${['–', '↑', '↓'][votes[i]] || '?'}` }), ' '));
  }

  function eventCards(events, opts = {}) {
    if (!events.length) return el('div', { class: 'muted', text: 'No events.' });
    return el('div', { class: 'events' }, events.map((e) => el('div', { class: 'event' },
      el('span', { class: 'muted', text: opts.where ? opts.where(e) : `#${e.event_index ?? ''}` }),
      el('span', null, 'from ', hashLink('contracts', e.contract)),
      e.payload_hex ? el('div', { class: 'payload' }, e.payload_hex.length > 200 ? `${e.payload_hex.slice(0, 200)}…` : e.payload_hex,
        asU64(e.payload_hex) != null ? el('span', { class: 'muted', text: `  (u64 ${asU64(e.payload_hex)})` }) : null) : el('div', { class: 'payload', text: '(empty)' }))));
  }

  function downloadButton(label, fetchHex, name) {
    const b = el('button', { class: 'btn', type: 'button', text: label });
    b.addEventListener('click', async () => {
      b.textContent = 'Preparing…';
      try {
        const bytesOut = hexBytes(await fetchHex());
        const url = URL.createObjectURL(new Blob([bytesOut], { type: 'application/octet-stream' }));
        const a = el('a', { href: url, download: name });
        document.body.append(a);
        a.click();
        a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 2000);
        b.textContent = label;
      } catch (e) { b.textContent = `Failed: ${e.message}`; }
    });
    return b;
  }

  async function viewBlock(ref) {
    const [b, st] = await Promise.all([api(`/v1/blocks/${ref}`), status()]);
    document.title = `Block ${fmt(b.height)} · Ting explorer`;
    const work = blockWork(b.target);
    const producer = asText(b.producer_hex);
    const head = el('div', { class: 'page-head' },
      el('div', null, el('div', { class: 'kicker', text: 'Block' }), el('h1', { text: fmt(b.height) }),
        el('div', { class: 'sub' }, b.canonical ? el('span', { class: 'badge good' }, el('span', { class: 'icon', 'aria-hidden': 'true', text: '✓' }), 'Canonical')
          : el('span', { class: 'badge serious' }, el('span', { class: 'icon', 'aria-hidden': 'true', text: '↯' }), 'Not on the canonical chain'),
        ' ', b.canonical ? `${plural(st.height - b.height + 1, 'confirmation', 'confirmations')}` : '')),
      el('div', { class: 'nav-arrows' },
        el('a', { class: 'btn', href: `/blocks/${b.height - 1}`, 'aria-disabled': b.height <= 1 ? 'true' : null, text: '← Previous' }),
        el('a', { class: 'btn', href: `/blocks/${b.height + 1}`, 'aria-disabled': b.height >= st.height ? 'true' : null, text: 'Next →' })));

    const summary = card('Summary', null, facts([
      ['Block id', fullHash(b.id)],
      ['Parent', b.height > 1 ? hashLink('blocks', b.parent) : el('span', { class: 'muted', text: 'genesis' })],
      ['Time', el('span', null, when(b.timestamp), el('span', { class: 'muted', text: ` · ${ago(b.timestamp)}` }))],
      ['Transactions', fmt(b.transactions.length)],
      ['Events', fmt(eventsOf(b))],
      ['Size', `${bytes(b.bytes)} (${fmt(b.bytes)} bytes)`],
      ['Producer', b.producer_hex ? (producer ? `“${producer}”` : el('span', { class: 'mono', text: short(b.producer_hex, 16, 8) })) : el('span', { class: 'muted', text: 'none' })],
      ['Capacity votes', votesPills(b.votes)],
      ['Block work', compact(work)],
      ['Chain work', compact(Number(b.work))],
      ['Target', el('span', { class: 'mono', text: b.target })],
      ['Nonce', el('span', { class: 'mono', text: b.nonce })],
      ['State root', fullHash(b.state_root)],
      ['Transactions root', fullHash(b.transactions_root)],
      ['Receipts root', fullHash(b.receipts_root)],
    ]), el('div', { class: 'inline-form' }, downloadButton('Download raw block', async () => {
      let hex = '', offset = 0, total = 1;
      while (offset < total) {
        const s = await api(`/v1/blocks/${b.id}/raw?offset=${offset}&length=1048576`);
        hex += s.hex; total = s.total; offset += s.hex.length / 2;
        if (!s.hex.length) break;
      }
      return hex;
    }, `block-${b.height}.bin`)));

    const parent = b.height > 1 ? await api(`/v1/blocks/${b.parent}`).catch(() => null) : null;
    const votes = await api('/v1/votes').catch(() => null);
    const timing = card('Timing', null, facts([
      ['Since the previous block', !parent ? '—' : b.timestamp >= parent.timestamp ? dur(b.timestamp - parent.timestamp)
        : `${dur(parent.timestamp - b.timestamp)} earlier than its parent, which the rules allow: a timestamp need only pass the median of the previous 11`],
      ['Capacity epoch', votes ? `epoch ${fmt(Math.floor((b.height - 1) / votes.epoch_blocks))}, block ${fmt(((b.height - 1) % votes.epoch_blocks) + 1)} of ${fmt(votes.epoch_blocks)}` : null],
      ['Work share', `${share(work / Number(b.work))} of the chain’s work`],
    ]));
    const usage = card('Resources used', b.limits ? 'of this block\'s capacity' : 'capacity unknown',
      b.limits ? el('div', { class: 'meters' }, RESOURCES.map((r, k) => meter(`${r.name} (${r.key})`, b.used[k], b.limits[k])))
        : el('div', { class: 'muted', text: 'This block has not been executed here.' }));

    const txRows = b.transactions.map((id, i) => {
      const r = b.receipts[i];
      return [fmt(i), hashLink('transactions', id), statusBadge(r.status_name), compact(r.used[0]), bytes(r.used[2]),
        fmt(r.events.length), r.result_hex ? el('span', { class: 'mono-cell', title: r.result_hex, text: short(r.result_hex, 10, 6) }) : el('span', { class: 'muted', text: '—' })];
    });
    const txs = card('Transactions', plural(b.transactions.length, 'transaction', 'transactions'),
      b.transactions.length ? table([{ name: '#', num: true }, { name: 'Transaction' }, { name: 'Outcome' }, { name: 'Compute', num: true },
        { name: 'Data', num: true }, { name: 'Events', num: true }, { name: 'Result' }], txRows) : el('div', { class: 'muted', text: 'An empty block.' }));

    const events = [];
    b.receipts.forEach((r, t) => r.events.forEach((e, j) => events.push({ ...e, tx: t, event_index: j })));
    const evCard = card('Events', plural(events.length, 'event', 'events'),
      eventCards(events.slice(0, 200), { where: (e) => `tx ${e.tx} · #${e.event_index}` }),
      events.length > 200 ? el('div', { class: 'muted', text: `and ${fmt(events.length - 200)} more` }) : null);

    return el('div', null, head, el('div', { class: 'grid two' }, summary, el('div', { class: 'stack' }, usage, timing)),
      el('div', { class: 'section' }, txs), el('div', { class: 'section' }, evCard));
  }

  async function viewTransaction(id) {
    const [t, st] = await Promise.all([api(`/v1/transactions/${id}`), status()]);
    document.title = `Transaction ${short(id)} · Ting explorer`;
    const occ = t.occurrences[0];
    const summaryOf = occ ? occ.transaction : t.pending_transaction && t.pending_transaction.transaction;
    let state;
    if (occ) state = statusBadge(occ.receipt.status_name);
    else if (t.deferred) state = el('span', { class: 'badge warning' }, el('span', { class: 'icon', 'aria-hidden': 'true', text: '⏱' }), 'Deferred');
    else state = el('span', { class: 'badge neutral' }, el('span', { class: 'icon', 'aria-hidden': 'true', text: '…' }), 'Pending');
    const head = el('div', { class: 'page-head' }, el('div', null, el('div', { class: 'kicker', text: 'Transaction' }),
      el('h1', { class: 'mono', text: short(id, 12, 8) }), el('div', { class: 'sub' }, state,
        occ ? ` in block ${fmt(occ.height)}, ${plural(st.height - occ.height + 1, 'confirmation', 'confirmations')}` : '')));

    const rows = [['Transaction id', fullHash(id)]];
    if (occ) {
      rows.push(['Block', el('span', null, blockLink(occ.height), ' · ', hashLink('blocks', occ.block))], ['Position', `transaction ${fmt(occ.index)} of the block`]);
    }
    if (summaryOf) {
      rows.push(['Kind', summaryOf.kind === 'deploy' ? 'Deploy a program' : 'Call a program']);
      if (summaryOf.target) rows.push(['Target', hashLink('contracts', summaryOf.target)]);
      if (summaryOf.kind === 'deploy' && occ && occ.receipt.status_name === 'Success' && occ.receipt.result_hex.length === 64) {
        rows.push(['Deployed program', hashLink('contracts', occ.receipt.result_hex)]);
      }
      if (summaryOf.code_bytes != null) rows.push(['Code', bytes(summaryOf.code_bytes)]);
      rows.push(['Input', bytes(summaryOf.input_bytes)], ['Envelope', bytes(summaryOf.encoded_bytes)]);
    }
    if (t.deferred) {
      const d = t.deferred;
      rows.push(['Not before', `${when(d.not_before)} (${ago(d.not_before)})`], ['Expires', when(d.expires)],
        ['Admitted to the pool', d.admitted ? 'yes' : 'not yet'],
        ['Last trial', d.last_attempt ? `${ago(d.last_attempt)}${d.last_result ? ` · ${d.last_result}` : ''}` : 'none yet']);
    }
    const parts = [head, el('div', { class: 'grid two' }, card('Summary', null, facts(rows)),
      occ && summaryOf ? card('Resources', 'used of the budget it declared', el('div', { class: 'meters' },
        RESOURCES.map((r, k) => meter(`${r.name} (${r.key})`, occ.receipt.used[k], summaryOf.budget[k]))))
        : summaryOf ? card('Budget', null, facts(RESOURCES.map((r, k) => [`${r.name} (${r.key})`, fmt(summaryOf.budget[k])]))) : null)];
    if (occ) {
      const res = occ.receipt.result_hex;
      const hints = [];
      if (asU64(res) != null) hints.push(`u64 ${asU64(res)}`);
      if (asText(res)) hints.push(`text “${asText(res)}”`);
      parts.push(el('div', { class: 'section' }, card('Result', res ? bytes(res.length / 2) : 'empty',
        res ? el('div', { class: 'full-hash' }, res) : el('div', { class: 'muted', text: 'The transaction returned nothing.' }),
        hints.length ? el('div', { class: 'muted', text: hints.join(' · ') }) : null)));
      parts.push(el('div', { class: 'section' }, card('Events', plural(occ.receipt.events.length, 'event', 'events'),
        eventCards(occ.receipt.events.map((e, j) => ({ ...e, event_index: j }))))));
      if (t.occurrences.length > 1 || t.has_more) {
        parts.push(el('div', { class: 'section' }, card('Also included in', null, table([{ name: 'Height' }, { name: 'Block' }, { name: 'Outcome' }],
          t.occurrences.slice(1).map((o) => [blockLink(o.height), hashLink('blocks', o.block), statusBadge(o.receipt.status_name)])))));
      }
    }
    return el('div', null, parts);
  }

  // Storage proofs, checked here (wiki: data/state-tree#proofs):
  // H(tag, x) = SHA-256("Ting/" ‖ u16le(len tag) ‖ tag ‖ x).
  const enc = new TextEncoder();
  async function th(tag, ...parts) {
    const t = enc.encode(tag);
    const buf = new Uint8Array(7 + t.length + parts.reduce((a, p) => a + p.length, 0));
    buf.set(enc.encode('Ting/'), 0);
    buf[5] = t.length & 255; buf[6] = t.length >> 8;
    buf.set(t, 7);
    let o = 7 + t.length;
    for (const p of parts) { buf.set(p, o); o += p.length; }
    return new Uint8Array(await crypto.subtle.digest('SHA-256', buf));
  }
  const u16le = (v) => new Uint8Array([v & 255, v >> 8]);
  const u32le = (v) => new Uint8Array([v & 255, (v >> 8) & 255, (v >> 16) & 255, (v >>> 24) & 255]);
  const bitOf = (k, i) => (k[i >> 3] >> (7 - (i & 7))) & 1;
  async function verifyProof(contract, key, ans) {
    const p = ans.proof;
    const rk = await th('state-key', new Uint8Array([2]), hexBytes(contract), hexBytes(key));
    if (toHex(rk) !== p.record_key) return [false, 'the record key does not match'];
    const sibs = p.siblings.map(hexBytes);
    const depth = sibs.length;
    let h;
    if (p.end === 'present') {
      if (ans.value_hex == null) return [false, 'a present record without a value'];
      const v = hexBytes(ans.value_hex);
      h = await th('state-leaf', rk, u32le(v.length), v);
    } else if (p.end === 'empty') {
      if (ans.value_hex != null) return [false, 'an absent record with a value'];
      h = await th('state-empty');
    } else {
      const ok = hexBytes(p.other_key);
      const ov = ans.others && ans.others[p.other_key];
      if (ov == null || p.other_key === p.record_key) return [false, 'the other record is missing'];
      for (let d = 0; d < depth; d++) if (bitOf(ok, d) !== bitOf(rk, d)) return [false, 'the other record is off the path'];
      const v = hexBytes(ov);
      h = await th('state-leaf', ok, u32le(v.length), v);
    }
    for (let i = 0; i < depth; i++) {
      const lvl = depth - 1 - i;
      h = bitOf(rk, lvl) === 0 ? await th('state-node', u16le(lvl), h, sibs[i]) : await th('state-node', u16le(lvl), sibs[i], h);
    }
    return toHex(h) === ans.state_root ? [true, `${plural(depth, 'level', 'levels')} hashed up to the state root`] : [false, 'the root does not match'];
  }

  async function viewContract(id) {
    const [m, st] = await Promise.all([api(`/v1/contracts/${id}`), status()]);
    document.title = `Program ${short(id)} · Ting explorer`;
    // Its events, by block, up to five pages from the start.
    const groups = [];
    let from = 1;
    for (let i = 0; i < 5; i++) {
      const p = await api(`/v1/events?contract=${id}&from=${from}&limit=100`);
      groups.push(...p.items);
      if (p.next > p.height || p.next <= from) break;
      from = p.next;
    }
    const total = groups.reduce((a, g) => a + g.events.length, 0);
    const head = el('div', { class: 'page-head' }, el('div', null, el('div', { class: 'kicker', text: 'Program' }),
      el('h1', { class: 'mono', text: short(id, 12, 8) }),
      el('div', { class: 'sub', text: total ? `${plural(total, 'event', 'events')} in ${plural(groups.length, 'block', 'blocks')}` : 'No events yet' })));

    const keyInput = el('input', { type: 'text', value: '0'.repeat(64), 'aria-label': 'Storage key, 64 hex characters', spellcheck: 'false' });
    const out = el('div');
    const read = async (e) => {
      if (e) e.preventDefault();
      const key = keyInput.value.trim().toLowerCase();
      out.replaceChildren();
      if (!/^[0-9a-f]{64}$/.test(key)) { out.append(el('div', { class: 'result-box unverified', text: 'A key is 64 lowercase hex characters (32 bytes).' })); return; }
      try {
        const ans = await api(`/v1/contracts/${id}/storage/${key}?proof=true`);
        const v = ans.value_hex;
        const box = el('div', { class: 'result-box' });
        box.append(facts([
          ['Value', v == null ? el('span', { class: 'muted', text: 'absent' }) : el('span', { class: 'full-hash', text: v || '(empty)' })],
          ['As u64', asU64(v) != null ? String(asU64(v)) : null],
          ['As text', asText(v) ? `“${asText(v)}”` : null],
          ['Proof', `${p(ans.proof.end)}, ${plural(ans.proof.siblings.length, 'sibling', 'siblings')}`],
          ['State root', fullHash(ans.state_root)],
        ]));
        const verdict = el('div', { class: 'muted', text: 'Checking the proof…' });
        box.append(verdict);
        out.append(box);
        if (!window.crypto || !crypto.subtle) { verdict.textContent = 'This page needs HTTPS to check the proof.'; return; }
        const [ok, why] = await verifyProof(id, key, ans);
        verdict.className = ok ? 'verified' : 'unverified';
        verdict.textContent = ok ? `✓ Proof checked in your browser: ${why}` : `✗ Proof does not check: ${why}`;
      } catch (err) { out.append(el('div', { class: 'result-box unverified', text: err.message })); }
    };
    const p = (end) => ({ present: 'the record’s own leaf', empty: 'an empty subtree', other: 'another record’s leaf' })[end] || end;
    const form = el('form', { class: 'inline-form', onsubmit: read }, keyInput, el('button', { class: 'btn', type: 'submit', text: 'Read with proof' }));

    const activity = groups.slice(-100).map((g) => ({
      x: fmt(g.height), value: g.events.length, title: `Block ${fmt(g.height)}`, href: `/blocks/${g.height}`,
    }));
    const recent = [];
    for (const g of groups.slice().reverse()) {
      for (const e of g.events.slice().reverse()) {
        if (recent.length >= 30) break;
        recent.push({ ...e, height: g.height });
      }
    }
    return el('div', null, head,
      el('div', { class: 'grid two' },
        card('Program', null, facts([
          ['Program id', fullHash(id)],
          ['Code hash', fullHash(m.code_hash)],
          ['Code size', bytes(m.code_bytes)],
          ['Code price', `${fmt(m.code_price)} C to prepare`],
          ['State root', el('span', null, fullHash(m.state_root), el('div', { class: 'muted', text: `at height ${fmt(st.height)}` }))],
        ])),
        card('Storage', 'read a record and check its Merkle proof',
          el('div', { class: 'muted', text: 'Enter a 32-byte storage key. The node returns the value with a proof that your browser checks against the state root.' }),
          form, out)),
      el('div', { class: 'section' }, card('Activity', 'events per block',
        activity.length ? columnChart({ label: 'Events per block', data: activity, unit: 'events', height: 150 }) : el('div', { class: 'muted', text: 'No events yet.' }))),
      el('div', { class: 'section' }, card('Latest events', null,
        eventCards(recent, { where: (e) => `block ${fmt(e.height)} · tx ${e.transaction_index}` }))));
  }

  async function viewMempool() {
    const [mp, st] = await Promise.all([api('/v1/mempool'), status()]);
    document.title = 'Mempool · Ting explorer';
    const budgetCell = (b) => el('span', { class: 'muted', title: RESOURCES.map((r, k) => `${r.key} ${fmt(b[k])}`).join(' · '), text: `${compact(b[0])} C · ${bytes(b[2])}` });
    return el('div', null,
      el('div', { class: 'page-head' }, el('div', null, el('h1', { text: 'Mempool' }),
        el('div', { class: 'sub', text: 'Transactions waiting for a block, and deferred ones waiting for their time' }))),
      el('div', { class: 'grid three' },
        tile('Pending', fmt(mp.items.length), bytes(mp.bytes)),
        tile('Deferred', fmt(mp.deferred.length), `${mp.deferred.filter((d) => d.deferred && d.deferred.admitted).length} admitted`),
        tile('Next block fits', bytes(st.limits[2]), `${compact(st.limits[0])} C of compute`)),
      el('div', { class: 'section' }, card('Pending', plural(mp.items.length, 'transaction', 'transactions'),
        mp.items.length ? table([{ name: 'Transaction' }, { name: 'Size', num: true }, { name: 'Budget' }, { name: 'Deferred' }],
          mp.items.map((t) => [hashLink('transactions', t.id), bytes(t.bytes), budgetCell(t.budget), t.deferred ? 'admitted' : '—']))
          : el('div', { class: 'empty', text: 'The pool is empty: every transaction it saw is in a block.' }))),
      el('div', { class: 'section' }, card('Deferred', plural(mp.deferred.length, 'transaction', 'transactions'),
        mp.deferred.length ? table([{ name: 'Transaction' }, { name: 'Not before' }, { name: 'Expires' }, { name: 'State' }, { name: 'Last trial' }],
          mp.deferred.map((t) => [hashLink('transactions', t.id), el('span', { title: when(t.deferred.not_before), text: ago(t.deferred.not_before) }),
            el('span', { title: when(t.deferred.expires), text: ago(t.deferred.expires) }), t.deferred.admitted ? 'admitted' : 'waiting',
            t.deferred.last_attempt ? `${ago(t.deferred.last_attempt)}${t.deferred.last_result ? ` · ${t.deferred.last_result}` : ''}` : '—']))
          : el('div', { class: 'empty', text: 'No deferred transactions.' }))));
  }

  async function viewNetwork() {
    const [st, votes] = await Promise.all([status(), api('/v1/votes')]);
    document.title = 'Network · Ting explorer';
    const recent = await blockRange(Math.max(1, st.height - 59), st.height);
    const avg = summarize(recent).avgTime;
    const te = votes.this_epoch;
    const toNext = votes.next_epoch_from - (st.height + 1);
    const epoch = card('Capacity epoch', `epoch ${fmt(votes.epoch)}`,
      el('div', { class: 'meters' }, meter('Progress', te.blocks, votes.epoch_blocks, { text: `${fmt(te.blocks)} of ${fmt(votes.epoch_blocks)} blocks` })),
      facts([
        ['Next epoch', `from height ${fmt(votes.next_epoch_from)}, in ${plural(toNext + 1, 'block', 'blocks')}${avg ? ` (about ${dur((toNext + 1) * avg)})` : ''}`],
        ['This epoch’s votes apply', `from height ${fmt(te.takes_effect_from)}`],
      ]));
    const thresholds = [{ at: 2 / 3, title: 'an increase needs two thirds of the work' }];
    const voteRows = el('div', { class: 'stack' }, RESOURCES.map((r, k) => el('div', null,
      el('div', { class: 'card-head' }, el('h3', { text: `${r.name} (${r.key})` }), el('span', null, 'so far: ', outcomeBadge(te.outcome_so_far[k]))),
      el('div', { class: 'meters' },
        meter('Increase', te.increase_percent[k], 100, { text: `${te.increase_percent[k]}% of the work`, ticks: thresholds }),
        meter('Decrease', te.decrease_percent[k], 100, { alt: true, text: `${te.decrease_percent[k]}% of the work`, ticks: [{ at: 0.5, title: 'a decrease needs more than half of the work' }] })))));
    const voting = card('Votes this epoch', 'share of the epoch’s work voting each way',
      el('div', { class: 'legend' }, el('span', null, el('span', { class: 'key' }), 'Increase · needs ⅔'), el('span', null, el('span', { class: 'key alt' }), 'Decrease · needs more than ½')),
      voteRows);
    const caps = votes.capacities;
    const capTable = card('Capacities', 'decided three epochs ahead', table(
      [{ name: 'Epoch' }, { name: 'From', num: true }].concat(RESOURCES.map((r) => ({ name: r.key, num: true }))),
      caps.map((c, i) => [fmt(c.epoch), fmt(c.from)].concat(c.limits.map((v, k) => {
        const prev = i ? caps[i - 1].limits[k] : v;
        const change = v > prev ? ' ↑' : v < prev ? ' ↓' : '';
        return `${k === 2 || k === 3 ? bytes(v) : compact(v)}${change}`;
      }))), el('div', { class: 'muted small', text: 'C compute · S state access · D data · G growth, per block' })));

    const reorg = st.reorganization;
    const node = card('This node', `goting ${st.version}`, facts([
      ['Name', st.name || '—'],
      ['Network', st.network],
      ['Chain id', fullHash(st.chain_id)],
      ['Genesis block', fullHash(st.genesis)],
      ['Up for', dur(st.uptime_seconds)],
      ['Stored blocks', fmt(st.stored_blocks)],
      ['Headers held', fmt(st.held_headers)],
      ['State records', fmt(st.state_records)],
      ['State stored up to', `height ${fmt(st.settled_height)}`],
      ['Undo data from', st.undo_pruned ? `height ${fmt(st.undo_pruned)}` : 'the start'],
      ['Last reorganization', reorg ? `${plural(reorg.replaced, 'block', 'blocks')} replaced above height ${fmt(reorg.fork_height)}, ${ago(reorg.time)}${reorg.deep ? ' (deep)' : ''}` : 'none since start'],
      ['Marked invalid', st.marked.length ? st.marked.map((m) => hashLink('blocks', m)) : 'none'],
    ]), st.warnings.length ? st.warnings.map((w) => el('div', { class: 'callout warn', text: w })) : null);
    return el('div', null,
      el('div', { class: 'page-head' }, el('div', null, el('h1', { text: 'Network' }),
        el('div', { class: 'sub', text: 'Capacity voting, the capacities it decides, and this node' }))),
      el('div', { class: 'grid two' }, el('div', { class: 'stack' }, epoch, capTable, node), voting));
  }
  function outcomeBadge(o) {
    if (o === 'increase') return el('span', { class: 'badge good' }, el('span', { class: 'icon', 'aria-hidden': 'true', text: '↑' }), 'increase');
    if (o === 'decrease') return el('span', { class: 'badge serious' }, el('span', { class: 'icon', 'aria-hidden': 'true', text: '↓' }), 'decrease');
    return el('span', { class: 'badge neutral' }, el('span', { class: 'icon', 'aria-hidden': 'true', text: '–' }), 'hold');
  }

  async function viewInsights(_, query) {
    const st = await status();
    const windows = [100, 250, 500];
    const win = windows.includes(Number(query.get('window'))) ? Number(query.get('window')) : 100;
    const blocks = await blockRange(Math.max(1, st.height - win + 1), st.height);
    const sum = summarize(blocks);
    document.title = 'Insights · Ting explorer';

    const filter = el('div', { class: 'segmented', role: 'group', 'aria-label': 'Blocks to look at' },
      windows.map((w) => el('button', { type: 'button', 'aria-pressed': String(w === win), text: `Last ${w} blocks`,
        onclick: () => go(`/insights?window=${w}`) })));

    const works = blocks.slice(1).map((b) => ({ x: fmt(b.height), value: blockWork(b.target), title: `Block ${fmt(b.height)}`, href: `/blocks/${b.height}` }));
    const difficulty = card('Work per block', 'the difficulty: expected hashes to find a block',
      el('div', { class: 'grid two' },
        tile('Estimated hash rate', sum.hashRate ? rate(sum.hashRate) : '—', 'the work of these blocks per second'),
        tile('Current work per block', works.length ? compact(works[works.length - 1].value) : '—', 'hashes expected per block')),
      el('div', { class: 'section' }, works.length ? lineChart({ label: 'Work per block', data: works, unit: 'hashes expected', format: compact, hrefOf: (d) => d.href })
        : el('div', { class: 'empty', text: 'Not enough blocks yet' })));

    const statusCounts = {};
    for (const b of blocks) for (const r of b.receipts) statusCounts[r.status_name] = (statusCounts[r.status_name] || 0) + 1;
    const statusRows = Object.entries(statusCounts).sort((a, b) => b[1] - a[1])
      .map(([name, value]) => ({ label: statusBadge(name), value }));
    const outcomes = card('Transaction outcomes', plural(sum.txs, 'transaction', 'transactions'),
      statusRows.length ? hbars(statusRows, { format: (r) => `${fmt(r.value)} · ${pct(r.value / sum.txs)}` }) : el('div', { class: 'empty', text: 'No transactions in these blocks.' }));

    const programs = new Map();
    for (const b of blocks) {
      for (const r of b.receipts) {
        for (const e of r.events) {
          const p = programs.get(e.contract) || { events: 0, blocks: new Set() };
          p.events++; p.blocks.add(b.height);
          programs.set(e.contract, p);
        }
      }
    }
    const top = [...programs.entries()].sort((a, b) => b[1].events - a[1].events).slice(0, 10);
    const active = card('Most active programs', 'by events emitted',
      top.length ? hbars(top.map(([id, p]) => ({ label: hashLink('contracts', id), value: p.events, blocks: p.blocks.size })),
        { format: (r) => `${fmt(r.value)} in ${plural(r.blocks, 'block', 'blocks')}` }) : el('div', { class: 'empty', text: 'No program emitted events in these blocks.' }));

    const demand = card('Average demand', 'resources used per block, against capacity',
      el('div', { class: 'meters' }, RESOURCES.map((r, k) => {
        const used = blocks.reduce((a, b) => a + b.used[k], 0) / Math.max(1, blocks.length);
        const cap = blocks.reduce((a, b) => a + (b.limits ? b.limits[k] : 0), 0) / Math.max(1, blocks.length);
        return meter(`${r.name} (${r.key})`, Math.round(used), Math.round(cap));
      })));

    const intervals = blocks.slice(1).map((b, i) => b.timestamp - blocks[i].timestamp);
    const hist = histogram(intervals);
    const timing = card('Block time distribution', sum.avgTime != null ? `average ${dur(sum.avgTime)}` : null,
      hist.length ? columnChart({ label: 'Blocks by time since the previous block', data: hist, unit: 'blocks', height: 170 }) : el('div', { class: 'empty', text: 'Not enough blocks yet' }),
      hist.length ? tableView([{ name: 'Time since the previous block' }, { name: 'Blocks', num: true }], hist.map((d) => [d.title, fmt(d.value)])) : null);

    return el('div', null,
      el('div', { class: 'page-head' }, el('div', null, el('h1', { text: 'Insights' }),
        el('div', { class: 'sub', text: `Heights ${fmt(blocks.length ? blocks[0].height : 0)}–${fmt(st.height)}: ${plural(sum.txs, 'transaction', 'transactions')}, ${plural(sum.events, 'event', 'events')}` })), filter),
      difficulty,
      el('div', { class: 'grid two section' }, outcomes, timing),
      el('div', { class: 'grid two section' }, active, demand));
  }
  function histogram(values) {
    if (!values.length) return [];
    const sorted = values.slice().sort((a, b) => a - b);
    const hi = Math.max(1, sorted[Math.floor(sorted.length * 0.95)]);
    const lo = Math.min(0, sorted[0]);
    const step = durationTicks(0, hi - lo, 10)[1] || 1;
    const start = Math.floor(lo / step) * step;
    const bins = [];
    for (let v = start; v <= hi; v += step) bins.push({ from: v, to: v + step, value: 0 });
    const last = { from: bins.length ? bins[bins.length - 1].to : 0, to: Infinity, value: 0 };
    for (const v of values) {
      const b = bins.find((x) => v >= x.from && v < x.to);
      if (b) b.value++; else last.value++;
    }
    if (last.value) bins.push(last);
    return bins.map((b) => ({
      x: b.to === Infinity ? `${shortDur(b.from)}+` : shortDur(b.from),
      value: b.value,
      title: b.to === Infinity ? `${dur(b.from)} or more` : `${dur(b.from)} to ${dur(b.to)}`,
    }));
  }

  function errorView(err, what) {
    const nf404 = err && err.status === 404;
    return el('div', { class: 'error-box' },
      el('h1', { text: nf404 ? 'Nothing found' : 'Something went wrong' }),
      el('p', { class: 'muted', text: nf404 ? (what || 'The node does not know this item.') : err.message }),
      el('a', { class: 'btn', href: '/', text: 'Back to the overview' }));
  }

  // ---- Search -------------------------------------------------------------
  async function search(q) {
    q = q.trim().toLowerCase().replace(/^0x/, '');
    if (!q) return;
    if (/^\d+$/.test(q)) return go(`/blocks/${Number(q)}`);
    if (!/^[0-9a-f]{64}$/.test(q)) {
      $('#main').replaceChildren(errorView({ status: 404 }, 'Search by a height, or a 64-character block, transaction or program id.'));
      return;
    }
    for (const [probe, page] of [['/v1/blocks/', '/blocks/'], ['/v1/transactions/', '/transactions/'], ['/v1/contracts/', '/contracts/']]) {
      try {
        await api(probe + q);
        return go(page + q);
      } catch (e) {
        if (e.status !== 404) break;
      }
    }
    $('#main').replaceChildren(errorView({ status: 404 }, `No block, transaction or program has the id ${q}.`));
  }

  // ---- Routing ------------------------------------------------------------
  const ROUTES = [
    [/^\/$/, 'overview', viewOverview],
    [/^\/blocks\/?$/, 'blocks', viewBlocks],
    [/^\/blocks\/([0-9]+|[0-9a-f]{64})$/, 'blocks', (m) => viewBlock(m[1])],
    [/^\/transactions\/([0-9a-f]{64})$/, null, (m) => viewTransaction(m[1])],
    [/^\/contracts\/([0-9a-f]{64})$/, null, (m) => viewContract(m[1])],
    [/^\/mempool\/?$/, 'mempool', viewMempool],
    [/^\/network\/?$/, 'network', viewNetwork],
    [/^\/insights\/?$/, 'insights', viewInsights],
  ];
  let seq = 0;
  let routeName = null;
  async function render(opts = {}) {
    const mine = ++seq;
    const path = location.pathname;
    const query = new URLSearchParams(location.search);
    const main = $('#main');
    hideTip();
    let match = null;
    for (const [re, name, view] of ROUTES) {
      const m = path.match(re);
      if (m) { match = [m, name, view]; break; }
    }
    routeName = match ? match[1] : null;
    for (const a of document.querySelectorAll('.nav a')) {
      if (a.dataset.nav === routeName) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    }
    if (opts.keep) main.classList.add('refreshing');
    else main.replaceChildren(el('div', { class: 'loading', text: 'Loading' }));
    let view;
    try {
      view = match ? await match[2](match[0], query) : errorView({ status: 404 }, 'There is no such page.');
    } catch (e) {
      view = errorView(e);
    }
    if (mine !== seq) return;
    main.classList.remove('refreshing');
    main.replaceChildren(view);
    if (!opts.keep) window.scrollTo(0, 0);
  }
  function go(path) {
    if (path === location.pathname + location.search) return render();
    history.pushState(null, '', path);
    render();
  }
  document.addEventListener('click', (e) => {
    const a = e.target.closest('a');
    if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || a.target || a.hasAttribute('download')) return;
    const url = new URL(a.href, location.href);
    if (url.origin !== location.origin || url.pathname.startsWith('/v1/') || url.pathname === '/health') return;
    e.preventDefault();
    if (a.getAttribute('aria-disabled') === 'true') return;
    go(url.pathname + url.search);
  });
  window.addEventListener('popstate', () => render());
  $('#search').addEventListener('submit', (e) => {
    e.preventDefault();
    const q = $('#q');
    search(q.value);
    q.blur();
  });

  // ---- Theme --------------------------------------------------------------
  function setTheme(t) {
    if (t) document.documentElement.dataset.theme = t; else delete document.documentElement.dataset.theme;
  }
  try { setTheme(localStorage.getItem('theme')); } catch { /* storage unavailable */ }
  $('#theme').addEventListener('click', () => {
    const current = document.documentElement.dataset.theme || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    const next = current === 'dark' ? 'light' : 'dark';
    setTheme(next);
    try { localStorage.setItem('theme', next); } catch { /* storage unavailable */ }
  });

  // ---- Live ---------------------------------------------------------------
  // Views that follow the tip re-render when it moves, holding their frame.
  const LIVE = new Set(['overview', 'mempool', 'network', 'insights']);
  let tipSeen = null;
  async function poll() {
    const live = $('#live');
    try {
      const hh = await api('/health');
      $('#live-height').textContent = `height ${fmt(hh.height)}`;
      live.classList.add('on');
      if (tipSeen && hh.tip !== tipSeen) {
        live.classList.remove('pulse');
        void live.offsetWidth;
        live.classList.add('pulse');
        const onFirstPage = routeName === 'blocks' && location.pathname.replace(/\/$/, '') === '/blocks' && !new URLSearchParams(location.search).get('top');
        const typing = document.activeElement && document.activeElement.tagName === 'INPUT';
        if ((LIVE.has(routeName) || onFirstPage) && !typing) render({ keep: true });
      }
      tipSeen = hh.tip;
    } catch {
      live.classList.remove('on');
      $('#live-height').textContent = 'offline';
    }
  }
  setInterval(() => { if (!document.hidden) poll(); }, 5000);
  poll();
  render();
})();
