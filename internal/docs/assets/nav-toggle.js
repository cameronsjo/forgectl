// Sidenav collapse/drawer toggle for `forgectl docs serve`.
//
// The shell root carries data-nav="open|closed|auto". Wide viewports read
// "closed" as collapse-the-column; narrow viewports (≤900px, where the nav is
// an off-canvas drawer) read "open" as slide-it-in. "auto" is the resting
// state: column visible when wide, drawer closed when narrow. The scrim click
// and any viewport crossing reset to "auto" so a choice made in one mode
// never leaks into the other.
//
// A served asset, not an inline <script>: the server's CSP is script-src
// 'self', so inline script fails silently in the browser (same story as
// sidenav-filter.js).
(function () {
  "use strict";

  var root = document.querySelector(".page-shell");
  var btn = document.querySelector("[data-nav-toggle]");
  var scrim = document.querySelector("[data-nav-scrim]");
  if (!root || !btn) { return; }

  var narrow = window.matchMedia("(max-width: 900px)");

  function isOpen() {
    var s = root.getAttribute("data-nav") || "auto";
    if (s === "open") { return true; }
    if (s === "closed") { return false; }
    return !narrow.matches;
  }

  function setState(s) {
    root.setAttribute("data-nav", s);
    btn.setAttribute("aria-expanded", String(isOpen()));
  }

  btn.addEventListener("click", function () {
    setState(isOpen() ? "closed" : "open");
  });
  if (scrim) {
    scrim.addEventListener("click", function () { setState("auto"); });
  }
  narrow.addEventListener("change", function () { setState("auto"); });

  setState(root.getAttribute("data-nav") || "auto");
})();
