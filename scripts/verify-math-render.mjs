#!/usr/bin/env node
// Pins internal/docs/assets/math-init.js in a real browser (forgectl#596).
// CI has no browser harness, so run this by hand after any math-init.js or
// KaTeX change and say so in the PR.
//
// It serves a scratch vault root and a scratch docs root with `forgectl
// docs serve` and loads each fixture page in Chromium, one page per
// fixture, so a crash names its case:
//
//   - ordinary inline and display math renders, delimiters stripped, TeX
//     stashed in data-math-source, and ForgectlMath.refresh() re-renders it;
//   - a mid-line $$ is prose in the docs root, and $$ on its own lines is
//     display math there;
//   - single-dollar math is math in the vault root and prose in the docs
//     root;
//   - the source scan: nesting at its limit (depth 100) renders; past it the
//     formula is left as TeX source (.math-skipped) and the tab survives,
//     including \left and braces mixed so neither count alone passes 100, a
//     %-comment that hides closers, and surplus closers that must not bank
//     credit for later openers;
//   - escaped braces (\{) are literals and do not count as nesting;
//   - a source over 10,000 characters is left as TeX source;
//   - the DOM bound: output at most 500 levels deep renders (for \frac,
//     pmatrix, \boxed and subscripts nested to just under it), and output
//     past it is left as TeX source, including \def and \newcommand
//     macros that nest deep from shallow source;
//   - the node bound: macros that multiply output past 100,000 nodes are
//     left as TeX source;
//   - maxExpand is 500, and maxSize caps \rule{100000em}{…} at 500em.
//
// Usage: node scripts/verify-math-render.mjs [path/to/forgectl]
//   Without a path it builds one with `go build` into a scratch dir.
//   PLAYWRIGHT_MODULE overrides the Playwright import (default: the global
//   install, `$(npm root -g)/playwright/index.mjs`). CHROMIUM_PATH sets the
//   browser executable (claude.ai cloud sessions: /opt/pw-browsers/chromium).
// Exit 0 and "VERDICT: PASS" when every case holds; non-zero otherwise.

import { execFileSync, spawn } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
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

// Each fixture is one document. `want` is 'rendered', 'skipped' (left as
// TeX source, .math-skipped), 'error' (a KaTeX too-many-expansions parse
// error) or 'none' (no .math element at all). Fixtures
// live in a vault root, because single-dollar inline math is recognized
// only there (forgectl#600); `root: 'docs'` puts one in a plain docs root.
const fixtures = {
  'plain.md': {
    body: 'Inline $a^2$ here.\n\n$$\n\\sum_i x_i\n$$\n',
    want: 'rendered',
  },
  // In a docs root, $…$ is shell prose, not math.
  'docs-dollars.md': {
    root: 'docs',
    body: 'Set $HOME/bin:$PATH and pay $5 or $6.\n',
    want: 'none',
  },
  // In a docs root, a mid-line $$ is the shell's PID or currency, not math
  // (forgectl#650)...
  'docs-pid.md': {
    root: 'docs',
    body: 'Run tmp=/tmp/x.$$; rm /tmp/y.$$ as PID $$ of the shell, for $$5 or $$10.\n',
    want: 'none',
  },
  // ...while $$ on lines of its own, even straight after text, is still
  // display math there.
  'docs-display.md': {
    root: 'docs',
    body: 'Display:\n$$\n\\sum_i x_i\n$$\n',
    want: 'rendered',
  },
  // The source scan: at most 100 levels of nesting. Plain groups build
  // about one DOM level each, so only the source scan can skip these.
  'groups-at-limit.md': {
    body: '$' + rep('{', 100) + 'x' + rep('}', 100) + '$\n',
    want: 'rendered',
  },
  'groups-over-limit.md': {
    body: '$' + rep('{', 101) + 'x' + rep('}', 101) + '$\n',
    want: 'skipped',
  },
  // 60 \left( and 60 braces: each kind alone stays under 100.
  'mixed-deep.md': {
    body: '$$' + rep('\\left(', 60) + rep('{', 60) + 'x' + rep('}', 60) + rep('\\right)', 60) + '$$\n',
    want: 'skipped',
  },
  // KaTeX ignores a % comment, so the "}" after each % closes nothing.
  'comment-deep.md': {
    body: '$$\n' + rep('{%}\n', 150) + 'x' + rep('}', 150) + '\n$$\n',
    want: 'skipped',
  },
  // Leading surplus closers must not offset the openers that follow.
  'surplus-closers.md': {
    body: '$$' + rep('}', 150) + rep('{', 150) + 'x' + rep('}', 150) + '$$\n',
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
  // Tab crashes the source scan alone caught on origin/main.
  'frac-deep.md': {
    body: '$$' + rep('\\frac{1}{', 200) + 'x' + rep('}', 200) + '$$\n',
    want: 'skipped',
  },
  'matrix-deep.md': {
    body: '$$' + rep('\\begin{matrix}', 200) + 'x' + rep('\\end{matrix}', 200) + '$$\n',
    want: 'skipped',
  },
  // The DOM bound: 70 nested fractions are 498 levels of output, 71 are
  // 505. Both pass the source scan, so only the DOM bound skips 71.
  'dom-at-cap.md': {
    body: '$$' + rep('\\frac{1}{', 70) + 'x' + rep('}', 70) + '$$\n',
    want: 'rendered',
    minDepth: 480,
  },
  'dom-over-cap.md': {
    body: '$$' + rep('\\frac{1}{', 71) + 'x' + rep('}', 71) + '$$\n',
    want: 'skipped',
  },
  // Constructs that build more DOM levels per TeX level than \frac, nested
  // to just under the DOM bound: they render and the tab survives.
  'pmatrix-near-cap.md': {
    body: '$$' + rep('\\begin{pmatrix}', 53) + 'x' + rep('\\end{pmatrix}', 53) + '$$\n',
    want: 'rendered',
    minDepth: 480,
  },
  'boxed-near-cap.md': {
    body: '$$' + rep('\\boxed{', 60) + 'x' + rep('}', 60) + '$$\n',
    want: 'rendered',
    minDepth: 480,
  },
  'sub-near-cap.md': {
    body: '$$' + rep('x_{', 69) + 'x' + rep('}', 69) + '$$\n',
    want: 'rendered',
    minDepth: 480,
  },
  // Macro expansion the source scan cannot see: braces never nest past 6,
  // but each macro applies the one before four times, so the output nests
  // 256 fractions (about 1,800 DOM levels), and 128 with \newcommand.
  'macro-deep-def.md': {
    body: '$\\def\\fa#1{\\frac1{#1}}\\def\\fb#1{\\fa{\\fa{\\fa{\\fa{#1}}}}}\\def\\fc#1{\\fb{\\fb{\\fb{\\fb{#1}}}}}\\def\\fd#1{\\fc{\\fc{\\fc{\\fc{#1}}}}}\\fd{\\fd{\\fd{\\fd{x}}}}$\n',
    want: 'skipped',
  },
  'macro-deep-newcommand.md': {
    body: '$$\\newcommand\\fa[1]{\\frac1{#1}}\\newcommand\\fb[1]{\\fa{\\fa{\\fa{\\fa{#1}}}}}\\newcommand\\fc[1]{\\fb{\\fb{\\fb{\\fb{#1}}}}}\\newcommand\\fd[1]{\\fc{\\fc{\\fc{\\fc{#1}}}}}\\fd{\\fd{x}}$$\n',
    want: 'skipped',
  },
  // Macro expansion that multiplies width, not depth: 3,000 \binom11 is
  // about 114,000 nodes, over the node bound, from 200 characters.
  'macro-wide.md': {
    body: '$\\def\\fa{' + rep('\\binom11', 10) + '}\\def\\fb{' + rep('\\fa', 10) + '}\\def\\fc{' + rep('\\fb', 10) + '}\\fc\\fc\\fc$\n',
    want: 'skipped',
  },
  // maxExpand is 500, half KaTeX's default: 600 expansions of a one-token
  // macro is a parse error (source shown in the error color), not output.
  'max-expand.md': {
    body: '$\\def\\fa{x}' + rep('\\fa', 600) + '$\n',
    want: 'error',
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

// The vault root is a vault because it holds a .obsidian directory.
const roots = { vault: join(scratch, 'vault'), docs: join(scratch, 'docs') };
mkdirSync(join(roots.vault, '.obsidian'), { recursive: true });
mkdirSync(roots.docs, { recursive: true });
for (const dir of Object.values(roots)) writeFileSync(join(dir, 'README.md'), '# Math fixtures\n');
for (const [name, f] of Object.entries(fixtures)) {
  writeFileSync(join(roots[f.root || 'vault'], name), `# ${name}\n\n${f.body}\nafter\n`);
}

const port = await freePort();
const base = `http://127.0.0.1:${port}`;
server = spawn(bin, ['docs', 'serve', '--addr', `127.0.0.1:${port}`, roots.vault, roots.docs], { stdio: 'ignore' });
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
    await page.goto(`${base}/doc/${f.root || 'vault'}/${name}`, { waitUntil: 'load', timeout: 30000 });
    await page.waitForFunction(() => window.ForgectlMath !== undefined, null, { timeout: 10000 });
    const r = await page.evaluate(() => [...document.querySelectorAll('.math')].map((el) => {
      const katexEl = el.querySelector('.katex');
      const errorEl = el.querySelector('.katex-error');
      const rule = el.querySelector('.katex-rule');
      return {
        skipped: el.classList.contains('math-skipped'),
        katex: katexEl !== null,
        error: errorEl ? errorEl.title : null,
        display: el.querySelector('.katex-display') !== null,
        text: el.textContent,
        source: el.dataset.mathSource,
        depth: (() => {
          let deepest = 0;
          const stack = [[el, 0]];
          while (stack.length > 0) {
            const [n, d] = stack.pop();
            if (d > deepest) deepest = d;
            for (let c = n.firstChild; c; c = c.nextSibling) stack.push([c, d + 1]);
          }
          return deepest;
        })(),
        ruleWidthEm: rule ? rule.getBoundingClientRect().width / parseFloat(getComputedStyle(rule).fontSize) : null,
      };
    }));
    if (f.want === 'none') {
      if (r.length !== 0) problems.push(`${name}: want no .math element, got ${r.length}`);
    } else if (r.length === 0) {
      problems.push(`${name}: no .math element on the page`);
    }
    for (const m of r) {
      if (f.want === 'error') {
        if (m.skipped || !m.error || !m.error.includes('Too many expansions')) problems.push(`${name}: want a too-many-expansions parse error, got skipped=${m.skipped} error=${m.error}`);
      } else if (f.want === 'skipped') {
        if (!m.skipped || m.katex) problems.push(`${name}: want TeX source left as is, got skipped=${m.skipped} katex=${m.katex}`);
        if (m.text !== m.source) problems.push(`${name}: skipped formula does not show its source`);
      } else {
        if (m.skipped || !m.katex) problems.push(`${name}: want rendered, got skipped=${m.skipped} katex=${m.katex}`);
        if (f.minDepth && m.depth < f.minDepth) problems.push(`${name}: output is ${m.depth} DOM levels deep, want at least ${f.minDepth} to test near the bound`);
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
