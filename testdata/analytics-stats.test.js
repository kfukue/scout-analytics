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

// exit rule: sell at the first touch of m×, else hold to the end
check('exitReturn: exactly at 2× sells', S.exitReturn(100, 20, 2), 100);
check('exitReturn: just under 2× holds', S.exitReturn(99.9, 20, 2), 20);
check('exitReturn: exactly at 3× sells', S.exitReturn(200, -40, 3), 200);
check('exitReturn: exactly at 5× sells', S.exitReturn(400, 900, 5), 400);
check('exitReturn: above m× sells at m×, not the peak', S.exitReturn(1000, 500, 2), 100);
check('exitReturn: under m× holds to the return', S.exitReturn(150, 120, 3), 120);
check('exitReturn: negative peak and return hold', S.exitReturn(-20, -60, 2), -60);
check('exitReturn: touched 2× then rugged sells at 2×', S.exitReturn(150, -100, 2), 100);
check('exitReturn: rugged without touching 3× keeps the loss', S.exitReturn(150, -100, 3), -100);
check('exitReturn: hold (Infinity) is the return', S.exitReturn(5000, 12.5, Infinity), 12.5);
near('exitReturn: missing peak', S.exitReturn(null, 30, 2), NaN);
near('exitReturn: missing peak (undefined)', S.exitReturn(undefined, 30, 2), NaN);
near('exitReturn: missing return, peak above m×', S.exitReturn(150, null, 2), NaN);
near('exitReturn: missing return, NaN', S.exitReturn(50, NaN, 2), NaN);
(function () {
  var pairs = [
    { peak: 400, ret: -100 }, // touched 2×, 3× and 5× (exactly), then rugged
    { peak: 100, ret: 20 },   // touched 2× exactly
    { peak: 250, ret: 250 },  // touched 2× and 3×, ended at its peak
    { peak: 0, ret: -50 },
    { peak: -10, ret: -100 },
    { peak: null, ret: 30 },  // missing peak: not counted by any rule
    { peak: 120, ret: NaN }   // missing return: not counted by any rule
  ];
  // 2×: 100, 100, 100, −50, −100; 3×: 200, 20, 200, −50, −100;
  // 5×: 400, 20, 250, −50, −100; hold: −100, 20, 250, −50, −100
  check('exitRuleStats 2×', S.exitRuleStats(pairs, 2), { n: 5, hit: 3, mean: 30, median: 100 });
  check('exitRuleStats 3×', S.exitRuleStats(pairs, 3), { n: 5, hit: 2, mean: 54, median: 20 });
  check('exitRuleStats 5×', S.exitRuleStats(pairs, 5), { n: 5, hit: 1, mean: 104, median: 20 });
  check('exitRuleStats hold', S.exitRuleStats(pairs, Infinity), { n: 5, hit: 0, mean: 4, median: -50 });
  var e = S.exitRuleStats([], 2);
  check('exitRuleStats: no pairs', [e.n, e.hit, isNaN(e.mean), isNaN(e.median)], [0, 0, true, true]);
})();

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

// ---- Timing tab --------------------------------------------------------------

// time bands: the lower end belongs to the band (exactly 5 minutes is 5–15m)
check('time band labels', S.TIME_BANDS.map(function (b) { return b.label; }),
  ['< 5m', '5–15m', '15–60m', '1–4h', '4–24h', '1–3d', '3–7d', '7–30d']);
[
  [0, 0], [299, 0], [300, 1], [899, 1], [900, 2], [3599, 2], [3600, 3], [14399, 3], [14400, 4], [86399, 4],
  [86400, 5], [259199, 5], [259200, 6], [604799, 6], [604800, 7], [2592000, 7], [9e9, 7],
  [-1, -1], [NaN, -1], [null, -1], [undefined, -1]
].forEach(function (c) { check('timeBand(' + c[0] + ')', S.timeBand(c[0]), c[1]); });
check('timeHistogram', S.timeHistogram([0, 300, 301, 3600, 86400, 604800, 2592000, null, -5]), [1, 2, 0, 1, 0, 1, 0, 2]);

// held at 2×: never touched = 0; a touched call counts what it really held
// (owner decision, 13 October 2026: also under 1 hour), touch and drop = 0
// as stored; touched with held missing = unknown
check('heldValue', [S.heldValue(null, null), S.heldValue(undefined, 0), S.heldValue(600, 0), S.heldValue(600, 3600), S.heldValue(600, null),
  S.heldValue(600, 1200), S.heldValue(600, 3599), S.heldValue(0, 299)],
  [0, 0, 0, 3600, NaN, 1200, 3599, 299]);
// … and the hold from the first cross: censored keeps its held time; a fall
// counts at most the time from the first cross to the first close under 2×
check('holdItem: never reached', S.holdItem(null, 0, null, false), null);
check('holdItem: touch and drop', S.holdItem(600, 0, 900, false), { t: 0, event: true });
check('holdItem: one stretch', S.holdItem(600, 1200, 2100, false), { t: 1200, event: true });
check('holdItem: fell back, rose again', S.holdItem(600, 7200, 2100, false), { t: 1500, event: true });
check('holdItem: censored', S.holdItem(600, 3000, null, true), { t: 3000, event: false });
check('holdItem: censored, first 2x in the last bucket', S.holdItem(3300, 0, null, true), { t: 0, event: false });

// "reached 2×" (owner decision, 12 October 2026: at or above 2× for at least
// 1 hour in total): held (above_2x_s) >= MIN_HELD_2X_S, in every window
// (1h included), whether or not the call was still at 2× when the window
// ended; otherwise the call is treated exactly like one that never touched
// 2×. As stored by candleTiming in timing.go: t2x = the START of the first 2×
// bucket, held is counted from that bucket's END, so the most that can be
// held is the window's end minus the end of the first 2× bucket. The cases
// below use a post on a 5-minute boundary, so buckets end on multiples of
// 300 s from the post (hourly ones after the first day on multiples of
// 3600 s).
check('MIN_HELD_2X_S', S.MIN_HELD_2X_S, 3600);
check('holdWords', [S.holdWords(3600), S.holdWords(7200), S.holdWords(5400)], ['1 hour', '2 hours', '90 minutes']);
// what every call that touched 2× (with a known held) but did not reach it
// gives: everything that hangs on "reached 2×" as for a call that never
// touched it, but held = the real stored held (owner decision, 13 October
// 2026; before it, 0)
function notReached(held) {
  return { touched: true, reached: false, short: true, t2x: NaN, held: held, tfall: NaN, cens: false, hold: null };
}
[
  // [name, t2x, held, tfall, censored, want]
  ['never touched', null, 0, null, false,
    { touched: false, reached: false, short: false, t2x: NaN, held: 0, tfall: NaN, cens: false, hold: null }],
  ['touched, 0 held (touch and drop)', 600, 0, 900, false, notReached(0)],
  ['held 3599 s', 600, 3599, 4500, false, notReached(3599)],
  ['held exactly 3600 s', 600, 3600, 4500, false,
    { touched: true, reached: true, short: false, t2x: 600, held: 3600, tfall: 4500, cens: false, hold: { t: 3600, event: true } }],
  ['held more than 3600 s', 600, 7200, 9000, false,
    { touched: true, reached: true, short: false, t2x: 600, held: 7200, tfall: 9000, cens: false, hold: { t: 7200, event: true } }],
  ['held 2 h in two stretches, first one 25 min', 600, 7200, 2100, false,
    { touched: true, reached: true, short: false, t2x: 600, held: 7200, tfall: 2100, cens: false, hold: { t: 1500, event: true } }],
  // 1d window (86400 s): first 2× in the bucket 84000-84300, at 2× from its
  // end to the window's end (2100 s): under 1 hour, so not reached
  ['1d, censored, first 2× 40 min before the end, held to the end', 84000, 2100, null, true, notReached(2100)],
  // 1d: first 2× in the last 30 min (bucket 84600-84900, held to the end 1500 s)
  ['1d, first 2× in the last 30 min, held to the end', 84600, 1500, null, true, notReached(1500)],
  ['1d, first 2× in the last 30 min, fell back one candle before the end', 84600, 1200, 86100, false, notReached(1200)],
  ['censored, first 2× in the last bucket (0 held)', 86100, 0, null, true, notReached(0)],
  // 1h window (3600 s): first 2× at 20 min, bucket 1200-1500, held to the end 2100 s
  ['1h, first 2× at 20 min, at 2× until the hour ended', 1200, 2100, null, true, notReached(2100)],
  // the most a 1h call can hold: first 2× in the first candle (0-300), at 2×
  // from its end to the end of the hour (3300 s, 55 minutes)
  ['1h, first 2× in the first candle, at 2× until the hour ended (55 min, the most possible)', 0, 3300, null, true, notReached(3300)],
  ['1h, first 2× at 20 min, fell back in the last candle', 1200, 1800, 3300, false, notReached(1800)],
  ['1h, first 2× at 20 min, first close under 2× at the end', 1200, 2100, 3600, false, notReached(2100)],
  ['1h, touched at 20 min, fell back at 50 min', 1200, 1500, 3000, false, notReached(1500)],
  ['1h, first 2× in the last candle, censored with 0 held (a wick at the end)', 3300, 0, null, true, notReached(0)],
  // rugged while still at 2×: never censored, the rug is the fall
  ['1h, at 2× until a rug at 50 min', 1200, 1500, 3000, false, notReached(1500)],
  // 7d window (604800 s), hourly candles: first 2× in the hourly bucket
  // 597600-601200, held to the end (3600 s): reached; in 601200-604800 (ends
  // at the window's end, 0 held): not reached
  ['7d, first 2× two hours before the end, held to the end', 597600, 3600, null, true,
    { touched: true, reached: true, short: false, t2x: 597600, held: 3600, tfall: NaN, cens: true, hold: { t: 3600, event: false } }],
  ['7d, first 2× in the last hourly candle, censored with 0 held', 601200, 0, null, true, notReached(0)],
  ['censored with exactly 1 h at 2×', 82500, 3600, null, true,
    { touched: true, reached: true, short: false, t2x: 82500, held: 3600, tfall: NaN, cens: true, hold: { t: 3600, event: false } }],
  ['censored with 3599 s at 2×', 82500, 3599, null, true, notReached(3599)],
  ['censored with over 1 h at 2×', 3000, 80000, null, true,
    { touched: true, reached: true, short: false, t2x: 3000, held: 80000, tfall: NaN, cens: true, hold: { t: 80000, event: false } }],
  ['touched, held missing: unknown (not counted as short)', 600, null, null, false,
    { touched: true, reached: false, short: false, t2x: NaN, held: NaN, tfall: NaN, cens: false, hold: null }],
  ['touched, censored, held missing: unknown (not counted as short)', 3300, null, null, true,
    { touched: true, reached: false, short: false, t2x: NaN, held: NaN, tfall: NaN, cens: false, hold: null }]
].forEach(function (c) {
  var got = S.timing2x(c[1], c[2], c[3], c[4]);
  var keys = Object.keys(c[5]);
  // JSON turns NaN into null on both sides: compare NaN explicitly
  keys.forEach(function (k) {
    var g = got[k], w = c[5][k];
    if (typeof w === 'number' && isNaN(w)) {
      check('timing2x ' + c[0] + ' (t2x ' + c[1] + ', held ' + c[2] + ', tfall ' + c[3] + ', cens ' + c[4] + ').' + k + ' is NaN', typeof g === 'number' && isNaN(g), true);
    } else {
      check('timing2x ' + c[0] + ' (t2x ' + c[1] + ', held ' + c[2] + ', tfall ' + c[3] + ', cens ' + c[4] + ').' + k, g, w);
    }
  });
  check('timing2x ' + c[0] + ': reached2x agrees', S.reached2x(c[1], c[2]), c[5].reached);
  check('timing2x ' + c[0] + ': keys', Object.keys(got).sort(), keys.slice().sort());
});

// the 1h window shows 0 reached by construction: whichever 5-minute candle
// holds the first 2×, a call at 2× from its end until the hour ended holds
// under 1 hour, so none reaches 2× (and none is in the time to 2×, the falls
// or the "still at 2× after t" curve), while every one stays in the totals
// and the time held figures show what each really held
(function () {
  var xs = [];
  for (var t = 0; t < 3600; t += 300) { xs.push(S.timing2x(t, 3600 - (t + 300), null, true)); }
  check('1h: calls at 2× until the hour ended, one per candle', xs.length, 12);
  check('1h: none reached 2×', xs.filter(function (x) { return x.reached; }).length, 0);
  check('1h: all touched 2× without reaching it', xs.filter(function (x) { return x.short; }).length, 12);
  check('1h: each counts its real time held (55 min down to 0)', xs.map(function (x) { return x.held; }),
    [3300, 3000, 2700, 2400, 2100, 1800, 1500, 1200, 900, 600, 300, 0]);
  // the held histogram: the 0 (last candle) is grey; the others are all
  // "touched, under 1 hour", in 5–15m (300, 600) and 15–60m (900 … 3300)
  var h = S.heldHistogram(xs.map(function (x) { return x.held; }), xs.map(function (x) { return x.held; }), []);
  check('1h: held histogram, 0 held', h.zero, [1, 0, 0, 0, 0, 0, 0, 0, 0]);
  check('1h: held histogram, touched under 1 hour', h.short, [0, 0, 2, 9, 0, 0, 0, 0, 0]);
  check('1h: held histogram, reached', h.reached, [0, 0, 0, 0, 0, 0, 0, 0, 0]);
  check('1h: none counted as still at 2× at the end (only reached calls are)', xs.filter(function (x) { return x.cens; }).length, 0);
  check('1h: none in the time to 2×', xs.map(function (x) { return x.t2x; }).filter(S.isNum).length, 0);
  check('1h: none in the KM curve', S.kaplanMeier(xs.map(function (x) { return x.hold; })).n, 0);
}());

// the rule moves calls from "reached" to "not reached" but never out of the
// totals: 6 timed calls (1d window), every one gives a held value (the
// denominator of the held figures and of the 2× rate stays 6); only the 2
// that held at least 1 h reach 2× (the one at 2× from 40 min before the end
// until the end held under 1 hour and does not); the time to 2×, the falls
// and the KM curve use those 2 only. The held figures use what each call
// really held (owner decision, 13 October 2026): 0 only for the call that
// never touched 2× and the touch and drop (stored 0), the real 3599 s and
// 2100 s for the two that touched 2× and held under 1 hour.
(function () {
  var calls = [[null, 0, null, false], [600, 0, 900, false], [600, 3599, 4500, false], [600, 3600, 4500, false],
    [600, 7200, 9000, false], [84000, 2100, null, true]];
  var xs = calls.map(function (c) { return S.timing2x(c[0], c[1], c[2], c[3]); });
  var helds = xs.map(function (x) { return x.held; }).filter(S.isNum);
  check('denominator: every timed call has a held value', helds.length, calls.length);
  check('denominator: held values (real for touched calls)', helds, [0, 0, 3599, 3600, 7200, 2100]);
  var reached = xs.filter(function (x) { return x.reached; }).length;
  check('2× rate: 2 of 6 (the denominator unchanged)', [reached, xs.length], [2, 6]);
  check('touched without reaching 2×', xs.filter(function (x) { return x.short; }).length, 3);
  check('time to 2× over the reached calls only', xs.map(function (x) { return x.t2x; }).filter(S.isNum), [600, 600]);
  check('falls over the reached calls only', xs.map(function (x) { return x.tfall; }).filter(S.isNum), [4500, 9000]);
  check('still at 2× at the end: none (the censored call did not reach 2×)', xs.filter(function (x) { return x.cens; }).length, 0);
  var km = S.kaplanMeier(xs.map(function (x) { return x.hold; }));
  check('KM over the reached calls only', [km.n, km.events, km.censored], [2, 2, 0]);
  // by hand: 0 + 0 + 3599 + 3600 + 7200 + 2100 = 16499 over 6;
  // sorted 0, 0, 2100, 3599, 3600, 7200: median (2100 + 3599) / 2 = 2849.5
  near('mean held (real held, 0 only for never touched and the touch and drop)', S.mean(helds), 16499 / 6);
  near('median held', S.median(helds), 2849.5);
  check('every reached call held at least 1 hour', xs.filter(function (x) { return x.reached && !(x.held >= S.MIN_HELD_2X_S); }).length, 0);
  check('every short call held under 1 hour', xs.filter(function (x) { return x.short && !(x.held < S.MIN_HELD_2X_S); }).length, 0);
  // the histogram: grey 0 = never touched + touch and drop; touched under
  // 1 hour = 3599 s and 2100 s, both 15–60m; reached = 3600 s and 7200 s, both 1–4h
  var h = S.heldHistogram(helds,
    xs.filter(function (x) { return x.short; }).map(function (x) { return x.held; }),
    xs.filter(function (x) { return x.reached; }).map(function (x) { return x.held; }));
  check('held histogram labels', h.labels, ['0', '< 5m', '5–15m', '15–60m', '1–4h', '4–24h', '1–3d', '3–7d', '7–30d']);
  check('held histogram: 0 held', h.zero, [2, 0, 0, 0, 0, 0, 0, 0, 0]);
  check('held histogram: touched, under 1 hour', h.short, [0, 0, 0, 2, 0, 0, 0, 0, 0]);
  check('held histogram: reached', h.reached, [0, 0, 0, 0, 2, 0, 0, 0, 0]);
  var tot = 0;
  [h.zero, h.short, h.reached].forEach(function (a) { a.forEach(function (k) { tot += k; }); });
  check('held histogram: every held value in exactly one bar', tot, helds.length);
}());

// the held histogram on its own: a touch and drop (short with 0 held) is in
// the grey 0 bar only, never also in "< 5m"; a short hold of 1 s to 299 s
// is in "< 5m"; band edges as timeBand (300 → 5–15m, 3600 → 1–4h)
(function () {
  var h = S.heldHistogram([0, 0, 1, 299, 300, 3599, 3600, 90000], [0, 1, 299, 300, 3599], [3600, 90000]);
  check('heldHistogram: 0 held', h.zero, [2, 0, 0, 0, 0, 0, 0, 0, 0]);
  check('heldHistogram: touched under 1 hour, the 0 left to the grey bar', h.short, [0, 2, 1, 1, 0, 0, 0, 0, 0]);
  check('heldHistogram: reached', h.reached, [0, 0, 0, 0, 1, 0, 1, 0, 0]);
  var e = S.heldHistogram([], [], []);
  check('heldHistogram: empty', [e.zero, e.short, e.reached], [[0, 0, 0, 0, 0, 0, 0, 0, 0], [0, 0, 0, 0, 0, 0, 0, 0, 0], [0, 0, 0, 0, 0, 0, 0, 0, 0]]);
}());

// a touched call with held missing is unknown: not in the held figures and
// not counted as "touched 2×, under 1 hour" (the count and the held
// histogram share the held figures' denominator)
(function () {
  var calls = [[600, 0, 900, false], [600, null, null, false], [3300, null, null, true], [1200, 2100, null, true]];
  var xs = calls.map(function (c) { return S.timing2x(c[0], c[1], c[2], c[3]); });
  var helds = xs.map(function (x) { return x.held; }).filter(S.isNum);
  check('held missing: held figures n', helds.length, 2);
  check('held missing: held values', helds, [0, 2100]);
  check('held missing: short count', xs.filter(function (x) { return x.short; }).length, 2);
  var h = S.heldHistogram(helds, xs.filter(function (x) { return x.short; }).map(function (x) { return x.held; }), []);
  var tot = 0;
  [h.zero, h.short, h.reached].forEach(function (a) { a.forEach(function (k) { tot += k; }); });
  check('held missing: 0 held + touched under 1 hour + reached = held figures n', tot, helds.length);
}());

// Kaplan–Meier, worked by hand: times 0 E, 300 E, 300 C, 600 C, 900 E, 1200 C
//   t=0:   6 at risk, 1 event → S = 5/6
//   t=300: 5 at risk, 1 event (the censored one at 300 still at risk) → S = 5/6 × 4/5 = 2/3
//   t=600: censored only → S stays 2/3
//   t=900: 2 at risk, 1 event → S = 2/3 × 1/2 = 1/3   (median: 900)
//   t=1200: censored → S stays 1/3
(function () {
  var km = S.kaplanMeier([
    { t: 900, event: true }, { t: 300, event: false }, { t: 0, event: true },
    { t: 1200, event: false }, { t: 300, event: true }, { t: 600, event: false }
  ]);
  check('KM n, events, censored, end', [km.n, km.events, km.censored, km.end], [6, 3, 3, 1200]);
  check('KM times', km.points.map(function (p) { return p.t; }), [0, 0, 300, 900]);
  check('KM at risk', km.points.map(function (p) { return p.atRisk; }), [6, 6, 5, 2]);
  near('KM S(0)', km.points[1].s, 5 / 6);
  near('KM S(300)', km.points[2].s, 2 / 3);
  near('KM S(900)', km.points[3].s, 1 / 3);
  near('KM median', km.median, 900);
  near('stepAt before the first event', S.stepAt([{ t: 0, s: 1 }, { t: 300, s: 0.5 }], 299), 1);
  near('stepAt(600) = S after 300', S.stepAt(km.points, 600), 2 / 3);
  near('stepAt(5000)', S.stepAt(km.points, 5000), 1 / 3);
  near('stepAt(0) takes the drop at 0', S.stepAt(km.points, 0), 5 / 6);
  // no events: flat at 1, no median; all censored at once
  var none = S.kaplanMeier([{ t: 100, event: false }, { t: 100, event: false }]);
  check('KM all censored', [none.points.length, none.events, none.censored], [1, 0, 2]);
  near('KM all censored: no median', none.median, NaN);
  check('KM empty', [S.kaplanMeier([]).n, S.kaplanMeier([]).points], [0, [{ t: 0, s: 1, atRisk: 0, d: 0 }]]);
  // every call an event (nothing censored): the curve is 1 − the share fallen
  var all = S.kaplanMeier([{ t: 10, event: true }, { t: 20, event: true }, { t: 20, event: true }, { t: 40, event: true }]);
  check('KM no censoring', all.points.map(function (p) { return p.s; }), [1, 0.75, 0.25, 0]);
  near('KM no censoring: median', all.median, 20);
  // "Half fell back by" = the first t where the curve drops to 50% or below
  // (exactly 50% counts)
  var half = S.kaplanMeier([{ t: 10, event: true }, { t: 20, event: true }, { t: 30, event: false }, { t: 40, event: true }]);
  near('KM median at exactly 50%', half.median, 20);
}());

// The "still at 2× after t" caption: t is the time held on closes, but at most
// the time from the first 2× (the start of its candle) to the first close under
// 2× or the rug. Here the first 2× candle (start 600) itself closes under 2×
// (at 900) and the call rose again later (3600 s held in total): only that
// first stretch counts, t = 900 − 600 = 300 s, a fall before 1 hour
check('reached, first 2× candle closes under 2×, rose again later: t = first 2× start to that close = 300 s',
  S.timing2x(600, 3600, 900, false).hold, { t: 300, event: true });
check('reached, at 2× until a rug: falls at the rug',
  S.timing2x(600, 4000, 4900, false).hold, { t: 4000, event: true });
check('reached, still at 2× at the end: censored at its held time',
  S.timing2x(600, 5000, null, true).hold, { t: 5000, event: false });

// share rugged by t: of n calls; ties make one step; times below 0 count at 0
check('cumulativeShare', S.cumulativeShare([3600, 300, 300, -20, null], 10),
  [{ t: 0, s: 0.1, k: 1 }, { t: 300, s: 0.3, k: 3 }, { t: 3600, s: 0.4, k: 4 }]);
check('cumulativeShare: none', S.cumulativeShare([], 5), [{ t: 0, s: 0, k: 0 }]);
check('cumulativeShare: n 0', S.cumulativeShare([5], 0), [{ t: 0, s: 0, k: 0 }]);

// means and medians of times held: 5 timed calls, two never touched 2× (0
// held), one touch and drop (0 as stored), one touched and held 20 minutes
// (real held, not 0), one held 2 hours
(function () {
  var held = [[null, 0], [null, 0], [600, 0], [600, 1200], [900, 7200]].map(function (x) { return S.heldValue(x[0], x[1]); });
  check('held values', held, [0, 0, 0, 1200, 7200]);
  near('mean held', S.mean(held), 8400 / 5);
  near('median held', S.median(held), 0);
}());

// windows too short for the "reached 2×" rule (owner decision, 12 October
// 2026): the first 2× candle closes at least 1 s after the post
// (timing.go), so a call holds at most window − 1 s; a window is too short
// only when even that is under MIN_HELD_2X_S
check('FIRST_2X_CLOSE_MIN_S', S.FIRST_2X_CLOSE_MIN_S, 1);
check('can2xReach: 1h', S.can2xReach(3600), false);
check('can2xReach: 1d, 3d, 7d, 30d', [86400, 259200, 604800, 2592000].map(S.can2xReach), [true, true, true, true]);
check('can2xReach: around the bound (3599, 3600, 3601, 3602, 3900)',
  [3599, 3600, 3601, 3602, 3900].map(S.can2xReach), [false, false, true, true, true]);
check('can2xReach: tiny windows', [0, 1, 300].map(S.can2xReach), [false, false, false]);
check('can2xReach: unknown window shows the figures', [undefined, null, NaN, '1h'].map(S.can2xReach), [true, true, true, true]);
// it agrees with the rule itself: the longest possible hold in a window
// reaches 2× exactly when the window is not too short
[3599, 3600, 3601, 86400].forEach(function (w) {
  check('can2xReach(' + w + ') = reached2x of the longest hold', S.can2xReach(w), S.reached2x(0, w - S.FIRST_2X_CLOSE_MIN_S));
});
check('na2x: the same words everywhere', S.na2x().text, 'n/a — needs 1 hour held');
check('na2x: short and needs make the text', S.na2x().short + ' — ' + S.na2x().needs, S.na2x().text);
check('na2x: why says where held is counted from',
  S.na2x().why.indexOf('counted from the end of the first 2× candle') >= 0 && S.na2x().why.indexOf('1 hour') >= 0, true);

// the time axis and durations
near('timeAxis(0)', S.timeAxis(0), 0);
near('timeAxis(9 minutes)', S.timeAxis(540), 1);
near('timeAxisInv(timeAxis(86400))', S.timeAxisInv(S.timeAxis(86400)), 86400, 1e-6);
check('fmtDur', [S.fmtDur(0), S.fmtDur(29), S.fmtDur(300), S.fmtDur(3599), S.fmtDur(5400), S.fmtDur(86399), S.fmtDur(86400),
  S.fmtDur(2592000), S.fmtDur(-120), S.fmtDur(NaN)], ['0m', '0m', '5m', '1h', '1.5h', '1d', '1d', '30d', '−2m', '–']);

// data health per week (weekHealth, quietByWeekFamily), over records as the
// page builds them (healthRecords in analytics.js decides verdict, known,
// quiet and rugged). Three weeks, given out of order: W2 a normal week, W1 a
// week with no known trade count, W3 one call.
(function () {
  var W1 = Date.UTC(2026, 8, 21), W2 = Date.UTC(2026, 8, 28), W3 = Date.UTC(2026, 9, 5);
  var ORDER = { v4: 0, v2: 1, gecko: 2, v3: 3 };
  function recs(n, start, f, o) {
    var out = [];
    for (var i = 0; i < n; i++) {
      out.push({ start: start, verdict: !!o.v, known: !!o.k, quiet: !!o.q, rugged: !!o.r, family: f, order: ORDER[f] });
    }
    return out;
  }
  var all = [].concat(
    recs(3, W2, 'v2', { v: 1, k: 1 }), recs(1, W2, 'v2', { v: 1, k: 1, q: 1 }),
    recs(4, W2, 'v4', { v: 1, k: 1, q: 1 }), recs(2, W2, 'v4', { v: 1, k: 1 }),
    recs(1, W2, 'v4', { k: 1, r: 1 }), // rugged: known, never quiet (the page's rule)
    recs(2, W2, 'gecko', {}),
    recs(1, W2, 'v3', { q: 1 }), // quiet without a known count: not counted
    recs(2, W1, 'gecko', { v: 1 }), recs(1, W1, 'gecko', { r: 1 }),
    recs(1, W3, 'v2', { v: 1, k: 1, q: 1 }));
  check('weekHealth: newest first, per week', S.weekHealth(all), [
    { start: W3, calls: 1, verdict: 1, known: 1, quiet: 1, rugged: 0 },
    { start: W2, calls: 14, verdict: 10, known: 11, quiet: 5, rugged: 1 },
    { start: W1, calls: 3, verdict: 2, known: 0, quiet: 0, rugged: 1 }
  ]);
  check('weekHealth: none', S.weekHealth([]), []);
  var qf = S.quietByWeekFamily(all);
  check('quietByWeekFamily: families with a known count, by order', qf.families, ['v4', 'v2']);
  check('quietByWeekFamily: weeks newest first', qf.weeks.map(function (w) { return w.start; }), [W3, W2, W1]);
  check('quietByWeekFamily: normal week', qf.weeks[1].cells, { v2: { k: 1, n: 4 }, v4: { k: 4, n: 7 } });
  check('quietByWeekFamily: week with no known count', qf.weeks[2].cells, {});
  check('quietByWeekFamily: one call', qf.weeks[0].cells, { v2: { k: 1, n: 1 } });
  check('quietByWeekFamily: none', S.quietByWeekFamily([]), { families: [], weeks: [] });
  check('quietByWeekFamily: no known count anywhere', S.quietByWeekFamily(recs(2, W1, 'gecko', { v: 1 })),
    { families: [], weeks: [{ start: W1, cells: {} }] });
}());

if (failures) {
  console.error(failures + ' of ' + count + ' checks failed');
  process.exit(1);
}
console.log('ok: ' + count + ' checks passed');
