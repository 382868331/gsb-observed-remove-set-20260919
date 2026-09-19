package orset

import (
	"math/rand"
	"sort"
	"testing"
)

// refSet is the uncompressed reference model: the causal context is a plain
// dot set, and merge uses the same observed-remove rule. The compressed
// Context and ORSet must agree with it exactly.
type refSet struct {
	ctx map[Dot]bool
	els map[string]map[Dot]bool
}

func newRefSet() *refSet {
	return &refSet{ctx: make(map[Dot]bool), els: make(map[string]map[Dot]bool)}
}

func (r *refSet) add(elem string, d Dot) {
	r.ctx[d] = true
	if r.els[elem] == nil {
		r.els[elem] = make(map[Dot]bool)
	}
	r.els[elem][d] = true
}

func (r *refSet) remove(elem string) { delete(r.els, elem) }

func (r *refSet) merge(o *refSet) {
	keep := make(map[string]map[Dot]bool)
	put := func(elem string, d Dot) {
		if keep[elem] == nil {
			keep[elem] = make(map[Dot]bool)
		}
		keep[elem][d] = true
	}
	for e, ds := range r.els {
		for d := range ds {
			if o.els[e][d] || !o.ctx[d] {
				put(e, d)
			}
		}
	}
	for e, ds := range o.els {
		for d := range ds {
			if r.els[e][d] {
				continue
			}
			if !r.ctx[d] {
				put(e, d)
			}
		}
	}
	for d := range o.ctx {
		r.ctx[d] = true
	}
	r.els = keep
}

func (r *refSet) clone() *refSet {
	out := newRefSet()
	for d := range r.ctx {
		out.ctx[d] = true
	}
	for e, ds := range r.els {
		out.els[e] = make(map[Dot]bool)
		for d := range ds {
			out.els[e][d] = true
		}
	}
	return out
}

// refFromORSet expands a compressed ORSet into the uncompressed model.
func refFromORSet(s *ORSet) *refSet {
	r := newRefSet()
	for id, e := range s.ctx.entries {
		for c := uint64(1); c <= e.max; c++ {
			r.ctx[Dot{Replica: id, Counter: c}] = true
		}
		for d := range e.dots {
			r.ctx[Dot{Replica: id, Counter: d}] = true
		}
	}
	for elem, dots := range s.els {
		r.els[elem] = make(map[Dot]bool)
		for d := range dots {
			r.els[elem][d] = true
		}
	}
	return r
}

func compareWithRef(t *testing.T, s *ORSet, r *refSet, ids []string, maxCounter uint64, label string) {
	t.Helper()
	// Context equivalence over every dot in range, including holes.
	for _, id := range ids {
		for c := uint64(1); c <= maxCounter+2; c++ {
			d := Dot{Replica: id, Counter: c}
			if s.ctx.Contains(d) != r.ctx[d] {
				t.Fatalf("%s: context mismatch at %v: compressed=%v reference=%v",
					label, d, s.ctx.Contains(d), r.ctx[d])
			}
		}
	}
	// Visible elements and their dots.
	want := make([]string, 0, len(r.els))
	for e, ds := range r.els {
		if len(ds) > 0 {
			want = append(want, e)
		}
	}
	sort.Strings(want)
	got := s.Elements()
	if len(got) != len(want) {
		t.Fatalf("%s: elements %v, reference %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: elements %v, reference %v", label, got, want)
		}
		for _, d := range s.Dots(want[i]) {
			if !r.els[want[i]][d] {
				t.Fatalf("%s: unexpected active dot %v for %q", label, d, want[i])
			}
		}
	}
}

// Randomized cross-check of the compressed implementation against the
// uncompressed reference: concurrent adds/removes, merges, snapshot
// round-trips, and out-of-order duplicate snapshot replay. Fixed seed,
// small sample.
func TestAgainstReferenceModel(t *testing.T) {
	rng := rand.New(rand.NewSource(20260919))
	ids := []string{"r1", "r2", "r3"}
	elems := []string{"apple", "banana", "cherry", "date"}
	sets := make(map[string]*ORSet)
	refs := make(map[string]*refSet)
	for _, id := range ids {
		sets[id] = mustNew(t, id)
		refs[id] = newRefSet()
	}
	counters := make(map[string]uint64)
	var snaps [][]byte // snapshot history for out-of-order replay

	for i := 0; i < 300; i++ {
		id := ids[rng.Intn(len(ids))]
		switch rng.Intn(6) {
		case 0, 1: // add
			e := elems[rng.Intn(len(elems))]
			d, err := sets[id].Add(e)
			if err != nil {
				t.Fatalf("step %d: Add: %v", i, err)
			}
			counters[id]++
			if d.Counter != counters[id] {
				t.Fatalf("step %d: dot counter %d, expected %d", i, d.Counter, counters[id])
			}
			refs[id].add(e, d)
		case 2: // remove
			e := elems[rng.Intn(len(elems))]
			sets[id].Remove(e)
			refs[id].remove(e)
		case 3: // merge a random peer into this replica
			other := ids[rng.Intn(len(ids))]
			if other == id {
				continue
			}
			if err := sets[id].Merge(sets[other].Clone()); err != nil {
				t.Fatalf("step %d: Merge: %v", i, err)
			}
			refs[id].merge(refs[other].clone())
		case 4: // snapshot + restore replaces the replica (must be identity)
			data := sets[id].Snapshot()
			snaps = append(snaps, data)
			restored, err := Restore(data)
			if err != nil {
				t.Fatalf("step %d: Restore: %v", i, err)
			}
			if !restored.Equal(sets[id]) {
				t.Fatalf("step %d: snapshot round-trip changed state", i)
			}
			sets[id] = restored
		case 5: // replay an old snapshot (possibly duplicated, out of order)
			if len(snaps) == 0 {
				continue
			}
			stale, err := Restore(snaps[rng.Intn(len(snaps))])
			if err != nil {
				t.Fatalf("step %d: Restore stale: %v", i, err)
			}
			if err := sets[id].Merge(stale); err != nil {
				t.Fatalf("step %d: Merge stale: %v", i, err)
			}
			refs[id].merge(refFromORSet(stale))
		}
		var maxC uint64
		for _, c := range counters {
			if c > maxC {
				maxC = c
			}
		}
		for _, rid := range ids {
			compareWithRef(t, sets[rid], refs[rid], ids, maxC, "random run")
		}
	}
}

// Merge must be commutative, associative, and idempotent, checked against
// independently generated divergent states.
func TestMergeLaws(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	elems := []string{"a", "b", "c", "d", "e"}

	randomDivergentStates := func(trial int) (a, b, c *ORSet) {
		// Common ancestor, then independent divergence per state.
		base := mustNew(t, "base")
		for i := 0; i < 5; i++ {
			mustAdd(t, base, elems[rng.Intn(len(elems))])
		}
		mk := func(id string) *ORSet {
			snap := base.Snapshot()
			s, err := Restore(snap)
			if err != nil {
				t.Fatal(err)
			}
			s.id = id // re-home the restored state under a fresh writer ID
			for i := 0; i < 4; i++ {
				switch rng.Intn(3) {
				case 0, 1:
					mustAdd(t, s, elems[rng.Intn(len(elems))])
				case 2:
					s.Remove(elems[rng.Intn(len(elems))])
				}
			}
			return s
		}
		return mk("la"), mk("lb"), mk("lc")
	}

	for trial := 0; trial < 20; trial++ {
		a, b, c := randomDivergentStates(trial)

		// Idempotence: merge(a, a) == a
		aa := a.Clone()
		if err := aa.Merge(a.Clone()); err != nil {
			t.Fatal(err)
		}
		if !aa.Equal(a) {
			t.Fatalf("trial %d: merge not idempotent", trial)
		}

		// Commutativity: merge(a, b) == merge(b, a)
		ab := a.Clone()
		if err := ab.Merge(b); err != nil {
			t.Fatal(err)
		}
		ba := b.Clone()
		if err := ba.Merge(a); err != nil {
			t.Fatal(err)
		}
		if !ab.Equal(ba) {
			t.Fatalf("trial %d: merge not commutative", trial)
		}

		// Associativity: merge(merge(a, b), c) == merge(a, merge(b, c))
		left := a.Clone()
		if err := left.Merge(b); err != nil {
			t.Fatal(err)
		}
		if err := left.Merge(c); err != nil {
			t.Fatal(err)
		}
		bc := b.Clone()
		if err := bc.Merge(c); err != nil {
			t.Fatal(err)
		}
		right := a.Clone()
		if err := right.Merge(bc); err != nil {
			t.Fatal(err)
		}
		if !left.Equal(right) {
			t.Fatalf("trial %d: merge not associative", trial)
		}
	}
}
