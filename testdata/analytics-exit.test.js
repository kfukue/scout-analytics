// Tests of the Peak vs final tab's exit-rule table (frontend/analytics.js):
// fillExitTable and the helpers it draws with (exitRow, ppCell, rateCell,
// pctCell, headRow, emptyRow), over the real calculations of
// analytics-stats.js. A normal window, a window with fewer than MIN_N calls
// (every row greyed), an empty window (the one empty row), and points with a
// missing peak or return (not counted by any rule). renderPeak calls
// fillExitTable before its own early return, so a throw here would blank the
// whole tab. Run: node testdata/analytics-exit.test.js [path to analytics.js]
// (no dependencies; exit code 1 on any failure). TestAnalyticsExitNode runs
// it from `go test` when node is on the PATH. analytics.js is one closure for
// the browser, so the functions and constants are taken from its source by
// name and run in a sandbox of their own, with a tiny stand-in for document
// (as testdata/app-model.test.js does for app.js); this file lives outside
// frontend/, so the website never serves it.
'use strict';

var fs = require('fs');
var path = require('path');
var vm = require('vm');

var S = require('../frontend/analytics-stats.js');
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
// outside brackets; the values taken have no brackets or semicolons in their
// strings).
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

// A node of the stand-in document: its tag, class, title, scope, colSpan and
// children. A fragment's children move into the node it is appended to.
function Node(tag) {
  this.tagName = tag; this.className = ''; this.title = ''; this.scope = ''; this.colSpan = 1;
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

var page = {};
function resetPage() { page = { 'h-exit': new Node('h3'), 'tbl-exit': new Node('table') }; }
var document = {
  createElement: function (tag) { return new Node(tag); },
  createTextNode: function (text) { var n = new Node('#text'); n._text = String(text); return n; },
  createDocumentFragment: function () { return new Node('#fragment'); },
  getElementById: function (id) { return page[id] || null; }
};
var box = { document: document, A: S };
vm.createContext(box);
vm.runInContext([takeVar('DASH'), takeVar('MIN_N'), takeVar('isNum'), takeVar('SUPERSCRIPT'),
  takeVar('EXIT_RULES'), takeVar('EXIT_HEADS')].concat(['$', 'el', 'fmtInt', 'sciText', 'fmtPct', 'exactPct',
  'headRow', 'pctCell', 'rateCell', 'emptyRow', 'ppCell', 'exitRow', 'fillExitTable'].map(take)).join('\n'), box);

var failures = 0;
var count = 0;
function check(name, got, want) {
  count++;
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    failures++;
    console.error('FAIL ' + name + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
  }
}

// Points as renderPeak builds them, from [count, peak, return] groups.
function pts(groups) {
  var out = [];
  groups.forEach(function (g) {
    for (var i = 0; i < g[0]; i++) { out.push({ r: { id: out.length + 1 }, peak: g[1], ret: g[2] }); }
  });
  return out;
}

// Draws the table on a fresh stand-in page; the error it threw, if any.
function draw(name, p, hz) {
  resetPage();
  try {
    box.fillExitTable(p, hz);
  } catch (e) {
    check(name + ': does not throw', String(e && e.stack || e), null);
    return false;
  }
  check(name + ': does not throw', null, null);
  return true;
}
function table() { return page['tbl-exit']; }
function tbody() { return table().children[1] || new Node('missing'); }
function head() { var t = table().children[0]; return (t && t.children[0]) || new Node('missing'); }
// The cell in column c of body row r (a stand-in when it is not there, so a
// missing row fails its checks instead of stopping the run).
function cellAt(r, c) { var tr = tbody().children[r]; return (tr && tr.children[c]) || new Node('missing'); }
function texts(tr) { return tr.children.map(function (c) { return c.textContent; }); }
function rowTexts() { return tbody().children.map(texts); }

var HEADS = ['Exit rule', 'n', 'Touched', 'Mean return', 'Median return', 'Mean vs hold', 'Median vs hold'];

// ---- A normal window: 25 calls -------------------------------------------
// 10 × peak +300%, ended −100% (touched 2× and 3×, not 5×, then rugged)
//  5 × peak +100%, ended +50%  (touched 2× exactly)
// 10 × peak +20%,  ended −40%
// hold: mean (−1000 + 250 − 400) / 25 = −46, median −40 (they differ, so a
//       mean/median mix-up in the "vs hold" columns shows)
// 2×:   15 × 100, 10 × −40: mean 44, median 100
// 3×:   10 × 200, 5 × 50, 10 × −40: mean 74, median 50
// 5×:   none touched: the same as hold
var normal = pts([[10, 300, -100], [5, 100, 50], [10, 20, -40]]);
var NORMAL_ROWS = [
  ['Sell at 2×', '25', '60.0%15 / 25', '+44.0%', '+100.0%', '+90.0 pp', '+140.0 pp'],
  ['Sell at 3×', '25', '40.0%10 / 25', '+74.0%', '+50.0%', '+120.0 pp', '+90.0 pp'],
  ['Sell at 5×', '25', '0.0%0 / 25', '−46.0%', '−40.0%', '0.0 pp', '0.0 pp'],
  ['Hold to the end', '25', '–', '−46.0%', '−40.0%', '–', '–']
];
if (draw('normal', normal, '7d')) {
  check('normal: heading', page['h-exit'].textContent, 'Exit rule: sell at 2×, 3× or 5× (7d window)');
  check('normal: table parts', table().children.map(function (c) { return c.tagName; }), ['thead', 'tbody']);
  check('normal: head', texts(head()), HEADS);
  check('normal: head scopes', head().children.map(function (c) { return c.scope; }), HEADS.map(function () { return 'col'; }));
  check('normal: row count', tbody().children.length, 4);
  check('normal: rows', rowTexts(), NORMAL_ROWS);
  check('normal: not greyed', tbody().children.map(function (tr) { return tr.className; }), ['', '', '', '']);
  check('normal: row headers', tbody().children.map(function (tr, r) { var th = cellAt(r, 0); return [th.tagName, th.scope, th.title]; }),
    [['th', 'row', ''], ['th', 'row', ''], ['th', 'row', ''], ['th', 'row', '']]);
  var two = [0, 1, 2, 3, 4, 5, 6].map(function (c) { return cellAt(0, c); });
  check('normal: 2× touched title', two[2].title, '15 of 25 calls touched 2× within the window');
  check('normal: 2× mean vs hold title', two[5].title, '+90 percentage points');
  check('normal: 2× vs hold classes', [two[5].className, two[6].className], ['num pos', 'num pos']);
  check('normal: 5× vs hold class (zero)', cellAt(2, 5).className, 'num zero');
  check('normal: hold mean class', cellAt(3, 3).className, 'num neg');
  // drawn again: the table is replaced, not appended to
  box.fillExitTable(normal, '1d');
  check('redrawn: table parts', table().children.map(function (c) { return c.tagName; }), ['thead', 'tbody']);
  check('redrawn: row count', tbody().children.length, 4);
  check('redrawn: heading', page['h-exit'].textContent, 'Exit rule: sell at 2×, 3× or 5× (1d window)');
}

// ---- Fewer than MIN_N (20) calls: every row greyed ------------------------
// 8 × (+300%, −100%), 4 × (+100%, +50%), 7 × (+20%, −50%): 19 calls,
// 12 touched 2× (63.2%)
var few = pts([[8, 300, -100], [4, 100, 50], [7, 20, -50]]);
if (draw('fewer than 20', few, '1d')) {
  check('few: heading', page['h-exit'].textContent, 'Exit rule: sell at 2×, 3× or 5× (1d window)');
  check('few: row count', tbody().children.length, 4);
  check('few: greyed', tbody().children.map(function (tr) { return tr.className; }), ['ana-few', 'ana-few', 'ana-few', 'ana-few']);
  check('few: greyed title', tbody().children.map(function (tr, r) { return cellAt(r, 0).title; }),
    ['Fewer than 20 calls with data', 'Fewer than 20 calls with data', 'Fewer than 20 calls with data', 'Fewer than 20 calls with data']);
  check('few: n', tbody().children.map(function (tr, r) { return cellAt(r, 1).textContent; }), ['19', '19', '19', '19']);
  check('few: 2× touched', cellAt(0, 2).textContent, '63.2%12 / 19');
}
// exactly MIN_N calls: not greyed
if (draw('exactly 20', pts([[8, 300, -100], [4, 100, 50], [8, 20, -50]]), '1d')) {
  check('exactly 20: not greyed', tbody().children.map(function (tr) { return tr.className; }), ['', '', '', '']);
}

// ---- No calls: the one empty row -------------------------------------------
if (draw('empty', [], '30d')) {
  check('empty: heading', page['h-exit'].textContent, 'Exit rule: sell at 2×, 3× or 5× (30d window)');
  check('empty: head', texts(head()), HEADS);
  check('empty: row count', tbody().children.length, 1);
  var td = cellAt(0, 0);
  check('empty: cell', [td.tagName, td.className, td.colSpan, td.textContent],
    ['td', 'muted', 7, 'No calls with a peak and a return for this window.']);
}

// ---- Missing peak or return: not counted by any rule ----------------------
var missing = [
  { r: { id: 901 }, peak: null, ret: 30 },
  { r: { id: 902 }, peak: 120, ret: NaN },
  { r: { id: 903 }, peak: undefined, ret: 5 },
  { r: { id: 904 }, peak: 500, ret: null },
  { r: { id: 905 } }
];
if (draw('normal + missing', missing.concat(normal), '7d')) {
  check('normal + missing: same rows as without them', rowTexts(), NORMAL_ROWS);
}
// 21 with both + the missing ones: n is 21 in every row, not greyed
if (draw('21 + missing', pts([[10, 300, -100], [5, 100, 50], [6, 20, -50]]).concat(missing), '7d')) {
  check('21 + missing: n', tbody().children.map(function (tr, r) { return cellAt(r, 1).textContent; }), ['21', '21', '21', '21']);
  check('21 + missing: not greyed', tbody().children.map(function (tr) { return tr.className; }), ['', '', '', '']);
  check('21 + missing: 2× touched', cellAt(0, 2).textContent, '71.4%15 / 21');
}
// only missing ones: the empty row
if (draw('only missing', missing, '7d')) {
  check('only missing: row count', tbody().children.length, 1);
  check('only missing: text', tbody().children[0].textContent, 'No calls with a peak and a return for this window.');
}

// ppCell on its own: the sign, the unit and the dash
check('ppCell +26', [box.ppCell(26).textContent, box.ppCell(26).title], ['+26.0 pp', '+26 percentage points']);
check('ppCell −3.25', box.ppCell(-3.25).textContent, '−3.3 pp');
check('ppCell NaN', [box.ppCell(NaN).textContent, box.ppCell(NaN).className], ['–', 'num']);

if (failures) {
  console.error(failures + ' of ' + count + ' checks failed');
  process.exit(1);
}
console.log('ok: ' + count + ' checks');
