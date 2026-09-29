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
//   - a formula that defines a macro (\def, \global\def, \newcommand, …)
//     is left as TeX source before KaTeX runs, fast, with a tooltip that
//     says so (forgectl#675), including one hidden behind \verb|%|; control
//     words that only start with a definer's name (\define, \letter) are
//     not definitions;
//   - the DOM bound: output at most 250 levels deep renders (for \frac,
//     pmatrix, \boxed and subscripts nested to just under it), and output
//     past it is left as TeX source;
//   - the node bound: a macro-free empty matrix row that builds more than
//     100,000 nodes is left as TeX source;
//   - maxExpand is 500 (250 \dots render, 251 do not), and maxSize caps
//     \rule{100000em}{…} at 500em.
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

// The tooltips math-init.js gives a skipped formula.
const TOO_COMPLEX = 'Not rendered: this formula is too long, too large or too deeply nested to render safely.';
const DEFINES_MACRO = 'Not rendered: this formula defines a macro (\\def, \\newcommand, \\let, …), which could make it too slow to render safely.';

// Each fixture is one document. `want` is 'rendered', 'skipped' (left as
// TeX source, .math-skipped), 'error' (a KaTeX parse error whose message
// includes the fixture's `error`) or 'none' (no .math element at all). A skipped
// fixture's `title` is the tooltip it must carry, and `maxMs` bounds the
// time from navigation to the render finishing. Fixtures
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
    title: TOO_COMPLEX,
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
  // The DOM bound: 34 nested fractions are 246 levels of output, 35 are
  // 253. Both pass the source scan, so only the DOM bound skips 35.
  'dom-at-cap.md': {
    body: '$$' + rep('\\frac{1}{', 34) + 'x' + rep('}', 34) + '$$\n',
    want: 'rendered',
    minDepth: 240,
  },
  'dom-over-cap.md': {
    body: '$$' + rep('\\frac{1}{', 35) + 'x' + rep('}', 35) + '$$\n',
    want: 'skipped',
    title: TOO_COMPLEX,
  },
  // The DOM bound at 240 and 260 levels (forgectl#675): 26 pmatrix are
  // 240 levels and render, 36 fractions are 260 and do not.
  'nest-240.md': {
    body: '$$' + rep('\\begin{pmatrix}', 26) + 'x' + rep('\\end{pmatrix}', 26) + '$$\n',
    want: 'rendered',
    minDepth: 240,
  },
  'nest-260.md': {
    body: '$$' + rep('\\frac{1}{', 36) + 'x' + rep('}', 36) + '$$\n',
    want: 'skipped',
    title: TOO_COMPLEX,
  },
  // Constructs that build more DOM levels per TeX level than \frac, nested
  // to just under the DOM bound: they render and the tab survives.
  'pmatrix-near-cap.md': {
    body: '$$' + rep('\\begin{pmatrix}', 27) + 'x' + rep('\\end{pmatrix}', 27) + '$$\n',
    want: 'rendered',
    minDepth: 245,
  },
  'boxed-near-cap.md': {
    body: '$$' + rep('\\boxed{', 30) + 'x' + rep('}', 30) + '$$\n',
    want: 'rendered',
    minDepth: 240,
  },
  'sub-near-cap.md': {
    body: '$$' + rep('x_{', 34) + 'x' + rep('}', 34) + '$$\n',
    want: 'rendered',
    minDepth: 240,
  },
  // The node bound without macros: an empty matrix row of 6,500 & builds
  // about 110,000 nodes from 6,526 characters.
  'nodes-over-cap.md': {
    body: '$$\\begin{matrix}' + rep('&', 6500) + '\\end{matrix}$$\n',
    want: 'skipped',
    title: TOO_COMPLEX,
  },
  // A formula that defines a macro is skipped before KaTeX runs
  // (forgectl#675). The multiplier: 445 expansions, under maxExpand, and
  // about 30 s and 1.44M nodes of KaTeX build before the node bound could
  // reject it. maxMs is what makes this a test of the pre-scan and not of
  // the node bound, which would also skip it, 30 s later.
  'macro-multiply.md': {
    body: '$\\def\\a{' + rep('\\binom11', 100) + '}\\def\\b{' + rep('\\a', 10) + '}\\def\\c{' + rep('\\b', 10) + '}\\def\\d{' + rep('\\c', 4) + '}\\d$\n',
    want: 'skipped',
    title: DEFINES_MACRO,
    maxMs: 5000,
  },
  // Macro expansion the source scan cannot see: braces never nest past 6,
  // but each macro applies the one before four times, so the output nests
  // 256 fractions (about 1,800 DOM levels), and 128 with \newcommand.
  'macro-deep-def.md': {
    body: '$\\def\\fa#1{\\frac1{#1}}\\def\\fb#1{\\fa{\\fa{\\fa{\\fa{#1}}}}}\\def\\fc#1{\\fb{\\fb{\\fb{\\fb{#1}}}}}\\def\\fd#1{\\fc{\\fc{\\fc{\\fc{#1}}}}}\\fd{\\fd{\\fd{\\fd{x}}}}$\n',
    want: 'skipped',
    title: DEFINES_MACRO,
  },
  'macro-deep-newcommand.md': {
    body: '$$\\newcommand\\fa[1]{\\frac1{#1}}\\newcommand\\fb[1]{\\fa{\\fa{\\fa{\\fa{#1}}}}}\\newcommand\\fc[1]{\\fb{\\fb{\\fb{\\fb{#1}}}}}\\newcommand\\fd[1]{\\fc{\\fc{\\fc{\\fc{#1}}}}}\\fd{\\fd{x}}$$\n',
    want: 'skipped',
    title: DEFINES_MACRO,
  },
  // \global\def: \def follows a letter, so a match that wanted a
  // non-letter before the backslash would miss it.
  'macro-global-def.md': {
    body: '$\\global\\def\\fa{x}\\fa$\n',
    want: 'skipped',
    title: DEFINES_MACRO,
  },
  // KaTeX lexes \verb|…| as one token, so the % is not a comment and the
  // \def after it is live. A pre-scan that dropped % comments would miss it.
  'macro-after-verb.md': {
    body: '$\\verb|%|\\def\\fa{x}\\fa$\n',
    want: 'skipped',
    title: DEFINES_MACRO,
  },
  // Control words that only start with a definer's name define nothing.
  // KaTeX does not know them, so it renders each in the error color
  // inside otherwise normal output.
  'macro-lookalike.md': {
    body: '$\\define x + \\letter y + \\globally z$\n',
    want: 'rendered',
  },
  // maxExpand is 500, half KaTeX's default: \dots is two expansions, so
  // 250 render and 251 are a parse error (source shown in the error
  // color), not output.
  'max-expand-at-limit.md': {
    body: '$' + rep('\\dots', 250) + '$\n',
    want: 'rendered',
  },
  'max-expand.md': {
    body: '$' + rep('\\dots', 251) + '$\n',
    want: 'error',
    error: 'Too many expansions',
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
    const start = Date.now();
    // Never 'networkidle': the live-reload SSE stream never goes idle.
    await page.goto(`${base}/doc/${f.root || 'vault'}/${name}`, { waitUntil: 'load', timeout: 30000 });
    await page.waitForFunction(() => window.ForgectlMath !== undefined, null, { timeout: 10000 });
    const elapsed = Date.now() - start;
    if (f.maxMs && elapsed > f.maxMs) problems.push(`${name}: took ${elapsed} ms, want at most ${f.maxMs}`);
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
        title: el.title,
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
        if (m.skipped || !m.error || !m.error.includes(f.error)) problems.push(`${name}: want a parse error with "${f.error}", got skipped=${m.skipped} error=${m.error}`);
      } else if (f.want === 'skipped') {
        if (!m.skipped || m.katex) problems.push(`${name}: want TeX source left as is, got skipped=${m.skipped} katex=${m.katex}`);
        if (m.text !== m.source) problems.push(`${name}: skipped formula does not show its source`);
        if (f.title && m.title !== f.title) problems.push(`${name}: want tooltip ${JSON.stringify(f.title)}, got ${JSON.stringify(m.title)}`);
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
