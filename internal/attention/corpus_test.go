package attention

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

// corpusMinRate is the item parse rate the real corpus must reach.
const corpusMinRate = 0.98

// TestCorpus is the parser's gate against real replies. It runs only when
// ATTENTION_CORPUS names a JSONL file: one JSON object per line whose "text"
// field holds one full assistant reply (as attention-corpus.py writes it).
// It fails when the corpus holds no block with a header, and when fewer than
// corpusMinRate of the item lines in those blocks parse. It prints counts
// only, never corpus text.
func TestCorpus(t *testing.T) {
	path := os.Getenv("ATTENTION_CORPUS")
	if path == "" {
		t.Skip("ATTENTION_CORPUS is not set")
	}
	f, err := os.Open(path) //nolint:gosec // G304: the operator names the corpus file
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var records, malformed, blocks, seen, read int
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		records++
		var rec struct {
			Text *string `json:"text"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil || rec.Text == nil {
			malformed++
			continue
		}
		p := Parse(*rec.Text)
		if !p.Found {
			continue
		}
		blocks++
		seen += p.Seen
		read += p.Read
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	t.Logf("records=%d malformed=%d blocks=%d seen=%d read=%d", records, malformed, blocks, seen, read)
	if blocks == 0 {
		t.Fatal("the corpus holds no block with a What's next header")
	}
	if seen == 0 {
		t.Fatal("the corpus blocks hold no item lines")
	}
	rate := float64(read) / float64(seen)
	t.Logf("read/seen = %.4f (gate %.2f)", rate, corpusMinRate)
	if rate < corpusMinRate {
		t.Errorf("item parse rate %.4f is below %.2f", rate, corpusMinRate)
	}
}
