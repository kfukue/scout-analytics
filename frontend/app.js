// Scout calls page. Plain JavaScript, no dependencies.
//
// Token names and symbols come from arbitrary on-chain contracts, so nothing
// from the API is ever parsed as HTML: every node is built with createElement
// and textContent, and links are only set from https:// addresses.
(function () {
  'use strict';

  var DASH = '–'; // en dash for "no value"
  var PER_PAGE = 50;
  var REFRESH_MS = 30000;
  var DEBOUNCE_MS = 300;
  var HORIZONS = ['1h', '1d', '3d', '7d', '30d'];
  // The window columns, in the order of HORIZONS: the field of each row and
  // the sort value of its header.
  var WINDOW_FIELDS = ['return_1h_pct', 'return_1d_pct', 'return_3d_pct', 'return_7d_pct', 'return_30d_pct'];
  // The cells at the end of a row that "no USD price" spans: Peak %, Worst
  // drop % and the five windows. Must match the headers in index.html.
  var NO_USD_SPAN = 2 + HORIZONS.length;
  // Columns of the table (index.html), for the detail row when the header
  // cannot be counted.
  var COLUMNS = 11 + HORIZONS.length;
  // "Calls from the last …": the choices of the age filter, in days (0 = all).
  var DAY_CHOICES = [1, 7, 30];
  var STATUS_TEXT = {
    pending: 'pending', tracking: 'tracking', done: 'done',
    no_pool: 'no pool', error: 'error', gave_up: 'gave up'
  };

  // Perceptor verdict of a token → words shown and style. The words differ, so
  // colour is never the only cue.
  var VERDICT_TEXT = {
    clean: { text: 'no red flags', cls: 'pv pv-clean' },
    caution: { text: 'caution', cls: 'pv pv-caution' },
    red_flags: { text: 'red flags', cls: 'pv pv-red' }
  };
  // The Perceptor filter: the buckets, in the order the server writes them,
  // and the short words of the button that summarises the choice.
  var VERDICT_FILTERS = ['clean', 'caution', 'red_flags', 'not_scanned'];
  var VERDICT_FILTER_TEXT = { clean: 'No red flags', caution: 'Caution', red_flags: 'Red flags', not_scanned: 'Not scanned' };
  // "Perceptor today": the token's latest Perceptor re-scan, run long after the
  // call (perceptor_today_*). Never the verdict at the time of the call: its own
  // words ("today: …") and an outlined style, and the Perceptor filter and its
  // counts never use it.
  var TODAY_TEXT = {
    clean: { text: 'no red flags', cls: 'pv-today pv-today-clean' },
    caution: { text: 'caution', cls: 'pv-today pv-today-caution' },
    red_flags: { text: 'red flags', cls: 'pv-today pv-today-red' }
  };
  var TODAY_UNKNOWN = { text: 'no readable verdict', cls: 'pv-today pv-today-unknown' };

  // verdicts: the Perceptor buckets to show, in VERDICT_FILTERS order; [] = all.
  // days: only calls from the last that many days (one of DAY_CHOICES; 0 = all).
  var state = { q: '', sort: 'date', dir: 'desc', horizon: '1d', verdicts: [], days: 0, page: 1 };
  // The server's time of the data shown (X-Snapshot-At, in ms; NaN until the
  // first answer): "now" for every "… ago" on the page, so the ages are those
  // of the data as read, and move on with every refresh, also one answered
  // "not modified".
  var snapshotMs = NaN;
  // The rows shown, by call_id (for the performance block of the row detail).
  var rowData = {};
  var loadedOnce = false;
  var searchTimer = null;
  // The request for the list that is on its way (null = none). A new request
  // cancels it, and only the answer to the newest request is ever shown, so an
  // answer to an older question never replaces a newer one.
  var callsAbort = null;
  var requestSeq = 0;
  // What the table shows: the address it was loaded from, the ETag of that
  // answer and a signature of its rows. A refresh that comes back "not
  // modified" (304), or with the same rows, leaves the table as it is.
  var shown = { url: '', etag: '', sig: '', data: null };
  var callsErrorShown = false;
  var summaryEtag = '';
  var refreshTimer = null;
  var lastRefresh = 0;
  var refreshing = false; // "Refresh now" is waiting for the server

  // Row detail (the token's Perceptor and sAlpha reports). Open rows are kept
  // by call_id, so they stay open across the 30-second refresh, "Refresh now",
  // sorting, the window selector and paging. Each open row has one detail row
  // node, made once and moved back under its row whenever the table is drawn
  // again, so what it shows stays as it is. details[id] is what the server
  // answered for that call (with its ETag), and which reports it was for.
  var openRows = {};   // call_id → true
  var detailRows = {}; // call_id → the detail <tr>
  var details = {};    // call_id → { etag, data, ids, status: 'loading' | 'ok' | 'error' | 'gone', why, seq }
  var rowReports = {}; // call_id → the report ids the row named when it was last drawn
  var detailSeq = 0;

  function $(id) { return document.getElementById(id); }

  function el(tag, className, text) {
    var n = document.createElement(tag);
    if (className) { n.className = className; }
    if (text !== undefined && text !== null) { n.textContent = String(text); }
    return n;
  }

  function isHttps(u) { return typeof u === 'string' && u.indexOf('https://') === 0; }

  // The "Perceptor today" of a row: words, style and the date of the re-scan;
  // null when the token has none.
  function todayOf(c) {
    if (!c || typeof c.perceptor_today_verdict !== 'string' || c.perceptor_today_verdict === '') { return null; }
    var v = Object.prototype.hasOwnProperty.call(TODAY_TEXT, c.perceptor_today_verdict) ? TODAY_TEXT[c.perceptor_today_verdict] : TODAY_UNKNOWN;
    var at = typeof c.perceptor_today_at === 'string' ? fmtDate(c.perceptor_today_at) : DASH;
    return { text: v.text, cls: v.cls, at: at === DASH ? '' : at, url: c.perceptor_today_url };
  }

  // The words of the title of a "Perceptor today" label.
  function todayTitle(t, linked) {
    return 'Perceptor re-scan' + (t.at ? ' on ' + t.at : '') + ', not the verdict at call time' +
      (linked ? ' (opens the re-scan report)' : '');
  }

  // A link that opens in a new tab, or plain text when the address is not https.
  function linkOrText(url, text, className) {
    var n;
    if (isHttps(url)) {
      n = el('a', className, text);
      n.href = url;
      n.target = '_blank';
      n.rel = 'noopener noreferrer';
    } else {
      n = el('span', className, text);
    }
    return n;
  }

  function isNum(v) { return typeof v === 'number' && isFinite(v); }

  function fmtInt(n) { return isNum(n) ? n.toLocaleString('en-US') : DASH; }

  // Share of the imported (first) calls; safe when nothing is imported yet.
  function pctOf(n, total) {
    if (!isNum(n) || !isNum(total) || total <= 0) { return 0; }
    return n / total * 100;
  }

  function fmtShare(n, total) { return pctOf(n, total).toFixed(1) + '%'; }

  function pad2(n) { return n < 10 ? '0' + n : String(n); }

  // Local date and time, e.g. 2026-10-02 14:05.
  function fmtDate(iso) {
    var d = new Date(iso);
    if (isNaN(d.getTime())) { return DASH; }
    return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) +
      ' ' + pad2(d.getHours()) + ':' + pad2(d.getMinutes());
  }

  // "day" or "7 days", after "the last".
  function daysText(n) { return n === 1 ? 'day' : fmtInt(n) + ' days'; }

  function fmtTime(d) { return pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds()); }

  // Huge values in scientific notation with superscript digits, so a column
  // never widens: 3.9e47 → "3.9×10⁴⁷" (one decimal; 9.96e47 → "1.0×10⁴⁸").
  var SUPERSCRIPT = { '0': '⁰', '1': '¹', '2': '²', '3': '³', '4': '⁴', '5': '⁵', '6': '⁶', '7': '⁷', '8': '⁸', '9': '⁹', '-': '⁻' };
  function sciText(a) {
    var parts = Math.abs(a).toExponential(1).split('e');
    var exp = String(Number(parts[1]));
    var sup = '';
    for (var i = 0; i < exp.length; i++) { sup += SUPERSCRIPT[exp.charAt(i)] || ''; }
    return parts[0] + '×10' + sup;
  }
  var HUGE_PCT = 1e6;  // |%| from here on: scientific notation
  var HUGE_USD = 1e12; // $ from here on ($1T)

  // The exact amount, for a title: all digits, grouped.
  function exactText(v, maxFraction) {
    return v.toLocaleString('en-US', { maximumFractionDigits: maxFraction });
  }

  function fmtPrice(v) {
    if (!isNum(v)) { return DASH; }
    if (v <= 0) { return '$0'; }
    if (Math.round(v) >= HUGE_USD) { return '$' + sciText(v); }
    if (v >= 1000) { return '$' + v.toLocaleString('en-US', { maximumFractionDigits: 0 }); }
    if (v >= 1) { return '$' + v.toFixed(2); }
    // small prices: three significant digits, written out (no exponent)
    var digits = Math.min(18, Math.ceil(-Math.log10(v)) + 2);
    return '$' + v.toFixed(digits);
  }

  // Market cap, compact: $850, $45.2k, $1.3M, $2.1B, then $1.0×10¹² from $1T
  // on. A dash when there is none (or it is not a positive number). One
  // decimal; a value that would round up to 1000 of a unit is written in the
  // next one ($999,960 → $1.0M).
  var MCAP_UNITS = [[1e3, 'k'], [1e6, 'M'], [1e9, 'B']];
  function fmtMcap(v) {
    if (!isNum(v) || v <= 0) { return DASH; }
    if (v < 1) { return '<$1'; }
    if (Math.round(v) < 1000) { return '$' + Math.round(v); }
    for (var i = 0; i < MCAP_UNITS.length; i++) {
      var body = (v / MCAP_UNITS[i][0]).toFixed(1);
      if (Number(body) < 1000) { return '$' + body + MCAP_UNITS[i][1]; }
    }
    return '$' + sciText(v);
  }

  // A market cap cell; the exact amount (and what it is) on hover.
  function mcapCell(v, what) {
    var td = el('td', 'num mc', fmtMcap(v));
    if (isNum(v) && v > 0) { td.title = what + ': $' + exactText(v, 0); }
    return td;
  }

  // Signed percentage: the sign is always written, so colour is never the only
  // cue. From ±1,000,000% on: scientific notation (+3.9×10⁴⁷%).
  function fmtPct(v) {
    if (!isNum(v)) { return DASH; }
    var a = Math.abs(v);
    var body = a >= 1000 ? a.toLocaleString('en-US', { maximumFractionDigits: 0 }) : a.toFixed(1);
    var n = Number(body.replace(/,/g, ''));
    if (n === 0) { return '0.0%'; }
    if (n >= HUGE_PCT) { body = sciText(a); }
    return (v > 0 ? '+' : '−') + body + '%';
  }

  // The exact percentage, for the title of a number written in scientific notation.
  function exactPct(v) { return (v > 0 ? '+' : v < 0 ? '−' : '') + exactText(Math.abs(v), 2) + '%'; }

  function isHugePct(v) { return isNum(v) && fmtPct(v).indexOf('×') >= 0; }

  // Size of a return, for its badge: rugged (or all lost), down by half or
  // more, down, flat, up to 2×, 2× to 11×, 11× and more. The sign is always
  // written as well, so colour is never the only cue.
  function returnBucket(v, rugged) {
    if (!isNum(v)) { return ''; }
    if (rugged === true || v <= -99.95) { return 'rb-rug'; }
    if (fmtPct(v) === '0.0%') { return 'rb-zero'; }
    if (v <= -50) { return 'rb-bad'; }
    if (v < 0) { return 'rb-neg'; }
    if (v < 100) { return 'rb-up'; }
    if (v < 1000) { return 'rb-2x'; }
    return 'rb-10x';
  }

  // A return as a badge coloured by its size (the Latest % column and the
  // row detail).
  function returnBadge(v, rugged) {
    var b = el('span', 'rb ' + returnBucket(v, rugged), fmtPct(v));
    if (isHugePct(v)) { b.title = exactPct(v); }
    return b;
  }

  // A window, peak or worst-drop cell: coloured text only (calm); a gain of
  // 100% or more is bold.
  function pctCell(v) {
    var td = el('td', 'num', fmtPct(v));
    if (isNum(v) && fmtPct(v) !== '0.0%') { td.classList.add(v > 0 ? 'pos' : 'neg'); }
    if (isNum(v) && v >= 100) { td.classList.add('big'); }
    if (isHugePct(v)) { td.title = exactPct(v); }
    return td;
  }

  // Peak % of a rugged call: none (the pool was drained).
  function peakCell(c) {
    if (c.rugged !== true) { return pctCell(c.peak_pct); }
    var td = el('td', 'num', DASH);
    td.title = 'No peak: the pool was rugged';
    return td;
  }

  var QUIET_MS = 7 * 24 * 3600 * 1000; // last trade this much older than the reading: "quiet"
  // The tracker reads the latest price about every 15 minutes for calls under
  // 30 days old and every hour for older ones (prod runs with
  // SCOUT_LATEST_REFRESH_OLD=1h since the 8 Oct deploy); a price older than
  // twice that is "stale" (the tracker may be stopped or behind). Rugged calls
  // found by the tracker (rug_block in their on-chain state, latestDueSQL) are
  // re-stamped at the old pace (SCOUT_LATEST_REFRESH_OLD) at any age; the page
  // has just the rugged flag, a superset of those, so every rugged call gets
  // the old limit: never a false "stale" (a young call flagged by the "< 5% of
  // entry" rule is still read every 15 minutes, so on it a stopped tracker
  // shows only after 2 h).
  // STALE_OLD_MS must follow SCOUT_LATEST_REFRESH_OLD (2x its value): raise it
  // again if that setting is raised (48 h for the 24 h default).
  var YOUNG_CALL_MS = 30 * 86400 * 1000;
  var STALE_YOUNG_MS = 30 * 60 * 1000;
  var STALE_OLD_MS = 2 * 3600 * 1000;

  // Age in the largest unit that reads well: 45m, 30h, 60d.
  function fmtAge(seconds) {
    if (!isNum(seconds)) { return ''; }
    var s = Math.max(0, seconds);
    if (s < 3600) { return Math.floor(s / 60) + 'm'; }
    if (s < 2 * 86400) { return Math.floor(s / 3600) + 'h'; }
    return Math.floor(s / 86400) + 'd';
  }

  // How long ago, e.g. "12m ago", "5h ago", "2d ago"; under a minute "just now".
  function fmtAgo(ms) {
    if (!isNum(ms)) { return ''; }
    return ms < 60000 ? 'just now' : fmtAge(ms / 1000) + ' ago';
  }

  // "Now" for the ages on the page: the server's time of the data shown
  // (this browser's clock until the first answer).
  function nowMs() { return isFinite(snapshotMs) ? snapshotMs : Date.now(); }

  function timeMs(iso) { return typeof iso === 'string' ? new Date(iso).getTime() : NaN; }

  // Whether a price read at readMs is stale at now for a call posted at callMs
  // (rugged: refreshed at the old pace whatever its age).
  function isStale(readMs, callMs, rugged, now) {
    var young = !rugged && isFinite(callMs) && now - callMs < YOUNG_CALL_MS;
    var limit = young ? STALE_YOUNG_MS : STALE_OLD_MS;
    return now - readMs > limit;
  }

  // A label of how old the latest price is ("12m ago · stale · quiet"),
  // written again by updateAges whenever "now" moves on.
  function priceAgeNode(c, className) {
    var n = el('span', 'price-age ' + (className || ''));
    var readAt = timeMs(c.latest_at);
    var tradeAt = timeMs(c.latest_trade_at);
    n.dataset.at = String(readAt);
    n.dataset.call = String(timeMs(c.message_date));
    if (c.rugged === true) { n.dataset.rugged = '1'; }
    if (!isNaN(readAt) && !isNaN(tradeAt) && readAt - tradeAt > QUIET_MS) { n.dataset.quiet = '1'; }
    drawPriceAge(n, nowMs());
    return n;
  }

  function drawPriceAge(n, now) {
    var at = Number(n.dataset.at);
    if (!isFinite(at)) { n.textContent = ''; return; }
    var stale = isStale(at, Number(n.dataset.call), n.dataset.rugged === '1', now);
    n.textContent = fmtAgo(Math.max(0, now - at)) + (stale ? ' · stale' : '') + (n.dataset.quiet === '1' ? ' · quiet' : '');
    n.classList.toggle('stale', stale);
  }

  // "… ago" of a time (data-t, ms) in the row detail.
  function agoNode(ms) {
    var n = el('span', 'rel-age');
    n.dataset.t = String(ms);
    n.textContent = fmtAgo(Math.max(0, nowMs() - ms));
    return n;
  }

  // Writes every "… ago" on the page again for the current "now": after each
  // answer of the list, also when it is "not modified" or has the same rows,
  // so the ages keep moving while the tracker (and so the data) stands still.
  function updateAges() {
    var now = nowMs();
    var root = $('rows');
    var ages = root.querySelectorAll('.price-age');
    for (var i = 0; i < ages.length; i++) { drawPriceAge(ages[i], now); }
    var rel = root.querySelectorAll('.rel-age');
    for (var j = 0; j < rel.length; j++) {
      var t = Number(rel[j].dataset.t);
      if (isFinite(t)) { rel[j].textContent = fmtAgo(Math.max(0, now - t)); }
    }
  }

  // Takes the server's time of an answer (X-Snapshot-At) as "now".
  function setSnapshotTime(iso) {
    var t = timeMs(iso);
    if (isFinite(t)) { snapshotMs = t; }
  }

  // "Latest %": the return at the most recent price as a badge, and under it
  // how long ago that price was read, e.g. "+35.2%" / "12m ago". The exact
  // times and the age of the call are in the title (and in the row detail).
  function latestCell(c) {
    var td = el('td', 'num latest');
    var v = c.latest_return_pct;
    if (!isNum(v)) { td.textContent = DASH; return td; }
    td.appendChild(returnBadge(v, c.rugged));
    var age = priceAgeNode(c, 'age');
    td.appendChild(age);

    var tip = [];
    if (isHugePct(v)) { tip.push(exactPct(v)); }
    if (!isNaN(timeMs(c.latest_at))) { tip.push('Price read ' + fmtDate(c.latest_at)); }
    if (!isNaN(timeMs(c.latest_trade_at))) { tip.push('last trade ' + fmtDate(c.latest_trade_at)); }
    if (isNum(c.latest_age_seconds)) { tip.push('the call was ' + fmtAge(c.latest_age_seconds) + ' old then'); }
    if (tip.length) { td.title = tip.join('; ') + (age.dataset.quiet === '1' ? ' (no recent trades)' : ''); }
    return td;
  }

  function shortAddress(ca) {
    ca = String(ca || '');
    if (ca.length <= 12) { return ca || DASH; }
    return ca.slice(0, 6) + '…' + ca.slice(-4);
  }

  // The call id of a row as a plain whole number ('' when it is not one).
  function callKey(c) {
    var id = c && c.call_id;
    return (typeof id === 'number' && isFinite(id) && id > 0 && Math.floor(id) === id) ? String(id) : '';
  }

  // Which reports a row names: when this changes, an open detail is asked for again.
  function reportIds(c) {
    return (isNum(c.perceptor_report_id) ? c.perceptor_report_id : 0) + ':' +
      (isNum(c.salpha_report_id) ? c.salpha_report_id : 0);
  }

  function tokenName(c) {
    return (typeof c.token_name === 'string' && c.token_name.trim() !== '') ? c.token_name : shortAddress(c.contract_address);
  }

  // One row, "now" first: Token (and symbol) · Latest % (and how old that
  // price is) · Latest MC · Call MC · Date · Perceptor · Status · Calls ·
  // Entry $ · Peak % · Worst drop % · the five windows. The order must match
  // the headers in index.html.
  function buildRow(c) {
    var tr = el('tr', 'call-row');
    var key = callKey(c);
    var name = tokenName(c);

    // Token: stays in view when the table scrolls sideways (sticky), with the
    // toggle of the row detail, so a narrow screen still shows whose row it is.
    var tdToken = el('td', 'tok');
    if (key) {
      tr.dataset.callId = key;
      // opens the row detail (performance and the token's reports); a real
      // button, so it works with the keyboard
      var tog = el('button', 'row-toggle');
      tog.type = 'button';
      tog.setAttribute('aria-expanded', 'false');
      tog.setAttribute('aria-label', 'Details of ' + name);
      tog.title = 'Show the performance and the Perceptor and sAlpha reports';
      var chev = el('span', 'chev', '▸');
      chev.setAttribute('aria-hidden', 'true');
      tog.appendChild(chev);
      tdToken.appendChild(tog);
    }
    var names = el('span', 'tok-names');
    var tokenNode = linkOrText(c.gmgn_url, name, 'token');
    tokenNode.title = name + '\n' + String(c.contract_address || '');
    names.appendChild(tokenNode);
    if (typeof c.token_symbol === 'string' && c.token_symbol.trim() !== '' && c.token_symbol !== name) {
      var symNode = el('span', 'symbol', c.token_symbol);
      symNode.title = c.token_symbol;
      names.appendChild(symNode);
    }
    tdToken.appendChild(names);
    tr.appendChild(tdToken);

    var unit = c.price_unit;
    var noUSD = typeof unit === 'string' && unit !== '' && unit !== 'usd';
    if (noUSD) {
      // tracked, but in another asset: no number here would be in dollars
      var tdNo = el('td', 'num muted nousd', 'no USD price');
      tdNo.title = 'The token trades against an asset with no dollar price';
      tr.appendChild(tdNo);
      tr.appendChild(el('td', 'num', DASH)); // latest market cap
      tr.appendChild(el('td', 'num', DASH)); // call market cap
    } else {
      tr.appendChild(latestCell(c));
      tr.appendChild(mcapCell(c.latest_mcap_usd, 'Estimate (market cap in the post × latest price ÷ price at the post)'));
      tr.appendChild(mcapCell(c.call_mcap_usd, 'Market cap in the post'));
    }

    var tdDate = el('td', 'date');
    tdDate.appendChild(linkOrText(c.post_url, fmtDate(c.message_date)));
    tr.appendChild(tdDate);

    // Latest Perceptor report of the token; a dash when it was never scanned
    // (or the report had no readable verdict).
    var tdPerc = el('td', 'perc');
    var pv = Object.prototype.hasOwnProperty.call(VERDICT_TEXT, c.perceptor_verdict) ? VERDICT_TEXT[c.perceptor_verdict] : null;
    if (pv) {
      var pvNode = linkOrText(c.perceptor_url, pv.text, pv.cls);
      if (pvNode.tagName === 'A') { pvNode.title = 'Open the Perceptor report'; }
      tdPerc.appendChild(pvNode);
    } else {
      tdPerc.appendChild(document.createTextNode(DASH));
    }
    // the token has an sAlpha report with text (an empty reply counts as none)
    if (c.has_salpha_report === true) {
      var sa = el('span', 'sa-badge', 'sA');
      sa.title = 'sAlpha report available: open the row (▸) to read it';
      tdPerc.appendChild(sa);
    }
    // "Perceptor today": a re-scan long after the call, on a line of its own
    var today = todayOf(c);
    if (today) {
      var line = el('span', 'today-line');
      var tNode = linkOrText(today.url, 'today: ' + today.text, today.cls);
      tNode.title = todayTitle(today, tNode.tagName === 'A');
      line.appendChild(tNode);
      tdPerc.appendChild(line);
    }
    tr.appendChild(tdPerc);

    // Status: "rugged" replaces the tracking status (which goes in the title).
    var tdStatus = el('td', 'status');
    var st = c.tracking_status;
    var stText = st ? (STATUS_TEXT[st] || String(st)) : DASH;
    if (c.rugged === true) {
      var rug = el('span', 'badge badge-rug', 'rugged');
      rug.title = 'Rugged: the pool was drained (tracking status: ' + stText + ')';
      tdStatus.appendChild(rug);
    } else {
      tdStatus.textContent = stText;
    }
    tr.appendChild(tdStatus);

    // The list has one row per token (its first call); this marks tokens called again.
    var tdCalls = el('td', 'calls');
    if (isNum(c.call_count) && c.call_count > 1) {
      var rep = el('span', 'repeat', '×' + fmtInt(c.call_count));
      rep.title = 'Called ' + fmtInt(c.call_count) + ' times, last on ' + fmtDate(c.last_call_date);
      tdCalls.appendChild(rep);
    }
    tr.appendChild(tdCalls);

    if (noUSD) {
      tr.appendChild(el('td', 'num', DASH)); // entry
      var td = el('td', 'center muted', DASH);
      td.colSpan = NO_USD_SPAN; // peak, worst drop and the windows
      td.title = 'No USD price: no performance is shown';
      tr.appendChild(td);
      return tr;
    }
    var tdEntry = el('td', 'num', fmtPrice(c.entry_price_usd));
    if (isNum(c.entry_price_usd) && fmtPrice(c.entry_price_usd).indexOf('×') >= 0) { tdEntry.title = '$' + exactText(c.entry_price_usd, 0); }
    tr.appendChild(tdEntry);
    tr.appendChild(peakCell(c));
    tr.appendChild(pctCell(c.drawdown_pct));
    for (var w = 0; w < WINDOW_FIELDS.length; w++) {
      var wc = pctCell(c[WINDOW_FIELDS[w]]);
      wc.classList.add('win');
      if (w === 0) { wc.classList.add('first'); }
      tr.appendChild(wc);
    }
    return tr;
  }

  function setMessage(text, isError) {
    var m = $('table-msg');
    if (!text) { m.hidden = true; m.textContent = ''; return; }
    m.hidden = false;
    m.textContent = text;
    m.classList.toggle('error', !!isError);
  }

  // ---- Row detail ---------------------------------------------------------

  function colCount() {
    var n = document.querySelectorAll('thead th').length;
    return n > 0 ? n : COLUMNS;
  }

  function setExpanded(tr, key, open) {
    var btn = tr.querySelector('.row-toggle');
    tr.classList.toggle('open', open);
    if (!btn) { return; }
    btn.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) {
      btn.setAttribute('aria-controls', 'detail-' + key);
    } else {
      btn.removeAttribute('aria-controls');
    }
  }

  // The detail row of a call: made once, kept while the page is open. It has
  // two parts: the performance of the row (from the row itself, drawn again
  // whenever the row changes) and the token's reports (from GET api/call).
  function detailRowFor(key) {
    var tr = detailRows[key];
    if (tr) { return tr; }
    tr = el('tr', 'detail-row');
    tr.id = 'detail-' + key;
    var td = el('td', 'detail-cell');
    td.colSpan = colCount();
    var box = el('div', 'detail-box');
    box.appendChild(el('div', 'detail-perf'));
    box.appendChild(el('div', 'detail-today'));
    box.appendChild(el('div', 'detail-reports'));
    td.appendChild(box);
    tr.appendChild(td);
    detailRows[key] = tr;
    drawPerf(key);
    drawDetail(key);
    return tr;
  }

  // One line of the performance block: a term and its value (text or nodes).
  function perfItem(dl, term, parts) {
    var div = el('div');
    div.appendChild(el('dt', '', term));
    var dd = el('dd');
    for (var i = 0; i < parts.length; i++) {
      if (parts[i] === null || parts[i] === undefined || parts[i] === '') { continue; }
      dd.appendChild(typeof parts[i] === 'string' ? document.createTextNode(parts[i]) : parts[i]);
    }
    div.appendChild(dd);
    dl.appendChild(div);
  }

  // A percentage as coloured text, with the exact value in the title when it
  // is written in powers of ten.
  function pctNode(v) {
    var n = el('span', '', fmtPct(v));
    if (isNum(v) && fmtPct(v) !== '0.0%') { n.className = v > 0 ? 'pos' : 'neg'; }
    if (isHugePct(v)) { n.title = exactPct(v); }
    return n;
  }

  // The performance block of the row detail, from the row as last drawn, and
  // the "Perceptor today" block (also from the row).
  function drawPerf(key) {
    var tr = detailRows[key];
    var c = rowData[key];
    if (!tr) { return; }
    drawToday(key);
    var box = tr.querySelector('.detail-perf');
    box.textContent = '';
    if (!c) { return; }
    var sec = el('section', 'report perf');
    sec.appendChild(el('h3', 'report-title', 'Performance'));
    var dl = el('dl', 'perf-list');
    var called = timeMs(c.message_date);
    var callParts = [fmtDate(c.message_date)];
    if (!isNaN(called)) { callParts.push(' (', agoNode(called), ')'); }
    perfItem(dl, 'Called', callParts);
    var unit = c.price_unit;
    if (typeof unit === 'string' && unit !== '' && unit !== 'usd') {
      perfItem(dl, 'Performance', ['none: the token trades against an asset with no dollar price']);
      sec.appendChild(dl);
      box.appendChild(sec);
      return;
    }
    var v = c.latest_return_pct;
    perfItem(dl, 'Latest %', [isNum(v) ? returnBadge(v, c.rugged) : DASH]);
    perfItem(dl, 'Entry price', [fmtPrice(c.entry_price_usd), isNum(c.entry_price_usd) ? ' (60 s after the post)' : '']);
    perfItem(dl, 'Latest price', [fmtPrice(c.latest_price_usd)]);
    var readAt = timeMs(c.latest_at);
    if (!isNaN(readAt)) {
      perfItem(dl, 'Price read', [fmtDate(c.latest_at), ' (', priceAgeNode(c, ''), ')',
        isNum(c.latest_age_seconds) ? '; the call was ' + fmtAge(c.latest_age_seconds) + ' old then' : '']);
    } else {
      perfItem(dl, 'Price read', ['not yet']);
    }
    var tradeAt = timeMs(c.latest_trade_at);
    if (!isNaN(tradeAt)) {
      perfItem(dl, 'Last trade', [fmtDate(c.latest_trade_at), ' (', agoNode(tradeAt), ')']);
    }
    perfItem(dl, 'Call MC → Latest MC', [fmtMcap(c.call_mcap_usd), ' → ', fmtMcap(c.latest_mcap_usd)]);
    var peak = c.rugged === true ? el('span', '', DASH + ' (rugged)') : pctNode(c.peak_pct);
    perfItem(dl, 'Peak % (' + state.horizon + ')', [peak]);
    perfItem(dl, 'Worst drop % (' + state.horizon + ')', [pctNode(c.drawdown_pct)]);
    sec.appendChild(dl);
    box.appendChild(sec);
  }

  // The "Perceptor today (re-scan)" block of the row detail, from the row as
  // last drawn (GET api/call has the call-time reports only). Empty when the
  // token has no re-scan.
  function drawToday(key) {
    var tr = detailRows[key];
    if (!tr) { return; }
    var box = tr.querySelector('.detail-today');
    box.textContent = '';
    var t = todayOf(rowData[key]);
    if (!t) { return; }
    var sec = el('section', 'report report-today');
    sec.appendChild(el('h3', 'report-title', 'Perceptor today (re-scan)'));
    var parts = [el('span', t.cls, 'today: ' + t.text)];
    if (t.at) { parts.push(t.at); }
    sec.appendChild(reportMeta(parts));
    sec.appendChild(el('p', 'muted small', 'A Perceptor re-scan made long after the call, not the verdict at call time. ' +
      'The Perceptor filter and its counts use the call-time verdict only.'));
    if (isHttps(t.url)) { sec.appendChild(linkOrText(t.url, 'Open the re-scan report', 'report-link')); }
    box.appendChild(sec);
  }

  function reportMeta(parts) {
    var p = el('p', 'report-meta');
    for (var i = 0; i < parts.length; i++) {
      if (i > 0) { p.appendChild(document.createTextNode(' · ')); }
      p.appendChild(typeof parts[i] === 'string' ? document.createTextNode(parts[i]) : parts[i]);
    }
    return p;
  }

  function perceptorSection(p) {
    var sec = el('section', 'report');
    sec.appendChild(el('h3', 'report-title', 'Perceptor'));
    if (!p || typeof p !== 'object') {
      sec.appendChild(el('p', 'muted', 'No Perceptor report'));
      return sec;
    }
    var pv = Object.prototype.hasOwnProperty.call(VERDICT_TEXT, p.verdict) ? VERDICT_TEXT[p.verdict] : null;
    // the verdict once: Perceptor's own label when there is one ("No red flags
    // found"), else the words of the list ("no red flags")
    var label = typeof p.label === 'string' ? p.label.trim() : '';
    var parts = [pv ? el('span', pv.cls, label || pv.text) : el('span', 'muted', label || 'verdict not readable')];
    if (typeof p.at === 'string') { parts.push(fmtDate(p.at)); }
    sec.appendChild(reportMeta(parts));
    if (typeof p.summary === 'string' && p.summary.trim() !== '') {
      sec.appendChild(el('p', 'report-text', p.summary));
    }
    if (p.truncated === true) { sec.appendChild(el('p', 'muted small', 'Cut at 32 KB.')); }
    if (isHttps(p.url)) { sec.appendChild(linkOrText(p.url, 'Open the Perceptor report', 'report-link')); }
    return sec;
  }

  function salphaSection(s) {
    var sec = el('section', 'report');
    sec.appendChild(el('h3', 'report-title', 'sAlpha'));
    if (s && typeof s === 'object' && s.declined === true) {
      // sAlpha replied, but only to decline ("Not enough public signals …")
      sec.appendChild(el('p', 'report-declined', 'sAlpha did not generate a report'));
      var why = [];
      if (typeof s.text === 'string' && s.text.trim() !== '') { why.push('Reason: ' + s.text.trim()); }
      if (typeof s.at === 'string') { why.push(fmtDate(s.at)); }
      if (why.length) { sec.appendChild(reportMeta(why)); }
      return sec;
    }
    if (!s || typeof s !== 'object' || typeof s.text !== 'string' || s.text.trim() === '') {
      sec.appendChild(el('p', 'muted', 'No sAlpha report'));
      return sec;
    }
    if (typeof s.at === 'string') { sec.appendChild(reportMeta([fmtDate(s.at)])); }
    sec.appendChild(el('p', 'report-text', s.text));
    if (s.truncated === true) { sec.appendChild(el('p', 'muted small', 'Cut at 32 KB.')); }
    if (isHttps(s.url)) { sec.appendChild(linkOrText(s.url, 'Open the sAlpha report', 'report-link')); }
    return sec;
  }

  // Draws what is known of a call's reports into its detail row.
  function drawDetail(key) {
    var tr = detailRows[key];
    if (!tr) { return; }
    var box = tr.querySelector('.detail-reports');
    var d = details[key];
    box.textContent = '';
    if (!d || (d.status === 'loading' && !d.data)) {
      box.appendChild(el('p', 'muted detail-note', 'Loading…'));
      return;
    }
    if (d.status === 'gone') {
      box.appendChild(el('p', 'error detail-note', 'Not available in the data now shown; refresh the page.'));
      return;
    }
    if (!d.data) {
      box.appendChild(el('p', 'error detail-note', 'Could not load the reports' + (d.why ? ' (' + d.why + ')' : '') +
        '. They are asked for again with the next refresh.'));
      return;
    }
    box.appendChild(perceptorSection(d.data.perceptor));
    box.appendChild(salphaSection(d.data.salpha));
    if (d.status === 'error') {
      box.appendChild(el('p', 'error small detail-note', 'Could not check for newer reports' + (d.why ? ' (' + d.why + ')' : '') + '.'));
    }
  }

  // Asks for a call's detail again when the row names other reports than the
  // ones shown, or the last try failed. The server answers 304 when nothing
  // changed, and then nothing is drawn again.
  function refreshDetail(key) {
    var d = details[key];
    if (d && d.status === 'ok' && d.ids === rowReports[key]) { return; }
    if (d && d.status === 'loading' && d.want === rowReports[key]) { return; } // already on its way
    loadDetail(key);
  }

  function loadDetail(key) {
    var d = details[key];
    if (!d) { d = details[key] = { etag: '', data: null, ids: '', status: '', why: '', seq: 0, want: '' }; }
    var seq = ++detailSeq;
    var want = rowReports[key] || '';
    var hadError = d.status === 'error';
    d.seq = seq;
    d.want = want;
    if (!d.data) { d.status = 'loading'; drawDetail(key); } else { d.status = 'loading'; }
    fetchJSON('api/call?id=' + encodeURIComponent(key), d.data ? d.etag : '').then(function (res) {
      if (d.seq !== seq) { return; } // a newer request has taken over
      d.ids = want;
      d.why = '';
      d.status = 'ok';
      if (res.data === null) { // not modified: what is shown is current
        if (hadError) { drawDetail(key); } // only the notice of the failed try goes
        return;
      }
      if (!res.data || typeof res.data !== 'object') { throw new Error('unexpected answer'); }
      d.data = res.data;
      d.etag = res.etag;
      drawDetail(key);
    }).catch(function (err) {
      if (d.seq !== seq) { return; }
      d.status = err && err.status === 404 ? 'gone' : 'error';
      d.why = err && err.message ? err.message : '';
      if (d.status === 'gone') { d.data = null; d.etag = ''; }
      drawDetail(key);
    });
  }

  // Open details whose last try failed are tried again with every refresh
  // (also when the list itself has not changed).
  function retryFailedDetails() {
    for (var key in openRows) {
      if (Object.prototype.hasOwnProperty.call(openRows, key) && details[key] && details[key].status === 'error' &&
          detailRows[key] && detailRows[key].parentNode) {
        loadDetail(key);
      }
    }
  }

  function toggleRow(tr) {
    var key = tr.dataset.callId;
    if (!key) { return; }
    if (openRows[key]) {
      delete openRows[key];
      var dr = detailRows[key];
      if (dr && dr.parentNode) { dr.parentNode.removeChild(dr); }
      setExpanded(tr, key, false);
      return;
    }
    openRows[key] = true;
    tr.parentNode.insertBefore(detailRowFor(key), tr.nextSibling);
    setExpanded(tr, key, true);
    refreshDetail(key);
  }

  // The label of a header: Peak % and Worst drop % name the window chosen.
  function headerLabel(base, th) {
    return th.dataset.window ? base + ' (' + state.horizon + ')' : base;
  }

  function renderHeaders() {
    var ths = document.querySelectorAll('th[data-sort]');
    for (var i = 0; i < ths.length; i++) {
      var th = ths[i];
      var btn = th.querySelector('button');
      if (!btn.dataset.label) { btn.dataset.label = btn.textContent; }
      var label = headerLabel(btn.dataset.label, th);
      if (th.dataset.sort === state.sort) {
        th.setAttribute('aria-sort', state.dir === 'asc' ? 'ascending' : 'descending');
        btn.textContent = label + ' ' + (state.dir === 'asc' ? '▲' : '▼');
      } else {
        th.removeAttribute('aria-sort');
        btn.textContent = label;
      }
    }
    var dd = $('th-drawdown');
    if (!dd.dataset.label) { dd.dataset.label = dd.textContent; }
    dd.textContent = headerLabel(dd.dataset.label, dd);
    var hb = document.querySelectorAll('#horizons button');
    for (var j = 0; j < hb.length; j++) {
      hb[j].setAttribute('aria-pressed', hb[j].dataset.horizon === state.horizon ? 'true' : 'false');
    }
    var db = document.querySelectorAll('#days button');
    for (var k = 0; k < db.length; k++) {
      db[k].setAttribute('aria-pressed', Number(db[k].dataset.days) === state.days ? 'true' : 'false');
    }
    $('explain').textContent = 'All numbers in USD, measured from the price 60 seconds after the post. ' +
      'Latest % is the return at the most recent price; under it, how long ago that price was read ("stale" = older' +
      ' than 30 minutes, or 2 hours for older or rugged calls). ' + HORIZONS.join(', ') + ' = the return over that window. Peak % and' +
      ' Worst drop % are over ' + state.horizon + ' (the selector changes only these two).';
  }

  function renderCalls(data) {
    var rows = $('rows');
    var frag = document.createDocumentFragment();
    var calls = Array.isArray(data.calls) ? data.calls : [];
    var reopen = [];
    rowData = {};
    for (var i = 0; i < calls.length; i++) {
      var tr = buildRow(calls[i]);
      frag.appendChild(tr);
      var key = callKey(calls[i]);
      if (!key) { continue; }
      rowReports[key] = reportIds(calls[i]);
      rowData[key] = calls[i];
      if (openRows[key]) {
        // still open: the same detail node goes back under the row, with the
        // performance of the row as it is now
        setExpanded(tr, key, true);
        frag.appendChild(detailRowFor(key));
        drawPerf(key);
        reopen.push(key);
      }
    }
    rows.textContent = '';
    rows.appendChild(frag);
    for (var j = 0; j < reopen.length; j++) { refreshDetail(reopen[j]); }

    var pages = renderPager(data);
    // the newest calls are on view now: nothing left to announce
    if (state.sort === 'date' && state.dir === 'desc' && state.page === 1) { setNewCount(0); }

    if (calls.length === 0) {
      if (state.page > pages) { state.page = pages; loadCalls(false); return; }
      setMessage(state.q ? 'No calls match this search.' : (state.verdicts.length ? 'No calls match this filter.'
        : state.days ? 'No calls in the last ' + daysText(state.days) + '.' : 'No calls yet.'), false);
    } else {
      setMessage('', false);
    }
  }

  // The pager and the total under the table; returns the number of pages.
  function renderPager(data) {
    var total = isNum(data.total) ? data.total : 0;
    var per = isNum(data.per) && data.per > 0 ? data.per : PER_PAGE;
    var pages = Math.max(1, Math.ceil(total / per));
    $('page-info').textContent = 'Page ' + state.page + ' of ' + pages;
    $('prev').disabled = state.page <= 1;
    $('next').disabled = state.page >= pages;
    $('total').textContent = fmtInt(total) + (total === 1 ? ' call' : ' calls') +
      (isNum(data.days) && data.days > 0 ? ' from the last ' + daysText(data.days) : '') +
      (data.usd_only ? ' (priced in USD only)' : '');
    return pages;
  }

  // Asks for JSON. With an etag the server may answer 304 ("what you have is
  // still current"): then data is null. at = when the server last read the
  // database (X-Snapshot-At).
  function fetchJSON(url, etag, signal) {
    var headers = { 'Accept': 'application/json' };
    if (etag) { headers['If-None-Match'] = etag; }
    return fetch(url, { headers: headers, cache: 'no-store', signal: signal }).then(function (r) {
      var meta = { etag: r.headers.get('ETag') || '', at: r.headers.get('X-Snapshot-At') || '' };
      if (r.status === 304) {
        // read the (empty) body so the browser counts the request as finished
        return r.text().then(function () { return { data: null, etag: etag, at: meta.at }; });
      }
      if (!r.ok) {
        var err = new Error('HTTP ' + r.status);
        err.status = r.status;
        throw err;
      }
      return r.json().then(function (data) { return { data: data, etag: meta.etag, at: meta.at }; });
    });
  }

  // Everything of an answer that the table shows (not the time it was read).
  function callsSignature(d) {
    return JSON.stringify([d.total, d.page, d.per, d.horizon, d.sort, d.dir, d.usd_only, d.verdict, d.days, d.calls]);
  }

  // quiet = background refresh: no "Loading…", and the rows stay if it fails.
  function loadCalls(quiet) {
    if (callsAbort) { callsAbort.abort(); }
    var ctl = (typeof AbortController === 'function') ? new AbortController() : null;
    callsAbort = ctl;
    var seq = ++requestSeq;
    var p = new URLSearchParams();
    if (state.q) { p.set('q', state.q); }
    p.set('sort', state.sort);
    p.set('dir', state.dir);
    p.set('horizon', state.horizon);
    // always in one order: the address decides whether to ask "has it changed?"
    if (state.verdicts.length) { p.set('verdict', state.verdicts.join(',')); }
    if (state.days) { p.set('days', String(state.days)); }
    p.set('page', String(state.page));
    p.set('per', String(PER_PAGE));
    var url = 'api/calls?' + p.toString();
    if (!quiet && !loadedOnce) { setMessage('Loading…', false); }
    // only ask "has it changed?" about the very list the table shows
    var etag = (shown.data && shown.url === url) ? shown.etag : '';
    var mine = function () { return seq === requestSeq; };
    fetchJSON(url, etag, ctl ? ctl.signal : undefined).then(function (res) {
      if (!mine()) { return; } // a newer request has taken over
      callsAbort = null;
      loadedOnce = true;
      var hadError = callsErrorShown;
      callsErrorShown = false;
      // the server's time moves on with every refresh, also when nothing
      // else does: the "… ago" labels follow it in every case below
      setSnapshotTime(res.at);
      if (res.data === null) {
        // not modified: the rows are current, only their ages move on
        if (hadError) { renderCalls(shown.data); }
        updateAges();
        return;
      }
      var sig = callsSignature(res.data);
      var same = shown.data !== null && shown.url === url && shown.sig === sig;
      shown = { url: url, etag: res.etag, sig: sig, data: res.data };
      if (!same || hadError) { renderCalls(res.data); } // same rows: nothing to draw again
      updateAges();
    }).catch(function (err) {
      if (!mine()) { return; } // cancelled by a newer request
      callsAbort = null;
      if (err && err.name === 'AbortError') { return; }
      var what = err && err.message ? ' (' + err.message + ')' : '';
      callsErrorShown = true;
      setMessage(loadedOnce
        ? 'Could not refresh the calls' + what + '. Showing the last loaded rows.'
        : 'Could not load the calls' + what + '.', true);
    });
  }

  function renderSummary(s) {
    var imported = isNum(s.imported) ? s.imported : 0;
    var tracked = isNum(s.tracked) ? s.tracked : 0;
    var share = pctOf(tracked, imported);

    var h = $('summary-headline');
    h.textContent = '';
    h.appendChild(el('strong', '', fmtInt(tracked)));
    h.appendChild(document.createTextNode(' / ' + fmtInt(imported) + ' calls tracked (' + share.toFixed(1) + '%)'));

    var bar = $('summary-bar');
    bar.setAttribute('aria-valuenow', share.toFixed(1));
    bar.setAttribute('aria-valuetext', fmtInt(tracked) + ' of ' + fmtInt(imported) + ' calls tracked');
    $('summary-bar-fill').style.width = Math.max(0, Math.min(100, share)) + '%';

    function stat(id, n) { $(id).textContent = fmtInt(n) + ' (' + fmtShare(n, imported) + ')'; }
    stat('stat-pending', s.pending || 0);
    stat('stat-nopool', s.no_pool || 0);
    stat('stat-errors', (s.error || 0) + (s.gave_up || 0));
    stat('stat-nousd', s.no_usd_price || 0);

    var repeats = isNum(s.repeat_calls) && s.repeat_calls > 0 ? s.repeat_calls : 0;
    var updates = isNum(s.update_posts) && s.update_posts > 0 ? s.update_posts : 0;
    var hidden = [];
    if (repeats > 0) { hidden.push(fmtInt(repeats) + (repeats === 1 ? ' repeat call' : ' repeat calls')); }
    if (updates > 0) { hidden.push(fmtInt(updates) + (updates === 1 ? ' update post' : ' update posts')); }
    $('summary-note').textContent = 'One row per token (its first call).' + (hidden.length
      ? ' ' + hidden.join(' and ') + (repeats + updates === 1 ? ' is' : ' are') + ' not shown.' : '');

    showUpdated(s.updated_at);
  }

  // "Updated hh:mm:ss": when the server last read the database.
  function showUpdated(iso) {
    var when = new Date(iso);
    if (!isNaN(when.getTime())) {
      $('summary-updated').textContent = 'Updated ' + fmtTime(when);
      // a newer server time also moves the "… ago" labels of the rows on
      if (!(when.getTime() <= snapshotMs)) { setSnapshotTime(iso); updateAges(); }
    }
    $('summary-error').hidden = true;
  }

  function loadSummary() {
    fetchJSON('api/summary', summaryEtag).then(function (res) {
      if (res.data === null) { showUpdated(res.at); return; } // the counts have not changed
      summaryEtag = res.etag;
      renderSummary(res.data);
    }).catch(function (err) {
      var e = $('summary-error');
      e.hidden = false;
      e.classList.add('error');
      e.textContent = 'Could not refresh the progress' + (err && err.message ? ' (' + err.message + ')' : '') + '.';
      if ($('summary-headline').textContent === 'Loading…') { $('summary-headline').textContent = DASH; }
    });
  }

  var refreshMsgTimer = null;

  // A notice under the progress; one that is not an error goes away by itself.
  function setRefreshMessage(text, isError) {
    var m = $('refresh-msg');
    clearTimeout(refreshMsgTimer);
    m.textContent = text || '';
    m.hidden = !text;
    m.classList.toggle('error', !!isError);
    if (text && !isError) {
      refreshMsgTimer = setTimeout(function () { setRefreshMessage('', false); }, 8000);
    }
  }

  // Short notice for a failed "Refresh now"; the server's own text is not shown.
  function refreshErrorText(status, data) {
    var why = status === 504 ? 'the database did not answer in time'
      : status === 503 ? 'the database could not be read'
        : status === 403 ? 'the request was refused'
          : status === 0 ? 'no answer from the website' : 'HTTP ' + status;
    var at = data && typeof data.snapshot_at === 'string' ? new Date(data.snapshot_at) : null;
    return 'Could not refresh: ' + why + '.' +
      (at && !isNaN(at.getTime()) ? ' Still showing the data read at ' + fmtTime(at) + '.' : '');
  }

  // "Refresh now": the website reads the database at once, then the page asks
  // for the progress and the list again. The server reads at most once per 5
  // seconds for this button and lets presses share a read under way.
  function refreshNow() {
    if (refreshing) { return; }
    refreshing = true;
    var btn = $('refresh-now');
    btn.disabled = true;
    btn.textContent = 'Refreshing…';
    setRefreshMessage('', false);
    var done = function () {
      refreshing = false;
      btn.disabled = false;
      btn.textContent = 'Refresh now';
    };
    fetch('api/refresh', { method: 'POST', headers: { 'Accept': 'application/json' }, cache: 'no-store', credentials: 'same-origin' })
      .then(function (r) {
        return r.json().then(function (d) { return d; }, function () { return null; }).then(function (d) {
          return { status: r.status, ok: r.ok, data: d };
        });
      })
      .then(function (res) {
        var d = res.data;
        if (!res.ok || !d || typeof d !== 'object') {
          setRefreshMessage(refreshErrorText(res.ok ? 0 : res.status, d), true);
          return;
        }
        if (typeof d.snapshot_at === 'string') { showUpdated(d.snapshot_at); }
        if (d.rate_limited === true) {
          // no new read: the last press was under 5 seconds ago (its read may have failed)
          var at = typeof d.snapshot_at === 'string' ? new Date(d.snapshot_at) : null;
          setRefreshMessage('Pressed less than 5 seconds ago; showing the data read at ' +
            (at && !isNaN(at.getTime()) ? fmtTime(at) : DASH) + '.', false);
        }
        refresh(); // the progress and the list, from the new snapshot
      })
      .catch(function () { setRefreshMessage(refreshErrorText(0, null), true); })
      .then(done, done);
  }

  function bind() {
    $('refresh-now').addEventListener('click', refreshNow);

    // Opening a row: its toggle button, or a click elsewhere on the row that
    // is not on a link or another control, nor the end of selecting text.
    $('rows').addEventListener('click', function (ev) {
      var t = ev.target;
      if (!t || typeof t.closest !== 'function') { return; }
      var tr = t.closest('tr');
      if (!tr || !tr.classList.contains('call-row')) { return; }
      if (t.closest('.row-toggle')) { toggleRow(tr); return; }
      if (t.closest('a, button, input, select, textarea, summary, label')) { return; }
      var sel = window.getSelection ? window.getSelection() : null;
      if (sel && !sel.isCollapsed && String(sel).trim() !== '') { return; }
      toggleRow(tr);
    });

    var ths = document.querySelectorAll('th[data-sort]');
    Array.prototype.forEach.call(ths, function (th) {
      th.querySelector('button').addEventListener('click', function () {
        var key = th.dataset.sort;
        if (state.sort === key) {
          state.dir = state.dir === 'desc' ? 'asc' : 'desc';
        } else {
          state.sort = key;
          state.dir = 'desc';
        }
        state.page = 1;
        renderHeaders();
        loadCalls(false);
      });
    });

    Array.prototype.forEach.call(document.querySelectorAll('#horizons button'), function (b) {
      b.addEventListener('click', function () {
        if (HORIZONS.indexOf(b.dataset.horizon) < 0 || b.dataset.horizon === state.horizon) { return; }
        state.horizon = b.dataset.horizon;
        state.page = 1;
        renderHeaders();
        loadCalls(false);
      });
    });

    $('search').addEventListener('input', function (ev) {
      var v = ev.target.value;
      clearTimeout(searchTimer);
      searchTimer = setTimeout(function () {
        var q = v.trim().slice(0, 100);
        if (q === state.q) { return; }
        state.q = q;
        state.page = 1;
        loadCalls(false);
      }, DEBOUNCE_MS);
    });

    bindVerdictPicker();
    bindDaysPicker();

    $('prev').addEventListener('click', function () {
      if (state.page > 1) { state.page--; loadCalls(false); }
    });
    $('next').addEventListener('click', function () {
      state.page++;
      loadCalls(false);
    });
  }

  // ---- Perceptor filter (several verdicts at once) --------------------------
  //
  // A button that opens a small panel of checkboxes; a change applies at once.
  // None ticked, or all four, is "all". The choice is kept in localStorage
  // (not in the address; only the age filter is kept there).

  var VERDICTS_KEY = 'scout.verdicts';

  // The known buckets of list, once each, in VERDICT_FILTERS order; every
  // bucket is the same as none ([] = all).
  function normVerdicts(list) {
    var out = [];
    for (var i = 0; i < VERDICT_FILTERS.length; i++) {
      if (Array.isArray(list) && list.indexOf(VERDICT_FILTERS[i]) >= 0) { out.push(VERDICT_FILTERS[i]); }
    }
    return out.length === VERDICT_FILTERS.length ? [] : out;
  }

  // The words of the button, e.g. "Perceptor: No red flags + Caution".
  function verdictLabel(list) {
    if (!list.length) { return 'Perceptor: all'; }
    var words = [];
    for (var i = 0; i < list.length; i++) { words.push(VERDICT_FILTER_TEXT[list[i]]); }
    return 'Perceptor: ' + words.join(' + ');
  }

  function readVerdicts() {
    var v = null;
    try { v = window.localStorage.getItem(VERDICTS_KEY); } catch (e) { return []; }
    return typeof v === 'string' && v ? normVerdicts(v.split(',')) : [];
  }

  function writeVerdicts(list) {
    try {
      if (list.length) {
        window.localStorage.setItem(VERDICTS_KEY, list.join(','));
      } else {
        window.localStorage.removeItem(VERDICTS_KEY);
      }
    } catch (e) {
      // storage blocked (private mode, settings): the choice lasts while the page is open
    }
  }

  function verdictBoxes() { return $('verdict-panel').querySelectorAll('input[type="checkbox"]'); }

  function renderVerdictLabel() {
    var text = verdictLabel(state.verdicts);
    $('verdict-label').textContent = text;
    $('verdict-btn').title = text;
  }

  function setVerdicts(list) {
    var next = normVerdicts(list);
    if (next.join(',') === state.verdicts.join(',')) { return; }
    state.verdicts = next;
    state.page = 1;
    writeVerdicts(next);
    renderVerdictLabel();
    loadCalls(false);
  }

  function verdictPanelOpen() { return $('verdict-btn').getAttribute('aria-expanded') === 'true'; }

  function openVerdictPanel() {
    $('verdict-panel').hidden = false;
    $('verdict-btn').setAttribute('aria-expanded', 'true');
    var first = verdictBoxes()[0];
    if (first) { first.focus(); }
  }

  // focusButton: put the focus back on the button (Esc, a click of the button).
  function closeVerdictPanel(focusButton) {
    if (!verdictPanelOpen()) { return; }
    $('verdict-panel').hidden = true;
    $('verdict-btn').setAttribute('aria-expanded', 'false');
    if (focusButton) { $('verdict-btn').focus(); }
  }

  function bindVerdictPicker() {
    var pick = $('verdict-pick');
    state.verdicts = readVerdicts();
    // the ticks follow the saved choice (a reload may keep the old ticks)
    var boxes = verdictBoxes();
    for (var i = 0; i < boxes.length; i++) { boxes[i].checked = state.verdicts.indexOf(boxes[i].value) >= 0; }
    renderVerdictLabel();
    $('verdict-btn').addEventListener('click', function () {
      if (verdictPanelOpen()) { closeVerdictPanel(true); } else { openVerdictPanel(); }
    });
    $('verdict-panel').addEventListener('change', function (ev) {
      var t = ev.target;
      if (!t || t.type !== 'checkbox') { return; }
      var picked = [];
      var all = verdictBoxes();
      for (var j = 0; j < all.length; j++) {
        if (all[j].checked) { picked.push(all[j].value); }
      }
      setVerdicts(picked);
    });
    $('verdict-all').addEventListener('click', function () {
      var all = verdictBoxes();
      for (var j = 0; j < all.length; j++) { all[j].checked = false; }
      setVerdicts([]);
    });
    pick.addEventListener('keydown', function (ev) {
      if ((ev.key === 'Escape' || ev.key === 'Esc') && verdictPanelOpen()) {
        ev.preventDefault();
        closeVerdictPanel(true);
      }
    });
    // a click or tap anywhere else closes it, and so does the focus leaving it
    document.addEventListener('pointerdown', function (ev) {
      if (verdictPanelOpen() && !pick.contains(ev.target)) { closeVerdictPanel(false); }
    });
    pick.addEventListener('focusout', function (ev) {
      var to = ev.relatedTarget;
      if (to && !pick.contains(to)) { closeVerdictPanel(false); }
    });
  }

  // ---- Age filter ("Calls from the last 1d / 7d / 30d / All") ---------------
  //
  // Kept in the page's address (?days=7), so a link or a reload shows the same
  // choice; without it, every call is shown (as before the filter existed).

  function readDaysFromURL() {
    var v = null;
    try { v = new URLSearchParams(window.location.search).get('days'); } catch (e) { return 0; }
    var n = Number(v);
    return (DAY_CHOICES.indexOf(n) >= 0 && String(n) === v) ? n : 0;
  }

  function writeDaysToURL() {
    if (!window.history || typeof window.history.replaceState !== 'function') { return; }
    var p = new URLSearchParams(window.location.search);
    if (state.days) { p.set('days', String(state.days)); } else { p.delete('days'); }
    var qs = p.toString();
    try {
      window.history.replaceState(null, '', window.location.pathname + (qs ? '?' + qs : '') + window.location.hash);
    } catch (e) {
      // some browsers refuse it for pages opened from a file: the choice lasts while the page is open
    }
  }

  function setDays(n) {
    if (n !== 0 && DAY_CHOICES.indexOf(n) < 0) { return; }
    if (n === state.days) { return; }
    state.days = n;
    state.page = 1;
    writeDaysToURL();
    renderHeaders();
    loadCalls(false);
  }

  function bindDaysPicker() {
    state.days = readDaysFromURL();
    writeDaysToURL(); // drops a days= the page does not offer
    Array.prototype.forEach.call(document.querySelectorAll('#days button'), function (b) {
      b.addEventListener('click', function () { setDays(Number(b.dataset.days)); });
    });
  }

  // ---- Live updates (Server-Sent Events) -----------------------------------
  //
  // GET api/events streams what changed in the list: "call" (a new token row),
  // "report" (a token's Perceptor verdict or sAlpha report changed, or, with
  // tool perceptor_today, its Perceptor re-scan, "Perceptor today", which
  // updates the row without a notice, sound or desktop alert) and
  // "reload" (many changes at once, or events were missed). A new row goes on
  // top of the table only when the table shows the newest calls first, on page
  // 1, and the row matches the search and the Perceptor filter; otherwise a
  // "N new — refresh" button appears. Only a call posted within the last hour
  // is announced (notice, sound, desktop alert); an older one (a post imported
  // by -backfill) reaches the table quietly, with one reload of the page per
  // burst of such rows. The 30-second refresh keeps running in
  // any case, so a page without live updates (refused, unsupported, broken
  // connection) still stays current.

  var LIVE_URL = 'api/events';
  var LIVE_RETRY_MS = 120000; // after the server refused the stream (e.g. too many open pages)
  var NOTICE_MAX = 4;
  var NOTICE_MS = 60000;
  var FRESH_MS = 6000;
  var RECENT_CALL_MS = 3600000; // a call posted longer ago than this is not announced
  var QUIET_LOAD_MS = 1000; // old calls arriving within this time make one reload of the list
  var quietLoadTimer = null;
  var live = { es: null, retryTimer: null, newCount: 0 };
  var prefs = { sound: false, desktop: false };
  var audioCtx = null;

  function readPref(name) {
    try { return window.localStorage.getItem('scout.' + name) === '1'; } catch (e) { return false; }
  }

  function writePref(name, on) {
    try {
      window.localStorage.setItem('scout.' + name, on ? '1' : '0');
    } catch (e) {
      // storage blocked (private mode, settings): the choice lasts while the page is open
    }
  }

  var LIVE_TEXT = {
    connecting: 'Live: connecting…',
    on: 'Live',
    off: 'Live off: updates every 30 s',
    unsupported: 'Updates every 30 s'
  };

  function setLiveState(st) {
    var n = $('live-state');
    n.textContent = LIVE_TEXT[st] || '';
    n.classList.toggle('live-on', st === 'on');
    n.title = st === 'on' ? 'New calls and reports appear as soon as they are recorded'
      : st === 'connecting' ? 'Connecting to live updates; the list still refreshes every 30 seconds'
        : 'No live updates; the list refreshes every 30 seconds';
  }

  function verdictBucket(v) {
    return (v === 'clean' || v === 'caution' || v === 'red_flags') ? v : 'not_scanned';
  }

  // Whether a row passes the search and the Perceptor filter of the page, the
  // way the server decides it (search: name, symbol or address contains the
  // text, letter case ignored; verdict: the row's bucket is one of those chosen).
  function rowMatchesFilters(c) {
    if (state.verdicts.length && state.verdicts.indexOf(verdictBucket(c.perceptor_verdict)) < 0) { return false; }
    if (state.days) {
      // the server counts back from its snapshot time, and so does this
      var posted = timeMs(c.message_date);
      if (!isFinite(posted) || posted < nowMs() - state.days * 86400000) { return false; }
    }
    if (state.q) {
      var q = state.q.toLowerCase();
      var fields = [c.token_name, c.token_symbol, c.contract_address];
      for (var i = 0; i < fields.length; i++) {
        if (typeof fields[i] === 'string' && fields[i].toLowerCase().indexOf(q) >= 0) { return true; }
      }
      return false;
    }
    return true;
  }

  // The table shows the newest calls first, on page 1: a new row goes on top.
  function viewTakesNewRows() {
    return state.sort === 'date' && state.dir === 'desc' && state.page === 1 &&
      shown.data !== null && Array.isArray(shown.data.calls) && !callsErrorShown;
  }

  function findRow(key) {
    // key is digits only (callKey), so it is safe in the selector
    return key ? $('rows').querySelector('tr.call-row[data-call-id="' + key + '"]') : null;
  }

  function shownIndex(key) {
    var calls = shown.data && Array.isArray(shown.data.calls) ? shown.data.calls : [];
    for (var i = 0; i < calls.length; i++) {
      if (callKey(calls[i]) === key) { return i; }
    }
    return -1;
  }

  function markFresh(tr) {
    tr.classList.add('fresh');
    setTimeout(function () { tr.classList.remove('fresh'); }, FRESH_MS);
  }

  function removeRow(key) {
    var tr = findRow(key);
    if (tr && tr.parentNode) { tr.parentNode.removeChild(tr); }
    var dr = detailRows[key];
    if (dr && dr.parentNode) { dr.parentNode.removeChild(dr); }
  }

  function setNewCount(n) {
    live.newCount = n;
    var b = $('new-calls');
    b.hidden = n <= 0;
    b.textContent = n > 0 ? fmtInt(n) + ' new — refresh' : '';
    b.title = n > 0 ? 'Show the newest calls first, from page 1' : '';
  }

  function showNewest() {
    setNewCount(0);
    if (state.sort !== 'date' || state.dir !== 'desc' || state.page !== 1) {
      state.sort = 'date';
      state.dir = 'desc';
      state.page = 1;
      renderHeaders();
    }
    refresh();
  }

  // Whether a call was posted within the last hour, by this browser's clock (a
  // date in the future counts as recent). A missing or unreadable date does not.
  function isRecentCall(c, nowMs) {
    var t = (c && typeof c.message_date === 'string') ? Date.parse(c.message_date) : NaN;
    if (!isFinite(t)) { return false; }
    return nowMs - t <= RECENT_CALL_MS;
  }

  // One quiet reload of the list for a burst of old calls (e.g. a -backfill),
  // instead of one per event.
  function scheduleQuietLoad() {
    if (quietLoadTimer !== null) { return; }
    quietLoadTimer = setTimeout(function () {
      quietLoadTimer = null;
      loadCalls(true);
    }, QUIET_LOAD_MS);
  }

  // A new token row: on top of the table when the view allows it. A call
  // posted more than an hour ago is old news: no notice, no sound, no desktop
  // alert, no "N new" count (it is not among the newest calls); the list is
  // reloaded quietly, once per burst.
  function onLiveCall(d) {
    var c = d.row;
    var key = callKey(c);
    if (!key) { return; }
    if (!isRecentCall(c, Date.now())) {
      if (rowMatchesFilters(c) && !findRow(key)) { scheduleQuietLoad(); }
      return;
    }
    liveNotice('call', c, '');
    if (!rowMatchesFilters(c) || findRow(key)) { return; }
    if (!viewTakesNewRows()) { setNewCount(live.newCount + 1); return; }
    var calls = shown.data.calls;
    var newer = calls.length === 0 || new Date(c.message_date).getTime() >= new Date(calls[0].message_date).getTime();
    if (d.horizon !== state.horizon || !newer) {
      loadCalls(true); // the server's snapshot already has the row: ask for the page
      return;
    }
    var rows = $('rows');
    var tr = buildRow(c);
    rows.insertBefore(tr, rows.firstChild);
    markFresh(tr);
    rowReports[key] = reportIds(c);
    rowData[key] = c;
    calls.unshift(c);
    shown.data.total = (isNum(shown.data.total) ? shown.data.total : 0) + 1;
    var per = isNum(shown.data.per) && shown.data.per > 0 ? shown.data.per : PER_PAGE;
    while (calls.length > per) { removeRow(callKey(calls.pop())); }
    shown.sig = callsSignature(shown.data);
    renderPager(shown.data);
    setMessage('', false);
  }

  // A report changed: the row's verdict and badge, in place.
  function onLiveReport(d) {
    var c = d.row;
    var key = callKey(c);
    if (!key) { return; }
    // A Perceptor re-scan ("Perceptor today") is about an old call, up to 100 a
    // day: it updates the row quietly, with no notice, sound or desktop alert,
    // so it never pushes the call-time notices out of the corner.
    if (d.tool !== 'perceptor_today') { liveNotice('report', c, d.tool); }
    var tr = findRow(key);
    if (!tr) { return; }
    if (!rowMatchesFilters(c)) { loadCalls(true); return; } // no longer in this filter
    var i = shownIndex(key);
    if (d.horizon !== state.horizon && i >= 0) {
      // the event carries another window's numbers: keep the ones shown
      var old = shown.data.calls[i];
      c.return_pct = old.return_pct;
      c.peak_pct = old.peak_pct;
      c.drawdown_pct = old.drawdown_pct;
    }
    var fresh = buildRow(c);
    if (openRows[key]) { setExpanded(fresh, key, true); }
    tr.parentNode.replaceChild(fresh, tr);
    markFresh(fresh);
    rowReports[key] = reportIds(c);
    rowData[key] = c;
    if (i >= 0) {
      shown.data.calls[i] = c;
      shown.sig = callsSignature(shown.data);
    }
    if (openRows[key]) { drawPerf(key); refreshDetail(key); }
  }

  function onLiveReload(d) {
    var n = d && isNum(d.calls) ? d.calls : 0;
    if (n > 0 && !viewTakesNewRows()) { setNewCount(live.newCount + n); }
    if (n > 0) { showNotice('New calls', fmtInt(n) + (n === 1 ? ' new call' : ' new calls'), '', '', ''); }
    refresh();
  }

  function tokenLabel(c) {
    var name = (typeof c.token_name === 'string' && c.token_name.trim() !== '') ? c.token_name : shortAddress(c.contract_address);
    if (typeof c.token_symbol === 'string' && c.token_symbol.trim() !== '' && c.token_symbol !== name) {
      name += ' (' + c.token_symbol + ')';
    }
    return name;
  }

  // The notice of an event: token, verdict and a GMGN link; a sound and a
  // desktop notification when they are switched on. Not called for a
  // Perceptor re-scan (tool perceptor_today), which only updates its row.
  function liveNotice(kind, c, tool) {
    var title, text, cls;
    if (kind === 'report' && tool === 'salpha') {
      title = 'sAlpha';
      if (c.has_salpha_report === true) {
        text = 'report available';
        cls = 'sa-badge';
      } else {
        text = 'did not generate a report';
        cls = 'muted';
      }
    } else {
      title = kind === 'call' ? 'New call' : 'Perceptor';
      var pv = Object.prototype.hasOwnProperty.call(VERDICT_TEXT, c.perceptor_verdict) ? VERDICT_TEXT[c.perceptor_verdict] : null;
      text = pv ? pv.text : (kind === 'call' ? 'not scanned yet' : 'no readable verdict');
      cls = pv ? pv.cls : 'muted';
    }
    var name = tokenLabel(c);
    showNotice(title, name, text, cls, c.gmgn_url);
    beep(kind === 'call' ? 880 : 660);
    desktopNotify(title + ': ' + name, text, callKey(c));
  }

  function showNotice(title, name, text, cls, url) {
    var box = $('live-notices');
    var li = el('li', 'live-note');
    li.appendChild(el('strong', 'live-note-title', title));
    li.appendChild(el('span', 'live-note-token', name));
    if (text) { li.appendChild(el('span', cls, text)); }
    if (isHttps(url)) { li.appendChild(linkOrText(url, 'GMGN', 'live-note-link')); }
    var close = el('button', 'live-note-close', '×');
    close.type = 'button';
    close.setAttribute('aria-label', 'Dismiss: ' + title + ' ' + name);
    li.appendChild(close);
    box.insertBefore(li, box.firstChild);
    while (box.children.length > NOTICE_MAX) { box.removeChild(box.lastChild); }
    setTimeout(function () { if (li.parentNode) { li.parentNode.removeChild(li); } }, NOTICE_MS);
  }

  // ---- Sound (Web Audio, no file) and desktop notifications ----------------

  function audioReady() {
    var AC = window.AudioContext || window.webkitAudioContext;
    if (!AC) { return false; }
    if (!audioCtx) {
      try { audioCtx = new AC(); } catch (e) { return false; }
    }
    if (audioCtx.state === 'suspended' && typeof audioCtx.resume === 'function') {
      var p = audioCtx.resume();
      if (p && typeof p.catch === 'function') { p.catch(function () { /* resumes with the next click */ }); }
    }
    return true;
  }

  // A short beep; browsers only let a page play sound after a click on it.
  function beep(freq) {
    if (!prefs.sound || !audioCtx || audioCtx.state !== 'running') { return; }
    var t = audioCtx.currentTime;
    var osc = audioCtx.createOscillator();
    var gain = audioCtx.createGain();
    osc.type = 'sine';
    osc.frequency.value = freq;
    gain.gain.setValueAtTime(0.0001, t);
    gain.gain.exponentialRampToValueAtTime(0.2, t + 0.01);
    gain.gain.exponentialRampToValueAtTime(0.0001, t + 0.18);
    osc.connect(gain);
    gain.connect(audioCtx.destination);
    osc.start(t);
    osc.stop(t + 0.2);
  }

  function setSound(on) {
    prefs.sound = on && audioReady();
    writePref('sound', prefs.sound);
    $('sound').checked = prefs.sound;
  }

  // Desktop notifications need a secure page: https, or localhost.
  function desktopSupported() {
    return typeof window.Notification === 'function' && window.isSecureContext === true;
  }

  function renderDesktopButton() {
    var b = $('desktop');
    if (!desktopSupported()) {
      b.disabled = true;
      b.setAttribute('aria-pressed', 'false');
      b.title = 'Desktop alerts only work on https:// or localhost';
      return;
    }
    var denied = window.Notification.permission === 'denied';
    var on = prefs.desktop && window.Notification.permission === 'granted';
    b.disabled = denied;
    b.setAttribute('aria-pressed', on ? 'true' : 'false');
    b.title = denied ? 'Desktop alerts are blocked in the browser settings for this site'
      : on ? 'Desktop alerts are on (shown while this tab is in the background); click to turn off'
        : 'Show a desktop alert for new calls and reports while this tab is in the background';
  }

  function toggleDesktop() {
    if (!desktopSupported()) { return; }
    if (prefs.desktop) {
      prefs.desktop = false;
      writePref('desktop', false);
      renderDesktopButton();
      return;
    }
    var answered = false;
    var done = function (perm) {
      if (answered) { return; }
      answered = true;
      prefs.desktop = perm === 'granted';
      writePref('desktop', prefs.desktop);
      renderDesktopButton();
    };
    var p = window.Notification.requestPermission(done); // older browsers: the callback
    if (p && typeof p.then === 'function') { p.then(done, function () { done('denied'); }); }
  }

  function desktopNotify(title, body, key) {
    if (!prefs.desktop || !desktopSupported() || window.Notification.permission !== 'granted' || !document.hidden) { return; }
    try {
      var n = new window.Notification(title, { body: body, tag: 'scout-' + (key || 'event') });
      n.addEventListener('click', function () { window.focus(); n.close(); });
    } catch (e) {
      // some browsers only allow notifications from a service worker: the in-page notice is shown anyway
    }
  }

  // ---- The stream ------------------------------------------------------------

  function liveData(ev) {
    var d;
    try { d = JSON.parse(ev.data); } catch (e) { return null; }
    return (d && typeof d === 'object') ? d : null;
  }

  function startLive() {
    live.retryTimer = null;
    if (typeof window.EventSource !== 'function') { setLiveState('unsupported'); return; }
    var es = new window.EventSource(LIVE_URL);
    live.es = es;
    setLiveState('connecting');
    es.addEventListener('open', function () { setLiveState('on'); });
    es.addEventListener('error', function () {
      if (es.readyState !== 2) { setLiveState('connecting'); return; } // the browser reconnects by itself
      // refused (e.g. 503: too many open pages) or not a stream: try again later
      es.close();
      if (live.es === es) { live.es = null; }
      setLiveState('off');
      if (live.retryTimer === null) { live.retryTimer = setTimeout(startLive, LIVE_RETRY_MS); }
    });
    es.addEventListener('call', function (ev) {
      var d = liveData(ev);
      if (d && d.row && typeof d.row === 'object') { onLiveCall(d); }
    });
    es.addEventListener('report', function (ev) {
      var d = liveData(ev);
      if (d && d.row && typeof d.row === 'object') { onLiveReport(d); }
    });
    es.addEventListener('reload', function (ev) { onLiveReload(liveData(ev)); });
  }

  function bindLive() {
    $('new-calls').addEventListener('click', showNewest);
    $('live-notices').addEventListener('click', function (ev) {
      var t = ev.target;
      if (!t || typeof t.closest !== 'function') { return; }
      var btn = t.closest('.live-note-close');
      var li = btn ? btn.closest('li') : null;
      if (li && li.parentNode) { li.parentNode.removeChild(li); }
    });
    $('sound').addEventListener('change', function (ev) { setSound(ev.target.checked); });
    $('desktop').addEventListener('click', toggleDesktop);
    // browsers start sound only after a click or a key on the page
    var unlock = function () {
      if (prefs.sound) { audioReady(); }
      document.removeEventListener('pointerdown', unlock);
      document.removeEventListener('keydown', unlock);
    };
    document.addEventListener('pointerdown', unlock);
    document.addEventListener('keydown', unlock);
    prefs.sound = readPref('sound');
    $('sound').checked = prefs.sound;
    prefs.desktop = readPref('desktop');
    renderDesktopButton();
  }

  // The table box scrolls down too (for the sticky header), and some browsers
  // count its vertical scrollbar in 100cqw: the row detail would then reach
  // under the scrollbar. A probe 100cqw wide tells by how much 100cqw is wider
  // than the visible part of the box: that is --sbw, taken off in style.css.
  // It also keeps --head-h and --tok-w (the sticky header row's height and
  // the Token column's width) up to date for the box's scroll-padding.
  function watchScrollbar() {
    var wrap = document.querySelector('.table-wrap');
    if (!wrap) { return; }
    var probe = el('div', 'cq-probe');
    probe.setAttribute('aria-hidden', 'true');
    wrap.insertBefore(probe, wrap.firstChild);
    var measure = function () {
      var sbw = Math.max(0, probe.offsetWidth - wrap.clientWidth);
      wrap.style.setProperty('--sbw', sbw + 'px');
      // the sticky header row and Token column, for scroll-padding in style.css
      var head = wrap.querySelector('thead');
      var tok = wrap.querySelector('th.tok');
      if (head) { wrap.style.setProperty('--head-h', Math.ceil(head.getBoundingClientRect().height) + 'px'); }
      if (tok) { wrap.style.setProperty('--tok-w', Math.ceil(tok.getBoundingClientRect().width) + 'px'); }
    };
    measure();
    if (typeof window.ResizeObserver === 'function') {
      new window.ResizeObserver(measure).observe(wrap);
      new window.ResizeObserver(measure).observe($('rows'));
    } else {
      window.addEventListener('resize', measure);
    }
  }

  bind();
  bindLive();
  watchScrollbar();
  renderHeaders();
  loadSummary();
  loadCalls(false);
  startLive();

  // Background refresh every 30 seconds, but not while the tab is hidden. When
  // the tab is shown again and the last refresh is older than that, refresh at once.
  function refresh() {
    lastRefresh = Date.now();
    loadSummary();
    loadCalls(true);
    retryFailedDetails();
    scheduleRefresh(REFRESH_MS);
  }

  function scheduleRefresh(ms) {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(function () {
      refreshTimer = null;
      if (document.hidden) { return; } // picked up again by visibilitychange
      refresh();
    }, ms);
  }

  document.addEventListener('visibilitychange', function () {
    if (document.hidden) { return; }
    var age = Date.now() - lastRefresh;
    if (age >= REFRESH_MS) { refresh(); } else if (refreshTimer === null) { scheduleRefresh(REFRESH_MS - age); }
  });

  lastRefresh = Date.now();
  scheduleRefresh(REFRESH_MS);
})();
