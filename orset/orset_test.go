package orset

import (
	"testing"
)

func mustNew(t *testing.T, id string) *ORSet {
	t.Helper()
	s, err := New(id)
	if err != nil {
		t.Fatalf("New(%q): %v", id, err)
	}
	return s
}

func mustAdd(t *testing.T, s *ORSet, elem string) Dot {
	t.Helper()
	d, err := s.Add(elem)
	if err != nil {
		t.Fatalf("Add(%q): %v", elem, err)
	}
	return d
}

// Concurrent Add and Remove: a remove that has not observed the add must not
// kill it.
func TestConcurrentAddAndRemove(t *testing.T) {
	a := mustNew(t, "alpha")
	b := mustNew(t, "beta")

	mustAdd(t, a, "milk") // alpha observes its own add
	if err := b.Merge(a); err != nil {
		t.Fatal(err)
	}
	b.Remove("milk") // beta removes what it observed

	// Concurrently alpha adds "milk" again (beta has not seen this dot).
	mustAdd(t, a, "milk")

	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if err := b.Merge(a); err != nil {
		t.Fatal(err)
	}
	if !a.Contains("milk") || !b.Contains("milk") {
		t.Fatalf("concurrent add lost: a=%v b=%v", a.Elements(), b.Elements())
	}
	if !a.Equal(b) {
		t.Fatalf("replicas diverged: a=%v b=%v", a.Elements(), b.Elements())
	}
}

// Remove after observing deletes the element for everyone.
func TestObservedRemove(t *testing.T) {
	a := mustNew(t, "alpha")
	b := mustNew(t, "beta")

	mustAdd(t, a, "eggs")
	if err := b.Merge(a); err != nil {
		t.Fatal(err)
	}
	b.Remove("eggs")
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if a.Contains("eggs") || b.Contains("eggs") {
		t.Fatalf("observed remove did not stick: a=%v b=%v", a.Elements(), b.Elements())
	}
}

// Retransmission of an old state must not resurrect a deleted element.
func TestOldStateRetransmission(t *testing.T) {
	a := mustNew(t, "alpha")
	b := mustNew(t, "beta")

	mustAdd(t, a, "bread")
	stale := a.Clone() // old state captured before the remove

	if err := b.Merge(a); err != nil {
		t.Fatal(err)
	}
	b.Remove("bread")
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if a.Contains("bread") {
		t.Fatal("remove lost before retransmission")
	}
	// The stale state is replayed into b; the delete must survive.
	if err := b.Merge(stale); err != nil {
		t.Fatal(err)
	}
	if b.Contains("bread") {
		t.Fatal("deleted element resurrected by old state retransmission")
	}
	// And converging back keeps it gone.
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if a.Contains("bread") {
		t.Fatal("deleted element resurrected after re-convergence")
	}
}

// Context holes: exceptions above the prefix must not be swallowed by max,
// and merge must not treat holed dots as observed.
func TestContextHoles(t *testing.T) {
	ctx := newContext()
	ctx.Add(Dot{Replica: "r1", Counter: 1})
	ctx.Add(Dot{Replica: "r1", Counter: 2})
	ctx.Add(Dot{Replica: "r1", Counter: 5}) // hole at 3,4
	ctx.Add(Dot{Replica: "r1", Counter: 7}) // hole at 6

	for _, c := range []uint64{1, 2, 5, 7} {
		if !ctx.Contains(Dot{Replica: "r1", Counter: c}) {
			t.Fatalf("expected dot %d observed", c)
		}
	}
	for _, c := range []uint64{3, 4, 6, 8} {
		if ctx.Contains(Dot{Replica: "r1", Counter: c}) {
			t.Fatalf("hole %d wrongly observed", c)
		}
	}
	if got := ctx.Max("r1"); got != 7 {
		t.Fatalf("Max = %d, want 7 (exceptions included)", got)
	}

	// Filling the gaps from another replica's context compacts the prefix.
	other := newContext()
	other.Add(Dot{Replica: "r1", Counter: 3})
	other.Add(Dot{Replica: "r1", Counter: 4})
	other.Add(Dot{Replica: "r1", Counter: 6})
	u := ctx.Union(other)
	for c := uint64(1); c <= 7; c++ {
		if !u.Contains(Dot{Replica: "r1", Counter: c}) {
			t.Fatalf("union missing dot %d", c)
		}
	}
	e := u.entries["r1"]
	if e.max != 7 || len(e.dots) != 0 {
		t.Fatalf("union did not compact: max=%d dots=%v", e.max, e.dots)
	}

	// A dot active only on one side and sitting in the other side's hole
	// must survive the merge.
	a := mustNew(t, "aa")
	b := mustNew(t, "bb")
	// Manually plant a holed context on b for replica "aa": observed 1..2,
	// hole at 3, exception at 5.
	b.ctx.Add(Dot{Replica: "aa", Counter: 1})
	b.ctx.Add(Dot{Replica: "aa", Counter: 2})
	b.ctx.Add(Dot{Replica: "aa", Counter: 5})
	// a holds dot (aa,3) for "x".
	a.ctx.Add(Dot{Replica: "aa", Counter: 3})
	a.els["x"] = map[Dot]struct{}{{Replica: "aa", Counter: 3}: {}}
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if !a.Contains("x") {
		t.Fatal("dot in the other side's context hole was wrongly dropped")
	}
}

// Merge is idempotent and convergent for a two-replica exchange.
func TestMergeConvergenceAndIdempotence(t *testing.T) {
	a := mustNew(t, "alpha")
	b := mustNew(t, "beta")
	mustAdd(t, a, "x")
	mustAdd(t, a, "y")
	mustAdd(t, b, "z")

	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if err := b.Merge(a); err != nil {
		t.Fatal(err)
	}
	if !a.Equal(b) {
		t.Fatal("replicas did not converge")
	}
	before := a.Clone()
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if !a.Equal(before) {
		t.Fatal("merge not idempotent")
	}
}

// Merge must not mutate its argument.
func TestMergeDoesNotMutateArgument(t *testing.T) {
	a := mustNew(t, "alpha")
	b := mustNew(t, "beta")
	mustAdd(t, a, "x")
	mustAdd(t, b, "y")
	b.Remove("y")

	snapBefore := b.Snapshot()
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if string(b.Snapshot()) != string(snapBefore) {
		t.Fatal("Merge mutated its argument")
	}
}

// Add refuses to wrap the counter past MaxCounter.
func TestCounterExhaustion(t *testing.T) {
	s := mustNew(t, "alpha")
	s.ctx.Add(Dot{Replica: "alpha", Counter: MaxCounter})
	if _, err := s.Add("x"); err != ErrCounterExhausted {
		t.Fatalf("expected ErrCounterExhausted, got %v", err)
	}
}

func TestInvalidReplicaID(t *testing.T) {
	for _, id := range []string{"", "has space", "has/slash", "这个不行"} {
		if _, err := New(id); err == nil {
			t.Fatalf("New(%q) unexpectedly accepted", id)
		}
	}
}
