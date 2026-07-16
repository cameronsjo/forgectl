/* Artificer · option-navigation core (WAI-ARIA APG listbox/menu patterns).
   The option-popover primitive (.menu / .listbox) ships the LOOK; this ships
   the unavoidable-JS BEHAVIOR for BOTH of its keyboard models — the same way
   artificer-tabs.js ships the tabs pattern (#92 precedent):

     enhance(el)            roving-tabindex mode — a standalone role="listbox"
                            or role="menu": arrows move focus, Home/End,
                            type-to-select, Enter/Space selects.
     combobox(input, list)  activedescendant mode — a combobox input driving a
                            listbox popup (the command-palette recipe): focus
                            stays PINNED to the input; arrows move .is-active +
                            aria-activedescendant over the VISIBLE options.

   Usage:
     <ul class="listbox" role="listbox" data-options>…</ul>      → observe()
     <div class="menu" role="menu" data-options>…</div>          → observe()
     <input data-combobox="listId">                              → observe()
     ArtificerOptions.enhance(el, { onSelect(opt, i){…}, wrap });
     ArtificerOptions.combobox(input, list, { onSelect(opt, i){…} });
     ArtificerOptions.observe(document);   // auto-enhance (SPA lifecycle)

   Scope — keyboard/ARIA mechanics ONLY (deliberate hard line):
   · printable keys are never consumed in combobox mode (typing is the
     consumer's filter; type-to-select lives only in roving mode)
   · Escape is never handled (ArtificerFocus.trap owns it)
   · open/close, filtering, and dispatch stay consumer-side; the filtering
     contract is: hide filtered-out options with [hidden], then refresh()
   · aria-disabled options stay FOCUSABLE/navigable but select() no-ops
     (the APG-correct half of disabled options — never skipped, never inert
     to the cursor)

   Movement clamps by default (APG listbox never wraps); menus wrap (APG menu).
   enhance()/combobox() are idempotent and return cached handles. */
(function () {
  'use strict';

  // --- pure: the cursor state machine (unit-tested, no DOM) ------------------
  // Returns the index the cursor should move to for `key`, or null if the key
  // isn't an option-navigation key (caller does nothing, no preventDefault).
  // opts.wrap default false = clamp (APG listbox); menus pass wrap: true.
  function nextOption(key, current, count, opts) {
    if (!(count > 0)) return null;
    var wrap = !!(opts && opts.wrap);
    switch (key) {
      case 'ArrowDown':
        return wrap ? (current + 1) % count : Math.min(current + 1, count - 1);
      case 'ArrowUp':
        return wrap ? (current - 1 + count) % count : Math.max(current - 1, 0);
      case 'Home':
        return 0;
      case 'End':
        return count - 1;
      default:
        return null;
    }
  }

  // --- pure: typeahead (unit-tested, no DOM) ---------------------------------
  // Case-insensitive startsWith over labels, searching current+1…end then
  // 0…current (current itself last, so a buffer that still prefixes the
  // current option stays put). A buffer of one repeated char ("ddd") cycles
  // through options starting with that char. Returns the matched index, or
  // null when nothing matches.
  function matchOption(labels, buffer, current) {
    if (!labels || !labels.length || !buffer) return null;
    var b = String(buffer).toLowerCase();
    var allSame = b.length > 1;
    for (var k = 1; k < b.length; k++) if (b[k] !== b[0]) { allSame = false; break; }
    var needle = allSame ? b[0] : b;
    var n = labels.length;
    var from = typeof current === 'number' && current >= 0 ? current : -1;
    for (var step = 1; step <= n; step++) {
      var i = (from + step) % n;
      var label = String(labels[i] == null ? '' : labels[i]).trim().toLowerCase();
      if (label.indexOf(needle) === 0) return i;
    }
    return null;
  }

  var noop = { setActive: function () {}, select: function () {}, refresh: function () {}, destroy: function () {} };
  var OPTION_SELECTOR = '[role="option"], [role="menuitem"]';
  var uid = 0;

  function isDisabled(opt) {
    return opt.getAttribute('aria-disabled') === 'true';
  }
  function isHidden(opt) {
    return !!opt.closest('[hidden]');
  }

  // --- DOM: roving-tabindex mode (standalone .listbox / .menu) ---------------
  function enhance(el, opts) {
    opts = opts || {};
    if (!el) return noop;
    if (el.__artificerOptions) return el.__artificerOptions;

    var options = [];
    var current = 0;
    var isMenu = el.getAttribute('role') === 'menu';
    var wrap = typeof opts.wrap === 'boolean' ? opts.wrap : isMenu; // APG: menus wrap, listboxes clamp
    var buffer = '';
    var bufferTimer = null;

    function refresh() {
      options = Array.prototype.slice.call(el.querySelectorAll(OPTION_SELECTOR));
      if (!options.length) return;
      // Initial/retained cursor: existing tabindex="0" wins, else the selected
      // option, else first — idempotent over markup that already roves.
      var keep = -1;
      options.forEach(function (o, j) {
        if (keep < 0 && o.getAttribute('tabindex') === '0') keep = j;
      });
      if (keep < 0) {
        options.forEach(function (o, j) {
          if (keep < 0 && o.getAttribute('aria-selected') === 'true') keep = j;
        });
      }
      current = keep < 0 ? 0 : keep;
      render(current, false);
    }

    function render(i, focus) {
      options.forEach(function (o, j) {
        o.setAttribute('tabindex', j === i ? '0' : '-1');
      });
      current = i;
      if (focus) options[i].focus();
    }

    function select(i) {
      var opt = options[i];
      if (!opt || isDisabled(opt)) return; // focusable but inert
      if (!isMenu) {
        // single-select listbox: aria-selected moves (mechanics only)
        options.forEach(function (o) {
          if (o === opt) o.setAttribute('aria-selected', 'true');
          else o.removeAttribute('aria-selected');
        });
      }
      if (typeof opts.onSelect === 'function') opts.onSelect(opt, i);
    }

    function onKeydown(e) {
      var i = options.indexOf(e.target.closest(OPTION_SELECTOR));
      if (i < 0) return;
      var target = nextOption(e.key, i, options.length, { wrap: wrap });
      if (target !== null) {
        e.preventDefault();
        render(target, true);
        return;
      }
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault(); // also stops a native button click double-firing
        select(i);
        return;
      }
      // type-to-select — single printable chars accumulate for 500ms
      if (e.key.length === 1 && e.key !== ' ' && !e.ctrlKey && !e.metaKey && !e.altKey) {
        buffer += e.key;
        clearTimeout(bufferTimer);
        bufferTimer = setTimeout(function () { buffer = ''; }, 500);
        var labels = options.map(function (o) { return o.textContent; });
        var match = matchOption(labels, buffer, i);
        if (match !== null) render(match, true);
      }
    }

    function onClick(e) {
      var opt = e.target.closest(OPTION_SELECTOR);
      if (!opt || !el.contains(opt)) return;
      var i = options.indexOf(opt);
      if (i < 0) return;
      render(i, true); // click also moves the roving tabindex
      select(i);
    }

    el.addEventListener('keydown', onKeydown);
    el.addEventListener('click', onClick);
    refresh();

    var handle = {
      setActive: function (i) { render(i, true); },
      select: select,
      refresh: refresh,
      destroy: function () {
        el.removeEventListener('keydown', onKeydown);
        el.removeEventListener('click', onClick);
        clearTimeout(bufferTimer);
        delete el.__artificerOptions;
      },
    };
    el.__artificerOptions = handle;
    return handle;
  }

  // --- DOM: activedescendant mode (combobox input + listbox popup) -----------
  function combobox(input, list, opts) {
    opts = opts || {};
    if (!input || !list) return noop;
    if (input.__artificerCombobox) return input.__artificerCombobox;

    if (!list.id) list.id = 'artificer-combobox-list-' + ++uid;
    // Combobox ARIA on the input — only where the markup hasn't already said it.
    if (!input.getAttribute('role')) input.setAttribute('role', 'combobox');
    if (!input.getAttribute('aria-controls')) input.setAttribute('aria-controls', list.id);
    if (!input.getAttribute('aria-autocomplete')) input.setAttribute('aria-autocomplete', 'list');
    // The module owns no open/close: the popup is perceivable whenever its
    // dialog is, so a static expanded=true is correct.
    if (!input.getAttribute('aria-expanded')) input.setAttribute('aria-expanded', 'true');

    var visible = [];
    var current = -1;

    function visibleOptions() {
      return Array.prototype.slice
        .call(list.querySelectorAll('[role="option"]'))
        .filter(function (o) { return !isHidden(o); });
    }

    function setActive(i) {
      visible.forEach(function (o, j) { o.classList.toggle('is-active', j === i); });
      current = i;
      var opt = visible[i];
      if (opt) {
        // mint a STABLE unique id, never the visible index — after filtering,
        // a different option at the same index would collide with an id
        // minted earlier (duplicate DOM ids → ambiguous activedescendant)
        if (!opt.id) opt.id = list.id + '-opt-' + ++uid;
        input.setAttribute('aria-activedescendant', opt.id);
      } else {
        input.removeAttribute('aria-activedescendant');
      }
    }

    // refresh() — the consumer just filtered (toggled [hidden]): recompute the
    // visible set, keep the cursor's option if it survived, else re-clamp; an
    // empty set clears the cursor + aria-activedescendant.
    function refresh() {
      var keep = visible[current] || null;
      visible = visibleOptions();
      if (!visible.length) { setActive(-1); return; }
      var kept = keep ? visible.indexOf(keep) : -1;
      if (kept < 0) {
        // adopt a pre-marked cursor (idempotent over PR-1-style markup)…
        kept = -1;
        visible.forEach(function (o, j) {
          if (kept < 0 && o.classList.contains('is-active')) kept = j;
        });
        // …else clamp the old index into the new range
        if (kept < 0) kept = Math.max(0, Math.min(current, visible.length - 1));
      }
      setActive(kept);
    }

    function select(i) {
      var opt = visible[i];
      if (!opt || isDisabled(opt)) return;
      if (typeof opts.onSelect === 'function') opts.onSelect(opt, i);
    }

    // Hard scope line: arrows/Home/End/Enter only. Printable keys are NEVER
    // consumed (typing = the consumer's filter); Escape is NEVER handled
    // (ArtificerFocus.trap owns it).
    function onKeydown(e) {
      var target = nextOption(e.key, current, visible.length, { wrap: false });
      if (target !== null) {
        e.preventDefault();
        setActive(target);
        return;
      }
      if (e.key === 'Enter') {
        if (current < 0) return;
        e.preventDefault();
        select(current);
      }
    }

    function onClick(e) {
      var opt = e.target.closest('[role="option"]');
      if (!opt || !list.contains(opt)) return;
      var i = visible.indexOf(opt);
      if (i < 0) return;
      setActive(i); // click parity with the keyboard cursor
      select(i);
    }

    input.addEventListener('keydown', onKeydown);
    list.addEventListener('click', onClick);
    refresh();

    var handle = {
      setActive: setActive,
      select: select,
      refresh: refresh,
      destroy: function () {
        input.removeEventListener('keydown', onKeydown);
        list.removeEventListener('click', onClick);
        delete input.__artificerCombobox;
      },
    };
    input.__artificerCombobox = handle;
    return handle;
  }

  // Auto-enhance [data-options] (roving) and input[data-combobox="listId"]
  // (activedescendant) now and on insert (SPA lifecycle, matching
  // ArtificerTabs.observe). Returns a disconnect fn.
  function observe(root) {
    root = root || (typeof document !== 'undefined' ? document : null);
    if (!root) return function () {};
    function hydrate(el) {
      if (el.matches && el.matches('[data-options]')) enhance(el);
      if (el.matches && el.matches('input[data-combobox]')) {
        var list = document.getElementById(el.getAttribute('data-combobox'));
        if (list) combobox(el, list);
      }
    }
    function enhanceIn(scope) {
      hydrate(scope);
      if (scope.querySelectorAll) {
        Array.prototype.forEach.call(
          scope.querySelectorAll('[data-options], input[data-combobox]'),
          hydrate
        );
      }
    }
    enhanceIn(root);
    if (typeof MutationObserver === 'undefined') return function () {};
    var mo = new MutationObserver(function (muts) {
      muts.forEach(function (m) {
        Array.prototype.forEach.call(m.addedNodes || [], function (n) {
          if (n.nodeType === 1) enhanceIn(n);
        });
      });
    });
    mo.observe(root === document ? document.body : root, { childList: true, subtree: true });
    return function () {
      mo.disconnect();
    };
  }

  var api = {
    enhance: enhance,
    combobox: combobox,
    observe: observe,
    nextOption: nextOption,
    matchOption: matchOption,
  };
  // globalThis === window in a browser (so window.ArtificerOptions works);
  // in Node it lets the unit test import this file and exercise the pure fns.
  if (typeof window !== 'undefined') window.ArtificerOptions = api;
  else if (typeof globalThis !== 'undefined') globalThis.ArtificerOptions = api;
})();
