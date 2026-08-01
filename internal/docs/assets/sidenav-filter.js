// Sidenav filter box for `forgectl docs serve`.
//
// Hides sidenav links whose data-filter-text does not contain the query, then
// hides any group heading left with no visible links under it.
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

  var links = document.querySelectorAll(".sidenav a");
  var groups = document.querySelectorAll(".sidenav__group");
  input.addEventListener("input", function () {
    var q = input.value.trim().toLowerCase();
    links.forEach(function (a) {
      var hay = a.getAttribute("data-filter-text") || "";
      a.style.display = (q === "" || hay.indexOf(q) !== -1) ? "" : "none";
    });
    groups.forEach(function (g) {
      var sib = g.nextElementSibling;
      var anyVisible = false;
      while (sib && !sib.classList.contains("sidenav__group")) {
        if (sib.tagName === "A" && sib.style.display !== "none") { anyVisible = true; }
        sib = sib.nextElementSibling;
      }
      g.style.display = anyVisible ? "" : "none";
    });
  });
})();
