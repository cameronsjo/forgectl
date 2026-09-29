#!/usr/bin/env node
// Pins the reader's chrome lookups in a real browser (forgectl#643).
// CI has no browser harness, so run this by hand after any change to the
// reader's scripts or shell template and say so in the PR.
//
// A doc can carry any class, and the sanitizer keeps some chrome tags
// (<aside>, <details>). This serves one hostile doc that plants a copy of
// every chrome class the scripts once looked up, and checks that the real
// chrome still does its job:
//
//   - the sidebar filter folds and hides only the sidenav, never the doc's
//     planted <div class="sidenav"> with its <details> and group heading;
//   - a live-reload swap updates the real outline pane and status bar, not
//     the doc's planted <aside class="outline">, and happens in place (no
//     full reload);
//   - the nav toggle drives the real shell;
//   - the copy handler still cleans a selection inside a planted
//     <div class="doc-body">;
//   - deleting the doc puts the missing banner in the real doc body.
//
// Usage: node scripts/verify-reader-chrome.mjs [path/to/forgectl]
//   Without a path it builds one with `go build` into a scratch dir.
//   PLAYWRIGHT_MODULE overrides the Playwright import (default: the global
//   install, `$(npm root -g)/playwright/index.mjs`). CHROMIUM_PATH sets the
//   browser executable (claude.ai cloud sessions: /opt/pw-browsers/chromium).
// Exit 0 and "VERDICT: PASS" when every check holds; non-zero otherwise.

import { execFileSync, spawn } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, unlinkSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

const scratch = mkdtempSync(join(tmpdir(), 'forgectl-chrome-'));
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

// The planted outline sits before the shell's own in document order, which
// is what made a class-based lookup return it.
const planted = [
  '<aside class="outline"><div class="outline-h">PLANTED-OUTLINE</div></aside>',
  '<details class="outline-inline"><summary>PLANTED-INLINE</summary>x</details>',
  '<div class="sidenav"><div class="sidenav__group">PLANTED-GROUP</div>' +
    '<details open><summary>PLANTED-DETAILS</summary>inside</details></div>',
  '<div class="doc-body"><p>PLANTED-BODY copy me</p></div>',
].join('\n\n');

function hostile(extra) {
  return `# Hostile\n\n## First\n\n${planted}\n\n## Second\n\nsome words here.\n${extra}`;
}

const bin = process.argv[2] || (() => {
  const out = join(scratch, 'forgectl');
  execFileSync('go', ['build', '-o', out, '.'], { stdio: 'inherit' });
  return out;
})();

const root = join(scratch, 'docs');
mkdirSync(root, { recursive: true });
writeFileSync(join(root, 'README.md'), '# Readme\n');
const docPath = join(root, 'hostile.md');
writeFileSync(docPath, hostile(''));

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

try {
  // Never 'networkidle': the live-reload SSE stream never goes idle.
  await page.goto(`${base}/doc/docs/hostile.md`, { waitUntil: 'load', timeout: 30000 });
  // Fixture sanity: every planted element made it through the sanitizer.
  const plantedCount = await page.evaluate(() => [
    'aside.outline', 'details.outline-inline', '.sidenav .sidenav__group', '.doc-body .doc-body',
  ].filter((s) => document.querySelector('[data-fc="doc-body"]').querySelector(s)).length);
  if (plantedCount !== 4) problems.push(`fixture: ${plantedCount}/4 planted elements survived; the checks below prove nothing`);

  // Sidebar filter: a query that matches no doc.
  await page.fill('[data-fc="doc-filter"]', 'zzz-no-such-doc');
  const filter = await page.evaluate(() => {
    const body = document.querySelector('[data-fc="doc-body"]');
    const det = [...body.querySelectorAll('details')].find((d) => d.textContent.includes('PLANTED-DETAILS'));
    const grp = [...body.querySelectorAll('.sidenav__group')].find((g) => g.textContent.includes('PLANTED-GROUP'));
    return {
      detOpen: det.open,
      detShown: getComputedStyle(det).display !== 'none',
      detMarked: det.dataset.openAtRest !== undefined,
      grpShown: getComputedStyle(grp).display !== 'none',
      empty: !document.querySelector('[data-fc="filter-empty"]').hidden,
    };
  });
  if (!filter.detOpen || !filter.detShown || filter.detMarked) problems.push(`filter: reached the doc's planted <details> ${JSON.stringify(filter)}`);
  if (!filter.grpShown) problems.push('filter: hid the doc\'s planted .sidenav__group');
  if (!filter.empty) problems.push('filter: the real "no docs match" note did not show');
  await page.fill('[data-fc="doc-filter"]', '');

  // Nav toggle drives the real shell.
  const before = await page.getAttribute('[data-fc="shell"]', 'data-nav');
  await page.click('[data-fc="nav-toggle"]');
  const after = await page.getAttribute('[data-fc="shell"]', 'data-nav');
  if (before === after) problems.push(`nav toggle: shell data-nav stayed ${before}`);
  await page.click('[data-fc="nav-toggle"]');

  // Copy inside the planted doc-body is still the reader's clean copy.
  const copied = await page.evaluate(() => {
    const p = [...document.querySelectorAll('p')].find((el) => el.textContent.includes('PLANTED-BODY'));
    const range = document.createRange();
    range.selectNodeContents(p);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    const ev = new ClipboardEvent('copy', { clipboardData: new DataTransfer(), bubbles: true, cancelable: true });
    document.dispatchEvent(ev);
    return { handled: ev.defaultPrevented, text: ev.clipboardData.getData('text/plain') };
  });
  if (!copied.handled || !copied.text.includes('PLANTED-BODY')) problems.push(`copy: ${JSON.stringify(copied)}`);

  // Live reload: add a heading and some words; the swap must update the real
  // outline and status bar, in place.
  await page.evaluate(() => { window.__noFullReload = true; });
  const wordsBefore = await page.textContent('[data-fc="statusbar"]');
  writeFileSync(docPath, hostile('\n## Added Heading\n\nmany more words to change the count here now.\n'));
  try {
    await page.waitForFunction(
      () => document.querySelector('[data-fc="doc-body"]').textContent.includes('Added Heading'),
      null, { timeout: 15000 });
  } catch {
    problems.push('live reload: the doc body never picked up the edit');
  }
  const swapped = await page.evaluate(() => ({
    inPlace: window.__noFullReload === true,
    outline: document.querySelector('[data-fc="outline"]').textContent.includes('Added Heading'),
    plantedIntact: [...document.querySelectorAll('[data-fc="doc-body"] aside.outline')]
      .some((a) => a.textContent.includes('PLANTED-OUTLINE') && !a.textContent.includes('Added Heading')),
    status: document.querySelector('[data-fc="statusbar"]').textContent,
  }));
  if (!swapped.inPlace) problems.push('live reload: fell back to a full reload');
  if (!swapped.outline) problems.push('live reload: the real outline pane is stale (the swap went to the planted aside)');
  if (!swapped.plantedIntact) problems.push('live reload: the planted aside was replaced by chrome');
  if (swapped.status === wordsBefore) problems.push('live reload: the status bar did not update');

  // Deleting the doc puts the banner in the real doc body.
  unlinkSync(docPath);
  try {
    await page.waitForSelector('[data-fc="doc-missing"]', { timeout: 15000 });
    const parentOK = await page.evaluate(() =>
      document.querySelector('[data-fc="doc-missing"]').parentElement.matches('[data-fc="doc-body"]'));
    if (!parentOK) problems.push('missing banner: not in the real doc body');
  } catch {
    problems.push('missing banner: never shown');
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
console.log('VERDICT: PASS reader chrome ignores planted chrome classes');
