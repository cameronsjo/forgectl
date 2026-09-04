// Package linkspike_test is a throwaway measurement spike for forgectl#443
// (Task 1 of docs/plans/2026-09-04-link-substrate.md). It builds a private,
// first-cut markdown index + link resolver over each of a set of roots and
// reports integer-only counters — nothing scanned from a document (path,
// title, alias, link target) may ever appear in a log line, an assertion
// message, or the report file. The whole package is deleted in Task 5.
package linkspike_test

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"go.abhg.dev/goldmark/wikilink"
	"gopkg.in/yaml.v3"
)

// --- flags -----------------------------------------------------------------

// stringSliceFlag collects repeated -roots occurrences. String() deliberately
// never renders a value — flag values are scanned-root paths and must never
// reach any output surface, including a -h usage line.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return fmt.Sprintf("%d roots", len(*s)) }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

var rootsFlag stringSliceFlag

func init() {
	flag.Var(&rootsFlag, "roots", "root directory to scan; repeatable")
}

// --- excluded-dir mirror (index.go's excludedDir) ---------------------------

var hiddenOrVendorDir = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
}

func excludedDir(name string) bool {
	return hiddenOrVendorDir[name] || strings.HasPrefix(name, ".")
}

// --- root-kind detection -----------------------------------------------------

const (
	rootKindDocs  = 0
	rootKindVault = 1
)

const (
	stopFound  = 0
	stopHome   = 1
	stopDevice = 2
	stopRoot   = 3
)

func devOf(p string) (uint64, bool) {
	info, err := os.Stat(p)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}

// detectRootKind walks up from rootAbs looking for a .obsidian directory,
// stopping at $HOME (inclusive — $HOME itself is never a vault), a device
// change, or "/".
func detectRootKind(rootAbs string) (kind, stopReason int) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	} else {
		home = filepath.Clean(home)
	}
	cur := filepath.Clean(rootAbs)
	startDev, ok := devOf(cur)
	if !ok {
		return rootKindDocs, stopFound
	}
	for {
		if home != "" && cur == home {
			return rootKindDocs, stopHome
		}
		if info, err := os.Stat(filepath.Join(cur, ".obsidian")); err == nil && info.IsDir() {
			return rootKindVault, stopFound
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return rootKindDocs, stopRoot
		}
		dev, ok := devOf(parent)
		if !ok || dev != startDev {
			return rootKindDocs, stopDevice
		}
		cur = parent
	}
}

// --- markdown walk -----------------------------------------------------------

func walkMarkdown(rootAbs string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(rootAbs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != rootAbs && excludedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(p), ".md") {
			return nil
		}
		files = append(files, p)
		return nil
	})
	return files, err
}

// --- frontmatter -------------------------------------------------------------

func isFrontmatterFence(line []byte) bool {
	line = bytes.TrimRight(line, "\r")
	if len(line) < 3 {
		return false
	}
	for _, c := range line {
		if c != '-' {
			return false
		}
	}
	return true
}

// extractFrontmatter returns aliases (list or scalar, folded to a list) and
// the byte offset where the document body begins (0 when there is no
// well-formed leading frontmatter block).
func extractFrontmatter(source []byte) (aliases []string, bodyOffset int) {
	lines := bytes.SplitAfter(source, []byte("\n"))
	if len(lines) == 0 {
		return nil, 0
	}
	first := bytes.TrimSuffix(lines[0], []byte("\n"))
	if !isFrontmatterFence(first) {
		return nil, 0
	}
	for i := 1; i < len(lines); i++ {
		line := bytes.TrimSuffix(lines[i], []byte("\n"))
		if !isFrontmatterFence(line) {
			continue
		}
		block := bytes.Join(lines[1:i], nil)
		offset := 0
		for j := 0; j <= i; j++ {
			offset += len(lines[j])
		}
		var m map[string]any
		if err := yaml.Unmarshal(block, &m); err == nil {
			if v, ok := m["aliases"]; ok {
				aliases = toStringList(v)
			}
		}
		return aliases, offset
	}
	return nil, 0
}

func toStringList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// --- block ids -----------------------------------------------------------

var blockIDPattern = regexp.MustCompile(`\^([A-Za-z0-9_-]+)\s*$`)

func scanBlockIDs(body []byte) map[string]bool {
	ids := map[string]bool{}
	scanner := bufio.NewScanner(body2Reader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if m := blockIDPattern.FindStringSubmatch(line); m != nil {
			ids[m[1]] = true
		}
	}
	return ids
}

func body2Reader(body []byte) *bytes.Reader { return bytes.NewReader(body) }

// --- title -----------------------------------------------------------------

func firstH1(body []byte) string {
	scanner := bufio.NewScanner(body2Reader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for i := 0; i < 200 && scanner.Scan(); i++ {
		line := strings.TrimSpace(scanner.Text())
		if after, ok := strings.CutPrefix(line, "# "); ok {
			if t := strings.TrimSpace(after); t != "" {
				return t
			}
		}
	}
	return ""
}

// --- goldmark AST scan: headings + outbound links ---------------------------

var spikeMarkdown = newSpikeMarkdown()

func newSpikeMarkdown() goldmark.Markdown {
	md := goldmark.New(
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	(&wikilink.Extender{}).Extend(md)
	return md
}

type headingRef struct {
	slug      string
	textLower string
}

const (
	formPlain   = "plain"
	formAlias   = "alias"
	formEmbed   = "embed"
	formHeading = "heading"
	formBlock   = "block"
	formRelPath = "relpath"
)

type linkOcc struct {
	form     string
	target   string
	fragment string
}

var urlSchemePrefix = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

func isURLLike(dest string) bool {
	if strings.HasPrefix(dest, "//") {
		return true
	}
	return urlSchemePrefix.MatchString(dest)
}

func splitFirstHash(s string) (string, string) {
	idx := strings.Index(s, "#")
	if idx < 0 {
		return s, ""
	}
	return s[:idx], s[idx+1:]
}

func headingText(n *ast.Heading, source []byte) string {
	var b strings.Builder
	appendNodeText(&b, n, source)
	return b.String()
}

func appendNodeText(b *strings.Builder, n ast.Node, source []byte) {
	if t, ok := n.(*ast.Text); ok {
		b.Write(t.Segment.Value(source))
		return
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		appendNodeText(b, c, source)
	}
}

func scanBody(body []byte) ([]headingRef, []linkOcc) {
	reader := text.NewReader(body)
	doc := spikeMarkdown.Parser().Parse(reader)

	var headings []headingRef
	var links []linkOcc

	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case ast.KindHeading:
			h := n.(*ast.Heading)
			slug := ""
			if v, ok := h.AttributeString("id"); ok {
				if b, ok := v.([]byte); ok {
					slug = string(b)
				}
			}
			headings = append(headings, headingRef{
				slug:      strings.ToLower(slug),
				textLower: strings.ToLower(strings.TrimSpace(headingText(h, body))),
			})
		case wikilink.Kind:
			wl := n.(*wikilink.Node)
			target := string(wl.Target)
			frag := string(wl.Fragment)

			// Reconstruct per the Global Constraint: the library splits on
			// the LAST '#'; rejoin and re-split on the FIRST '#'.
			combined := target
			if frag != "" {
				combined = target + "#" + frag
			}
			path0, frag0 := splitFirstHash(combined)

			isAlias := false
			if c := wl.FirstChild(); c != nil {
				if tn, ok := c.(*ast.Text); ok {
					label := string(tn.Segment.Value(body))
					reconstruct := target
					if frag != "" {
						reconstruct = target + "#" + frag
					}
					isAlias = label != reconstruct
				}
			}

			form := formPlain
			switch {
			case wl.Embed:
				form = formEmbed
			case strings.HasPrefix(frag0, "^"):
				form = formBlock
			case frag0 != "":
				form = formHeading
			case isAlias:
				form = formAlias
			}
			links = append(links, linkOcc{form: form, target: path0, fragment: frag0})
		case ast.KindLink:
			l := n.(*ast.Link)
			dest := string(l.Destination)
			if isURLLike(dest) {
				return ast.WalkContinue, nil
			}
			path0, frag0 := splitFirstHash(dest)
			links = append(links, linkOcc{form: formRelPath, target: path0, fragment: frag0})
		}
		return ast.WalkContinue, nil
	})

	return headings, links
}

// --- doc records + tables ----------------------------------------------------

type docRecord struct {
	idx          int
	relLower     string
	dirRel       string
	nameLower    string
	aliasesLower []string
	headings     []headingRef
	blockIDs     map[string]bool
	links        []linkOcc
	hasHash      bool
}

func buildDocRecord(idx int, rootAbs, absPath string) (docRecord, error) {
	relOS, err := filepath.Rel(rootAbs, absPath)
	if err != nil {
		return docRecord{}, err
	}
	rel := filepath.ToSlash(relOS)

	source, err := os.ReadFile(absPath)
	if err != nil {
		return docRecord{}, err
	}

	aliases, bodyOffset := extractFrontmatter(source)
	body := source[bodyOffset:]
	_ = firstH1(body) // title is scanned per spec; not used by any output column

	blockIDs := scanBlockIDs(body)
	headings, links := scanBody(body)

	base := path.Base(rel)
	nameNoExt := strings.TrimSuffix(base, path.Ext(base))
	relNoExt := strings.TrimSuffix(rel, path.Ext(rel))
	dirRel := path.Dir(rel)

	aliasesLower := make([]string, 0, len(aliases))
	for _, a := range aliases {
		if t := strings.ToLower(strings.TrimSpace(a)); t != "" {
			aliasesLower = append(aliasesLower, t)
		}
	}

	return docRecord{
		idx:          idx,
		relLower:     strings.ToLower(relNoExt),
		dirRel:       dirRel,
		nameLower:    strings.ToLower(nameNoExt),
		aliasesLower: aliasesLower,
		headings:     headings,
		blockIDs:     blockIDs,
		links:        links,
		hasHash:      strings.Contains(base, "#"),
	}, nil
}

type rootTables struct {
	docs    []docRecord
	byRel   map[string][]int
	byName  map[string][]int
	byAlias map[string][]int
}

func buildTables(docs []docRecord) rootTables {
	t := rootTables{
		docs:    docs,
		byRel:   map[string][]int{},
		byName:  map[string][]int{},
		byAlias: map[string][]int{},
	}
	for _, d := range docs {
		t.byRel[d.relLower] = append(t.byRel[d.relLower], d.idx)
		t.byName[d.nameLower] = append(t.byName[d.nameLower], d.idx)
		for _, a := range d.aliasesLower {
			t.byAlias[a] = append(t.byAlias[a], d.idx)
		}
	}
	return t
}

// --- resolution --------------------------------------------------------------

const (
	missNone        = 0
	missAmbiguousV  = 1
	missOutsideRoot = 2
)

func resolveRelative(tables rootTables, from docRecord, target string) (int, int) {
	joined := path.Clean(path.Join(from.dirRel, target))
	if joined == ".." || strings.HasPrefix(joined, "../") || strings.HasPrefix(joined, "/") {
		return -1, missOutsideRoot
	}
	key := strings.ToLower(strings.TrimSuffix(joined, ".md"))
	switch cands := tables.byRel[key]; len(cands) {
	case 0:
		return -1, missNone
	case 1:
		return cands[0], missNone
	default:
		return -1, missAmbiguousV
	}
}

func resolveVaultTarget(tables rootTables, target string) (int, int) {
	cleanTarget := path.Clean(target)
	if cleanTarget == ".." || strings.HasPrefix(cleanTarget, "../") || strings.HasPrefix(cleanTarget, "/") {
		return -1, missOutsideRoot
	}
	key := strings.ToLower(strings.TrimSuffix(cleanTarget, ".md"))

	if cands := tables.byRel[key]; len(cands) == 1 {
		return cands[0], missNone
	} else if len(cands) > 1 {
		return -1, missAmbiguousV
	}

	base := path.Base(key)
	nameCands := tables.byName[base]
	if strings.Contains(cleanTarget, "/") {
		filtered := make([]int, 0, len(nameCands))
		for _, ci := range nameCands {
			rel := tables.docs[ci].relLower
			if rel == key || strings.HasSuffix(rel, "/"+key) {
				filtered = append(filtered, ci)
			}
		}
		nameCands = filtered
	}
	switch len(nameCands) {
	case 1:
		return nameCands[0], missNone
	case 0:
		// fall through to alias lookup
	default:
		return -1, missAmbiguousV
	}

	switch aliasCands := tables.byAlias[strings.ToLower(target)]; len(aliasCands) {
	case 1:
		return aliasCands[0], missNone
	case 0:
		return -1, missNone
	default:
		return -1, missAmbiguousV
	}
}

func fragmentOK(doc docRecord, fragment string) bool {
	if fragment == "" {
		return true
	}
	if strings.HasPrefix(fragment, "^") {
		return doc.blockIDs[fragment[1:]]
	}
	seg := fragment
	if idx := strings.LastIndex(fragment, "#"); idx >= 0 {
		seg = fragment[idx+1:]
	}
	segLower := strings.ToLower(seg)
	for _, h := range doc.headings {
		if h.slug == segLower || h.textLower == segLower {
			return true
		}
	}
	return false
}

// --- vault-wide basename set (read-only, built once) -------------------------

var (
	vaultBasenameSet  map[string]bool
	vaultBasenameOnce sync.Once
)

func vaultBasenames() map[string]bool {
	vaultBasenameOnce.Do(func() {
		vaultBasenameSet = map[string]bool{}
		root := os.Getenv("OBSIDIAN_VAULT")
		if root == "" {
			return
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return
		}
		files, err := walkMarkdown(abs)
		if err != nil {
			return
		}
		for _, f := range files {
			base := filepath.Base(f)
			name := strings.TrimSuffix(base, filepath.Ext(base))
			vaultBasenameSet[strings.ToLower(name)] = true
		}
	})
	return vaultBasenameSet
}

func basenameElsewhereInVault(target string) bool {
	set := vaultBasenames()
	if len(set) == 0 {
		return false
	}
	base := path.Base(strings.TrimSuffix(path.Clean(target), "/"))
	base = strings.TrimSuffix(base, ".md")
	base = strings.ToLower(base)
	if base == "" || base == "." {
		return false
	}
	return set[base]
}

// --- per-root measurement -----------------------------------------------------

type resolveStats struct {
	plain, alias, embed, heading, block, relpath int64
	resolved, noTarget, ambiguous, outsideRoot   int64
	unresolvedBasenameElsewhere                  int64
}

func resolveAll(tables rootTables, kind int) resolveStats {
	var stats resolveStats
	for _, d := range tables.docs {
		for _, lk := range d.links {
			switch lk.form {
			case formPlain:
				stats.plain++
			case formAlias:
				stats.alias++
			case formEmbed:
				stats.embed++
			case formHeading:
				stats.heading++
			case formBlock:
				stats.block++
			case formRelPath:
				stats.relpath++
			}

			var targetIdx = -1
			var miss = missNone
			switch {
			case lk.target == "":
				targetIdx = d.idx
			case lk.form == formRelPath || kind == rootKindDocs:
				targetIdx, miss = resolveRelative(tables, d, lk.target)
			default:
				targetIdx, miss = resolveVaultTarget(tables, lk.target)
			}

			switch {
			case miss == missOutsideRoot:
				stats.outsideRoot++
			case miss == missAmbiguousV:
				stats.ambiguous++
			case targetIdx < 0:
				stats.noTarget++
				if basenameElsewhereInVault(lk.target) {
					stats.unresolvedBasenameElsewhere++
				}
			case !fragmentOK(tables.docs[targetIdx], lk.fragment):
				stats.noTarget++
			default:
				stats.resolved++
			}
		}
	}
	return stats
}

type rootRow struct {
	kind, stopReason                                   int64
	docs                                               int64
	linksPlain, linksAlias, linksEmbed, linksHeading   int64
	linksBlock, linksRelPath                           int64
	resolved, missNoTarget, missAmbiguous, missOutside int64
	missPermille                                       int64
	indexMS, resolveMS                                 int64
	heapAllocDelta, heapAllocPeak                      int64
	unresolvedBasenameElsewhere, ambiguousBasenames    int64
	filenamesWithHash                                  int64
}

func measureRoot(rootAbs string) (rootRow, error) {
	kind, stopReason := detectRootKind(rootAbs)

	t0 := time.Now()
	files, err := walkMarkdown(rootAbs)
	if err != nil {
		return rootRow{}, err
	}
	docs := make([]docRecord, 0, len(files))
	for _, f := range files {
		rec, err := buildDocRecord(len(docs), rootAbs, f)
		if err != nil {
			continue // unreadable file mid-walk: skip, not fatal
		}
		docs = append(docs, rec)
	}
	tables := buildTables(docs)
	indexMS := time.Since(t0).Milliseconds()

	var ambiguousBasenames int64
	var filenamesWithHash int64
	for _, cands := range tables.byName {
		if len(cands) > 1 {
			ambiguousBasenames++
		}
	}
	for _, d := range docs {
		if d.hasHash {
			filenamesWithHash++
		}
	}

	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	t1 := time.Now()
	stats := resolveAll(tables, kind)
	resolveMS := time.Since(t1).Milliseconds()

	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	heapAllocDelta := int64(memAfter.HeapAlloc) - int64(memBefore.HeapAlloc)
	heapAllocPeak := int64(memBefore.HeapAlloc)
	if int64(memAfter.HeapAlloc) > heapAllocPeak {
		heapAllocPeak = int64(memAfter.HeapAlloc)
	}

	total := stats.plain + stats.alias + stats.embed + stats.heading + stats.block + stats.relpath
	missTotal := stats.noTarget + stats.ambiguous + stats.outsideRoot
	var missPermille int64
	if total > 0 {
		missPermille = (missTotal * 1000) / total
	}

	return rootRow{
		kind:                        int64(kind),
		stopReason:                  int64(stopReason),
		docs:                        int64(len(docs)),
		linksPlain:                  stats.plain,
		linksAlias:                  stats.alias,
		linksEmbed:                  stats.embed,
		linksHeading:                stats.heading,
		linksBlock:                  stats.block,
		linksRelPath:                stats.relpath,
		resolved:                    stats.resolved,
		missNoTarget:                stats.noTarget,
		missAmbiguous:               stats.ambiguous,
		missOutside:                 stats.outsideRoot,
		missPermille:                missPermille,
		indexMS:                     indexMS,
		resolveMS:                   resolveMS,
		heapAllocDelta:              heapAllocDelta,
		heapAllocPeak:               heapAllocPeak,
		unresolvedBasenameElsewhere: stats.unresolvedBasenameElsewhere,
		ambiguousBasenames:          ambiguousBasenames,
		filenamesWithHash:           filenamesWithHash,
	}, nil
}

// --- report writer -----------------------------------------------------------

var columnHeader = []string{
	"root#", "kind", "stopReason", "docs", "links_plain", "links_alias", "links_embed",
	"links_heading", "links_block", "links_relpath", "resolved", "missNoTarget",
	"missAmbiguous", "missOutsideRoot", "missPermille", "indexMS", "resolveMS",
	"heapAllocDeltaBytes", "heapAllocPeakBytes", "unresolvedBasenameElsewhereInVault",
	"ambiguousBasenames", "filenamesWithHash",
}

func rowValues(rootNum int, r rootRow) []int64 {
	return []int64{
		int64(rootNum), r.kind, r.stopReason, r.docs,
		r.linksPlain, r.linksAlias, r.linksEmbed, r.linksHeading, r.linksBlock, r.linksRelPath,
		r.resolved, r.missNoTarget, r.missAmbiguous, r.missOutside, r.missPermille,
		r.indexMS, r.resolveMS, r.heapAllocDelta, r.heapAllocPeak,
		r.unresolvedBasenameElsewhere, r.ambiguousBasenames, r.filenamesWithHash,
	}
}

func appendRow(outPath string, values []int64) error {
	needsHeader := true
	if info, err := os.Stat(outPath); err == nil && info.Size() > 0 {
		needsHeader = false
	}
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	if needsHeader {
		fmt.Fprintf(w, "| %s |\n", strings.Join(columnHeader, " | "))
		seps := make([]string, len(columnHeader))
		for i := range seps {
			seps[i] = "---"
		}
		fmt.Fprintf(w, "| %s |\n", strings.Join(seps, " | "))
	}
	cells := make([]string, len(values))
	for i, v := range values {
		cells[i] = strconv.FormatInt(v, 10)
	}
	fmt.Fprintf(w, "| %s |\n", strings.Join(cells, " | "))
	return w.Flush()
}

// --- entry point ---------------------------------------------------------

func TestLinkSpike(t *testing.T) {
	if os.Getenv("FORGECTL_LINK_SPIKE") != "1" {
		t.Skip("set FORGECTL_LINK_SPIKE=1 to run this spike")
	}

	outPath := os.Getenv("FORGECTL_LINK_SPIKE_OUT")
	if outPath == "" {
		t.Fatal("FORGECTL_LINK_SPIKE_OUT is required")
	}
	if len(rootsFlag) == 0 {
		t.Fatal("at least one -roots value is required")
	}

	for i, r := range rootsFlag {
		rootNum := i + 1
		abs, err := filepath.Abs(r)
		if err != nil {
			t.Fatalf("root %d: path error class %T", rootNum, err)
		}
		row, err := measureRoot(abs)
		if err != nil {
			t.Fatalf("root %d: measurement error class %T", rootNum, err)
		}
		if err := appendRow(outPath, rowValues(rootNum, row)); err != nil {
			t.Fatalf("root %d: write error class %T", rootNum, err)
		}
	}
}
