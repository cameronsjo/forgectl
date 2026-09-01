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

  var links = Array.prototype.slice.call(
    document.querySelectorAll(".sidenav a[data-filter-text]"));
  var groups = Array.prototype.slice.call(
    document.querySelectorAll(".sidenav .sidenav__group"));
  var dirs = Array.prototype.slice.call(
    document.querySelectorAll(".sidenav details"));

  // Remember each directory's server-rendered open state (the path to the
  // current doc) so clearing the filter restores it instead of leaving the
  // whole tree sprung open.
  dirs.forEach(function (d) { d.dataset.openAtRest = d.open ? "1" : ""; });

  function matches(a, q) {
    if (q === "") { return true; }
    return (a.getAttribute("data-filter-text") || "").indexOf(q) !== -1;
  }

  input.addEventListener("input", function () {
    var q = input.value.trim().toLowerCase();

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
      d.open = q === "" ? d.dataset.openAtRest === "1" : anyHit;
    });

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
