/* nav-toggle.js — sidebar drawer/collapse toggle. CSP: served asset, no inline.
   Contract: button#nav-toggle sets data-nav on the shell root (#shell):
   wide  (>900px): auto <-> closed  (collapses the nav column)
   narrow(≤900px): auto <-> open    (off-canvas drawer)
   Scrim click and any viewport crossing reset to auto, so a choice made in
   one mode never leaks into the other. Ported from the design reference. */
(function () {
  'use strict';
  var shell = document.getElementById('shell');
  var btn = document.getElementById('nav-toggle');
  var scrim = document.getElementById('drawer-scrim');
  if (!shell || !btn) return;
  var mq = window.matchMedia('(max-width: 900px)');
  // The system convention (artificer.css): an expand/collapse trigger keeps
  // aria-expanded current. Expanded = the nav is visible in the current mode.
  function syncExpanded() {
    var cur = shell.getAttribute('data-nav') || 'auto';
    var open = cur === 'open' || (cur === 'auto' && !mq.matches);
    btn.setAttribute('aria-expanded', String(open));
  }
  btn.addEventListener('click', function () {
    var cur = shell.getAttribute('data-nav') || 'auto';
    if (mq.matches) {
      shell.setAttribute('data-nav', cur === 'open' ? 'auto' : 'open');
    } else {
      shell.setAttribute('data-nav', cur === 'closed' ? 'auto' : 'closed');
    }
    syncExpanded();
  });
  if (scrim) scrim.addEventListener('click', function () {
    shell.setAttribute('data-nav', 'auto');
    syncExpanded();
  });
  mq.addEventListener('change', function () {
    shell.setAttribute('data-nav', 'auto');
    syncExpanded();
  });
  syncExpanded();
})();
