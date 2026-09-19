package orset

import (
	"bytes"
	"strings"
	"testing"
)

// Snapshot encoding is deterministic: same state, same bytes.
func TestSnapshotDeterministic(t *testing.T) {
	a := mustNew(t, "alpha")
	mustAdd(t, a, "x")
	mustAdd(t, a, "y")
	b := mustNew(t, "beta")
	mustAdd(t, b, "z")
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	first := a.Snapshot()
	second := a.Clone().Snapshot()
	if !bytes.Equal(first, second) {
		t.Fatalf("snapshot not deterministic:\n%s\n%s", first, second)
	}
	// Snapshotting must not change the state.
	before := a.Clone()
	a.Snapshot()
	if !a.Equal(before) {
		t.Fatal("Snapshot mutated the receiver")
	}
}

// Snapshot/restore round-trip preserves state, including context holes.
func TestSnapshotRoundTrip(t *testing.T) {
	a := mustNew(t, "alpha")
	mustAdd(t, a, "keep")
	mustAdd(t, a, "drop")
	a.Remove("drop")
	// Plant a hole: observed (beta,1) and (beta,4), missing 2 and 3.
	a.ctx.Add(Dot{Replica: "beta", Counter: 1})
	a.ctx.Add(Dot{Replica: "beta", Counter: 4})

	restored, err := Restore(a.Snapshot())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !restored.Equal(a) {
		t.Fatal("round-trip changed state")
	}
	if restored.ctx.Contains(Dot{Replica: "beta", Counter: 2}) {
		t.Fatal("context hole lost in round-trip")
	}
	if !restored.ctx.Contains(Dot{Replica: "beta", Counter: 4}) {
		t.Fatal("exception dot lost in round-trip")
	}
}

// After a legitimate restore, the next counter continues from the context
// maximum including exception dots, and never wraps.
func TestRestoreHighExceptionCounter(t *testing.T) {
	data := []byte(`{"id":"r1","context":[{"replica":"r1","max":2,"dots":[9999]}],"elements":[]}`)
	s, err := Restore(data)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	d, err := s.Add("x")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if d.Counter != 10000 {
		t.Fatalf("next counter = %d, want 10000 (exception max + 1)", d.Counter)
	}
	if _, err := s.Add("y"); err != ErrCounterExhausted {
		t.Fatalf("expected ErrCounterExhausted past the limit, got %v", err)
	}
}

// A visible dot may not map to two different elements.
func TestRestoreRejectsVisibleDotConflict(t *testing.T) {
	data := []byte(`{"id":"r1","context":[{"replica":"r1","max":1,"dots":[]}],"elements":[` +
		`{"element":"x","dots":[{"replica":"r1","counter":1}]},` +
		`{"element":"y","dots":[{"replica":"r1","counter":1}]}]}`)
	if _, err := Restore(data); err == nil {
		t.Fatal("snapshot with a visible dot on two elements was accepted")
	}
}

// Restore must reject malformed snapshots as a whole.
func TestRestoreRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"not JSON":                `{"id":`,
		"trailing data":           `{"id":"r1","context":[],"elements":[]} {}`,
		"unknown field":           `{"id":"r1","context":[],"elements":[],"extra":1}`,
		"empty replica ID":        `{"id":"","context":[],"elements":[]}`,
		"bad replica ID":          `{"id":"r 1","context":[],"elements":[]}`,
		"zero context max+dots":   `{"id":"r1","context":[{"replica":"r1","max":0,"dots":[]}],"elements":[]}`,
		"exception not above max": `{"id":"r1","context":[{"replica":"r1","max":3,"dots":[2]}],"elements":[]}`,
		"zero exception":          `{"id":"r1","context":[{"replica":"r1","max":0,"dots":[0]}],"elements":[]}`,
		"counter over limit":      `{"id":"r1","context":[{"replica":"r1","max":10001,"dots":[]}],"elements":[]}`,
		"unsorted context":        `{"id":"r1","context":[{"replica":"r2","max":1,"dots":[]},{"replica":"r1","max":1,"dots":[]}],"elements":[]}`,
		"duplicate context entry": `{"id":"r1","context":[{"replica":"r1","max":1,"dots":[]},{"replica":"r1","max":2,"dots":[]}],"elements":[]}`,
		"unsorted exceptions":     `{"id":"r1","context":[{"replica":"r1","max":1,"dots":[5,3]}],"elements":[]}`,
		"dot not in context":      `{"id":"r1","context":[{"replica":"r1","max":1,"dots":[]}],"elements":[{"element":"x","dots":[{"replica":"r1","counter":2}]}]}`,
		"zero dot counter":        `{"id":"r1","context":[{"replica":"r1","max":1,"dots":[]}],"elements":[{"element":"x","dots":[{"replica":"r1","counter":0}]}]}`,
		"dot counter over limit":  `{"id":"r1","context":[{"replica":"r1","max":1,"dots":[]}],"elements":[{"element":"x","dots":[{"replica":"r1","counter":10001}]}]}`,
		"element without dots":    `{"id":"r1","context":[],"elements":[{"element":"x","dots":[]}]}`,
		"unsorted elements":       `{"id":"r1","context":[{"replica":"r1","max":2,"dots":[]}],"elements":[{"element":"y","dots":[{"replica":"r1","counter":1}]},{"element":"x","dots":[{"replica":"r1","counter":2}]}]}`,
		"unsorted element dots":   `{"id":"r1","context":[{"replica":"r1","max":2,"dots":[]}],"elements":[{"element":"x","dots":[{"replica":"r1","counter":2},{"replica":"r1","counter":1}]}]}`,
		"bad dot replica ID":      `{"id":"r1","context":[{"replica":"r1","max":1,"dots":[]}],"elements":[{"element":"x","dots":[{"replica":"r 1","counter":1}]}]}`,
	}
	for name, input := range cases {
		if _, err := Restore([]byte(input)); err == nil {
			t.Fatalf("%s: invalid snapshot accepted", name)
		}
	}

	// More than MaxReplicas distinct replicas in the context.
	var sb strings.Builder
	sb.WriteString(`{"id":"r1","context":[`)
	for i := 0; i < MaxReplicas+1; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"replica":"r`)
		sb.WriteString(strings.Repeat("x", i+1)) // distinct, sorted IDs
		sb.WriteString(`","max":1,"dots":[]}`)
	}
	sb.WriteString(`],"elements":[]}`)
	if _, err := Restore([]byte(sb.String())); err != ErrTooManyReplicas {
		t.Fatalf("expected ErrTooManyReplicas, got %v", err)
	}
}

// Restore must not modify the input bytes.
func TestRestoreDoesNotMutateInput(t *testing.T) {
	a := mustNew(t, "alpha")
	mustAdd(t, a, "x")
	data := a.Snapshot()
	before := append([]byte(nil), data...)
	if _, err := Restore(data); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !bytes.Equal(data, before) {
		t.Fatal("Restore modified its input")
	}
}

// Restoring an out-of-order duplicate of an old snapshot and merging it
// must not resurrect removed elements (end-to-end via JSON snapshots).
func TestStaleSnapshotReplay(t *testing.T) {
	a := mustNew(t, "alpha")
	b := mustNew(t, "beta")
	mustAdd(t, a, "victim")
	staleSnap := a.Snapshot() // captured before the remove propagates

	if err := b.Merge(a); err != nil {
		t.Fatal(err)
	}
	b.Remove("victim")
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	// Replay the stale snapshot twice, out of order, into both replicas.
	for i := 0; i < 2; i++ {
		stale, err := Restore(staleSnap)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Merge(stale); err != nil {
			t.Fatal(err)
		}
		if err := a.Merge(stale); err != nil {
			t.Fatal(err)
		}
	}
	if a.Contains("victim") || b.Contains("victim") {
		t.Fatalf("stale snapshot replay resurrected a removed element: a=%v b=%v",
			a.Elements(), b.Elements())
	}
}
