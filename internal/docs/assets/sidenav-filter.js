// Sidenav filter box for `forgectl docs serve`.
//
// The sidenav holds two shapes: the flat Recent link list, and one
// .tree--static per root (nested details/summary). Filtering hides
// non-matching leaves in both, hides any directory left with no visible
// descendant, auto-expands directories that contain a match while a query is
// live, and restores each directory's original open state when the box
// clears. Group headings hide when their whole section is filtered out.
//
// This is a file rather than an inline <script> in shell.html.tmpl because the
// server sends a Content-Security-Policy with script-src 'self', which forbids
// inline script execution. Keeping it inline would have meant a nonce or a hash
// per response — machinery the reader does not otherwise need — so the script
// moved out instead. Anything added here must stay a served asset for the same
// reason: an inline <script> anywhere in the shell fails silently in the browser
// console, not in Go.
(function () {
  "use strict";

  var input = document.getElementById("doc-filter");
  // Inline, this script sat directly beneath the markup it drove, so the input
  // was guaranteed present. A standalone asset is loaded by any page that links
  // it and cannot assume that adjacency.
  if (!input) { return; }

  // Nodes are looked up on every keystroke, not once at load: live reload
  // (reload.js) swaps the whole sidenav in place, and a list captured at load
  // would keep filtering rows that are no longer in the page. reload.js
  // re-applies a live query by dispatching an input event after each swap.
  function all(sel) {
    return Array.prototype.slice.call(document.querySelectorAll(sel));
  }

  function matches(a, q) {
    if (q === "") { return true; }
    return (a.getAttribute("data-filter-text") || "").indexOf(q) !== -1;
  }

  input.addEventListener("input", function () {
    var q = input.value.trim().toLowerCase();
    var links = all(".sidenav a[data-filter-text]");
    var groups = all(".sidenav .sidenav__group");
    var dirs = all(".sidenav details");

    // Remember each directory's server-rendered open state (the path to the
    // current doc) the first time a query touches it, so clearing the filter
    // restores it instead of leaving the whole tree sprung open. A freshly
    // swapped-in sidenav has no record yet and gets one here.
    dirs.forEach(function (d) {
      if (d.dataset.openAtRest === undefined) { d.dataset.openAtRest = d.open ? "1" : ""; }
    });

    links.forEach(function (a) {
      var row = a.closest("li") || a;
      row.style.display = matches(a, q) ? "" : "none";
    });

    dirs.forEach(function (d) {
      var anyHit = Array.prototype.some.call(
        d.querySelectorAll("a[data-filter-text]"),
        function (a) { return matches(a, q); });
      var row = d.closest("li") || d;
      row.style.display = anyHit ? "" : "none";
      if (q === "") {
        d.open = d.dataset.openAtRest === "1";
        // Forget it once restored, so the next query records whatever the
        // reader has opened or closed by hand since.
        delete d.dataset.openAtRest;
      } else {
        d.open = anyHit;
      }
    });

    // Say so when nothing matches, rather than leaving a blank sidebar.
    var empty = document.getElementById("filter-empty");
    if (empty) {
      var anyLink = links.some(function (a) { return matches(a, q); });
      empty.hidden = anyLink;
      empty.textContent = anyLink ? "" : "No docs match \u201c" + input.value.trim() + "\u201d.";
    }

    groups.forEach(function (g) {
      var anyVisible = false;
      var sib = g.nextElementSibling;
      while (sib && !sib.classList.contains("sidenav__group")) {
        var candidates = sib.matches("a[data-filter-text]")
          ? [sib]
          : Array.prototype.slice.call(sib.querySelectorAll("a[data-filter-text]"));
        if (candidates.some(function (a) { return matches(a, q); })) {
          anyVisible = true;
          break;
        }
        sib = sib.nextElementSibling;
      }
      g.style.display = anyVisible ? "" : "none";
    });
  });
})();
