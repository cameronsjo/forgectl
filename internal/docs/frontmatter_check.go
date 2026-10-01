package docs

import (
	"gopkg.in/yaml.v3"

	"github.com/cameronsjo/forgectl/internal/yamlsafe"
)

// This file holds the checks that keep a frontmatter block's decode linear
// (#910). The block is untrusted document text, and it is decoded before
// every other bound on a request (the render deadline, the markup guard,
// the render lock) and in the index scan of every doc.
//
// YAML is decoded once, into a yaml.Node, which is linear in the block.
// yaml.v3's decode into a map is not: it compares every key of a mapping
// with every later key, and formats an error string for each duplicate
// pair, so 3276 copies of `a: 1` in 16 KiB took 2.5 s of CPU per decode
// (4.8 s for the page); it also expands merge keys and aliases, so
// `<<: [*b, *b, …]` over one large anchor took 420 ms. So no consumer
// decodes the block into a map: yamlFrontmatterRoot refuses the block
// shapes that decode refused, with one linear walk of the node tree
// (yamlsafe.CheckTree, shared with the sops and registry readers since
// #959), and every consumer reads the node.
//
// TOML is decoded only to show the page's properties block, once, but
// BurntSushi/toml is quadratic in the segments of a dotted key (5.7 s of
// CPU for a page whose 16 KiB block is `a.a.…b = 1`), in those of a table
// header (1.9 s) and in the nesting of inline tables (1.4 s), so a +++
// block has a much smaller cap of its own, maxTOMLFrontmatterBytes.

// yamlFrontmatterRoot decodes a --- block and returns its top-level
// mapping (nil for an empty block, or one that is only `null`), and
// whether the block is well-formed frontmatter. A block is refused when
// the decode fails, when its top level is not a mapping, or when any
// mapping in it has a duplicate key, a merge key (`<<`), a key that is a
// sequence or a mapping, or a value that contains its own anchor, or when
// a scalar's explicit tag does not fit its value. Those are the blocks a
// yaml.Unmarshal into map[string]any rejects, except merge keys, which it
// expands. The consumers read the node, where a merge is not applied, so
// a merged key would silently go missing; a block with one is refused
// instead, and written frontmatter does not use them. A block the map
// decode refused only for its aliasing ("excessive aliasing") is
// accepted: nothing here expands an alias.
func yamlFrontmatterRoot(block []byte) (*yaml.Node, bool) {
	var doc yaml.Node
	if yaml.Unmarshal(block, &doc) != nil {
		return nil, false
	}
	if len(doc.Content) == 0 {
		// Empty, or only comments: legal, empty frontmatter.
		return nil, true
	}
	root := doc.Content[0]
	switch {
	case root.Kind == yaml.ScalarNode && root.ShortTag() == "!!null":
		return nil, true
	case root.Kind != yaml.MappingNode:
		return nil, false
	}
	if yamlsafe.CheckTree(root, 0) != nil {
		return nil, false
	}
	return root, true
}

// maxTOMLFrontmatterBytes is the largest +++ block splitFrontmatter
// accepts; a larger one is treated as no frontmatter, like a block over
// maxFrontmatterBytes. It bounds the one TOML decode (the properties block)
// by size alone, whatever the block's shape: BurntSushi/toml exposes no
// parse step to inspect first, and a hand-written pre-scan that tried to
// bound the costly shapes missed one (a multi-line string closed by extra
// quotes, `"""x""""`, which desynchronized it). The cost is quadratic in
// the block at worst, so the cap is small: at 1 KiB the worst shapes
// measured, a dotted key of about 500 segments at the top level, inside an
// inline table or after such a string, render a page in about 50 ms of
// CPU, against 3.5 ms for plain keys; at 2 KiB they took 100 to 150 ms.
// TOML frontmatter (Hugo's) is a handful of keys, a few hundred bytes.
const maxTOMLFrontmatterBytes = 1 << 10
