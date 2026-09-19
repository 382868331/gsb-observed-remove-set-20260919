package orset

import (
	"encoding/json"
	"math/rand"
	"testing"
)

// stateFromRef builds a fresh Set with exactly the ref's active dots and
// observed context. The owning counter is derived the same way Restore does.
func stateFromRef(t *testing.T, owner string, r *refSet) *Set {
	t.Helper()
	s, err := New(owner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for id, cs := range r.seen {
		for c := range cs {
			ctxAdd(s.ctx, Dot{Replica: id, Counter: c})
		}
	}
	for elem, dots := range r.active {
		m := map[Dot]struct{}{}
		for d := range dots {
			m[d] = struct{}{}
		}
		s.active[elem] = m
	}
	s.counter = 0
	for c := range r.seen[owner] {
		if c > s.counter {
			s.counter = c
		}
	}
	return s
}

func assertSetEqRef(t *testing.T, s *Set, r *refSet) {
	t.Helper()

	// Active dots.
	gotElems := map[string]map[Dot]bool{}
	for elem, dots := range s.active {
		if len(dots) == 0 {
			t.Errorf("empty active bucket for %q", elem)
		}
		m := map[Dot]bool{}
		for d := range dots {
			m[d] = true
		}
		gotElems[elem] = m
	}
	if len(gotElems) != len(r.active) {
		t.Errorf("element count mismatch: got %d want %d", len(gotElems), len(r.active))
	}
	for elem, want := range r.active {
		got := gotElems[elem]
		if len(got) != len(want) {
			t.Errorf("active dots for %q: got %v want %v", elem, got, want)
			continue
		}
		for d := range want {
			if !got[d] {
				t.Errorf("missing active dot %v for %q", d, elem)
			}
		}
		delete(gotElems, elem)
	}
	for elem := range gotElems {
		t.Errorf("unexpected active element %q", elem)
	}

	// Context membership, checked both directions over 1..largest counter.
	maxSeen := map[string]int{}
	for id, cs := range r.seen {
		for c := range cs {
			if c > maxSeen[id] {
				maxSeen[id] = c
			}
		}
	}
	for id, rc := range s.ctx {
		if (rc.prefix > 0 || len(rc.exc) > 0) && maxSeen[id] == 0 {
			t.Errorf("non-empty context for replica %q unknown to ref", id)
		}
	}
	for id, top := range maxSeen {
		rc := s.ctx[id]
		for c := 1; c <= top; c++ {
			d := Dot{Replica: id, Counter: c}
			want := r.seen[id][c]
			if rc == nil {
				if want {
					t.Errorf("context missing %v", d)
				}
				continue
			}
			if got := rc.contains(c); got != want {
				t.Errorf("context contains (%s,%d) = %v, want %v (prefix=%d exc=%v)",
					id, c, got, want, rc.prefix, rc.exc)
			}
		}
	}

	// Compression invariants: exceptions strictly above prefix and sorted,
	// and the largest counter never masks a hole (prefix truly contiguous).
	for id, rc := range s.ctx {
		prev := rc.prefix
		for _, c := range rc.exc {
			if c <= prev {
				t.Errorf("context for %q not compressed: prefix=%d exc=%v", id, rc.prefix, rc.exc)
			}
			prev = c
		}
	}

	if got := s.Elements(); !equalStrings(got, r.elements()) {
		t.Errorf("Elements = %v, want %v", got, r.elements())
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// normalizedJSON renders merge-relevant state (everything except owner id),
// so states owned by different replicas can be compared after merging.
func normalizedJSON(t *testing.T, s *Set) string {
	t.Helper()
	snap := s.Snapshot()
	snap.Replica = ""
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// partialSnapshot selects a random subset of a ref state: a sparse context
// plus only active dots still covered by that context. Restoring it is how
// out-of-order delivery creates context holes.
func partialSnapshot(t *testing.T, rng *rand.Rand, owner string, r *refSet) (*Set, *refSet) {
	t.Helper()
	seen := map[string]map[int]bool{}
	for id, cs := range r.seen {
		m := map[int]bool{}
		for c := range cs {
			if rng.Intn(2) == 0 {
				m[c] = true
			}
		}
		if len(m) > 0 {
			seen[id] = m
		}
	}
	active := map[string]map[Dot]bool{}
	for elem, dots := range r.active {
		m := map[Dot]bool{}
		for d := range dots {
			if seen[d.Replica][d.Counter] {
				m[d] = true
			}
		}
		if len(m) > 0 {
			active[elem] = m
		}
	}

	pr := &refSet{id: owner, active: active, seen: seen}
	s := stateFromRef(t, owner, pr)

	// Round-trip through canonical JSON exactly like a network snapshot.
	data, err := Marshal(s)
	if err != nil {
		t.Fatalf("marshal partial: %v", err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal partial: %v\njson: %s", err, data)
	}
	return got, pr
}

func TestRandomizedMatchesReference(t *testing.T) {
	const nReplicas = 4
	const iterations = 300
	rng := rand.New(rand.NewSource(20260920))
	elems := []string{"a", "b", "c", "d", "e"}

	ids := make([]string, nReplicas)
	sets := make([]*Set, nReplicas)
	refs := make([]*refSet, nReplicas)
	for i := range ids {
		ids[i] = "r" + string(rune('0'+i))
		sets[i], _ = New(ids[i])
		refs[i] = newRef(ids[i])
	}

	for iter := 0; iter < iterations; iter++ {
		k := rng.Intn(nReplicas)
		switch rng.Intn(10) {
		case 0, 1, 2: // local add
			e := elems[rng.Intn(len(elems))]
			if _, err := sets[k].Add(e); err != nil {
				t.Fatalf("iter %d Add: %v", iter, err)
			}
			refs[k].add(e)
		case 3: // local remove
			e := elems[rng.Intn(len(elems))]
			sets[k].Remove(e)
			refs[k].remove(e)
		case 4, 5, 6: // full-state gossip
			j := rng.Intn(nReplicas - 1)
			if j >= k {
				j++
			}
			if err := sets[k].Merge(sets[j]); err != nil {
				t.Fatalf("iter %d merge %d<- %d: %v", iter, k, j, err)
			}
			refs[k] = refMerge(refs[k], refs[j])
		case 7, 8: // sparse/out-of-order gossip -> context holes
			j := rng.Intn(nReplicas - 1)
			if j >= k {
				j++
			}
			ps, pr := partialSnapshot(t, rng, ids[j], refs[j])
			if err := sets[k].Merge(ps); err != nil {
				t.Fatalf("iter %d partial merge: %v", iter, err)
			}
			refs[k] = refMerge(refs[k], pr)
			// Repeated identical partial merge is idempotent.
			if err := sets[k].Merge(ps); err != nil {
				t.Fatalf("iter %d repeated partial merge: %v", iter, err)
			}
			refs[k] = refMerge(refs[k], pr)
		case 9: // crash recovery from snapshot
			data, err := Marshal(sets[k])
			if err != nil {
				t.Fatalf("iter %d marshal: %v", iter, err)
			}
			restored, err := Unmarshal(data)
			if err != nil {
				t.Fatalf("iter %d unmarshal: %v", iter, err)
			}
			sets[k] = restored
		}
		assertSetEqRef(t, sets[k], refs[k])

		// Periodically verify the algebra laws on random live states.
		if iter%25 == 24 {
			i, j, q := rng.Intn(nReplicas), rng.Intn(nReplicas), rng.Intn(nReplicas)
			checkLaws(t, sets[i], sets[j], sets[q], refs[i], refs[j], refs[q], ids[i])
		}
	}

	// Final full convergence: everyone merges everyone in shuffled order.
	for pass := 0; pass < 2; pass++ {
		order := rng.Perm(nReplicas)
		for _, k := range order {
			for _, j := range rng.Perm(nReplicas) {
				if err := sets[k].Merge(sets[j]); err != nil {
					t.Fatalf("convergence merge: %v", err)
				}
				refs[k] = refMerge(refs[k], refs[j])
			}
		}
	}
	for i := range sets {
		assertSetEqRef(t, sets[i], refs[i])
	}
	canon := normalizedJSON(t, sets[0])
	for i := 1; i < nReplicas; i++ {
		if got := normalizedJSON(t, sets[i]); got != canon {
			t.Fatalf("replica %d did not converge", i)
		}
	}
}

func checkLaws(t *testing.T, sa, sb, sc *Set, ra, rb, rc *refSet, owner string) {
	t.Helper()

	// Commutativity: a*b == b*a
	ab := stateFromRef(t, owner, ra)
	if err := ab.Merge(stateFromRef(t, owner, rb)); err != nil {
		t.Fatalf("law merge a*b: %v", err)
	}
	ba := stateFromRef(t, owner, rb)
	if err := ba.Merge(stateFromRef(t, owner, ra)); err != nil {
		t.Fatalf("law merge b*a: %v", err)
	}
	if normalizedJSON(t, ab) != normalizedJSON(t, ba) {
		t.Fatal("merge is not commutative")
	}
	assertSetEqRef(t, ab, refMerge(ra, rb))

	// Associativity: (a*b)*c == a*(b*c)
	abC := stateFromRef(t, owner, refMerge(ra, rb))
	if err := abC.Merge(stateFromRef(t, owner, rc)); err != nil {
		t.Fatalf("law merge (a*b)*c: %v", err)
	}
	aBC := stateFromRef(t, owner, ra)
	if err := aBC.Merge(stateFromRef(t, owner, refMerge(rb, rc))); err != nil {
		t.Fatalf("law merge a*(b*c): %v", err)
	}
	if normalizedJSON(t, abC) != normalizedJSON(t, aBC) {
		t.Fatal("merge is not associative")
	}
	assertSetEqRef(t, abC, refMerge(refMerge(ra, rb), rc))

	// Idempotence: a*a == a, including via an independent restored copy.
	aa := stateFromRef(t, owner, ra)
	if err := aa.Merge(stateFromRef(t, owner, ra)); err != nil {
		t.Fatalf("law merge a*a: %v", err)
	}
	if normalizedJSON(t, aa) != normalizedJSON(t, stateFromRef(t, owner, ra)) {
		t.Fatal("merge is not idempotent")
	}
	data, err := Marshal(aa)
	if err != nil {
		t.Fatalf("law marshal: %v", err)
	}
	self, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("law unmarshal: %v", err)
	}
	if err := aa.Merge(self); err != nil {
		t.Fatalf("law merge snapshot of self: %v", err)
	}
	assertSetEqRef(t, aa, ra)
}
