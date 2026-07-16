/* ═══════════════════════════════════════════════════════════════════════════
   ARTIFICER · Icons
   Minimal Lucide-rooted set, hand-tuned to system stroke weight.
   - 16px viewBox, 1.5 stroke (matches body type ascender weight)
   - Inherit color via stroke="currentColor" — never hardcode
   - Use semantic icons sparingly: max 1 per "row" of UI
   ═══════════════════════════════════════════════════════════════════════════ */

(function () {
  'use strict';

  // 16×16 paths. Stroke-only (no fills) so they read on any surface.
  // Sourced from Lucide (lucide.dev, ISC license), hand-redrawn onto the
  // 16-grid (24-grid × ⅔, then tuned) — never pasted. Keys are the
  // canonical Lucide names; legacy names live in ALIASES below.
  const PATHS = {
    // Navigation / chrome
    'menu':            'M3 4h10M3 8h10M3 12h10', // bars span the box (Lucide 6/12/18 × ⅔) — reads standing alone at 16px (#114)
    'chevron-right':   'M6 4l4 4-4 4',
    'chevron-left':    'M10 4l-4 4 4 4',
    'chevron-down':    'M4 6l4 4 4-4',
    'chevron-up':      'M4 10l4-4 4 4',
    'arrow-right':     'M3 8h10M9 4l4 4-4 4',
    'arrow-left':      'M13 8H3M7 4L3 8l4 4',
    'external-link':   'M10 3h3v3M13 3l-6 6M11 9v3H4V5h3',

    // Status / semantic
    'check':           'M3 8l3 3 7-7',
    'x':               'M4 4l8 8M12 4l-8 8',
    'triangle-alert':  'M8 2L1.5 13h13L8 2zM8 6v3M8 11.5v.01',
    'info':            'M8 1a7 7 0 100 14A7 7 0 008 1zM8 7v4M8 4.5v.01',
    'circle-dot':      'M8 1a7 7 0 100 14A7 7 0 008 1zM8 6a2 2 0 100 4 2 2 0 000-4z',
    'loader':          'M8 1.5v2.5M8 12v2.5M3.4 3.4l1.8 1.8M10.8 10.8l1.8 1.8M1.5 8H4M12 8h2.5M3.4 12.6l1.8-1.8M10.8 5.2l1.8-1.8',

    // Actions
    'plus':            'M8 3v10M3 8h10',
    'minus':           'M3 8h10',
    'search':          'M11.5 11.5L14 14M2.5 7a4.5 4.5 0 109 0 4.5 4.5 0 00-9 0z',
    'pencil':          'M11 2l3 3-8 8H3v-3l8-8zM10 3l3 3',
    'trash-2':         'M3 4h10M5.5 4V2.5h5V4M4.5 4l.5 9.5h6L11.5 4M6.5 7v4M9.5 7v4',
    'copy':            'M5 5V3a1 1 0 011-1h7a1 1 0 011 1v7a1 1 0 01-1 1h-2M3 5h7a1 1 0 011 1v7a1 1 0 01-1 1H3a1 1 0 01-1-1V6a1 1 0 011-1z',
    'download':        'M8 2v8M4 7l4 4 4-4M2 13h12',
    'upload':          'M8 11V3M4 6l4-4 4 4M2 13h12',
    'send':            'M14 2L1 8l5 2 8-8zM14 2l-6 12-2-5',
    'refresh-cw':      'M14 4v3.5h-3.5M2 12V8.5h3.5M3 6a5.5 5.5 0 0110.5-1M13 10a5.5 5.5 0 01-10.5 1',

    // Files & system
    'file':            'M4 1h6l3 3v10a1 1 0 01-1 1H4a1 1 0 01-1-1V2a1 1 0 011-1zM10 1v3h3',
    'folder':          'M2 4a1 1 0 011-1h3l1.5 1.5H13a1 1 0 011 1V13a1 1 0 01-1 1H3a1 1 0 01-1-1V4z',
    'square-terminal': 'M2 3h12a1 1 0 011 1v8a1 1 0 01-1 1H2a1 1 0 01-1-1V4a1 1 0 011-1zM4 6l2 2-2 2M8 10h4',
    'code':            'M5 4L1 8l4 4M11 4l4 4-4 4',
    'settings':        'M8 5.5a2.5 2.5 0 100 5 2.5 2.5 0 000-5zM8 1v1.5M8 13.5V15M2.5 8H1M15 8h-1.5M3.5 3.5l1 1M11.5 11.5l1 1M3.5 12.5l1-1M11.5 4.5l1-1',
    'theme':           'M8 1a7 7 0 100 14V1z', // Artificer-custom — no Lucide counterpart

    // Person
    'user':            'M8 8a3 3 0 100-6 3 3 0 000 6zM2 14c.5-3 3-5 6-5s5.5 2 6 5',
    'log-out':         'M10 4V3a1 1 0 00-1-1H3a1 1 0 00-1 1v10a1 1 0 001 1h6a1 1 0 001-1v-1M7 8h7M11 5l3 3-3 3',

    // Common objects
    'lock':            'M4 7.5h8a1 1 0 011 1v4a1 1 0 01-1 1H4a1 1 0 01-1-1v-4a1 1 0 011-1zM5.5 7.5V5a2.5 2.5 0 015 0v2.5',
    'lock-open':       'M4 7.5h8a1 1 0 011 1v4a1 1 0 01-1 1H4a1 1 0 01-1-1v-4a1 1 0 011-1zM5.5 7.5V5a2.5 2.5 0 014.9-.6',
    'layers':          'M8 2L2 5l6 3 6-3-6-3zM2 8l6 3 6-3M2 11l6 3 6-3',
    'droplet':         'M8 1.5c2 3 4.5 5.5 4.5 8.5a4.5 4.5 0 01-9 0c0-3 2.5-5.5 4.5-8.5z',
    'eye':             'M1.5 8s2.5-4.5 6.5-4.5S14.5 8 14.5 8s-2.5 4.5-6.5 4.5S1.5 8 1.5 8zM8 6.5a1.5 1.5 0 100 3 1.5 1.5 0 000-3z',
    'eye-off':         'M1.5 8s2.5-4.5 6.5-4.5S14.5 8 14.5 8s-2.5 4.5-6.5 4.5S1.5 8 1.5 8zM3 3l10 10',
    'clock':           'M8 1a7 7 0 100 14A7 7 0 008 1zM8 4v4l2.5 1.5',

    // Observability & app-nav (#228 — 8 curated additions, hand-tuned 16-grid)
    'activity':        'M1 8h3l2 5 3-10 2 5h4',                          // Metrics — ECG pulse
    'bell':            'M8 2a4 4 0 00-4 4c0 4-1.5 5-1.5 5h11s-1.5-1-1.5-5a4 4 0 00-4-4zM6.5 13.5a1.5 1.5 0 003 0', // Alerts
    'git-branch':      'M4 2v8M4 10a2 2 0 100 4 2 2 0 000-4zM12 2a2 2 0 100 4 2 2 0 000-4zM12 6a6 6 0 01-6 6', // Traces
    'users':           'M6 3a2.5 2.5 0 100 5 2.5 2.5 0 000-5zM1.5 14c0-2.8 2-4.3 4.5-4.3s4.5 1.5 4.5 4.3M11 3.2a2.5 2.5 0 010 4.6M11.8 9.8c1.6.5 2.7 2 2.7 4.2', // Team
    'box':             'M8 1.5l6 3v7l-6 3-6-3v-7l6-3zM2 4.5l6 3 6-3M8 7.5v7', // Environments — cube
    'credit-card':     'M2.5 4h11a1 1 0 011 1v6a1 1 0 01-1 1h-11a1 1 0 01-1-1V5a1 1 0 011-1zM1.5 7h13M4 10h3', // Billing
    'list':            'M6 4h8M6 8h8M6 12h8M3 4h.01M3 8h.01M3 12h.01',   // Logs — dotted rows
    'layout':          'M3.5 2h9a1.5 1.5 0 011.5 1.5v9a1.5 1.5 0 01-1.5 1.5h-9A1.5 1.5 0 012 12.5v-9A1.5 1.5 0 013.5 2zM2 6h12M6 6v8', // Dashboards — header + sidebar
  };

  // Legacy names → canonical Lucide keys. Markup written against the old
  // set keeps working; list() reports canonical names only. close/x were
  // byte-identical entries — close folds in here.
  const ALIASES = {
    'close':     'x',
    'chevron-r': 'chevron-right',
    'chevron-l': 'chevron-left',
    'chevron-d': 'chevron-down',
    'chevron-u': 'chevron-up',
    'arrow-r':   'arrow-right',
    'arrow-l':   'arrow-left',
    'external':  'external-link',
    'alert':     'triangle-alert',
    'spinner':   'loader',
    'edit':      'pencil',
    'trash':     'trash-2',
    'refresh':   'refresh-cw',
    'terminal':  'square-terminal',
    'logout':    'log-out',
  };

  const SVG_NS = 'http://www.w3.org/2000/svg';
  const hasOwn = Object.prototype.hasOwnProperty;

  // Own-property lookups only — DOM-sourced names like "constructor" must
  // miss cleanly instead of reaching the Object prototype (which made
  // build() throw mid-hydrate).
  function resolve(name) {
    if (hasOwn.call(PATHS, name)) return name;
    if (hasOwn.call(ALIASES, name)) return ALIASES[name];
    return null;
  }

  function baseSvg(opts) {
    const svg = document.createElementNS(SVG_NS, 'svg');
    const size = (opts && opts.size) || 16;
    svg.setAttribute('viewBox', '0 0 16 16');
    svg.setAttribute('width', size);
    svg.setAttribute('height', size);
    svg.setAttribute('fill', 'none');
    svg.setAttribute('stroke', 'currentColor');
    svg.setAttribute('stroke-width', (opts && opts.strokeWidth) || 1.5);
    svg.setAttribute('stroke-linecap', 'round');
    svg.setAttribute('stroke-linejoin', 'round');
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('focusable', 'false');
    svg.classList.add('icon');
    return svg;
  }

  function build(name, opts) {
    const key = resolve(name);
    if (!key) {
      console.warn('[artificer/icons] Unknown icon: ' + name);
      return null;
    }
    const svg = baseSvg(opts);
    if (opts && opts.className) {
      svg.classList.add(...opts.className.split(/\s+/).filter(Boolean));
    }

    // Split path data on M commands so each subpath is its own <path>;
    // keeps stroke joins clean across disconnected strokes.
    const subpaths = PATHS[key].split(/(?=M)/g).filter(Boolean);
    subpaths.forEach(p => {
      const path = document.createElementNS(SVG_NS, 'path');
      path.setAttribute('d', p.trim());
      svg.appendChild(path);
    });
    return svg;
  }

  // Dashed-box stand-in for an unknown name. A blank <i> was a silent
  // failure only the console knew about (#170) — the box makes the miss
  // visible in the page itself. DOM hydration only; build() keeps its
  // returns-null contract for programmatic callers.
  function buildMissing(opts) {
    const svg = baseSvg(opts);
    svg.classList.add('icon--missing');
    const rect = document.createElementNS(SVG_NS, 'rect');
    rect.setAttribute('x', '2.5');
    rect.setAttribute('y', '2.5');
    rect.setAttribute('width', '11');
    rect.setAttribute('height', '11');
    rect.setAttribute('rx', '1');
    rect.setAttribute('stroke-dasharray', '2 2');
    svg.appendChild(rect);
    return svg;
  }

  // Replace <i data-icon="name"></i> placeholders in the DOM
  function hydrate(root) {
    const scope = root || document;
    scope.querySelectorAll('[data-icon]').forEach(el => {
      if (el.dataset.iconHydrated === '1') return;
      const name = el.getAttribute('data-icon');
      const opts = {
        size: el.getAttribute('data-icon-size') || 16,
        strokeWidth: el.getAttribute('data-icon-stroke') || 1.5,
      };
      // Marking unknown names hydrated stops the warn-per-mutation-batch
      // loop; an attribute change clears the lock, so fixing data-icon
      // still rebuilds.
      const svg = build(name, opts) || buildMissing(opts);
      el.textContent = '';
      el.appendChild(svg);
      el.dataset.iconHydrated = '1';
    });
  }

  // Public API
  // SPA lifecycle — auto-hydrate nodes inserted after first paint, and
  // re-hydrate existing nodes when their data-icon* attributes change.
  // Returns a disconnect fn. Idempotent guards make the re-scan cheap.
  function observe(root) {
    root = root || document.body;
    hydrate(root);
    if (typeof MutationObserver === 'undefined') return function () {};
    var schedule = window.requestAnimationFrame
      ? function (cb) { window.requestAnimationFrame(cb); }
      : function (cb) { window.setTimeout(cb, 0); };
    var scheduled = false;
    var mo = new MutationObserver(function (mutations) {
      // Clear the hydration lock on any node whose icon attrs changed so
      // hydrate() rebuilds it rather than skipping it as already-done.
      mutations.forEach(function (m) {
        if (m.type === 'attributes' && m.target.hasAttribute('data-icon')) {
          delete m.target.dataset.iconHydrated;
        }
      });
      if (scheduled) return;
      scheduled = true;
      schedule(function () { scheduled = false; hydrate(root); });
    });
    mo.observe(root, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ['data-icon', 'data-icon-size', 'data-icon-stroke'],
    });
    return function () { mo.disconnect(); };
  }

  window.ArtificerIcons = {
    build:   build,
    hydrate: hydrate,
    observe: observe,
    list:    () => Object.keys(PATHS),
  };

  // Auto-hydrate on DOM ready, then arm the MutationObserver for SPA use.
  // observe() calls hydrate() internally, so no double-scan on static pages.
  function autoInit() {
    if (typeof MutationObserver !== 'undefined') {
      observe(document.body);
    } else {
      hydrate();
    }
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', autoInit);
  } else {
    autoInit();
  }
})();
