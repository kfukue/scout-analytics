// Tests of the Overview tab's "Data health by week" tables
// (frontend/analytics.js): renderHealth and what it draws with
// (healthRecords, fewRateCell, fillHealthTable, fillHealthFamily, and the
// page's own isQuiet, isRugged, verdictKey, familyKey and period labels), over
// the real calculations of analytics-stats.js. A normal week, a week with no
// known trade count, the MIN_N greying, no calls at all, the Month period,
// and the page filters set (they must not change anything: the section uses
// every row). renderOverview draws it, so a throw here would blank the rest
// of the tab. Run: node testdata/analytics-health.test.js [path to
// analytics.js] (no dependencies; exit code 1 on any failure).
// TestAnalyticsHealthNode runs it from `go test` when node is on the PATH.
// analytics.js is one closure for the browser, so the functions and
// constants are taken from its source by name and run in a sandbox of their
// own, with a tiny stand-in for document and the page's data, col and state
// (as testdata/analytics-exit.test.js does); this file lives outside
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
// outside brackets; the values taken have no unbalanced brackets or
// semicolons in their strings).
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
function resetPage() {
  page = {
    't-health': new Node('h3'), 'tbl-health': new Node('table'),
    't-health-fam': new Node('h3'), 'tbl-health-fam': new Node('table'), 'lg-health-quiet': new Node('span')
  };
}
var document = {
  createElement: function (tag) { return new Node(tag); },
  createTextNode: function (text) { var n = new Node('#text'); n._text = String(text); return n; },
  createDocumentFragment: function () { return new Node('#fragment'); },
  getElementById: function (id) { return page[id] || null; }
};

// The page's state: data, col and state as load() and the controls set them.
var COLUMNS = ['call_id', 't', 'flags', 'verdict', 'family', 'dex', 'quote', 'trades_24h', 'no_data'];
var col = {};
COLUMNS.forEach(function (c, i) { col[c] = i; });
var box = { document: document, A: S, col: col, data: null, state: null, snapshotMs: NaN };
vm.createContext(box);
vm.runInContext([takeVar('DASH'), takeVar('MIN_N'), takeVar('FLAG_USD'), takeVar('FAMILY_LABEL'), takeVar('MONTHS'),
  takeVar('DAY_MS'), takeVar('isNum'), takeVar('HEALTH_HEADS')].concat(['$', 'el', 'fmtInt', 'cap', 'headRow', 'rateCell',
  'emptyRow', 'verdictKey', 'familyKey', 'isRugged', 'isQuiet', 'now', 'periodStart', 'periodEnd', 'periodLabel',
  'healthRecords', 'fewRateCell', 'fillHealthTable', 'fillHealthFamily', 'renderHealth'].map(take)).join('\n'), box);

var failures = 0;
var count = 0;
function check(name, got, want) {
  count++;
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    failures++;
    console.error('FAIL ' + name + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
  }
}

// Rows as /api/analytics sends them. verdicts: 0 clean, 1 caution,
// 2 red_flags, 3 unknown, 4 none; families: 0 v4, 1 v2, 2 gecko, −1 none;
// flags: 1 USD, 2 rugged, 4 tracked.
var VERDICTS = ['clean', 'caution', 'red_flags', 'unknown', 'none'];
var FAMILIES = ['v4', 'v2', 'gecko'];
var T_A = Date.UTC(2026, 8, 22, 12) / 1000; // Tue 22 Sep 2026: week of Mon 21 Sep
var T_B = Date.UTC(2026, 8, 29, 12) / 1000; // Tue 29 Sep 2026: week of Mon 28 Sep
var AFTER = Date.UTC(2026, 9, 14);          // both weeks are over
function rows(n, t, flags, verdict, family, trades) {
  var out = [];
  for (var i = 0; i < n; i++) { out.push([out.length + 1, t + i, flags, verdict, family, -1, -1, trades, 0]); }
  return out;
}
// Week A (25 calls):
//   10 v4, clean, 10 trades           quiet
//    5 v4, no verdict, 10 trades, rugged: known, not quiet (rugged is never quiet)
//    5 v4, unreadable, 50 trades       not quiet (50 is not fewer than 50)
//    3 v2, caution, 49 trades          quiet
//    2 GeckoTerminal, no verdict, count unknown
//   verdict 18 / 25 (unreadable counts), known 23 / 25, quiet 13 / 23,
//   rugged 5 / 25; quiet by family: v4 10 / 20 (exactly MIN_N, not grey),
//   v2 3 / 3 (grey)
// Week B (4 calls, no known trade count):
//    3 GeckoTerminal, no verdict, count unknown
//    1 not tracked (no family), rugged, count unknown
var WEEK_A = [].concat(rows(10, T_A, 5, 0, 0, 10), rows(5, T_A, 7, 4, 0, 10), rows(5, T_A, 5, 3, 0, 50),
  rows(3, T_A, 5, 1, 1, 49), rows(2, T_A, 5, 4, 2, null));
var WEEK_B = [].concat(rows(3, T_B, 5, 4, 2, null), rows(1, T_B, 2, 4, -1, null));

function setPage(list, st, snap) {
  box.data = { verdicts: VERDICTS, families: FAMILIES, quiet_below: 50, rows: list };
  box.state = Object.assign({ quiet: 'all', period: 'week', family: 'all', verdict: 'all' }, st || {});
  box.snapshotMs = snap === undefined ? AFTER : snap;
}

// Draws the section on a fresh stand-in page; false when it threw.
function draw(name, list, st, snap) {
  resetPage();
  setPage(list, st, snap);
  try {
    box.renderHealth(box.data.rows);
  } catch (e) {
    check(name + ': does not throw', String(e && e.stack || e), null);
    return false;
  }
  check(name + ': does not throw', null, null);
  return true;
}
function tbody(id) { return page[id].children[1] || new Node('missing'); }
function head(id) { var t = page[id].children[0]; return (t && t.children[0]) || new Node('missing'); }
function texts(tr) { return tr.children.map(function (c) { return c.textContent; }); }
function rowTexts(id) { return tbody(id).children.map(texts); }
function cellAt(id, r, c) { var tr = tbody(id).children[r]; return (tr && tr.children[c]) || new Node('missing'); }
function classes(id) { return tbody(id).children.map(function (tr) { return tr.children.map(function (c) { return c.className; }); }); }

var HEADS = ['Week', 'Calls', 'Verdict at the call', 'Trade count known', 'Quiet', 'Rugged'];
var LABEL_A = 'Week of Mon 21 Sep 2026', LABEL_B = 'Week of Mon 28 Sep 2026';
var HEALTH_ROWS = [
  [LABEL_B, '4', '0.0%0 / 4', '0.0%0 / 4', '–', '25.0%1 / 4'],
  [LABEL_A, '25', '72.0%18 / 25', '92.0%23 / 25', '56.5%13 / 23', '20.0%5 / 25']
];
var FAM_ROWS = [[LABEL_B, '–', '–'], [LABEL_A, '50.0%10 / 20', '100.0%3 / 3']];

// ---- A normal week and a week with no known trade count --------------------
var both = WEEK_A.concat(WEEK_B);
if (draw('normal', both)) {
  check('normal: headings', [page['t-health'].textContent, page['t-health-fam'].textContent],
    ['Data health by week', 'Quiet share by week and pool family']);
  check('normal: threshold in the caption', page['lg-health-quiet'].textContent, '50');
  check('normal: table parts', page['tbl-health'].children.map(function (c) { return c.tagName; }), ['thead', 'tbody']);
  check('normal: head', texts(head('tbl-health')), HEADS);
  check('normal: head scopes', head('tbl-health').children.map(function (c) { return c.scope; }), HEADS.map(function () { return 'col'; }));
  // the Rugged share is of every call in the period (not only those with a
  // known trade count, unlike Quiet)
  check('normal: rugged head title', head('tbl-health').children[5].title,
    'Share of all calls in the period flagged rugged by the tracker (as of now)');
  check('normal: rows (newest first)', rowTexts('tbl-health'), HEALTH_ROWS);
  check('normal: row headers', tbody('tbl-health').children.map(function (tr, r) { var th = cellAt('tbl-health', r, 0); return [th.tagName, th.scope]; }),
    [['th', 'row'], ['th', 'row']]);
  // MIN_N greying: every share of week B (4 calls) is grey, except the
  // dash; week A's shares (23 or 25) are not
  check('normal: greying', classes('tbl-health'), [
    ['tok', 'num', 'num ana-few', 'num ana-few', 'num', 'num ana-few'],
    ['tok', 'num', 'num', 'num', 'num', 'num']
  ]);
  check('normal: quiet title', cellAt('tbl-health', 1, 4).title, '13 of 23 calls with a known trade count are quiet');
  check('normal: grey title', cellAt('tbl-health', 0, 5).title, '1 of 4 calls are flagged rugged (fewer than 20)');
  // the family matrix: v4 then v2 (the order of data.families); no
  // GeckoTerminal or "Not tracked" column (no known count)
  check('family: head', texts(head('tbl-health-fam')), ['Week', 'Uniswap v4', 'Uniswap v2']);
  check('family: rows', rowTexts('tbl-health-fam'), FAM_ROWS);
  check('family: greying (20 not grey, 3 grey, no count a plain dash)', classes('tbl-health-fam'), [
    ['tok', 'num', 'num'],
    ['tok', 'num', 'num ana-few']
  ]);
  check('family: titles', [cellAt('tbl-health-fam', 1, 1).title, cellAt('tbl-health-fam', 1, 2).title], [
    '10 of 20 Uniswap v4 calls with a known trade count are quiet, ' + LABEL_A,
    '3 of 3 Uniswap v2 calls with a known trade count are quiet, ' + LABEL_A + ' (fewer than 20)'
  ]);
  // drawn again: the tables are replaced, not appended to
  box.renderHealth(box.data.rows);
  check('redrawn: rows', rowTexts('tbl-health'), HEALTH_ROWS);
  check('redrawn: family rows', rowTexts('tbl-health-fam'), FAM_ROWS);
}

// ---- The page filters do not apply ---------------------------------------
if (draw('filters set', both, { quiet: 'only', family: 'v2', verdict: 'clean' })) {
  check('filters set: same rows', rowTexts('tbl-health'), HEALTH_ROWS);
  check('filters set: same family rows', rowTexts('tbl-health-fam'), FAM_ROWS);
}

// ---- 19 known in a cell: grey; the order of the rows given does not matter
if (draw('19 known', rows(19, T_A, 5, 0, 0, 10).concat(rows(1, T_A, 5, 0, 2, null)).reverse())) {
  check('19 known: row', rowTexts('tbl-health'), [[LABEL_A, '20', '100.0%20 / 20', '95.0%19 / 20', '100.0%19 / 19', '0.0%0 / 20']]);
  check('19 known: greying', classes('tbl-health'), [['tok', 'num', 'num', 'num', 'num ana-few', 'num']]);
  check('19 known: family', [rowTexts('tbl-health-fam'), classes('tbl-health-fam')],
    [[[LABEL_A, '100.0%19 / 19']], [['tok', 'num ana-few']]]);
}

// ---- A week not over yet: "(so far)", as in the By week table -------------
if (draw('so far', WEEK_B.concat(rows(1, T_B, 5, 0, 0, 10)), {}, Date.UTC(2026, 8, 30))) {
  check('so far: label', [cellAt('tbl-health', 0, 0).textContent, cellAt('tbl-health-fam', 0, 0).textContent],
    [LABEL_B + ' (so far)', LABEL_B + ' (so far)']);
}

// ---- The Month period: the same periods as the By month table ------------
if (draw('month', both, { period: 'month' })) {
  check('month: headings', [page['t-health'].textContent, page['t-health-fam'].textContent],
    ['Data health by month', 'Quiet share by month and pool family']);
  check('month: head', texts(head('tbl-health'))[0], 'Month');
  check('month: rows', rowTexts('tbl-health'), [['Sep 2026', '29', '62.1%18 / 29', '79.3%23 / 29', '56.5%13 / 23', '20.7%6 / 29']]);
  check('month: family rows', rowTexts('tbl-health-fam'), [['Sep 2026', '50.0%10 / 20', '100.0%3 / 3']]);
}

// ---- Only calls without a known trade count ------------------------------
if (draw('no known count', WEEK_B)) {
  check('no known count: row', rowTexts('tbl-health'), [HEALTH_ROWS[0]]);
  check('no known count: family head', texts(head('tbl-health-fam')), ['Week']);
  check('no known count: family empty row', rowTexts('tbl-health-fam'), [['No calls with a known trade count.']]);
  check('no known count: family empty cell', [cellAt('tbl-health-fam', 0, 0).tagName, cellAt('tbl-health-fam', 0, 0).className,
    cellAt('tbl-health-fam', 0, 0).colSpan], ['td', 'muted', 1]);
}

// ---- No calls ------------------------------------------------------------
if (draw('empty', [])) {
  check('empty: headings', page['t-health'].textContent, 'Data health by week');
  check('empty: head', texts(head('tbl-health')), HEADS);
  check('empty: row', rowTexts('tbl-health'), [['No calls.']]);
  check('empty: cell', [cellAt('tbl-health', 0, 0).tagName, cellAt('tbl-health', 0, 0).className, cellAt('tbl-health', 0, 0).colSpan],
    ['td', 'muted', 6]);
  check('empty: family', rowTexts('tbl-health-fam'), [['No calls with a known trade count.']]);
}

// fewRateCell on its own: n 0 is a plain dash (nothing to grey); 19 grey,
// 20 not
check('fewRateCell n 0', [box.fewRateCell(0, 0, 'x').textContent, box.fewRateCell(0, 0, 'x').className], ['–', 'num']);
check('fewRateCell 19 / 20', [box.fewRateCell(1, 19, 'x').className, box.fewRateCell(1, 20, 'x').className], ['num ana-few', 'num']);

if (failures) {
  console.error(failures + ' of ' + count + ' checks failed');
  process.exit(1);
}
console.log('ok: ' + count + ' checks');
