#!/usr/bin/env node
// Pins the reader's chrome lookups in a real browser (forgectl#643).
// CI has no browser harness, so run this by hand after any change to the
// reader's scripts or shell template and say so in the PR.
//
// A doc can carry any class, and the sanitizer keeps some chrome tags
// (<aside>, <details>). Mermaid renders in the browser after the sanitizer,
// and its own DOMPurify keeps data-*, so with HTML labels on a diagram label
// could carry a data-fc hook. This serves one hostile doc that plants a copy
// of every chrome class the scripts once looked up, plus data-fc hooks in a
// classDiagram label and in a flowchart whose directive asks for
// htmlLabels, and checks that the real chrome still does its job:
//
//   - the mermaid labels render as inert SVG text (forgectl#713), and no
//     data-fc hook survives inside the doc body once mermaid has rendered,
//     on first load and again after a live-reload swap re-renders it;
//   - the sanitizer strips the planted chrome classes (forgectl#700), so a
//     planted Artificer overlay (.scrim, .toast-region) stays in the doc's
//     flow instead of pinning itself over the reader;
//   - the sidebar filter folds and hides only the sidenav, never the doc's
//     planted <div class="sidenav"> with its <details> and group heading;
//   - a live-reload swap updates the real outline pane and status bar, not
//     the doc's planted <aside class="outline">, and happens in place (no
//     full reload);
//   - the nav toggle drives the real shell;
//   - the copy handler still cleans a selection inside a planted
//     <div class="doc-body">;
//   - a live-reload swap keeps focus on a doc link that mimics a sidenav
//     link's href and class, rather than moving it to the sidenav;
//   - a swap that adds text above a heading slugged "doc-filter" (a chrome
//     id) keeps that heading where the reader had it;
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
  '<div class="scrim">PLANTED-SCRIM</div>',
  '<div class="toast-region">PLANTED-TOAST</div>',
].join('\n\n');

// Rendered by mermaid, whose DOMPurify keeps data-* in HTML labels. The
// flowchart asks for htmlLabels with a doc-level init directive, which the
// reader's config pins off (forgectl#713).
const mermaidPlants = [
  '```mermaid\nclassDiagram\nclass Foo["<i data-fc=\'outline\'>MER-CLASS</i>"]\n```',
  '```mermaid\n%%{init: {"flowchart": {"htmlLabels": true}}}%%\nflowchart LR\n' +
    '  A["<details open data-fc=\'sidenav\'><summary>MER-DETAILS</summary>x</details>' +
    '<span data-fc=\'outline\'>MER-OUTLINE</span><span data-fc=\'statusbar\'>s</span>' +
    '<span data-fc=\'live-status\'>l</span><span data-fc=\'doc-missing\'>m</span>"]\n```',
].join('\n\n');

// Enough prose that the doc pane scrolls, with a heading whose slug is the
// id of the chrome filter box.
const filler = Array.from({ length: 40 }, (_, i) => `Paragraph ${i} of filler text for scrolling.`).join('\n\n');

function hostile({ above = '', extra = '', mimic = '' } = {}) {
  return `# Hostile\n\n${above}\n\n## First\n\n${planted}\n\n${mermaidPlants}\n\n${mimic}\n\n` +
    `## Second\n\nsome words here.\n\n${filler}\n\n## Doc filter\n\n${filler}\n${extra}`;
}

// Waits until every diagram in the doc body has rendered to SVG.
async function mermaidRendered(page) {
  await page.waitForFunction(() => {
    const pres = document.querySelectorAll('[data-fc="doc-body"] pre.mermaid');
    return pres.length === 2 && [...pres].every((p) => p.querySelector('svg'));
  }, null, { timeout: 20000 });
  // One more task, so an async scrub would have run too.
  await page.waitForTimeout(100);
}

async function noForgedHooks(page, when) {
  const forged = await page.evaluate(() =>
    [...document.querySelector('[data-fc="doc-body"]').querySelectorAll('[data-fc]')]
      .map((el) => `${el.localName}[data-fc=${el.getAttribute('data-fc')}]`));
  if (forged.length > 0) problems.push(`${when}: forged hooks inside the doc body: ${forged.join(', ')}`);
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
writeFileSync(docPath, hostile());

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
  // Fixture sanity: every planted element made it through the sanitizer,
  // as content. Its chrome classes are stripped (forgectl#700), so find each
  // by tag and text.
  const plantedCount = await page.evaluate(() => {
    const body = document.querySelector('[data-fc="doc-body"]');
    const has = (sel, text) => [...body.querySelectorAll(sel)].some((el) => el.textContent.includes(text));
    return [['aside', 'PLANTED-OUTLINE'], ['details', 'PLANTED-INLINE'], ['div', 'PLANTED-GROUP'],
      ['div', 'PLANTED-BODY'], ['div', 'PLANTED-SCRIM'], ['div', 'PLANTED-TOAST']]
      .filter(([sel, text]) => has(sel, text)).length;
  });
  if (plantedCount !== 6) problems.push(`fixture: ${plantedCount}/6 planted elements survived; the checks below prove nothing`);

  // A planted overlay class must not pin anything over the reader.
  const overlays = await page.evaluate(() => {
    const body = document.querySelector('[data-fc="doc-body"]');
    return ['PLANTED-SCRIM', 'PLANTED-TOAST'].map((text) => {
      const el = [...body.querySelectorAll('div')].find((d) => d.textContent === text);
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return { text, position: getComputedStyle(el).position, width: Math.round(r.width), height: Math.round(r.height) };
    }).filter(Boolean);
  });
  for (const o of overlays) {
    if (o.position === 'fixed' || o.position === 'sticky') problems.push(`overlay: the doc's ${o.text} is position:${o.position} (${o.width}x${o.height}); a chrome class reached it`);
  }

  await mermaidRendered(page);
  // The labels rendered, and as SVG text rather than live markup.
  const labels = await page.evaluate(() => {
    const body = document.querySelector('[data-fc="doc-body"]');
    const text = [...body.querySelectorAll('pre.mermaid svg')].map((s) => s.textContent).join(' ');
    return {
      rendered: text.includes('MER-CLASS') && text.includes('MER-DETAILS'),
      markup: body.querySelectorAll('pre.mermaid i, pre.mermaid details, pre.mermaid span[data-fc]').length,
    };
  });
  if (!labels.rendered) problems.push('fixture: the mermaid label text did not render; the hook checks prove nothing');
  if (labels.markup > 0) problems.push(`mermaid: ${labels.markup} label element(s) rendered as live HTML (htmlLabels is on)`);
  await noForgedHooks(page, 'first render');

  // Sidebar filter: a query that matches no doc.
  await page.fill('[data-fc="doc-filter"]', 'zzz-no-such-doc');
  const filter = await page.evaluate(() => {
    const body = document.querySelector('[data-fc="doc-body"]');
    const det = [...body.querySelectorAll('details')].find((d) => d.textContent.includes('PLANTED-DETAILS'));
    const grp = [...body.querySelectorAll('div')].find((g) => g.textContent === 'PLANTED-GROUP');
    return {
      detOpen: det.open,
      detShown: getComputedStyle(det).display !== 'none',
      detMarked: det.dataset.openAtRest !== undefined,
      grpShown: getComputedStyle(grp).display !== 'none',
      empty: !document.querySelector('[data-fc="filter-empty"]').hidden,
    };
  });
  if (!filter.detOpen || !filter.detShown || filter.detMarked) problems.push(`filter: reached the doc's planted <details> ${JSON.stringify(filter)}`);
  if (!filter.grpShown) problems.push('filter: hid the doc\'s planted sidenav group');
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
  // outline and status bar, in place. The edit also adds a doc link that
  // copies the first sidenav link's href and class, for the focus probe.
  await page.evaluate(() => { window.__noFullReload = true; });
  const wordsBefore = await page.textContent('[data-fc="statusbar"]');
  const side = await page.evaluate(() => {
    const a = document.querySelector('[data-fc="sidenav"] a[href]');
    return { href: a.getAttribute('href'), cls: a.className };
  });
  const mimic = `<a href="${side.href}"${side.cls ? ` class="${side.cls}"` : ''}>MIMIC-LINK</a>`;
  const added = '\n## Added Heading\n\nmany more words to change the count here now.\n';
  writeFileSync(docPath, hostile({ extra: added, mimic }));
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
    plantedIntact: [...document.querySelectorAll('[data-fc="doc-body"] aside')]
      .some((a) => a.textContent.includes('PLANTED-OUTLINE') && !a.textContent.includes('Added Heading')),
    status: document.querySelector('[data-fc="statusbar"]').textContent,
  }));
  if (!swapped.inPlace) problems.push('live reload: fell back to a full reload');
  if (!swapped.outline) problems.push('live reload: the real outline pane is stale (the swap went to the planted aside)');
  if (!swapped.plantedIntact) problems.push('live reload: the planted aside was replaced by chrome');
  if (swapped.status === wordsBefore) problems.push('live reload: the status bar did not update');
  await mermaidRendered(page);
  await noForgedHooks(page, 'after a swap');

  // Focus and reading position across a swap. Focus the mimic link, put the
  // "Doc filter" heading 10px above the top of the pane, then add text
  // ABOVE it, so only a heading-anchored restore keeps it in place.
  const pos = await page.evaluate(() => {
    const link = [...document.querySelectorAll('[data-fc="doc-body"] a')].find((a) => a.textContent === 'MIMIC-LINK');
    if (!link) return null;
    link.focus({ preventScroll: true });
    const main = document.querySelector('[data-fc="doc-main"]');
    const h = main.querySelector('#doc-filter');
    if (!h) return { noHeading: true };
    main.scrollTop += h.getBoundingClientRect().top - main.getBoundingClientRect().top + 10;
    return { delta: main.getBoundingClientRect().top - h.getBoundingClientRect().top };
  });
  if (pos === null) {
    problems.push('focus probe: the mimic link is not in the doc');
  } else if (pos.noHeading) {
    problems.push('scroll probe: no heading with id doc-filter in the doc; the probe proves nothing');
  } else {
    const above = Array.from({ length: 20 }, (_, i) => `Inserted paragraph ${i} above everything.`).join('\n\n');
    writeFileSync(docPath, hostile({ above, extra: added, mimic }));
    try {
      await page.waitForFunction(
        () => document.querySelector('[data-fc="doc-body"]').textContent.includes('Inserted paragraph 19'),
        null, { timeout: 15000 });
    } catch {
      problems.push('live reload: the doc body never picked up the second edit');
    }
    const after2 = await page.evaluate(() => {
      const main = document.querySelector('[data-fc="doc-main"]');
      const h = main.querySelector('#doc-filter');
      const f = document.activeElement;
      return {
        delta: main.getBoundingClientRect().top - h.getBoundingClientRect().top,
        focusInDoc: !!f && !!f.closest('[data-fc="doc-body"]'),
        focusText: f ? f.textContent : null,
      };
    });
    if (!after2.focusInDoc || after2.focusText !== 'MIMIC-LINK') problems.push(`focus: a swap moved focus off the doc link to ${JSON.stringify(after2.focusText)}`);
    if (Math.abs(after2.delta - pos.delta) > 2) problems.push(`scroll: the "Doc filter" heading moved ${after2.delta - pos.delta}px across a swap (anchor restore used the chrome id)`);
  }

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
