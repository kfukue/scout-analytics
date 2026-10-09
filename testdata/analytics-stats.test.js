// Tests of frontend/analytics-stats.js. Run: node testdata/analytics-stats.test.js
// (no dependencies; exit code 1 on any failure). TestAnalyticsStatsNode runs
// it from `go test` when node is on the PATH. It lives outside frontend/ so
// the website (which serves every file of frontend/) does not serve it.
'use strict';

var S = require('../frontend/analytics-stats.js');

var failures = 0;
var count = 0;

function check(name, got, want) {
  count++;
  var g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) {
    failures++;
    console.error('FAIL ' + name + ': got ' + g + ', want ' + w);
  }
}
function near(name, got, want, tol) {
  count++;
  if (!(Math.abs(got - want) <= (tol || 1e-9)) && !(isNaN(got) && isNaN(want))) {
    failures++;
    console.error('FAIL ' + name + ': got ' + got + ', want ' + want);
  }
}

// mean, median
[
  { in: [], mean: NaN, median: NaN },
  { in: [5], mean: 5, median: 5 },
  { in: [3, 1, 2], mean: 2, median: 2 },
  { in: [4, 1, 3, 2], mean: 2.5, median: 2.5 },
  { in: [-100, -100, 10000], mean: 3266.6666666666665, median: -100 }
].forEach(function (c) {
  near('mean(' + JSON.stringify(c.in) + ')', S.mean(c.in), c.mean);
  near('median(' + JSON.stringify(c.in) + ')', S.median(c.in), c.median);
});
var unsorted = [3, 1, 2];
S.median(unsorted);
check('median leaves its input as is', unsorted, [3, 1, 2]);
check('summarize', S.summarize([1, 2, 3, 10]), { n: 4, mean: 4, median: 2.5 });

// equal-count groups
function groups(values, k, zs) {
  var g = S.equalCountGroups(values, k, zs);
  return g.groups.map(function (x) { return [x.lo, x.hi, x.n, x.zero]; });
}
function seq(n) { var a = []; for (var i = 1; i <= n; i++) { a.push(i); } return a; }
check('groups: 10 values in 5', groups(seq(10), 5), [[1, 2, 2, false], [3, 4, 2, false], [5, 6, 2, false], [7, 8, 2, false], [9, 10, 2, false]]);
check('groups: 0 values', groups([], 5), []);
check('groups: nulls only', groups([null, NaN, undefined], 5), []);
check('groups: fewer values than groups', groups([5, 1, 3], 5), [[1, 1, 1, false], [3, 3, 1, false], [5, 5, 1, false]]);
check('groups: all zeros', groups([0, 0, 0, 0], 5), [[0, 0, 4, true]]);
check('groups: all equal (not zero)', groups([7, 7, 7], 4), [[7, 7, 3, false]]);
// ties stay in one group: the ranges never overlap
check('groups: ties', groups([1, 1, 1, 1, 1, 1, 2, 3, 4, 5], 5), [[1, 1, 6, false], [2, 3, 2, false], [4, 5, 2, false]]);
// zero share exactly at 10% → own group; just under → not
var tenPct = [0].concat(seq(9));
check('groups: zero share 10% → own group', groups(tenPct, 4), [[0, 0, 1, true], [1, 3, 3, false], [4, 6, 3, false], [7, 9, 3, false]]);
var under = [0].concat(seq(10));
check('groups: zero share 9.1% → no own group', groups(under, 4)[0], [0, 2, 3, false]);
// a zero group among negative values goes in order
check('groups: zero between negatives and positives', groups([-5, -4, 0, 0, 3, 4], 3), [[-5, -4, 2, false], [0, 0, 2, true], [3, 4, 2, false]]);
// index: every value lands in the group whose range holds it
(function () {
  var vals = [0, 0, 0, 5, 9, 12, 12, 12, 40, 41, 100, 2500, 7, 7];
  var g = S.equalCountGroups(vals, 4);
  var ok = true;
  vals.forEach(function (v) {
    var i = g.index(v);
    var gr = g.groups[i];
    if (!gr || v < gr.lo || v > gr.hi) { ok = false; console.error('  value ' + v + ' → group ' + i); }
  });
  check('groups: index matches the ranges', ok, true);
  var total = g.groups.reduce(function (s, x) { return s + x.n; }, 0);
  check('groups: counts add up', total, vals.length);
  check('groups: index of null', g.index(null), -1);
  check('groups: a value above the range → the last group', g.index(1e9), g.groups.length - 1);
  check('groups: a value below the range → the first group', g.index(-5), 0);
  var z = S.equalCountGroups([0, 0, 0], 5);
  check('groups: all zeros, index(0)', z.index(0), 0);
  check('groups: all zeros, index(3)', z.index(3), 0);
  check('groups: no values, index(3)', S.equalCountGroups([], 5).index(3), -1);
}());

// zero group with negative values (pre_chg): negatives and positives are cut
// separately, so no group spans 0 and the groups stay in order
function rep(v, n) { var a = []; for (var i = 0; i < n; i++) { a.push(v); } return a; }
function checkOrdered(name, g, vals) {
  var gs = g.groups;
  var ok = true;
  for (var i = 1; i < gs.length; i++) {
    if (!(gs[i - 1].hi < gs[i].lo)) { ok = false; console.error('  ' + name + ': group ' + (i - 1) + ' hi ' + gs[i - 1].hi + ' ≥ group ' + i + ' lo ' + gs[i].lo); }
  }
  check(name + ': groups in order, not overlapping', ok, true);
  var hasZero = gs.some(function (x) { return x.zero; });
  check(name + ': no group spans 0', hasZero && gs.every(function (x) { return x.zero || !(x.lo < 0 && x.hi > 0); }), true);
  check(name + ': counts add up', gs.reduce(function (s, x) { return s + x.n; }, 0), vals.length);
  var bad = vals.filter(function (v) { var x = gs[g.index(v)]; return !x || v < x.lo || v > x.hi; });
  check(name + ': every value in a group that holds it', bad, []);
  check(name + ': index(1e9) → the last group', g.index(1e9), gs.length - 1);
  check(name + ': index(-50) → the lowest group', g.index(-50), 0);
}
(function () {
  var a = rep(0, 5).concat([-30, -20, -10, -5, -2, 1, 2, 3, 4, 8, 12, 20, 40, 60, 90]);
  var ga = S.equalCountGroups(a, 5);
  check('groups A (k=5)', groups(a, 5), [[-30, -2, 5, false], [0, 0, 5, true], [1, 4, 4, false], [8, 20, 3, false], [40, 90, 3, false]]);
  checkOrdered('groups A', ga, a);
  check('groups A: index(-1) → the negative group, not the zero group', ga.index(-1), 0);
  check('groups A: index(-0.5) → the negative group', ga.index(-0.5), 0);
  check('groups A: index(0) → the zero group', ga.index(0), 1);
  check('groups A: index(0.5) → the first positive group', ga.index(0.5), 2);
  check('groups A: index(5) → the group above 4', ga.index(5), 3);

  var b = rep(0, 3).concat([-1], seq(17).slice(1));
  var gb = S.equalCountGroups(b, 4);
  check('groups B (k=4)', groups(b, 4), [[-1, -1, 1, false], [0, 0, 3, true], [2, 9, 8, false], [10, 17, 8, false]]);
  checkOrdered('groups B', gb, b);
  check('groups B: index(-1)', gb.index(-1), 0);
  check('groups B: index(1) → the first positive group', gb.index(1), 2);

  // only zeros and positives: index(-50) → the zero group (the lowest)
  var c = S.equalCountGroups([0, 0, 1, 2, 3, 4], 3);
  check('groups C: zero first', c.groups[0].zero, true);
  check('groups C: index(-50)', c.index(-50), 0);
  // only negatives and zeros: index(1e9) → the zero group (the highest)
  var d = S.equalCountGroups([-4, -3, -2, -1, 0, 0], 3);
  check('groups D: zero last', d.groups[d.groups.length - 1].zero, true);
  check('groups D: index(1e9)', d.index(1e9), d.groups.length - 1);
  check('groups D: index(-50)', d.index(-50), 0);
  // k = 2 with both signs: 3 groups (negative, 0, positive)
  check('groups: k=2 both signs', groups([-2, -1, 0, 0, 1, 2], 2), [[-2, -1, 2, false], [0, 0, 2, true], [1, 2, 2, false]]);
  // a side with fewer distinct values than its share gives the rest to the other
  check('groups: spare parts go to the other side', groups([-1, -1, -1, -1, -1, -1, 0, 0, 1, 2, 3, 4], 5).length, 5);
}());

// histogram bands: boundaries go farther from 0; 0 counts in −25 to 0
[
  [-100, 0], [-75, 0], [-74.9, 1], [-50, 1], [-49.9, 2], [-25, 2], [-24.9, 3], [0, 3], [0.1, 4],
  [24.9, 4], [25, 5], [50, 6], [99.9, 6], [100, 7], [299.9, 7], [300, 8], [1e6, 8], [-120, 0], [null, -1], [NaN, -1]
].forEach(function (c) { check('band(' + c[0] + ')', S.band(c[0]), c[1]); });
check('bands: 9 labels', S.BANDS.length, 9);
check('histogram', S.histogram([-100, -60, -10, 0, 10, 30, 70, 150, 400, null]), [1, 1, 0, 2, 1, 1, 1, 1, 1]);

// outcome classes
[
  [-100, true, 0], [500, true, 0], [-50, false, 1], [-49.9, false, 2], [0, false, 2], [0.1, false, 3],
  [99.9, false, 3], [100, false, 4], [null, false, -1]
].forEach(function (c) { check('outcome(' + c[0] + ', ' + c[1] + ')', S.outcome(c[0], c[1]), c[2]); });

// symlog and log1p round trips
[-100, -99.9, -50, -10, -0.1, 0, 0.1, 10, 50, 300, 1e4, 1e8].forEach(function (r) {
  near('symlogInv(symlog(' + r + '))', S.symlogInv(S.symlog(r)), r, Math.abs(r) * 1e-9 + 1e-9);
});
near('symlog(0)', S.symlog(0), 0);
near('symlog(90)', S.symlog(90), 1);
near('symlog(-90)', S.symlog(-90), -1);
check('symlog is odd', S.symlog(-300) === -S.symlog(300), true);
[0, 1, 9, 99, 12345].forEach(function (x) { near('log1p10Inv(log1p10(' + x + '))', S.log1p10Inv(S.log1p10(x)), x, x * 1e-9 + 1e-9); });
near('log1p10(0)', S.log1p10(0), 0);
check('symlogTicks(-100, 1000)', S.symlogTicks(-100, 1000), [-100, -50, -20, 0, 20, 50, 100, 300, 1000]);
check('symlogTicks keeps 0 and stays in range', S.symlogTicks(-100, 1e6).every(function (v) { return v >= -100 && v <= 1e6; }) &&
  S.symlogTicks(-100, 1e6).indexOf(0) >= 0, true);
check('log1pTicks(5000)', S.log1pTicks(5000), [0, 1, 10, 100, 1000]);
check('log1pTicks(8)', S.log1pTicks(8), [0, 1, 2, 5]);
check('log1pTicks(60)', S.log1pTicks(60), [0, 1, 2, 5, 10, 20, 50]);
check('log1pTicks(0)', S.log1pTicks(0), [0]);
check('log1pTicks(1e10)', S.log1pTicks(1e10), [0, 1, 100, 1e4, 1e6, 1e8, 1e10]);

// weeks (Monday to Sunday, UTC), months, hour and weekday
var mon = Date.UTC(2026, 7, 3) / 1000; // Mon 3 Aug 2026 00:00 UTC
check('weekStart(Monday 00:00)', S.weekStart(mon), mon * 1000);
check('weekStart(Sunday 23:59:59)', S.weekStart(mon + 7 * 86400 - 1), mon * 1000);
check('weekStart(next Monday)', S.weekStart(mon + 7 * 86400), (mon + 7 * 86400) * 1000);
check('monthStart', S.monthStart(mon + 40 * 86400), Date.UTC(2026, 8, 1));
check('hourWeekday(Monday 00:00)', S.hourWeekday(mon), { hour: 0, weekday: 0 });
check('hourWeekday(Sunday 23:30)', S.hourWeekday(mon + 6 * 86400 + 23.5 * 3600), { hour: 23, weekday: 6 });

// $100 on every call: capped at +1,000%, in time order
check('cumulativePL', S.cumulativePL([{ t: 3, ret: 5000 }, { t: 1, ret: -100 }, { t: 2, ret: 20 }]),
  [{ t: 1, cum: -100 }, { t: 2, cum: -80 }, { t: 3, cum: 920 }]);
check('cumulativePL: other cap', S.cumulativePL([{ t: 1, ret: 500 }], 300), [{ t: 1, cum: 300 }]);
check('cumulativePL: none', S.cumulativePL([]), []);

// gave back more than half of the peak
check('gaveBack', S.gaveBack([
  { peak: 100, ret: 49.9 }, // gave back more than half
  { peak: 100, ret: 50 },   // exactly half: not more
  { peak: 0, ret: -20 },    // no peak above 0: not counted
  { peak: -5, ret: -30 },   // not counted
  { peak: 400, ret: -100 }, // gave back more than half
  { peak: null, ret: 3 }    // missing: not counted
]), { k: 2, n: 3 });

// jitter: fixed per id, in range
check('jitter is fixed', S.jitter(812) === S.jitter(812), true);
check('jitter differs between ids', S.jitter(812) !== S.jitter(813), true);
(function () {
  var ok = true;
  for (var i = 0; i < 5000; i++) { var j = S.jitter(i); if (!(j >= -0.5 && j < 0.5)) { ok = false; } }
  check('jitter in [−0.5, 0.5)', ok, true);
}());

// labels
check('shortUSD', [S.shortUSD(950), S.shortUSD(20000), S.shortUSD(45300), S.shortUSD(1250000), S.shortUSD(0)], ['$950', '$20k', '$45.3k', '$1.3M', '$0']);
check('rangeLabel usd', S.rangeLabel({ lo: 20000, hi: 45000 }, 'usd'), '$20k–$45k');
check('rangeLabel count single', S.rangeLabel({ lo: 0, hi: 0 }, 'count'), '0');
check('rangeLabel count', S.rangeLabel({ lo: 3, hi: 1200 }, 'count'), '3–1.2k');
check('rangeLabel pct', S.rangeLabel({ lo: -12.3, hi: 4.1 }, 'pct'), '−12%–+4.1%');
// ends that would print the same get more digits (never "$1.3k–$1.3k")
check('rangeLabel usd 1250–1260', S.rangeLabel({ lo: 1250, hi: 1260 }, 'usd'), '$1.25k–$1.26k');
check('rangeLabel count 1001–1049', S.rangeLabel({ lo: 1001, hi: 1049 }, 'count'), '1k–1.05k');
check('rangeLabel pct −0.04–−0.01', S.rangeLabel({ lo: -0.04, hi: -0.01 }, 'pct'), '−0.04%–−0.01%');
check('rangeLabels: defaults unchanged', S.rangeLabels([{ lo: 20000, hi: 45000 }, { lo: 46000, hi: 90000 }], 'usd'), ['$20k–$45k', '$46k–$90k']);
check('rangeLabels: count default', S.rangeLabels([{ lo: 3, hi: 1200 }], 'count'), ['3–1.2k']);
check('rangeLabels: pct default', S.rangeLabels([{ lo: -12.3, hi: 4.1 }], 'pct'), ['−12%–+4.1%']);
check('rangeLabels: 1250–1260 alone', S.rangeLabels([{ lo: 1250, hi: 1260 }], 'usd'), ['$1.25k–$1.26k']);
// a group end that prints like the next group's start: both get more digits
check('rangeLabels: neighbours 1220 | 1240', S.rangeLabels([{ lo: 1100, hi: 1220 }, { lo: 1240, hi: 1400 }], 'usd'), ['$1.1k–$1.22k', '$1.24k–$1.4k']);
check('rangeLabels: neighbours 1049 | 1050 (count)', S.rangeLabels([{ lo: 1001, hi: 1049 }, { lo: 1050, hi: 1099 }], 'count'), ['1.001k–1.049k', '1.05k–1.099k']);
check('rangeLabels: pct next to the zero group', S.rangeLabels([{ lo: -3, hi: -0.02 }, { lo: 0, hi: 0, zero: true }, { lo: 0.3, hi: 5 }], 'pct'), ['−3%–−0.02%', '0%', '+0.3%–+5%']);
check('rangeLabels: no groups', S.rangeLabels([], 'usd'), []);
(function () {
  // neighbouring labels never look overlapping (a group's shown end is below
  // the next group's shown start), over spread-out values
  function shown(s) {
    var m = { k: 1e3, M: 1e6, B: 1e9 }[s.slice(-1)] || 1;
    var b = s.replace(/[$]/g, '');
    return parseFloat(m === 1 ? b : b.slice(0, -1)) * m;
  }
  var vals = [];
  for (var i = 0; i < 400; i++) { vals.push(1000 + (i * 7919) % 400 + (i % 3) * 0.4); }
  var gs = S.equalCountGroups(vals, 10).groups;
  var labs = S.rangeLabels(gs, 'usd');
  var bad = [];
  for (var j = 0; j + 1 < labs.length; j++) {
    var a = labs[j].split('–'), b = labs[j + 1].split('–');
    if (!(shown(a[a.length - 1]) < shown(b[0]))) { bad.push(labs[j] + ' | ' + labs[j + 1]); }
  }
  labs.forEach(function (l) { var p = l.split('–'); if (p.length === 2 && !(shown(p[0]) < shown(p[1]))) { bad.push(l); } });
  check('rangeLabels: ends in order within and between groups', bad, []);
}());
// rounding up to 1,000 of a unit moves to the next unit
check('shortNum rollover', [S.shortNum(999), S.shortNum(999.96), S.shortNum(1000), S.shortNum(999999), S.shortNum(999950000), S.shortNum(-999.96)],
  ['999', '1k', '1k', '1M', '1B', '−1k']);
// signedPct: a value that rounds to 0 has no sign (not a win, not a loss)
check('signedPct', [S.signedPct(0), S.signedPct(0.04), S.signedPct(-0.04), S.signedPct(0.05), S.signedPct(4.1), S.signedPct(-12.3), S.signedPct(999.6), S.signedPct(1234)],
  ['0%', '0%', '0%', '+0.1%', '+4.1%', '−12%', '+1k%', '+1.2k%']);

if (failures) {
  console.error(failures + ' of ' + count + ' checks failed');
  process.exit(1);
}
console.log('ok: ' + count + ' checks passed');
