// Tests of the Model 7d column of the list page (frontend/app.js): modelCell
// and the helpers it uses (the medium bucket's runner probability, "top N%",
// "n.v." for a model that did not pass its gates, a dash without a score, the
// title with the exact values), and the row detail's model list (one line per
// horizon), and in a second sandbox with a small stand-in page: switching
// scores off and on (setModelScores: the column, the legend, open details,
// the sort) and a live score event (onLiveScore and replaceRow: the row and an
// open detail drawn again, with no notice, sound or desktop alert). Run:
// node testdata/app-model.test.js (no
// dependencies; exit code 1 on any failure). TestAppModelNode runs it from
// `go test` when node is on the PATH. app.js is one closure for the browser,
// so the functions are taken from its source by name and run in a sandbox of
// their own, with a tiny stand-in for document; this file lives outside
// frontend/, so the website never serves it.
'use strict';

var fs = require('fs');
var path = require('path');
var vm = require('vm');

var src = fs.readFileSync(path.join(__dirname, '..', 'frontend', 'app.js'), 'utf8');

// The source of the function declaration called name (up to its closing
// brace; the functions taken have no braces in their strings).
function take(name) {
  var start = src.indexOf('  function ' + name + '(');
  if (start < 0) { throw new Error('app.js: function ' + name + ' not found'); }
  var open = src.indexOf('{', start);
  var depth = 0;
  for (var i = open; i < src.length; i++) {
    if (src[i] === '{') { depth++; }
    if (src[i] === '}') { depth--; if (depth === 0) { return src.slice(start, i + 1); } }
  }
  throw new Error('app.js: function ' + name + ' has no end');
}

// A node of the stand-in document: its tag, class, title and children.
function Node(tag) { this.tagName = tag; this.className = ''; this.title = ''; this.children = []; this._text = ''; }
Object.defineProperty(Node.prototype, 'textContent', {
  get: function () { return this._text + this.children.map(function (c) { return c.textContent; }).join(''); },
  set: function (v) { this._text = String(v); this.children = []; }
});
Node.prototype.appendChild = function (c) { this.children.push(c); return c; };

var document = {
  createElement: function (tag) { return new Node(tag); },
  createTextNode: function (text) { var n = new Node('#text'); n._text = String(text); return n; }
};
var box = {
  document: document, DASH: '–', MODEL_COLUMN: 'medium',
  MODEL_BUCKETS: [
    { bucket: 'short', label: '1d' }, { bucket: '3day', label: '3d' },
    { bucket: 'medium', label: '7d' }, { bucket: 'long', label: '30d', up: true }
  ]
};
vm.createContext(box);
vm.runInContext(['el', 'isNum', 'exactText', 'modelOf', 'modelBucket', 'fmtProb', 'exactProb', 'topText', 'afterText',
  'validationText', 'notValidatedNode', 'capitalized', 'modelMissingTitle', 'modelCellTitle', 'modelCell',
  'appendValidation', 'modelBucketText', 'modelBucketItem', 'modelList'].map(take).join('\n'), box);

var failures = 0;
var count = 0;
function check(name, got, want) {
  count++;
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    failures++;
    console.error('FAIL ' + name + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
  }
}

function row(buckets, afterS) {
  return { model: { version: '20261013-1909', scored_at: '2026-10-13T19:12:00Z', after_s: afterS === undefined ? 120 : afterS, buckets: buckets } };
}
function medium(runner, rank, passed, collapse) {
  return { bucket: 'medium', horizon: '7d', runner_prob: runner, collapse_prob: collapse === undefined ? 0.44 : collapse, runner_rank_pct: rank, passed: passed };
}
// the cell's parts: its text, whether it has an n.v. mark, its rank line
function cell(c) {
  var td = box.modelCell(c);
  var nv = td.children.filter(function (n) { return n.className === 'model-nv'; }).length > 0;
  var rank = td.children.filter(function (n) { return n.className === 'model-rank'; }).map(function (n) { return n.textContent; })[0] || '';
  return { text: td.textContent, nv: nv, rank: rank, cls: td.className, title: td.title };
}

var cases = [
  // name, row, text, n.v., rank line, words the title must contain
  ['validated', row([medium(0.3123, 92.5, true)]), '31%top 8%', false, 'top 8%', ['31.23%', 'collapse 44%', 'scored higher than 92.5% of the calls the model was tested on when it was trained', 'Model 20261013-1909', '2 min after the call', 'Validated: the 7d model passed its gates']],
  ['not validated', row([medium(0.3123, 92.5, false)]), '31%n.v.top 8%', true, 'top 8%', ['Not validated: the 7d model did not pass its gates']],
  ['validation not recorded', row([medium(0.05, 10, null)]), '5%top 90%', false, 'top 90%', ['Validation not recorded']],
  ['passed missing', row([medium(0.05, 10)]), '5%top 90%', false, 'top 90%', ['Validation not recorded']],
  ['no rank', row([medium(0.5, null, true)]), '50%', false, '', ['50%']],
  ['top of the list', row([medium(0.99, 99.9, true)]), '99%top 1%', false, 'top 1%', []],
  ['no runner number', row([medium(null, 50, true)]), '–top 50%', false, 'top 50%', []],
  ['other buckets only', row([{ bucket: 'short', runner_prob: 0.4, passed: true }]), '–', false, '', ['no 7d model']],
  ['no score', { model: null }, '–', false, '', ['Not scored']],
  ['no model field', {}, '–', false, '', ['Not scored']],
  ['scored an hour later', row([medium(0.2, 50, true)], 3 * 3600), '20%top 50%', false, 'top 50%', ['3 h after the call']],
  ['not an array', { model: { buckets: 'x' } }, '–', false, '', []]
];
cases.forEach(function (c) {
  var got = cell(c[1]);
  check(c[0] + ': text', got.text, c[2]);
  check(c[0] + ': n.v.', got.nv, c[3]);
  check(c[0] + ': rank', got.rank, c[4]);
  check(c[0] + ': class', got.cls, 'num model');
  c[5].forEach(function (w) { check(c[0] + ': title has ' + w, got.title.indexOf(w) >= 0, true); });
});
check('topText 92.5', box.topText(92.5), 'top 8%');
check('topText 0', box.topText(0), 'top 100%');
check('topText null', box.topText(null), null);
check('afterText 30 s', box.afterText({ after_s: 30 }), '1 min');
check('afterText none', box.afterText({}), '');
check('fmtProb', box.fmtProb(0.005), '1%');
check('capitalized', box.capitalized('not validated'), 'Not validated');

// The row detail's model list (modelList and its helpers): one line per
// horizon in MODEL_BUCKETS order, "(not validated)" / "(validation not
// recorded)" in words, "up" for 30d, "no model" for a bucket the score lacks.
var listed = box.modelList({ buckets: [
  { bucket: 'short', runner_prob: 0.3123, collapse_prob: 0.44, runner_rank_pct: 92.5, passed: false },
  { bucket: 'medium', runner_prob: 0.05, collapse_prob: null, runner_rank_pct: null, passed: null },
  { bucket: 'long', runner_prob: 0.6, collapse_prob: 0.1, runner_rank_pct: 40, passed: true }
] });
check('list class', listed.className, 'model-list');
check('list lines', listed.children.map(function (li) { return li.textContent; }), [
  '1d (not validated): runner 31%, collapse 44% · top 8%',
  '3d: no model',
  '7d (validation not recorded): runner 5%',
  '30d: up 60%, collapse 10% · top 60%'
]);
check('list classes', listed.children.map(function (li) { return li.className; }), ['', 'muted', '', '']);
check('list titles', listed.children.map(function (li) { return li.title; }), [
  'not validated: the 1d model did not pass its gates', '', 'validation not recorded',
  'validated: the 30d model passed its gates'
]);
check('list n.v. mark', listed.children[0].children.filter(function (n) { return n.className === 'model-nv'; }).length, 1);
check('bucketText runner', box.modelBucketText({ label: '7d' }, { runner_prob: 0.2, collapse_prob: 0.3, runner_rank_pct: 50 }),
  ': runner 20%, collapse 30% · top 50%');
check('bucketText up, no rank', box.modelBucketText({ label: '30d', up: true }, { runner_prob: 0.7 }), ': up 70%');

// ---- Scores off and on, and live score events (a second sandbox) ----------

// An element of the stand-in page: enough of the DOM for setModelScores,
// drawModel, colCount, onLiveScore and replaceRow (and setExpanded, findRow).
function Elem(tag) {
  this.tagName = tag; this.className = ''; this.title = ''; this.id = ''; this.hidden = false; this.colSpan = 1;
  this.children = []; this.parentNode = null; this.dataset = {}; this.attrs = {}; this._text = '';
  var self = this;
  this.classList = {
    contains: function (c) { return self.className.split(/\s+/).indexOf(c) >= 0; },
    add: function (c) { if (!self.classList.contains(c)) { self.className = (self.className + ' ' + c).trim(); } },
    remove: function (c) { self.className = self.className.split(/\s+/).filter(function (x) { return x !== c; }).join(' '); },
    toggle: function (c, on) {
      if (on === undefined) { on = !self.classList.contains(c); }
      if (on) { self.classList.add(c); } else { self.classList.remove(c); }
      return on;
    }
  };
}
Object.defineProperty(Elem.prototype, 'textContent', {
  get: function () { return this._text + this.children.map(function (c) { return c.textContent; }).join(''); },
  set: function (v) { this.children.forEach(function (c) { c.parentNode = null; }); this._text = String(v); this.children = []; }
});
Object.defineProperty(Elem.prototype, 'firstChild', { get: function () { return this.children[0] || null; } });
Elem.prototype.appendChild = function (c) { c.parentNode = this; this.children.push(c); return c; };
Elem.prototype.insertBefore = function (c, ref) {
  var i = this.children.indexOf(ref);
  c.parentNode = this;
  if (i < 0) { this.children.push(c); } else { this.children.splice(i, 0, c); }
  return c;
};
Elem.prototype.replaceChild = function (c, old) {
  var i = this.children.indexOf(old);
  if (i < 0) { throw new Error('replaceChild: not a child'); }
  old.parentNode = null; c.parentNode = this; this.children[i] = c;
  return old;
};
Elem.prototype.removeChild = function (c) {
  var i = this.children.indexOf(c);
  if (i >= 0) { this.children.splice(i, 1); c.parentNode = null; }
  return c;
};
Elem.prototype.setAttribute = function (k, v) { this.attrs[k] = String(v); };
Elem.prototype.getAttribute = function (k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; };
Elem.prototype.removeAttribute = function (k) { delete this.attrs[k]; };
// An attribute as a selector sees it (data-* from dataset, hidden from the property).
Elem.prototype.attrOf = function (k) {
  if (k === 'hidden') { return this.hidden ? '' : null; }
  if (k.indexOf('data-') === 0) {
    var dk = k.slice(5).replace(/-([a-z])/g, function (_, ch) { return ch.toUpperCase(); });
    return Object.prototype.hasOwnProperty.call(this.dataset, dk) ? this.dataset[dk] : null;
  }
  return this.getAttribute(k);
};
Elem.prototype.descendants = function () {
  var out = [];
  this.children.forEach(function (c) { out.push(c); out.push.apply(out, c.descendants()); });
  return out;
};
// The selectors app.js uses here: tag, .class, [attr], [attr="v"] and
// :not([hidden]), joined by spaces (descendant).
function matchesCompound(n, comp) {
  var notHidden = comp.indexOf(':not([hidden])') >= 0;
  comp = comp.replace(':not([hidden])', '');
  if (notHidden && n.hidden) { return false; }
  var tag = /^[a-z]+/.exec(comp);
  if (tag && n.tagName !== tag[0]) { return false; }
  var cls = comp.match(/\.[\w-]+/g) || [];
  for (var i = 0; i < cls.length; i++) { if (!n.classList.contains(cls[i].slice(1))) { return false; } }
  var re = /\[([\w-]+)(?:="([^"]*)")?\]/g;
  var a;
  while ((a = re.exec(comp)) !== null) {
    var v = n.attrOf(a[1]);
    if (v === null || (a[2] !== undefined && v !== a[2])) { return false; }
  }
  return true;
}
function matches(n, root, comps) {
  if (!matchesCompound(n, comps[comps.length - 1])) { return false; }
  if (comps.length === 1) { return true; }
  for (var p = n.parentNode; p && p !== root; p = p.parentNode) {
    if (matches(p, root, comps.slice(0, -1))) { return true; }
  }
  return false;
}
Elem.prototype.querySelectorAll = function (sel) {
  var comps = sel.trim().split(/\s+/);
  var root = this;
  return this.descendants().filter(function (n) { return matches(n, root, comps); });
};
Elem.prototype.querySelector = function (sel) { return this.querySelectorAll(sel)[0] || null; };

function mk(tag, cls, props) {
  var n = new Elem(tag);
  if (cls) { n.className = cls; }
  Object.keys(props || {}).forEach(function (k) { n[k] = props[k]; });
  return n;
}

// What the stubs were called with: name → list of argument lists.
var called = {};
function spy(name, ret) {
  return function () { (called[name] = called[name] || []).push([].slice.call(arguments)); return ret; };
}
function times(name) { return (called[name] || []).length; }
// What a call, a verdict or an sAlpha report sets off, and a score must not.
var NOTIFIERS = ['liveNotice', 'showNotice', 'beep', 'desktopNotify'];

// A fresh stand-in page: a header of four columns (one of them Model 7d), the
// Model legend, the rows' tbody with the row of call 5 and its open detail;
// app.js's own functions run in a sandbox of their own on it, with stubs for
// the rest (recorded in called).
function page() {
  called = {};
  var root = mk('html');
  var htr = root.appendChild(mk('thead')).appendChild(mk('tr'));
  htr.appendChild(mk('th', 'tok'));
  htr.appendChild(mk('th', '', { dataset: { sort: 'date' } }));
  var thModel = htr.appendChild(mk('th', 'num model', { dataset: { sort: 'model' } }));
  htr.appendChild(mk('th'));
  var legend = root.appendChild(mk('div', '', { id: 'legend-model' }));
  var rows = root.appendChild(mk('tbody', '', { id: 'rows' }));
  var tr = rows.appendChild(mk('tr', 'call-row', { dataset: { callId: '5' } }));
  var detail = rows.appendChild(mk('tr', 'detail-row', { id: 'detail-5' }));
  var td = detail.appendChild(mk('td', 'detail-cell', { colSpan: 4 }));
  var dbox = td.appendChild(mk('div', 'detail-box'));
  dbox.appendChild(mk('div', 'detail-perf'));
  var model = dbox.appendChild(mk('div', 'detail-model'));
  var old = { call_id: 5, return_pct: 10, peak_pct: 20, drawdown_pct: -5, model: null };
  var ctx = {
    document: {
      createElement: function (tag) { return new Elem(tag); },
      createTextNode: function (text) { var n = new Elem('#text'); n._text = String(text); return n; },
      getElementById: function (id) { return root.descendants().filter(function (n) { return n.id === id; })[0] || null; },
      querySelector: function (sel) { return root.querySelector(sel); },
      querySelectorAll: function (sel) { return root.querySelectorAll(sel); }
    },
    DASH: '–', MODEL_COLUMN: 'medium', MODEL_BUCKETS: box.MODEL_BUCKETS, COLUMNS: 17,
    modelScores: true,
    state: { sort: 'date', dir: 'desc', page: 1, horizon: '7d', verdicts: [] },
    shown: { data: { calls: [old] }, sig: 'old' },
    detailRows: { 5: detail }, openRows: { 5: true }, rowData: { 5: old }, rowReports: { 5: '0:0' },
    filterMatch: true,
    renderHeaders: spy('renderHeaders'), markFresh: spy('markFresh'), callsSignature: spy('callsSignature', 'sig'),
    scheduleQuietLoad: spy('scheduleQuietLoad'), loadCalls: spy('loadCalls'),
    fmtDate: function () { return '13 Oct 19:12'; },
    rowMatchesFilters: function (c) { spy('rowMatchesFilters')(c); return ctx.filterMatch; },
    buildRow: function (c) {
      spy('buildRow')(c);
      var n = mk('tr', 'call-row', { dataset: { callId: String(c.call_id) } });
      n.appendChild(mk('button', 'row-toggle'));
      return n;
    },
    // like the real drawPerf (checked below): recorded, then the model block drawn
    drawPerf: function (key) { spy('drawPerf')(key); ctx.drawModel(key); }
  };
  NOTIFIERS.forEach(function (name) { ctx[name] = spy(name); });
  vm.createContext(ctx);
  vm.runInContext(['$', 'el', 'isNum', 'exactText', 'modelOf', 'modelBucket', 'fmtProb', 'exactProb', 'topText', 'afterText',
    'validationText', 'notValidatedNode', 'appendValidation', 'modelBucketText', 'modelBucketItem', 'modelList', 'modelMeta', 'reportMeta',
    'modelSection', 'drawModel', 'colCount', 'setModelScores', 'callKey', 'reportIds', 'findRow', 'shownIndex',
    'setExpanded', 'replaceRow', 'onLiveScore'].map(take).join('\n'), ctx);
  return { ctx: ctx, thModel: thModel, legend: legend, rows: rows, tr: tr, td: td, model: model, old: old };
}
function score(runner) {
  return { version: 'v2', scored_at: '2026-10-13T19:12:00Z', after_s: 60, buckets: [medium(runner, 90, true)] };
}
function noNotices(name) {
  NOTIFIERS.forEach(function (n) { check(name + ': ' + n + ' not called', times(n), 0); });
}

check('drawPerf draws the model block', take('drawPerf').indexOf('drawModel(key);') >= 0, true);

// setModelScores(false): column and legend hidden, the open detail spans the
// visible columns and has no model block, a Model sort goes back to date ▼.
var p = page();
p.ctx.rowData[5] = { call_id: 5, model: score(0.42) };
p.ctx.drawModel('5');
check('model block drawn', p.model.querySelectorAll('section.report-model').length, 1);
check('colCount with the column', p.ctx.colCount(), 4);
p.ctx.state.sort = 'model';
p.ctx.state.dir = 'asc';
p.ctx.setModelScores(false);
check('off: modelScores', p.ctx.modelScores, false);
check('off: column hidden', p.thModel.hidden, true);
check('off: legend hidden', p.legend.hidden, true);
check('off: colCount', p.ctx.colCount(), 3);
check('off: detail colSpan', p.td.colSpan, 3);
check('off: model block empty', p.model.children.length, 0);
check('off: sort', [p.ctx.state.sort, p.ctx.state.dir], ['date', 'desc']);
check('off: headers drawn again', times('renderHeaders'), 1);
p.ctx.setModelScores(false);
check('off twice: nothing more', times('renderHeaders'), 1);

// setModelScores(true) (and undefined, what an api/calls without
// model_scores gives): everything back.
p.ctx.setModelScores(true);
check('on: modelScores', p.ctx.modelScores, true);
check('on: column shown', p.thModel.hidden, false);
check('on: legend shown', p.legend.hidden, false);
check('on: detail colSpan', p.td.colSpan, 4);
check('on: model block drawn', p.model.querySelectorAll('section.report-model').length, 1);
check('on: model block text', p.model.textContent.indexOf('7d: runner 42%') >= 0, true);
check('on: sort kept', p.ctx.state.sort, 'date');
p.ctx.state.sort = 'peak';
p.ctx.setModelScores(false);
check('off, other sort: kept', [p.ctx.state.sort, p.ctx.state.dir], ['peak', 'desc']);
check('off, other sort: headers not drawn', times('renderHeaders'), 1);
p.ctx.setModelScores(undefined);
check('undefined counts as on', [p.ctx.modelScores, p.thModel.hidden, p.td.colSpan], [true, false, 4]);

// onLiveScore: the row shown is drawn again with the score, and its open
// detail too, with no notice, sound or desktop alert.
p = page();
var fresh = { call_id: 5, perceptor_report_id: 7, return_pct: 11, peak_pct: 21, drawdown_pct: -6, model: score(0.31) };
p.ctx.onLiveScore({ row: fresh, horizon: '7d' });
var shownTr = p.rows.children[0];
check('score: row replaced', shownTr !== p.tr && p.tr.parentNode === null, true);
check('score: new row built from the event', times('buildRow') === 1 && called.buildRow[0][0] === fresh, true);
check('score: new row keyed', shownTr.dataset.callId, '5');
check('score: open row stays open', [shownTr.classList.contains('open'), shownTr.firstChild.getAttribute('aria-expanded')], [true, 'true']);
check('score: highlighted', times('markFresh'), 1);
check('score: rowData', p.ctx.rowData[5] === fresh, true);
check('score: report ids', p.ctx.rowReports[5], '7:0');
check('score: shown calls', p.ctx.shown.data.calls[0] === fresh, true);
check('score: signature', p.ctx.shown.sig, 'sig');
check('score: detail drawn again', called.drawPerf, [['5']]);
check('score: detail has the score', p.model.textContent.indexOf('7d: runner 31%') >= 0, true);
check('score: same window keeps the event numbers', [fresh.return_pct, fresh.peak_pct, fresh.drawdown_pct], [11, 21, -6]);
check('score: no reload', [times('loadCalls'), times('scheduleQuietLoad')], [0, 0]);
noNotices('score');

// An event with another window's numbers keeps the numbers shown.
p = page();
var other = { call_id: 5, return_pct: 99, peak_pct: 99, drawdown_pct: -99, model: score(0.2) };
p.ctx.onLiveScore({ row: other, horizon: '1d' });
check('other window: numbers shown kept', [other.return_pct, other.peak_pct, other.drawdown_pct], [10, 20, -5]);
check('other window: row replaced', p.rows.children[0] !== p.tr, true);
noNotices('other window');

// A closed row: replaced, but no detail drawn.
p = page();
p.ctx.openRows = {};
p.ctx.onLiveScore({ row: { call_id: 5, model: score(0.2) }, horizon: '7d' });
check('closed: row replaced', p.rows.children[0] !== p.tr, true);
check('closed: no detail drawn', times('drawPerf'), 0);
check('closed: not marked open', p.rows.children[0].classList.contains('open'), false);
noNotices('closed');

// Scores off: nothing at all.
p = page();
p.ctx.modelScores = false;
p.ctx.onLiveScore({ row: { call_id: 5, model: score(0.2) }, horizon: '7d' });
check('scores off: row kept', p.rows.children[0] === p.tr, true);
check('scores off: nothing called',
  [times('buildRow'), times('loadCalls'), times('scheduleQuietLoad'), times('rowMatchesFilters'), times('drawPerf')], [0, 0, 0, 0, 0]);
check('scores off: rowData kept', p.ctx.rowData[5] === p.old, true);
noNotices('scores off');

// Sorted by Model 7d: the row may move, so one quiet reload instead.
p = page();
p.ctx.state.sort = 'model';
p.ctx.onLiveScore({ row: { call_id: 5, model: score(0.2) }, horizon: '7d' });
check('model sort: quiet reload', times('scheduleQuietLoad'), 1);
check('model sort: row kept', p.rows.children[0] === p.tr, true);
check('model sort: nothing else', [times('buildRow'), times('loadCalls'), times('drawPerf')], [0, 0, 0]);
noNotices('model sort');

// The row no longer matches the filters (its latest Perceptor verdict, or
// the age filter): the list loads again.
p = page();
p.ctx.filterMatch = false;
p.ctx.onLiveScore({ row: { call_id: 5, model: score(0.2) }, horizon: '7d' });
check('out of filter: list loads', called.loadCalls, [[true]]);
check('out of filter: row kept', [p.rows.children[0] === p.tr, times('buildRow')], [true, 0]);
noNotices('out of filter');

// A row not shown, or an event without a call id: nothing.
p = page();
p.ctx.onLiveScore({ row: { call_id: 9, model: score(0.2) }, horizon: '7d' });
p.ctx.onLiveScore({ row: { call_id: 'x', model: score(0.2) }, horizon: '7d' });
check('not shown: nothing called', [times('buildRow'), times('loadCalls'), times('scheduleQuietLoad'), times('drawPerf')], [0, 0, 0, 0]);
check('not shown: row kept', p.rows.children[0] === p.tr, true);
noNotices('not shown');

if (failures > 0) {
  console.error(failures + ' of ' + count + ' checks failed');
  process.exit(1);
}
console.log('ok: ' + count + ' checks');
