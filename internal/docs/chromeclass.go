package docs

import (
	"regexp"
	"strings"
)

// chromeClassFamilies names the class families a document may not wear
// (forgectl#700). The sanitizer lets "class" through because goldmark and
// chroma generate classes (render.go, AllowStyling), so a doc could also
// write <div class="statusbar"> and get the reader's own chrome styling
// inside the doc pane. Worse, Artificer's overlay primitives are
// position:fixed: a doc's <div class="scrim"> covered the whole reader at
// z-index 2000. Neither is an XSS, but both let a doc draw something the
// reader would take for the reader.
//
// A family is a BEM block. It matches the bare block name and any token
// that extends it with "-" or "_" (statusbar, sidenav__group,
// outline-item, trust-badge--stale). Two tests keep the list honest:
// every class the shell template uses must be denied here or named as
// shared content vocabulary, and every class in a position:fixed or
// position:sticky rule of a served stylesheet must be denied. mermaid-init.js
// carries a copy for rendered diagrams, which never pass through this strip;
// TestChromeClasses_MermaidInitMirrorsGoList keeps the copy in step.
var chromeClassFamilies = []string{
	// The reader shell (templates/shell.html.tmpl).
	"appbar", "content-grid", "doc-body", "docs-nav", "empty-state", "home",
	"live-dot", "live-status", "nav-toggle", "outline", "sidenav",
	"skip-link", "status-item", "status-spacer", "statusbar",
	"surface-document", "surface-tool", "theme-toggle", "wordmark",
	// Generated after the sanitizer (frontmatterHTML), so a doc never
	// needs them and a fake one reads as the reader's verdict on the doc.
	"trust-badge", "status-chip",
	// Artificer's fixed and sticky overlays and page frames.
	"app-shell", "nav-drawer", "nav-scrim", "page-shell", "palette", "scrim",
	"toast-region",
}

// isChromeClass reports whether one class token belongs to a chrome family.
func isChromeClass(token string) bool {
	for _, f := range chromeClassFamilies {
		if token == f {
			return true
		}
		if strings.HasPrefix(token, f) && len(token) > len(f) {
			if c := token[len(f)]; c == '-' || c == '_' {
				return true
			}
		}
	}
	return false
}

// classAttr matches a class attribute in bluemonday's output. bluemonday
// writes every attribute as name="value" with the value HTML-escaped (a
// quote becomes &#34;) and escapes quotes in text the same way, so
// class=" can only open a real class attribute, and the value ends at the
// next quote.
var classAttr = regexp.MustCompile(`\sclass="([^"]*)"`)

// stripChromeClasses removes chrome class tokens from every class attribute
// of sanitized HTML, keeping the other tokens, and drops an attribute left
// empty. It runs on the sanitizer's output, never before it.
func stripChromeClasses(sanitized string) string {
	if !strings.Contains(sanitized, `class="`) {
		return sanitized
	}
	return classAttr.ReplaceAllStringFunc(sanitized, func(attr string) string {
		value := classAttr.FindStringSubmatch(attr)[1]
		tokens := strings.Fields(value)
		kept := tokens[:0]
		for _, t := range tokens {
			if !isChromeClass(t) {
				kept = append(kept, t)
			}
		}
		if len(kept) == len(tokens) {
			return attr
		}
		if len(kept) == 0 {
			return ""
		}
		return attr[:1] + `class="` + strings.Join(kept, " ") + `"`
	})
}
