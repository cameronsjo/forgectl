package docs

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/net/html"
)

var mediaTypes = map[string]string{
	".avif": "image/avif",
	".gif":  "image/gif",
	".jpeg": "image/jpeg",
	".jpg":  "image/jpeg",
	".png":  "image/png",
	".svg":  "image/svg+xml",
	".webp": "image/webp",
}

// AllowedMediaExt reports whether path is a browser-readable image type the
// docs server is willing to expose. The response type comes from this same
// closed table rather than content sniffing.
func AllowedMediaExt(path string) bool {
	_, ok := mediaTypes[strings.ToLower(filepath.Ext(path))]
	return ok
}

func mediaType(path string) (string, bool) {
	t, ok := mediaTypes[strings.ToLower(filepath.Ext(path))]
	return t, ok
}

// RewriteLocalImageURLs points relative img sources at the reader's
// same-origin media endpoint. The endpoint receives both the serving document
// and the normalized target path so it can prove that the document actually
// references the requested file before reading it.
func RewriteLocalImageURLs(rendered, rootLabel, docRel string) (string, error) {
	var out strings.Builder
	tokens := html.NewTokenizer(strings.NewReader(rendered))
	for {
		tokenType := tokens.Next()
		if tokenType == html.ErrorToken {
			if err := tokens.Err(); err != nil && !errors.Is(err, io.EOF) {
				return "", fmt.Errorf("parse rendered markdown: %w", err)
			}
			return out.String(), nil
		}

		// Raw aliases the tokenizer's scratch buffer; Token may normalize names
		// in that same buffer (notably SVG viewBox/linearGradient). Copy before
		// asking for the parsed token so unrelated markup stays byte-for-byte.
		rawToken := append([]byte(nil), tokens.Raw()...)
		token := tokens.Token()
		rewritten := false
		if (tokenType == html.StartTagToken || tokenType == html.SelfClosingTagToken) && token.Data == "img" {
			for i := range token.Attr {
				if token.Attr[i].Key != "src" {
					continue
				}
				mediaRel, fragment, ok := relativeMediaPath(token.Attr[i].Val, docRel)
				if !ok {
					continue
				}
				query := url.Values{"doc": {docRel}, "path": {mediaRel}}
				token.Attr[i].Val = "/media/" + url.PathEscape(rootLabel) + "?" + query.Encode()
				if fragment != "" {
					token.Attr[i].Val += "#" + url.PathEscape(fragment)
				}
				rewritten = true
			}
		}
		if rewritten {
			out.WriteString(token.String())
		} else {
			out.Write(rawToken)
		}
	}
}

// relativeMediaPath converts a relative URL from docRel's directory into a
// root-relative path. Absolute, remote, data, and root-escaping references are
// deliberately not rewritten; the existing CSP leaves remote images blocked.
func relativeMediaPath(raw, docRel string) (mediaRel, fragment string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "" || u.Host != "" || u.Path == "" || strings.HasPrefix(u.Path, "/") {
		return "", "", false
	}
	joined := path.Clean(path.Join(path.Dir(docRel), u.Path))
	if joined == ".." || strings.HasPrefix(joined, "../") || !AllowedMediaExt(joined) {
		return "", "", false
	}
	return joined, u.Fragment, true
}

// ResolveMedia applies the same canonical-root and excluded-directory
// boundaries as document resolution. A single-file root may resolve a sibling
// image only because handleMedia separately proves that the indexed document
// explicitly references it; this does not widen which Markdown files it can
// serve.
func (idx *Index) ResolveMedia(rootLabel, relPath string) (string, error) {
	for _, root := range idx.roots {
		if root.Label != rootLabel {
			continue
		}
		resolved, err := ResolveInRoot(root.Path, filepath.FromSlash(relPath))
		if err != nil {
			return "", err
		}
		if !AllowedMediaExt(resolved) {
			return "", ErrDisallowedExt
		}
		rel, err := filepath.Rel(root.Path, resolved)
		if err != nil {
			return "", ErrOutsideRoot
		}
		segments := strings.Split(filepath.ToSlash(rel), "/")
		for _, dir := range segments[:len(segments)-1] {
			if excludedDir(dir) {
				return "", ErrNotIndexed
			}
		}
		return resolved, nil
	}
	return "", ErrRootNotFound
}

func handleMedia(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rootLabel := r.PathValue("root")
		docRel := r.URL.Query().Get("doc")
		requested := r.URL.Query().Get("path")
		if docRel == "" || requested == "" {
			http.NotFound(w, r)
			return
		}

		idx := store.Current()
		docPath, err := idx.Resolve(rootLabel, docRel)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		// docPath came from Index.Resolve: canonical root containment, extension,
		// and exact index membership have all been checked.
		source, err := os.ReadFile(docPath) //nolint:gosec // resolved indexed document path, not a raw request path
		if err != nil {
			http.NotFound(w, r)
			return
		}
		rendered, err := Render(source)
		if err != nil {
			slog.Debug("docs: media source document could not be rendered.", "root", rootLabel, "doc", docRel, "error", err)
			http.NotFound(w, r)
			return
		}
		if !renderedReferencesMedia(rendered, docRel, requested) {
			slog.Debug("docs: media request was not referenced by its document.", "root", rootLabel, "doc", docRel, "media", requested)
			http.NotFound(w, r)
			return
		}

		mediaPath, err := idx.ResolveMedia(rootLabel, requested)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		contentType, ok := mediaType(mediaPath)
		if !ok {
			http.NotFound(w, r)
			return
		}
		// mediaPath came from ResolveMedia after reference authorization and the
		// same canonical containment chain used for Markdown files.
		file, err := os.Open(mediaPath) //nolint:gosec // resolved contained media path, not a raw request path
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() {
			if closeErr := file.Close(); closeErr != nil {
				slog.Debug("docs: media file could not be closed.", "path", mediaPath, "error", closeErr)
			}
		}()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		if contentType == "image/svg+xml" {
			// An SVG loaded through <img> is inert in modern browsers, but this
			// response can also be navigated to directly. Sandbox that document so
			// an authored script cannot execute with the reader origin's authority.
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
		}
		http.ServeContent(w, r, filepath.Base(mediaPath), info.ModTime(), file)
	}
}

func renderedReferencesMedia(rendered, docRel, requested string) bool {
	tokens := html.NewTokenizer(strings.NewReader(rendered))
	for {
		tokenType := tokens.Next()
		if tokenType == html.ErrorToken {
			return false
		}
		if tokenType != html.StartTagToken && tokenType != html.SelfClosingTagToken {
			continue
		}
		token := tokens.Token()
		if token.Data != "img" {
			continue
		}
		for _, attr := range token.Attr {
			if attr.Key != "src" {
				continue
			}
			mediaRel, _, ok := relativeMediaPath(attr.Val, docRel)
			if ok && mediaRel == requested {
				return true
			}
		}
	}
}
