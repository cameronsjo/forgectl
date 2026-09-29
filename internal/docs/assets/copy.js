// Rich copy for `forgectl docs serve` (forgectl#588, folding in the #583
// copy-tex item).
//
// Left alone, Chromium serializes a copied selection with the reader's
// computed theme styles inlined on every element, so a paste imports the dark
// theme. The DOM itself carries no style attributes to strip; the styles are
// added by the browser's serializer. So this handler serializes a DETACHED
// clone of the selection itself, where nothing is styled, and puts clean
// semantic HTML plus plain text on the clipboard.
//
// It is a `copy` event listener with clipboardData.setData rather than the
// async Clipboard API because that API needs a secure context, and an
// off-loopback bind is not one.
//
// There is exactly ONE copy listener. KaTeX's contrib/copy-tex is not
// vendored for the same reason: a second listener would fight this one over
// preventDefault and setData. Formulas are swapped for their TeX here, from
// the data-math-source math-init.js stores on each .math element.
(function () {
  "use strict";

  var XHTML = "http://www.w3.org/1999/xhtml";

  function docBody(node) {
    var el = node && node.nodeType === 1 ? node : node && node.parentElement;
    return el ? el.closest(".doc-body") : null;
  }

  // The selection's TeX when it lies wholly inside one rendered formula:
  // cloneContents() would then return bare KaTeX spans with no .math ancestor
  // left to substitute.
  function enclosingMath(range) {
    var n = range.commonAncestorContainer;
    var el = n.nodeType === 1 ? n : n.parentElement;
    return el ? el.closest(".math") : null;
  }

  // Swap each rendered formula for its TeX source, delimiters kept. The tag is
  // kept (a display block stays a block) and everything else on it goes. A
  // .math without a stored source was never rendered, so its text already is
  // the TeX and it is left alone.
  function substituteMath(root) {
    root.querySelectorAll(".math").forEach(function (el) {
      var src = el.getAttribute("data-math-source");
      if (src === null) { return; }
      var plain = document.createElement(el.localName);
      plain.textContent = src;
      el.replaceWith(plain);
    });
  }

  // Reader-only nodes: the diagram frame's reset bar, and the pan/zoom
  // viewport and stage (unwrapped, leaving the diagram itself).
  function dropChrome(root) {
    root.querySelectorAll(".embed-bar").forEach(function (el) { el.remove(); });
    root.querySelectorAll(".dia-viewport, .dia-stage").forEach(function (el) {
      el.replaceWith.apply(el, Array.prototype.slice.call(el.childNodes));
    });
  }

  // Strip what only the reader's own CSS and scripts use, then unwrap the
  // attribute-less <span>s that leaves behind (syntax-highlight tokens).
  function stripAttributes(root) {
    var drop = ["class", "id", "style", "tabindex", "role", "aria-label", "aria-hidden"];
    root.querySelectorAll("*").forEach(function (el) {
      drop.forEach(function (a) { el.removeAttribute(a); });
      Array.prototype.slice.call(el.attributes).forEach(function (attr) {
        if (attr.name.indexOf("data-") === 0) { el.removeAttribute(attr.name); }
      });
    });
    var spans = Array.prototype.slice.call(root.querySelectorAll("span")).filter(function (el) {
      return el.namespaceURI === XHTML && el.attributes.length === 0;
    });
    // Innermost first, so an unwrapped child is still in the tree when its
    // parent goes.
    spans.reverse().forEach(function (el) {
      el.replaceWith.apply(el, Array.prototype.slice.call(el.childNodes));
    });
  }

  // Plain text comes from the clone too, so it carries TeX rather than the
  // doubled MathML+HTML text Selection.toString() yields for a formula. The
  // clone is detached, and innerText needs layout, so it is measured in an
  // off-screen holder for the length of the call.
  function plainText(clone) {
    var holder = document.createElement("div");
    holder.style.cssText = "position:fixed;left:-99999px;top:0;width:800px;white-space:normal";
    holder.appendChild(clone.cloneNode(true));
    document.body.appendChild(holder);
    try {
      return holder.innerText;
    } finally {
      holder.remove();
    }
  }

  function onCopy(e) {
    var sel = window.getSelection();
    if (!e.clipboardData || !sel || sel.isCollapsed || sel.rangeCount === 0) { return; }

    // Only selections wholly inside the document body. Anything else (the
    // sidenav, the filter box) copies natively.
    var ranges = [];
    for (var i = 0; i < sel.rangeCount; i++) {
      var r = sel.getRangeAt(i);
      if (!docBody(r.startContainer) || !docBody(r.endContainer)) { return; }
      ranges.push(r);
    }

    var clone = document.createElement("div");
    ranges.forEach(function (r) {
      var math = enclosingMath(r);
      if (math && math.getAttribute("data-math-source") !== null) {
        var src = document.createElement("span");
        src.textContent = math.getAttribute("data-math-source");
        clone.appendChild(src);
      } else {
        clone.appendChild(r.cloneContents());
      }
    });

    substituteMath(clone);
    dropChrome(clone);
    stripAttributes(clone);

    e.clipboardData.setData("text/html", clone.innerHTML);
    e.clipboardData.setData("text/plain", plainText(clone));
    e.preventDefault();
  }

  document.addEventListener("copy", onCopy);
})();
