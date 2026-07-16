/* Artificer · tablist keyboard helper (WAI-ARIA APG tabs pattern).
   The .tabs primitive ships the LOOK (active underline keyed off
   [aria-selected]); this ships the unavoidable-JS BEHAVIOR — roving
   tabindex, arrow-key movement, Home/End, selection, and panel toggling
   via aria-controls — the same way artificer-focus.js ships focus-trapping.

   Usage (in-page view-switching tabs — see .tabs doctrine, "switch the VIEW"):
     <div class="tabs" data-tabs>
       <button aria-controls="p1" aria-selected="true">Overview</button>
       <button aria-controls="p2">Activity</button>
     </div>
     <section id="p1">…</section>
     <section id="p2">…</section>

     ArtificerTabs.enhance(tablistEl, { onSelect(tab, i){…} });   // one element
     ArtificerTabs.observe(document);   // auto-enhance every [data-tabs] (SPA)

   Orientation follows aria-orientation (default "horizontal"): horizontal uses
   ←/→, vertical uses ↑/↓. enhance() is idempotent and returns a handle with
   { select(i), destroy() }. Anchors used as tabs are treated as in-page
   (default click prevented) — for cross-page navigation use .sidenav, not .tabs. */
(function () {
  'use strict';

  // --- pure: the roving-tabindex state machine (unit-tested, no DOM) ---------
  // Returns the index the focus/selection should move to for `key`, or null if
  // the key isn't a tab-navigation key (caller does nothing, no preventDefault).
  function nextIndex(key, current, count, opts) {
    if (!(count > 0)) return null;
    var vertical = opts && opts.orientation === 'vertical';
    var fwd = vertical ? 'ArrowDown' : 'ArrowRight';
    var back = vertical ? 'ArrowUp' : 'ArrowLeft';
    switch (key) {
      case fwd:
        return (current + 1) % count;
      case back:
        return (current - 1 + count) % count;
      case 'Home':
        return 0;
      case 'End':
        return count - 1;
      default:
        return null;
    }
  }

  var noop = { select: function () {}, destroy: function () {} };

  // --- DOM: wire one tablist (verified in live-spec/navigation.html) ---------
  function enhance(tablist, opts) {
    opts = opts || {};
    if (!tablist) return noop;
    if (tablist.__artificerTabs) return tablist.__artificerTabs;

    var tabs = Array.prototype.filter.call(tablist.children, function (n) {
      return n.matches && n.matches('button, a, [role="tab"]');
    });
    if (!tabs.length) return noop;

    var orientation =
      opts.orientation || tablist.getAttribute('aria-orientation') || 'horizontal';

    tablist.setAttribute('role', 'tablist');
    tablist.setAttribute('aria-orientation', orientation);

    function panelOf(tab) {
      var id = tab.getAttribute('aria-controls');
      return id ? document.getElementById(id) : null;
    }

    // Initial selection: an explicit aria-selected / aria-current wins, else first.
    var selected = -1;
    tabs.forEach(function (t, j) {
      if (
        selected < 0 &&
        (t.getAttribute('aria-selected') === 'true' ||
          t.getAttribute('aria-current') === 'page')
      ) {
        selected = j;
      }
    });
    if (selected < 0) selected = 0;

    function render(i, focus) {
      tabs.forEach(function (tab, j) {
        var on = j === i;
        tab.setAttribute('role', 'tab');
        tab.setAttribute('aria-selected', on ? 'true' : 'false');
        tab.tabIndex = on ? 0 : -1;
        var panel = panelOf(tab);
        if (panel) {
          panel.setAttribute('role', 'tabpanel');
          if (tab.id) panel.setAttribute('aria-labelledby', tab.id);
          panel.hidden = !on;
          if (on) panel.tabIndex = 0;
        }
      });
      selected = i;
      if (focus) tabs[i].focus();
      if (typeof opts.onSelect === 'function') opts.onSelect(tabs[i], i);
    }

    function onKeydown(e) {
      var target = nextIndex(e.key, selected, tabs.length, { orientation: orientation });
      if (target === null) return;
      e.preventDefault();
      render(target, true);
    }
    function onClick(e) {
      var tab = e.target.closest('button, a, [role="tab"]');
      if (!tab || !tablist.contains(tab)) return;
      var i = tabs.indexOf(tab);
      if (i < 0) return;
      if (tab.tagName === 'A') e.preventDefault(); // in-page tab, not navigation
      render(i, true);
    }

    tablist.addEventListener('keydown', onKeydown);
    tablist.addEventListener('click', onClick);
    render(selected, false); // sync ARIA/tabindex/panels without stealing focus

    var handle = {
      select: function (i) {
        render(i, false);
      },
      destroy: function () {
        tablist.removeEventListener('keydown', onKeydown);
        tablist.removeEventListener('click', onClick);
        delete tablist.__artificerTabs;
      },
    };
    tablist.__artificerTabs = handle;
    return handle;
  }

  // Auto-enhance every [data-tabs] now, and any inserted later (SPA lifecycle,
  // matching ArtificerIcons.observe / Whimsy.observe). Returns a disconnect fn.
  function observe(root) {
    root = root || (typeof document !== 'undefined' ? document : null);
    if (!root) return function () {};
    function enhanceIn(scope) {
      if (scope.matches && scope.matches('[data-tabs]')) enhance(scope);
      if (scope.querySelectorAll) {
        Array.prototype.forEach.call(scope.querySelectorAll('[data-tabs]'), function (el) {
          enhance(el);
        });
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

  var api = { enhance: enhance, observe: observe, nextIndex: nextIndex };
  // globalThis === window in a browser (so window.ArtificerTabs works);
  // in Node it lets the unit test import this file and exercise nextIndex.
  if (typeof window !== 'undefined') window.ArtificerTabs = api;
  else if (typeof globalThis !== 'undefined') globalThis.ArtificerTabs = api;
})();
