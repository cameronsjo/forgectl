// Package yamlsafe parses YAML into a node tree in time and memory bounded
// by the input, for a caller that must not decode an arbitrary document into
// a map or a struct first (#910, #959).
//
// yaml.v3's decode into a map or a struct is not linear. It compares every
// key of a mapping with every later key, and it formats an error string for
// each duplicate pair, so 64 KiB of `a: 1` took 92 s of CPU and 256 KiB of
// unique keys took 4.5 s (measured on v3.0.1). It also expands merge keys
// and aliases, and it runs the pairwise check again on every expansion.
// Parsing into a yaml.Node is linear: 4 MiB took 0.17 s for a sops-shaped
// file and 2 s for the worst shape, one-letter duplicate keys.
//
// So a caller parses with Parse, which refuses input over its byte cap,
// and then either reads the node or runs CheckTree before it decodes. A
// size cap alone does not help a map decode, which goes quadratic well
// inside any cap a real file needs.
package yamlsafe

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ErrTooLarge is wrapped by Parse's error for input over its byte cap.
var ErrTooLarge = errors.New("document is too large")

// Parse checks that data is at most maxBytes bytes and parses it into a
// node tree. The result is the DocumentNode, or the zero Node for empty
// input. Over the cap, the error wraps ErrTooLarge and gives the size and the
// limit. The input is never cut short, because a truncated document parses
// as a different one. A parse failure comes back as yaml.v3's own error.
func Parse(data []byte, maxBytes int) (*yaml.Node, error) {
	if len(data) > maxBytes {
		return nil, fmt.Errorf("%w: %d bytes, over the %d-byte limit", ErrTooLarge, len(data), maxBytes)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// Root returns a parsed document's top-level node, or nil when the document
// is empty or only comments.
func Root(doc *yaml.Node) *yaml.Node {
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	return doc.Content[0]
}

// CheckTree walks the tree under root once, without following aliases, and
// returns an error for the first node a map decode would refuse, plus merge
// keys and over-wide mappings. It returns nil when there is no such node.
//
// It refuses:
//   - a mapping with a duplicate key, by the decoder's own test (same kind
//     and same value);
//   - a merge key (`<<`), which a reader of the node would not apply, so a
//     merged key would silently go missing;
//   - a key that is a sequence, a mapping, or an alias to one;
//   - an alias to a node that contains it;
//   - a scalar whose explicit tag does not fit its value.
//
// A positive maxKeys also refuses a mapping with more than that many keys.
// The cap bounds the decoder's pairwise key check, which runs again each
// time an alias to the mapping expands. maxKeys < 1 means no cap.
//
// Each node is visited once and each mapping's keys go through one set, so
// the walk is linear in the tree. The errors name a line, never a key or a
// value, because the document is untrusted text.
func CheckTree(root *yaml.Node, maxKeys int) error {
	type frame struct {
		n    *yaml.Node
		exit bool
	}
	onPath := make(map[*yaml.Node]bool)
	stack := []frame{{n: root}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.exit {
			delete(onPath, f.n)
			continue
		}
		n := f.n
		if n == nil {
			continue
		}
		switch n.Kind {
		case yaml.AliasNode:
			// An alias to a node that contains it never finishes
			// expanding; the decode refuses it, and so does this.
			if n.Alias == nil || onPath[n.Alias] {
				return fmt.Errorf("line %d: an alias refers to a node that contains it", n.Line)
			}
			continue
		case yaml.ScalarNode:
			if n.Style&yaml.TaggedStyle != 0 {
				var v any
				if n.Decode(&v) != nil {
					return fmt.Errorf("line %d: a value does not fit its explicit tag", n.Line)
				}
			}
			continue
		case yaml.MappingNode:
			if maxKeys > 0 && len(n.Content)/2 > maxKeys {
				return fmt.Errorf("line %d: a mapping has %d keys, over the limit of %d", n.Line, len(n.Content)/2, maxKeys)
			}
			if err := CheckKeys(n); err != nil {
				return err
			}
		}
		onPath[n] = true
		stack = append(stack, frame{n: n, exit: true})
		for _, c := range n.Content {
			stack = append(stack, frame{n: c})
		}
	}
	return nil
}

// CheckKeys returns an error unless a mapping's keys are unique (by the
// decoder's own test: same kind and same value), free of merge keys, and
// all scalars or aliases of scalars. It is linear in the mapping.
func CheckKeys(m *yaml.Node) error {
	type key struct {
		kind  yaml.Kind
		value string
	}
	seen := make(map[key]bool, len(m.Content)/2)
	for i := 0; i < len(m.Content); i += 2 {
		k := m.Content[i]
		switch k.Kind {
		case yaml.ScalarNode:
			if k.Value == "<<" && k.ShortTag() == "!!merge" {
				return fmt.Errorf("line %d: a merge key (<<) is not supported", k.Line)
			}
		case yaml.AliasNode:
			if k.Alias == nil || k.Alias.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: a key is an alias to a sequence or a mapping", k.Line)
			}
		default:
			return fmt.Errorf("line %d: a key is a sequence or a mapping", k.Line)
		}
		kk := key{k.Kind, k.Value}
		if seen[kk] {
			return fmt.Errorf("line %d: a mapping key is repeated", k.Line)
		}
		seen[kk] = true
	}
	return nil
}
