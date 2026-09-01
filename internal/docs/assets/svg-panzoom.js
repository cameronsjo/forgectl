// Pan/zoom for diagrams in `forgectl docs serve`.
//
// Applies to both kinds of diagram the reader can show: inline <svg> authored in
// the markdown, and the SVG mermaid generates client-side. It attaches to the
// SVG's parent wrapper and drives a CSS transform, rather than rewriting the
// viewBox.
//
// Why transform and not viewBox: rewriting viewBox re-runs SVG layout on every
// wheel tick and, for mermaid output, fights the sizing mermaid computed for
// itself. A transform is composited, so panning stays smooth on a large diagram,
// and — the part that matters for correctness — it leaves the document's own
// markup untouched, so a live-reload swap does not have to preserve any state we
// scribbled into the SVG.
(function () {
  "use strict";

  var MIN_SCALE = 0.2;
  var MAX_SCALE = 12;
  var WHEEL_SENSITIVITY = 0.0015;

  function clamp(v, lo, hi) { return v < lo ? lo : (v > hi ? hi : v); }

  // Wrap the SVG so the transform has somewhere to live that is not the SVG
  // itself — mermaid re-renders into its own element and would blow away a
  // transform written onto its output.
  function wrap(svg) {
    if (svg.parentElement && svg.parentElement.classList.contains("dia-viewport")) {
      return svg.parentElement;
    }
    var viewport = document.createElement("div");
    viewport.className = "dia-viewport";
    var stage = document.createElement("div");
    stage.className = "dia-stage";

    svg.parentNode.insertBefore(viewport, svg);
    viewport.appendChild(stage);
    stage.appendChild(svg);
    return viewport;
  }

  function enhance(svg) {
    if (svg.dataset.panzoom === "on") { return; }
    svg.dataset.panzoom = "on";

    var viewport = wrap(svg);
    var stage = viewport.querySelector(".dia-stage");

    var scale = 1, tx = 0, ty = 0;
    var dragging = false, lastX = 0, lastY = 0;

    function apply() {
      stage.style.transform = "translate(" + tx + "px," + ty + "px) scale(" + scale + ")";
    }

    function reset() {
      scale = 1; tx = 0; ty = 0;
      apply();
    }

    // Zoom toward the cursor: convert the pointer to stage coordinates, scale,
    // then correct the translation so the point under the cursor stays put.
    // Without this correction the diagram slides away from wherever you aimed.
    viewport.addEventListener("wheel", function (e) {
      if (!e.ctrlKey && !e.metaKey && !viewport.dataset.zoomArmed) {
        // Bare wheel scrolls the PAGE. Hijacking it would trap the reader's
        // scroll whenever the cursor crossed a diagram, which is the single most
        // irritating thing a pan/zoom widget can do. Zoom needs a modifier, or
        // a click into the diagram first (see zoomArmed below).
        return;
      }
      e.preventDefault();

      var rect = viewport.getBoundingClientRect();
      var px = e.clientX - rect.left;
      var py = e.clientY - rect.top;

      var next = clamp(scale * Math.exp(-e.deltaY * WHEEL_SENSITIVITY), MIN_SCALE, MAX_SCALE);
      var ratio = next / scale;

      tx = px - ratio * (px - tx);
      ty = py - ratio * (py - ty);
      scale = next;
      apply();
    }, { passive: false });

    // Clicking a diagram arms bare-wheel zoom for that diagram; clicking away
    // disarms it. This keeps page scrolling intact by default while making
    // sustained zooming comfortable once the reader is clearly working inside a
    // diagram.
    viewport.addEventListener("pointerdown", function (e) {
      viewport.dataset.zoomArmed = "1";
      dragging = true;
      lastX = e.clientX;
      lastY = e.clientY;
      viewport.setPointerCapture(e.pointerId);
      viewport.classList.add("is-panning");
    });
    document.addEventListener("pointerdown", function (e) {
      if (!viewport.contains(e.target)) { delete viewport.dataset.zoomArmed; }
    }, true);

    viewport.addEventListener("pointermove", function (e) {
      if (!dragging) { return; }
      tx += e.clientX - lastX;
      ty += e.clientY - lastY;
      lastX = e.clientX;
      lastY = e.clientY;
      apply();
    });

    function endDrag() {
      dragging = false;
      viewport.classList.remove("is-panning");
    }
    viewport.addEventListener("pointerup", endDrag);
    viewport.addEventListener("pointercancel", endDrag);

    // Double-click restores the original framing — the escape hatch for having
    // zoomed into nowhere.
    viewport.addEventListener("dblclick", function (e) {
      e.preventDefault();
      reset();
    });

    // Keyboard parity, so a diagram is not mouse-only.
    viewport.tabIndex = 0;
    viewport.setAttribute("role", "group");
    viewport.setAttribute("aria-label", "Diagram — arrow keys pan, plus and minus zoom, 0 resets");
    viewport.addEventListener("keydown", function (e) {
      var step = e.shiftKey ? 60 : 20;
      switch (e.key) {
        case "ArrowLeft":  tx += step; break;
        case "ArrowRight": tx -= step; break;
        case "ArrowUp":    ty += step; break;
        case "ArrowDown":  ty -= step; break;
        case "+": case "=": scale = clamp(scale * 1.2, MIN_SCALE, MAX_SCALE); break;
        case "-": case "_": scale = clamp(scale / 1.2, MIN_SCALE, MAX_SCALE); break;
        case "0":          reset(); return;
        default: return;
      }
      e.preventDefault();
      apply();
    });

    apply();
  }

  function enhanceAll() {
    // Inline SVG authored in the doc, plus whatever mermaid has rendered by
    // now. aria-hidden svgs are excluded: those are the reader's own chrome
    // (property-block and callout icons), and wrapping an 11px icon in a
    // pan/zoom viewport renders it as a giant bordered capsule.
    document.querySelectorAll('main svg:not([aria-hidden="true"])').forEach(enhance);
  }

  // Mermaid renders asynchronously and replaces pre.mermaid contents, so the
  // SVGs it produces do not exist at DOMContentLoaded. Watch <main> for added
  // SVG rather than guessing at a delay.
  function watchForRenderedDiagrams() {
    var main = document.querySelector("main");
    if (!main) { return; }
    new MutationObserver(function () { enhanceAll(); })
      .observe(main, { childList: true, subtree: true });
  }

  enhanceAll();
  watchForRenderedDiagrams();
})();
