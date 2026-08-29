package docs

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	mermaid "github.com/zkrebbekx/go-mermaid"
	"github.com/zkrebbekx/go-mermaid/raster"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const (
	maxLocalImageBytes = 32 << 20
	maxDecodedPixels   = 64 << 20
	maxDiagramBytes    = 1 << 20
)

type TerminalLink struct {
	Text   string
	Target string
}

type TerminalPage struct {
	Content  string
	Graphics string
	Links    []TerminalLink
	ImageIDs []uint32
}

type terminalSegment struct {
	kind   string
	source string
	target string
	alt    string
}

var (
	standaloneImageRE = regexp.MustCompile(`^\s*!\[([^]]*)\]\(([^) ]+)(?:\s+"[^"]*")?\)\s*$`)
	linkRE            = regexp.MustCompile(`(^|[^!])\[([^]]+)\]\(([^) ]+)(?:\s+"[^"]*")?\)`)
)

// RenderTerminal renders a document for a cell-based viewport. Block images,
// SVG, and Mermaid are kept as distinct segments so Kitty placeholders remain
// attached to the document rows Bubble Tea scrolls.
func RenderTerminal(source []byte, doc Doc, root Root, width int, graphics bool) (TerminalPage, error) {
	if width < 20 {
		width = 20
	}
	safeSource := safeMarkdownSource(string(source))
	segments := splitTerminalSegments(safeSource)
	page := TerminalPage{Links: terminalLinks(safeSource)}
	var rendered strings.Builder

	for _, segment := range segments {
		if segment.kind == "text" {
			text, err := renderTerminalMarkdown(segment.source, width)
			if err != nil {
				return TerminalPage{}, err
			}
			rendered.WriteString(text)
			continue
		}

		fallback := mediaFallback(segment)
		if !graphics {
			rendered.WriteString(fallback)
			continue
		}
		img, err := renderTerminalMedia(segment, doc, root)
		if err != nil {
			rendered.WriteString(fallbackWithError(fallback, err))
			continue
		}
		transmission, placeholders, id, err := KittyImageBlock(img, width-4)
		if err != nil {
			rendered.WriteString(fallbackWithError(fallback, err))
			continue
		}
		rendered.WriteString("\n")
		page.Graphics += transmission
		rendered.WriteString(placeholders)
		rendered.WriteString("\n\n")
		page.ImageIDs = append(page.ImageIDs, id)
	}
	page.Content = rendered.String()
	return page, nil
}

// safeMarkdownSource preserves Markdown's structural newlines and tabs while
// visibly quoting terminal controls and bidi formatting. Glamour intentionally
// emits ANSI of its own, so sanitization must happen before rendering rather
// than stripping the completed output.
func safeMarkdownSource(value string) string {
	var out strings.Builder
	for _, r := range value {
		if r == '\n' || r == '\t' || (!termsafe.IsUnsafeTerminalRune(r) && (unicode.IsGraphic(r) || r == ' ')) {
			out.WriteRune(r)
			continue
		}
		out.WriteString(termsafe.SafeLine(string(r)))
	}
	return out.String()
}

func renderTerminalMarkdown(source string, width int) (string, error) {
	style := styles.DarkStyleConfig
	style.Document.Color = stringPtr("#c5c8c6")
	style.Heading.Color = stringPtr("#B0B9F9")
	style.H1.Color = stringPtr("#f0c674")
	style.H1.BackgroundColor = nil
	style.Link.Color = stringPtr("#8abeb7")
	style.LinkText.Color = stringPtr("#8abeb7")
	style.Code.Color = stringPtr("#b5bd68")
	style.BlockQuote.Color = stringPtr("#8abeb7")

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return "", fmt.Errorf("terminal markdown renderer: %w", err)
	}
	out, err := renderer.Render(source)
	if err != nil {
		return "", fmt.Errorf("render terminal markdown: %w", err)
	}
	return out, nil
}

func stringPtr(value string) *string { return &value }

func splitTerminalSegments(source string) []terminalSegment {
	lines := strings.SplitAfter(source, "\n")
	var segments []terminalSegment
	var text strings.Builder
	flush := func() {
		if text.Len() == 0 {
			return
		}
		segments = append(segments, terminalSegment{kind: "text", source: text.String()})
		text.Reset()
	}

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\n")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```mermaid") {
			flush()
			var diagram strings.Builder
			for i++; i < len(lines); i++ {
				candidate := strings.TrimSuffix(lines[i], "\n")
				if strings.TrimSpace(candidate) == "```" {
					break
				}
				diagram.WriteString(lines[i])
			}
			segments = append(segments, terminalSegment{kind: "mermaid", source: diagram.String()})
			continue
		}
		if match := standaloneImageRE.FindStringSubmatch(line); match != nil {
			flush()
			segments = append(segments, terminalSegment{kind: "image", alt: match[1], target: match[2]})
			continue
		}
		if strings.Contains(trimmed, "<svg") {
			flush()
			var svg strings.Builder
			svg.WriteString(lines[i])
			for !strings.Contains(svg.String(), "</svg>") && i+1 < len(lines) {
				i++
				svg.WriteString(lines[i])
			}
			segments = append(segments, terminalSegment{kind: "svg", source: svg.String()})
			continue
		}
		text.WriteString(lines[i])
	}
	flush()
	return segments
}

func terminalLinks(source string) []TerminalLink {
	matches := linkRE.FindAllStringSubmatch(source, -1)
	links := make([]TerminalLink, 0, len(matches))
	for _, match := range matches {
		links = append(links, TerminalLink{Text: match[2], Target: match[3]})
	}
	return links
}

func mediaFallback(segment terminalSegment) string {
	switch segment.kind {
	case "image":
		label := segment.alt
		if label == "" {
			label = filepath.Base(segment.target)
		}
		return fmt.Sprintf("\n  [image: %s — %s]\n\n", termsafe.SafeLine(label), termsafe.SafeLine(segment.target))
	case "mermaid":
		return "\n  [Mermaid diagram — graphics unavailable]\n\n```mermaid\n" + safeMultiline(segment.source) + "```\n\n"
	case "svg":
		return "\n  [inline SVG — graphics unavailable]\n\n"
	default:
		return "\n  [media unavailable]\n\n"
	}
}

func fallbackWithError(fallback string, err error) string {
	return fallback + fmt.Sprintf("  [rendering note: %s; use `forgectl docs serve --open` for the web reader]\n\n", termsafe.SafeLine(err.Error()))
}

func safeMultiline(value string) string {
	lines := strings.SplitAfter(value, "\n")
	var out strings.Builder
	for _, line := range lines {
		if strings.HasSuffix(line, "\n") {
			out.WriteString(termsafe.SafeLine(strings.TrimSuffix(line, "\n")))
			out.WriteByte('\n')
		} else {
			out.WriteString(termsafe.SafeLine(line))
		}
	}
	return out.String()
}

func renderTerminalMedia(segment terminalSegment, doc Doc, root Root) (image.Image, error) {
	switch segment.kind {
	case "mermaid":
		if len(segment.source) > maxDiagramBytes {
			return nil, fmt.Errorf("Mermaid source exceeds 1 MiB") //nolint:staticcheck // Mermaid is a proper name.
		}
		pngBytes, err := raster.PNG(segment.source, 1,
			mermaid.WithCustomTheme("artificer", mermaid.Palette{
				Background: "#1d1f21", NodeFill: "#282a2e", NodeStroke: "#B0B9F9",
				Text: "#c5c8c6", Edge: "#8abeb7",
			}),
		)
		if err != nil {
			return nil, fmt.Errorf("Mermaid is unsupported or invalid: %w", err) //nolint:staticcheck // Mermaid is a proper name.
		}
		img, _, err := image.Decode(bytes.NewReader(pngBytes))
		return img, err
	case "svg":
		if len(segment.source) > maxDiagramBytes {
			return nil, fmt.Errorf("inline SVG exceeds 1 MiB")
		}
		pngBytes, err := raster.RasterizeSVG([]byte(segment.source), 1)
		if err != nil {
			return nil, err
		}
		img, _, err := image.Decode(bytes.NewReader(pngBytes))
		return img, err
	case "image":
		path, err := resolveTerminalResource(segment.target, doc, root)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(filepath.Ext(path), ".svg") {
			raw, err := readBounded(path, maxLocalImageBytes)
			if err != nil {
				return nil, err
			}
			pngBytes, err := raster.RasterizeSVG(raw, 1)
			if err != nil {
				return nil, err
			}
			img, _, err := image.Decode(bytes.NewReader(pngBytes))
			return img, err
		}
		return decodeBoundedImage(path)
	default:
		return nil, fmt.Errorf("unsupported media kind %q", segment.kind)
	}
}

func resolveTerminalResource(target string, doc Doc, root Root) (string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("invalid image target")
	}
	if u.Scheme != "" || u.Host != "" || strings.HasPrefix(target, "//") {
		return "", fmt.Errorf("remote images are disabled")
	}
	if u.Path == "" || filepath.IsAbs(filepath.FromSlash(u.Path)) {
		return "", fmt.Errorf("image path must be relative")
	}
	candidate := filepath.Join(filepath.Dir(doc.AbsPath), filepath.FromSlash(u.Path))
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve image: %w", err)
	}
	resolved = filepath.Clean(resolved)
	if !withinRoot(root.Path, resolved) {
		return "", fmt.Errorf("image escapes docs root")
	}
	if root.OnlyFile != "" {
		return "", fmt.Errorf("single-file roots do not grant sibling image access")
	}
	switch strings.ToLower(filepath.Ext(resolved)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".svg":
	default:
		return "", fmt.Errorf("unsupported local image format")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("image is not a regular file")
	}
	if info.Size() > maxLocalImageBytes {
		return "", fmt.Errorf("image exceeds 32 MiB")
	}
	return resolved, nil
}

func decodeBoundedImage(path string) (image.Image, error) {
	// path was canonicalized, contained to the selected docs root, extension
	// checked, and statted as a regular file by resolveTerminalResource.
	// #nosec G304 -- this is the validated local resource the user selected.
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	config, _, err := image.DecodeConfig(io.LimitReader(f, maxLocalImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decode image metadata: %w", err)
	}
	if config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > maxDecodedPixels {
		return nil, fmt.Errorf("decoded image exceeds 64 megapixels")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(io.LimitReader(f, maxLocalImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return img, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	// path passed the same canonical containment and regular-file validation as
	// decodeBoundedImage.
	// #nosec G304 -- this is the validated local resource the user selected.
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("image exceeds 32 MiB")
	}
	return raw, nil
}
