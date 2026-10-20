// Tests of the list page's rugged and collapsed calls (frontend/app.js):
// isCollapsed and isDrained (a rugged call with rug_kind "price" is
// collapsed: rugged by the price rule, down more than 95%, the pool not seen
// drained; every other rugged call counts as drained, also one without a
// kind), peakCell and the row detail's detailPeakNode (a dash for a drained
// call only) and statusCell (the "rugged" or "collapsed" badge with the
// tracking status in its title, or the tracking status as text). Run: node
// testdata/app-rug.test.js [path to app.js] (no dependencies; exit code 1 on
// any failure). TestAppRugNode runs it from `go test` when node is on the
// PATH. app.js is one closure for the browser, so the functions are taken
// from its source by name and run in a sandbox of their own, with a tiny
// stand-in for document; this file lives outside frontend/, so the website
// never serves it.
'use strict';

var fs = require('fs');
var path = require('path');
var vm = require('vm');

var src = fs.readFileSync(process.argv[2] || path.join(__dirname, '..', 'frontend', 'app.js'), 'utf8');

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

// The source of the top-level "var name = …;" (up to the first semicolon
// outside brackets).
function takeVar(name) {
  var start = src.indexOf('  var ' + name + ' = ');
  if (start < 0) { throw new Error('app.js: var ' + name + ' not found'); }
  var depth = 0;
  for (var i = start; i < src.length; i++) {
    var c = src[i];
    if (c === '[' || c === '{' || c === '(') { depth++; }
    if (c === ']' || c === '}' || c === ')') { depth--; }
    if (c === ';' && depth === 0) { return src.slice(start, i + 1); }
  }
  throw new Error('app.js: var ' + name + ' has no end');
}

// A node of the stand-in document: its tag, class, title and children.
function Node(tag) { this.tagName = tag; this.className = ''; this.title = ''; this.children = []; this._text = ''; }
Object.defineProperty(Node.prototype, 'textContent', {
  get: function () { return this._text + this.children.map(function (c) { return c.textContent; }).join(''); },
  set: function (v) { this._text = String(v); this.children = []; }
});
Node.prototype.appendChild = function (c) { this.children.push(c); return c; };

var document = { createElement: function (tag) { return new Node(tag); } };
// pctCell and pctNode stand in for the real number cell and text: peakCell
// and detailPeakNode only decide whether to call them.
var box = {
  document: document, DASH: '–',
  pctCell: function (v) { var td = new Node('td'); td.className = 'num'; td.textContent = 'peak ' + v; return td; },
  pctNode: function (v) { var n = new Node('span'); n.textContent = 'peak ' + v; return n; }
};
vm.createContext(box);
vm.runInContext([takeVar('STATUS_TEXT')].concat(['el', 'isCollapsed', 'isDrained', 'peakCell', 'detailPeakNode', 'statusCell'].map(take)).join('\n'), box);

var failures = 0;
var count = 0;
function check(name, got, want) {
  count++;
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    failures++;
    console.error('FAIL ' + name + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
  }
}

function call(rugged, kind) { return { rugged: rugged, rug_kind: kind, tracking_status: 'done', peak_pct: 250 }; }

// [rugged, rug_kind, collapsed, drained, peak cell, row detail's peak]
[
  [true, 'price', true, false, 'peak 250', 'peak 250'],
  [true, 'event', false, true, '–', '– (rugged)'],
  [true, 'at_call', false, true, '–', '– (rugged)'],
  [true, 'detected', false, true, '–', '– (rugged)'],
  [true, null, false, true, '–', '– (rugged)'],
  [true, 'later_kind', false, true, '–', '– (rugged)'],
  [false, 'price', false, false, 'peak 250', 'peak 250'],
  [false, null, false, false, 'peak 250', 'peak 250'],
  [null, null, false, false, 'peak 250', 'peak 250']
].forEach(function (x) {
  var c = call(x[0], x[1]);
  var name = 'rugged ' + x[0] + ' kind ' + x[1];
  check(name + ': collapsed, drained', [box.isCollapsed(c), box.isDrained(c)], [x[2], x[3]]);
  check(name + ': peak cell', box.peakCell(c).textContent, x[4]);
  check(name + ': row detail peak', box.detailPeakNode(c).textContent, x[5]);
});
check('drained peak title', box.peakCell(call(true, 'event')).title, 'No peak: the pool was rugged');

// statusCell: the badge (class, words, title with the tracking status) or the status as text
function status(c) {
  var td = box.statusCell(c);
  var b = td.children[0];
  return b ? [td.className, b.className, b.textContent, b.title] : [td.className, td.textContent];
}
check('status collapsed', status(call(true, 'price')), ['status', 'badge badge-collapse', 'collapsed',
  'Collapsed: price down more than 95% (under 5% of the entry price at the end of tracking); the pool was not seen drained (tracking status: done)']);
check('status drained', status(call(true, 'event')), ['status', 'badge badge-rug', 'rugged',
  'Rugged: the pool was drained (tracking status: done)']);
check('status rugged without a kind', status(call(true, null)), ['status', 'badge badge-rug', 'rugged',
  'Rugged: the pool was drained (tracking status: done)']);
var tracking = call(true, 'price');
tracking.tracking_status = 'tracking';
check('status collapsed while tracking', / the pool was not seen drained \(tracking status: tracking\)$/.test(status(tracking)[3]), true);
check('status not rugged', status(call(false, null)), ['status', 'done']);
var unknown = call(false, null);
unknown.tracking_status = 'new_state';
check('status unknown', status(unknown), ['status', 'new_state']);
var none = call(true, 'price');
none.tracking_status = null;
check('status collapsed, no tracking status', / the pool was not seen drained \(tracking status: –\)$/.test(status(none)[3]), true);

if (failures) {
  console.error(failures + ' of ' + count + ' checks failed');
  process.exit(1);
}
console.log('ok: ' + count + ' checks');
