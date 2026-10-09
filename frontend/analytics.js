// Scout analytics page: three tabs (Overview, By factor, Peak vs final) over
// one compact feed. Plain JavaScript, no dependencies; the pure calculations
// are in analytics-stats.js (window.AnaStats), loaded before this file.
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
  var FORMAT = 2;          // the body's "format" this page understands
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
    { key: 'mcap', label: 'Market cap at call (posted)', kind: 'usd' },
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
    tab: 'overview', factor: 'verdict', k: '5', path: 'all'
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

  // ---- render -----------------------------------------------------------------

  function render() {
    if (!data) { return; }
    var t0 = performance.now();
    var hIdx = data.horizons.indexOf(state.horizon);
    if (hIdx < 0) { hIdx = 1; state.horizon = data.horizons[hIdx]; }
    $('ana-body').hidden = false;
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
      if (state.tab === 'factor') { renderFactor(rows, hIdx); } else { renderPeak(rows); }
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
  segmented('ana-path', 'data-path', 'path');
  function selectControl(id, key) {
    $(id).addEventListener('change', function (ev) { state[key] = ev.target.value; render(); });
  }
  selectControl('ana-family', 'family');
  selectControl('ana-verdict', 'verdict');
  selectControl('ana-factor', 'factor');
  (function () {
    var sel = $('ana-factor');
    FACTORS.forEach(function (f) {
      var o = el('option', null, f.label);
      o.value = f.key;
      sel.appendChild(o);
    });
    sel.value = state.factor;
  }());
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
