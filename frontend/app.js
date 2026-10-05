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
  var STATUS_TEXT = {
    pending: 'pending', tracking: 'tracking', done: 'done',
    no_pool: 'no pool', error: 'error', gave_up: 'gave up'
  };

  var state = { q: '', sort: 'date', dir: 'desc', horizon: '1d', page: 1 };
  var loadedOnce = false;
  var requestSeq = 0;
  var searchTimer = null;

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

    var unit = c.price_unit;
    if (typeof unit === 'string' && unit !== '' && unit !== 'usd') {
      // tracked, but in another asset: no number here would be in dollars
      tr.appendChild(el('td', 'num', DASH));
      var td = el('td', 'center muted', 'no USD price');
      td.colSpan = 3;
      tr.appendChild(td);
    } else {
      tr.appendChild(el('td', 'num', fmtPrice(c.entry_price_usd)));
      tr.appendChild(pctCell(c.return_pct));
      tr.appendChild(pctCell(c.peak_pct));
      tr.appendChild(pctCell(c.drawdown_pct));
    }

    var tdStatus = el('td');
    var st = c.tracking_status;
    tdStatus.appendChild(document.createTextNode(st ? (STATUS_TEXT[st] || String(st)) : DASH));
    if (c.rugged === true) {
      tdStatus.appendChild(document.createTextNode(' · '));
      tdStatus.appendChild(el('span', 'flag', 'rugged'));
    }
    tr.appendChild(tdStatus);
    return tr;
  }

  function setMessage(text, isError) {
    var m = $('table-msg');
    if (!text) { m.hidden = true; m.textContent = ''; return; }
    m.hidden = false;
    m.textContent = text;
    m.classList.toggle('error', !!isError);
  }

  function renderHeaders() {
    var ths = document.querySelectorAll('th[data-sort]');
    for (var i = 0; i < ths.length; i++) {
      var th = ths[i];
      var btn = th.querySelector('button');
      if (!btn.dataset.label) { btn.dataset.label = btn.textContent; }
      if (th.dataset.sort === state.sort) {
        th.setAttribute('aria-sort', state.dir === 'asc' ? 'ascending' : 'descending');
        btn.textContent = btn.dataset.label + ' ' + (state.dir === 'asc' ? '▲' : '▼');
      } else {
        th.removeAttribute('aria-sort');
        btn.textContent = btn.dataset.label;
      }
    }
    var hb = document.querySelectorAll('#horizons button');
    for (var j = 0; j < hb.length; j++) {
      hb[j].setAttribute('aria-pressed', hb[j].dataset.horizon === state.horizon ? 'true' : 'false');
    }
    $('explain').textContent = 'Return, peak and worst drop over ' + state.horizon +
      ', in USD, measured from the price 60 seconds after the post.';
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
      setMessage(state.q ? 'No calls match this search.' : 'No calls yet.', false);
    } else {
      setMessage('', false);
    }
  }

  function fetchJSON(url) {
    return fetch(url, { headers: { 'Accept': 'application/json' }, cache: 'no-store' }).then(function (r) {
      if (!r.ok) { throw new Error('HTTP ' + r.status); }
      return r.json();
    });
  }

  // quiet = background refresh: no "Loading…", and the rows stay if it fails.
  function loadCalls(quiet) {
    var seq = ++requestSeq;
    var p = new URLSearchParams();
    if (state.q) { p.set('q', state.q); }
    p.set('sort', state.sort);
    p.set('dir', state.dir);
    p.set('horizon', state.horizon);
    p.set('page', String(state.page));
    p.set('per', String(PER_PAGE));
    if (!quiet && !loadedOnce) { setMessage('Loading…', false); }
    fetchJSON('api/calls?' + p.toString()).then(function (data) {
      if (seq !== requestSeq) { return; } // a newer request is on its way
      loadedOnce = true;
      renderCalls(data);
    }).catch(function (err) {
      if (seq !== requestSeq) { return; }
      var what = err && err.message ? ' (' + err.message + ')' : '';
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
    $('summary-note').textContent = 'One row per token (its first call).' + (repeats > 0
      ? ' ' + fmtInt(repeats) + (repeats === 1 ? ' repeat call is' : ' repeat calls are') + ' not shown.' : '');

    var when = new Date(s.updated_at);
    if (isNaN(when.getTime())) { when = new Date(); }
    $('summary-updated').textContent = 'Updated ' + fmtTime(when);
    $('summary-error').hidden = true;
  }

  function loadSummary() {
    fetchJSON('api/summary').then(renderSummary).catch(function (err) {
      var e = $('summary-error');
      e.hidden = false;
      e.classList.add('error');
      e.textContent = 'Could not refresh the progress' + (err && err.message ? ' (' + err.message + ')' : '') + '.';
      if ($('summary-headline').textContent === 'Loading…') { $('summary-headline').textContent = DASH; }
    });
  }

  function bind() {
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

    $('prev').addEventListener('click', function () {
      if (state.page > 1) { state.page--; loadCalls(false); }
    });
    $('next').addEventListener('click', function () {
      state.page++;
      loadCalls(false);
    });
  }

  bind();
  renderHeaders();
  loadSummary();
  loadCalls(false);
  setInterval(function () {
    if (document.hidden) { return; }
    loadSummary();
    loadCalls(true);
  }, REFRESH_MS);
})();
