// Artificer · theme handling (persisted in localStorage, shared across pages)
//
// Three modes, one key: 'dark' | 'light' | 'auto'. Explicit modes pin the
// theme; 'auto' (or nothing stored) follows prefers-color-scheme live. The
// toggle cycles dark → light → auto. The canonical control is an EMPTY
// <button class="theme-toggle" data-theme-toggle aria-label="Toggle theme">
// — bind() injects the half-circle `theme` glyph itself (no dependency on
// artificer-icons.js) and keeps aria-label/title narrating state + next
// action. Legacy markup (.dot + [data-theme-label]) keeps working; the
// label reads Dark / Light / Auto.
(function () {
  var KEY = 'artificer.theme';
  var MODES = ['dark', 'light', 'auto'];
  var SVG_NS = 'http://www.w3.org/2000/svg';
  var GLYPH = 'M8 1a7 7 0 100 14V1z'; // the Artificer-custom `theme` mark

  function systemTheme() {
    return window.matchMedia &&
      window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
  }

  function storedMode() {
    var s = null;
    try { s = localStorage.getItem(KEY); } catch (e) {}
    return MODES.indexOf(s) > -1 ? s : 'auto';
  }

  function resolvedTheme(mode) {
    return mode === 'auto' ? systemTheme() : mode;
  }

  // Paint the mode everywhere: the html attribute, plus state narration on
  // every toggle (data-theme-mode for styling hooks, aria-label/title for
  // people, the legacy label span if present).
  function reflect(mode) {
    var theme = resolvedTheme(mode);
    document.documentElement.setAttribute('data-theme', theme);
    var next = MODES[(MODES.indexOf(mode) + 1) % MODES.length];
    var state = mode === 'auto' ? 'auto (' + theme + ')' : mode;
    document.querySelectorAll('[data-theme-toggle]').forEach(function (b) {
      b.setAttribute('data-theme-mode', mode);
      b.setAttribute('aria-label', 'Theme: ' + state + '. Switch to ' + next + '.');
      b.setAttribute('title', 'Theme: ' + state);
      var label = b.querySelector('[data-theme-label]');
      if (label) {
        label.textContent = mode === 'auto' ? 'Auto'
          : theme === 'light' ? 'Light' : 'Dark';
      }
    });
  }

  function apply(next) {
    if (MODES.indexOf(next) === -1) {
      console.warn('[artificer/theme] Unknown mode: ' + next);
      return;
    }
    try { localStorage.setItem(KEY, next); } catch (e) {}
    reflect(next);
  }

  function toggle() {
    apply(MODES[(MODES.indexOf(storedMode()) + 1) % MODES.length]);
  }

  // The canonical empty button gets the glyph injected; buttons that ship
  // their own children (legacy dot + label, or a consumer's custom icon)
  // are left untouched.
  function ensureIcon(b) {
    if (b.firstElementChild) return;
    var svg = document.createElementNS(SVG_NS, 'svg');
    svg.setAttribute('viewBox', '0 0 16 16');
    svg.setAttribute('width', 16);
    svg.setAttribute('height', 16);
    svg.setAttribute('fill', 'none');
    svg.setAttribute('stroke', 'currentColor');
    svg.setAttribute('stroke-width', 1.5);
    svg.setAttribute('stroke-linecap', 'round');
    svg.setAttribute('stroke-linejoin', 'round');
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('focusable', 'false');
    svg.classList.add('icon');
    var path = document.createElementNS(SVG_NS, 'path');
    path.setAttribute('d', GLYPH);
    svg.appendChild(path);
    b.appendChild(svg);
    b.classList.add('theme-toggle--icon');
  }

  function bind() {
    var fresh = false;
    document.querySelectorAll('[data-theme-toggle]').forEach(function (b) {
      if (b.dataset.themeBound === '1') return;
      b.dataset.themeBound = '1';
      fresh = true;
      ensureIcon(b);
      b.addEventListener('click', toggle);
    });
    if (fresh) reflect(storedMode());
  }

  // In auto, track the OS preference live. Pinned modes ignore the event.
  if (window.matchMedia) {
    var mq = window.matchMedia('(prefers-color-scheme: light)');
    var onChange = function () { if (storedMode() === 'auto') reflect('auto'); };
    if (mq.addEventListener) mq.addEventListener('change', onChange);
    else if (mq.addListener) mq.addListener(onChange);
  }

  // Early paint: an explicit stored mode applies before bind so pages
  // without the pre-paint bootstrap still land on the pinned theme fast.
  if (storedMode() !== 'auto') {
    document.documentElement.setAttribute('data-theme', storedMode());
  }

  // SPA lifecycle — auto-hydrate nodes inserted after first paint. Returns a
  // disconnect fn. Idempotent guards make the re-scan cheap (done nodes skip).
  function observe(root) {
    root = root || document.body;
    bind();
    if (typeof MutationObserver === 'undefined') return function () {};
    var schedule = window.requestAnimationFrame
      ? function (cb) { window.requestAnimationFrame(cb); }
      : function (cb) { window.setTimeout(cb, 0); };
    var scheduled = false;
    var mo = new MutationObserver(function () {
      if (scheduled) return;
      scheduled = true;
      schedule(function () { scheduled = false; bind(); });
    });
    mo.observe(root, { childList: true, subtree: true });
    return function () { mo.disconnect(); };
  }
  // KEY is exposed as the ONE canonical definition of the persistence key
  // (#36 item 1): framework adapters read window.ArtificerTheme.KEY instead of
  // re-hardcoding the 'artificer.theme' literal, so the dot/hyphen split can't
  // reopen. This is a classic <script> (not an ESM), so the key ships on the
  // runtime object rather than as an `export`.
  window.ArtificerTheme = { apply: apply, toggle: toggle, bind: bind, observe: observe, KEY: KEY };

  // Auto-bind on DOM ready, then arm the MutationObserver for SPA use —
  // the same autoInit shape as artificer-icons.js. observe() calls bind()
  // internally, so static pages see no double-scan; SPA-mounted
  // [data-theme-toggle] buttons now bind without manual observe() arming.
  function autoInit() {
    if (typeof MutationObserver !== 'undefined') {
      observe(document.body);
    } else {
      bind();
    }
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', autoInit);
  } else {
    autoInit();
  }
})();
