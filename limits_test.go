package orset

import (
	"errors"
	"fmt"
	"testing"
)

func TestInvalidReplicaID(t *testing.T) {
	for _, id := range []string{"", "with space", "slash/x", "中文", "a23456789012345678901234567890123"} {
		if _, err := New(id); !errors.Is(err, ErrInvalidReplica) {
			t.Errorf("New(%q): want ErrInvalidReplica, got %v", id, err)
		}
	}
	for _, id := range []string{"a", "r-1_2", "AZ09_-"} {
		if _, err := New(id); err != nil {
			t.Errorf("New(%q): unexpected error %v", id, err)
		}
	}
}

func TestCounterLimit(t *testing.T) {
	// Place the writer directly at the limit via a restored snapshot.
	snap := Snapshot{
		Version: 1,
		Replica: "r1",
		Context: []ContextEntry{{Replica: "r1", Prefix: MaxCounter}},
	}
	restored, err := Restore(snap)
	if err != nil {
		t.Fatalf("Restore at limit: %v", err)
	}
	if _, err := restored.Add("x"); !errors.Is(err, ErrCounterLimit) {
		t.Fatalf("want ErrCounterLimit, got %v", err)
	}
}

func TestMergeReplicaLimit(t *testing.T) {
	a := mustSet(t, "r0")
	b := mustSet(t, "r9")
	// a already observes MaxReplicas other replicas; b adds one more.
	for i := 0; i < MaxReplicas; i++ {
		ctxAdd(a.ctx, Dot{Replica: fmt.Sprintf("za%02d", i), Counter: 1})
	}
	ctxAdd(b.ctx, Dot{Replica: "zz-fresh", Counter: 1})

	before := normalizedJSON(t, a)
	err := a.Merge(b)
	if !errors.Is(err, ErrReplicaLimit) {
		t.Fatalf("want ErrReplicaLimit, got %v", err)
	}
	if after := normalizedJSON(t, a); after != before {
		t.Fatal("failed limit merge mutated receiver")
	}
}
