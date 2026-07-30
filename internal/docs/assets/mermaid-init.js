// Mermaid initialization for `forgectl docs serve`.
//
// Colors are NOT picked here. Mermaid is initialized with theme:'base' and
// themeVariables read from the Artificer --dia-* custom properties, which is the
// override surface artificer.css documents for exactly this purpose. The
// consequence that matters: the reader's diagrams change with the theme because
// they are reading the same tokens the rest of the page reads, so there is no
// second palette to keep in sync with the first.
//
// Mermaid bakes resolved colors into the SVG it generates, so a theme flip cannot
// restyle an already-rendered diagram through CSS alone — the diagrams have to be
// re-rendered. That is what the observer at the bottom is for.
(function () {
  "use strict";

  if (typeof mermaid === "undefined") {
    // The bundle failed to load. Diagram sources stay visible as preformatted
    // text, which is a legible degradation, so this is a console note and not an
    // on-page error.
    console.warn("[forgectl docs] mermaid bundle unavailable; diagrams will render as source text");
    return;
  }

  function token(name, fallback) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name);
    v = (v || "").trim();
    return v === "" ? fallback : v;
  }

  // Read the diagram tokens artificer.css defines. The fallbacks are only
  // reached if the stylesheet failed to load, in which case mermaid's own base
  // theme is a reasonable floor.
  function themeVariables() {
    var nodeBg = token("--dia-node-bg", "#292c33");
    var nodeFg = token("--dia-node-fg", "#e6e6e6");
    var nodeBorder = token("--dia-node-border", "#3c4150");
    var edge = token("--dia-edge", "#c5c8c6");
    var edgeStrong = token("--dia-edge-strong", "#ffffff");
    var surface = token("--bg", nodeBg);
    var accent = token("--accent", edgeStrong);
    var mono = token("--font-mono", "ui-monospace, monospace");

    return {
      darkMode: (document.documentElement.getAttribute("data-theme") || "dark") === "dark",
      background: surface,
      fontFamily: mono,

      primaryColor: nodeBg,
      primaryTextColor: nodeFg,
      primaryBorderColor: nodeBorder,
      secondaryColor: nodeBg,
      secondaryTextColor: nodeFg,
      secondaryBorderColor: nodeBorder,
      tertiaryColor: surface,
      tertiaryTextColor: nodeFg,
      tertiaryBorderColor: nodeBorder,

      mainBkg: nodeBg,
      nodeBorder: nodeBorder,
      nodeTextColor: nodeFg,
      clusterBkg: surface,
      clusterBorder: nodeBorder,
      titleColor: nodeFg,

      lineColor: edge,
      textColor: nodeFg,
      edgeLabelBackground: surface,

      // One accent per diagram is the Artificer diagram rule; mermaid's notion
      // of "the highlighted thing" is these.
      activeTaskBkgColor: accent,
      activeTaskBorderColor: accent
    };
  }

  function config() {
    return {
      startOnLoad: false,
      theme: "base",
      themeVariables: themeVariables(),
      // The reader renders documents the operator wrote or Claude wrote for
      // them; it is not a hosted service accepting diagrams from strangers.
      // 'strict' keeps mermaid from honoring click-directives and inline HTML in
      // labels, which is the posture that matches the sanitizer upstream of it.
      securityLevel: "strict",
      flowchart: { useMaxWidth: false, htmlLabels: false, curve: "basis" },
      sequence: { useMaxWidth: false },
      gantt: { useMaxWidth: false }
    };
  }

  // useMaxWidth:false above is deliberate and pairs with svg-panzoom.js: with
  // useMaxWidth:true mermaid writes a percentage width that fights a transform,
  // so a zoomed diagram jitters as it rescales.

  function render() {
    mermaid.initialize(config());
    var blocks = document.querySelectorAll("pre.mermaid");
    if (!blocks.length) { return; }
    // mermaid.run replaces each element's content with rendered SVG. Passing the
    // node list explicitly (rather than letting it scan) keeps it off anything
    // else on the page.
    mermaid.run({ nodes: blocks }).catch(function (err) {
      console.warn("[forgectl docs] mermaid render failed", err);
    });
  }

  // Re-render on theme change. artificer-theme.js flips data-theme on <html>,
  // so observing that attribute is the seam — no coordination with, or patch to,
  // the vendored theme script.
  function watchTheme() {
    var last = document.documentElement.getAttribute("data-theme");
    new MutationObserver(function () {
      var now = document.documentElement.getAttribute("data-theme");
      if (now === last) { return; }
      last = now;
      // Diagram sources are gone from the DOM after the first render (replaced
      // by SVG), so a re-render needs the original text back. Stash it on first
      // render and restore before re-running.
      document.querySelectorAll("pre.mermaid").forEach(function (el) {
        if (el.dataset.mermaidSource) {
          el.textContent = el.dataset.mermaidSource;
          el.removeAttribute("data-processed");
        }
      });
      render();
    }).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  }

  function stashSources() {
    document.querySelectorAll("pre.mermaid").forEach(function (el) {
      if (!el.dataset.mermaidSource) {
        el.dataset.mermaidSource = el.textContent;
      }
    });
  }

  stashSources();
  render();
  watchTheme();
})();
