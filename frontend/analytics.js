// Scout analytics page: four tabs (Overview, By factor, Peak vs final, Timing)
// over one compact feed. Plain JavaScript; the pure calculations are in
// analytics-stats.js (window.AnaStats), loaded before this file. The Timing
// tab draws with the vendored ECharts build (frontend/vendor, window.echarts,
// loaded first); the other tabs draw their own SVG.
//
// GET /api/analytics sends one compact row per first call (built once per
// snapshot on the server); this page filters, groups and draws them itself,
// so every choice is instant and only the visible tab is drawn. Names in the
// data (DEX, quote asset) come from posts and contracts: nothing is ever
// parsed as HTML or SVG markup; every node is built with createElement or
// createElementNS, and text goes in with textContent. Colours are CSS
// classes (style.css), never style attributes (the page's CSP).
(function () {
  'use strict';

  var A = window.AnaStats;
  var DASH = '–';
  var FORMAT = 4;          // the body's "format" this page understands
  var MIN_N = 20;          // fewer calls with data than this: the group is greyed out
  var REFRESH_MS = 60000;  // ask again (cheap: 304 while nothing changed)
  var REDRAW_MS = 600000;  // on a 304, draw again only this often ("not due yet" moves slowly)
  var TOP_DEX = 12;        // posted DEX names shown on their own; the rest are "Other"
  var TOP_QUOTE = 8;
  var PL_CAP = 1000;       // "$100 on every call": each return capped at +1,000%
  var FLAG_USD = 1, FLAG_RUGGED = 2, FLAG_TRACKED = 4; // the "flags" column
  var SVGNS = 'http://www.w3.org/2000/svg';

  var VERDICT_LABEL = {
    clean: 'No red flags', caution: 'Caution', red_flags: 'Red flags',
    unknown: 'Unreadable verdict', none: 'Not scanned'
  };
  var VERDICT_SHORT = { clean: 'No red flags', caution: 'Caution', red_flags: 'Red flags', unknown: 'Unreadable', none: 'Not scanned' };
  var FAMILY_LABEL = {
    v2: 'Uniswap v2', v3: 'Uniswap v3', v4: 'Uniswap v4', pons: 'Pons launch curve',
    gecko: 'GeckoTerminal (no on-chain pool)', untracked: 'Not tracked'
  };
  var FAMILY_SHORT = { v2: 'v2', v3: 'v3', v4: 'v4', pons: 'Pons', gecko: 'GeckoTerminal', untracked: 'Not tracked' };
  var METRIC_WORD = { ret: 'return', peak: 'peak', dd: 'drawdown' };
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  var WEEKDAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];
  var DAY_MS = 86400000;

  // The factors of the "By factor" tab: key = the column (verdict: the
  // category column); kind = how its values read ('usd' | 'count' | 'pct').
  var FACTORS = [
    { key: 'verdict', label: 'Perceptor verdict at the call', kind: 'cat' },
    { key: 'mcap', label: 'Market cap at call (price × supply, fully diluted)', kind: 'usd' },
    { key: 'proof_elite', label: 'Elite holders', kind: 'count' },
    { key: 'proof_good', label: 'Good holders', kind: 'count' },
    { key: 'holders', label: 'Holders', kind: 'count' },
    { key: 'buys_elite_n', label: 'Elite buyers (count)', kind: 'count' },
    { key: 'buys_good_n', label: 'Good buyers (count)', kind: 'count' },
    { key: 'buys_elite_usd', label: 'Elite buys (USD)', kind: 'usd' },
    { key: 'buys_good_usd', label: 'Good buys (USD)', kind: 'usd' },
    { key: 'pre_buy_usd', label: 'Hour before the call: buy volume (USD)', kind: 'usd' },
    { key: 'pre_sell_usd', label: 'Hour before the call: sell volume (USD)', kind: 'usd' },
    { key: 'pre_swaps', label: 'Hour before the call: swaps', kind: 'count' },
    { key: 'pre_chg', label: 'Hour before the call: price change', kind: 'pct' }
  ];

  var state = {
    horizon: '1d', metric: 'ret', stat: 'median', quiet: 'all', period: 'week', family: 'all', verdict: 'all',
    tab: 'overview', factor: 'verdict', k: '5', path: 'all', tmGroup: 'verdict'
  };
  var data = null;        // the last body read
  var col = {};           // column name → index in a row
  var etag = '';
  var snapshotMs = NaN;   // X-Snapshot-At: "now" for "not due yet"
  var loading = false;
  var timer = null;
  var lastLoad = 0;
  var lastDraw = 0;

  function $(id) { return document.getElementById(id); }

  function el(tag, className, text) {
    var n = document.createElement(tag);
    if (className) { n.className = className; }
    if (text !== undefined && text !== null) { n.textContent = String(text); }
    return n;
  }

  var isNum = A.isNum;
  function pad2(n) { return n < 10 ? '0' + n : String(n); }
  function fmtInt(n) { return isNum(n) ? n.toLocaleString('en-US') : DASH; }
  function fmtTime(d) { return pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds()); }
  function fmtDay(ms) { var d = new Date(ms); return d.getUTCDate() + ' ' + MONTHS[d.getUTCMonth()] + ' ' + d.getUTCFullYear(); }
  function fmtCallTime(t) {
    var d = new Date(t * 1000);
    return fmtDay(t * 1000) + ' ' + pad2(d.getUTCHours()) + ':' + pad2(d.getUTCMinutes()) + ' UTC';
  }

  // Huge values in powers of ten with superscript digits (as on the list page):
  // 3.9e47 → "3.9×10⁴⁷".
  var SUPERSCRIPT = { '0': '⁰', '1': '¹', '2': '²', '3': '³', '4': '⁴', '5': '⁵', '6': '⁶', '7': '⁷', '8': '⁸', '9': '⁹', '-': '⁻' };
  function sciText(a) {
    var parts = Math.abs(a).toExponential(1).split('e');
    var exp = String(Number(parts[1]));
    var sup = '';
    for (var i = 0; i < exp.length; i++) { sup += SUPERSCRIPT[exp.charAt(i)] || ''; }
    return parts[0] + '×10' + sup;
  }

  // Signed percentage; the sign is always written, so colour is never the only cue.
  function fmtPct(v) {
    if (!isNum(v)) { return DASH; }
    var a = Math.abs(v);
    var body = a >= 1000 ? a.toLocaleString('en-US', { maximumFractionDigits: 0 }) : a.toFixed(1);
    var n = Number(body.replace(/,/g, ''));
    if (n === 0) { return '0.0%'; }
    if (n >= 1e6) { body = sciText(a); }
    return (v > 0 ? '+' : '−') + body + '%';
  }
  function exactPct(v) {
    return (v > 0 ? '+' : v < 0 ? '−' : '') + Math.abs(v).toLocaleString('en-US', { maximumFractionDigits: 1 }) + '%';
  }
  function fmtUSD(v) {
    if (!isNum(v)) { return DASH; }
    return (v < 0 ? '−$' : '+$') + Math.abs(Math.round(v)).toLocaleString('en-US');
  }
  function fmtFactor(v, kind) {
    if (kind === 'usd') { return '$' + Math.round(v).toLocaleString('en-US'); }
    if (kind === 'pct') { return fmtPct(v); }
    return fmtInt(v);
  }
  function statOf(list) { return state.stat === 'mean' ? A.mean(list) : A.median(list); }
  function statWord() { return state.stat === 'mean' ? 'mean' : 'median'; }
  function cap(s) { return s.charAt(0).toUpperCase() + s.slice(1); }

  // ---- rows ---------------------------------------------------------------

  function verdictKey(r) { return data.verdicts[r[col.verdict]] || 'none'; }
  function familyKey(r) { return data.families[r[col.family]] || 'untracked'; }
  function isUSD(r) { return (r[col.flags] & FLAG_USD) !== 0; }
  function isRugged(r) { return (r[col.flags] & FLAG_RUGGED) !== 0; }
  // the value of metric m (ret | peak | dd) at window h ('1d' …); NaN when missing
  function metricAt(r, m, h) {
    if (!isUSD(r)) { return NaN; }
    var v = r[col[m + '_' + h]];
    return isNum(v) ? v : NaN;
  }
  function now() { return isNum(snapshotMs) ? snapshotMs : Date.now(); }

  function isQuiet(r) {
    var t = r[col.trades_24h];
    return !isRugged(r) && isNum(t) && t < data.quiet_below;
  }
  function passQuiet(r) {
    if (state.quiet === 'exclude') { return !isQuiet(r); }
    if (state.quiet === 'only') { return isQuiet(r); }
    return true;
  }
  function passVerdict(r) { return state.verdict === 'all' || verdictKey(r) === state.verdict; }
  function passFamily(r) { return state.family === 'all' || familyKey(r) === state.family; }
  // The name of the pool family and verdict choice; "" when both are All,
  // and when only "No red flags" is chosen (that line is drawn anyway).
  function selectionName() {
    var parts = [];
    if (state.family !== 'all') { parts.push(FAMILY_LABEL[state.family] || state.family); }
    if (state.verdict !== 'all') {
      if (state.verdict === 'clean' && !parts.length) { return ''; }
      parts.push(VERDICT_LABEL[state.verdict] || state.verdict);
    }
    return parts.join(' · ');
  }

  // ---- grouping -----------------------------------------------------------

  // The accumulator of one group for the chosen window.
  function newAcc() {
    return {
      calls: 0, data: 0, notDue: 0, noData: 0, pending: 0, untracked: 0, noUsd: 0,
      rets: [], peaks: [], dds: [], wins: 0, collapses: 0, peak100: 0, rugged: 0, mix: [0, 0, 0, 0, 0]
    };
  }

  // What a row has for window h: 'data', or why it has none.
  function classify(r, h, iRet) {
    var flags = r[col.flags];
    if ((flags & FLAG_USD) && isNum(r[iRet])) { return 'data'; }
    // The window ends at the post time + its length, as the tracker counts it
    // (tracker_onchain.go and prices.go: due = entry_at + horizon, entry_at =
    // the post's time); late entry moves only the entry price, not the end.
    if ((r[col.t] + data.horizon_seconds[h]) * 1000 > now()) { return 'notDue'; }
    if (!(flags & FLAG_TRACKED)) { return 'untracked'; }
    if (!(flags & FLAG_USD)) { return 'noUsd'; }
    if ((r[col.no_data] >> h) & 1) { return 'noData'; }
    return 'pending';
  }

  function add(acc, r, kind, ix) {
    acc.calls++;
    acc[kind]++;
    if (kind !== 'data') { return; }
    var ret = r[ix.ret], peak = r[ix.peak], dd = r[ix.dd];
    acc.rets.push(ret);
    if (ret > 0) { acc.wins++; }
    if (ret <= -50) { acc.collapses++; }
    if (isNum(peak)) {
      acc.peaks.push(peak);
      if (peak >= 100) { acc.peak100++; }
    }
    if (isNum(dd)) { acc.dds.push(dd); }
    var rug = isRugged(r);
    if (rug) { acc.rugged++; }
    acc.mix[A.outcome(ret, rug)]++;
  }

  function periodStart(t) { return state.period === 'month' ? A.monthStart(t) : A.weekStart(t); }
  function periodEnd(start) {
    if (state.period === 'month') {
      var d = new Date(start);
      return Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1);
    }
    return start + 7 * DAY_MS;
  }
  function periodLabel(start) {
    var d = new Date(start);
    var s = state.period === 'month'
      ? MONTHS[d.getUTCMonth()] + ' ' + d.getUTCFullYear()
      : 'Week of Mon ' + d.getUTCDate() + ' ' + MONTHS[d.getUTCMonth()] + ' ' + d.getUTCFullYear();
    return periodEnd(start) > now() ? s + ' (so far)' : s;
  }
  function periodShort(start) {
    var d = new Date(start);
    return state.period === 'month' ? MONTHS[d.getUTCMonth()] + ' ' + String(d.getUTCFullYear()).slice(2)
      : d.getUTCDate() + ' ' + MONTHS[d.getUTCMonth()];
  }

  // A group map: key → { label, acc, order }.
  function group(map, key, label, order) {
    var g = map[key];
    if (!g) { g = map[key] = { label: label, acc: newAcc(), order: order }; }
    return g.acc;
  }
  function sorted(map) {
    var out = [];
    for (var k in map) { if (Object.prototype.hasOwnProperty.call(map, k)) { out.push(map[k]); } }
    out.sort(function (a, b) { return a.order - b.order; });
    return out;
  }

  // ---- tables -------------------------------------------------------------

  var METRIC_HEADS = [
    ['Calls', 'First calls in the group'],
    ['With data', 'Calls with a return for the window: the n of every number in the row'],
    ['Mean return', 'Average return over the window'],
    ['Median return', 'Middle return over the window'],
    ['Mean peak', 'Average of the highest price within the window, from the entry'],
    ['Median peak', 'Middle of the highest price within the window, from the entry'],
    ['Mean drop', 'Average of the lowest price within the window, from the entry'],
    ['Median drop', 'Middle of the lowest price within the window, from the entry'],
    ['Win rate', 'Share with a return above 0%'],
    ['≥ +100% peak', 'Share whose peak within the window was +100% or more'],
    ['Collapse', 'Share with a return of −50% or worse'],
    ['Rugged', 'Calls with data flagged rugged by the tracker (as of now)']
  ];

  function headRow(first, heads) {
    var thead = el('thead');
    var tr = el('tr');
    var th = el('th', 'tok', first);
    th.scope = 'col';
    tr.appendChild(th);
    heads.forEach(function (h) {
      var c = el('th', 'num', h[0]);
      c.scope = 'col';
      if (h[1]) { c.title = h[1]; }
      tr.appendChild(c);
    });
    thead.appendChild(tr);
    return thead;
  }

  function pctCell(v) {
    var td = el('td', 'num');
    td.textContent = fmtPct(v);
    if (isNum(v)) {
      td.classList.add(v > 0 ? 'pos' : v < 0 ? 'neg' : 'zero');
      if (td.textContent.indexOf('×') >= 0) { td.title = exactPct(v); }
    }
    return td;
  }

  // A share with its count under it ("120 / 249"): n beside every rate.
  function rateCell(k, n, what) {
    var td = el('td', 'num');
    if (n === 0) { td.textContent = DASH; return td; }
    td.appendChild(el('span', 'ana-rate', (k / n * 100).toFixed(1) + '%'));
    td.appendChild(el('span', 'ana-n', fmtInt(k) + ' / ' + fmtInt(n)));
    td.title = fmtInt(k) + ' of ' + fmtInt(n) + ' ' + what;
    return td;
  }

  function whyNone(a) {
    var parts = [fmtInt(a.data) + ' with data'];
    if (a.notDue) { parts.push(fmtInt(a.notDue) + ' not due yet'); }
    if (a.noData) { parts.push(fmtInt(a.noData) + ' due but no data'); }
    if (a.pending) { parts.push(fmtInt(a.pending) + ' waiting for the tracker'); }
    if (a.untracked) { parts.push(fmtInt(a.untracked) + ' not tracked'); }
    if (a.noUsd) { parts.push(fmtInt(a.noUsd) + ' not priced in USD'); }
    return 'Of ' + fmtInt(a.calls) + ' calls: ' + parts.join(', ');
  }

  function metricRow(label, a, labelTitle) {
    var tr = el('tr');
    var th = el('th', 'tok', label);
    th.scope = 'row';
    if (labelTitle) { th.title = labelTitle; }
    tr.appendChild(th);
    if (a.data < MIN_N) {
      tr.className = 'ana-few';
      th.title = (labelTitle ? labelTitle + '. ' : '') + 'Fewer than ' + MIN_N + ' calls with data';
    }
    tr.appendChild(el('td', 'num', fmtInt(a.calls)));
    var wd = el('td', 'num', fmtInt(a.data));
    wd.title = whyNone(a);
    tr.appendChild(wd);
    tr.appendChild(pctCell(A.mean(a.rets)));
    tr.appendChild(pctCell(A.median(a.rets)));
    tr.appendChild(pctCell(A.mean(a.peaks)));
    tr.appendChild(pctCell(A.median(a.peaks)));
    tr.appendChild(pctCell(A.mean(a.dds)));
    tr.appendChild(pctCell(A.median(a.dds)));
    tr.appendChild(rateCell(a.wins, a.data, 'calls with data were up'));
    tr.appendChild(rateCell(a.peak100, a.peaks.length, 'calls with a peak reached +100%'));
    tr.appendChild(rateCell(a.collapses, a.data, 'calls with data lost half or more'));
    tr.appendChild(rateCell(a.rugged, a.data, 'calls with data are flagged rugged'));
    return tr;
  }

  function emptyRow(tbody, cols, text) {
    var tr = el('tr');
    var td = el('td', 'muted', text);
    td.colSpan = cols;
    tr.appendChild(td);
    tbody.appendChild(tr);
  }

  function fillTable(id, first, groups) {
    var frag = document.createDocumentFragment();
    frag.appendChild(headRow(first, METRIC_HEADS));
    var tbody = el('tbody');
    groups.forEach(function (g) { tbody.appendChild(metricRow(g.label, g.acc, g.title)); });
    if (groups.length === 0) { emptyRow(tbody, METRIC_HEADS.length + 1, 'No calls.'); }
    frag.appendChild(tbody);
    $(id).replaceChildren(frag);
  }

  // A cell with a value, its n under it, greyed under MIN_N; heat = colour it;
  // more = a second line before the n (the other statistic).
  function valueCell(v, n, title, heat, more) {
    var td = el('td', 'num');
    if (!n || !isNum(v)) { td.textContent = DASH; return td; }
    td.appendChild(el('span', 'ana-rate ' + (v > 0 ? 'pos' : v < 0 ? 'neg' : 'zero'), fmtPct(v)));
    if (more) { td.appendChild(el('span', 'ana-n', more)); }
    td.appendChild(el('span', 'ana-n', 'n ' + fmtInt(n)));
    td.title = title + (n < MIN_N ? ' (fewer than ' + MIN_N + ')' : '');
    if (n < MIN_N) {
      td.classList.add('ana-few');
    } else if (heat) {
      td.classList.add(heatClass(v));
    }
    return td;
  }

  function fillMatrix(periods, verdictKeys) {
    var heads = verdictKeys.map(function (v) { return [VERDICT_LABEL[v] || v, 'Median return, mean under it; n = calls with data']; });
    var frag = document.createDocumentFragment();
    frag.appendChild(headRow(state.period === 'month' ? 'Month' : 'Week', heads));
    var tbody = el('tbody');
    periods.forEach(function (p) {
      var tr = el('tr');
      var th = el('th', 'tok', p.label);
      th.scope = 'row';
      tr.appendChild(th);
      verdictKeys.forEach(function (v) {
        var a = p.byVerdict[v];
        var m = a ? A.median(a.rets) : NaN;
        var mn = a ? A.mean(a.rets) : NaN;
        tr.appendChild(valueCell(m, a ? a.data : 0, (VERDICT_LABEL[v] || v) + ', ' + p.label + ': median ' +
          (isNum(m) ? exactPct(m) : DASH) + ', mean ' + (isNum(mn) ? exactPct(mn) : DASH) + ' over ' + fmtInt(a ? a.data : 0) + ' calls with data',
          false, 'mean ' + fmtPct(mn)));
      });
      tbody.appendChild(tr);
    });
    frag.appendChild(tbody);
    $('tbl-matrix').replaceChildren(frag);
  }

  function stat(dl, label, value, title) {
    var d = el('div');
    d.appendChild(el('dt', null, label));
    d.appendChild(el('dd', null, value));
    if (title) { d.title = title; }
    dl.appendChild(d);
  }

  // ---- SVG ----------------------------------------------------------------

  function svgEl(tag, attrs, parent) {
    var n = document.createElementNS(SVGNS, tag);
    if (attrs) {
      for (var k in attrs) {
        if (Object.prototype.hasOwnProperty.call(attrs, k) && attrs[k] !== undefined && attrs[k] !== null) {
          n.setAttribute(k, String(attrs[k]));
        }
      }
    }
    if (parent) { parent.appendChild(n); }
    return n;
  }
  function svgText(parent, x, y, text, cls, anchor) {
    var t = svgEl('text', { x: r1(x), y: r1(y), 'class': cls || null, 'text-anchor': anchor || null }, parent);
    t.textContent = text;
    return t;
  }
  function tip(node, text) { svgEl('title', null, node).textContent = text; }

  // The hover text of the points of a scatter chart is made when the pointer
  // first reaches a point (one listener per chart box), not for every point
  // up front: thousands of <title> elements made drawing twice as slow.
  // pointTips[box id] = function (i) → the text of point i (data-i).
  var pointTips = {};
  function lazyTips(id) {
    $(id).addEventListener('mouseover', function (ev) {
      var t = ev.target;
      if (!t || t.tagName !== 'circle' || !t.hasAttribute('data-i') || t.firstChild) { return; }
      var f = pointTips[id];
      if (f) { tip(t, f(Number(t.getAttribute('data-i')))); }
    });
  }
  function r1(v) { return Math.round(v * 10) / 10; }

  // A new chart in the box with this id: as wide as the box (so the text keeps
  // its size on a phone), height h.
  function newChart(id, h, label) {
    var box = $(id);
    var w = Math.max(280, Math.floor(box.clientWidth || 600));
    var svg = svgEl('svg', { viewBox: '0 0 ' + w + ' ' + h, width: w, height: h, role: 'img', 'aria-label': label });
    return { box: box, svg: svg, w: w, h: h };
  }
  function mount(c) { c.box.replaceChildren(c.svg); }
  function emptyChart(id, text) { $(id).replaceChildren(el('p', 'muted small', text)); }

  function lin(d0, d1, r0, r1_) {
    var k = d1 === d0 ? 0 : (r1_ - r0) / (d1 - d0);
    return function (v) { return r0 + (v - d0) * k; };
  }
  // linear ticks: about n round values between lo and hi
  function niceTicks(lo, hi, n) {
    if (!(hi > lo)) { return [lo]; }
    var step = Math.pow(10, Math.floor(Math.log10((hi - lo) / n)));
    var err = (hi - lo) / n / step;
    if (err >= 7.5) { step *= 10; } else if (err >= 3.5) { step *= 5; } else if (err >= 1.5) { step *= 2; }
    var out = [];
    for (var v = Math.ceil(lo / step) * step; v <= hi + step * 1e-9; v += step) { out.push(Math.abs(v) < step * 1e-9 ? 0 : v); }
    return out;
  }
  // a symmetric-log scale of percentages over the values (0 always in it)
  function symScale(values, r0, r1_) {
    var lo = 0, hi = 0;
    for (var i = 0; i < values.length; i++) {
      if (values[i] < lo) { lo = values[i]; }
      if (values[i] > hi) { hi = values[i]; }
    }
    lo = Math.max(lo, -100);
    var t0 = A.symlog(lo), t1 = A.symlog(hi);
    var pad = (t1 - t0) * 0.04 || 0.1;
    var f = lin(t0 - pad, t1 + pad, r0, r1_);
    var y = function (v) { return f(A.symlog(Math.max(v, -100))); };
    y.ticks = A.symlogTicks(lo, hi);
    if (y.ticks.indexOf(0) < 0) { y.ticks.push(0); }
    return y;
  }
  function yGrid(svg, ticks, y, x0, x1, fmt, side) {
    ticks.forEach(function (t) {
      var yy = r1(y(t));
      svgEl('line', { x1: x0, x2: x1, y1: yy, y2: yy, 'class': t === 0 && side !== 'right' ? 'zero-line' : 'grid' }, svg);
      if (side === 'right') {
        svgText(svg, x1 + 4, yy + 3.5, fmt(t), null, 'start');
      } else {
        svgText(svg, x0 - 4, yy + 3.5, fmt(t), null, 'end');
      }
    });
  }
  function axisPct(v) { return A.signedPct(v); }

  // a line through the points (x, y); NaN breaks it
  function linePath(pts) {
    var d = '';
    var pen = false;
    for (var i = 0; i < pts.length; i++) {
      var p = pts[i];
      if (!isNum(p[0]) || !isNum(p[1])) { pen = false; continue; }
      d += (pen ? 'L' : 'M') + r1(p[0]) + ' ' + r1(p[1]);
      pen = true;
    }
    return d;
  }

  function keyItem(ul, swClass, text, title) {
    var li = el('li');
    li.appendChild(el('span', 'sw ' + swClass));
    li.appendChild(document.createTextNode(text));
    if (title) { li.title = title; }
    ul.appendChild(li);
  }

  // The heat scale of the hour × weekday chart and the grid.
  var HEAT = [
    { cls: 'hm-n3', label: '≤ −50%' }, { cls: 'hm-n2', label: '−50% to −20%' }, { cls: 'hm-n1', label: '−20% to −5%' },
    { cls: 'hm-z', label: '−5% to +5%' }, { cls: 'hm-p1', label: '+5% to +20%' }, { cls: 'hm-p2', label: '+20% to +50%' },
    { cls: 'hm-p3', label: '≥ +50%' }
  ];
  function heatClass(v) {
    if (v <= -50) { return 'hm-n3'; }
    if (v <= -20) { return 'hm-n2'; }
    if (v <= -5) { return 'hm-n1'; }
    if (v < 5) { return 'hm-z'; }
    if (v < 20) { return 'hm-p1'; }
    if (v < 50) { return 'hm-p2'; }
    return 'hm-p3';
  }

  // ---- Overview -------------------------------------------------------------

  function drawTrend(periods) {
    var label = state.period === 'month' ? 'month' : 'week';
    $('h-trend').textContent = 'Trend by ' + label + ' (' + state.horizon + ' return)';
    if (!periods.length) { emptyChart('ch-trend', 'No calls.'); return; }
    var c = newChart('ch-trend', 300, 'Calls, collapse rate, mean and median return by ' + label);
    var L = 46, R = 40, T = 8, B = 22;
    var topH = 100, gap = 18;
    var x0 = L, x1 = c.w - R;
    var bw = (x1 - x0) / periods.length;
    var maxCalls = 1;
    var vals = [];
    periods.forEach(function (p) {
      if (p.acc.calls > maxCalls) { maxCalls = p.acc.calls; }
      p.mean = A.mean(p.acc.rets);
      p.median = A.median(p.acc.rets);
      if (isNum(p.mean)) { vals.push(p.mean); }
      if (isNum(p.median)) { vals.push(p.median); }
    });
    var cTicks = niceTicks(0, maxCalls, 3);
    var cMax = Math.max(maxCalls, cTicks[cTicks.length - 1]);
    var yc = lin(0, cMax, T + topH, T);
    var yr = lin(0, 100, T + topH, T);
    yGrid(c.svg, cTicks, yc, x0, x1, function (v) { return A.shortNum(v); });
    [0, 50, 100].forEach(function (t) { svgText(c.svg, x1 + 4, yr(t) + 3.5, t + '%', null, 'start'); });
    var bTop = T + topH + gap, bBot = c.h - B;
    var ys = symScale(vals, bBot, bTop);
    yGrid(c.svg, ys.ticks, ys, x0, x1, axisPct);
    var every = Math.max(1, Math.ceil(56 / bw));
    var collapse = [], mean = [], med = [];
    periods.forEach(function (p, i) {
      var cx = x0 + bw * (i + 0.5);
      var a = p.acc;
      var bh = Math.max(0, yc(0) - yc(a.calls));
      svgEl('rect', { x: r1(cx - bw * 0.35), y: r1(yc(a.calls)), width: r1(bw * 0.7), height: r1(bh), 'class': 'bar-count' }, c.svg);
      var cr = a.data ? a.collapses / a.data * 100 : NaN;
      collapse.push([cx, isNum(cr) ? yr(cr) : NaN, a.data < MIN_N]);
      mean.push([cx, isNum(p.mean) ? ys(p.mean) : NaN, a.data < MIN_N]);
      med.push([cx, isNum(p.median) ? ys(p.median) : NaN, a.data < MIN_N]);
      if (i % every === 0) { svgText(c.svg, cx, c.h - 6, periodShort(p.start), null, 'middle'); }
    });
    [[collapse, 'collapse'], [mean, 'mean'], [med, 'median']].forEach(function (s) {
      svgEl('path', { d: linePath(s[0]), 'class': 'ln ln-' + s[1] }, c.svg);
      s[0].forEach(function (p) {
        if (isNum(p[1])) { svgEl('circle', { cx: r1(p[0]), cy: r1(p[1]), r: 3, 'class': p[2] ? 'few' : 'dot-' + s[1] }, c.svg); }
      });
    });
    // one hover area per period, over both panels
    periods.forEach(function (p, i) {
      var a = p.acc;
      var hit = svgEl('rect', { x: r1(x0 + bw * i), y: T, width: r1(bw), height: bBot - T, 'class': 'hit' }, c.svg);
      tip(hit, p.label + ': ' + fmtInt(a.calls) + ' calls, ' + fmtInt(a.data) + ' with data' +
        (a.data ? '; mean ' + fmtPct(p.mean) + ', median ' + fmtPct(p.median) + ', collapse ' +
          (a.collapses / a.data * 100).toFixed(1) + '% (' + fmtInt(a.collapses) + ' / ' + fmtInt(a.data) + ')' : '') +
        (a.data < MIN_N ? ' — fewer than ' + MIN_N + ' calls with data' : ''));
    });
    svgText(c.svg, x0, T + topH + 13, 'Return (symmetric log scale)', 't-strong', 'start');
    mount(c);
    $('cap-trend').textContent = 'Bars: first calls per ' + label + ' (left axis, top). Collapse rate: right axis, top. Mean and median ' +
      state.horizon + ' return: bottom, over the calls with data.';
  }

  function drawMix(periods) {
    var label = state.period === 'month' ? 'month' : 'week';
    $('h-mix').textContent = 'Outcome mix by ' + label + ' (' + state.horizon + ' return)';
    var key = $('key-mix');
    key.replaceChildren();
    A.OUTCOMES.forEach(function (o, i) { keyItem(key, 'mix-' + i, o); });
    if (!periods.length) { emptyChart('ch-mix', 'No calls.'); return; }
    var c = newChart('ch-mix', 200, 'Outcome mix by ' + label);
    var L = 46, R = 12, T = 8, B = 22;
    var x0 = L, x1 = c.w - R;
    var bw = (x1 - x0) / periods.length;
    var y = lin(0, 100, c.h - B, T);
    yGrid(c.svg, [0, 25, 50, 75, 100], y, x0, x1, function (v) { return v + '%'; });
    var every = Math.max(1, Math.ceil(56 / bw));
    periods.forEach(function (p, i) {
      var a = p.acc;
      var cx = x0 + bw * (i + 0.5);
      if (i % every === 0) { svgText(c.svg, cx, c.h - 6, periodShort(p.start), null, 'middle'); }
      if (!a.data) { return; }
      var g = svgEl('g', { 'class': a.data < MIN_N ? 'few-bar' : null }, c.svg);
      var acc = 0;
      a.mix.forEach(function (k, j) {
        if (!k) { return; }
        var share = k / a.data * 100;
        var seg = svgEl('rect', {
          x: r1(cx - bw * 0.38), y: r1(y(acc + share)), width: r1(bw * 0.76), height: r1(y(acc) - y(acc + share)), 'class': 'mix-' + j
        }, g);
        tip(seg, p.label + ' — ' + A.OUTCOMES[j] + ': ' + share.toFixed(1) + '% (' + fmtInt(k) + ' of ' + fmtInt(a.data) + ' calls with data)' +
          (a.data < MIN_N ? ' — fewer than ' + MIN_N + ' calls with data' : ''));
        acc += share;
      });
    });
    mount(c);
    $('cap-mix').textContent = 'Each bar: the calls with a ' + state.horizon + ' return in that ' + label +
      ' (100%). Rugged = flagged rugged by the tracker, whatever the return; the others by their return. Faded bars: fewer than ' + MIN_N + ' calls.';
  }

  // quietRows: the rows of the quiet choice only. "All calls" and "No red
  // flags at the call" are fixed baselines that ignore the pool family and
  // verdict choices; a third line follows those choices when one is set.
  function drawPL(quietRows, hIdx) {
    var hz = state.horizon;
    var iRet = col['ret_' + hz];
    var series = [
      { name: 'All calls', cls: 'c0', test: function () { return true; } },
      { name: 'No red flags at the call', cls: 'c2', test: function (r) { return verdictKey(r) === 'clean'; } }
    ];
    var chosen = selectionName();
    if (chosen) {
      series.push({ name: chosen, cls: 'c1', test: function (r) { return passFamily(r) && passVerdict(r); } });
    }
    var tMin = Infinity, tMax = -Infinity;
    series.forEach(function (s) {
      var pts = [];
      quietRows.forEach(function (r) {
        if (classify(r, hIdx, iRet) !== 'data' || !s.test(r)) { return; }
        pts.push({ t: r[col.t], ret: r[iRet] });
      });
      s.n = pts.length;
      s.pl = A.cumulativePL(pts, PL_CAP);
      if (s.pl.length) {
        tMin = Math.min(tMin, s.pl[0].t);
        tMax = Math.max(tMax, s.pl[s.pl.length - 1].t);
      }
    });
    var key = $('key-pl');
    key.replaceChildren();
    series.forEach(function (s) {
      var last = s.pl.length ? s.pl[s.pl.length - 1].cum : NaN;
      keyItem(key, 'sw-line ' + s.cls, s.name + ': ' + fmtInt(s.n) + ' calls, ' + fmtUSD(last) +
        (s.n ? ' (' + fmtUSD(last / s.n) + ' per call)' : ''));
    });
    $('h-pl').textContent = '$100 on every call (' + hz + ', return capped at +1,000%)';
    $('cap-pl').textContent = 'Running profit or loss if $100 went into every call with data and came out after ' + hz +
      ', in call order; each call\'s return capped at +1,000% (+$1,000); before tax and fees. ' +
      '"All calls" and "No red flags at the call" follow only the quiet choice, not the pool family or verdict choice' +
      (chosen ? '; the "' + chosen + '" line follows every choice.' :
        state.verdict === 'clean' ? '; your verdict choice is the "No red flags at the call" line.' :
          '; choose a pool family or a verdict to add a line for it.');
    if (!isFinite(tMin)) { emptyChart('ch-pl', 'No calls with data for this window.'); return; }
    if (tMax === tMin) { tMax = tMin + 3600; }
    var c = newChart('ch-pl', 240, 'Cumulative profit or loss of $100 on every call');
    var L = 64, R = 12, T = 8, B = 22;
    var x = lin(tMin, tMax, L, c.w - R);
    var lo = 0, hi = 0;
    series.forEach(function (s) {
      s.pl.forEach(function (p) {
        if (p.cum < lo) { lo = p.cum; }
        if (p.cum > hi) { hi = p.cum; }
      });
    });
    var ticks = niceTicks(lo, hi, 4);
    lo = Math.min(lo, ticks[0]);
    hi = Math.max(hi, ticks[ticks.length - 1]);
    var y = lin(lo, hi, c.h - B, T);
    yGrid(c.svg, ticks, y, L, c.w - R, function (v) { return (v < 0 ? '−$' : '$') + A.shortNum(Math.abs(v)); });
    var nT = Math.max(2, Math.floor((c.w - L - R) / 90));
    for (var i = 0; i <= nT; i++) {
      var t = tMin + (tMax - tMin) * i / nT;
      svgText(c.svg, x(t), c.h - 6, fmtDay(t * 1000).replace(/ \d{4}$/, ''), null, i === 0 ? 'start' : i === nT ? 'end' : 'middle');
    }
    series.forEach(function (s) {
      // one point per half pixel at most: the path stays small
      var pts = [];
      var lastX = -1;
      s.pl.forEach(function (p, j) {
        var px = x(p.t);
        if (px - lastX >= 0.5 || j === s.pl.length - 1) { pts.push([px, y(p.cum)]); lastX = px; }
      });
      var path = svgEl('path', { d: linePath(pts), 'class': 'ln cs ' + s.cls }, c.svg);
      tip(path, s.name + ': ' + fmtInt(s.n) + ' calls, ' + fmtUSD(s.pl.length ? s.pl[s.pl.length - 1].cum : NaN));
    });
    // hover columns: the running totals at that time
    var cols = Math.max(4, Math.floor((c.w - L - R) / 24));
    for (var k = 0; k < cols; k++) {
      var tEnd = tMin + (tMax - tMin) * (k + 1) / cols;
      var parts = series.map(function (s) {
        var v = NaN, n = 0;
        var a = 0, b = s.pl.length - 1;
        while (a <= b) { var m = (a + b) >> 1; if (s.pl[m].t <= tEnd) { v = s.pl[m].cum; n = m + 1; a = m + 1; } else { b = m - 1; } }
        return s.name + ' ' + (n ? fmtUSD(v) + ' after ' + fmtInt(n) + ' calls' : DASH);
      });
      var hit = svgEl('rect', { x: r1(x(tMin + (tMax - tMin) * k / cols)), y: T, width: r1((c.w - L - R) / cols), height: c.h - B - T, 'class': 'hit' }, c.svg);
      tip(hit, 'Up to ' + fmtCallTime(tEnd) + ': ' + parts.join('; '));
    }
    mount(c);
  }

  function drawHeat(rows) {
    var m = state.metric, hz = state.horizon;
    var cells = [];
    for (var d = 0; d < 7; d++) { cells.push([]); for (var hh = 0; hh < 24; hh++) { cells[d].push({ vals: [], calls: 0 }); } }
    rows.forEach(function (r) {
      var hw = A.hourWeekday(r[col.t]);
      var cell = cells[hw.weekday][hw.hour];
      cell.calls++;
      var v = metricAt(r, m, hz);
      if (isNum(v)) { cell.vals.push(v); }
    });
    $('h-heat').textContent = cap(statWord()) + ' ' + METRIC_WORD[m] + ' (' + hz + ') by hour and weekday of the call (UTC)';
    var key = $('key-heat');
    key.replaceChildren();
    HEAT.forEach(function (hc) { keyItem(key, hc.cls, hc.label); });
    keyItem(key, 'hm-few', 'Fewer than ' + MIN_N + ' calls with data');
    var c = newChart('ch-heat', 7 * 24 + 26, statWord() + ' ' + METRIC_WORD[m] + ' by hour and weekday');
    var L = 34, R = 4, T = 2;
    var cw = (c.w - L - R) / 24, ch = 24;
    var showText = cw >= 30;
    for (d = 0; d < 7; d++) {
      svgText(c.svg, L - 6, T + ch * d + ch / 2 + 4, WEEKDAYS[d], null, 'end');
      for (hh = 0; hh < 24; hh++) {
        var cell = cells[d][hh];
        var n = cell.vals.length;
        var v = statOf(cell.vals);
        var few = n < MIN_N;
        var rect = svgEl('rect', {
          x: r1(L + cw * hh), y: r1(T + ch * d), width: r1(cw), height: ch,
          'class': 'hm-cell ' + (few || !isNum(v) ? 'hm-few' : heatClass(v))
        }, c.svg);
        tip(rect, WEEKDAYS[d] + ' ' + pad2(hh) + ':00–' + pad2(hh) + ':59 UTC: ' +
          (n ? statWord() + ' ' + METRIC_WORD[m] + ' ' + exactPct(v) + ' over ' + fmtInt(n) + ' calls with data' : 'no calls with data') +
          ' (' + fmtInt(cell.calls) + ' calls)' + (few && n ? ' — fewer than ' + MIN_N : ''));
        if (showText && n) {
          svgText(c.svg, L + cw * hh + cw / 2, T + ch * d + ch / 2 + 3.5, A.signedPct(Math.round(v)).replace('%', ''),
            'hm-label' + (few ? ' hm-few-t' : ''), 'middle');
        }
      }
    }
    var step = cw >= 22 ? 1 : cw >= 11 ? 3 : 6;
    for (hh = 0; hh < 24; hh += step) { svgText(c.svg, L + cw * hh + cw / 2, T + ch * 7 + 14, pad2(hh), null, 'middle'); }
    mount(c);
    $('cap-heat').textContent = 'Rows: weekday; columns: hour of the post (UTC).' + (showText ? ' Cell text: the value in percent.' : ' Hover over a cell for its numbers.');
  }

  function renderOverview(base, hIdx) {
    var hz = state.horizon;
    var ix = { ret: col['ret_' + hz], peak: col['peak_' + hz], dd: col['dd_' + hz] };
    var all = {}, byVerdict = {}, byFamily = {}, byDex = {}, byQuote = {}, byPeriod = {};
    var usedVerdicts = {};
    var quiet = 0, quietKnown = 0, quietUnknown = 0;
    var otherDex = {}, otherQuote = {}; // the names under "Other" in this selection
    var rows = [];
    base.forEach(function (r) {
      var known = isNum(r[col.trades_24h]);
      if (known) { quietKnown++; } else { quietUnknown++; }
      if (isQuiet(r)) { quiet++; }
      if (!passQuiet(r)) { return; }
      rows.push(r);
      var kind = classify(r, hIdx, ix.ret);

      add(group(all, 'all', 'All first calls', 0), r, kind, ix);

      var vi = r[col.verdict];
      var vk = data.verdicts[vi] || 'none';
      usedVerdicts[vk] = true;
      add(group(byVerdict, vk, VERDICT_LABEL[vk] || vk, vi), r, kind, ix);

      var fk = familyKey(r);
      var fi = r[col.family];
      add(group(byFamily, fk, FAMILY_LABEL[fk] || fk, fi < 0 ? 1e6 : fi), r, kind, ix);

      var di = r[col.dex];
      if (di < 0) {
        add(group(byDex, 'none', 'Not given', 2e6), r, kind, ix);
      } else if (di < TOP_DEX) {
        add(group(byDex, 'd' + di, data.dexes[di], di), r, kind, ix);
      } else {
        otherDex[di] = true;
        add(group(byDex, 'other', 'Other', 1e6), r, kind, ix);
      }

      var qi = r[col.quote];
      if (qi < 0) {
        add(group(byQuote, 'none', 'Unknown (no on-chain pool)', 2e6), r, kind, ix);
      } else if (qi < TOP_QUOTE) {
        add(group(byQuote, 'q' + qi, data.quotes[qi], qi), r, kind, ix);
      } else {
        otherQuote[qi] = true;
        add(group(byQuote, 'other', 'Other', 1e6), r, kind, ix);
      }

      var ps = periodStart(r[col.t]);
      var p = byPeriod[ps];
      if (!p) { p = byPeriod[ps] = { label: periodLabel(ps), acc: newAcc(), order: -ps, start: ps, byVerdict: {} }; }
      add(p.acc, r, kind, ix);
      var pv = p.byVerdict[vk];
      if (!pv) { pv = p.byVerdict[vk] = newAcc(); }
      add(pv, r, kind, ix);
    });

    // "Other" names only what the rows shown hold, not the whole dictionary
    if (byDex.other) { byDex.other.label = 'Other (' + fmtInt(Object.keys(otherDex).length) + ' names)'; }
    if (byQuote.other) { byQuote.other.label = 'Other (' + fmtInt(Object.keys(otherQuote).length) + ')'; }

    // the counts of the whole selection
    var a = all.all ? all.all.acc : newAcc();
    var dl = $('ana-counts');
    dl.replaceChildren();
    stat(dl, 'Calls', fmtInt(a.calls));
    stat(dl, 'With data (' + hz + ')', fmtInt(a.data), whyNone(a));
    stat(dl, 'Not due yet', fmtInt(a.notDue), 'Called less than ' + hz + ' ago');
    stat(dl, 'Due, no data', fmtInt(a.noData), 'The window has passed but the price source had no trades for it');
    stat(dl, 'Waiting for the tracker', fmtInt(a.pending), 'Due, tracked in USD, but not recorded yet');
    stat(dl, 'Not tracked / no USD', fmtInt(a.untracked + a.noUsd), fmtInt(a.untracked) + ' not tracked, ' + fmtInt(a.noUsd) + ' not priced in USD');
    var note = 'Quiet after the call (fewer than ' + fmtInt(data.quiet_below) + ' trades in 24 hours, not rugged): ' +
      fmtInt(quiet) + ' of the ' + fmtInt(quietKnown) + ' calls whose count is known; unknown for ' + fmtInt(quietUnknown) +
      ' (not priced from an on-chain pool, or the first day is not stored yet).';
    if (state.quiet === 'exclude') { note += ' Quiet calls are left out below.'; }
    if (state.quiet === 'only') { note += ' Only quiet calls are counted below.'; }
    $('ana-counts-note').textContent = note;
    $('lg-quiet').textContent = fmtInt(data.quiet_below);

    var periods = sorted(byPeriod);
    var chron = periods.slice().reverse();
    drawTrend(chron);
    drawMix(chron);
    drawHeat(rows);

    fillTable('tbl-all', 'Group', sorted(all));
    fillTable('tbl-verdict', 'Perceptor at the call', sorted(byVerdict));
    fillTable('tbl-family', 'Pool family', sorted(byFamily));
    fillTable('tbl-dex', 'DEX in the post', sorted(byDex));
    fillTable('tbl-quote', 'Quote asset', sorted(byQuote));
    var word = state.period === 'month' ? 'month' : 'week';
    $('t-period').textContent = 'By ' + word + ' (UTC' + (word === 'week' ? ', Monday to Sunday' : '') + ')';
    $('t-matrix').textContent = 'Median (and mean) return by ' + word + ' and Perceptor verdict at the call';
    fillTable('tbl-period', word === 'week' ? 'Week' : 'Month', periods);
    var vkeys = data.verdicts.filter(function (v) { return usedVerdicts[v]; });
    fillMatrix(periods, vkeys);
    return rows.length;
  }

  // ---- By factor -------------------------------------------------------------

  function factorDef() {
    for (var i = 0; i < FACTORS.length; i++) { if (FACTORS[i].key === state.factor) { return FACTORS[i]; } }
    return FACTORS[0];
  }
  function factorValue(r, f) {
    if (f.kind === 'cat') { return r[col.verdict]; }
    var v = r[col[f.key]];
    return isNum(v) ? v : NaN;
  }
  function fmtShortFactor(v, kind) {
    if (kind === 'usd') { return A.shortUSD(v); }
    if (kind === 'pct') { return A.signedPct(v); }
    return A.shortNum(v);
  }

  // The groups of the chosen factor: [{ label, key, lo, hi }] and of(r): the
  // group index of a row (−1 = no value). Number factors: equal-count groups
  // over the calls with data for the window and a value.
  function factorGroups(rows, f, hIdx) {
    var iRet = col['ret_' + state.horizon];
    if (f.kind === 'cat') {
      var present = {};
      rows.forEach(function (r) { present[r[col.verdict]] = true; });
      var groups = [], pos = {};
      data.verdicts.forEach(function (v, i) {
        if (!present[i]) { return; }
        pos[i] = groups.length;
        groups.push({ label: VERDICT_LABEL[v] || v, short: VERDICT_SHORT[v] || v, key: v });
      });
      return { groups: groups, of: function (r) { var p = pos[r[col.verdict]]; return p === undefined ? -1 : p; } };
    }
    var vals = [];
    rows.forEach(function (r) {
      if (classify(r, hIdx, iRet) !== 'data') { return; }
      var v = factorValue(r, f);
      if (isNum(v)) { vals.push(v); }
    });
    var eq = A.equalCountGroups(vals, Number(state.k));
    var labels = A.rangeLabels(eq.groups, f.kind);
    var gs = eq.groups.map(function (g, i) {
      var lab = labels[i];
      return { label: lab, short: lab, lo: g.lo, hi: g.hi, zero: g.zero };
    });
    return { groups: gs, of: function (r) { var v = factorValue(r, f); return isNum(v) ? eq.index(v) : -1; } };
  }

  function renderFactor(rows, hIdx) {
    var f = factorDef();
    var hz = state.horizon, m = state.metric;
    $('ana-k-box').hidden = f.kind === 'cat';
    var fg = factorGroups(rows, f, hIdx);
    var G = fg.groups;
    var ix = { ret: col['ret_' + hz], peak: col['peak_' + hz], dd: col['dd_' + hz] };
    var accs = G.map(function () { return newAcc(); });
    var members = G.map(function () { return []; });
    var noValue = 0;
    rows.forEach(function (r) {
      var gi = fg.of(r);
      if (gi < 0) { noValue++; return; }
      members[gi].push(r);
      add(accs[gi], r, classify(r, hIdx, ix.ret), ix);
    });
    $('factor-note').textContent = f.label + (f.kind === 'cat' ? '' : ', ' + G.length + ' groups') + '; window ' + hz + '. ' +
      (noValue ? fmtInt(noValue) + ' of ' + fmtInt(rows.length) + ' calls have no value for this factor and are left out.' : 'Every call has a value.') +
      (f.key === 'pre_buy_usd' || f.key === 'pre_sell_usd' ? ' Volumes measured in a quote asset rather than USD count as no value.' : '');

    // 6. histograms and the table
    drawHistograms(G, members, m, hz);
    var tableGroups = G.map(function (g, i) { return { label: g.label, acc: accs[i] }; });
    fillTable('tbl-factor', f.label, tableGroups);
    $('t-factor').textContent = 'Per group (' + hz + ')';

    // 7. scatter
    drawScatter(f, G, members, m, hz);
    // 8. path
    drawPath(G, members, m);
    // 9. verdict × family
    fillGrid(rows, m, hz);
  }

  function drawHistograms(G, members, m, hz) {
    var box = $('ch-hist');
    $('h-hist').textContent = 'Distribution of the ' + hz + ' ' + METRIC_WORD[m] + ' per group';
    box.replaceChildren();
    if (!G.length) { box.appendChild(el('p', 'muted small', 'No calls with data and a value.')); return; }
    var hists = G.map(function (g, i) {
      var list = [];
      members[i].forEach(function (r) { var v = metricAt(r, m, hz); if (isNum(v)) { list.push(v); } });
      return { counts: A.histogram(list), n: list.length };
    });
    var maxShare = 0;
    hists.forEach(function (h) { h.counts.forEach(function (k) { if (h.n && k / h.n > maxShare) { maxShare = k / h.n; } }); });
    var yMax = Math.min(100, Math.max(10, Math.ceil(maxShare * 100 / 10) * 10));
    var cards = G.map(function (g, i) {
      var card = el('div', 'ana-multiple chart' + (hists[i].n < MIN_N ? ' is-few' : ''));
      card.appendChild(el('h4', null, g.label + ' · n ' + fmtInt(hists[i].n)));
      box.appendChild(card);
      return card;
    });
    var w = Math.max(200, Math.floor(cards[0].clientWidth - 16 || 230));
    var h = 130;
    var L = 30, R = 4, T = 6, B = 18;
    var bands = A.BANDS.length;
    var bw = (w - L - R) / bands;
    var y = lin(0, yMax, h - B, T);
    var BAND_TICK = ['−100', '−75', '−50', '−25', '0', '+25', '+50', '+100', '+300'];
    G.forEach(function (g, i) {
      var hst = hists[i];
      var svg = svgEl('svg', { viewBox: '0 0 ' + w + ' ' + h, width: w, height: h, role: 'img',
        'aria-label': 'Distribution of the ' + METRIC_WORD[m] + ' for ' + g.label });
      yGrid(svg, [0, yMax / 2, yMax], y, L, w - R, function (v) { return v + '%'; });
      hst.counts.forEach(function (k, j) {
        var share = hst.n ? k / hst.n * 100 : 0;
        var bar = svgEl('rect', { x: r1(L + bw * j + 1), y: r1(y(share)), width: r1(bw - 2), height: r1(y(0) - y(share)), 'class': 'hist-bar' }, svg);
        tip(bar, g.label + ', ' + A.BANDS[j].label + '%: ' + share.toFixed(1) + '% (' + fmtInt(k) + ' of ' + fmtInt(hst.n) + ' calls)');
        // a full-height hover area, so even an empty band can be hovered
        var hit = svgEl('rect', { x: r1(L + bw * j), y: T, width: r1(bw), height: h - B - T, 'class': 'hit' }, svg);
        tip(hit, g.label + ', ' + A.BANDS[j].label + '%: ' + share.toFixed(1) + '% (' + fmtInt(k) + ' of ' + fmtInt(hst.n) + ' calls)');
        if (j === bands - 1) { svgText(svg, w - R, h - 5, BAND_TICK[j], null, 'end'); } else if (bw >= 26 || j % 2 === 0) { svgText(svg, L + bw * j + 1, h - 5, BAND_TICK[j], null, 'start'); }
      });
      cards[i].appendChild(svg);
    });
    $('cap-hist').textContent = 'Bands of the ' + METRIC_WORD[m] + ' in percent: −100 to −75, −75 to −50, −50 to −25, −25 to 0, 0 to +25, +25 to +50, +50 to +100, +100 to +300, +300 and above. ' +
      'A value on a boundary goes to the band farther from 0: a band below 0 includes its upper end (−50 is in −75 to −50, 0 in −25 to 0), ' +
      'a band above 0 its lower end (+25 is in +25 to +50), and 0 to +25 starts just above 0. ' +
      'Each bar is a share of its group; every group uses the same axes. Grey: fewer than ' + MIN_N + ' calls.';
  }

  function drawScatter(f, G, members, m, hz) {
    $('h-scatter').textContent = 'Each call: ' + f.label + ' and the ' + hz + ' ' + METRIC_WORD[m];
    var key = $('key-scatter');
    key.replaceChildren();
    G.forEach(function (g, i) { keyItem(key, 'c' + (i % 10), g.short); });
    keyItem(key, 'sw-line', 'Group median');
    keyItem(key, 'sw-dash', 'Group mean');
    var pts = [];
    var yVals = [];
    var xVals = [];
    G.forEach(function (g, i) {
      members[i].forEach(function (r) {
        var v = metricAt(r, m, hz);
        if (!isNum(v)) { return; }
        var xv = factorValue(r, f);
        pts.push({ r: r, g: i, x: xv, y: v });
        yVals.push(v);
        if (f.kind !== 'cat') { xVals.push(xv); }
      });
    });
    if (!pts.length) { emptyChart('ch-scatter', 'No calls with data and a value.'); return; }
    var c = newChart('ch-scatter', 340, 'Each call by ' + f.label + ' and ' + METRIC_WORD[m]);
    var L = 46, R = 12, T = 8, B = 24;
    // narrow: the category names on two rows, so they do not run into each other
    var stagger = f.kind === 'cat' && (c.w - L - R) / G.length < 90;
    if (stagger) { B = 36; }
    var y = symScale(yVals, c.h - B, T);
    yGrid(c.svg, y.ticks, y, L, c.w - R, axisPct);
    var xOf, xTicks = [], tf;
    if (f.kind === 'cat') {
      var bw = (c.w - L - R) / G.length;
      xOf = function (p) { return L + bw * (p.g + 0.5 + A.jitter(p.r[col.call_id]) * 0.7); };
      G.forEach(function (g, i) { svgText(c.svg, L + bw * (i + 0.5), c.h - (stagger && i % 2 ? 4 : 18) + (stagger ? 0 : 12), g.short, null, 'middle'); });
    } else {
      tf = f.kind === 'pct' ? A.symlog : A.log1p10;
      var lo = Infinity, hi = -Infinity;
      xVals.forEach(function (v) { if (v < lo) { lo = v; } if (v > hi) { hi = v; } });
      var t0 = tf(lo), t1 = tf(hi);
      if (t1 === t0) { t0 -= 0.5; t1 += 0.5; }
      var pad = (t1 - t0) * 0.03;
      var sx = lin(t0 - pad, t1 + pad, L, c.w - R);
      xOf = function (p) { return sx(tf(p.x)); };
      xTicks = f.kind === 'pct' ? A.symlogTicks(lo, hi) : A.log1pTicks(hi).filter(function (v) { return v >= lo || v === 0; });
      var lastX = -Infinity;
      xTicks.forEach(function (t) {
        var px = sx(tf(t));
        svgEl('line', { x1: r1(px), x2: r1(px), y1: T, y2: c.h - B, 'class': 'grid' }, c.svg);
        if (px - lastX >= 40) { svgText(c.svg, px, c.h - 6, fmtShortFactor(t, f.kind), null, 'middle'); lastX = px; }
      });
    }
    var frag = document.createDocumentFragment();
    pts.forEach(function (p, i) {
      svgEl('circle', { cx: r1(xOf(p)), cy: r1(y(p.y)), r: 2.6, 'class': 'pt cf c' + (p.g % 10), 'data-i': i }, frag);
    });
    c.svg.appendChild(frag);
    pointTips['ch-scatter'] = function (i) {
      var p = pts[i];
      return 'Call ' + p.r[col.call_id] + ' · ' + fmtCallTime(p.r[col.t]) + ' · ' + f.label + ': ' +
        (f.kind === 'cat' ? G[p.g].label : fmtFactor(p.x, f.kind)) + ' · ' + hz + ' ' + METRIC_WORD[m] + ' ' + exactPct(p.y);
    };
    // the mean and median of each group, across its range of x
    G.forEach(function (g, i) {
      var list = [];
      members[i].forEach(function (r) { var v = metricAt(r, m, hz); if (isNum(v)) { list.push(v); } });
      if (!list.length) { return; }
      var xa, xb;
      if (f.kind === 'cat') {
        var bw2 = (c.w - L - R) / G.length;
        xa = L + bw2 * (i + 0.12);
        xb = L + bw2 * (i + 0.88);
      } else {
        xa = xOf({ x: g.lo });
        xb = xOf({ x: g.hi });
        if (xb - xa < 8) { xa -= 4; xb += 4; }
      }
      var md = A.median(list), mn = A.mean(list);
      var few = list.length < MIN_N ? ' few' : '';
      var l1 = svgEl('path', { d: 'M' + r1(xa) + ' ' + r1(y(md)) + 'H' + r1(xb), 'class': 'ln cs c' + (i % 10) + few }, c.svg);
      tip(l1, g.label + ': median ' + exactPct(md) + ' (n ' + fmtInt(list.length) + ')');
      var l2 = svgEl('path', { d: 'M' + r1(xa) + ' ' + r1(y(mn)) + 'H' + r1(xb), 'class': 'ln ln-dash cs c' + (i % 10) + few }, c.svg);
      tip(l2, g.label + ': mean ' + exactPct(mn) + ' (n ' + fmtInt(list.length) + ')');
    });
    mount(c);
    $('cap-scatter').textContent = fmtInt(pts.length) + ' calls. Up: the ' + METRIC_WORD[m] + ' on a symmetric log scale. ' +
      (f.kind === 'cat' ? 'Across: the verdict (points spread sideways so they do not overlap).'
        : f.kind === 'pct' ? 'Across: the price change on a symmetric log scale.' : 'Across: a log scale that keeps 0.') +
      ' Hover over a point for the call.';
  }

  function drawPath(G, members, m) {
    var H = data.horizons;
    var same = state.path === 'same';
    $('h-path').textContent = 'Typical path after the call: ' + statWord() + ' ' + METRIC_WORD[m];
    var stats = G.map(function (g, i) {
      var rowsG = members[i];
      if (same) {
        rowsG = rowsG.filter(function (r) { return H.every(function (h) { return isNum(metricAt(r, m, h)); }); });
      }
      return H.map(function (h) {
        var list = [];
        rowsG.forEach(function (r) { var v = metricAt(r, m, h); if (isNum(v)) { list.push(v); } });
        return { v: statOf(list), other: state.stat === 'mean' ? A.median(list) : A.mean(list), n: list.length };
      });
    });
    var key = $('key-path');
    key.replaceChildren();
    G.forEach(function (g, i) { keyItem(key, 'sw-line c' + (i % 10), g.short); });
    // the table: n at every point
    var frag = document.createDocumentFragment();
    var otherWord = state.stat === 'mean' ? 'median' : 'mean';
    frag.appendChild(headRow('Group', H.map(function (h) { return [h, cap(statWord()) + ' ' + METRIC_WORD[m] + ' at ' + h + ', the ' + otherWord + ' under it; n = calls with data']; })));
    var tbody = el('tbody');
    G.forEach(function (g, i) {
      var tr = el('tr');
      var th = el('th', 'tok', g.label);
      th.scope = 'row';
      tr.appendChild(th);
      stats[i].forEach(function (s, j) {
        tr.appendChild(valueCell(s.v, s.n, g.label + ' at ' + H[j] + ': ' + statWord() + ' ' + (isNum(s.v) ? exactPct(s.v) : DASH) + ', ' + otherWord + ' ' +
          (isNum(s.other) ? exactPct(s.other) : DASH) + ' over ' + fmtInt(s.n) + ' calls', false, otherWord + ' ' + fmtPct(s.other)));
      });
      tbody.appendChild(tr);
    });
    if (!G.length) { emptyRow(tbody, H.length + 1, 'No calls.'); }
    frag.appendChild(tbody);
    $('tbl-path').replaceChildren(frag);
    var vals = [];
    stats.forEach(function (s) { s.forEach(function (p) { if (isNum(p.v)) { vals.push(p.v); } }); });
    if (!vals.length) { emptyChart('ch-path', 'No calls with data.'); $('cap-path').textContent = ''; return; }
    var c = newChart('ch-path', 260, 'Typical path after the call per group');
    var L = 46, R = 24, T = 8, B = 22;
    var y = symScale(vals, c.h - B, T);
    yGrid(c.svg, y.ticks, y, L, c.w - R, axisPct);
    var x = lin(0, H.length - 1, L + 10, c.w - R);
    H.forEach(function (h, j) { svgText(c.svg, x(j), c.h - 6, h, null, 'middle'); });
    stats.forEach(function (s, i) {
      var pts = s.map(function (p, j) { return [x(j), isNum(p.v) ? y(p.v) : NaN]; });
      svgEl('path', { d: linePath(pts), 'class': 'ln cs c' + (i % 10) }, c.svg);
      s.forEach(function (p, j) {
        if (!isNum(p.v)) { return; }
        var dot = svgEl('circle', { cx: r1(x(j)), cy: r1(y(p.v)), r: 3.5, 'class': 'cf c' + (i % 10) + (p.n < MIN_N ? ' few' : '') }, c.svg);
        tip(dot, G[i].label + ' at ' + H[j] + ': ' + statWord() + ' ' + exactPct(p.v) + ' (n ' + fmtInt(p.n) + ')');
      });
    });
    mount(c);
    $('cap-path').textContent = (same ? 'Only the calls with data at all five points (the same calls at every point).'
      : 'Every call with data at each point: n differs from point to point (table below).') +
      ' Groups by the factor as above (cut at the chosen window). Hollow points: fewer than ' + MIN_N + ' calls.';
  }

  function fillGrid(rows, m, hz) {
    $('t-grid').textContent = cap(statWord()) + ' ' + hz + ' ' + METRIC_WORD[m] + ': Perceptor verdict at the call × pool family';
    var cells = {}, fams = {}, verds = {};
    rows.forEach(function (r) {
      var v = metricAt(r, m, hz);
      if (!isNum(v)) { return; }
      var vk = verdictKey(r), fk = familyKey(r);
      fams[fk] = true;
      verds[vk] = true;
      var k = vk + '|' + fk;
      (cells[k] = cells[k] || []).push(v);
    });
    var fk = data.families.filter(function (f) { return fams[f]; });
    var vk = data.verdicts.filter(function (v) { return verds[v]; });
    var frag = document.createDocumentFragment();
    frag.appendChild(headRow('Verdict at the call', fk.map(function (f) { return [FAMILY_SHORT[f] || f, FAMILY_LABEL[f] || f]; })));
    var tbody = el('tbody');
    vk.forEach(function (v) {
      var tr = el('tr');
      var th = el('th', 'tok', VERDICT_LABEL[v] || v);
      th.scope = 'row';
      tr.appendChild(th);
      fk.forEach(function (f) {
        var list = cells[v + '|' + f] || [];
        var s = statOf(list);
        tr.appendChild(valueCell(s, list.length, (VERDICT_LABEL[v] || v) + ', ' + (FAMILY_LABEL[f] || f) + ': ' + statWord() + ' ' +
          (isNum(s) ? exactPct(s) : DASH) + ' over ' + fmtInt(list.length) + ' calls with data', true));
      });
      tbody.appendChild(tr);
    });
    if (!vk.length) { emptyRow(tbody, 1, 'No calls with data.'); }
    frag.appendChild(tbody);
    $('tbl-grid').replaceChildren(frag);
  }

  // ---- Peak vs final ---------------------------------------------------------

  function renderPeak(rows) {
    var hz = state.horizon;
    var pts = [];
    var all = [];
    rows.forEach(function (r) {
      var p = metricAt(r, 'peak', hz), v = metricAt(r, 'ret', hz);
      if (!isNum(p) || !isNum(v)) { return; }
      pts.push({ r: r, peak: p, ret: v });
      all.push(p, v);
    });
    fillExitTable(pts, hz);
    var gb = A.gaveBack(pts);
    var big = $('peak-big');
    big.replaceChildren();
    if (gb.n) {
      big.appendChild(el('strong', null, (gb.k / gb.n * 100).toFixed(1) + '%'));
      big.appendChild(document.createTextNode(' of the calls with a peak above 0% gave back more than half of it by the end of the ' +
        hz + ' window (' + fmtInt(gb.k) + ' of ' + fmtInt(gb.n) + ').'));
    } else {
      big.textContent = 'No calls with a peak above 0% for this window.';
    }
    if (!pts.length) { emptyChart('ch-peak', 'No calls with data for this window.'); $('cap-peak').textContent = ''; return; }
    var c = newChart('ch-peak', Math.min(460, Math.max(300, Math.round($('ch-peak').clientWidth * 0.6))), 'Peak against return, each call');
    var L = 46, R = 12, T = 8, B = 24;
    var y = symScale(all, c.h - B, T);
    var x = symScale(all, L, c.w - R);
    yGrid(c.svg, y.ticks, y, L, c.w - R, axisPct);
    var lastX = -Infinity;
    x.ticks.forEach(function (t) {
      var px = x(t);
      svgEl('line', { x1: r1(px), x2: r1(px), y1: T, y2: c.h - B, 'class': t === 0 ? 'zero-line' : 'grid' }, c.svg);
      if (px - lastX >= 36) { svgText(c.svg, px, c.h - 6, axisPct(t), null, 'middle'); lastX = px; }
    });
    // the diagonal: return = peak (the call ended at its peak)
    var lo = Math.max(-100, Math.min.apply(null, all)), hi = Math.max.apply(null, all);
    var diag = [];
    for (var i = 0; i <= 60; i++) { var v = A.symlogInv(A.symlog(lo) + (A.symlog(hi) - A.symlog(lo)) * i / 60); diag.push([x(v), y(v)]); }
    svgEl('path', { d: linePath(diag), 'class': 'ref-line' }, c.svg);
    var frag = document.createDocumentFragment();
    pts.forEach(function (p, i) {
      var gave = p.peak > 0 && p.ret < p.peak / 2;
      svgEl('circle', { cx: r1(x(p.peak)), cy: r1(y(p.ret)), r: 2.6, 'class': 'pt ' + (gave ? 'pt-gave' : 'pt-kept'), 'data-i': i }, frag);
    });
    c.svg.appendChild(frag);
    pointTips['ch-peak'] = function (i) {
      var p = pts[i];
      return 'Call ' + p.r[col.call_id] + ' · ' + fmtCallTime(p.r[col.t]) + ' · peak ' + exactPct(p.peak) + ' · ' + hz + ' return ' + exactPct(p.ret) +
        (isRugged(p.r) ? ' · rugged' : '');
    };
    svgText(c.svg, c.w - R - 4, c.h - B - 6, 'Peak →', 't-strong', 'end');
    svgText(c.svg, L + 4, T + 12, '↑ Return at ' + hz, 't-strong', 'start');
    mount(c);
    $('cap-peak').textContent = fmtInt(pts.length) + ' calls with a peak and a return for ' + hz + ', both on a symmetric log scale. ' +
      'Points on the dashed line ended at their peak; below it, they gave some back. Rugged calls keep their peak before the rug.';
  }

  // The exit-rule table: sell at the first touch of 2×, 3× or 5×, else hold
  // to the end of the window (A.exitReturn), against holding every call.
  // Over the same points as the chart (a peak and a return for the window),
  // so rugged calls keep their peak before the rug and their final return.
  var EXIT_RULES = [
    { m: 2, label: 'Sell at 2×' }, { m: 3, label: 'Sell at 3×' }, { m: 5, label: 'Sell at 5×' },
    { m: Infinity, label: 'Hold to the end' }
  ];
  var EXIT_HEADS = [
    ['n', 'Calls with a peak and a return for the window: the n of every number in the row'],
    ['Touched', 'Share of the calls whose peak within the window reached the exit multiple'],
    ['Mean return', 'Average return under the rule'],
    ['Median return', 'Middle return under the rule'],
    ['Mean vs hold', 'Mean return under the rule minus the mean when held to the end, in percentage points'],
    ['Median vs hold', 'Median return under the rule minus the median when held to the end, in percentage points']
  ];

  // A difference of two percentages, in percentage points ("+26.0 pp").
  function ppCell(v) {
    var td = pctCell(v);
    if (isNum(v)) {
      td.textContent = fmtPct(v).replace(/%$/, ' pp');
      td.title = exactPct(v).replace(/%$/, ' percentage points');
    }
    return td;
  }

  // One row of the exit-rule table: rule x, its stats s, and hold = the
  // stats of "hold to the end" (the baseline of the differences).
  function exitRow(x, s, hold) {
    var tr = el('tr');
    var th = el('th', 'tok', x.label);
    th.scope = 'row';
    tr.appendChild(th);
    if (s.n < MIN_N) {
      tr.className = 'ana-few';
      th.title = 'Fewer than ' + MIN_N + ' calls with data';
    }
    var base = x.m === Infinity;
    tr.appendChild(el('td', 'num', fmtInt(s.n)));
    tr.appendChild(base ? el('td', 'num', DASH) : rateCell(s.hit, s.n, 'calls touched ' + x.m + '× within the window'));
    tr.appendChild(pctCell(s.mean));
    tr.appendChild(pctCell(s.median));
    tr.appendChild(base ? el('td', 'num', DASH) : ppCell(s.mean - hold.mean));
    tr.appendChild(base ? el('td', 'num', DASH) : ppCell(s.median - hold.median));
    return tr;
  }

  function fillExitTable(pts, hz) {
    $('h-exit').textContent = 'Exit rule: sell at 2×, 3× or 5× (' + hz + ' window)';
    var frag = document.createDocumentFragment();
    frag.appendChild(headRow('Exit rule', EXIT_HEADS));
    var tbody = el('tbody');
    var hold = A.exitRuleStats(pts, Infinity);
    if (hold.n) {
      EXIT_RULES.forEach(function (x) { tbody.appendChild(exitRow(x, A.exitRuleStats(pts, x.m), hold)); });
    } else {
      emptyRow(tbody, EXIT_HEADS.length + 1, 'No calls with a peak and a return for this window.');
    }
    frag.appendChild(tbody);
    $('tbl-exit').replaceChildren(frag);
  }

  // ---- Timing -----------------------------------------------------------------
  //
  // When things happened after the call (time to peak, to the first 2×, time
  // held at 2×, time to fall back below 2×, time to rug), for the calls with
  // data for the window whose timing is computed ("timed"). Drawn with ECharts
  // (frontend/vendor, window.echarts): canvas only; tooltips are elements built
  // here with textContent (the library's own tooltip markup carries style
  // attributes, which the page's CSP blocks); colours from the CSS variables.

  // The groups of the Timing tab: the Perceptor verdict at the call, the week
  // or month (the Period choice), the pool family, the DEX named in the post,
  // and the number factors of the By factor tab.
  var TM_GROUPS = [
    { key: 'verdict', label: 'Perceptor verdict at the call', kind: 'cat' },
    { key: 'period', label: 'Week or month of the call (Period above)', kind: 'period' },
    { key: 'family', label: 'Pool family (DEX family)', kind: 'family' },
    { key: 'dex', label: 'DEX named in the post', kind: 'dex' }
  ].concat(FACTORS.filter(function (f) { return f.kind !== 'cat'; }));
  var TM_TICKS = [0, 300, 900, 3600, 14400, 86400, 259200, 604800, 2592000];
  var RUG_EVENT = 'event', RUG_AT_CALL = 'at_call', RUG_DETECTED = 'detected';
  var darkMQ = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
  var calmMQ = window.matchMedia ? window.matchMedia('(prefers-reduced-motion: reduce)') : null;
  var charts = {};      // box id → ECharts instance
  var themes = {};      // registered theme names
  var chartScheme = ''; // 'dark' | 'light': the scheme the charts were made in

  function nz(v) { return isNum(v) ? v : NaN; }

  // The timing of a row for window hz (index hIdx); null when it has none.
  // "Reached 2×" and everything that hangs on it come from A.timing2x (the
  // rule of A.reached2x: held at least A.MIN_HELD_2X_S in total, in every
  // window; a call that touched 2× without reaching it is treated like one
  // that never touched 2×, except that its real time held counts in the
  // time-held figures: owner decision, 13 October 2026).
  function timingOf(r, hIdx, hz) {
    if (!((r[col.timed] >> hIdx) & 1)) { return null; }
    var cens = ((r[col.cens_2x] >> hIdx) & 1) === 1;
    var x = A.timing2x(r[col['t2x_' + hz]], r[col['held_' + hz]], r[col['tfall_' + hz]], cens);
    return {
      peak: nz(r[col['tpeak_' + hz]]), t2x: x.t2x, held: x.held, tfall: x.tfall, cens: x.cens,
      reached: x.reached, short: x.short, hold: x.hold
    };
  }

  // The rug time of a row: { kind, s } (kind '' = not rugged; 'none' =
  // flagged rugged without a rug time).
  function rugOf(r) {
    var ki = r[col.rug_kind];
    if (!(ki >= 0)) { return { kind: isRugged(r) ? 'none' : '', s: NaN }; }
    return { kind: data.rug_kinds[ki] || 'other', s: nz(r[col.rug_s]) };
  }

  function newTm() {
    return {
      n: 0, reached: 0, cens: 0, short: 0, peaks: [], t2xs: [], helds: [], heldsShort: [], heldsReached: [], falls: [], holds: [],
      rugIn: [], rugLater: [], atCall: [], detected: [], other: 0, noTime: 0
    };
  }
  // Adds a timed call to an accumulator; H = the window in seconds.
  function addTm(acc, tm, rug, H) {
    acc.n++;
    if (isNum(tm.peak)) { acc.peaks.push(tm.peak); }
    if (isNum(tm.held)) { acc.helds.push(tm.held); }
    if (tm.short) {
      acc.short++;
      if (isNum(tm.held)) { acc.heldsShort.push(tm.held); }
    }
    if (tm.reached) {
      acc.reached++;
      acc.t2xs.push(tm.t2x);
      if (isNum(tm.held)) { acc.heldsReached.push(tm.held); }
      if (tm.cens) { acc.cens++; }
      if (tm.hold) { acc.holds.push(tm.hold); }
      if (isNum(tm.tfall)) { acc.falls.push(tm.tfall); }
    }
    switch (rug.kind) {
      case '': break;
      case RUG_EVENT:
        if (isNum(rug.s) && rug.s <= H) { acc.rugIn.push(Math.max(0, rug.s)); } else { acc.rugLater.push(rug.s); }
        break;
      case RUG_AT_CALL: acc.atCall.push(rug.s); break;
      case RUG_DETECTED: acc.detected.push(rug.s); break;
      case 'none': acc.noTime++; break;
      default: acc.other++;
    }
  }

  function tmGroupDef() {
    for (var i = 0; i < TM_GROUPS.length; i++) { if (TM_GROUPS[i].key === state.tmGroup) { return TM_GROUPS[i]; } }
    return TM_GROUPS[0];
  }

  // The groups of the timed calls by the chosen grouping: [{ label, acc }],
  // in the order of the other tabs (periods newest first), and how many
  // calls have no value for a number factor.
  function tmGroups(timed, H) {
    var g = tmGroupDef();
    var map = {};
    var noValue = 0;
    var otherDex = {};
    var keyOf;
    if (g.kind === 'cat') {
      keyOf = function (r) { var v = verdictKey(r); return { key: v, label: VERDICT_LABEL[v] || v, order: r[col.verdict] }; };
    } else if (g.kind === 'period') {
      keyOf = function (r) { var ps = periodStart(r[col.t]); return { key: 'p' + ps, label: periodLabel(ps), order: -ps }; };
    } else if (g.kind === 'family') {
      keyOf = function (r) { var f = familyKey(r), fi = r[col.family]; return { key: f, label: FAMILY_LABEL[f] || f, order: fi < 0 ? 1e6 : fi }; };
    } else if (g.kind === 'dex') {
      keyOf = function (r) {
        var di = r[col.dex];
        if (di < 0) { return { key: 'none', label: 'Not given', order: 2e6 }; }
        if (di < TOP_DEX) { return { key: 'd' + di, label: data.dexes[di], order: di }; }
        otherDex[di] = true;
        return { key: 'other', label: 'Other', order: 1e6 };
      };
    } else {
      var vals = [];
      timed.forEach(function (x) { var v = factorValue(x.r, g); if (isNum(v)) { vals.push(v); } });
      var eq = A.equalCountGroups(vals, Number(state.k));
      var labels = A.rangeLabels(eq.groups, g.kind);
      keyOf = function (r) {
        var v = factorValue(r, g);
        var i = isNum(v) ? eq.index(v) : -1;
        return i < 0 ? null : { key: 'g' + i, label: labels[i], order: i };
      };
    }
    timed.forEach(function (x) {
      var k = keyOf(x.r);
      if (!k) { noValue++; return; }
      var e = map[k.key];
      if (!e) { e = map[k.key] = { label: k.label, order: k.order, acc: newTm() }; }
      addTm(e.acc, x.tm, x.rug, H);
    });
    if (map.other && g.kind === 'dex') { map.other.label = 'Other (' + fmtInt(Object.keys(otherDex).length) + ' names)'; }
    return { def: g, groups: sorted(map), noValue: noValue };
  }

  // ---- ECharts plumbing ----

  function cssVar(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }
  function chartsReady() { return !!(window.echarts && typeof window.echarts.init === 'function'); }
  function disposeCharts() {
    Object.keys(charts).forEach(function (id) { charts[id].dispose(); });
    charts = {};
  }
  // The registered theme of the colour scheme now in use, made from the page's
  // CSS variables (light or dark) the first time it is needed.
  function themeName() {
    var scheme = darkMQ && darkMQ.matches ? 'dark' : 'light';
    var name = 'scout-' + scheme;
    if (!themes[name]) {
      var axis = {
        axisLine: { lineStyle: { color: cssVar('--border-input') } },
        axisTick: { lineStyle: { color: cssVar('--border-input') } },
        axisLabel: { color: cssVar('--muted') },
        splitLine: { lineStyle: { color: cssVar('--border') } }
      };
      window.echarts.registerTheme(name, {
        color: ['--c0', '--c1', '--c2', '--c3', '--c4', '--c5', '--c6', '--c7', '--c8', '--c9'].map(cssVar),
        backgroundColor: 'transparent',
        textStyle: { color: cssVar('--muted'), fontFamily: getComputedStyle(document.body).fontFamily, fontSize: 11 },
        categoryAxis: axis, valueAxis: axis,
        tooltip: { backgroundColor: cssVar('--surface'), borderColor: cssVar('--border'), textStyle: { color: cssVar('--text') } }
      });
      themes[name] = true;
    }
    if (chartScheme !== scheme) { disposeCharts(); chartScheme = scheme; }
    return name;
  }
  // The chart of a box (made on first use, in the theme of the scheme now in use).
  function chartIn(box) {
    var name = themeName();
    var c = charts[box.id];
    if (c && c.getDom() !== box) { c.dispose(); c = null; }
    if (!c) {
      c = window.echarts.init(box, name, { renderer: 'canvas' });
      charts[box.id] = c;
    }
    return c;
  }
  // empties a box with no chart in it: a message left by chartMsg or naChart
  function clearMsg(box) {
    if (charts[box.id]) { return; }
    box.replaceChildren();
    box.removeAttribute('aria-label');
  }
  function draw(box, option) {
    clearMsg(box);
    var c = chartIn(box);
    c.setOption(option, { notMerge: true });
    c.resize();
  }
  function animate() { return !(calmMQ && calmMQ.matches); }

  // A tooltip: a heading and rows of [swatch class, text], as elements with
  // textContent only.
  function tipBox(head, rows) {
    var box = el('div', 'ech-tip');
    box.appendChild(el('div', 'ech-tip-h', head));
    rows.forEach(function (row) {
      var line = el('div', 'ech-tip-row');
      if (row[0]) { line.appendChild(el('span', 'sw ' + row[0])); }
      line.appendChild(el('span', null, row[1]));
      box.appendChild(line);
    });
    return box;
  }
  function tooltipBase(formatter, trigger) {
    return {
      trigger: trigger || 'axis', confine: true, renderMode: 'html', formatter: formatter,
      axisPointer: { type: trigger === 'item' ? 'none' : 'shadow' }
    };
  }
  // the time axis (log of minutes), with ticks at the band edges up to H
  function timeXAxis(H) {
    var ticks = TM_TICKS.filter(function (s) { return s <= H; });
    if (ticks[ticks.length - 1] !== H) { ticks.push(H); }
    var at = ticks.map(A.timeAxis);
    return {
      type: 'value', min: 0, max: A.timeAxis(H),
      axisLabel: { customValues: at, hideOverlap: true, formatter: function (x) { var s = A.timeAxisInv(x); return s < 1 ? '0' : A.fmtDur(Math.round(s)); } },
      axisTick: { customValues: at }, splitLine: { show: true }
    };
  }
  function pctAxis(max) {
    return { type: 'value', min: 0, max: max, axisLabel: { formatter: '{value}%' } };
  }
  function chartMsg(box, text) {
    var c = charts[box.id];
    if (c) { c.dispose(); delete charts[box.id]; }
    box.replaceChildren(el('p', 'muted small ech-msg', text));
    box.setAttribute('aria-label', text);
  }

  // A window too short for the "reached 2×" rule (!A.can2xReach): the
  // reached-2× figures show A.na2x() instead of numbers (owner decision, 12
  // October 2026). A table cell with it:
  function naCell() {
    var x = A.na2x();
    var td = el('td', 'num muted');
    td.appendChild(el('span', 'ana-rate', x.short));
    td.appendChild(el('span', 'ana-n', x.needs));
    td.title = x.why;
    return td;
  }
  // and a chart box with it instead of a chart (also when the charts did not load).
  function naChart(box) {
    var x = A.na2x();
    chartMsg(box, x.text);
    box.firstChild.title = x.why;
    box.setAttribute('aria-label', x.text + '. ' + x.why);
  }

  // ---- the parts of the tab ----

  // The "reached 2×" rule (A.reached2x) in words, for the captions.
  function rule2x() {
    var hold = A.holdWords(A.MIN_HELD_2X_S);
    return 'Reached 2× = at or above 2× on closes for at least ' + hold + ' in total within the window ' +
      '(in every window; a call still at 2× at the window\'s end needs it too).';
  }

  // na: the window is too short for the rule (!A.can2xReach)
  function tmCounts(all, calls, withData, notTimed, hz, na) {
    var hold = A.holdWords(A.MIN_HELD_2X_S);
    var x = A.na2x();
    var dl = $('tm-counts');
    dl.replaceChildren();
    stat(dl, 'Calls', fmtInt(calls));
    stat(dl, 'With data (' + hz + ')', fmtInt(withData), 'Calls with a return for the window');
    stat(dl, 'Timed', fmtInt(all.n), 'With data and their timing computed: the n of this tab');
    stat(dl, 'Timing not computed', fmtInt(notTimed), 'With data but no timing yet: left out');
    if (na) {
      stat(dl, 'Reached 2×', x.text, x.why + ' ' + rule2x());
      stat(dl, 'Still at 2× at the end', x.text, x.why);
    } else {
      stat(dl, 'Reached 2×', all.n ? fmtInt(all.reached) + ' (' + (all.reached / all.n * 100).toFixed(1) + '%)' : DASH,
        fmtInt(all.reached) + ' of ' + fmtInt(all.n) + ' timed calls reached 2× the entry within the window. ' + rule2x());
      stat(dl, 'Still at 2× at the end', fmtInt(all.cens), 'Reached 2× and had not closed under it when the window ended (censored)');
    }
    stat(dl, 'Touched 2×, under ' + hold, fmtInt(all.short),
      'Touched 2× but held it less than ' + hold + ' in total (also those still at 2× at the window\'s end): counted as not reaching 2×; ' +
      'their real time held is in the time held at 2× figures');
    stat(dl, 'Rugged in the window', fmtInt(all.rugIn.length), 'Event rug times within the window (at-the-call and detected rugs are counted apart, below)');
    $('tm-counts-note').textContent = 'n timed = ' + fmtInt(all.n) + ': first calls priced in USD with a ' + hz + ' return whose timing is computed. ' +
      (notTimed ? fmtInt(notTimed) + ' calls with a ' + hz + ' return have no timing yet (tracked before the timing existed, or the candles did not match the stored peak) and are left out. ' : '') +
      'Repeat calls are never counted (first calls only). ' +
      rule2x() + ' Held is measured on closes, within the window. ' +
      (na ? x.why + ' So the reached-2× figures of the ' + hz + ' window show "' + x.text + '" instead of a misleading 0. ' : '') +
      fmtInt(all.short) + ' calls touched 2× but did not reach it by this rule: ' +
      'they count as not reaching 2×, like a call that never touched it, and stay in every total; ' +
      'the time held at 2× figures count the time they really held.';
  }

  // The five distributions: one small bar chart each (shares of its n).
  var TM_DIST = [
    { key: 'peak', title: 'Time to peak' },
    { key: 't2x', title: 'Time to first 2×' },
    { key: 'held', title: 'Time held at 2× (on closes)' },
    { key: 'fall', title: 'Time to fall back below 2×' },
    { key: 'rug', title: 'Time to rug (event rug times in the window)' }
  ];
  function distCards() {
    var box = $('tm-dist');
    if (box.childElementCount === TM_DIST.length) { return; }
    disposeCharts();
    box.replaceChildren();
    TM_DIST.forEach(function (d, i) {
      var card = el('div', 'ana-multiple');
      card.appendChild(el('h4', null, d.title));
      var c = el('div', 'ech');
      c.id = 'ch-tm-d' + i;
      c.setAttribute('role', 'img');
      card.appendChild(c);
      // the time held at 2× has three kinds of bars: its key under the chart
      // (after it, so the chart stays card.children[1])
      if (d.key === 'held') {
        var key = el('ul', 'ana-key');
        key.id = 'key-tm-held';
        card.appendChild(key);
      }
      box.appendChild(card);
    });
  }

  // na: the window is too short for the rule (!A.can2xReach): the time to
  // first 2× and the time to fall back below 2× show A.na2x()
  function drawTmDist(all, hz, na) {
    distCards();
    var cards = $('tm-dist').children;
    var bandLabels = A.TIME_BANDS.map(function (b) { return b.label; });
    var hold = A.holdWords(A.MIN_HELD_2X_S);
    var narrow = $('tm-dist').clientWidth < 520;
    TM_DIST.forEach(function (d, i) {
      var cats = bandLabels.slice();
      var series;
      var n;
      if (d.key === 'held') {
        // the real time held of every timed call (owner decision, 13 October
        // 2026), from A.heldHistogram: grey = 0 held (never touched 2×, or
        // touched it without a close at or above it); second tone = touched
        // 2× and held more than 0 but under A.MIN_HELD_2X_S (did not reach
        // 2×; only in the bands under 1 hour); accent = reached 2× (at least
        // A.MIN_HELD_2X_S held, so 1–4h or above). All three come from the
        // same held values as n, so a call with held missing is in none.
        var hh = A.heldHistogram(all.helds, all.heldsShort, all.heldsReached);
        cats = hh.labels;
        n = all.helds.length;
        series = [
          { name: '0 held', counts: hh.zero, sw: 'sw-never', color: cssVar('--muted') },
          { name: 'Touched 2×, held over 0 but under ' + hold, counts: hh.short, sw: 'c1', color: cssVar('--c1') },
          { name: 'Reached 2× (' + hold + ' or more)', counts: hh.reached, sw: 'sw-acc', color: cssVar('--accent') }
        ];
        var hkey = $('key-tm-held');
        hkey.replaceChildren();
        series.forEach(function (s) {
          var k = 0;
          s.counts.forEach(function (v) { k += v; });
          keyItem(hkey, s.sw, s.name + ' (' + fmtInt(k) + ')');
        });
      } else {
        var list = d.key === 'peak' ? all.peaks : d.key === 't2x' ? all.t2xs : d.key === 'fall' ? all.falls : all.rugIn;
        n = list.length;
        series = [{ name: d.title, counts: A.timeHistogram(list), sw: 'sw-acc', color: cssVar('--accent') }];
      }
      var card = cards[i];
      var box = card.children[1];
      if (na && (d.key === 't2x' || d.key === 'fall')) {
        card.className = 'ana-multiple';
        card.firstChild.textContent = d.title;
        naChart(box);
        return;
      }
      var few = n < MIN_N;
      card.className = 'ana-multiple' + (few ? ' is-few' : '');
      card.firstChild.textContent = d.title + ' · n ' + fmtInt(n);
      if (!chartsReady()) { clearMsg(box); return; }
      var maxShare = 0;
      cats.forEach(function (c, j) {
        var tot = 0;
        series.forEach(function (s) { tot += s.counts[j]; });
        if (n && tot / n > maxShare) { maxShare = tot / n; }
      });
      var yMax = Math.min(100, Math.max(10, Math.ceil(maxShare * 100 / 10) * 10));
      var title = d.title + ' (' + hz + ')';
      draw(box, {
        animation: animate(),
        aria: { enabled: true, label: { description: title + ', share of ' + fmtInt(n) + ' calls per band: ' + cats.map(function (c, j) {
          var k = 0;
          series.forEach(function (s) { k += s.counts[j]; });
          return c + ' ' + (n ? (k / n * 100).toFixed(1) : '0') + '%';
        }).join(', ') } },
        grid: { left: 36, right: 6, top: 8, bottom: narrow ? 42 : 30 },
        xAxis: { type: 'category', data: cats, axisLabel: { interval: 0, rotate: narrow ? 40 : 30, fontSize: 10 } },
        yAxis: pctAxis(yMax),
        tooltip: tooltipBase(function (params) {
          var p = Array.isArray(params) ? params : [params];
          var j = p.length ? p[0].dataIndex : 0;
          return tipBox(d.title + ', ' + cats[j], series.map(function (s) {
            var k = s.counts[j];
            return [s.sw, (series.length > 1 ? s.name + ': ' : '') + (n ? (k / n * 100).toFixed(1) : '0.0') + '% (' + fmtInt(k) + ' of ' + fmtInt(n) + ')'];
          }));
        }),
        series: series.map(function (s) {
          return {
            type: 'bar', name: s.name, stack: 'all', barCategoryGap: '20%',
            itemStyle: { color: few ? cssVar('--muted') : s.color, opacity: s.sw === 'sw-never' ? 0.55 : 0.8 },
            emphasis: { itemStyle: { opacity: 1 } },
            data: s.counts.map(function (k) { return n ? Math.round(k / n * 1000) / 10 : 0; })
          };
        })
      });
    });
    $('h-tm-dist').textContent = 'How long it took (' + hz + ' window)';
    $('cap-tm-dist').textContent = 'Bands: < 5m, 5–15m, 15–60m, 1–4h, 4–24h, 1–3d, 3–7d, 7–30d after the post; a time on a boundary goes to the band above it. ' +
      'Each bar is a share of the n of its chart. ' + rule2x() + ' ' +
      'Time to peak: every timed call. ' +
      (na ? 'Time to first 2× and time to fall back below 2×: "' + A.na2x().text + '". ' + A.na2x().why + ' '
        : 'Time to first 2×: the calls that reached 2× (the time of their first touch of 2×). ') +
      'Time held at 2×: every timed call, at the time it really held on closes: blue = reached 2× (' + fmtInt(all.heldsReached.length) + ' calls, ' +
      hold + ' or more); amber = touched 2×, held over 0 but under ' + hold + ' (' +
      fmtInt(all.heldsShort.filter(function (v) { return v > 0; }).length) + ' calls; they did not reach 2×); grey = 0 held (' +
      fmtInt(all.helds.filter(function (v) { return v === 0; }).length) + ' calls: never touched 2×, or touched it without a close at or above it). ' +
      (na ? '' : 'Time to fall back below 2×: the calls that reached 2× and fell back within the window (' + fmtInt(all.cens) + ' still at 2× at the end are left out). ') +
      'Time to rug: the event rug times within the window; rugs at the call and rugs only detected at the end of tracking are in the table below, not here. Grey: n under ' + MIN_N + '.';
  }

  function fillTmRugs(all, hz) {
    var med = function (list) { return A.median(list.filter(isNum)); };
    var rows = [
      ['Event, within the window', all.rugIn, 'Exact (rug block). In the histogram and the "share rugged by t" curve.'],
      ['Event, after the window', all.rugLater, 'Exact, but later than ' + hz + ': not in this window\'s charts.'],
      ['At the call (at or before the post)', all.atCall, 'Drained at or before the post: no time after the call. Never in the charts.'],
      ['Detected at the end of tracking', all.detected, 'Found only by the end-of-tracking check; the time is that check\'s (approximate: an upper bound). Never in the charts.']
    ];
    var frag = document.createDocumentFragment();
    frag.appendChild(headRow('Kind of rug time', [
      ['Calls', 'Timed calls with this kind of rug time'], ['Of timed', 'Share of the timed calls'],
      ['Median time', 'Median rug time after the post (negative: before the post)'], ['In the charts', '']
    ]));
    var tbody = el('tbody');
    function row(label, k, medTxt, note) {
      var tr = el('tr');
      var th = el('th', 'tok', label);
      th.scope = 'row';
      tr.appendChild(th);
      tr.appendChild(el('td', 'num', fmtInt(k)));
      tr.appendChild(rateCell(k, all.n, 'timed calls'));
      tr.appendChild(el('td', 'num', medTxt));
      tr.appendChild(el('td', null, note));
      tbody.appendChild(tr);
    }
    rows.forEach(function (x) {
      var m = med(x[1]);
      var txt = isNum(m) ? (x[1] === all.detected ? '≈ ' : '') + A.fmtDur(m) : DASH;
      row(x[0], x[1].length, txt, x[2]);
    });
    if (all.other) { row('Other kind', all.other, DASH, 'A kind of rug time this page does not know. Never in the charts.'); }
    row('Rugged, no rug time stored', all.noTime, DASH, 'Flagged rugged by the tracker (as of now) without a rug block time. Never in the charts.');
    frag.appendChild(tbody);
    $('tbl-tm-rugs').replaceChildren(frag);
    $('cap-tm-rugs').textContent = 'Over the ' + fmtInt(all.n) + ' timed calls. Only event rug times are exact; the three other kinds are kept apart, as the owner decided.';
  }

  var TM_HEADS = [
    ['Timed', 'Calls with a return for the window and their timing computed: the n of the row'],
    ['Reached 2×', 'Share that reached 2× the entry within the window. ' + rule2x()],
    ['Time to peak', 'Median, the mean under it, n'],
    ['First 2×', 'Time to the first 2×: median and mean over the calls that reached 2×'],
    ['Held at 2×', 'Time at or above 2× on closes: median and mean over every timed call, at the time it really held (also under ' + A.holdWords(A.MIN_HELD_2X_S) + '; never touched 2× = 0)'],
    ['Fell below 2×', 'Time to fall back below 2×, from the post: median and mean over the calls that reached 2× and fell back'],
    ['Still at 2× at end', 'Of the calls that reached 2×: still at 2× when the window ended (censored)'],
    ['Half fell back by', 'Kaplan–Meier median of "Still at 2× after t" (calls that reached 2×): the t where the curve first drops to 50% or below (those still at 2× at the end count until then, not as fallen back)'],
    ['Rugged in window', 'Event rug times within the window'],
    ['Time to rug', 'Median and mean of the event rug times within the window']
  ];

  // a cell with the median duration, the mean under it and n
  function durCell(list, what) {
    var td = el('td', 'num');
    if (!list.length) { td.textContent = DASH; return td; }
    var md = A.median(list), mn = A.mean(list);
    td.appendChild(el('span', 'ana-rate', A.fmtDur(md)));
    td.appendChild(el('span', 'ana-n', 'mean ' + A.fmtDur(mn)));
    td.appendChild(el('span', 'ana-n', 'n ' + fmtInt(list.length)));
    td.title = what + ': median ' + Math.round(md).toLocaleString('en-US') + ' s, mean ' + Math.round(mn).toLocaleString('en-US') + ' s over ' + fmtInt(list.length) + ' calls';
    if (list.length < MIN_N) { td.classList.add('ana-few'); }
    return td;
  }

  // na: the window is too short for the rule (!A.can2xReach): the columns
  // that hang on "reached 2×" show A.na2x(); Timed, Time to peak, Held at 2×
  // (every timed call) and the rug columns stay
  function tmRow(label, a, na) {
    var tr = el('tr');
    var th = el('th', 'tok', label);
    th.scope = 'row';
    tr.appendChild(th);
    if (a.n < MIN_N) {
      tr.className = 'ana-few';
      th.title = 'Fewer than ' + MIN_N + ' timed calls';
    }
    tr.appendChild(el('td', 'num', fmtInt(a.n)));
    tr.appendChild(na ? naCell() : rateCell(a.reached, a.n, 'timed calls reached 2×'));
    tr.appendChild(durCell(a.peaks, 'Time to peak'));
    tr.appendChild(na ? naCell() : durCell(a.t2xs, 'Time to first 2×'));
    tr.appendChild(durCell(a.helds, 'Time held at 2× (never touched 2× = 0)'));
    tr.appendChild(na ? naCell() : durCell(a.falls, 'Time to fall back below 2×'));
    tr.appendChild(na ? naCell() : rateCell(a.cens, a.reached, 'calls that reached 2× were still at 2× at the end'));
    if (na) {
      tr.appendChild(naCell());
    } else {
      var km = A.kaplanMeier(a.holds);
      var kmTd = el('td', 'num', km.n ? (isNum(km.median) ? A.fmtDur(km.median) : 'not reached') : DASH);
      kmTd.title = km.n ? (isNum(km.median) ? 'Half of the ' + fmtInt(km.n) + ' calls that reached 2× had fallen back ' + A.fmtDur(km.median) + ' after the first 2×'
        : 'Fewer than half of the ' + fmtInt(km.n) + ' calls that reached 2× fell back within the window') : 'No call reached 2×';
      tr.appendChild(kmTd);
    }
    tr.appendChild(rateCell(a.rugIn.length, a.n, 'timed calls rugged within the window (event rug times)'));
    tr.appendChild(durCell(a.rugIn, 'Time to rug'));
    return tr;
  }

  function fillTmGroups(all, gr, hz, na) {
    var g = gr.def;
    $('tm-k-box').hidden = g.kind === 'cat' || g.kind === 'period' || g.kind === 'family' || g.kind === 'dex';
    var frag = document.createDocumentFragment();
    frag.appendChild(headRow(g.kind === 'period' ? (state.period === 'month' ? 'Month' : 'Week') : g.label, TM_HEADS));
    var tbody = el('tbody');
    tbody.appendChild(tmRow('All timed calls', all, na));
    gr.groups.forEach(function (x) { tbody.appendChild(tmRow(x.label, x.acc, na)); });
    frag.appendChild(tbody);
    $('tbl-tm-group').replaceChildren(frag);
    $('t-tm-group').textContent = 'Mean and median by group (' + hz + ')';
    $('tm-group-note').textContent = g.label + (g.kind !== 'cat' && g.kind !== 'period' && g.kind !== 'family' && g.kind !== 'dex'
      ? ', ' + gr.groups.length + ' groups of about the same number of timed calls' : '') + '. ' +
      (gr.noValue ? fmtInt(gr.noValue) + ' of ' + fmtInt(all.n) + ' timed calls have no value for it and are left out of the groups (not of "All").' : '');
    $('cap-tm-group').textContent = 'Each cell: the median, the mean under it, and n. ' + rule2x() + ' ' +
      'Held at 2× is the time each timed call really held: also the time held by the calls that touched 2× without reaching it (under ' +
      A.holdWords(A.MIN_HELD_2X_S) + '); the calls that never touched 2× count as 0 (so its median is often 0). ' +
      (na ? 'Reached 2×, First 2×, Fell below 2×, Still at 2× at end and Half fell back by: "' + A.na2x().text + '". ' + A.na2x().why + ' '
        : 'Fell below 2× is counted from the post and leaves out the calls still at 2× at the end; "Half fell back by" is the Kaplan–Meier median of "Still at 2× after t" below, where they count until the window ended. ') +
      'Grey: fewer than ' + MIN_N + ' calls.';
  }

  // The series of a step chart: All (when more than one group) and each group.
  function curveSeries(all, gr, pick) {
    var list = [{ name: 'All timed calls', acc: all, sw: 'sw-all', color: cssVar('--text-2'), width: 2.5 }];
    gr.groups.forEach(function (x, i) {
      list.push({ name: x.label, acc: x.acc, sw: 'c' + (i % 10), color: cssVar('--c' + (i % 10)), width: 1.8 });
    });
    return list.map(function (s) { var c = pick(s.acc); s.curve = c.points; s.n = c.n; s.end = c.end; s.label = c.label; return s; });
  }

  // yMax: the top of the share axis (percent)
  function drawStep(boxId, keyId, H, series, unit, aria, yMax) {
    var key = $(keyId);
    key.replaceChildren();
    series.forEach(function (s) { keyItem(key, 'sw-line ' + s.sw, s.name + ': ' + s.label); });
    var box = $(boxId);
    if (!chartsReady()) { clearMsg(box); return; }
    if (!series.some(function (s) { return s.n > 0; })) { chartMsg(box, 'No calls for this curve.'); return; }
    var tip = tooltipBase(function (params) {
      // the value at the pointer, not at the nearest point of each line
      var p = Array.isArray(params) ? params : [params];
      var t = A.timeAxisInv(p.length ? p[0].axisValue : 0);
      return tipBox(A.fmtDur(t) + ' after ' + unit, series.map(function (s) {
        var v = s.n && t <= s.end + 1e-6 ? A.stepAt(s.curve, t) : NaN;
        return [s.sw, s.name + ': ' + (isNum(v) ? (v * 100).toFixed(1) + '%' : DASH) + ' (n ' + fmtInt(s.n) + ')'];
      }));
    });
    tip.axisPointer = { type: 'line', snap: false };
    draw(box, {
      animation: animate(),
      aria: { enabled: true, label: { description: aria } },
      grid: { left: 40, right: 14, top: 10, bottom: 26 },
      xAxis: timeXAxis(H),
      yAxis: pctAxis(yMax || 100),
      tooltip: tip,
      series: series.filter(function (s) { return s.n > 0; }).map(function (s) {
        var pts = s.curve.map(function (p) { return [A.timeAxis(p.t), Math.round(p.s * 1000) / 10]; });
        var last = s.curve[s.curve.length - 1];
        pts.push([A.timeAxis(Math.max(s.end, last.t)), Math.round(last.s * 1000) / 10]);
        return {
          type: 'line', name: s.name, step: 'end', showSymbol: false, data: pts,
          lineStyle: { color: s.color, width: s.width, type: s.n < MIN_N ? 'dashed' : 'solid' }, itemStyle: { color: s.color },
          emphasis: { disabled: true }
        };
      })
    });
  }
  // na: the window is too short for the rule (!A.can2xReach): no curve, A.na2x()
  function drawTmKM(all, gr, H, hz, na) {
    $('h-tm-km').textContent = 'Still at 2× after t (from the first 2×, ' + hz + ' window)';
    if (na) {
      var x = A.na2x();
      $('key-tm-km').replaceChildren();
      naChart($('ch-tm-km'));
      $('cap-tm-km').textContent = 'The share of the calls that reached 2× still at 2× t after first reaching it (Kaplan–Meier), and "half fell back by": "' + x.text + '". ' +
        x.why + ' ' + rule2x();
      return;
    }
    var series = curveSeries(all, gr, function (a) {
      var km = A.kaplanMeier(a.holds);
      return { points: km.points, n: km.n, end: km.end,
        label: 'n ' + fmtInt(km.n) + ', ' + fmtInt(km.censored) + ' still at 2× at the end' + (isNum(km.median) ? ', half fell back by ' + A.fmtDur(km.median) : '') };
    });
    var aKM = A.kaplanMeier(all.holds);
    drawStep('ch-tm-km', 'key-tm-km', H, series, 'the first 2×',
      'Share still at 2× after the first 2×, Kaplan–Meier, ' + fmtInt(aKM.n) + ' calls that reached 2× in the ' + hz + ' window' +
      (isNum(aKM.median) ? '; half had fallen back by ' + A.fmtDur(aKM.median) : '; fewer than half fell back'));
    $('cap-tm-km').textContent = 'Of the ' + fmtInt(aKM.n) + ' timed calls that reached 2×, the share still at or above 2× t after first reaching it (Kaplan–Meier). ' +
      rule2x() + ' The other ' + fmtInt(all.n - all.reached) + ' timed calls are not in it (' + fmtInt(all.short) +
      ' of them touched 2× but held it under ' + A.holdWords(A.MIN_HELD_2X_S) + '). ' +
      't is the time held at 2× on closes, but at most the time from the first 2× (the start of its candle) to the first close under 2× or the rug, ' +
      'so a call that fell back and rose again counts only its first stretch and can fall back before ' +
      A.holdWords(A.MIN_HELD_2X_S) + '. The ' + fmtInt(aKM.censored) + ' calls still at 2× when the window ended are censored: counted until then, not as fallen back. ' +
      '"Half fell back by" is the Kaplan–Meier median: the t where the curve first drops to 50% or below. ' +
      'Across: log scale of time. Dashed: fewer than ' + MIN_N + ' calls.';
  }

  function drawTmRug(all, gr, H, hz) {
    var series = curveSeries(all, gr, function (a) {
      var pts = A.cumulativeShare(a.rugIn, a.n);
      return { points: pts, n: a.n, end: H, label: 'n ' + fmtInt(a.n) + ', ' + fmtInt(a.rugIn.length) + ' rugged (' + (a.n ? (a.rugIn.length / a.n * 100).toFixed(1) : '0.0') + '%)' };
    });
    drawStep('ch-tm-rug', 'key-tm-rug', H, series, 'the post',
      'Share of the ' + fmtInt(all.n) + ' timed calls rugged within t of the post (event rug times), ' + hz + ' window: ' +
      (all.n ? (all.rugIn.length / all.n * 100).toFixed(1) : '0') + '% by the end',
      // the axis up to the highest curve (rugs are a small share): steps of 10%
      Math.min(100, Math.max(10, Math.ceil(Math.max.apply(null, series.map(function (s) {
        return s.n ? s.curve[s.curve.length - 1].s * 100 : 0;
      })) / 10) * 10)));
    $('h-tm-rug').textContent = 'Share rugged by t (from the post, ' + hz + ' window)';
    $('cap-tm-rug').textContent = 'Of the timed calls, the share whose pool was drained within t of the post (event rug times only, exact to the block). ' +
      fmtInt(all.atCall.length) + ' rugged at the call and ' + fmtInt(all.detected.length) + ' only detected at the end of tracking are not in this curve (see the table above). ' +
      'Every timed call was followed for the whole window, so nothing is censored here. Across: log scale of time.';
  }

  function renderTiming(rows, hIdx) {
    var hz = state.horizon, H = data.horizon_seconds[hIdx];
    // too short for any call to reach 2× by the rule: n/a, not a misleading 0
    var na = !A.can2xReach(H);
    var iRet = col['ret_' + hz];
    var all = newTm();
    var withData = 0, notTimed = 0;
    var timed = [];
    rows.forEach(function (r) {
      if (classify(r, hIdx, iRet) !== 'data') { return; }
      withData++;
      var tm = timingOf(r, hIdx, hz);
      if (!tm) { notTimed++; return; }
      var rug = rugOf(r);
      timed.push({ r: r, tm: tm, rug: rug });
      addTm(all, tm, rug, H);
    });
    var err = $('tm-chart-error');
    err.hidden = chartsReady();
    if (!chartsReady()) { err.textContent = 'The charts could not be loaded (the chart library did not load). The counts and tables still show every number.'; }
    tmCounts(all, rows.length, withData, notTimed, hz, na);
    drawTmDist(all, hz, na);
    fillTmRugs(all, hz);
    var gr = tmGroups(timed, H);
    fillTmGroups(all, gr, hz, na);
    drawTmKM(all, gr, H, hz, na);
    drawTmRug(all, gr, H, hz);
  }

  // ---- render -----------------------------------------------------------------

  function render() {
    if (!data) { return; }
    var t0 = performance.now();
    var hIdx = data.horizons.indexOf(state.horizon);
    if (hIdx < 0) { hIdx = 1; state.horizon = data.horizons[hIdx]; }
    $('ana-body').hidden = false;
    $('lg-2x-hold').textContent = A.holdWords(A.MIN_HELD_2X_S);
    // the two group-count pickers (By factor, Timing) share one choice
    ['ana-k', 'tm-k'].forEach(function (id) {
      Array.prototype.forEach.call($(id).querySelectorAll('button[data-k]'), function (b) {
        b.setAttribute('aria-pressed', b.getAttribute('data-k') === state.k ? 'true' : 'false');
      });
    });
    // filters: family and verdict for everything; quiet inside each view
    var base = [], quietOnly = [];
    data.rows.forEach(function (r) {
      if (passQuiet(r)) { quietOnly.push(r); }
      if (passVerdict(r) && passFamily(r)) { base.push(r); }
    });
    var shown;
    if (state.tab === 'overview') {
      shown = renderOverview(base, hIdx);
      drawPL(quietOnly, hIdx);
    } else {
      var rows = base.filter(passQuiet);
      shown = rows.length;
      if (state.tab === 'factor') {
        renderFactor(rows, hIdx);
      } else if (state.tab === 'timing') {
        renderTiming(rows, hIdx);
      } else {
        renderPeak(rows);
      }
    }
    $('ana-msg').hidden = shown > 0;
    if (shown === 0) { $('ana-msg').textContent = 'No calls match this choice.'; }
    lastDraw = Date.now();
    // for measuring (headless browser checks): the time of the last drawing
    $('ana-body').setAttribute('data-render-ms', String(Math.round(performance.now() - t0)));
  }

  // ---- loading ----------------------------------------------------------------

  function setError(msg) {
    var e = $('ana-error');
    e.textContent = msg;
    e.hidden = !msg;
  }

  function fillSelect(id, keys, labels, key) {
    var sel = $(id);
    var frag = document.createDocumentFragment();
    var opt = el('option', null, 'All');
    opt.value = 'all';
    frag.appendChild(opt);
    keys.forEach(function (k) {
      var o = el('option', null, labels[k] || k);
      o.value = k;
      frag.appendChild(o);
    });
    sel.replaceChildren(frag);
    if (state[key] !== 'all' && keys.indexOf(state[key]) < 0) { state[key] = 'all'; }
    sel.value = state[key];
  }

  function load() {
    if (loading) { return; }
    loading = true;
    lastLoad = Date.now();
    var headers = { 'Accept': 'application/json' };
    if (etag && data) { headers['If-None-Match'] = etag; }
    fetch('api/analytics', { headers: headers, cache: 'no-store' }).then(function (r) {
      var at = Date.parse(r.headers.get('X-Snapshot-At') || '');
      if (r.status === 304) {
        return r.text().then(function () { return { body: null, at: at }; });
      }
      if (!r.ok) { throw new Error('HTTP ' + r.status); }
      var tag = r.headers.get('ETag') || '';
      return r.json().then(function (body) { return { body: body, at: at, etag: tag }; });
    }).then(function (res) {
      var changed = false;
      if (res.body) {
        var b = res.body;
        if (!b || b.format !== FORMAT || !Array.isArray(b.rows) || !Array.isArray(b.columns)) {
          throw new Error('the data has an unexpected format; reload the page');
        }
        var c = {};
        b.columns.forEach(function (name, i) { c[name] = i; });
        data = b;
        col = c;
        etag = res.etag;
        changed = true;
        fillSelect('ana-family', data.families, FAMILY_LABEL, 'family');
        fillSelect('ana-verdict', data.verdicts, VERDICT_LABEL, 'verdict');
      }
      if (isNum(res.at)) { snapshotMs = res.at; }
      setError('');
      $('ana-updated').textContent = isNum(snapshotMs) ? 'Data as of ' + fmtTime(new Date(snapshotMs)) : '';
      // on a 304 the charts stay (no flicker under the pointer); only "not
      // due yet" moves, slowly
      if (changed || Date.now() - lastDraw >= REDRAW_MS) { render(); }
    }).catch(function (err) {
      var msg = 'Could not load the data: ' + (err && err.message ? err.message : 'network error') + '.';
      if (data && isNum(snapshotMs)) {
        msg += ' The tables and charts still show the data read at ' + fmtTime(new Date(snapshotMs)) + '.';
      } else {
        $('ana-msg').hidden = true;
      }
      setError(msg);
    }).then(function () {
      loading = false;
      schedule();
    });
  }

  function schedule() {
    if (timer) { clearTimeout(timer); }
    timer = setTimeout(function () {
      timer = null;
      if (document.hidden) { return; } // picked up again by visibilitychange
      load();
    }, REFRESH_MS);
  }

  document.addEventListener('visibilitychange', function () {
    if (!document.hidden && Date.now() - lastLoad >= REFRESH_MS) { load(); }
  });

  // one listener per segmented control
  function segmented(id, attr, key) {
    var box = $(id);
    box.addEventListener('click', function (ev) {
      var b = ev.target.closest('button[' + attr + ']');
      if (!b || !box.contains(b)) { return; }
      state[key] = b.getAttribute(attr);
      Array.prototype.forEach.call(box.querySelectorAll('button[' + attr + ']'), function (x) {
        x.setAttribute('aria-pressed', x === b ? 'true' : 'false');
      });
      render();
    });
  }
  segmented('ana-horizon', 'data-horizon', 'horizon');
  segmented('ana-metric', 'data-metric', 'metric');
  segmented('ana-stat', 'data-stat', 'stat');
  segmented('ana-quiet', 'data-quiet', 'quiet');
  segmented('ana-period', 'data-period', 'period');
  segmented('ana-k', 'data-k', 'k');
  segmented('tm-k', 'data-k', 'k');
  segmented('ana-path', 'data-path', 'path');
  function selectControl(id, key) {
    $(id).addEventListener('change', function (ev) { state[key] = ev.target.value; render(); });
  }
  selectControl('ana-family', 'family');
  selectControl('ana-verdict', 'verdict');
  selectControl('ana-factor', 'factor');
  selectControl('tm-group', 'tmGroup');
  (function () {
    var sel = $('ana-factor');
    FACTORS.forEach(function (f) {
      var o = el('option', null, f.label);
      o.value = f.key;
      sel.appendChild(o);
    });
    sel.value = state.factor;
    var tg = $('tm-group');
    TM_GROUPS.forEach(function (g) {
      var o = el('option', null, g.label);
      o.value = g.key;
      tg.appendChild(o);
    });
    tg.value = state.tmGroup;
  }());
  // the charts follow the colour scheme (their theme is made from the CSS
  // variables of the scheme in use): drawn again when it changes
  if (darkMQ && darkMQ.addEventListener) {
    darkMQ.addEventListener('change', function () { disposeCharts(); render(); });
  }
  $('ana-reload').addEventListener('click', load);
  lazyTips('ch-scatter');
  lazyTips('ch-peak');

  // tabs (WAI-ARIA tabs: arrows, Home and End move between them)
  var tabs = Array.prototype.slice.call(document.querySelectorAll('.ana-tabs [role="tab"]'));
  function setTab(name, focus) {
    state.tab = name;
    tabs.forEach(function (t) {
      var on = t.getAttribute('data-tab') === name;
      t.setAttribute('aria-selected', on ? 'true' : 'false');
      t.tabIndex = on ? 0 : -1;
      $(t.getAttribute('aria-controls')).hidden = !on;
      if (on && focus) { t.focus(); }
    });
    Array.prototype.forEach.call(document.querySelectorAll('[data-tabs]'), function (n) {
      n.hidden = n.getAttribute('data-tabs').split(' ').indexOf(name) < 0;
    });
    render();
  }
  tabs.forEach(function (t, i) {
    t.addEventListener('click', function () { setTab(t.getAttribute('data-tab'), false); });
    t.addEventListener('keydown', function (ev) {
      var j = -1;
      if (ev.key === 'ArrowRight') { j = (i + 1) % tabs.length; }
      if (ev.key === 'ArrowLeft') { j = (i + tabs.length - 1) % tabs.length; }
      if (ev.key === 'Home') { j = 0; }
      if (ev.key === 'End') { j = tabs.length - 1; }
      if (j < 0) { return; }
      ev.preventDefault();
      setTab(tabs[j].getAttribute('data-tab'), true);
    });
  });

  // draw again when the width changes (the charts are drawn to the width)
  if (window.ResizeObserver) {
    var lastW = 0, rt = null;
    new ResizeObserver(function (entries) {
      var w = Math.round(entries[0].contentRect.width);
      if (!w || Math.abs(w - lastW) < 4) { return; }
      var first = lastW === 0;
      lastW = w;
      if (first) { return; }
      if (rt) { clearTimeout(rt); }
      rt = setTimeout(function () { rt = null; render(); }, 150);
    }).observe($('ana-body'));
  }

  load();
}());
