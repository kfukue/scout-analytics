// Tests of the Timing tab's rug kinds (frontend/analytics.js): rugOf reads a
// row's rug kind by name from data.rug_kinds, addTm puts each kind in its own
// list ("price" = collapsed: rugged by the price rule, down more than 95%, the
// pool not seen drained; never "other"), and fillTmRugs draws the "Rugs by
// kind of rug time" table with a "Collapsed (price down more than 95%)" row
// whose median is approximate (≈), like the detected one; the captions name
// collapsed calls among the rugs left out of the charts. Run:
// node testdata/analytics-rugs.test.js [path to analytics.js] (no
// dependencies; exit code 1 on any failure). TestAnalyticsRugsNode runs it
// from `go test` when node is on the PATH. analytics.js is one closure for
// the browser, so the functions and constants are taken from its source by
// name and run in a sandbox of their own, with a tiny stand-in for document
// and the page's data and col (as testdata/analytics-health.test.js does);
// this file lives outside frontend/, so the website never serves it.
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

// A node of the stand-in document: its tag, class, title, scope and
// children. A fragment's children move into the node it is appended to.
function Node(tag) { this.tagName = tag; this.className = ''; this.title = ''; this.scope = ''; this.children = []; this._text = ''; }
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

var page = { 'tbl-tm-rugs': new Node('table'), 'cap-tm-rugs': new Node('p') };
var document = {
  createElement: function (tag) { return new Node(tag); },
  createTextNode: function (text) { var n = new Node('#text'); n._text = String(text); return n; },
  createDocumentFragment: function () { return new Node('#fragment'); },
  getElementById: function (id) { return page[id] || null; }
};

// The page's data and col as load() sets them: only the columns rugOf reads.
var col = { flags: 0, rug_s: 1, rug_kind: 2 };
var data = { rug_kinds: ['event', 'at_call', 'detected', 'price', 'other'] };
var box = { document: document, A: S, col: col, data: data };
vm.createContext(box);
vm.runInContext([takeVar('DASH'), takeVar('isNum'), takeVar('FLAG_USD'), takeVar('RUG_EVENT')].concat(['$', 'el', 'fmtInt',
  'headRow', 'rateCell', 'nz', 'isRugged', 'rugOf', 'newTm', 'addTm', 'fillTmRugs', 'rugsApartText'].map(take)).join('\n'), box);

var failures = 0;
var count = 0;
function check(name, got, want) {
  count++;
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    failures++;
    console.error('FAIL ' + name + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
  }
}

// rugOf: the kind by name; no kind index: "none" when flagged rugged, '' when not
var RUGGED = 2;
check('rugOf price', box.rugOf([RUGGED, 2592000, 3]), { kind: 'price', s: 2592000 });
check('rugOf event', box.rugOf([RUGGED, 3600, 0]), { kind: 'event', s: 3600 });
check('rugOf other', box.rugOf([RUGGED, 5, 4]), { kind: 'other', s: 5 });
check('rugOf rugged, no kind', box.rugOf([RUGGED, null, -1]).kind, 'none');
check('rugOf not rugged', box.rugOf([0, null, -1]).kind, '');

// addTm: each kind in its own list; price is never "other"
var H = 2592000; // the 30d window
var tm = { peak: NaN, held: NaN, short: false, reached: false };
var all = box.newTm();
[[RUGGED, 3600, 0], [RUGGED, H + 60, 0], [RUGGED, -120, 1], [RUGGED, H + 3600, 2], [RUGGED, H, 3], [RUGGED, H + 7200, 3],
  [RUGGED, 5, 4], [RUGGED, null, -1], [0, null, -1]].forEach(function (r) { box.addTm(all, tm, box.rugOf(r), H); });
check('addTm counts', [all.n, all.rugIn, all.rugLater, all.atCall, all.detected, all.price, all.other, all.noTime],
  [9, [3600], [H + 60], [-120], [H + 3600], [H, H + 7200], 1, 1]);

// fillTmRugs: one row per kind; collapsed has its own row, an approximate
// median and is never in the charts
box.fillTmRugs(all, '30d');
var tbody = page['tbl-tm-rugs'].children[1];
var rows = tbody.children.map(function (tr) { return tr.children.map(function (c) { return c.textContent; }); });
check('row labels', rows.map(function (r) { return r[0]; }), ['Event, within the window', 'Event, after the window',
  'At the call (at or before the post)', 'Detected at the end of tracking', 'Collapsed (price down more than 95%)',
  'Other kind', 'Rugged, no rug time stored']);
var coll = rows[4];
check('collapsed count', coll[1], '2');
check('collapsed median approximate', coll[3], '≈ ' + S.fmtDur(H + 3600));
check('collapsed never in the charts', /Never in the charts\.$/.test(coll[4]), true);
check('detected median approximate', rows[3][3], '≈ ' + S.fmtDur(H + 3600));
check('event median exact', rows[0][3], S.fmtDur(3600));
check('caption', page['cap-tm-rugs'].textContent,
  'Over the 9 timed calls. Only event rug times are exact; the four other kinds are kept apart, as the owner decided.');

// no collapsed call: the row is still there, with a dash
var none = box.newTm();
box.addTm(none, tm, box.rugOf([RUGGED, 3600, 0]), H);
box.fillTmRugs(none, '30d');
var noneRow = page['tbl-tm-rugs'].children[1].children[4].children.map(function (c) { return c.textContent; });
check('no collapsed call', [noneRow[0], noneRow[1], noneRow[3]], ['Collapsed (price down more than 95%)', '0', '–']);

// The captions name collapsed calls among the rugs left out of the charts:
// the count sentence under the "share rugged by t" curve, and (fixed text,
// checked in the source) the "Rugged in the window" stat and the charts'
// caption.
check('rugs apart text', box.rugsApartText(all),
  '1 rugged at the call, 1 only detected at the end of tracking and 2 collapsed');
check('rug stat names collapsed', src.indexOf('(at-the-call, detected and collapsed rugs are counted apart, below)') >= 0, true);
check('charts caption names collapsed',
  src.indexOf('rugs at the call, rugs only detected at the end of tracking and collapsed calls are in the table below, not here.') >= 0, true);

if (failures) {
  console.error(failures + ' of ' + count + ' checks failed');
  process.exit(1);
}
console.log('ok: ' + count + ' checks');
