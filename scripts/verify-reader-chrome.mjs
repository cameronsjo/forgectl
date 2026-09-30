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
//     on first load and again after a live-reload swap re-renders it, and a
//     hook forged inside a diagram is gone by the next task boundary;
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
//   - a live-reload swap puts focus back on a diagram's pan/zoom viewport or
//     reset button once the diagram has re-rendered, and so does a theme flip;
//   - a planted .tooltip paints nothing outside the doc pane and never makes
//     the page itself scroll, and a diagram's own chrome class names
//     (A:::scrim) are scrubbed from its nodes;
//   - a swap that adds text above a heading slugged "doc-filter" (a chrome
//     id) keeps that heading where the reader had it;
//   - deleting the doc puts the missing banner in the real doc body;
//   - at narrow width a tooltip on a .phase just below the skip-content
//     banner, the inline outline, the properties block or the missing-doc
//     banner paints over none of them;
//   - with the mermaid and KaTeX bundles blocked, a doc carrying
//     <div id="ForgectlMermaid"> and <div id="ForgectlMath"> still gets an
//     in-place live-reload swap;
//   - with both bundles blocked, a doc whose headings take the ids "mermaid"
//     and "katex" (so those globals are the heading elements) still makes
//     each init script stop at its bundle gate, with no page error; with
//     the bundles loaded, the same doc renders its diagram and formula
//     (forgectl#772);
//   - data-forgectl-notice and data-forgectl-props forged inside a diagram
//     are scrubbed by the next task boundary, like data-fc (forgectl#772).
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
  '<div class="tooltip tooltip--bottom">PLANTED-TIP-BOTTOM</div>',
  `<div class="tooltip">PLANTED-TIP-LONG ${'wide '.repeat(600)}</div>`,
].join('\n\n');

// Rendered by mermaid, whose DOMPurify keeps data-* in HTML labels. The
// flowchart asks for htmlLabels with a doc-level init directive, which the
// reader's config pins off (forgectl#713).
const mermaidPlants = [
  '```mermaid\nclassDiagram\nclass Foo["<i data-fc=\'outline\'>MER-CLASS</i>"]\n```',
  '```mermaid\n%%{init: {"flowchart": {"htmlLabels": true}}}%%\nflowchart LR\n' +
    '  A["<details open data-fc=\'sidenav\'><summary>MER-DETAILS</summary>x</details>' +
    '<span data-fc=\'outline\'>MER-OUTLINE</span><span data-fc=\'statusbar\'>s</span>' +
    '<span data-fc=\'live-status\'>l</span><span data-fc=\'doc-missing\'>m</span>"]\n' +
    '  A --> B:::scrim\n  class A statusbar\n```',
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

// Docs for the in-pane chrome check (forgectl#759). Each opens with a
// .phase wrapper, which is position: relative, holding a .tooltip--top that
// reaches up out of it, right below one piece of chrome the reader puts in
// the pane: the skip-content banner (the unclosed <noscript> at the end
// raises it), the narrow-width outline (a doc with no banner), the
// properties block with its trust badge (frontmatter), and the missing-doc
// banner (reload.js prepends it once the file is deleted).
const overlapTip = '<div class="phase"><div class="tooltip tooltip--top">OVERLAP-TIP</div>phase body</div>';
writeFileSync(join(root, 'overlap-banner.md'), `${overlapTip}\n\n# Banner\n\n## One\n\n${filler}\n\n<noscript>\n\nhidden rest\n`);
writeFileSync(join(root, 'overlap-inline.md'), `${overlapTip}\n\n# Inline\n\n## One\n\n${filler}\n\n## Two\n\n${filler}\n`);
writeFileSync(join(root, 'overlap-props.md'), `---\nstatus: deprecated\n---\n\n${overlapTip}\n\n# Props\n\n${filler}\n`);
const overlapMissing = join(root, 'overlap-missing.md');
writeFileSync(overlapMissing, `${overlapTip}\n\n# Missing\n\n${filler}\n`);

// A doc whose ids name the globals mermaid-init.js and math-init.js set, for
// the clobbering check (forgectl#759).
const clobberPath = join(root, 'clobber.md');
const clobber = (marker) => `# Clobber\n\n<div id="ForgectlMermaid">m</div>\n\n<div id="ForgectlMath">k</div>\n\n` +
  `\`\`\`mermaid\nflowchart LR\n  A --> B\n\`\`\`\n\n$x^2$\n\n${marker}\n`;
writeFileSync(clobberPath, clobber('CLOBBER-FIRST'));

// Headings slugged "mermaid" and "katex" name the bundles' globals, so with
// a bundle blocked the global is the heading element (forgectl#772).
writeFileSync(join(root, 'heading-ids.md'), '# Mermaid\n\n## Katex\n\n' +
  '```mermaid\nflowchart LR\n  A --> B\n```\n\n$$\nx^2\n$$\n');

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

  // A doc's absolute-positioned Artificer classes stay inside the doc pane
  // (forgectl#745). .tooltip is position:absolute, z-index 1000 and
  // white-space:nowrap; with no positioned ancestor a .tooltip--bottom sat
  // under the status bar and made the page itself scroll, and the long one
  // ran across the outline column. The pane clips what overflows it, so the
  // check is what paints outside it: each chrome region is screenshotted
  // with the tooltips in place and again with them removed.
  const tipsFound = await page.evaluate(() => [...document.querySelectorAll('[data-fc="doc-body"] div')]
    .filter((d) => d.textContent.startsWith('PLANTED-TIP-')).length);
  if (tipsFound !== 2) {
    problems.push(`fixture: ${tipsFound}/2 planted tooltips survived; the containment check proves nothing`);
  } else {
    const pageScroll = await page.evaluate(() => {
      const se = document.scrollingElement;
      return { h: se.scrollHeight - window.innerHeight, w: se.scrollWidth - window.innerWidth };
    });
    if (pageScroll.h > 0 || pageScroll.w > 0) problems.push(`containment: the page itself scrolls by ${JSON.stringify(pageScroll)}; a planted tooltip escaped the doc pane`);
    const regions = await page.evaluate(() => ['outline', 'statusbar', 'appbar', 'sidenav'].map((id) => {
      const el = document.querySelector(`[data-fc="${id}"]`);
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return { id, clip: { x: r.left, y: r.top, width: Math.min(r.width, window.innerWidth - r.left), height: Math.min(r.height, window.innerHeight - r.top) } };
    }).filter((r) => r && r.clip.width > 0 && r.clip.height > 0));
    const shots = async () => Promise.all(regions.map((r) => page.screenshot({ clip: r.clip })));
    const withTips = await shots();
    await page.evaluate(() => [...document.querySelectorAll('[data-fc="doc-body"] div')]
      .filter((d) => d.textContent.startsWith('PLANTED-TIP-')).forEach((d) => { d.hidden = true; }));
    const without = await shots();
    await page.evaluate(() => [...document.querySelectorAll('[data-fc="doc-body"] div')]
      .filter((d) => d.textContent.startsWith('PLANTED-TIP-')).forEach((d) => { d.hidden = false; }));
    regions.forEach((r, i) => {
      if (!withTips[i].equals(without[i])) problems.push(`containment: a planted tooltip paints over the ${r.id} chrome`);
    });
  }

  // Chrome class names a diagram gives its own nodes (A:::scrim, class A
  // statusbar) are scrubbed like forged hooks (forgectl#745).
  const merChrome = await page.evaluate(() => [...document.querySelectorAll('[data-fc="doc-body"] pre.mermaid [class]')]
    .flatMap((el) => [...el.classList]).filter((c) => /^(scrim|statusbar)([-_]|$)/.test(c)));
  if (merChrome.length > 0) problems.push(`mermaid: chrome classes survived on diagram nodes: ${merChrome.join(', ')}`);

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

  // The scrub's timing (forgectl#718). With htmlLabels pinned off, mermaid
  // 11.12.3 emits no hook to watch for, so the probe forges two itself: an
  // element inserted with data-fc, and data-fc set on an element already in
  // the diagram. A MessageChannel message is a task, so its handler samples
  // at the first task boundary after the forgery, which is the earliest
  // point an event or network callback could see it. A scrub that ran only
  // when mermaid.run resolved would leave both up here. The server's other
  // two chrome markers are forged the same way (forgectl#772).
  const timing = await page.evaluate(() => new Promise((resolve) => {
    const attrs = ['data-fc', 'data-forgectl-notice', 'data-forgectl-props'];
    const pre = document.querySelector('[data-fc="doc-body"] pre.mermaid');
    const g = pre && pre.querySelector('svg g');
    if (!g) { resolve(null); return; }
    const spans = attrs.map((a) => {
      const span = document.createElement('span');
      span.setAttribute(a, a === 'data-fc' ? 'outline' : 'forged');
      pre.appendChild(span);
      return span;
    });
    attrs.forEach((a) => g.setAttribute(a, a === 'data-fc' ? 'statusbar' : 'forged'));
    const sync = attrs.every((a, i) => spans[i].hasAttribute(a) && g.hasAttribute(a));
    const ch = new MessageChannel();
    ch.port1.onmessage = () => {
      const inserted = attrs.filter((a, i) => spans[i].hasAttribute(a));
      const attribute = attrs.filter((a) => g.hasAttribute(a));
      spans.forEach((span) => span.remove());
      attrs.forEach((a) => g.removeAttribute(a));
      resolve({ sync, inserted: inserted.length ? inserted : null, attribute: attribute.length ? attribute : null });
    };
    ch.port2.postMessage(0);
  }));
  if (timing === null) {
    problems.push('scrub timing: no rendered diagram to forge a hook in; the check proves nothing');
  } else {
    if (!timing.sync) problems.push('scrub timing: the forged hooks were gone synchronously; the probe cannot tell a task-boundary scrub from none');
    if (timing.inserted || timing.attribute) problems.push(`scrub timing: a forged hook survived to the next task ${JSON.stringify(timing)}`);
  }

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

  // Focus inside a diagram survives a swap (forgectl#718): the swap brings
  // each diagram back as source, so the viewport and reset button are put
  // back only once mermaid has rendered it again.
  await mermaidRendered(page);
  for (const [n, part] of [[1, '.dia-viewport'], [2, '.embed-reset']]) {
    const focused = await page.evaluate((sel) => {
      const embeds = document.querySelectorAll('[data-fc="doc-body"] .embed');
      const el = embeds[1] && embeds[1].querySelector(sel);
      if (!el) return false;
      el.focus({ preventScroll: true });
      return document.activeElement === el;
    }, part);
    if (!focused) {
      problems.push(`diagram focus: could not focus the second diagram's ${part}; the check proves nothing`);
      continue;
    }
    const marker = `Diagram focus edit ${n}.`;
    writeFileSync(docPath, hostile({ extra: `${added}\n${marker}\n`, mimic }));
    try {
      await page.waitForFunction((m) => document.querySelector('[data-fc="doc-body"]').textContent.includes(m),
        marker, { timeout: 15000 });
      await mermaidRendered(page);
      await page.waitForFunction((sel) => {
        const embeds = document.querySelectorAll('[data-fc="doc-body"] .embed');
        return embeds[1] && document.activeElement === embeds[1].querySelector(sel);
      }, part, { timeout: 3000 });
    } catch {
      const now = await page.evaluate(() => {
        const f = document.activeElement;
        return f ? `${f.localName}.${f.className}` : null;
      });
      problems.push(`diagram focus: after a swap focus is on ${now}, not the second diagram's ${part}`);
    }
  }

  // A theme flip re-renders every diagram too, and puts focus back the
  // same way (forgectl#745).
  // The flip's observer runs as a microtask, so the drop is sampled after
  // one: the old viewport must have left the document by then.
  const themeFocus = await page.evaluate(async () => {
    const vp = document.querySelectorAll('[data-fc="doc-body"] .embed')[1]?.querySelector('.dia-viewport');
    if (!vp) return false;
    vp.focus({ preventScroll: true });
    const root = document.documentElement;
    root.setAttribute('data-theme', root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark');
    await Promise.resolve();
    return !vp.isConnected && document.activeElement !== vp;
  });
  if (!themeFocus) {
    problems.push('theme focus: the flip did not drop focus from the viewport; the check proves nothing');
  } else {
    try {
      await page.waitForFunction(() => {
        const embed = document.querySelectorAll('[data-fc="doc-body"] .embed')[1];
        return embed && document.activeElement === embed.querySelector('.dia-viewport');
      }, null, { timeout: 10000 });
    } catch {
      const now = await page.evaluate(() => document.activeElement && document.activeElement.localName);
      problems.push(`theme focus: after a theme flip focus is on ${now}, not the second diagram's .dia-viewport`);
    }
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

// In-pane chrome stays on top of the doc (forgectl#759). <main>'s paint
// clip keeps a doc's tooltip inside the pane, but the skip-content banner
// and the narrow-width outline are inside the pane too. Each region is
// screenshotted with the tooltip in place and again with it hidden; the
// region's rounded corners are left out, since a doc showing through a
// corner's transparent pixels mimics nothing.
try {
  const narrow = await browser.newPage({ viewport: { width: 800, height: 900 } });
  for (const [doc, sel, id] of [
    ['overlap-banner.md', '[data-fc="doc-body"] [data-forgectl-notice]', 'skip-content banner'],
    ['overlap-inline.md', '[data-fc="outline-inline"]', 'inline outline'],
    ['overlap-props.md', '[data-fc="doc-body"] [data-forgectl-props]', 'properties block'],
    ['overlap-missing.md', '[data-fc="doc-missing"]', 'missing-doc banner'],
  ]) {
    await narrow.goto(`${base}/doc/docs/${doc}`, { waitUntil: 'load', timeout: 30000 });
    if (doc === 'overlap-missing.md') {
      unlinkSync(overlapMissing);
      await narrow.waitForSelector(sel, { timeout: 15000 });
    }
    const probe = await narrow.evaluate((s) => {
      const el = document.querySelector(s);
      const tip = [...document.querySelectorAll('[data-fc="doc-body"] .tooltip')].find((d) => d.textContent === 'OVERLAP-TIP');
      if (!el || !tip) return { missing: !el ? 'region' : 'tooltip' };
      const r = el.getBoundingClientRect();
      const t = tip.getBoundingClientRect();
      const cs = getComputedStyle(el);
      const rad = Math.ceil(Math.max(...['TopLeft', 'TopRight', 'BottomLeft', 'BottomRight']
        .map((c) => parseFloat(cs[`border${c}Radius`]) || 0)));
      const clip = { x: r.left + rad, y: r.top, width: r.width - 2 * rad, height: r.height };
      const overlaps = t.left < clip.x + clip.width && t.right > clip.x && t.top < clip.y + clip.height && t.bottom > clip.y;
      return { clip, overlaps };
    }, sel);
    if (probe.missing) {
      problems.push(`in-pane chrome: no ${probe.missing} on ${doc}; the check proves nothing`);
      continue;
    }
    if (!probe.overlaps) {
      problems.push(`in-pane chrome: the tooltip does not reach the ${id} on ${doc}; the check proves nothing`);
      continue;
    }
    const setTip = (v) => narrow.evaluate((vis) => [...document.querySelectorAll('[data-fc="doc-body"] .tooltip')]
      .filter((d) => d.textContent === 'OVERLAP-TIP').forEach((d) => { d.style.visibility = vis; }), v);
    const withTip = await narrow.screenshot({ clip: probe.clip, animations: 'disabled' });
    await setTip('hidden');
    const withoutTip = await narrow.screenshot({ clip: probe.clip, animations: 'disabled' });
    await setTip('');
    if (!withTip.equals(withoutTip)) problems.push(`in-pane chrome: a doc tooltip paints over the ${id}`);
  }
  await narrow.close();
} catch (err) {
  problems.push(`in-pane chrome: ${String(err).split('\n')[0]}`);
}

// Live reload survives a doc that clobbers the init scripts' globals
// (forgectl#759). With the mermaid and KaTeX bundles blocked, each init
// script returns early, and a doc's <div id="ForgectlMermaid"> would be
// window.ForgectlMermaid: calling its .focusKey or .refresh threw inside the
// swap, and live reload fell back to a full page reload.
try {
  const blocked = await browser.newPage({ viewport: { width: 1400, height: 900 } });
  const errors = [];
  blocked.on('pageerror', (e) => errors.push(String(e).split('\n')[0]));
  await blocked.route(/\/assets\/(mermaid\.min\.js|katex\/katex\.min\.js)$/, (r) => r.abort());
  await blocked.goto(`${base}/doc/docs/clobber.md`, { waitUntil: 'load', timeout: 30000 });
  const setup = await blocked.evaluate(() => {
    const body = document.querySelector('[data-fc="doc-body"]');
    return {
      bundlesBlocked: typeof mermaid === 'undefined' && typeof katex === 'undefined',
      idsKept: !!body.querySelector('#ForgectlMermaid') && !!body.querySelector('#ForgectlMath'),
    };
  });
  if (!setup.bundlesBlocked || !setup.idsKept) {
    problems.push(`clobbering: fixture ${JSON.stringify(setup)}; the check proves nothing`);
  } else {
    await blocked.evaluate(() => { window.__noFullReload = true; });
    writeFileSync(clobberPath, clobber('CLOBBER-SECOND'));
    try {
      await blocked.waitForFunction(
        () => document.querySelector('[data-fc="doc-body"]').textContent.includes('CLOBBER-SECOND'),
        null, { timeout: 15000 });
      // A fallback reload lands a moment after the fetch; give it the time.
      await blocked.waitForTimeout(500);
      const inPlace = await blocked.evaluate(() => window.__noFullReload === true).catch(() => false);
      if (!inPlace) problems.push('clobbering: live reload fell back to a full reload');
    } catch {
      problems.push('clobbering: the doc body never picked up the edit');
    }
  }
  if (errors.length > 0) problems.push(`clobbering: page errors: ${errors.join('; ')}`);
  await blocked.close();
} catch (err) {
  problems.push(`clobbering: ${String(err).split('\n')[0]}`);
}

// The bundle gates test each bundle's entry point, not its global
// (forgectl#772). A heading slugged "mermaid" or "katex" is that global when
// the bundle is blocked, and typeof it is "object", so a typeof-undefined
// gate let each init script run against a heading and throw.
try {
  const gated = await browser.newPage({ viewport: { width: 1400, height: 900 } });
  const errors = [];
  const warnings = [];
  gated.on('pageerror', (e) => errors.push(String(e).split('\n')[0]));
  gated.on('console', (m) => { if (m.type() === 'warning') warnings.push(m.text()); });
  await gated.route(/\/assets\/(mermaid\.min\.js|katex\/katex\.min\.js)$/, (r) => r.abort());
  await gated.goto(`${base}/doc/docs/heading-ids.md`, { waitUntil: 'load', timeout: 30000 });
  await gated.waitForTimeout(500);
  const setup = await gated.evaluate(() => ({
    mermaidIsHeading: window.mermaid instanceof HTMLHeadingElement,
    katexIsHeading: window.katex instanceof HTMLHeadingElement,
  }));
  if (!setup.mermaidIsHeading || !setup.katexIsHeading) {
    problems.push(`bundle gate: fixture ${JSON.stringify(setup)}; the headings do not clobber the globals, so the check proves nothing`);
  } else {
    if (!warnings.some((w) => w.includes('mermaid bundle unavailable'))) problems.push('bundle gate: mermaid-init.js ran past its gate with the bundle blocked and a heading id="mermaid"');
    if (!warnings.some((w) => w.includes('KaTeX bundle unavailable'))) problems.push('bundle gate: math-init.js ran past its gate with the bundle blocked and a heading id="katex"');
    if (errors.length > 0) problems.push(`bundle gate: page errors: ${errors.join('; ')}`);
  }
  await gated.close();

  // CONTROL: the entry-point gates still pass a real bundle, and the same
  // doc renders its diagram and its formula.
  const loaded = await browser.newPage({ viewport: { width: 1400, height: 900 } });
  await loaded.goto(`${base}/doc/docs/heading-ids.md`, { waitUntil: 'load', timeout: 30000 });
  try {
    await loaded.waitForFunction(() => {
      const body = document.querySelector('[data-fc="doc-body"]');
      return !!(body.querySelector('pre.mermaid svg') && body.querySelector('.math .katex'));
    }, null, { timeout: 20000 });
  } catch {
    problems.push('bundle gate control: with the bundles loaded, the doc never rendered its diagram and formula');
  }
  await loaded.close();
} catch (err) {
  problems.push(`bundle gate: ${String(err).split('\n')[0]}`);
}

await browser.close();
cleanup();
if (problems.length > 0) {
  for (const p of problems) console.error(`  - ${p}`);
  console.error(`VERDICT: FAIL ${problems.length} problem(s)`);
  process.exit(1);
}
console.log('VERDICT: PASS reader chrome ignores planted chrome classes');
