// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// allocated is the bytes f allocates, from the runtime's running total.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A queue of large items costs about their bytes, not a copy per line: Queue
// reads each item once to check its hash and builds no body. 20 pending
// items of 1 MiB of newlines each (20 MiB, a million lines apiece) must stay
// under 96 MiB of allocation per Queue call. Building the escaped body and
// marks for every item, as Queue once did, allocates gigabytes here. Loading
// a run beside that queue builds no queue at all.
func TestAQueueOfLargeItemsCostsAboutTheirBytes(t *testing.T) {
	const items, size, bound = 20, 1 << 20, 96 << 20
	d := openDesk(t)
	for i := range items {
		writeFile(t, filepath.Join(d.Path(), "pending", fmt.Sprintf("%02d-big.sh", i+1)), strings.Repeat("\n", size))
	}
	done := filepath.Join(d.Path(), "done")
	writeFile(t, filepath.Join(done, "90-x.sh"), "true\n")
	writeFile(t, filepath.Join(done, "90-x.log"), "EXIT=0\n")
	writeFile(t, filepath.Join(done, "90-x.events"), "RUN-START id=90-x pid=1\nRUN-END rc=0 reason=ok\n")

	s := NewDeskSource(d).(QueueSource)
	if _, err := s.Queue(); err != nil { // the first scan fixes every hash
		t.Fatal(err)
	}
	var q QueueList
	var err error
	n := allocated(func() { q, err = s.Queue() })
	if err != nil || len(q.Items) != items {
		t.Fatalf("Queue = %d items, %v", len(q.Items), err)
	}
	t.Logf("Queue allocated %d MiB for %d MiB of items", n>>20, items*size>>20)
	if n > bound {
		t.Errorf("Queue allocated %d MiB, over the %d MiB bound", n>>20, bound>>20)
	}
	for _, it := range q.Items {
		if it.Body != nil || it.Marks != nil {
			t.Fatalf("Queue built %s's body", it.Name)
		}
	}

	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	n = allocated(func() { _, err = s.Load(RunRef{Source: "desk", Name: "90-x", Kind: KindDesk}, &Cursor{}) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Load allocated %d KiB", n>>10)
	if n > 4<<20 {
		t.Errorf("Load of one small run allocated %d MiB beside a large queue", n>>20)
	}

	// The one body the dialog opens on is built on demand, and checked.
	it, err := s.QueueBody(q.Items[0].Name, q.Items[0].SHA256)
	if err != nil || len(it.Body) != size || len(it.Marks) != size {
		t.Fatalf("QueueBody: %d lines, %d marks, %v", len(it.Body), len(it.Marks), err)
	}
	if _, err := s.QueueBody(q.Items[0].Name, q.Items[1].SHA256[:63]+"0"); err == nil {
		t.Error("QueueBody accepted a hash the bytes do not have")
	}
}
