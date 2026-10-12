---
name: scout-web
description: Rules for the scout analytics website (-web server, snapshot, plain JavaScript in frontend/, CSP, charts).
---

# Scout analytics: website

For the Go side of the website, also load `scout-go`.

## Website (`-web`)

- Requests are answered from the in-memory snapshot (`websnapshot.go`) and must
  never query the database. The only exception is `POST /api/refresh`, which
  goes through the single shared refresh mechanism (one DB read at a time,
  coalesced with the background loop).
- Keep p95 under 10 ms. Run
  `go test -run xxx -bench BenchmarkWebSnapshot -benchmem .`
  before and after any website change, and report both (who runs which number:
  `TESTING.md` "Website").
- Anything shown per row must also be part of the pre-encoded row JSON and of
  `hashWebRows`, or the snapshot version/ETag won't change and clients see
  stale data.
- The front end is plain JavaScript in `frontend/`. Build the DOM with
  `textContent` only (token names and bot texts are arbitrary), with no inline
  script or style (strict Content-Security-Policy).
- Check pages at 1280 px and 390 px, light and dark, with headless Edge/Chrome
  if available (browser rules in `TESTING.md`); otherwise list what the owner
  must check by eye.
- `web_db_test.go`'s `webCallJSON` decodes with `DisallowUnknownFields`: add
  every new JSON field there.

## Plain JavaScript and CSP

1. Never use `innerHTML`, `outerHTML`, `insertAdjacentHTML`, `document.write`,
   `eval`, `new Function` or `setTimeout` with a string; use `createElement` +
   `textContent`.
   ([OWASP DOM XSS](https://cheatsheetseries.owasp.org/cheatsheets/DOM_based_XSS_Prevention_Cheat_Sheet.html))
2. Check the URL scheme before setting `href`/`src`; allow only `https:`.
3. `addEventListener` only (inline `on*=` is blocked by the CSP); prefer one
   delegated listener on the table.
4. `rel="noopener noreferrer"` on every `target="_blank"` link.
5. Never loosen the CSP (`web.go`); `checkSecurityHeaders` fails on
   `unsafe-inline`/`unsafe-eval`. Report any CSP change.
   ([MDN CSP](https://developer.mozilla.org/en-US/docs/Web/HTTP/Guides/CSP))
6. Sortable headers: a `<button>` inside the `<th>`, `aria-sort` on the active
   column; abbreviated numbers keep the full value in `title`.
   ([WAI-ARIA APG sortable table](https://www.w3.org/WAI/ARIA/apg/patterns/table/examples/sortable-table/))
7. Real elements before ARIA; visible focus; everything works by keyboard;
   WCAG AA contrast in light and dark.
8. Update rows in place (keyed rows / `DocumentFragment`); respect ETag/304; no
   new libraries or CDNs, except vendored files approved in the task.
9. Respect `prefers-reduced-motion` and `prefers-color-scheme`; when a fetch
   fails, show an error state, never silently stale data.

## Charts

Decided by the owner after research. The shipped Analytics charts are
hand-built SVG, not ECharts (see `HANDOFF.md` section 4 item 5); the rules
below apply to new chart work.

- Library: **Apache ECharts 6.1.x or later** (Apache-2.0), vendored as ONE
  custom-built UMD file at `frontend/vendor/echarts-<version>.custom.min.js`,
  with `frontend/vendor/ECHARTS-LICENSE.txt` and `ECHARTS-NOTICE.txt`. Load it
  with `<script src defer>` before `app.js`.
  - The Go server embeds, gzips and ETags it automatically. Only `.js` is
    served (no `.mjs` or `.map`).
  - Build it with only the needed components: scatter, bar, boxplot, heatmap,
    candlestick, grid, tooltip, dataZoom, brush, visualMap, aria, legend;
    canvas renderer. ([ECharts import guide](https://echarts.apache.org/handbook/en/basics/import/))
  - Record the version and the file's sha256 in the report.
  - 6.0 had a large-scatter zoom freeze, fixed in 6.1.0
    ([releases](https://github.com/apache/echarts/releases)).
- Before shipping, grep the vendored file for `new Function`, `eval(` and
  `<style`, and check DevTools for CSP violations.
- **CSP gotcha:** the default ECharts tooltip builds HTML with `style=""` via
  `innerHTML`, which our CSP blocks. Always use a tooltip
  [`formatter`](https://echarts.apache.org/en/option.html#tooltip.formatter)
  that returns an HTMLElement built with `textContent`, plus `className`
  (styles in `style.css`) and `confine: true`. `renderMode: 'richText'` is the
  fallback.
- Large data: `large: true` (with `largeThreshold`) for scatter. Large mode
  draws one colour per series, so colouring by category means one series per
  category. Use `dataZoom` and `brush` for interaction. Resize with a
  `ResizeObserver`; theme through registered light/dark themes that use the CSS
  variables.
- There is no symlog axis. For returns (−100% … +10,000%) plot
  `t(r) = sign(r) * log10(1 + |r|/10)`, set ticks with
  `axisLabel.customValues` and invert in the label formatter; or plot the
  multiple `1 + r/100` on a `log` axis with a floor.
- Data comes from a compact columnar endpoint built once per snapshot (e.g.
  `GET /api/points`, plain + gzip, ETag = snapshot version), never from the
  paged `/api/calls`.
- Specialists only if ECharts proves too slow: uPlot (MIT, asinh scale) for
  100k-point time series; TradingView Lightweight Charts for candles (its
  licence requires a TradingView attribution link). No 3D (judged not worth it
  for this data).
- Not allowed: Plotly scattergl / regl / deck.gl (need `unsafe-eval` or blob
  workers), Highcharts (commercial licence), SVG-based libraries for more than
  5k points.
