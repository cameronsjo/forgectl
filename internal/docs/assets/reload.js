// Live-reload client for `forgectl docs serve`.
//
// Listens on the server's SSE endpoint and reloads the page when a watched
// markdown file changes, preserving reading position across the reload by
// remembering the nearest HEADING rather than a pixel offset.
//
// Why an anchor and not scrollY: the point of live reload here is watching a
// document change while reading it, and an edit above the viewport shifts
// everything below it. Restoring a pixel offset would land the reader in a
// different paragraph than the one they were on — the more the document changed,
// the further off it lands, which is exactly backwards. A heading id is stable
// under edits elsewhere in the file (goldmark's auto-heading-id extension
// derives it from the heading text), so restoring to a heading plus a small
// within-section delta keeps the reader where they were.
(function () {
  "use strict";

  var STORAGE_KEY = "forgectl-docs-scroll";

  function headings() {
    return document.querySelectorAll("main :is(h1,h2,h3,h4,h5,h6)[id]");
  }

  // Record the last heading at or above the top of the viewport, plus how far
  // past it the reader had scrolled, so restore can re-apply the within-section
  // offset when the section itself has moved.
  function capture() {
    var found = null;
    headings().forEach(function (h) {
      if (h.getBoundingClientRect().top <= 1) {
        found = h;
      }
    });
    if (!found) {
      // Above the first heading: nothing to anchor to, so let the reload land
      // at the natural top rather than inventing a position.
      try { sessionStorage.removeItem(STORAGE_KEY); } catch (e) { /* private mode */ }
      return;
    }
    try {
      sessionStorage.setItem(STORAGE_KEY, JSON.stringify({
        path: location.pathname,
        id: found.id,
        delta: Math.round(-found.getBoundingClientRect().top)
      }));
    } catch (e) { /* private mode: reading position is not worth failing over */ }
  }

  function restore() {
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
    // Only restore onto the same document the position was captured in —
    // otherwise following a sidenav link during a reload would scroll the new
    // doc to an unrelated heading.
    if (!saved || saved.path !== location.pathname) { return; }

    // getElementById covers the case where the heading text (and therefore its
    // id) survived the edit; a heading that was renamed or deleted correctly
    // yields nothing and the page stays at the top.
    var target = document.getElementById(saved.id);
    if (!target) { return; }

    var top = target.getBoundingClientRect().top + window.scrollY + (saved.delta || 0);
    window.scrollTo({ top: top, behavior: "instant" });
  }

  function connect() {
    var source = new EventSource("/events");

    source.onmessage = function () {
      capture();
      // A full reload rather than a fetch-and-swap: it picks up sidenav changes
      // (a new doc, a retitled one, a reordered "Recent" group) with the same
      // server-rendered path the initial load uses, so there is exactly one
      // rendering code path to keep correct. On loopback the cost is
      // negligible.
      location.reload();
    };

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

  restore();
  connect();
})();
