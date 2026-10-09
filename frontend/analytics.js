// Scout analytics page, section "Call performance". Plain JavaScript, no
// dependencies.
//
// GET /api/analytics sends one compact row per first call (built once per
// snapshot on the server); this page groups and counts them itself, so every
// choice (window, quiet calls, week or month) is instant. Names in the data
// (DEX, quote asset) come from posts and contracts: nothing is ever parsed as
// HTML, every node is built with createElement and textContent.
(function () {
  'use strict';

  var DASH = '–';
  var FORMAT = 1;          // the body's "format" this page understands
  var MIN_N = 20;          // fewer calls with data than this: the group is greyed out
  var REFRESH_MS = 60000;  // ask again (cheap: 304 while nothing changed)
  var TOP_DEX = 12;        // posted DEX names shown on their own; the rest are "Other"
  var TOP_QUOTE = 8;
  var FLAG_USD = 1, FLAG_RUGGED = 2, FLAG_TRACKED = 4; // the "flags" column

  var VERDICT_LABEL = {
    clean: 'No red flags', caution: 'Caution', red_flags: 'Red flags',
    unknown: 'Unreadable verdict', none: 'Not scanned'
  };
  var FAMILY_LABEL = {
    v2: 'Uniswap v2', v3: 'Uniswap v3', v4: 'Uniswap v4', pons: 'Pons launch curve',
    gecko: 'GeckoTerminal (no on-chain pool)', untracked: 'Not tracked'
  };
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  var DAY_MS = 86400000;

  var state = { horizon: '1d', quiet: 'all', period: 'week' };
  var data = null;        // the last body read
  var col = {};           // column name → index in a row
  var etag = '';
  var snapshotMs = NaN;   // X-Snapshot-At: "now" for "not due yet"
  var loading = false;
  var timer = null;
  var lastLoad = 0;

  function $(id) { return document.getElementById(id); }

  function el(tag, className, text) {
    var n = document.createElement(tag);
    if (className) { n.className = className; }
    if (text !== undefined && text !== null) { n.textContent = String(text); }
    return n;
  }

  function isNum(v) { return typeof v === 'number' && isFinite(v); }
  function pad2(n) { return n < 10 ? '0' + n : String(n); }
  function fmtInt(n) { return isNum(n) ? n.toLocaleString('en-US') : DASH; }
  function fmtTime(d) { return pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds()); }

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

  function median(list) {
    if (list.length === 0) { return NaN; }
    var s = list.slice().sort(function (a, b) { return a - b; });
    var m = s.length >> 1;
    return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
  }

  // ---- grouping ---------------------------------------------------------

  // The accumulator of one group for the chosen window.
  function newAcc() {
    return {
      calls: 0, data: 0, notDue: 0, noData: 0, pending: 0, untracked: 0, noUsd: 0,
      rets: [], peaks: [], dds: [], wins: 0, collapses: 0, peak100: 0, rugged: 0
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
    if (r[col.flags] & FLAG_RUGGED) { acc.rugged++; }
  }

  function now() { return isNum(snapshotMs) ? snapshotMs : Date.now(); }

  function isQuiet(r) {
    var t = r[col.trades_24h];
    return !(r[col.flags] & FLAG_RUGGED) && isNum(t) && t < data.quiet_below;
  }

  // Monday 00:00 UTC of the week of t (seconds), or the 1st of its month.
  function periodStart(t) {
    var d = new Date(t * 1000);
    if (state.period === 'month') { return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1); }
    var back = (d.getUTCDay() + 6) % 7; // days since Monday
    return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate() - back);
  }
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

  // ---- rendering --------------------------------------------------------

  var METRIC_HEADS = [
    ['Calls', 'First calls in the group'],
    ['With data', 'Calls with a return for the window: the n of every number in the row'],
    ['Mean', 'Average return over the window'],
    ['Median', 'Middle return over the window'],
    ['Win rate', 'Share with a return above 0%'],
    ['≥ +100% peak', 'Share whose peak within the window was +100% or more'],
    ['Collapse', 'Share with a return of −50% or worse'],
    ['Median peak', 'Middle of the highest price within the window, from the entry'],
    ['Median drop', 'Middle of the lowest price within the window, from the entry'],
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
    var mean = NaN;
    if (a.rets.length) {
      var sum = 0;
      for (var i = 0; i < a.rets.length; i++) { sum += a.rets[i]; }
      mean = sum / a.rets.length;
    }
    tr.appendChild(pctCell(mean));
    tr.appendChild(pctCell(median(a.rets)));
    tr.appendChild(rateCell(a.wins, a.data, 'calls with data were up'));
    tr.appendChild(rateCell(a.peak100, a.peaks.length, 'calls with a peak reached +100%'));
    tr.appendChild(rateCell(a.collapses, a.data, 'calls with data lost half or more'));
    tr.appendChild(pctCell(median(a.peaks)));
    tr.appendChild(pctCell(median(a.dds)));
    tr.appendChild(rateCell(a.rugged, a.data, 'calls with data are flagged rugged'));
    return tr;
  }

  function fillTable(id, first, groups) {
    var frag = document.createDocumentFragment();
    frag.appendChild(headRow(first, METRIC_HEADS));
    var tbody = el('tbody');
    groups.forEach(function (g) { tbody.appendChild(metricRow(g.label, g.acc, g.title)); });
    if (groups.length === 0) {
      var tr = el('tr');
      var td = el('td', 'muted', 'No calls.');
      td.colSpan = METRIC_HEADS.length + 1;
      tr.appendChild(td);
      tbody.appendChild(tr);
    }
    frag.appendChild(tbody);
    $(id).replaceChildren(frag);
  }

  function fillMatrix(periods, verdictKeys) {
    var heads = verdictKeys.map(function (v) { return [VERDICT_LABEL[v] || v, 'Median return; n = calls with data']; });
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
        var td = el('td', 'num');
        if (!a || a.data === 0) {
          td.textContent = DASH;
        } else {
          var m = median(a.rets);
          var main = el('span', 'ana-rate ' + (m > 0 ? 'pos' : m < 0 ? 'neg' : 'zero'), fmtPct(m));
          td.appendChild(main);
          td.appendChild(el('span', 'ana-n', 'n ' + fmtInt(a.data)));
          td.title = (VERDICT_LABEL[v] || v) + ', ' + p.label + ': median ' + exactPct(m) + ' over ' + fmtInt(a.data) + ' calls with data' +
            (a.data < MIN_N ? ' (fewer than ' + MIN_N + ')' : '');
          if (a.data < MIN_N) { td.classList.add('ana-few'); }
        }
        tr.appendChild(td);
      });
      tbody.appendChild(tr);
    });
    frag.appendChild(tbody);
    $('tbl-matrix').replaceChildren(frag);
  }

  function stat(dl, label, value, title) {
    var d = el('div');
    d.appendChild(el('dt', null, label));
    var dd = el('dd', null, value);
    d.appendChild(dd);
    if (title) { d.title = title; }
    dl.appendChild(d);
  }

  function render() {
    if (!data) { return; }
    var h = data.horizons.indexOf(state.horizon);
    if (h < 0) { h = 1; }
    var ix = { ret: col['ret_' + state.horizon], peak: col['peak_' + state.horizon], dd: col['dd_' + state.horizon] };

    var all = {}, byVerdict = {}, byFamily = {}, byDex = {}, byQuote = {}, byPeriod = {};
    var usedVerdicts = {};
    var quiet = 0, quietKnown = 0, quietUnknown = 0, shown = 0;
    var otherDex = {}, otherQuote = {}; // the names under "Other" in this selection
    data.rows.forEach(function (r) {
      var known = isNum(r[col.trades_24h]);
      var q = isQuiet(r);
      if (known) { quietKnown++; } else { quietUnknown++; }
      if (q) { quiet++; }
      if (state.quiet === 'exclude' && q) { return; }
      if (state.quiet === 'only' && !q) { return; }
      shown++;
      var kind = classify(r, h, ix.ret);

      add(group(all, 'all', 'All first calls', 0), r, kind, ix);

      var vi = r[col.verdict];
      var vk = data.verdicts[vi] || 'none';
      usedVerdicts[vk] = true;
      add(group(byVerdict, vk, VERDICT_LABEL[vk] || vk, vi), r, kind, ix);

      var fk = data.families[r[col.family]] || 'untracked';
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
      if (!p) { p = byPeriod[ps] = { label: periodLabel(ps), acc: newAcc(), order: -ps, byVerdict: {} }; }
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
    stat(dl, 'With data (' + state.horizon + ')', fmtInt(a.data), whyNone(a));
    stat(dl, 'Not due yet', fmtInt(a.notDue), 'Called less than ' + state.horizon + ' ago');
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

    fillTable('tbl-all', 'Group', sorted(all));
    fillTable('tbl-verdict', 'Perceptor at the call', sorted(byVerdict));
    fillTable('tbl-family', 'Pool family', sorted(byFamily));
    fillTable('tbl-dex', 'DEX in the post', sorted(byDex));
    fillTable('tbl-quote', 'Quote asset', sorted(byQuote));
    var periods = sorted(byPeriod);
    var word = state.period === 'month' ? 'month' : 'week';
    $('t-period').textContent = 'By ' + word + ' (UTC' + (word === 'week' ? ', Monday to Sunday' : '') + ')';
    $('t-matrix').textContent = 'Median return by ' + word + ' and Perceptor verdict at the call';
    fillTable('tbl-period', word === 'week' ? 'Week' : 'Month', periods);
    var vkeys = data.verdicts.filter(function (v) { return usedVerdicts[v]; });
    fillMatrix(periods, vkeys);

    $('ana-body').hidden = false;
    $('ana-msg').hidden = shown > 0;
    if (shown === 0) { $('ana-msg').textContent = 'No calls match this choice.'; }
  }

  // ---- loading ----------------------------------------------------------

  function setError(msg) {
    var e = $('ana-error');
    e.textContent = msg;
    e.hidden = !msg;
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
      }
      if (isNum(res.at)) { snapshotMs = res.at; }
      setError('');
      $('ana-updated').textContent = isNum(snapshotMs) ? 'Data as of ' + fmtTime(new Date(snapshotMs)) : '';
      render();
    }).catch(function (err) {
      var msg = 'Could not load the data: ' + (err && err.message ? err.message : 'network error') + '.';
      if (data && isNum(snapshotMs)) {
        msg += ' The tables still show the data read at ' + fmtTime(new Date(snapshotMs)) + '.';
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
  segmented('ana-quiet', 'data-quiet', 'quiet');
  segmented('ana-period', 'data-period', 'period');
  $('ana-reload').addEventListener('click', load);

  load();
}());
