// Scout analytics: the pure calculations of the Analytics page (no DOM, no
// state): mean and median, equal-count groups, the fixed return bands, the
// outcome classes, the symmetric-log and log scales, UTC weeks and hours,
// "$100 on every call" and "gave back more than half". Plain JavaScript,
// loaded by analytics.html before analytics.js (window.AnaStats), and by
// testdata/analytics-stats.test.js under Node (module.exports).
(function (root) {
  'use strict';

  var DAY_MS = 86400000;

  function isNum(v) { return typeof v === 'number' && isFinite(v); }

  function mean(list) {
    if (!list.length) { return NaN; }
    var s = 0;
    for (var i = 0; i < list.length; i++) { s += list[i]; }
    return s / list.length;
  }

  // median of a list (not changed); NaN when empty
  function median(list) {
    if (!list.length) { return NaN; }
    var s = list.slice().sort(function (a, b) { return a - b; });
    var m = s.length >> 1;
    return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
  }

  // n, mean and median of a list
  function summarize(list) {
    return { n: list.length, mean: mean(list), median: median(list) };
  }

  // Equal-count groups of the values of a number factor. k = the number of
  // groups wanted (4, 5, 10). Equal values always stay in one group, so a
  // group can be larger than n / k and there can be fewer groups than asked
  // for (when there are fewer distinct values).
  //
  // When at least zeroShare (default 0.1) of the values are exactly 0, 0 is a
  // group of its own, and the negative and the positive values are cut
  // separately, so no group spans 0: the k − 1 other groups are shared between
  // the two sides in proportion to their number of values (rounded), each
  // side that has values getting at least one, and no side more than its
  // number of distinct values (the rest go to the other side). So the total
  // is k when there are enough distinct values, except k = 2 with values on
  // both sides of 0: 3 groups (negative, 0, positive).
  //
  // Returns the groups in increasing order, each { lo, hi, n, zero } (lo and
  // hi: the smallest and largest value in it), and index(v): the group of a
  // value (−1 when not a number or when there are no groups). A value outside
  // every group goes to the nearest group on its side of 0 (below every group:
  // the lowest; above: the highest), so other calls can be put into the same
  // groups; 0 goes to the zero group when there is one.
  function equalCountGroups(values, k, zeroShare) {
    if (zeroShare === undefined) { zeroShare = 0.1; }
    var vals = [];
    var zeros = 0;
    for (var i = 0; i < values.length; i++) {
      if (!isNum(values[i])) { continue; }
      vals.push(values[i]);
      if (values[i] === 0) { zeros++; }
    }
    vals.sort(function (a, b) { return a - b; });
    var want = Math.max(1, k | 0);
    var zeroOwn = vals.length > 0 && zeros > 0 && zeros >= zeroShare * vals.length;
    if (!zeroOwn) {
      var all = cutSorted(vals, want);
      return { groups: all.groups, index: function (v) { return isNum(v) ? findUpper(all.uppers, v) : -1; } };
    }
    var neg = vals.filter(function (v) { return v < 0; });
    var pos = vals.filter(function (v) { return v > 0; });
    var share = splitParts(neg, pos, Math.max(1, want - 1));
    var negCut = cutSorted(neg, share[0]);
    var posCut = cutSorted(pos, share[1]);
    var zeroAt = negCut.groups.length;
    var groups = negCut.groups.concat([{ lo: 0, hi: 0, n: zeros, zero: true }], posCut.groups);
    function index(v) {
      if (!isNum(v)) { return -1; }
      if (v < 0 && negCut.uppers.length) { return findUpper(negCut.uppers, v); }
      if (v > 0 && posCut.uppers.length) { return zeroAt + 1 + findUpper(posCut.uppers, v); }
      // 0, or a side with no group of its own: the zero group
      return zeroAt;
    }
    return { groups: groups, index: index };
  }

  // How many groups the negative and the positive values get (see
  // equalCountGroups): parts in proportion to their counts, at least one for
  // a side with values, at most its number of distinct values.
  function splitParts(neg, pos, parts) {
    if (!neg.length) { return [0, pos.length ? parts : 0]; }
    if (!pos.length) { return [parts, 0]; }
    var total = Math.max(2, parts);
    var pn = Math.min(total - 1, Math.max(1, Math.round(total * neg.length / (neg.length + pos.length))));
    var pp = total - pn;
    var dn = distinct(neg), dp = distinct(pos);
    if (pn > dn) { pp += pn - dn; pn = dn; }
    if (pp > dp) { pn = Math.min(dn, pn + pp - dp); pp = dp; }
    return [pn, pp];
  }
  function distinct(sorted) {
    var d = 0;
    for (var i = 0; i < sorted.length; i++) { if (i === 0 || sorted[i] !== sorted[i - 1]) { d++; } }
    return d;
  }

  // Cuts sorted values into at most parts groups of about the same size
  // (equal values in one group). Returns the groups and their upper bounds
  // (inclusive, strictly increasing).
  function cutSorted(sorted, parts) {
    var uppers = [];
    for (var g = 1; g <= parts && sorted.length; g++) {
      var u = sorted[Math.min(Math.ceil(g * sorted.length / parts) - 1, sorted.length - 1)];
      if (!uppers.length || u > uppers[uppers.length - 1]) { uppers.push(u); }
    }
    if (sorted.length && uppers[uppers.length - 1] < sorted[sorted.length - 1]) { uppers.push(sorted[sorted.length - 1]); }
    var groups = [];
    var lo = 0;
    for (var j = 0; j < uppers.length; j++) {
      var start = lo;
      while (lo < sorted.length && sorted[lo] <= uppers[j]) { lo++; }
      groups.push({ lo: sorted[start], hi: sorted[lo - 1], n: lo - start, zero: false });
    }
    return { groups: groups, uppers: uppers };
  }

  // The first group whose upper bound is ≥ v; the last one when v is above
  // them all; −1 when there are none.
  function findUpper(uppers, v) {
    if (!uppers.length) { return -1; }
    var a = 0, b = uppers.length - 1;
    if (v > uppers[b]) { return b; }
    while (a < b) {
      var m = (a + b) >> 1;
      if (uppers[m] >= v) { b = m; } else { a = m + 1; }
    }
    return a;
  }

  // The fixed return bands of the histograms (percent). A value on a boundary
  // goes to the band farther from 0, and 0 itself to "−25 to 0": so the first
  // two bands are exactly the collapses (≤ −50%), the bands above 0 exactly
  // the wins (> 0%), and the last two exactly the ≥ +100% calls.
  var BANDS = [
    { label: '−100 to −75', lo: -Infinity, hi: -75 },
    { label: '−75 to −50', lo: -75, hi: -50 },
    { label: '−50 to −25', lo: -50, hi: -25 },
    { label: '−25 to 0', lo: -25, hi: 0 },
    { label: '0 to +25', lo: 0, hi: 25 },
    { label: '+25 to +50', lo: 25, hi: 50 },
    { label: '+50 to +100', lo: 50, hi: 100 },
    { label: '+100 to +300', lo: 100, hi: 300 },
    { label: '≥ +300', lo: 300, hi: Infinity }
  ];

  function band(v) {
    if (!isNum(v)) { return -1; }
    if (v <= 0) {
      if (v <= -75) { return 0; }
      if (v <= -50) { return 1; }
      if (v <= -25) { return 2; }
      return 3;
    }
    if (v < 25) { return 4; }
    if (v < 50) { return 5; }
    if (v < 100) { return 6; }
    if (v < 300) { return 7; }
    return 8;
  }

  // Counts per band of a list.
  function histogram(list) {
    var out = [];
    for (var i = 0; i < BANDS.length; i++) { out.push(0); }
    for (var j = 0; j < list.length; j++) {
      var b = band(list[j]);
      if (b >= 0) { out[b]++; }
    }
    return out;
  }

  // Outcome classes of the "outcome mix" chart, by the return at the window:
  // rugged comes first (whatever the return), then ≤ −50%, −50% to 0% (0
  // included: not a win), above 0% to below +100%, ≥ +100%.
  var OUTCOMES = ['Rugged', '≤ −50%', '−50% to 0%', '0% to +100%', '≥ +100%'];
  function outcome(ret, rugged) {
    if (rugged) { return 0; }
    if (!isNum(ret)) { return -1; }
    if (ret <= -50) { return 1; }
    if (ret <= 0) { return 2; }
    if (ret < 100) { return 3; }
    return 4;
  }

  // The symmetric-log scale of returns: t(r) = sign(r) · log10(1 + |r| / 10),
  // and its inverse.
  function symlog(r) {
    return r < 0 ? -Math.log10(1 - r / 10) : Math.log10(1 + r / 10);
  }
  function symlogInv(t) {
    return t < 0 ? -10 * (Math.pow(10, -t) - 1) : 10 * (Math.pow(10, t) - 1);
  }
  // The log scale of amounts and counts that can be 0: log10(1 + x).
  function log1p10(x) { return Math.log10(1 + Math.max(0, x)); }
  function log1p10Inv(t) { return Math.pow(10, t) - 1; }

  // Ticks of a symmetric-log axis between lo and hi (percent).
  var SYM_TICKS = [-100, -50, -20, 0, 20, 50, 100, 300, 1000, 3000, 1e4, 3e4, 1e5, 1e6, 1e7, 1e8, 1e9];
  function symlogTicks(lo, hi) {
    var out = SYM_TICKS.filter(function (v) { return v >= lo && v <= hi; });
    // thin out when the range is wide (keep 0 and the ends)
    if (out.length > 9) {
      out = out.filter(function (v) { return v === 0 || v === -100 || [-50, 100, 1000, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9].indexOf(v) >= 0; });
    }
    return out;
  }
  // Ticks of a log10(1 + x) axis between 0 and hi: 0, 1, 2, 5, 10, 20, 50 …
  // up to hi; only the powers of ten when that makes more than 8, and every
  // other power of ten when those are still more than 8.
  function log1pTicks(hi) {
    var all = [0], tens = [0];
    for (var p = 1; p <= 1e12 && p <= hi * 1.0001; p *= 10) {
      tens.push(p);
      [1, 2, 5].forEach(function (k) { if (p * k <= hi * 1.0001) { all.push(p * k); } });
    }
    if (all.length <= 8) { return all; }
    if (tens.length <= 8) { return tens; }
    return tens.filter(function (v, i) { return i === 0 || i % 2 === 1; });
  }

  // Monday 00:00 UTC of the week of t (Unix seconds), in ms; or the 1st of its month.
  function weekStart(t) {
    var d = new Date(t * 1000);
    var back = (d.getUTCDay() + 6) % 7; // days since Monday
    return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate() - back);
  }
  function monthStart(t) {
    var d = new Date(t * 1000);
    return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1);
  }
  // The hour (0–23) and weekday (0 = Monday … 6 = Sunday) of t in UTC.
  function hourWeekday(t) {
    var d = new Date(t * 1000);
    return { hour: d.getUTCHours(), weekday: (d.getUTCDay() + 6) % 7 };
  }

  // "$100 on every call": points = [{ t, ret }] (any order). Returns the
  // running profit or loss in dollars after each call, in time order: $100 ×
  // ret / 100 = ret dollars per call, each return capped at +cap% (default
  // 1,000%). Same time: by the order given.
  function cumulativePL(points, cap) {
    if (cap === undefined) { cap = 1000; }
    var p = points.map(function (x, i) { return { t: x.t, ret: x.ret, i: i }; });
    p.sort(function (a, b) { return a.t - b.t || a.i - b.i; });
    var cum = 0;
    var out = [];
    for (var i = 0; i < p.length; i++) {
      cum += Math.min(p[i].ret, cap);
      out.push({ t: p[i].t, cum: cum });
    }
    return out;
  }

  // "Gave back more than half of the peak": among the pairs { peak, ret } with
  // a peak above 0, those whose return is below half of the peak.
  function gaveBack(pairs) {
    var k = 0, n = 0;
    for (var i = 0; i < pairs.length; i++) {
      var p = pairs[i];
      if (!isNum(p.peak) || !isNum(p.ret) || p.peak <= 0) { continue; }
      n++;
      if (p.ret < p.peak / 2) { k++; }
    }
    return { k: k, n: n };
  }

  // A fixed offset in [−0.5, 0.5) for a call id: the jitter of a point, the
  // same on every drawing.
  function jitter(id) {
    var h = (id | 0) ^ 0x9e3779b9;
    h = Math.imul(h ^ (h >>> 16), 0x85ebca6b);
    h = Math.imul(h ^ (h >>> 13), 0xc2b2ae35);
    h ^= h >>> 16;
    return (h >>> 0) / 4294967296 - 0.5;
  }

  // Short numbers for labels: 1234 → "1.2k", 45000 → "45k", 2.5e6 → "2.5M".
  // A value that rounds up to 1,000 of a unit is written in the next one:
  // 999.96 → "1k", 999999 → "1M" (as 1000 → "1k"; ".0" is never written).
  var UNITS = [{ d: 1, s: '' }, { d: 1e3, s: 'k' }, { d: 1e6, s: 'M' }, { d: 1e9, s: 'B' }];
  function shortNum(v) {
    var a = Math.abs(v);
    var u = UNITS.length - 1;
    while (u > 0 && a < UNITS[u].d) { u--; }
    var s = trim(a / UNITS[u].d);
    if (Number(s) >= 1000 && u < UNITS.length - 1) {
      u++;
      s = trim(a / UNITS[u].d);
    }
    return (v < 0 ? '−' : '') + s + UNITS[u].s;
  }
  function trim(x) {
    var s = x >= 100 ? x.toFixed(0) : x >= 10 ? x.toFixed(1) : x.toFixed(x === Math.round(x) ? 0 : 1);
    return s.replace(/\.0$/, '');
  }
  function shortUSD(v) { return (v < 0 ? '−$' : '$') + shortNum(Math.abs(v)); }
  // A signed percentage for axis ticks and group labels: one decimal below
  // 10, none from 10, short from 1,000 ("+1.2k%"). A value that rounds to 0
  // is "0%" without a sign (like the tables' "0.0%": not a win, not a loss).
  function signedPct(v) {
    var a = Math.abs(v);
    var body = a >= 1000 ? shortNum(a) : a >= 10 ? a.toFixed(0) : a.toFixed(1).replace(/\.0$/, '');
    if (Number(body) === 0) { return '0%'; }
    if (body === '1000') { body = shortNum(1000); }
    return (v > 0 ? '+' : '−') + body + '%';
  }

  // The label of a group of a number factor: kind = 'usd' | 'count' | 'pct'.
  // A range whose ends would print the same ("$1.3k–$1.3k" for 1,250 to
  // 1,260) is written with more significant digits ("$1.25k–$1.26k").
  function rangeLabel(g, kind) {
    return rangeText(g, kind, rangeLevel(g, kind, 0));
  }

  // The labels of the groups of a number factor, in order. Like rangeLabel,
  // and when a group's end does not show as below the next group's start
  // ("…–$1.2k", "$1.2k–…" for 1,220 and 1,240; or "…–$1.04k", "$1k–…")
  // both get more digits, so groups never look as if they overlap.
  function rangeLabels(groups, kind) {
    var lv = groups.map(function (g) { return rangeLevel(g, kind, 0); });
    for (var changed = true; changed;) {
      changed = false;
      for (var i = 0; i + 1 < groups.length; i++) {
        var a = groups[i], b = groups[i + 1];
        if (a.hi >= b.lo || shownValue(fmtAt(a.hi, kind, lv[i])) < shownValue(fmtAt(b.lo, kind, lv[i + 1]))) { continue; }
        [i, i + 1].forEach(function (j) {
          if (lv[j] < LEVELS.length - 1) { lv[j] = rangeLevel(groups[j], kind, lv[j] + 1); changed = true; }
        });
      }
    }
    return groups.map(function (g, i) { return rangeText(g, kind, lv[i]); });
  }

  // Precision levels of labels: the short default, then 3 to 6 significant
  // digits.
  var LEVELS = [0, 3, 4, 5, 6];
  function fmtAt(v, kind, level) {
    var sig = LEVELS[level];
    if (kind === 'usd') { return (v < 0 ? '−$' : '$') + shortNumSig(Math.abs(v), sig); }
    if (kind === 'pct') { return signedPctSig(v, sig); }
    return shortNumSig(v, sig);
  }
  // The lowest level from `from` at which the ends of the group differ.
  function rangeLevel(g, kind, from) {
    var l = from;
    while (g.lo !== g.hi && l < LEVELS.length - 1 && fmtAt(g.lo, kind, l) === fmtAt(g.hi, kind, l)) { l++; }
    return l;
  }
  function rangeText(g, kind, level) {
    if (g.lo === g.hi) { return fmtAt(g.lo, kind, level); }
    return fmtAt(g.lo, kind, level) + '–' + fmtAt(g.hi, kind, level);
  }

  // The number a label shows: "−$1.25k" → −1250, "+4.1%" → 4.1.
  var SUFFIX = { k: 1e3, M: 1e6, B: 1e9 };
  function shownValue(s) {
    var neg = s.charAt(0) === '−';
    var body = s.replace(/[−+$%]/g, '');
    var m = SUFFIX[body.slice(-1)] || 1;
    var x = parseFloat(m === 1 ? body : body.slice(0, -1)) * m;
    return neg ? -x : x;
  }

  // shortNum with sig significant digits (0 = shortNum itself).
  function shortNumSig(v, sig) {
    if (!sig) { return shortNum(v); }
    var a = Math.abs(v);
    var u = UNITS.length - 1;
    while (u > 0 && a < UNITS[u].d) { u--; }
    var s = sigDigits(a / UNITS[u].d, sig);
    if (Number(s) >= 1000 && u < UNITS.length - 1) {
      u++;
      s = sigDigits(a / UNITS[u].d, sig);
    }
    return (v < 0 && Number(s) !== 0 ? '−' : '') + s + UNITS[u].s;
  }
  // x (≥ 0) with sig significant digits, trailing zeros dropped, never in
  // exponent form: 1.25 → "1.25", 1.2 → "1.2", 999.96 → "1000", 0.0123 → "0.0123".
  function sigDigits(x, sig) {
    if (x === 0) { return '0'; }
    var e = Math.floor(Math.log10(x));
    var dec = Math.max(0, sig - 1 - e);
    var s = x.toFixed(Math.min(dec, 20));
    if (s.indexOf('.') >= 0) { s = s.replace(/0+$/, '').replace(/\.$/, ''); }
    return s;
  }
  // signedPct with sig significant digits (0 = signedPct itself); a value
  // that rounds to 0 is still "0%".
  function signedPctSig(v, sig) {
    if (!sig) { return signedPct(v); }
    var a = Math.abs(v);
    var body = a >= 1000 ? shortNumSig(a, sig) : sigDigits(a, sig);
    if (Number(body) === 0) { return '0%'; }
    if (body === '1000') { body = shortNumSig(1000, sig); }
    return (v > 0 ? '+' : '−') + body + '%';
  }

  var api = {
    isNum: isNum, mean: mean, median: median, summarize: summarize,
    equalCountGroups: equalCountGroups, BANDS: BANDS, band: band, histogram: histogram,
    OUTCOMES: OUTCOMES, outcome: outcome,
    symlog: symlog, symlogInv: symlogInv, log1p10: log1p10, log1p10Inv: log1p10Inv,
    symlogTicks: symlogTicks, log1pTicks: log1pTicks,
    weekStart: weekStart, monthStart: monthStart, hourWeekday: hourWeekday, DAY_MS: DAY_MS,
    cumulativePL: cumulativePL, gaveBack: gaveBack, jitter: jitter,
    shortNum: shortNum, shortUSD: shortUSD, signedPct: signedPct, rangeLabel: rangeLabel,
    rangeLabels: rangeLabels
  };
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  } else {
    root.AnaStats = api;
  }
}(this));
