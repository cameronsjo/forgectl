#!/usr/bin/env node
// Pins the reader's mermaid config against a doc's own config (forgectl#713).
// CI has no browser harness, so run this by hand after any change to
// mermaid-init.js or a mermaid re-vendor, and say so in the PR.
//
// A diagram can carry a %%{init: {...}}%% directive or a frontmatter
// config: block, and mermaid merges either over the config mermaid-init.js
// sets. This serves one doc with the same hostile config in both forms, plus
// a plain control diagram, and checks in a real browser that none of it
// takes effect:
//
//   - themeCSS and fontFamily never reach a diagram's <style>;
//   - theme: 'forest' does not replace the reader's base theme colors;
//   - htmlLabels: true (top-level and flowchart.htmlLabels) does not turn a
//     label into live HTML: no <details>, no <foreignObject>;
//   - securityLevel: 'loose' does not bind a click directive's callback;
//   - the control diagram renders with no label markup either, since the
//     reader's own config keeps labels as SVG text;
//   - with mermaid's defaultConfig gone (a bad re-vendor), a re-render
//     leaves every diagram as source text and logs an error.
//
// Every check first proves the diagram rendered and its label text is there,
// so a render failure cannot pass as "inert". Run it against a build from
// before #713 and it fails on every hostile key.
//
// Usage: node scripts/verify-mermaid-config.mjs [path/to/forgectl]
//   Without a path it builds one with `go build` into a scratch dir.
//   PLAYWRIGHT_MODULE overrides the Playwright import (default: the global
//   install, `$(npm root -g)/playwright/index.mjs`). CHROMIUM_PATH sets the
//   browser executable (claude.ai cloud sessions: /opt/pw-browsers/chromium).
// Exit 0 and "VERDICT: PASS" when every check holds; non-zero otherwise.

import { execFileSync, spawn } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

const scratch = mkdtempSync(join(tmpdir(), 'forgectl-mermaid-cfg-'));
let server;
let browser;
function cleanup() {
  if (server && server.exitCode === null) server.kill();
  rmSync(scratch, { recursive: true, force: true });
}

async function fail(msg) {
  console.error(`VERDICT: FAIL ${msg}`);
  if (browser) await browser.close().catch(() => {});
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

// Markers that can only reach the page if the hostile config took effect.
const CSS_MARK = 'rgb(1, 2, 3)';
const FONT_MARK = 'FC713FONT';
// A forest color the reader's themeVariables do not override (border2).
const FOREST = '#6eaa49';

const hostileConfig = {
  themeCSS: `.node rect { fill: ${CSS_MARK}; }`,
  fontFamily: FONT_MARK,
  theme: 'forest',
  htmlLabels: true,
  flowchart: { htmlLabels: true },
  securityLevel: 'loose',
};

function flow(label, node) {
  return `flowchart LR\n  ${node}["<details open><summary>${label}</summary>x</details>"] --> Z${node}\n` +
    `  click ${node} call fc713()\n`;
}

const doc = [
  '# Mermaid config',
  '```mermaid\n%%{init: ' + JSON.stringify(hostileConfig) + '}%%\n' + flow('DIRECTIVE-LABEL', 'D') + '```',
  '```mermaid\n---\nconfig: ' + JSON.stringify(hostileConfig) + '\n---\n' + flow('FRONTMATTER-LABEL', 'F') + '```',
  '```mermaid\n' + flow('CONTROL-LABEL', 'C') + '```',
].join('\n\n') + '\n';

const bin = process.argv[2] || (() => {
  const out = join(scratch, 'forgectl');
  execFileSync('go', ['build', '-o', out, '.'], { stdio: 'inherit' });
  return out;
})();

const root = join(scratch, 'docs');
mkdirSync(root, { recursive: true });
writeFileSync(join(root, 'config.md'), doc);

const port = await freePort();
const base = `http://127.0.0.1:${port}`;
server = spawn(bin, ['docs', 'serve', '--addr', `127.0.0.1:${port}`, root], { stdio: 'ignore' });
let up = false;
for (let i = 0; i < 100 && !up; i++) {
  try { up = (await fetch(`${base}/`)).ok; } catch { await new Promise((r) => setTimeout(r, 100)); }
}
if (!up) await fail(`docs serve did not come up on ${base}`);

const { chromium } = await loadPlaywright();
const launch = process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {};
browser = await chromium.launch(launch);
const problems = [];
const page = await browser.newPage({ viewport: { width: 1400, height: 900 } });
// The click directive's target. Under 'loose' mermaid binds node clicks to it.
await page.addInitScript(() => { window.fc713 = () => { window.__fc713Clicked = true; }; });

try {
  // Never 'networkidle': the live-reload SSE stream never goes idle.
  await page.goto(`${base}/doc/docs/config.md`, { waitUntil: 'load', timeout: 30000 });
  await page.waitForFunction(() => {
    const pres = document.querySelectorAll('[data-fc="doc-body"] pre.mermaid');
    return pres.length === 3 && [...pres].every((p) => p.querySelector('svg'));
  }, null, { timeout: 20000 });
  await page.waitForTimeout(100);

  const results = await page.evaluate(({ cssMark, fontMark, forest }) => {
    return [...document.querySelectorAll('[data-fc="doc-body"] pre.mermaid')].map((pre) => {
      const styles = [...pre.querySelectorAll('style')].map((s) => s.textContent).join('\n');
      return {
        text: pre.querySelector('svg').textContent,
        details: pre.querySelectorAll('details, summary').length,
        foreignObject: pre.querySelectorAll('foreignObject').length,
        themeCSS: styles.includes(cssMark) || styles.includes(cssMark.replace(/ /g, '')),
        font: styles.includes(fontMark),
        forest: styles.toLowerCase().includes(forest),
      };
    });
  }, { cssMark: CSS_MARK, fontMark: FONT_MARK, forest: FOREST });

  const names = ['directive', 'frontmatter', 'control'];
  const labels = ['DIRECTIVE-LABEL', 'FRONTMATTER-LABEL', 'CONTROL-LABEL'];
  results.forEach((r, i) => {
    const n = names[i];
    if (!r.text.includes(labels[i])) {
      problems.push(`${n}: label text missing from the rendered diagram; the checks below prove nothing`);
      return;
    }
    if (r.details > 0) problems.push(`${n}: the label rendered as live HTML (<details>)`);
    if (r.foreignObject > 0) problems.push(`${n}: the label rendered in a <foreignObject> (htmlLabels on)`);
    if (r.themeCSS) problems.push(`${n}: themeCSS reached the diagram's <style>`);
    if (r.font) problems.push(`${n}: fontFamily reached the diagram's <style>`);
    if (r.forest) problems.push(`${n}: theme 'forest' replaced the reader's base theme`);
  });

  // securityLevel: click every node that carries a click directive. The
  // click is dispatched on the node itself: a Playwright pointer click on the
  // node never fired the callback, even with the reader's own config set to
  // 'loose' (svg-panzoom sits on the pointer path), so it proved nothing.
  const clicked = await page.evaluate(() => ['D', 'F', 'C'].map((id) => {
    const node = document.querySelector(`[data-fc="doc-body"] pre.mermaid g.node[id*="-${id}-"]`);
    if (!node) return id;
    node.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    return null;
  }).filter(Boolean));
  if (clicked.length > 0) problems.push(`click: no node for ${clicked.join(', ')}; the securityLevel check proves nothing`);
  if (await page.evaluate(() => window.__fc713Clicked === true)) {
    problems.push("securityLevel: a click directive's callback ran ('loose' took effect)");
  }

  // Fail closed: if a re-vendor drops mermaid's defaultConfig, the pin list
  // cannot be derived, and the reader must leave diagrams as source rather
  // than render them with a doc-settable config. Simulated by hiding the
  // key set and flipping the theme, which re-renders every diagram.
  const errors = [];
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
  const hidden = await page.evaluate(() => {
    // mermaidAPI is frozen, but mermaid.mermaidAPI itself is writable.
    mermaid.mermaidAPI = Object.assign({}, mermaid.mermaidAPI, { defaultConfig: undefined });
    const root = document.documentElement;
    root.setAttribute('data-theme', root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark');
    return mermaid.mermaidAPI.defaultConfig === undefined;
  });
  if (!hidden) {
    problems.push('fail-closed: could not hide mermaid defaultConfig; the check proves nothing');
  } else {
    await page.waitForTimeout(1500);
    const after = await page.evaluate(() => [...document.querySelectorAll('[data-fc="doc-body"] pre.mermaid')]
      .map((p) => ({ svg: !!p.querySelector('svg'), source: p.textContent.includes('flowchart LR') })));
    if (after.some((d) => d.svg || !d.source)) problems.push(`fail-closed: diagrams rendered without a derivable pin list ${JSON.stringify(after)}`);
    if (!errors.some((e) => e.includes('defaultConfig'))) problems.push('fail-closed: no console error named the missing defaultConfig');
  }
} catch (err) {
  problems.push(String(err).split('\n')[0]);
}

await browser.close();
cleanup();
if (problems.length > 0) {
  for (const p of problems) console.error(`  - ${p}`);
  console.error(`VERDICT: FAIL ${problems.length} problem(s)`);
  process.exit(1);
}
console.log('VERDICT: PASS a doc-level mermaid config changes nothing the reader pins');
