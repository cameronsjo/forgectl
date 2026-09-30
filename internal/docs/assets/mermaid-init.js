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

  // A doc can carry <div id="ForgectlMermaid">, and the sanitizer keeps ids,
  // so until this script assigns the global, window.ForgectlMermaid is that
  // element (forgectl#759). Assign an inert stub before the early return
  // below; the real hooks replace it once the diagrams are set up.
  window.ForgectlMermaid = {
    refresh: function () { return null; },
    focusKey: function () { return null; },
    restoreFocus: function () {}
  };

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

  // A doc can reconfigure its own diagram with a %%{init: {...}}%% directive
  // or a frontmatter config: block, and mermaid merges either over the config
  // below (forgectl#713). Measured on 11.12.3, that let a doc turn on
  // themeCSS (raw CSS in the diagram's <style>, url() fetches included),
  // htmlLabels, flowchart.htmlLabels, fontFamily, theme and themeVariables.
  //
  // mermaid already deletes any directive key that is not a key somewhere in
  // its defaultConfig (sanitizeDirective's keyify set), and then any key named
  // in `secure`, which it checks at the top level only. So `secure` here is
  // that same keyify set, every key at every depth, minus DOC_SETTABLE. A
  // top-level key that survives the first filter is then caught by the
  // second, so a doc can set nothing but DOC_SETTABLE, and a key a future
  // mermaid adds is pinned the day it is vendored. NO_DEFAULT_KEYS covers
  // schema keys with no default, in case a re-vendor drops one from the set.
  // mermaid's own six secure keys come first: initialize merges this list
  // over its default one, and the default can never be narrowed.
  var DOC_SETTABLE = { wrap: true };
  var NO_DEFAULT_KEYS = ["htmlLabels", "dompurifyConfig", "altFontFamily"];

  function keyify(obj, out) {
    Object.keys(obj).forEach(function (k) {
      out.push(k);
      var v = obj[k];
      if (v !== null && typeof v === "object" && !Array.isArray(v)) { keyify(v, out); }
    });
    return out;
  }

  // MIN_DEFAULT_KEYS guards the derivation. 11.12.3's defaultConfig has 45
  // top-level keys; a re-vendor that renames or drops it would otherwise
  // hand `secure` only the six fixed names and silently reopen #713.
  var MIN_DEFAULT_KEYS = 30;

  // pinnedKeys returns null when mermaid's defaultConfig is missing or
  // thinner than MIN_DEFAULT_KEYS. render() then leaves every diagram as
  // source text: failing closed, not rendering with a doc-settable config.
  function pinnedKeys() {
    var defaults = mermaid.mermaidAPI && mermaid.mermaidAPI.defaultConfig;
    if (!defaults || typeof defaults !== "object" || Object.keys(defaults).length < MIN_DEFAULT_KEYS) {
      return null;
    }
    var keys = ["secure", "securityLevel", "startOnLoad", "maxTextSize", "suppressErrorRendering", "maxEdges"]
      .concat(keyify(defaults, []), NO_DEFAULT_KEYS);
    var seen = Object.create(null);
    return keys.filter(function (k) {
      if (seen[k] === true || DOC_SETTABLE[k] === true) { return false; }
      seen[k] = true;
      return true;
    });
  }

  function config(secure) {
    return {
      secure: secure,
      startOnLoad: false,
      theme: "base",
      themeVariables: themeVariables(),
      // The reader renders documents the operator wrote or Claude wrote for
      // them; it is not a hosted service accepting diagrams from strangers.
      // 'strict' keeps mermaid from honoring click-directives, which is the
      // posture that matches the sanitizer upstream of it.
      securityLevel: "strict",
      // Labels as SVG text, not live HTML in a foreignObject. In 11.12.3 the
      // top-level key is the one that decides: with only flowchart.htmlLabels
      // false, flowchart and classDiagram labels still rendered author
      // markup (a <details> stayed a live <details>). Markdown-string bold
      // and italics still render as SVG text styling.
      htmlLabels: false,
      flowchart: { useMaxWidth: false, htmlLabels: false, curve: "basis" },
      sequence: { useMaxWidth: false },
      gantt: { useMaxWidth: false }
    };
  }

  // useMaxWidth:false above is deliberate and pairs with svg-panzoom.js: with
  // useMaxWidth:true mermaid writes a percentage width that fights a transform,
  // so a zoomed diagram jitters as it rescales.

  // The reader's scripts find chrome by data-fc (forgectl#617, #643), which
  // holds only because a doc cannot plant one. The server's sanitizer strips
  // every data-* attribute, but mermaid renders in the browser AFTER it, and
  // mermaid's own DOMPurify pass keeps data-*: with HTML labels on, a
  // flowchart, classDiagram, state, mindmap or kanban label can emit
  // <i data-fc="outline">. The htmlLabels:false pin above closes that on
  // 11.12.3; this scrub stays as the second wall, for a diagram type or a
  // future mermaid that puts author markup in the SVG some other way. It
  // covers every diagram. mermaid.run renders into a temporary #dmermaid-N
  // container, but creates it inside the target pre.mermaid (measured on
  // 11.12.3), so this selector covers that too.
  var FORGED_HOOKS = "pre.mermaid [data-fc]";

  // The same pass strips the reader's chrome class families (forgectl#745).
  // A diagram's `A:::scrim` or `class A statusbar` puts the author's class on
  // an SVG <g>, and mermaid's output never meets the server's class strip
  // (chromeclass.go). position is inert on SVG, and with htmlLabels off there
  // is no HTML label to style, so no live spoof was measured; this keeps it
  // that way if either changes. The list mirrors chromeClassFamilies;
  // TestChromeClasses_MermaidInitMirrorsGoList keeps the two in step.
  var CHROME_CLASS_FAMILIES = [
    "appbar", "content-grid", "doc-body", "docs-nav", "empty-state", "home",
    "live-dot", "live-status", "nav-toggle", "outline", "sidenav",
    "skip-link", "status-item", "status-spacer", "statusbar",
    "surface-document", "surface-tool", "theme-toggle", "wordmark",
    "trust-badge", "status-chip",
    "app-shell", "nav-drawer", "nav-scrim", "page-shell", "palette", "scrim",
    "toast-region"
  ];

  function isChromeClass(token) {
    return CHROME_CLASS_FAMILIES.some(function (f) {
      if (token === f) { return true; }
      var c = token.charAt(f.length);
      return token.length > f.length && token.lastIndexOf(f, 0) === 0 && (c === "-" || c === "_");
    });
  }

  function scrubHooks() {
    document.querySelectorAll(FORGED_HOOKS).forEach(function (el) {
      el.removeAttribute("data-fc");
    });
    document.querySelectorAll("pre.mermaid [class]").forEach(function (el) {
      var chrome = Array.prototype.filter.call(el.classList, isChromeClass);
      if (chrome.length) { el.classList.remove.apply(el.classList, chrome); }
    });
  }

  // Scrubbing only when mermaid.run resolves would leave the forged hooks up
  // for the whole async render, and a filter keystroke or live-reload swap in
  // that window would find them. A MutationObserver callback runs as a
  // microtask straight after the insertion, before any event or network task
  // can reach the reader's scripts. It watches <body>, not the doc pane, so
  // the diagrams a live-reload swap brings in are covered without re-arming
  // it. scripts/verify-reader-chrome.mjs pins the timing: a hook planted
  // inside a diagram must be gone at the next task boundary.
  function watchForForgedHooks() {
    new MutationObserver(scrubHooks).observe(document.body, {
      childList: true, subtree: true, attributes: true, attributeFilter: ["data-fc"]
    });
  }

  function render() {
    var blocks = document.querySelectorAll("pre.mermaid");
    var secure = pinnedKeys();
    if (!secure) {
      console.error("[forgectl docs] mermaid defaultConfig missing or incomplete; diagrams left as source so a doc cannot reconfigure mermaid (forgectl#713)");
      return Promise.resolve();
    }
    mermaid.initialize(config(secure));
    if (!blocks.length) { return Promise.resolve(); }
    // mermaid.run replaces each element's content with rendered SVG. Passing the
    // node list explicitly (rather than letting it scan) keeps it off anything
    // else on the page. The promise settles once the scrub has run, which is
    // when reload.js puts focus back into a re-rendered diagram.
    return mermaid.run({ nodes: blocks }).then(scrubHooks, function (err) {
      scrubHooks();
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
      // The re-render replaces each diagram's SVG and its pan/zoom viewport,
      // which drops keyboard focus held there (forgectl#745).
      var focus = diagramFocusKey(document.activeElement);
      // Diagram sources are gone from the DOM after the first render (replaced
      // by SVG), so a re-render needs the original text back. Stash it on first
      // render and restore before re-running.
      document.querySelectorAll("pre.mermaid").forEach(function (el) {
        if (el.dataset.mermaidSource) {
          el.textContent = el.dataset.mermaidSource;
          el.removeAttribute("data-processed");
        }
      });
      render().then(function () { restoreDiagramFocus(focus); });
    }).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  }

  // Focus inside a diagram (its reset button, its pan/zoom viewport, or a
  // link in the SVG) cannot survive a re-render: the viewport and links exist
  // only once mermaid has rendered the diagram again (forgectl#718, #745).
  // The key names the diagram by its index among the doc's diagrams, plus
  // the part of it that held focus. A live-reload swap (reload.js) and a
  // theme flip (watchTheme) both take a key before and restore after.
  function diagramFocusKey(el) {
    if (!el || el === document.body || !el.closest) { return null; }
    var embed = el.closest(".embed");
    var body = document.querySelector('[data-fc="doc-body"]');
    if (!embed || !body || !body.contains(embed)) { return null; }
    var diagrams = Array.prototype.slice.call(body.querySelectorAll("pre.mermaid"));
    var index = diagrams.indexOf(embed.querySelector("pre.mermaid"));
    if (index < 0) { return null; }
    var key = { region: "doc-body", diagram: index };
    var href = el.getAttribute("href") || el.getAttribute("xlink:href");
    if (el.classList.contains("embed-reset")) { key.part = "reset"; }
    else if (el.classList.contains("dia-viewport")) { key.part = "viewport"; }
    else if (href) { key.part = "link"; key.link = href; }
    else { return null; }
    return key;
  }

  // Best effort: if the reader has put focus somewhere else while the
  // diagram re-rendered, that choice stands.
  function restoreDiagramFocus(key) {
    if (!key) { return; }
    var active = document.activeElement;
    if (active && active !== document.body) { return; }
    var body = document.querySelector('[data-fc="doc-body"]');
    var pre = body ? body.querySelectorAll("pre.mermaid")[key.diagram] : null;
    var embed = pre ? pre.closest(".embed") : null;
    if (!embed) { return; }
    var el = null;
    if (key.part === "reset") {
      el = embed.querySelector(".embed-reset");
    } else if (key.part === "viewport") {
      el = embed.querySelector(".dia-viewport");
    } else {
      Array.prototype.some.call(pre.querySelectorAll("a"), function (a) {
        if ((a.getAttribute("href") || a.getAttribute("xlink:href")) === key.link) { el = a; return true; }
        return false;
      });
    }
    if (el) { el.focus({ preventScroll: true }); }
  }

  function stashSources() {
    document.querySelectorAll("pre.mermaid").forEach(function (el) {
      if (!el.dataset.mermaidSource) {
        el.dataset.mermaidSource = el.textContent;
      }
    });
  }

  // Wrap each diagram in the .embed card (docs-reader-v2): a labeled header
  // bar with a reset control, and a centered body. The reset button drives
  // svg-panzoom through its own public gesture — a dblclick on the
  // .dia-viewport it creates — so there is no second transform authority.
  function wrapEmbeds() {
    document.querySelectorAll("pre.mermaid").forEach(function (el) {
      if (el.closest(".embed")) { return; }
      var embed = document.createElement("div");
      embed.className = "embed";
      var bar = document.createElement("div");
      bar.className = "embed-bar";
      var label = document.createElement("span");
      label.textContent = "mermaid";
      var spacer = document.createElement("span");
      spacer.className = "spacer";
      var reset = document.createElement("button");
      reset.type = "button";
      reset.className = "embed-reset";
      reset.textContent = "reset";
      reset.setAttribute("aria-label", "Reset diagram view");
      reset.addEventListener("click", function () {
        var viewport = embed.querySelector(".dia-viewport");
        if (viewport) {
          viewport.dispatchEvent(new MouseEvent("dblclick", { bubbles: true }));
        }
      });
      bar.appendChild(label);
      bar.appendChild(spacer);
      bar.appendChild(reset);
      var body = document.createElement("div");
      body.className = "embed-body";
      el.parentNode.insertBefore(embed, el);
      embed.appendChild(bar);
      embed.appendChild(body);
      body.appendChild(el);
    });
  }

  watchForForgedHooks();
  stashSources();
  wrapEmbeds();
  render();
  watchTheme();

  // Live reload (reload.js) swaps the document body in place, so the diagrams
  // it brings in are new, unrendered pre.mermaid blocks. refresh runs the same
  // first-render path over them; already-rendered diagrams elsewhere are gone
  // with the old body. svg-panzoom.js needs no hook: it watches <main>.
  // refresh returns a promise that settles when the render has; focusKey and
  // restoreFocus let reload.js carry diagram focus across it.
  window.ForgectlMermaid = {
    refresh: function () {
      stashSources();
      wrapEmbeds();
      return render();
    },
    focusKey: diagramFocusKey,
    restoreFocus: restoreDiagramFocus
  };
})();
