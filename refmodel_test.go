package orset

// Uncompressed reference model used by the property tests: causal context
// is a plain per-replica set of observed counters, with no prefix/exception
// compression. Merge semantics mirror the specification line by line.

type refSet struct {
	id     string
	active map[string]map[Dot]bool // element -> dots keeping it visible
	seen   map[string]map[int]bool // replica -> observed counters
}

func newRef(id string) *refSet {
	return &refSet{
		id:     id,
		active: map[string]map[Dot]bool{},
		seen:   map[string]map[int]bool{},
	}
}

func (r *refSet) nextCounter() int {
	max := 0
	for c := range r.seen[r.id] {
		if c > max {
			max = c
		}
	}
	return max + 1
}

func (r *refSet) observe(d Dot) {
	m := r.seen[d.Replica]
	if m == nil {
		m = map[int]bool{}
		r.seen[d.Replica] = m
	}
	m[d.Counter] = true
}

func (r *refSet) add(elem string) Dot {
	d := Dot{Replica: r.id, Counter: r.nextCounter()}
	r.observe(d)
	m := r.active[elem]
	if m == nil {
		m = map[Dot]bool{}
		r.active[elem] = m
	}
	m[d] = true
	return d
}

func (r *refSet) remove(elem string) { delete(r.active, elem) }

func (r *refSet) contains(elem string) bool { return len(r.active[elem]) > 0 }

func (r *refSet) elements() []string {
	out := []string{}
	for e, dots := range r.active {
		if len(dots) > 0 {
			out = append(out, e)
		}
	}
	sortStrings(out)
	return out
}

func (r *refSet) clone() *refSet {
	out := newRef(r.id)
	for e, dots := range r.active {
		m := make(map[Dot]bool, len(dots))
		for d := range dots {
			m[d] = true
		}
		out.active[e] = m
	}
	for id, cs := range r.seen {
		m := make(map[int]bool, len(cs))
		for c := range cs {
			m[c] = true
		}
		out.seen[id] = m
	}
	return out
}

// refMerge returns a fresh refSet equal to a merged with b.
func refMerge(a, b *refSet) *refSet {
	out := newRef(a.id)
	for id, cs := range a.seen {
		out.seen[id] = map[int]bool{}
		for c := range cs {
			out.seen[id][c] = true
		}
	}
	for id, cs := range b.seen {
		if out.seen[id] == nil {
			out.seen[id] = map[int]bool{}
		}
		for c := range cs {
			out.seen[id][c] = true
		}
	}

	aDots, bDots := refIndex(a), refIndex(b)
	put := func(elem string, d Dot) {
		m := out.active[elem]
		if m == nil {
			m = map[Dot]bool{}
			out.active[elem] = m
		}
		m[d] = true
	}
	for d, elem := range aDots {
		if _, ok := bDots[d]; ok {
			put(elem, d)
			continue
		}
		if !b.seen[d.Replica][d.Counter] {
			put(elem, d)
		}
	}
	for d, elem := range bDots {
		if _, ok := aDots[d]; ok {
			continue
		}
		if !a.seen[d.Replica][d.Counter] {
			put(elem, d)
		}
	}
	return out
}

func refIndex(r *refSet) map[Dot]string {
	idx := map[Dot]string{}
	for elem, dots := range r.active {
		for d := range dots {
			idx[d] = elem
		}
	}
	return idx
}
