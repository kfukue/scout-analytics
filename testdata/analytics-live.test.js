// Tests of the Analytics page's "Model insights" section: the pure
// calculations in frontend/analytics-stats.js (LIVE_RULES, liveRunner,
// joinModelLive, liveBucketStats) and the drawing code in
// frontend/analytics.js (applyLive, loadLive, renderLive and what it draws
// with), over a tiny stand-in for document and the page's data, col and state
// (as testdata/analytics-health.test.js does). A normal case, no outcomes yet,
// the 404 (hidden) and error states, MIN_N greying, a true, false and null
// "passed", the page filters (they apply here), extreme calls (an outcome
// above +100,000%: left out before the join by pageRows, as everywhere on
// the page) and the +1,000% cap on the mean returns; and that load() asks for
// model-live and render() draws the section on every tab. render() calls
// renderLive, so a throw here would blank the page. Run:
// node testdata/analytics-live.test.js [analytics.js] [analytics-stats.js]
// (no dependencies; exit code 1 on any failure). TestAnalyticsLiveNode runs
// it from `go test` when node is on the PATH. This file lives outside
// frontend/, so the website never serves it.
'use strict';

var fs = require('fs');
var path = require('path');
var vm = require('vm');

var S = require(process.argv[3] ? path.resolve(process.argv[3]) : '../frontend/analytics-stats.js');
var src = fs.readFileSync(process.argv[2] || path.join(__dirname, '..', 'frontend', 'analytics.js'), 'utf8');

// The source of the function declaration called name (up to its closing
// brace; the functions taken have no braces in their strings or regexps).
function take(name) {
  var start = src.indexOf('  function ' + name + '(');
  if (start < 0) { throw new Error('analytics.js: function ' + name + ' not found'); }
  var open = src.indexOf('{', start);
  var depth = 0;
  for (var i = open; i < src.length; i++) {
    if (src[i] === '{') { depth++; }
    if (src[i] === '}') { depth--; if (depth === 0) { return src.slice(start, i + 1); } }
  }
  throw new Error('analytics.js: function ' + name + ' has no end');
}

// The source of the top-level "var name = …;" (up to the first semicolon
// outside brackets).
function takeVar(name) {
  var start = src.indexOf('  var ' + name + ' = ');
  if (start < 0) { throw new Error('analytics.js: var ' + name + ' not found'); }
  var depth = 0;
  for (var i = start; i < src.length; i++) {
    var c = src[i];
    if (c === '[' || c === '{' || c === '(') { depth++; }
    if (c === ']' || c === '}' || c === ')') { depth--; }
    if (c === ';' && depth === 0) { return src.slice(start, i + 1); }
  }
  throw new Error('analytics.js: var ' + name + ' has no end');
}

var failures = 0;
var count = 0;
function check(name, got, want) {
  count++;
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    failures++;
    console.error('FAIL ' + name + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
  }
}

// ---- The pure calculations -------------------------------------------------

// The runner rules mirror ml/scout_ml/config.py BUCKETS (before tax)
check('rules', S.LIVE_RULES, {
  short: { col: 'peak', op: '>=', at: 100 }, '3day': { col: 'ret', op: '>=', at: 50 },
  medium: { col: 'ret', op: '>=', at: 50 }, long: { col: 'ret', op: '>', at: 0 }
});
check('top rank', S.LIVE_TOP_RANK, 90);
check('runner short: 100 yes, 99.9 no', [S.liveRunner(100, S.LIVE_RULES.short), S.liveRunner(99.9, S.LIVE_RULES.short)], [true, false]);
check('runner 3day: 50 yes, 49.9 no', [S.liveRunner(50, S.LIVE_RULES['3day']), S.liveRunner(49.9, S.LIVE_RULES['3day'])], [true, false]);
check('runner medium: 50 yes, 49.9 no', [S.liveRunner(50, S.LIVE_RULES.medium), S.liveRunner(49.9, S.LIVE_RULES.medium)], [true, false]);
check('runner long: 0 no, 0.1 yes', [S.liveRunner(0, S.LIVE_RULES.long), S.liveRunner(0.1, S.LIVE_RULES.long)], [false, true]);
check('runner: missing value', [S.liveRunner(NaN, S.LIVE_RULES.long), S.liveRunner(null, S.LIVE_RULES.short)], [false, false]);

var BUCKETS = ['short', '3day', 'medium', 'long'];
var LIVE_COLUMNS = ['call_id', 'version', 'after_s'];
BUCKETS.forEach(function (b) { LIVE_COLUMNS.push('runner_' + b, 'collapse_' + b, 'rank_' + b, 'passed_' + b); });
function liveBody(rows, versions) {
  return {
    format: 1, buckets: BUCKETS, horizons: ['1d', '3d', '7d', '30d'], first_calls: 0, scored: rows.length,
    versions: versions || ['vB', 'vA'], columns: LIVE_COLUMNS, rows: rows
  };
}

// joinModelLive: matched by call id in the page rows' order; a bucket
// without a runner probability is null; live rows not among the page rows
// are left out
var jb = liveBody([
  [7, 1, 60, 0.3, 0.1, 95, true, null, null, null, null, 0.2, 0.1, 40, false, 0.1, 0.05, null, null],
  [5, 0, 120, null, null, null, null, 0.5, 0.2, 91, null, null, null, null, null, null, null, null, null],
  [999, 0, 1, 0.1, 0.1, 10, true, null, null, null, null, null, null, null, null, null, null, null, null]
]);
var jr = S.joinModelLive(jb, [[5, 'a'], [6, 'b'], [7, 'c']], 0);
check('join: one record per scored page row, in row order', jr.map(function (j) { return j.row[1]; }), ['a', 'c']);
check('join: record of call 5', [jr[0].version, jr[0].after, jr[0].b], ['vB', 120, {
  short: null, '3day': { prob: 0.5, rank: 91, passed: null }, medium: null, long: null
}]);
check('join: record of call 7', [jr[1].version, jr[1].after, jr[1].b], ['vA', 60, {
  short: { prob: 0.3, rank: 95, passed: true }, '3day': null, medium: { prob: 0.2, rank: 40, passed: false },
  long: { prob: 0.1, rank: null, passed: null }
}]);
check('join: by the column names (other order)', S.joinModelLive({
  buckets: ['short'], versions: ['v'], columns: ['version', 'rank_short', 'call_id', 'runner_short', 'passed_short', 'after_s'],
  rows: [[0, 92, 3, 0.4, false, 9]]
}, [[3]], 0)[0].b, { short: { prob: 0.4, rank: 92, passed: false } });
check('join: none', S.joinModelLive(jb, [[1]], 0), []);

// liveBucketStats
function rec(t, rank, x, ret, known, version, passed) {
  return {
    t: t, rank: rank, passed: passed === undefined ? true : passed, version: version || 'v1', known: known !== false,
    x: x, ret: ret
  };
}
var st = S.liveBucketStats([
  rec(30, 95, 150, 40), rec(20, 90, 100, 20), rec(25, 89.9, 200, 60), rec(40, null, 100, 0), rec(10, 50, 10, -20),
  rec(5, 99, 500, 300, false)
], S.LIVE_RULES.short);
check('stats: counts', [st.scored, st.known, st.top.n, st.top.k, st.all.n, st.all.k], [6, 5, 2, 2, 5, 4]);
check('stats: means over the known calls', [st.top.mean, st.all.mean], [30, 20]);
check('stats: lift = top rate ÷ all rate', st.lift, 1 / 0.8);
check('stats: first call (known or not)', st.firstT, 5);
var empty = S.liveBucketStats([], S.LIVE_RULES.short);
check('stats: empty', [empty.scored, empty.known, empty.top, empty.all, isNaN(empty.lift), isNaN(empty.firstT), empty.version,
  empty.versionN, empty.passed], [0, 0, { n: 0, k: 0, mean: null }, { n: 0, k: 0, mean: null }, true, true, null, 0, null]);
// The cap (ml/scout_ml/config.py SIM_MAX_RET_PCT): each mean return is
// capped at cap (the page passes PL_CAP), the runner rule reads the uncapped
// value. Extreme calls (MAX_OUTCOME_PCT) are the page's (pageRows, below):
// one shared cap, no second constant here
check('one outcome cap: no LIVE_MAX_OUTCOME_PCT', [typeof S.LIVE_MAX_OUTCOME_PCT, S.MAX_OUTCOME_PCT], ['undefined', 1e5]);
var capRecs = [rec(1, 95, 5000, 5000), rec(2, 50, 0, 0)];
var capped = S.liveBucketStats(capRecs, S.LIVE_RULES['3day'], 1000);
check('stats: +5000% capped at +1000%',
  [capped.scored, capped.known, capped.top.n, capped.top.k, capped.all.n, capped.all.k, capped.top.mean, capped.all.mean],
  [2, 2, 1, 1, 2, 1, 1000, 500]);
var uncapped = S.liveBucketStats(capRecs, S.LIVE_RULES['3day']);
check('stats: no cap given: not capped', [uncapped.top.mean, uncapped.all.mean], [5000, 2500]);
var noRunner = S.liveBucketStats([rec(1, 95, 10, 0), rec(2, 50, 10, 0)], S.LIVE_RULES.short);
check('stats: no runner at all: no lift', [noRunner.top.k, noRunner.all.k, isNaN(noRunner.lift)], [0, 0, true]);
var noTop = S.liveBucketStats([rec(1, 50, 150, 0), rec(2, 50, 10, 0)], S.LIVE_RULES.short);
check('stats: no top 10%: no lift', [noTop.top.n, isNaN(noTop.lift)], [0, true]);
var zeroTop = S.liveBucketStats([rec(1, 95, 10, 0), rec(2, 50, 150, 0)], S.LIVE_RULES.short);
check('stats: top with no runner: lift 0', zeroTop.lift, 0);
// passed: of the version that scored most of the calls, not the first one
var votes = S.liveBucketStats([rec(1, 50, 0, 0, true, 'old', false), rec(2, 50, 0, 0, true, 'new', true),
  rec(3, 50, 0, 0, false, 'new', true)], S.LIVE_RULES.long);
check('stats: most frequent version', [votes.version, votes.versionN, votes.passed], ['new', 2, true]);
var tie = S.liveBucketStats([rec(1, 50, 0, 0, true, 'old', null), rec(2, 50, 0, 0, true, 'new', true)], S.LIVE_RULES.long);
check('stats: a tie goes to the version counted first', [tie.version, tie.passed], ['old', null]);

// ---- The drawing code ------------------------------------------------------

// A node of the stand-in document: its tag, class, title, scope, colSpan,
// hidden and children. A fragment's children move into the node it is
// appended to.
function Node(tag) {
  this.tagName = tag; this.className = ''; this.title = ''; this.scope = ''; this.colSpan = 1; this.hidden = false;
  this.children = []; this._text = '';
  var self = this;
  this.classList = {
    add: function (c) { if (!this.contains(c)) { self.className = self.className ? self.className + ' ' + c : c; } },
    contains: function (c) { return self.className.split(/\s+/).indexOf(c) >= 0; }
  };
}
Object.defineProperty(Node.prototype, 'textContent', {
  get: function () { return this._text + this.children.map(function (c) { return c.textContent; }).join(''); },
  set: function (v) { this._text = String(v); this.children = []; }
});
Node.prototype.appendChild = function (c) {
  if (c.tagName === '#fragment') {
    var moved = c.children;
    c.children = [];
    moved.forEach(function (m) { this.children.push(m); }, this);
    return c;
  }
  this.children.push(c);
  return c;
};
Node.prototype.replaceChildren = function () {
  this.children = [];
  this._text = '';
  for (var i = 0; i < arguments.length; i++) { this.appendChild(arguments[i]); }
};

// The section as analytics.html has it: hidden until model-live answers.
var page = {};
function resetPage() {
  page = {
    'model-insights': new Node('section'), 'ml-error': new Node('p'), 'ml-body': new Node('div'),
    'ml-cover': new Node('p'), 'tbl-ml': new Node('table')
  };
  page['model-insights'].hidden = true;
  page['ml-error'].hidden = true;
  page['ml-body'].hidden = true;
}
var document = {
  createElement: function (tag) { return new Node(tag); },
  createTextNode: function (text) { var n = new Node('#text'); n._text = String(text); return n; },
  createDocumentFragment: function () { return new Node('#fragment'); },
  getElementById: function (id) { return page[id] || null; }
};

var HORIZONS = ['1h', '1d', '3d', '7d', '30d'];
var COLUMNS = ['call_id', 't', 'flags', 'verdict', 'family', 'dex', 'quote', 'trades_24h', 'no_data'];
HORIZONS.forEach(function (h) { COLUMNS.push('ret_' + h, 'peak_' + h, 'dd_' + h); });
var col = {};
COLUMNS.forEach(function (c, i) { col[c] = i; });
var box = {
  document: document, A: S, col: col, data: null, state: null, snapshotMs: NaN,
  live: null, liveEtag: '', liveLoading: false, fetch: null, Promise: Promise, kept: [], keptOf: null
};
vm.createContext(box);
vm.runInContext([takeVar('DASH'), takeVar('MIN_N'), takeVar('PL_CAP'), takeVar('LIVE_FORMAT'), takeVar('FLAG_USD'),
  takeVar('MONTHS'), takeVar('SUPERSCRIPT'), takeVar('isNum'), takeVar('LIVE_HEADS')].concat(['$', 'el', 'fmtInt', 'fmtDay',
  'sciText', 'fmtPct', 'exactPct', 'headRow', 'pctCell', 'rateCell', 'fewRateCell', 'verdictKey', 'familyKey', 'isUSD',
  'isRugged', 'metricAt', 'now', 'isQuiet', 'passQuiet', 'passVerdict', 'passFamily', 'liveRuleText', 'liveError',
  'applyLive', 'loadLive', 'pageRows', 'liveRecs', 'liveCoverText', 'liveMeanCell', 'liveLiftCell', 'liveValidCell', 'liveRow',
  'renderLive'].map(take)).join('\n'), box);

var VERDICTS = ['clean', 'caution', 'red_flags', 'unknown', 'none'];
var FAMILIES = ['v4', 'v2'];
var NOW = Date.UTC(2026, 9, 14); // 14 Oct 2026 00:00 UTC

// A page row: USD and tracked, the same peak and return at every window
// (o.at3d: a different return at 3d and 7d), verdict, family and trades.
function anaRow(id, t, peak, ret, o) {
  o = o || {};
  var r = [id, t, o.flags === undefined ? 5 : o.flags, o.verdict || 0, o.family || 0, -1, -1,
    o.trades === undefined ? 100 : o.trades, 0];
  HORIZONS.forEach(function (h) {
    var v = o.at3d !== undefined && (h === '3d' || h === '7d') ? o.at3d : ret;
    r.push(v, peak, -10);
  });
  return r;
}
// A model-live row with the same rank in every bucket; passed per bucket.
function liveRow(id, version, rank, passed, after) {
  var s = [id, version, after === undefined ? 7200 : after];
  BUCKETS.forEach(function (b, i) {
    if (passed[i] === undefined) { s.push(null, null, null, null); return; }
    s.push(0.2, 0.1, rank, passed[i]);
  });
  return s;
}

// The normal case: 55 first calls from 23:00 UTC on 1 Sep 2026 (every window
// over), 50 scored (all after 2 h: the first score is on 2 Sep), in the order:
//   G 10 calls, v2, caution, quiet (10 trades), rank 50, peak 10, return 0, version vA
//   A 10 calls, rank 95, peak 150, return +50
//   B  9 calls, rank 95, peak 99.9, return −20
//   C  1 call,  rank 90 (in the top 10%), peak 100 (a short runner), return +50
//   D  1 call,  rank 89.9 (not in it), peak 150, return +50
//   E  2 calls, rank 50, peak 100, return 0 (not a long runner: 0 is not above 0)
//   F 17 calls, rank 50, peak 10, return 0
//   (A–F: v4, no red flags, 100 trades, version vB)
//   U  5 unscored: 3 v4 / no red flags / 100 trades, 2 v2 / caution / quiet
// passed: short false, 3day true, medium null, long vA false and vB true.
var T0 = Date.UTC(2026, 8, 1, 23) / 1000;
var NORMAL = [], NORMAL_LIVE = [];
var nextId = 1;
function add(n, rank, peak, ret, version, o) {
  for (var i = 0; i < n; i++) {
    var id = nextId++;
    NORMAL.push(anaRow(id, T0 + id, peak, ret, o));
    if (version !== null) {
      NORMAL_LIVE.push(liveRow(id, version, rank, [false, true, null, version === 1 ? false : true]));
    }
  }
}
var V2_QUIET = { family: 1, verdict: 1, trades: 10 };
add(10, 50, 10, 0, 1, V2_QUIET);
add(10, 95, 150, 50, 0);
add(9, 95, 99.9, -20, 0);
add(1, 90, 100, 50, 0);
add(1, 89.9, 150, 50, 0);
add(2, 50, 100, 0, 0);
add(17, 50, 10, 0, 0);
add(3, null, 0, 0, null);
add(2, null, 0, 0, null, V2_QUIET);
NORMAL_LIVE.push(liveRow(9999, 0, 99, [true, true, true, true])); // not a page row: left out

function setPage(rows, st, snap) {
  box.data = {
    verdicts: VERDICTS, families: FAMILIES, quiet_below: 50, horizons: HORIZONS,
    horizon_seconds: [3600, 86400, 3 * 86400, 7 * 86400, 30 * 86400], rows: rows
  };
  box.state = Object.assign({ quiet: 'all', family: 'all', verdict: 'all' }, st || {});
  box.snapshotMs = snap === undefined ? NOW : snap;
}

// Draws the section for these rows and model-live rows on a fresh page
// (model-live answering 200); false when it threw.
function draw(name, rows, liveRows, st, snap) {
  resetPage();
  box.live = null;
  box.liveEtag = '';
  setPage(rows, st, snap);
  try {
    check(name + ': applyLive 200 draws', box.applyLive(200, liveBody(liveRows), 'W/"m1"'), true);
    box.renderLive();
  } catch (e) {
    check(name + ': does not throw', String(e && e.stack || e), null);
    return false;
  }
  check(name + ': does not throw', null, null);
  return true;
}
function tbody() { return page['tbl-ml'].children[1] || new Node('missing'); }
function head() { var t = page['tbl-ml'].children[0]; return (t && t.children[0]) || new Node('missing'); }
function texts(tr) { return tr.children.map(function (c) { return c.textContent; }); }
function rowTexts() { return tbody().children.map(texts); }
function classes() { return tbody().children.map(function (tr) { return tr.children.map(function (c) { return c.className; }); }); }
function cell(r, c) { var tr = tbody().children[r]; return (tr && tr.children[c]) || new Node('missing'); }

var HEADS = ['Bucket', 'Scored', 'Outcome known', 'Top 10%', 'Runner rate, top 10%', 'Runner rate, all', 'Live lift',
  'Mean return, top 10%', 'Mean return, all', 'Validated'];
var PLAIN = ['tok', 'num', 'num', 'num', 'num', 'num', 'num', 'num pos', 'num pos', 'num'];

// ---- Normal ------------------------------------------------------------------
if (draw('normal', NORMAL, NORMAL_LIVE)) {
  check('normal: shown', [page['model-insights'].hidden, page['ml-body'].hidden, page['ml-error'].hidden], [false, false, true]);
  check('normal: coverage', page['ml-cover'].textContent,
    '50 of 55 first calls (after the page filters) scored at call time (first score 2 Sep 2026).');
  check('normal: table parts', page['tbl-ml'].children.map(function (c) { return c.tagName; }), ['thead', 'tbody']);
  check('normal: head', texts(head()), HEADS);
  check('normal: rows', rowTexts(), [
    ['short (1d)', '50', '50', '20', '55.0%11 / 20', '28.0%14 / 50', '2.0×', '+18.5%', '+8.4%', 'n.v.'],
    ['3day (3d)', '50', '50', '20', '55.0%11 / 20', '24.0%12 / 50', '2.3×', '+18.5%', '+8.4%', 'yes'],
    ['medium (7d)', '50', '50', '20', '55.0%11 / 20', '24.0%12 / 50', '2.3×', '+18.5%', '+8.4%', '–'],
    ['long (30d)', '50', '50', '20', '55.0%11 / 20', '24.0%12 / 50', '2.3×', '+18.5%', '+8.4%', 'yes']
  ]);
  check('normal: no grey at 20 or more', classes(), [PLAIN, PLAIN, PLAIN, PLAIN]);
  check('normal: row headers', [cell(0, 0).tagName, cell(0, 0).scope], ['th', 'row']);
  check('normal: rule titles', [0, 1, 2, 3].map(function (r) { return cell(r, 0).title; }), [
    'Runner = peak within 1d of +100.0% or more, before tax', 'Runner = return at 3d of +50.0% or more, before tax',
    'Runner = return at 7d of +50.0% or more, before tax', 'Runner = return at 30d above 0.0%, before tax'
  ]);
  check('normal: n.v. as the call list marks it', [cell(0, 9).children[0].className, cell(0, 9).title],
    ['model-nv', 'Not validated: this model did not pass its validation gates (model vB, 40 of 50 calls)']);
  check('normal: validated titles', [cell(1, 9).title, cell(2, 9).title, cell(3, 9).title], [
    'Validated: the 3day model passed its gates (model vB, 40 of 50 calls)', 'Validation not recorded (model vB, 40 of 50 calls)',
    'Validated: the long model passed its gates (model vB, 40 of 50 calls)'
  ]);
  check('normal: top 10% and mean titles', [head().children[3].title, head().children[8].title], [
    'Calls with an outcome in the top 10%. Top 10% = scored above 90% of the calls the model was tested on when it was trained ' +
    '(so the share can drift from 10%).',
    'Average return over the window of all scored calls with an outcome, each call capped at +1,000% ' +
    '(as on "$100 on every call" and in the model\'s money simulation)'
  ]);
  check('normal: lift title', cell(0, 6).title, 'Runner rate of the top 10% ÷ runner rate of all');
  check('normal: mean title', cell(0, 8).title, 'Mean over 50 calls');
  // drawn again: replaced, not appended to
  box.renderLive();
  check('redrawn: four rows', tbody().children.length, 4);
}

// ---- The page filters apply ---------------------------------------------------
if (draw('family v2', NORMAL, NORMAL_LIVE, { family: 'v2' })) {
  check('family v2: coverage', page['ml-cover'].textContent,
    '10 of 12 first calls (after the page filters) scored at call time (first score 2 Sep 2026).');
  check('family v2: rows', rowTexts(), [
    ['short (1d)', '10', '10', '0', '–', '0.0%0 / 10', '–', '–', '0.0%', 'n.v.'],
    ['3day (3d)', '10', '10', '0', '–', '0.0%0 / 10', '–', '–', '0.0%', 'yes'],
    ['medium (7d)', '10', '10', '0', '–', '0.0%0 / 10', '–', '–', '0.0%', '–'],
    ['long (30d)', '10', '10', '0', '–', '0.0%0 / 10', '–', '–', '0.0%', 'n.v.']
  ]);
  var FEW = ['tok', 'num', 'num', 'num', 'num', 'num ana-few', 'num', 'num', 'num zero ana-few', 'num'];
  check('family v2: greying', classes(), [FEW, FEW, FEW, FEW]);
  check('family v2: grey titles', [cell(0, 5).title, cell(0, 8).title],
    ['0 of 10 scored calls with an outcome were runners (fewer than 20)', 'Mean over 10 calls (fewer than 20)']);
}
if (draw('no red flags', NORMAL, NORMAL_LIVE, { verdict: 'clean' })) {
  check('no red flags: coverage', page['ml-cover'].textContent,
    '40 of 43 first calls (after the page filters) scored at call time (first score 2 Sep 2026).');
  check('no red flags: short', rowTexts()[0], ['short (1d)', '40', '40', '20', '55.0%11 / 20', '35.0%14 / 40', '1.6×', '+18.5%', '+10.5%', 'n.v.']);
  check('no red flags: long (only vB left)', rowTexts()[3][9], 'yes');
}
if (draw('quiet excluded', NORMAL, NORMAL_LIVE, { quiet: 'exclude' })) {
  check('quiet excluded: coverage', page['ml-cover'].textContent,
    '40 of 43 first calls (after the page filters) scored at call time (first score 2 Sep 2026).');
  check('quiet excluded: 3day', rowTexts()[1].slice(1, 6), ['40', '40', '20', '55.0%11 / 20', '30.0%12 / 40']);
}
if (draw('only quiet', NORMAL, NORMAL_LIVE, { quiet: 'only' })) {
  check('only quiet: coverage', page['ml-cover'].textContent,
    '10 of 12 first calls (after the page filters) scored at call time (first score 2 Sep 2026).');
}
if (draw('nothing passes', NORMAL, NORMAL_LIVE, { family: 'v2', verdict: 'clean' })) {
  check('nothing passes: coverage', page['ml-cover'].textContent, '0 of 0 first calls (after the page filters) scored at call time.');
  check('nothing passes: rows', rowTexts()[0], ['short (1d)', '0', '0', '0', '–', '–', '–', '–', '–', '–']);
}

// ---- MIN_N greying: 10 calls, 5 in the top 10% ---------------------------------
var SMALL = [], SMALL_LIVE = [];
for (var i = 1; i <= 10; i++) {
  SMALL.push(anaRow(i, T0 + i, i <= 5 ? 150 : 10, i <= 5 ? 50 : 0));
  SMALL_LIVE.push(liveRow(i, 0, i <= 5 ? 95 : 50, [true, true, true, true]));
}
if (draw('few', SMALL, SMALL_LIVE)) {
  check('few: short', rowTexts()[0], ['short (1d)', '10', '10', '5', '100.0%5 / 5', '50.0%5 / 10', '2.0×', '+50.0%', '+25.0%', 'yes']);
  check('few: every number grey', classes()[0],
    ['tok', 'num', 'num', 'num', 'num ana-few', 'num ana-few', 'num ana-few', 'num pos ana-few', 'num pos ana-few', 'num']);
  check('few: lift title', cell(0, 6).title, 'Runner rate of the top 10% ÷ runner rate of all (fewer than 20 calls)');
}
// 20 in the top 10% but 19 in all is impossible; 20 top and 25 all: no grey;
// 19 top: the top cells and the lift grey, the all cells not
var MIX = [], MIX_LIVE = [];
for (var j = 1; j <= 25; j++) {
  MIX.push(anaRow(j, T0 + j, 150, 50));
  MIX_LIVE.push(liveRow(j, 0, j <= 19 ? 95 : 50, [true, true, true, true]));
}
if (draw('19 top', MIX, MIX_LIVE)) {
  check('19 top: greying', classes()[0],
    ['tok', 'num', 'num', 'num', 'num ana-few', 'num', 'num ana-few', 'num pos ana-few', 'num pos', 'num']);
}

// ---- Extreme calls and the +1,000% cap ---------------------------------------------
// 1: a drained pool: +3.9e47% in its 30d return only (every other value
//    normal): left out of the whole page (pageRows), so neither scored nor
//    counted among the first calls here, in every bucket (1d included)
// 2: +5000% everywhere (peak 150): a runner in every bucket, its return
//    capped at +1,000% in the means
// 3: nothing (peak 10, return 0); 4: a 1d peak of exactly +100,000% (not above
//    the cap: kept, a short runner)
var WILD = [anaRow(1, T0, 150, 50), anaRow(2, T0 + 1, 150, 5000), anaRow(3, T0 + 2, 10, 0), anaRow(4, T0 + 3, 1e5, 0)];
WILD[0][col.ret_30d] = 3.9e47;
var WILD_LIVE = [liveRow(1, 0, 95, [true, true, true, true]), liveRow(2, 0, 95, [true, true, true, true]),
  liveRow(3, 0, 50, [true, true, true, true]), liveRow(4, 0, 50, [true, true, true, true])];
if (draw('extreme', WILD, WILD_LIVE)) {
  check('extreme: not among the calls, no clause (the page-wide note has it)', page['ml-cover'].textContent,
    '3 of 3 first calls (after the page filters) scored at call time (first score 2 Sep 2026).');
  check('extreme: not scored, never an outcome; +5000% capped', rowTexts(), [
    ['short (1d)', '3', '3', '1', '100.0%1 / 1', '66.7%2 / 3', '1.5×', '+1,000%', '+333.3%', 'yes'],
    ['3day (3d)', '3', '3', '1', '100.0%1 / 1', '33.3%1 / 3', '3.0×', '+1,000%', '+333.3%', 'yes'],
    ['medium (7d)', '3', '3', '1', '100.0%1 / 1', '33.3%1 / 3', '3.0×', '+1,000%', '+333.3%', 'yes'],
    ['long (30d)', '3', '3', '1', '100.0%1 / 1', '33.3%1 / 3', '3.0×', '+1,000%', '+333.3%', 'yes']
  ]);
  var wildRows = rowTexts();
}
// without the drained call at all: exactly the same section
if (draw('not extreme', WILD.slice(1), WILD_LIVE.slice(1))) {
  check('not extreme: coverage', page['ml-cover'].textContent,
    '3 of 3 first calls (after the page filters) scored at call time (first score 2 Sep 2026).');
  check('not extreme: same rows as with it', rowTexts(), wildRows);
}

// ---- No outcomes yet: called 2 days before now ----------------------------------
// The 3d and 7d returns are there (as if early) but their windows are not over,
// so they are not outcomes; the long bucket has no score at all.
var TN = Date.UTC(2026, 9, 12, 6) / 1000;
var RECENT = [anaRow(1, TN + 10, 150, 50), anaRow(2, TN, 10, 0), anaRow(3, TN + 20, 10, 0)];
var RECENT_LIVE = [1, 2, 3].map(function (id) { return liveRow(id, 0, 95, [true, null, false, undefined], 60); });
if (draw('recent', RECENT, RECENT_LIVE)) {
  check('recent: coverage', page['ml-cover'].textContent,
    '3 of 3 first calls (after the page filters) scored at call time (first score 12 Oct 2026).');
  check('recent: rows', rowTexts(), [
    ['short (1d)', '3', '3', '3', '33.3%1 / 3', '33.3%1 / 3', '1.0×', '+16.7%', '+16.7%', 'yes'],
    ['3day (3d)', '3', '0 with an outcome yet; first 3d outcomes from 15 Oct 2026', '–'],
    ['medium (7d)', '3', '0 with an outcome yet; first 7d outcomes from 19 Oct 2026', 'n.v.'],
    ['long (30d)', '0', '0', '0', '–', '–', '–', '–', '–', '–']
  ]);
  check('recent: the message spans the outcome columns', [cell(1, 2).tagName, cell(1, 2).className, cell(1, 2).colSpan],
    ['td', 'muted', 7]);
}
// a window that is over without the value (the tracker has not recorded it):
// not an outcome either
if (draw('due, no data', [anaRow(1, T0, null, null)], [liveRow(1, 0, 95, [true, true, true, true])])) {
  check('due, no data: short', rowTexts()[0], ['short (1d)', '1', '0 with an outcome yet; first 1d outcomes from 2 Sep 2026', 'yes']);
}
// not priced in USD: no outcome
if (draw('not USD', [anaRow(1, T0, 150, 50, { flags: 4 })], [liveRow(1, 0, 95, [true, true, true, true])])) {
  check('not USD: short', rowTexts()[0].slice(0, 3), ['short (1d)', '1', '0 with an outcome yet; first 1d outcomes from 2 Sep 2026']);
}
// short needs both the peak and the return (as the training label)
if (draw('short without return', [anaRow(1, T0, 150, null)], [liveRow(1, 0, 95, [true, true, true, true])])) {
  check('short without return: no outcome', rowTexts()[0][2], '0 with an outcome yet; first 1d outcomes from 2 Sep 2026');
}

// ---- What model-live answered -----------------------------------------------------
resetPage();
setPage(NORMAL);
box.live = null;
check('404: nothing to draw again', box.applyLive(404), true);
box.renderLive();
check('404: the whole section hidden, no error', [page['model-insights'].hidden, page['ml-error'].hidden, page['ml-body'].hidden],
  [true, true, true]);
box.applyLive(200, liveBody(NORMAL_LIVE), 'W/"a"');
box.renderLive();
check('200 after 404: shown', [page['model-insights'].hidden, page['ml-body'].hidden, box.liveEtag], [false, false, 'W/"a"']);
var drawn = tbody();
check('304: not drawn again', box.applyLive(304), false);
check('304: same body', [box.live !== null, box.liveEtag, tbody() === drawn], [true, 'W/"a"', true]);
check('500: draw again', box.applyLive(500), true);
box.renderLive();
check('500: one muted line, no table', [page['model-insights'].hidden, page['ml-error'].hidden, page['ml-error'].textContent,
  page['ml-body'].hidden, box.live, box.liveEtag],
[false, false, 'Model scores could not be loaded (HTTP 500).', true, null, '']);
box.applyLive(0);
check('no answer', page['ml-error'].textContent, 'Model scores could not be loaded (no answer).');
var future = liveBody(NORMAL_LIVE);
future.format = 2;
box.applyLive(200, future, 'W/"b"');
check('unknown format', [page['ml-error'].textContent, box.live], ['Model scores could not be loaded (unexpected format).', null]);
var noBuckets = liveBody(NORMAL_LIVE);
delete noBuckets.buckets;
box.applyLive(200, noBuckets, 'W/"b"');
check('no buckets', page['ml-error'].textContent, 'Model scores could not be loaded (unexpected format).');
box.applyLive(200, liveBody(NORMAL_LIVE), 'W/"c"');
check('200 after an error: the line goes', [page['ml-error'].hidden, page['ml-error'].textContent], [true, '']);
check('404 after 200: hidden again, body dropped', [box.applyLive(404), page['model-insights'].hidden, box.live], [true, true, null]);
// before /api/analytics is read: the section shows, without a table
resetPage();
box.data = null;
box.applyLive(200, liveBody(NORMAL_LIVE), 'W/"d"');
box.renderLive();
check('no analytics yet: no table', [page['model-insights'].hidden, page['ml-body'].hidden], [false, true]);

// ---- The hooks: load() asks for model-live with /api/analytics, and render()
// draws the section on every tab (so the page filters apply at once) ---------------
var hooks = { loadLive: 0, renderLive: 0, note: 0, fetched: [] };
var hookFake = {
  hidden: true, textContent: '',
  querySelectorAll: function () { return []; }, setAttribute: function () {}
};
var hookBox = {
  loading: false, lastLoad: 0, etag: '', data: null, lastDraw: 0, state: null, A: { holdWords: function () { return ''; }, MIN_HELD_2X_S: 3600 },
  performance: { now: function () { return 0; } },
  $: function () { return hookFake; },
  loadLive: function () { hooks.loadLive++; },
  renderLive: function () { hooks.renderLive++; },
  fetch: function (url) { hooks.fetched.push(url); return new Promise(function () {}); }, // never answers
  passQuiet: function () { return true; }, passVerdict: function () { return true; }, passFamily: function () { return true; },
  renderOverview: function () { return 1; }, drawPL: function () {}, renderFactor: function () {}, renderTiming: function () {},
  renderPeak: function () {},
  pageRows: function () { return hookBox.data.rows; }, renderExtremeNote: function () { hooks.note++; }
};
vm.createContext(hookBox);
vm.runInContext([take('load'), take('render')].join('\n'), hookBox);
hookBox.load();
check('load: asks for model-live too', [hooks.loadLive, hooks.fetched], [1, ['api/analytics']]);
hookBox.load(); // still loading: nothing asked again
check('load: not again while loading', [hooks.loadLive, hooks.fetched.length], [1, 1]);
hookBox.data = { horizons: ['1h', '1d'], rows: [[1]] };
['overview', 'factor', 'timing', 'peak'].forEach(function (tab, i) {
  hookBox.state = { horizon: '1d', k: '4', tab: tab };
  hookBox.render();
  check('render ' + tab + ': draws the model section', hooks.renderLive, i + 1);
  check('render ' + tab + ': writes the page-wide note', hooks.note, i + 1);
});

// ---- loadLive: the request, its ETag, and what each answer does -------------------
function answer(status, body, tag) {
  return {
    status: status,
    headers: { get: function (n) { return n === 'ETag' ? tag || null : null; } },
    json: function () { return Promise.resolve(body); },
    text: function () { return Promise.resolve(''); }
  };
}
var asked = [];
function fakeFetch(res) {
  box.fetch = function (url, opts) {
    asked.push({ url: url, inm: opts.headers['If-None-Match'] || null, cache: opts.cache });
    return res instanceof Error ? Promise.reject(res) : Promise.resolve(res);
  };
}
(async function () {
  resetPage();
  setPage(NORMAL);
  box.live = null;
  box.liveEtag = '';
  fakeFetch(answer(404));
  await box.loadLive();
  check('load 404: hidden', [page['model-insights'].hidden, page['ml-error'].hidden, box.liveLoading], [true, true, false]);
  fakeFetch(answer(200, liveBody(NORMAL_LIVE), 'W/"m9"'));
  await box.loadLive();
  check('load 200: drawn', [page['model-insights'].hidden, page['ml-body'].hidden, rowTexts().length, box.liveEtag],
    [false, false, 4, 'W/"m9"']);
  var before = tbody();
  fakeFetch(answer(304));
  await box.loadLive();
  check('load 304: asked with the ETag, not drawn again', [asked[asked.length - 1], tbody() === before],
    [{ url: 'api/model-live', inm: 'W/"m9"', cache: 'no-store' }, true]);
  check('load: the first request had no ETag', asked[0], { url: 'api/model-live', inm: null, cache: 'no-store' });
  fakeFetch(answer(503));
  await box.loadLive();
  check('load 503: the muted line', [page['ml-error'].textContent, page['ml-body'].hidden],
    ['Model scores could not be loaded (HTTP 503).', true]);
  fakeFetch(new Error('offline'));
  await box.loadLive();
  check('load, network error', [page['ml-error'].textContent, box.liveLoading], ['Model scores could not be loaded (no answer).', false]);
  check('load: after an error, no ETag sent', asked[asked.length - 1].inm, null);

  if (failures) {
    console.error(failures + ' of ' + count + ' checks failed');
    process.exit(1);
  }
  console.log('ok: ' + count + ' checks');
}()).catch(function (e) {
  console.error('FAIL: ' + (e && e.stack || e));
  process.exit(1);
});
