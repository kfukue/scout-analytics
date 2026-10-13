// Tests of the Perceptor filter as the list page (frontend/app.js) applies it
// to rows of live events: rowMatchesFilters with filterVerdict (the latest
// Perceptor report with a verdict, perceptor_latest_verdict) and
// filterUsesToday. Run: node testdata/app-filter.test.js (no dependencies;
// exit code 1 on any failure). TestAppFilterNode runs it from `go test` when
// node is on the PATH. app.js is one closure for the browser, so the
// functions are taken from its source by name and run in a sandbox of their
// own; this file lives outside frontend/, so the website never serves it.
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

var box = { state: { verdicts: [], days: 0, q: '' }, timeMs: Date.parse, nowMs: Date.now };
vm.createContext(box);
vm.runInContext(['verdictBucket', 'filterVerdict', 'filterUsesToday', 'rowMatchesFilters'].map(take).join('\n'), box);

var failures = 0;
var count = 0;
function check(name, got, want) {
  count++;
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    failures++;
    console.error('FAIL ' + name + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
  }
}

function row(call, today, latest) {
  var c = { message_date: '2026-10-01T00:00:00Z', token_name: 'T', contract_address: '0xabc', perceptor_verdict: call, perceptor_today_verdict: today };
  if (latest !== undefined) { c.perceptor_latest_verdict = latest; }
  return c;
}

var cases = [
  // name, row, the filters it passes, filterUsesToday
  ['clean, then a re-scan red_flags', row('clean', 'red_flags', 'red_flags'), ['red_flags'], true],
  ['no report, then a re-scan caution', row(null, 'caution', 'caution'), ['caution'], true],
  ['insufficient, then the 2-hour re-check clean', row('insufficient', 'clean', 'clean'), ['clean'], true],
  ['clean, then a failed re-scan', row('clean', null, 'clean'), ['clean'], false],
  ['clean, then a re-scan insufficient', row('clean', 'insufficient', 'clean'), ['clean'], false],
  ['re-scan red_flags, then a newer call-time clean', row('clean', 'red_flags', 'clean'), ['clean'], false],
  ['the same verdict twice', row('caution', 'caution', 'caution'), ['caution'], false],
  ['no report at all', row(null, null, null), ['not_scanned'], false],
  ['only insufficient', row('insufficient', null, 'insufficient'), ['not_scanned'], false],
  ['a server without perceptor_latest_verdict', row('caution', 'red_flags'), ['caution'], false]
];
var buckets = ['clean', 'caution', 'red_flags', 'not_scanned'];
cases.forEach(function (c) {
  var name = c[0], r = c[1], pass = c[2];
  buckets.forEach(function (b) {
    box.state.verdicts = [b];
    check(name + ': verdict=' + b, box.rowMatchesFilters(r), pass.indexOf(b) >= 0);
  });
  box.state.verdicts = ['clean', 'caution', 'red_flags'];
  check(name + ': verdict=clean,caution,red_flags', box.rowMatchesFilters(r), pass[0] !== 'not_scanned');
  box.state.verdicts = [];
  check(name + ': no filter', box.rowMatchesFilters(r), true);
  check(name + ': filterUsesToday', box.filterUsesToday(r), c[3]);
});

if (failures > 0) {
  console.error(failures + ' of ' + count + ' checks failed');
  process.exit(1);
}
console.log('ok: ' + count + ' checks');
