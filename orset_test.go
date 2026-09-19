package orset

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func mustSet(t *testing.T, id string) *Set {
	t.Helper()
	s, err := New(id)
	if err != nil {
		t.Fatalf("New(%q): %v", id, err)
	}
	return s
}

func TestNewAndBasicAddContains(t *testing.T) {
	a := mustSet(t, "r1")
	if a.Contains("x") {
		t.Fatal("empty set should not contain x")
	}
	d1, err := a.Add("x")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	d2, err := a.Add("x")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if d1.Replica != "r1" || d1.Counter != 1 || d2.Counter != 2 {
		t.Fatalf("unexpected dots: %v %v", d1, d2)
	}
	if !a.Contains("x") || !reflect.DeepEqual(a.Elements(), []string{"x"}) {
		t.Fatal("x should be present")
	}
}

// Concurrent Add vs Remove: the removal happened before the add was
// observed, so the concurrent add must survive the merge.
func TestConcurrentAddAndRemove(t *testing.T) {
	a := mustSet(t, "r1")
	b := mustSet(t, "r2")

	if _, err := a.Add("shared"); err != nil {
		t.Fatal(err)
	}
	// b learns the element, then the replicas diverge.
	if err := b.Merge(a); err != nil {
		t.Fatal(err)
	}
	old := a.Clone() // snapshot taken before the concurrent operations

	b.Remove("shared")                         // observed removal
	if _, err := a.Add("shared"); err != nil { // concurrent add on a
		t.Fatal(err)
	}

	// Both delivery orders must converge to the same result.
	x := b.Clone()
	y := a.Clone()
	if err := x.Merge(a); err != nil {
		t.Fatal(err)
	}
	if err := y.Merge(b); err != nil {
		t.Fatal(err)
	}
	if !x.Contains("shared") {
		t.Error("concurrent add was removed: merge b<-a lost it")
	}
	if !y.Contains("shared") {
		t.Error("concurrent add was removed: merge a<-b lost it")
	}
	if !reflect.DeepEqual(x.Elements(), y.Elements()) {
		t.Errorf("delivery orders diverged: %v vs %v", x.Elements(), y.Elements())
	}

	// Even if b also gets the stale pre-divergence snapshot again, the
	// observed deletion must not resurrect.
	if err := b.Merge(old); err != nil {
		t.Fatal(err)
	}
	if b.Contains("shared") {
		t.Error("old state retransmission resurrected removed element on b")
	}
}

// Observed removal: once both replicas have seen a dot, removing it and
// merging either direction makes the element stay gone.
func TestObservedRemoval(t *testing.T) {
	a := mustSet(t, "r1")
	b := mustSet(t, "r2")
	if _, err := a.Add("doc"); err != nil {
		t.Fatal(err)
	}
	if err := b.Merge(a); err != nil {
		t.Fatal(err)
	}
	b.Remove("doc")
	if b.Contains("doc") {
		t.Fatal("local observed remove failed")
	}
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if a.Contains("doc") {
		t.Error("observed removal did not propagate to a")
	}
	if err := b.Merge(a); err != nil {
		t.Fatal(err)
	}
	if b.Contains("doc") {
		t.Error("re-merge resurrected removed element")
	}
}

// Old state retransmission: a deleted dot never comes back when an ancient
// snapshot (still carrying the dot as active) is merged repeatedly.
func TestOldStateRetransmission(t *testing.T) {
	a := mustSet(t, "r1")
	b := mustSet(t, "r2")
	if _, err := a.Add("tag"); err != nil {
		t.Fatal(err)
	}
	if err := b.Merge(a); err != nil {
		t.Fatal(err)
	}
	old := a.Clone()

	a.Remove("tag")
	if err := b.Merge(a); err != nil {
		t.Fatal(err)
	}
	if b.Contains("tag") {
		t.Fatal("remove not observed")
	}
	for i := 0; i < 3; i++ {
		if err := b.Merge(old); err != nil {
			t.Fatal(err)
		}
		if b.Contains("tag") {
			t.Fatalf("old state resurrected tag on delivery %d", i+1)
		}
	}
}

// Context holes: observing counters out of order (5, then 3) must not let
// the maximum counter swallow the unobserved gaps. Also verifies the merge
// keeps an add whose dot sits in the other side's hole.
func TestContextHole(t *testing.T) {
	s := mustSet(t, "r1")
	remote, _ := New("rx")
	// Observe rx:5 first as context only, no active dot.
	ctxAdd(s.ctx, Dot{Replica: "rx", Counter: 5})
	rc := s.ctx["rx"]
	if rc.prefix != 0 || !reflect.DeepEqual(rc.exc, []int{5}) {
		t.Fatalf("hole not compressed as exception: prefix=%d exc=%v", rc.prefix, rc.exc)
	}
	for c := 1; c <= 5; c++ {
		got := rc.contains(c)
		want := c == 5
		if got != want {
			t.Errorf("contains %d = %v, want %v (max counter must not swallow holes)", c, got, want)
		}
	}

	// rx:3 arrives later as an active dot; it must be accepted and kept,
	// while gaps 1,2,4 remain unobserved.
	ctxAdd(remote.ctx, Dot{Replica: "rx", Counter: 3})
	remote.active["later"] = map[Dot]struct{}{{Replica: "rx", Counter: 3}: {}}
	if err := s.Merge(remote); err != nil {
		t.Fatal(err)
	}
	if !s.Contains("later") {
		t.Error("active dot inside a context hole was dropped")
	}
	rc = s.ctx["rx"] // merge replaces the context map; re-acquire it
	for _, c := range []int{1, 2, 4} {
		if rc.contains(c) {
			t.Errorf("hole %d was swallowed", c)
		}
	}
	if !rc.contains(3) || !rc.contains(5) {
		t.Error("observed counters lost after merge")
	}

	// Filling 1 and 2 must extend the prefix without claiming 4.
	ctxAdd(s.ctx, Dot{Replica: "rx", Counter: 1})
	ctxAdd(s.ctx, Dot{Replica: "rx", Counter: 2})
	rc = s.ctx["rx"]
	if rc.prefix != 3 || !reflect.DeepEqual(rc.exc, []int{5}) {
		t.Fatalf("prefix extension wrong: prefix=%d exc=%v", rc.prefix, rc.exc)
	}
	if rc.contains(4) {
		t.Error("counter 4 swallowed after prefix extension")
	}
}

// Exception-point high-counter recovery: after snapshots containing sparse
// high counters, restore derives the writer counter as the max observed
// counter INCLUDING exception points, and never wraps backwards.
func TestExceptionHighCounterRecovery(t *testing.T) {
	s := mustSet(t, "r1")
	// Directly construct a sparse history: observed 1..2 and 100, dot 100
	// keeps "high" alive.
	for _, c := range []int{1, 2, 100} {
		ctxAdd(s.ctx, Dot{Replica: "r1", Counter: c})
	}
	s.counter = 100
	s.active["high"] = map[Dot]struct{}{{Replica: "r1", Counter: 100}: {}}

	data, err := Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	d, err := got.Add("after-restore")
	if err != nil {
		t.Fatalf("Add after restore: %v", err)
	}
	if d.Counter != 101 {
		t.Fatalf("next counter = %d, want 101 (exceptions must count)", d.Counter)
	}
	if !got.Contains("high") {
		t.Error("exception-point active dot lost across snapshot")
	}
	// Hole 3..99 must stay unobserved.
	for c := 3; c <= 99; c++ {
		if got.ctx["r1"].contains(c) {
			t.Fatalf("hole %d swallowed after restore", c)
		}
	}
}

// Visible dot conflict: the same dot active under two different elements is
// rejected both on merge and on snapshot restore, without mutating state.
func TestVisibleDotConflict(t *testing.T) {
	a := mustSet(t, "r1")
	b := mustSet(t, "r2")
	d := Dot{Replica: "r1", Counter: 1}
	ctxAdd(a.ctx, d)
	a.active["alpha"] = map[Dot]struct{}{d: {}}
	ctxAdd(b.ctx, d)
	b.active["beta"] = map[Dot]struct{}{d: {}}

	before := normalizedJSON(t, a)
	err := a.Merge(b)
	if !errors.Is(err, ErrDotConflict) {
		t.Fatalf("want ErrDotConflict, got %v", err)
	}
	if after := normalizedJSON(t, a); after != before {
		t.Fatal("failed merge must not mutate the receiver")
	}
	if a.Contains("beta") {
		t.Fatal("conflicting element leaked in after failed merge")
	}

	// Same conflict delivered through a snapshot: rejected as a whole.
	snap := Snapshot{
		Version: 1,
		Replica: "r2",
		Active: []ActiveElement{
			{Element: "alpha", Dots: []Dot{d}},
			{Element: "beta", Dots: []Dot{d}},
		},
		Context: []ContextEntry{{Replica: "r1", Prefix: 1}, {Replica: "r2", Prefix: 0}},
	}
	if _, err := Restore(snap); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("conflicting snapshot: want ErrInvalidSnapshot, got %v", err)
	}
	data, _ := json.Marshal(snap)
	if _, err := Unmarshal(data); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("conflicting JSON: want ErrInvalidSnapshot, got %v", err)
	}
}

// Inputs must not be modified: Merge leaves the other set untouched, and
// snapshot round trips never mutate the supplied value or bytes.
func TestInputsNotMutated(t *testing.T) {
	a := mustSet(t, "r1")
	b := mustSet(t, "r2")
	if _, err := a.Add("x"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Add("y"); err != nil {
		t.Fatal(err)
	}
	b.Remove("y")
	bSnap, err := json.Marshal(b.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(b.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bSnap, after) {
		t.Errorf("Merge mutated the other set:\nbefore %s\nafter  %s", bSnap, after)
	}

	// Restore must not retain or mutate the passed Snapshot.
	snap := Snapshot{
		Version: 1,
		Replica: "r1",
		Active: []ActiveElement{
			{Element: "z", Dots: []Dot{{Replica: "r1", Counter: 7}}},
		},
		Context: []ContextEntry{{Replica: "r1", Prefix: 0, Exceptions: []int{7}}},
	}
	snapJSON, _ := json.Marshal(snap)
	s, err := Restore(snap)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// Mutating the restored set must not affect the snapshot value.
	if _, err := s.Add("w"); err != nil {
		t.Fatal(err)
	}
	s.Remove("z")
	again, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapJSON, again) {
		t.Error("Restore retained a reference to the input Snapshot")
	}

	// Unmarshal must not modify the input byte slice.
	raw := []byte(strings.TrimSpace(string(snapJSON)))
	orig := append([]byte(nil), raw...)
	if _, err := Unmarshal(raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !bytes.Equal(raw, orig) {
		t.Error("Unmarshal mutated the input bytes")
	}
}

func TestSnapshotValidation(t *testing.T) {
	base := func() Snapshot {
		return Snapshot{
			Version: 1,
			Replica: "r1",
			Active: []ActiveElement{
				{Element: "e", Dots: []Dot{{Replica: "r1", Counter: 1}}},
			},
			Context: []ContextEntry{{Replica: "r1", Prefix: 1}},
		}
	}
	cases := []struct {
		name string
		mut  func(*Snapshot)
	}{
		{"bad version", func(s *Snapshot) { s.Version = 2 }},
		{"empty owner id", func(s *Snapshot) { s.Replica = "" }},
		{"illegal owner id", func(s *Snapshot) { s.Replica = "bad/id" }},
		{"illegal dot id", func(s *Snapshot) { s.Active[0].Dots[0].Replica = "r 1" }},
		{"zero counter", func(s *Snapshot) {
			s.Active[0].Dots[0].Counter = 0
			s.Context[0].Prefix = 0
		}},
		{"counter over limit", func(s *Snapshot) {
			s.Active[0].Dots[0].Counter = MaxCounter + 1
			s.Context[0].Prefix = MaxCounter + 1
		}},
		{"active dot outside context", func(s *Snapshot) { s.Context = nil }},
		{"active dot in context hole", func(s *Snapshot) {
			s.Context[0].Prefix = 0
			s.Context[0].Exceptions = []int{2}
			s.Active[0].Dots[0].Counter = 1
		}},
		{"unsorted exceptions", func(s *Snapshot) {
			s.Context[0].Exceptions = []int{5, 3}
		}},
		{"exception inside prefix", func(s *Snapshot) {
			s.Context[0].Exceptions = []int{1}
		}},
		{"duplicate context replica", func(s *Snapshot) {
			s.Context = append(s.Context, ContextEntry{Replica: "r1", Prefix: 1})
		}},
		{"garbage json", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "garbage json" {
				if _, err := Unmarshal([]byte("{not json")); !errors.Is(err, ErrInvalidSnapshot) {
					t.Fatalf("got %v", err)
				}
				return
			}
			snap := base()
			tc.mut(&snap)
			if _, err := Restore(snap); !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("want ErrInvalidSnapshot, got %v", err)
			}
		})
	}

	// Replica limit.
	snap := base()
	for i := 0; i < MaxReplicas; i++ {
		id := "x" + string(rune('a'+i/10)) + string(rune('a'+i%10))
		snap.Context = append(snap.Context, ContextEntry{Replica: id, Prefix: 0})
	}
	// base already has r1 plus MaxReplicas extras => over the limit.
	if _, err := Restore(snap); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("replica limit: want ErrInvalidSnapshot, got %v", err)
	}
}

func TestCanonicalEncodingIsDeterministic(t *testing.T) {
	a := mustSet(t, "r1")
	b := mustSet(t, "r2")
	for _, e := range []string{"zeta", "alpha", "m"} {
		if _, err := a.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	ctxAdd(b.ctx, Dot{Replica: "r1", Counter: 9}) // hole stays an exception
	if err := b.Merge(a); err != nil {
		t.Fatal(err)
	}
	one, err := Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, two) {
		t.Fatal("marshalling is not deterministic")
	}
	// JSON arrays must appear sorted: elements and dots.
	if !strings.Contains(string(one), `"element": "alpha"`) {
		t.Fatalf("encoding not sorted as expected:\n%s", one)
	}
	restored, err := Unmarshal(one)
	if err != nil {
		t.Fatal(err)
	}
	three, err := Marshal(restored)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, three) {
		t.Fatal("snapshot round trip changed encoding")
	}
}
