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
  // Cells after Latest MC that "no USD price" spans: Latest %, Peak %, Worst
  // drop % and the five windows. Must match the headers in index.html.
  var NO_USD_SPAN = 3 + HORIZONS.length;
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
  var VERDICT_FILTERS = ['', 'clean', 'caution', 'red_flags', 'not_scanned'];

  var state = { q: '', sort: 'date', dir: 'desc', horizon: '1d', verdict: '', page: 1 };
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

  function $(id) { return document.getElementById(id); }

  function el(tag, className, text) {
    var n = document.createElement(tag);
    if (className) { n.className = className; }
    if (text !== undefined && text !== null) { n.textContent = String(text); }
    return n;
  }

  function isHttps(u) { return typeof u === 'string' && u.indexOf('https://') === 0; }

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

  function fmtTime(d) { return pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds()); }

  function fmtPrice(v) {
    if (!isNum(v)) { return DASH; }
    if (v <= 0) { return '$0'; }
    if (v >= 1000) { return '$' + v.toLocaleString('en-US', { maximumFractionDigits: 0 }); }
    if (v >= 1) { return '$' + v.toFixed(2); }
    // small prices: three significant digits, written out (no exponent)
    var digits = Math.min(18, Math.ceil(-Math.log10(v)) + 2);
    return '$' + v.toFixed(digits);
  }

  // Market cap, compact: $850, $45.2k, $1.3M, $2.1B. A dash when there is none
  // (or it is not a positive number). One decimal; a value that would round up
  // to 1000 of a unit is written in the next one ($999,960 → $1.0M).
  var MCAP_UNITS = [[1e3, 'k'], [1e6, 'M'], [1e9, 'B'], [1e12, 'T']];
  function fmtMcap(v) {
    if (!isNum(v) || v <= 0) { return DASH; }
    if (v < 1) { return '<$1'; }
    if (Math.round(v) < 1000) { return '$' + Math.round(v); }
    for (var i = 0; i < MCAP_UNITS.length; i++) {
      var body = (v / MCAP_UNITS[i][0]).toFixed(1);
      if (Number(body) < 1000 || i === MCAP_UNITS.length - 1) { return '$' + body + MCAP_UNITS[i][1]; }
    }
    return DASH;
  }

  // A market cap cell; the exact amount (and what it is) on hover.
  function mcapCell(v, what) {
    var td = el('td', 'num mc', fmtMcap(v));
    if (isNum(v) && v > 0) { td.title = what + ': $' + v.toLocaleString('en-US', { maximumFractionDigits: 0 }); }
    return td;
  }

  // Signed percentage: the sign is always written, so colour is never the only cue.
  function fmtPct(v) {
    if (!isNum(v)) { return DASH; }
    var a = Math.abs(v);
    var body = a >= 1000 ? a.toLocaleString('en-US', { maximumFractionDigits: 0 }) : a.toFixed(1);
    if (Number(body.replace(/,/g, '')) === 0) { return '0.0%'; }
    return (v > 0 ? '+' : '−') + body + '%';
  }

  function pctCell(v) {
    var td = el('td', 'num', fmtPct(v));
    if (isNum(v) && fmtPct(v) !== '0.0%') { td.classList.add(v > 0 ? 'pos' : 'neg'); }
    return td;
  }

  var QUIET_MS = 7 * 24 * 3600 * 1000; // last trade this much older than the reading: "quiet"

  // Age of a call in the largest unit that reads well: 45m, 30h, 60d.
  function fmtAge(seconds) {
    if (!isNum(seconds)) { return ''; }
    var s = Math.max(0, seconds);
    if (s < 3600) { return Math.floor(s / 60) + 'm'; }
    if (s < 2 * 86400) { return Math.floor(s / 3600) + 'h'; }
    return Math.floor(s / 86400) + 'd';
  }

  // "Latest %": the return at the most recent price, then a quiet label with
  // the age of the call when that price was read, e.g. "+35.2% · 60d".
  function latestCell(c) {
    var td = el('td', 'num latest');
    var v = c.latest_return_pct;
    if (!isNum(v)) { td.textContent = DASH; return td; }
    var text = fmtPct(v);
    var val = el('span', 'latest-val', text);
    if (text !== '0.0%') { val.classList.add(v > 0 ? 'pos' : 'neg'); }
    td.appendChild(val);

    var readAt = new Date(c.latest_at).getTime();
    var tradeAt = (typeof c.latest_trade_at === 'string') ? new Date(c.latest_trade_at).getTime() : NaN;
    var quiet = !isNaN(readAt) && !isNaN(tradeAt) && readAt - tradeAt > QUIET_MS;
    var age = fmtAge(c.latest_age_seconds);
    var label = (age ? ' · ' + age : '') + (quiet ? ' · quiet' : '');
    if (label) { td.appendChild(el('span', 'age', label)); }

    var tip = [];
    if (!isNaN(readAt)) { tip.push('Price as of ' + fmtDate(c.latest_at)); }
    if (!isNaN(tradeAt)) { tip.push('last trade ' + fmtDate(c.latest_trade_at)); }
    if (tip.length) { td.title = tip.join('; ') + (quiet ? ' (no recent trades)' : ''); }
    return td;
  }

  function shortAddress(ca) {
    ca = String(ca || '');
    if (ca.length <= 12) { return ca || DASH; }
    return ca.slice(0, 6) + '…' + ca.slice(-4);
  }

  function buildRow(c) {
    var tr = el('tr');

    var tdDate = el('td');
    var dateNode = linkOrText(c.post_url, fmtDate(c.message_date));
    tdDate.appendChild(dateNode);
    tr.appendChild(tdDate);

    var name = (typeof c.token_name === 'string' && c.token_name.trim() !== '') ? c.token_name : shortAddress(c.contract_address);
    var tdToken = el('td');
    var tokenNode = linkOrText(c.gmgn_url, name, 'token');
    tokenNode.title = name + '\n' + String(c.contract_address || '');
    tdToken.appendChild(tokenNode);
    tr.appendChild(tdToken);

    var tdSym = el('td');
    if (typeof c.token_symbol === 'string' && c.token_symbol.trim() !== '') {
      var symNode = linkOrText(c.gmgn_url, c.token_symbol, 'symbol');
      symNode.title = c.token_symbol;
      tdSym.appendChild(symNode);
    } else {
      tdSym.textContent = DASH;
    }
    tr.appendChild(tdSym);

    // The list has one row per token (its first call); this marks tokens called again.
    var tdCalls = el('td', 'calls');
    if (isNum(c.call_count) && c.call_count > 1) {
      var rep = el('span', 'repeat', '×' + fmtInt(c.call_count));
      rep.title = 'Called ' + fmtInt(c.call_count) + ' times, last on ' + fmtDate(c.last_call_date);
      tdCalls.appendChild(rep);
    }
    tr.appendChild(tdCalls);

    // Latest Perceptor report of the token; a dash when it was never scanned
    // (or the report had no readable verdict).
    var tdPerc = el('td');
    var pv = Object.prototype.hasOwnProperty.call(VERDICT_TEXT, c.perceptor_verdict) ? VERDICT_TEXT[c.perceptor_verdict] : null;
    if (pv) {
      var pvNode = linkOrText(c.perceptor_url, pv.text, pv.cls);
      if (pvNode.tagName === 'A') { pvNode.title = 'Open the Perceptor report'; }
      tdPerc.appendChild(pvNode);
    } else {
      tdPerc.textContent = DASH;
    }
    tr.appendChild(tdPerc);

    var tdStatus = el('td');
    var st = c.tracking_status;
    tdStatus.appendChild(document.createTextNode(st ? (STATUS_TEXT[st] || String(st)) : DASH));
    if (c.rugged === true) {
      tdStatus.appendChild(document.createTextNode(' · '));
      tdStatus.appendChild(el('span', 'flag', 'rugged'));
    }
    tr.appendChild(tdStatus);

    var unit = c.price_unit;
    if (typeof unit === 'string' && unit !== '' && unit !== 'usd') {
      // tracked, but in another asset: no number here would be in dollars
      tr.appendChild(el('td', 'num', DASH)); // entry
      tr.appendChild(el('td', 'num', DASH)); // call market cap
      tr.appendChild(el('td', 'num', DASH)); // latest market cap
      var td = el('td', 'center muted', 'no USD price');
      td.colSpan = NO_USD_SPAN; // latest, peak, worst drop and the windows
      tr.appendChild(td);
    } else {
      tr.appendChild(el('td', 'num', fmtPrice(c.entry_price_usd)));
      tr.appendChild(mcapCell(c.call_mcap_usd, 'Market cap in the post'));
      tr.appendChild(mcapCell(c.latest_mcap_usd, 'Estimate (market cap in the post × latest price ÷ price at the post)'));
      tr.appendChild(latestCell(c));
      tr.appendChild(pctCell(c.peak_pct));
      tr.appendChild(pctCell(c.drawdown_pct));
      for (var w = 0; w < WINDOW_FIELDS.length; w++) {
        var wc = pctCell(c[WINDOW_FIELDS[w]]);
        wc.classList.add('win');
        if (w === 0) { wc.classList.add('first'); }
        tr.appendChild(wc);
      }
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
    $('explain').textContent = 'All numbers in USD, measured from the price 60 seconds after the post. ' +
      HORIZONS.join(', ') + ' = the return over that window. Peak % and Worst drop % are over ' + state.horizon +
      ' (the selector changes only these two). Latest % is the return at the most recent price, with the age of the' +
      ' call at that time.';
  }

  function renderCalls(data) {
    var rows = $('rows');
    var frag = document.createDocumentFragment();
    var calls = Array.isArray(data.calls) ? data.calls : [];
    for (var i = 0; i < calls.length; i++) { frag.appendChild(buildRow(calls[i])); }
    rows.textContent = '';
    rows.appendChild(frag);

    var total = isNum(data.total) ? data.total : 0;
    var per = isNum(data.per) && data.per > 0 ? data.per : PER_PAGE;
    var pages = Math.max(1, Math.ceil(total / per));
    $('page-info').textContent = 'Page ' + state.page + ' of ' + pages;
    $('prev').disabled = state.page <= 1;
    $('next').disabled = state.page >= pages;
    $('total').textContent = fmtInt(total) + (total === 1 ? ' call' : ' calls') +
      (data.usd_only ? ' (priced in USD only)' : '');

    if (calls.length === 0) {
      if (state.page > pages) { state.page = pages; loadCalls(false); return; }
      setMessage(state.q ? 'No calls match this search.' : (state.verdict ? 'No calls match this filter.' : 'No calls yet.'), false);
    } else {
      setMessage('', false);
    }
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
      if (!r.ok) { throw new Error('HTTP ' + r.status); }
      return r.json().then(function (data) { return { data: data, etag: meta.etag, at: meta.at }; });
    });
  }

  // Everything of an answer that the table shows (not the time it was read).
  function callsSignature(d) {
    return JSON.stringify([d.total, d.page, d.per, d.horizon, d.sort, d.dir, d.usd_only, d.verdict, d.calls]);
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
    if (state.verdict) { p.set('verdict', state.verdict); }
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
      if (res.data === null) {
        // not modified: the table is current
        if (hadError) { renderCalls(shown.data); }
        return;
      }
      var sig = callsSignature(res.data);
      var same = shown.data !== null && shown.url === url && shown.sig === sig;
      shown = { url: url, etag: res.etag, sig: sig, data: res.data };
      if (same && !hadError) { return; } // same rows: nothing to draw
      renderCalls(res.data);
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
    if (!isNaN(when.getTime())) { $('summary-updated').textContent = 'Updated ' + fmtTime(when); }
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

    $('verdict').addEventListener('change', function (ev) {
      var v = ev.target.value;
      if (VERDICT_FILTERS.indexOf(v) < 0 || v === state.verdict) { return; }
      state.verdict = v;
      state.page = 1;
      loadCalls(false);
    });

    $('prev').addEventListener('click', function () {
      if (state.page > 1) { state.page--; loadCalls(false); }
    });
    $('next').addEventListener('click', function () {
      state.page++;
      loadCalls(false);
    });
  }

  bind();
  // a reload can keep the old choice in the select: start from "All reports"
  $('verdict').value = '';
  renderHeaders();
  loadSummary();
  loadCalls(false);

  // Background refresh every 30 seconds, but not while the tab is hidden. When
  // the tab is shown again and the last refresh is older than that, refresh at once.
  function refresh() {
    lastRefresh = Date.now();
    loadSummary();
    loadCalls(true);
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
