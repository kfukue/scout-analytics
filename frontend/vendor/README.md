# frontend/vendor

Third-party files served with the website, kept byte for byte as built
(`.gitattributes` here turns off line-ending conversion). The Go server embeds,
gzips and ETags them like the page's own files; only `.js` and `.txt` are
served (this README is embedded but not served).

## echarts-6.1.0.custom.min.js

A custom build of **Apache ECharts 6.1.0** (Apache-2.0) for the Analytics
page's Timing tab, loaded by `analytics.html` with `<script src defer>` before
`analytics.js`. It defines one global, `window.echarts` (an esbuild IIFE with
`--global-name=echarts`, not a UMD wrapper; fine for a plain `<script>`).

| | |
|---|---|
| File | `echarts-6.1.0.custom.min.js`, 686,269 bytes (231,378 with `gzip -9`) |
| sha256 | `11c0d37dd643ce90816bb4825e557658b3a37a887c494cb49e8c79aed8678ba8` |
| Source | npm `echarts@6.1.0`, <https://registry.npmjs.org/echarts/-/echarts-6.1.0.tgz> |
| Tarball integrity (npm) | `sha512-q0yaFPggC9FUdsWH4blavRWFmxdrIodbkoKNAjJudAI6CA9gNPxHtV2RcZNEepZVlk4yvBYkOkbk6HIVpIyHZA==` (shasum `ae0f68590f5ebbd728d900907c27acde7c5456d1`) |
| Bundled dependencies | `zrender@6.1.0` (BSD-3-Clause), integrity `sha512-oEGMDB6pOP2S6OwRR4PdVv610zrjnA3Bh+JnSG12fYJlBKjtNAoEb5fSUoCOOINlH96I2fU38/A2UpRKs67xYQ==`; `tslib@2.3.0` (0BSD), integrity `sha512-N82ooyxVNm6h1riLCoyS9e3fuJ3AMG2zIZs2Gd1ATcSFjSA23Q0fzjjZeh0jbJvWVDZ0cJT8yaNNaaXHzueNjg==` |
| Bundler | `esbuild@0.25.12`, integrity `sha512-bbPBYYrtZbkt6Os6FiTLCTFxvq4tt3JKall1vRwshA3fdVztsLAatFaZobhkBC8/BrPetoa0oksYoKXoG4ryJg==` (Windows binary `@esbuild/win32-x64@0.25.12`, `sha512-alJC0uCZpTFrSL0CCDjcgleBXPnCrEAhTBILpeAp7M/OFgoqtAetfBzX0xM00MUsVVPpVjlPuMbREqnZCXaTnA==`) |

The integrities above are the ones the npm registry publishes
(`npm view echarts@6.1.0 dist.integrity`); the downloaded tarballs were checked
against them (sha512, base64). Two independent builds from them (the first when
the file was added, a second from freshly downloaded tarballs) gave the same
sha256.

### What is in it

Tree-shaken `echarts/core` with the canvas renderer and only these parts:

- charts: bar, line, scatter, boxplot, heatmap, candlestick;
- components: grid, tooltip, dataZoom, brush, visualMap, aria, legend.

This is the set the project chose for its charts, plus **line**, which the
Timing tab needs for its step curves ("still at 2× after t", "share rugged by
t"). No SVG renderer, no maps, no `<style>`
injection, no `eval` or `new Function` (checked with grep). The library's own
tooltip builds HTML with inline styles, which the page's Content-Security-Policy
blocks, so the page always passes a tooltip `formatter` that returns an element
built with `textContent`.

### How to rebuild it

In an empty folder **outside the repo** (Node.js 22, npm 10):

`package.json`:

```json
{
  "name": "scout-echarts-build",
  "private": true,
  "version": "0.0.0",
  "dependencies": {
    "echarts": "6.1.0",
    "esbuild": "0.25.12"
  }
}
```

`entry.js`:

```js
// Custom ECharts build for scout analytics (frontend/vendor): only the
// components the project uses, canvas renderer only.
import * as echarts from 'echarts/core';
import { BarChart, LineChart, ScatterChart, BoxplotChart, HeatmapChart, CandlestickChart } from 'echarts/charts';
import {
  GridComponent, TooltipComponent, DataZoomComponent, BrushComponent, VisualMapComponent,
  AriaComponent, LegendComponent
} from 'echarts/components';
import { CanvasRenderer } from 'echarts/renderers';

echarts.use([
  BarChart, LineChart, ScatterChart, BoxplotChart, HeatmapChart, CandlestickChart,
  GridComponent, TooltipComponent, DataZoomComponent, BrushComponent, VisualMapComponent,
  AriaComponent, LegendComponent, CanvasRenderer
]);

export * from 'echarts/core';
```

Then:

```sh
npm install --ignore-scripts --no-audit --no-fund
npm ls --all          # echarts 6.1.0, zrender 6.1.0, tslib 2.3.0, esbuild 0.25.12
grep '"integrity"' package-lock.json   # compare with the table above
npx esbuild entry.js --bundle --minify --format=iife --global-name=echarts \
  --legal-comments=eof --target=es2017 --outfile=out/echarts-6.1.0.custom.min.js
sha256sum out/echarts-6.1.0.custom.min.js   # must print the sha256 above
```

(`--ignore-scripts` skips esbuild's post-install check; its platform binary
comes from the `@esbuild/<platform>` optional package. On Windows
`node_modules/@esbuild/win32-x64/esbuild.exe` can be run directly with the same
arguments.) Another esbuild version or `--target` gives a different file: the
target matters because esbuild writes `typeof x<"u"` for newer targets and
`typeof x!="undefined"` for es2017.

### Licences

- `ECHARTS-NOTICE.txt` is `echarts/NOTICE` from the package, unchanged
  (sha256 `d491d358344f842685c1b1585970999db65fe30ecf7ef3867af8814f4016c016`).
- `ECHARTS-LICENSE.txt` (sha256
  `35096b37bef36449bf289badcfae8dd004e7482af50d4b5926d3fe6594bdc73c`) is,
  after a one-line heading, the licence files of what the build contains, each
  under a `===== <path in node_modules> =====` heading and unchanged apart from
  line endings (LF) and a final newline: `echarts/LICENSE` (Apache License 2.0,
  with its list of subcomponents), `echarts/licenses/LICENSE-d3` (BSD-3-Clause,
  d3 code inside ECharts), `zrender/LICENSE` (BSD-3-Clause) and
  `tslib/LICENSE.txt` (0BSD).
- The build itself ends with esbuild's legal comments (tslib and ZRender).

### Updating

Use a release at or above 6.1.0 (6.0 had a large-scatter zoom freeze, fixed in
6.1.0). Change the versions above, rebuild, rename the file to the new version,
change the `<script src>` in `analytics.html`, refresh the licence files from
the new package, grep the new file for `new Function`, `eval(` and `<style`,
check the page in a browser for Content-Security-Policy errors, and record the
new hashes here.
