#!/usr/bin/env node
// Pins internal/docs/assets/math-init.js in a real browser (forgectl#596).
// CI has no browser harness, so run this by hand after any math-init.js or
// KaTeX change and say so in the PR.
//
// It serves a scratch docs root with `forgectl docs serve` and loads each
// fixture page in Chromium, one page per fixture, so a crash names its case:
//
//   - ordinary inline and display math renders, delimiters stripped, TeX
//     stashed in data-math-source, and ForgectlMath.refresh() re-renders it;
//   - nesting at the limit (depth 100) renders;
//   - nesting past it is left as TeX source (.math-skipped) and the tab
//     survives: \frac{ braces, brace-free nested matrices, the two mixed so
//     neither count alone passes 100, a %-comment that hides closers, and
//     surplus closers that must not bank credit for later openers;
//   - escaped braces (\{) are literals and do not count as nesting;
//   - a source over 10,000 characters is left as TeX source;
//   - maxSize caps \rule{100000em}{…} at 500em.
//
// Usage: node scripts/verify-math-render.mjs [path/to/forgectl]
//   Without a path it builds one with `go build` into a scratch dir.
//   PLAYWRIGHT_MODULE overrides the Playwright import (default: the global
//   install, `$(npm root -g)/playwright/index.mjs`). CHROMIUM_PATH sets the
//   browser executable (claude.ai cloud sessions: /opt/pw-browsers/chromium).
// Exit 0 and "VERDICT: PASS" when every case holds; non-zero otherwise.

import { execFileSync, spawn } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

const scratch = mkdtempSync(join(tmpdir(), 'forgectl-math-'));
let server;
function cleanup() {
  if (server && server.exitCode === null) server.kill();
  rmSync(scratch, { recursive: true, force: true });
}

function fail(msg) {
  console.error(`VERDICT: FAIL ${msg}`);
  cleanup();
  process.exit(1);
}

async function loadPlaywright() {
  let spec = process.env.PLAYWRIGHT_MODULE;
  if (!spec) {
    const root = execFileSync('npm', ['root', '-g'], { encoding: 'utf8' }).trim();
    spec = pathToFileURL(join(root, 'playwright', 'index.mjs')).href;
  }
  return import(spec);
}

function freePort() {
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.once('error', reject);
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
  });
}

const rep = (s, n) => s.repeat(n);

// Each fixture is one document. `want` is 'rendered' or 'skipped'.
const fixtures = {
  'plain.md': {
    body: 'Inline $a^2$ here.\n\n$$\n\\sum_i x_i\n$$\n',
    want: 'rendered',
  },
  'frac-at-limit.md': {
    body: '$$' + rep('\\frac{1}{', 100) + 'x' + rep('}', 100) + '$$\n',
    want: 'rendered',
  },
  'frac-deep.md': {
    body: '$$' + rep('\\frac{1}{', 200) + 'x' + rep('}', 200) + '$$\n',
    want: 'skipped',
  },
  'matrix-deep.md': {
    body: '$$' + rep('\\begin{matrix}', 200) + 'x' + rep('\\end{matrix}', 200) + '$$\n',
    want: 'skipped',
  },
  // 60 matrices and 60 fractions: each kind alone stays under 100.
  'mixed-deep.md': {
    body: '$$' + rep('\\begin{matrix}\\frac{1}{', 60) + 'x' + rep('}\\end{matrix}', 60) + '$$\n',
    want: 'skipped',
  },
  // KaTeX ignores a % comment, so the "}" after each % closes nothing.
  'comment-deep.md': {
    body: '$$\n' + rep('\\frac{1}{%}\n', 150) + 'x' + rep('}', 150) + '\n$$\n',
    want: 'skipped',
  },
  // Leading surplus closers must not offset the openers that follow.
  'surplus-closers.md': {
    body: '$$' + rep('}', 150) + rep('\\frac{1}{', 150) + 'x' + rep('}', 150) + '$$\n',
    want: 'skipped',
  },
  'escaped-braces.md': {
    body: '$' + rep('\\{', 150) + 'x' + rep('\\}', 150) + '$\n',
    want: 'rendered',
  },
  'long-source.md': {
    body: '$' + rep('a+', 5001) + 'a$\n',
    want: 'skipped',
  },
  'huge-rule.md': {
    body: 'Rule $\\rule{100000em}{1em}$ end.\n',
    want: 'rendered',
  },
};

const bin = process.argv[2] || (() => {
  const out = join(scratch, 'forgectl');
  execFileSync('go', ['build', '-o', out, '.'], { stdio: 'inherit' });
  return out;
})();

const root = join(scratch, 'root');
execFileSync('mkdir', ['-p', root]);
writeFileSync(join(root, 'README.md'), '# Math fixtures\n');
for (const [name, f] of Object.entries(fixtures)) {
  writeFileSync(join(root, name), `# ${name}\n\n${f.body}\nafter\n`);
}

const port = await freePort();
const base = `http://127.0.0.1:${port}`;
server = spawn(bin, ['docs', 'serve', '--addr', `127.0.0.1:${port}`, root], { stdio: 'ignore' });
let up = false;
for (let i = 0; i < 100 && !up; i++) {
  try { up = (await fetch(`${base}/`)).ok; } catch { await new Promise((r) => setTimeout(r, 100)); }
}
if (!up) fail(`docs serve did not come up on ${base}`);

const { chromium } = await loadPlaywright();
const launch = process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {};
const browser = await chromium.launch(launch);
const problems = [];

for (const [name, f] of Object.entries(fixtures)) {
  const page = await browser.newPage();
  let crashed = false;
  page.on('crash', () => { crashed = true; });
  try {
    // Never 'networkidle': the live-reload SSE stream never goes idle.
    await page.goto(`${base}/doc/root/${name}`, { waitUntil: 'load', timeout: 30000 });
    await page.waitForFunction(() => window.ForgectlMath !== undefined, null, { timeout: 10000 });
    const r = await page.evaluate(() => [...document.querySelectorAll('.math')].map((el) => {
      const katexEl = el.querySelector('.katex');
      const rule = el.querySelector('.katex-rule');
      return {
        skipped: el.classList.contains('math-skipped'),
        katex: katexEl !== null,
        display: el.querySelector('.katex-display') !== null,
        text: el.textContent,
        source: el.dataset.mathSource,
        ruleWidthEm: rule ? rule.getBoundingClientRect().width / parseFloat(getComputedStyle(rule).fontSize) : null,
      };
    }));
    if (r.length === 0) {
      problems.push(`${name}: no .math element on the page`);
    }
    for (const m of r) {
      if (f.want === 'skipped') {
        if (!m.skipped || m.katex) problems.push(`${name}: want TeX source left as is, got skipped=${m.skipped} katex=${m.katex}`);
        if (m.text !== m.source) problems.push(`${name}: skipped formula does not show its source`);
      } else {
        if (m.skipped || !m.katex) problems.push(`${name}: want rendered, got skipped=${m.skipped} katex=${m.katex}`);
      }
    }
    if (name === 'plain.md') {
      const [inline, display] = r;
      if (!inline || inline.source !== '$a^2$' || inline.text.includes('$')) problems.push(`plain.md: inline delimiters not stripped or source not stashed: ${JSON.stringify(inline)}`);
      if (!display || !display.display) problems.push('plain.md: display math has no .katex-display');
      const again = await page.evaluate(() => {
        window.ForgectlMath.refresh();
        return [...document.querySelectorAll('.math')].every((el) => el.querySelector('.katex') !== null);
      });
      if (!again) problems.push('plain.md: ForgectlMath.refresh() lost a rendered formula');
    }
    if (name === 'huge-rule.md') {
      const w = r[0] && r[0].ruleWidthEm;
      if (w === null || w === undefined || w > 501) problems.push(`huge-rule.md: \\rule width ${w}em, want at most maxSize (500em)`);
    }
  } catch (err) {
    problems.push(`${name}: ${String(err).split('\n')[0]}`);
  }
  if (crashed) problems.push(`${name}: the tab crashed`);
  await page.close().catch(() => {});
}

await browser.close();
cleanup();
if (problems.length > 0) {
  for (const p of problems) console.error(`  - ${p}`);
  console.error(`VERDICT: FAIL ${problems.length} problem(s)`);
  process.exit(1);
}
console.log(`VERDICT: PASS ${Object.keys(fixtures).length} fixtures`);
