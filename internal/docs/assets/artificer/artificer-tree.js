/* Artificer · tree keyboard helper (WAI-ARIA APG tree pattern).
   The .tree primitive ships the LOOK (twisty, rows, indent); this ships the
   unavoidable-JS BEHAVIOR — roving tabindex on the [role="treeitem"] li,
   clamped ↑/↓ over the VISIBLE items (an APG tree never wraps), Home/End,
   →/← expand/collapse/first-child/parent, Enter/Space activate, and click
   parity — the same way artificer-tabs.js ships the tabs pattern.

   Usage:
     <ul class="tree" role="tree" data-tree>
       <li role="treeitem" aria-expanded="true" tabindex="0"> <div class="tree__row">…</div>
         <ul class="tree__group" role="group"> <li role="treeitem" tabindex="-1">…</li> </ul>
       </li>
     </ul>
     ArtificerTree.enhance(treeEl, { onSelect(item){…}, onToggle(item, open){…} });
     ArtificerTree.observe(document);   // auto-enhance every [data-tree] (SPA)

   Mechanics ONLY: expand/collapse toggles aria-expanded + hides the item's
   own .tree__group; activating a leaf moves aria-selected (single-select);
   side-effects (loading children, navigation) belong in onSelect/onToggle.
   Focus (and the ring — see the [role="treeitem"]:focus-visible rule) rides
   the li host; visibility = no [hidden] ancestor group. enhance() is
   idempotent and returns a cached handle. */
(function () {
  'use strict';

  // --- pure: vertical movement over the visible list (unit-tested, no DOM) ---
  // Returns the visible-index to move to, or null for non-movement keys.
  // Clamped at both ends — an APG tree never wraps.
  function nextVisible(key, current, count) {
    if (!(count > 0)) return null;
    switch (key) {
      case 'ArrowDown':
        return Math.min(current + 1, count - 1);
      case 'ArrowUp':
        return Math.max(current - 1, 0);
      case 'Home':
        return 0;
      case 'End':
        return count - 1;
      default:
        return null;
    }
  }

  // --- pure: the horizontal/activation action table (unit-tested, no DOM) ----
  // state.expanded: true (open parent) | false (closed parent) | null (leaf).
  // Returns 'expand' | 'first-child' | 'collapse' | 'parent' | 'activate' | null.
  function treeAction(key, state) {
    var expanded = state ? state.expanded : null;
    var hasParent = !!(state && state.hasParent);
    switch (key) {
      case 'ArrowRight':
        if (expanded === false) return 'expand';
        if (expanded === true) return 'first-child';
        return null; // leaf: APG right-arrow does nothing
      case 'ArrowLeft':
        if (expanded === true) return 'collapse';
        return hasParent ? 'parent' : null;
      case 'Enter':
      case ' ':
        return 'activate';
      default:
        return null;
    }
  }

  var noop = { select: function () {}, toggle: function () {}, destroy: function () {} };

  // --- DOM: wire one tree (verified in live-spec/components-extended.html) ---
  function enhance(treeEl, opts) {
    opts = opts || {};
    if (!treeEl) return noop;
    if (treeEl.__artificerTree) return treeEl.__artificerTree;

    function allItems() {
      return Array.prototype.slice.call(treeEl.querySelectorAll('[role="treeitem"]'));
    }
    function visibleItems() {
      // visible = no [hidden] ancestor group (closest() from the li sees them all)
      return allItems().filter(function (it) {
        return !it.closest('[hidden]');
      });
    }
    function expandedState(item) {
      var v = item.getAttribute('aria-expanded');
      return v === null ? null : v === 'true'; // null = leaf
    }
    function parentItem(item) {
      var p = item.parentElement && item.parentElement.closest('[role="treeitem"]');
      return p && treeEl.contains(p) ? p : null;
    }

    function setCurrent(item, focus) {
      allItems().forEach(function (t) {
        t.setAttribute('tabindex', t === item ? '0' : '-1');
      });
      if (focus) item.focus();
    }

    function select(item) {
      // aria-selected belongs on the [role="treeitem"], not the row (single-select)
      allItems().forEach(function (t) {
        if (t === item) t.setAttribute('aria-selected', 'true');
        else t.removeAttribute('aria-selected');
      });
      if (typeof opts.onSelect === 'function') opts.onSelect(item);
    }

    function toggle(item, open) {
      if (typeof open !== 'boolean') open = expandedState(item) !== true;
      item.setAttribute('aria-expanded', String(open));
      var group = item.querySelector(':scope > .tree__group');
      if (group) group.hidden = !open;
      if (typeof opts.onToggle === 'function') opts.onToggle(item, open);
    }

    // activate = the click path: parents toggle, leaves select
    function activate(item) {
      if (expandedState(item) === null) select(item);
      else toggle(item);
    }

    function onClick(e) {
      var row = e.target.closest('.tree__row');
      if (!row) return;
      var item = row.closest('[role="treeitem"]');
      if (!item || !treeEl.contains(item)) return;
      setCurrent(item, false); // click also moves the roving tabindex
      activate(item);
    }

    function onKeydown(e) {
      var item = e.target.closest('[role="treeitem"]');
      if (!item || !treeEl.contains(item)) return;

      var items = visibleItems();
      var move = nextVisible(e.key, items.indexOf(item), items.length);
      if (move !== null) {
        e.preventDefault();
        setCurrent(items[move], true);
        return;
      }

      var action = treeAction(e.key, {
        expanded: expandedState(item),
        hasParent: !!parentItem(item),
      });
      if (!action) return;
      e.preventDefault();
      if (action === 'expand') toggle(item, true);
      else if (action === 'collapse') toggle(item, false);
      else if (action === 'first-child') {
        var child = item.querySelector(':scope > .tree__group > [role="treeitem"]');
        if (child) setCurrent(child, true);
      } else if (action === 'parent') {
        setCurrent(parentItem(item), true);
      } else if (action === 'activate') {
        activate(item);
      }
    }

    // Initial roving tabindex: an existing tabindex="0" wins (idempotent over
    // markup that already roves), else the selected item, else the first.
    var items = allItems();
    if (items.length) {
      var keep = -1;
      items.forEach(function (t, j) {
        if (keep < 0 && t.getAttribute('tabindex') === '0') keep = j;
      });
      if (keep < 0) {
        items.forEach(function (t, j) {
          if (keep < 0 && t.getAttribute('aria-selected') === 'true') keep = j;
        });
      }
      setCurrent(items[keep < 0 ? 0 : keep], false);
    }

    treeEl.addEventListener('click', onClick);
    treeEl.addEventListener('keydown', onKeydown);

    var handle = {
      select: select,
      toggle: toggle,
      destroy: function () {
        treeEl.removeEventListener('click', onClick);
        treeEl.removeEventListener('keydown', onKeydown);
        delete treeEl.__artificerTree;
      },
    };
    treeEl.__artificerTree = handle;
    return handle;
  }

  // Auto-enhance every [data-tree] now and on insert (SPA lifecycle, matching
  // ArtificerTabs.observe). Returns a disconnect fn.
  function observe(root) {
    root = root || (typeof document !== 'undefined' ? document : null);
    if (!root) return function () {};
    function enhanceIn(scope) {
      if (scope.matches && scope.matches('[data-tree]')) enhance(scope);
      if (scope.querySelectorAll) {
        Array.prototype.forEach.call(scope.querySelectorAll('[data-tree]'), function (el) {
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

  var api = { enhance: enhance, observe: observe, nextVisible: nextVisible, treeAction: treeAction };
  // globalThis === window in a browser (so window.ArtificerTree works);
  // in Node it lets the unit test import this file and exercise the pure fns.
  if (typeof window !== 'undefined') window.ArtificerTree = api;
  else if (typeof globalThis !== 'undefined') globalThis.ArtificerTree = api;
})();
