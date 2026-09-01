(function () {
  "use strict";

  var shell = document.querySelector("[data-reader-shell]");
  var drawer = document.querySelector("[data-docs-nav]");
  var toggle = document.querySelector("[data-docs-nav-toggle]");
  var filter = document.getElementById("doc-filter");

  if (!shell || !drawer || !toggle) return;

  function openDrawer() {
    shell.setAttribute("data-nav-open", "");
    toggle.setAttribute("aria-expanded", "true");
    toggle.setAttribute("aria-label", "Close document navigator");
    drawer.setAttribute("aria-hidden", "false");
    if (filter) filter.focus();
  }

  function closeDrawer(returnFocus) {
    shell.removeAttribute("data-nav-open");
    toggle.setAttribute("aria-expanded", "false");
    toggle.setAttribute("aria-label", "Open document navigator");
    drawer.setAttribute("aria-hidden", "true");
    if (returnFocus) toggle.focus();
  }

  toggle.addEventListener("click", function () {
    if (shell.hasAttribute("data-nav-open")) {
      closeDrawer(true);
    } else {
      openDrawer();
    }
  });

  document.querySelectorAll("[data-docs-nav-close], [data-docs-nav-scrim]").forEach(function (control) {
    control.addEventListener("click", function () {
      closeDrawer(true);
    });
  });

  drawer.addEventListener("click", function (event) {
    if (event.target.closest("a")) closeDrawer(false);
  });

  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape" && shell.hasAttribute("data-nav-open")) {
      closeDrawer(true);
    }
  });
})();
