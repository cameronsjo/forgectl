// Live-reload client for `forgectl docs serve`.
//
// Listens on the server's SSE endpoint and, when a watched markdown file
// changes, re-fetches the current page and swaps the changed regions into the
// live document instead of reloading it. A swap keeps what a reload throws
// away: the reading position, which folders and <details> the reader opened,
// the sidenav's own scroll, and a live filter query.
//
// THE SCROLL CONTAINER IS <main>, NOT THE WINDOW. The shell is a fixed-height
// flex column and each pane scrolls on its own, so window.scrollY is always 0.
// The first version of this file measured and scrolled the window, which is
// why reload used to land the reader back at the top.
//
// Position is kept as the nearest HEADING plus a delta, not a pixel offset:
// the point of live reload is watching a document change while reading it, and
// an edit above the viewport shifts everything below. A heading id is stable
// under edits elsewhere in the file (goldmark derives it from the heading
// text), so restoring to it keeps the reader in the same paragraph.
//
// Anything the swap cannot express falls back to a full reload with the same
// anchor carried through sessionStorage: a failed fetch, a doc that no longer
// resolves, or a page whose layout changed shape (an outline appearing or
// going away).
(function () {
  "use strict";

  var STORAGE_KEY = "forgectl-docs-scroll";

  function scroller() {
    return document.querySelector("main");
  }

  // The last heading at or above the top of the doc pane, and how far past it
  // the reader had scrolled. Null above the first heading.
  function captureAnchor() {
    var main = scroller();
    if (!main) { return null; }
    var top = main.getBoundingClientRect().top;
    var found = null;
    main.querySelectorAll(":is(h1,h2,h3,h4,h5,h6)[id]").forEach(function (h) {
      if (h.getBoundingClientRect().top <= top + 1) { found = h; }
    });
    if (!found) { return null; }
    return { id: found.id, delta: Math.round(top - found.getBoundingClientRect().top) };
  }

  // Scroll <main> so the anchored heading sits where it was. A heading that
  // was renamed or deleted yields nothing, and the pane keeps its pixel
  // offset, which is the best remaining guess.
  function applyAnchor(anchor) {
    var main = scroller();
    if (!main || !anchor) { return; }
    var target = document.getElementById(anchor.id);
    if (!target || !main.contains(target)) { return; }
    var offset = target.getBoundingClientRect().top - main.getBoundingClientRect().top;
    main.scrollTop += offset + (anchor.delta || 0);
  }

  // A stable key per <details>: sidenav folders by their path of summary
  // labels, document collapsibles by their order in the body.
  function detailsKey(d, i) {
    var parts = [];
    for (var el = d; el; el = el.parentElement && el.parentElement.closest("details")) {
      var label = el.querySelector(":scope > summary .label");
      parts.unshift(label ? label.textContent : "#" + i);
    }
    return parts.join("/");
  }

  function openDetails(root) {
    var open = {};
    if (!root) { return open; }
    root.querySelectorAll("details").forEach(function (d, i) {
      open[detailsKey(d, i)] = d.open;
    });
    return open;
  }

  // Re-apply the reader's open/closed choices; a key the reader never saw
  // keeps the server's default. keepServerOpen is for the sidenav: a folder
  // the server opened because the current doc sits in it stays open.
  function restoreDetails(root, open, keepServerOpen) {
    if (!root) { return; }
    root.querySelectorAll("details").forEach(function (d, i) {
      var k = detailsKey(d, i);
      if (Object.prototype.hasOwnProperty.call(open, k)) {
        d.open = open[k] || (keepServerOpen && d.open);
      }
    });
  }

  function replace(sel, fresh) {
    var old = document.querySelector(sel);
    var next = fresh.querySelector(sel);
    if (old && next) { old.replaceWith(document.importNode(next, true)); }
  }

  // swap moves the fresh page's changing regions into the live one. Returns
  // false when the page changed shape and only a full reload renders it right.
  function swap(fresh) {
    var hadOutline = !!document.querySelector("aside.outline");
    if (hadOutline !== !!fresh.querySelector("aside.outline")) { return false; }
    if (!fresh.querySelector("main") || !fresh.querySelector("nav.sidenav")) { return false; }

    var main = scroller();
    var nav = document.querySelector("nav.sidenav");
    var anchor = captureAnchor();
    var navScroll = nav ? nav.scrollTop : 0;
    var navOpen = openDetails(nav);
    var bodyOpen = openDetails(document.querySelector(".doc-body"));
    var inlineOutline = document.querySelector("details.outline-inline");
    var inlineOpen = inlineOutline ? inlineOutline.open : false;

    document.title = fresh.title;
    // <main> itself stays: it is the scroll container, and svg-panzoom.js
    // observes it. Only its contents change.
    main.replaceChildren.apply(main, Array.prototype.map.call(
      fresh.querySelector("main").childNodes, function (n) { return document.importNode(n, true); }));
    replace("nav.sidenav", fresh);
    replace("aside.outline", fresh);
    replace("footer.statusbar", fresh);

    nav = document.querySelector("nav.sidenav");
    restoreDetails(nav, navOpen, true);
    if (nav) { nav.scrollTop = navScroll; }
    restoreDetails(document.querySelector(".doc-body"), bodyOpen, false);
    inlineOutline = document.querySelector("details.outline-inline");
    if (inlineOutline) { inlineOutline.open = inlineOpen; }

    var filter = document.getElementById("doc-filter");
    if (filter && filter.value.trim() !== "") {
      filter.dispatchEvent(new Event("input"));
    }
    if (window.ForgectlMermaid) { window.ForgectlMermaid.refresh(); }
    applyAnchor(anchor);
    return true;
  }

  function fullReload() {
    try {
      sessionStorage.setItem(STORAGE_KEY, JSON.stringify({
        path: location.pathname,
        anchor: captureAnchor(),
        top: scroller() ? scroller().scrollTop : 0
      }));
    } catch (e) { /* private mode: reading position is not worth failing over */ }
    location.reload();
  }

  // After a fallback full reload, put the pane back where it was.
  function restoreAfterReload() {
    var raw;
    try {
      raw = sessionStorage.getItem(STORAGE_KEY);
      sessionStorage.removeItem(STORAGE_KEY);
    } catch (e) {
      return;
    }
    if (!raw) { return; }
    var saved;
    try { saved = JSON.parse(raw); } catch (e) { return; }
    // Only onto the same document: following a sidenav link during a reload
    // must not scroll the new doc to an unrelated heading.
    if (!saved || saved.path !== location.pathname) { return; }
    var main = scroller();
    if (!main) { return; }
    main.scrollTop = saved.top || 0;
    applyAnchor(saved.anchor);
  }

  // Changes arrive in bursts (an editor's save is often several writes), so a
  // reload that starts while one is in flight runs once more afterwards rather
  // than overlapping it.
  var busy = false;
  var again = false;

  function refresh() {
    if (busy) { again = true; return; }
    busy = true;
    fetch(location.pathname, { cache: "no-store", credentials: "same-origin" })
      .then(function (res) {
        if (!res.ok) { throw new Error("status " + res.status); }
        return res.text();
      })
      .then(function (html) {
        var fresh = new DOMParser().parseFromString(html, "text/html");
        if (!swap(fresh)) { fullReload(); }
      })
      .catch(function () { fullReload(); })
      .then(function () {
        busy = false;
        if (again) { again = false; refresh(); }
      });
  }

  function connect() {
    var source = new EventSource("/events");
    source.onmessage = refresh;

    // EventSource reconnects on its own after a transient drop. It does NOT
    // recover once the server exits, which is the common case here (Ctrl-C in
    // the terminal) — so stop retrying after a run of failures instead of
    // reconnecting forever against a port nothing is listening on.
    var failures = 0;
    source.onopen = function () { failures = 0; };
    source.onerror = function () {
      if (source.readyState === EventSource.CLOSED) { return; }
      if (++failures >= 10) { source.close(); }
    };
  }

  restoreAfterReload();
  connect();
})();
