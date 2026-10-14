// Tests of the Analytics page's page-wide rule on extreme calls
// (frontend/analytics.js): render() takes its rows from pageRows, which
// leaves out every call with a return or peak at any window above +100,000%
// (A.isExtremeCall; the cap and strict ">" of ml/scout_ml/labels.py
// extreme_outcome, which checks fewer columns), and writes the page-wide note
// (#ana-extreme, renderExtremeNote) only when it left some out; "Data health
// by week" alone still counts them. The real render, renderOverview (with its
// counts, the "All first calls" table and the data health tables),
// renderPeak (the Peak vs final scatter, its caption and the exit-rule
// table) and the page's own helpers run over the real calculations of
// analytics-stats.js; only the Overview's other charts, the By factor and
// Timing tabs, "$100 on every call" and Model insights are stand-ins that
// record the rows they are given (analytics-live.test.js covers Model
// insights). Run: node testdata/analytics-outliers.test.js [path to
// analytics.js] [path to analytics-stats.js] (no dependencies; exit code 1
// on any failure). TestAnalyticsOutliersNode runs it from `go test` when node
// is on the PATH. analytics.js is one closure for the browser, so the
// functions and constants are taken from its source by name and run in a
// sandbox of their own, with a tiny stand-in for document (as
// testdata/analytics-health.test.js does); this file lives outside
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

// A node of the stand-in document (HTML or SVG): its tag, class, title,
// scope, colSpan, hidden, attributes and children. A fragment's children
// move into the node it is appended to.
function Node(tag) {
  this.tagName = tag; this.className = ''; this.title = ''; this.scope = ''; this.colSpan = 1; this.hidden = false;
  this.children = []; this._text = ''; this.attrs = {};
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
Node.prototype.setAttribute = function (k, v) { this.attrs[k] = String(v); };
Node.prototype.querySelectorAll = function () { return []; };
// Every node of the tree under n with this tag.
function all(n, tag, out) {
  out = out || [];
  n.children.forEach(function (c) { if (c.tagName === tag) { out.push(c); } all(c, tag, out); });
  return out;
}
// The tree under n as plain data, to compare two drawings.
function dump(n) {
  return { tag: n.tagName, cls: n.className, title: n.title, attrs: n.attrs, text: n._text, children: n.children.map(dump) };
}

// The page: every element asked for is made on first use (a fresh page per
// drawing).
var page = {};
var document = {
  createElement: function (tag) { return new Node(tag); },
  createElementNS: function (ns, tag) { return new Node(tag); },
  createTextNode: function (text) { var n = new Node('#text'); n._text = String(text); return n; },
  createDocumentFragment: function () { return new Node('#fragment'); },
  getElementById: function (id) { return page[id] || (page[id] = new Node('div')); }
};

// Stand-ins for the parts this test does not look into: they record the rows
// they are given.
var got = {};
function record(name, ret) {
  return function (rows) { got[name] = rows; return ret; };
}

var HORIZONS = ['1h', '1d', '3d', '7d', '30d'];
var COLUMNS = ['call_id', 't', 'flags', 'verdict', 'family', 'dex', 'quote', 'trades_24h', 'no_data'];
HORIZONS.forEach(function (h) { COLUMNS.push('ret_' + h, 'peak_' + h, 'dd_' + h); });
var col = {};
COLUMNS.forEach(function (c, i) { col[c] = i; });

var box = {
  document: document, A: S, col: col, data: null, state: null, snapshotMs: NaN, lastDraw: 0, kept: [], keptOf: null,
  performance: { now: function () { return 0; } },
  drawTrend: function () {}, drawMix: function () {}, drawHeat: record('heat'), fillMatrix: function () {},
  drawPL: record('pl'), renderFactor: record('factor'), renderTiming: record('timing'),
  renderLive: function () { got.live = (got.live || 0) + 1; }
};
vm.createContext(box);
vm.runInContext([takeVar('DASH'), takeVar('MIN_N'), takeVar('TOP_DEX'), takeVar('TOP_QUOTE'), takeVar('FLAG_USD'),
  takeVar('VERDICT_LABEL'), takeVar('FAMILY_LABEL'), takeVar('MONTHS'), takeVar('DAY_MS'), takeVar('SUPERSCRIPT'),
  takeVar('isNum'), takeVar('METRIC_HEADS'), takeVar('HEALTH_HEADS'), takeVar('EXIT_RULES'), takeVar('EXIT_HEADS'),
  takeVar('pointTips'), takeVar('SVGNS')].concat([
  '$', 'el', 'pad2', 'fmtInt', 'fmtDay', 'fmtCallTime', 'sciText', 'fmtPct', 'exactPct', 'cap',
  'verdictKey', 'familyKey', 'isUSD', 'isRugged', 'metricAt', 'now', 'isQuiet', 'passQuiet', 'passVerdict', 'passFamily',
  'pageRows', 'extremeNoteText', 'renderExtremeNote',
  'newAcc', 'classify', 'add', 'periodStart', 'periodEnd', 'periodLabel', 'group', 'sorted',
  'headRow', 'pctCell', 'rateCell', 'whyNone', 'metricRow', 'emptyRow', 'fillTable', 'stat',
  'healthRecords', 'fewRateCell', 'fillHealthTable', 'fillHealthFamily', 'renderHealth',
  'svgEl', 'svgText', 'tip', 'r1', 'newChart', 'mount', 'emptyChart', 'lin', 'symScale', 'yGrid', 'axisPct', 'linePath',
  'ppCell', 'exitRow', 'fillExitTable', 'renderOverview', 'renderPeak', 'render'].map(take)).join('\n'), box);

var failures = 0;
var count = 0;
function check(name, got_, want) {
  count++;
  if (JSON.stringify(got_) !== JSON.stringify(want)) {
    failures++;
    console.error('FAIL ' + name + ': got ' + JSON.stringify(got_) + ', want ' + JSON.stringify(want));
  }
}

// Rows as /api/analytics sends them: USD and tracked (flags 5), no red
// flags, Uniswap v4, 100 trades; return +10%, peak +50%, drop −10% at every
// window. id 999: the prod drained pool, +3.9×10⁴⁷ % as its return and peak
// at every window.
var T0 = Date.UTC(2026, 8, 1, 12) / 1000; // Tue 1 Sep 2026: week of Mon 31 Aug
var NOW = Date.UTC(2026, 9, 14);          // every window over
function row(id, ret, peak) {
  var r = [id, T0 + id, 5, 0, 0, 0, 0, 100, 0];
  HORIZONS.forEach(function () { r.push(ret, peak, -10); });
  return r;
}
var ORDINARY = [];
for (var i = 1; i <= 25; i++) { ORDINARY.push(row(i, 10, 50)); }
var DRAINED = row(999, 3.9e47, 3.9e47);
// only its 30d return above the cap (one of the ten columns alone)
var LATE = row(998, 10, 50);
LATE[col.ret_30d] = 2e5;
// only its 7d peak above the cap: training keeps it (labels.py reads only
// the 1d peak), but the page checks every window it shows, so it leaves the
// page too
var PEAK7 = row(997, 10, 50);
PEAK7[col.peak_7d] = 2e5;

function body(rows) {
  return {
    verdicts: ['clean', 'caution', 'red_flags', 'unknown', 'none'], families: ['v4'], dexes: ['Uniswap'], quotes: ['WETH'],
    horizons: HORIZONS, horizon_seconds: [3600, 86400, 3 * 86400, 7 * 86400, 30 * 86400], quiet_below: 50, rows: rows
  };
}

// Draws tab with these rows on a fresh page (a new body, as load() reads
// one); false when it threw.
function draw(name, rows, tab, st, keepBody) {
  page = {};
  got = {};
  if (!keepBody) { box.data = body(rows); }
  box.state = Object.assign({
    horizon: '1d', metric: 'ret', stat: 'median', quiet: 'all', period: 'week', family: 'all', verdict: 'all',
    tab: tab, factor: 'verdict', k: '5', path: 'all', tmGroup: 'verdict'
  }, st || {});
  box.snapshotMs = NOW;
  try {
    box.render();
  } catch (e) {
    check(name + ': does not throw', String(e && e.stack || e), null);
    return false;
  }
  check(name + ': does not throw', null, null);
  return true;
}
function ids(rows) { return (rows || []).map(function (r) { return r[col.call_id]; }); }
function counts() {
  return page['ana-counts'].children.map(function (d) { return d.children[0].textContent + ': ' + d.children[1].textContent; });
}
function tableRows(id) {
  var t = page[id].children[1];
  return t ? t.children.map(function (tr) { return tr.children.map(function (c) { return c.textContent; }); }) : [];
}
function note() { return [page['ana-extreme'].hidden, page['ana-extreme'].textContent]; }

var NOTE_1 = '1 call left out everywhere except Data health: an outcome above +100,000% (bad pool data), with the same cap as the model\'s training.';
var ORD_IDS = ORDINARY.map(function (r) { return r[0]; });
var COUNTS = ['Calls: 25', 'With data (1d): 25', 'Not due yet: 0', 'Due, no data: 0', 'Waiting for the tracker: 0', 'Not tracked / no USD: 0'];
var ALL_ROW = ['All first calls', '25', '25', '+10.0%', '+10.0%', '+50.0%', '+50.0%', '−10.0%', '−10.0%',
  '100.0%25 / 25', '0.0%0 / 25', '0.0%0 / 25', '0.0%0 / 25'];

// ---- Overview: without the drained call (the baseline) ---------------------
var base = {};
if (draw('overview, ordinary', ORDINARY, 'overview')) {
  check('ordinary: no note', note(), [true, '']);
  check('ordinary: counts', counts(), COUNTS);
  check('ordinary: all first calls', tableRows('tbl-all'), [ALL_ROW]);
  check('ordinary: data health counts 25', tableRows('tbl-health')[0].slice(0, 2), ['Week of Mon 31 Aug 2026', '25']);
  base.verdict = dump(page['tbl-verdict']);
  base.period = dump(page['tbl-period']);
}

// ---- Overview: with it -----------------------------------------------------
if (draw('overview, drained', ORDINARY.concat([DRAINED]), 'overview')) {
  check('drained: the note', note(), [false, NOTE_1]);
  check('drained: counts as without it', counts(), COUNTS);
  check('drained: means as without it', tableRows('tbl-all'), [ALL_ROW]);
  check('drained: by verdict as without it', dump(page['tbl-verdict']), base.verdict);
  check('drained: by week as without it', dump(page['tbl-period']), base.period);
  check('drained: hour × weekday rows', ids(got.heat), ORD_IDS);
  check('drained: "$100 on every call" rows', ids(got.pl), ORD_IDS);
  check('drained: Model insights drawn', got.live, 1);
  // Data health counts every call, the drained one included
  check('drained: data health counts 26', tableRows('tbl-health')[0].slice(0, 2), ['Week of Mon 31 Aug 2026', '26']);
}
// the page filters set: Data health still counts it, the note still shows
if (draw('drained, filters set', ORDINARY.concat([DRAINED]), 'overview', { quiet: 'exclude', verdict: 'clean', family: 'v4' })) {
  check('filters set: the note', note(), [false, NOTE_1]);
  check('filters set: data health counts 26', tableRows('tbl-health')[0][1], '26');
  check('filters set: calls', counts()[0], 'Calls: 25');
}
// drawn again for a body without it: the note goes
if (draw('overview, drained gone', ORDINARY, 'overview')) {
  check('drained gone: no note', note(), [true, '']);
}

// ---- Two left out (one by its 30d return alone): the plural ----------------
if (draw('two left out', ORDINARY.concat([DRAINED, LATE]), 'overview')) {
  check('two: the note', note(), [false,
    '2 calls left out everywhere except Data health: an outcome above +100,000% (bad pool data), with the same cap as the model\'s training.']);
  check('two: counts', counts()[0], 'Calls: 25');
  check('two: data health counts 27', tableRows('tbl-health')[0][1], '27');
}

// ---- Peak vs final: the scatter, its caption and the exit table ------------
var peakBase = {};
if (draw('peak, ordinary', ORDINARY, 'peak')) {
  check('peak ordinary: caption', page['cap-peak'].textContent.split(',')[0], '25 calls with a peak and a return for 1d');
  check('peak ordinary: points', all(page['ch-peak'], 'circle').length, 25);
  peakBase.chart = dump(page['ch-peak']);
  peakBase.exit = tableRows('tbl-exit');
}
if (draw('peak, drained', ORDINARY.concat([DRAINED]), 'peak')) {
  check('peak drained: the note', note(), [false, NOTE_1]);
  check('peak drained: caption count', page['cap-peak'].textContent.split(',')[0], '25 calls with a peak and a return for 1d');
  check('peak drained: points', all(page['ch-peak'], 'circle').length, 25);
  // the axes are not stretched: the same drawing as without it
  check('peak drained: same chart as without it', dump(page['ch-peak']), peakBase.chart);
  check('peak drained: same exit table', tableRows('tbl-exit'), peakBase.exit);
}
// a 7d peak above the cap (every return below it): left out of the page as
// well, so the 7d scatter, its caption and the exit table are those without
// it
if (draw('peak 7d, ordinary', ORDINARY, 'peak', { horizon: '7d' })) {
  peakBase.chart7 = dump(page['ch-peak']);
  peakBase.exit7 = tableRows('tbl-exit');
  check('peak 7d ordinary: exit n', peakBase.exit7.map(function (r) { return r[1]; }), ['25', '25', '25', '25']);
}
if (draw('peak, 7d peak', ORDINARY.concat([PEAK7]), 'peak', { horizon: '7d' })) {
  check('7d peak: the note', note(), [false, NOTE_1]);
  check('7d peak: caption count', page['cap-peak'].textContent.split(',')[0], '25 calls with a peak and a return for 7d');
  check('7d peak: points', all(page['ch-peak'], 'circle').length, 25);
  check('7d peak: same chart as without it', dump(page['ch-peak']), peakBase.chart7);
  check('7d peak: same exit table as without it', tableRows('tbl-exit'), peakBase.exit7);
}
// and on the Overview at 7d: the mean and median peak are those without it
// (with it the mean peak would be about +7,740%); Data health still counts it
if (draw('overview 7d, 7d peak', ORDINARY.concat([PEAK7]), 'overview', { horizon: '7d' })) {
  check('overview 7d peak: the note', note(), [false, NOTE_1]);
  check('overview 7d peak: calls', counts().slice(0, 2), ['Calls: 25', 'With data (7d): 25']);
  check('overview 7d peak: all first calls (mean peak +50.0%)', tableRows('tbl-all'), [ALL_ROW]);
  check('overview 7d peak: hour × weekday rows', ids(got.heat), ORD_IDS);
  check('overview 7d peak: data health counts 26', tableRows('tbl-health')[0].slice(0, 2), ['Week of Mon 31 Aug 2026', '26']);
}

// ---- Each of the ten columns alone: the call leaves the page --------------
['ret_1h', 'peak_1h', 'ret_1d', 'peak_1d', 'ret_3d', 'peak_3d', 'ret_7d', 'peak_7d', 'ret_30d', 'peak_30d'].forEach(function (name) {
  var r = row(996, 10, 50);
  r[col[name]] = 2e5;
  if (draw('only ' + name, ORDINARY.concat([r]), 'overview')) {
    check(name + ' alone: the note', note(), [false, NOTE_1]);
    check(name + ' alone: calls', counts()[0], 'Calls: 25');
    check(name + ' alone: data health counts 26', tableRows('tbl-health')[0][1], '26');
  }
});
// a worst drop or a factor far above the cap is not an outcome of the rule
if (draw('only dd_7d', ORDINARY.concat([(function () { var r = row(995, 10, 50); r[col.dd_7d] = 2e5; return r; })()]), 'overview')) {
  check('dd_7d alone: no note', note(), [true, '']);
  check('dd_7d alone: calls', counts()[0], 'Calls: 26');
}
// exactly +100,000% at every window stays on the page (strict ">")
if (draw('at the cap', ORDINARY.concat([row(994, 1e5, 1e5)]), 'overview')) {
  check('at the cap: no note', note(), [true, '']);
  check('at the cap: calls', counts()[0], 'Calls: 26');
}

// ---- By factor and Timing get the same rows --------------------------------
if (draw('factor, drained', ORDINARY.concat([DRAINED]), 'factor')) {
  check('factor: rows', ids(got.factor), ORD_IDS);
  check('factor: the note', note(), [false, NOTE_1]);
}
if (draw('timing, drained', ORDINARY.concat([DRAINED]), 'timing')) {
  check('timing: rows', ids(got.timing), ORD_IDS);
}

// ---- Only extreme calls: nothing to show, the note says why ---------------
if (draw('only drained', [DRAINED], 'overview')) {
  check('only drained: the note', note(), [false, NOTE_1]);
  check('only drained: message', [page['ana-msg'].hidden, page['ana-msg'].textContent], [false, 'No calls match this choice.']);
  check('only drained: data health counts 1', tableRows('tbl-health')[0][1], '1');
}

// ---- pageRows: worked out once per body read ------------------------------
box.data = body(ORDINARY.concat([DRAINED]));
var first = box.pageRows();
check('pageRows: the same list while the body is the same', box.pageRows() === first, true);
check('pageRows: data.rows untouched', box.data.rows.length, 26);
box.data = body(ORDINARY.concat([DRAINED]));
check('pageRows: a new body, worked out again', box.pageRows() !== first && box.pageRows().length === 25, true);

// extremeNoteText on its own
check('extremeNoteText 0', box.extremeNoteText(0), '');
check('extremeNoteText 1,234', box.extremeNoteText(1234).slice(0, 23), '1,234 calls left out ev');

if (failures) {
  console.error(failures + ' of ' + count + ' checks failed');
  process.exit(1);
}
console.log('ok: ' + count + ' checks');
