// Package orset implements a state-based observed-remove set (OR-Set)
// with a compressed causal context.
//
// Elements are strings. Every Add mints a unique dot (replica id, counter).
// The state records, per element, the set of currently active dots, plus the
// causal context of every dot this replica has observed. Merge keeps a dot
// when both sides show it active, or when only one side does and the other
// side has never observed it; therefore an observed deletion wins over an
// old retransmitted state while a concurrent add survives.
package orset

import (
	"errors"
	"fmt"
	"sort"
)

// Limits mandated by the specification.
const (
	MaxReplicas = 20
	MaxCounter  = 10000
	maxIDLen    = 32
)

// Errors returned by the library. Callers can use errors.Is to match them.
var (
	ErrInvalidReplica  = errors.New("orset: invalid replica id")
	ErrCounterLimit    = errors.New("orset: per-replica counter limit reached")
	ErrReplicaLimit    = errors.New("orset: replica limit reached")
	ErrDotConflict     = errors.New("orset: active dot maps to different elements")
	ErrInvalidSnapshot = errors.New("orset: invalid snapshot")
)

// Dot is the unique identity of one Add operation: a replica id paired with
// that replica's positive, strictly increasing counter.
type Dot struct {
	Replica string `json:"replica"`
	Counter int    `json:"counter"`
}

// Set is an in-memory observed-remove set. A Set is not safe for concurrent
// use by multiple goroutines; each replica id has exactly one writer.
type Set struct {
	id      string
	counter int // last counter used by this replica; next Add uses counter+1
	// element -> active dots. Every active dot is contained in ctx.
	active map[string]map[Dot]struct{}
	// replica id -> compressed observed context
	ctx map[string]*replicaCtx
}

// replicaCtx is the compressed causal context for one replica:
// counters 1..prefix observed contiguously, plus sorted discrete exception
// counters strictly greater than prefix. Holes are represented explicitly
// and are never swallowed by the largest observed counter.
type replicaCtx struct {
	prefix int
	exc    []int
}

func (r *replicaCtx) contains(counter int) bool {
	if counter <= r.prefix {
		return counter >= 1
	}
	i := sort.SearchInts(r.exc, counter)
	return i < len(r.exc) && r.exc[i] == counter
}

// add records an observed counter, extending the contiguous prefix whenever
// the next counter (or a run of queued exceptions) closes the gap.
func (r *replicaCtx) add(counter int) {
	if counter <= r.prefix {
		return
	}
	if counter == r.prefix+1 {
		r.prefix++
		for len(r.exc) > 0 && r.exc[0] == r.prefix+1 {
			r.prefix++
			r.exc = r.exc[1:]
		}
		return
	}
	i := sort.SearchInts(r.exc, counter)
	if i < len(r.exc) && r.exc[i] == counter {
		return
	}
	r.exc = append(r.exc, 0)
	copy(r.exc[i+1:], r.exc[i:])
	r.exc[i] = counter
}

// unionR returns the compressed union of two per-replica contexts. The
// result never aliases the input exception slices.
func unionR(a, b *replicaCtx) *replicaCtx {
	out := &replicaCtx{}
	if a != nil {
		out.prefix = a.prefix
	}
	if b != nil && b.prefix > out.prefix {
		out.prefix = b.prefix
	}
	seen := map[int]struct{}{}
	if a != nil {
		for _, c := range a.exc {
			if c > out.prefix {
				seen[c] = struct{}{}
			}
		}
	}
	if b != nil {
		for _, c := range b.exc {
			if c > out.prefix {
				seen[c] = struct{}{}
			}
		}
	}
	cand := make([]int, 0, len(seen))
	for c := range seen {
		cand = append(cand, c)
	}
	sort.Ints(cand)
	for _, c := range cand {
		if c == out.prefix+1 {
			out.prefix++
		} else {
			out.exc = append(out.exc, c)
		}
	}
	return out
}

func ctxContains(m map[string]*replicaCtx, d Dot) bool {
	if r := m[d.Replica]; r != nil {
		return r.contains(d.Counter)
	}
	return false
}

func ctxAdd(m map[string]*replicaCtx, d Dot) {
	r := m[d.Replica]
	if r == nil {
		r = &replicaCtx{}
		m[d.Replica] = r
	}
	r.add(d.Counter)
}

// ctxUnion returns a fresh merged context map. Inputs are never mutated.
func ctxUnion(a, b map[string]*replicaCtx) map[string]*replicaCtx {
	out := make(map[string]*replicaCtx, len(a)+len(b))
	for id, r := range a {
		out[id] = unionR(r, b[id])
	}
	for id, r := range b {
		if _, ok := out[id]; !ok {
			out[id] = unionR(nil, r)
		}
	}
	return out
}

func unionReplicaCount(a, b map[string]*replicaCtx) int {
	n := len(a)
	for id := range b {
		if _, ok := a[id]; !ok {
			n++
		}
	}
	return n
}

// New creates an empty set owned by replicaID. The id must be 1..32 bytes of
// [A-Za-z0-9_-].
func New(replicaID string) (*Set, error) {
	if !validID(replicaID) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidReplica, replicaID)
	}
	return &Set{
		id:     replicaID,
		active: map[string]map[Dot]struct{}{},
		ctx:    map[string]*replicaCtx{},
	}, nil
}

// ReplicaID returns the writer id of this set.
func (s *Set) ReplicaID() string { return s.id }

// Add appends element to the set, minting and returning the new dot. The dot
// is immediately recorded in this replica's causal context.
func (s *Set) Add(element string) (Dot, error) {
	if _, ok := s.ctx[s.id]; !ok && len(s.ctx) >= MaxReplicas {
		return Dot{}, fmt.Errorf("%w: cannot register %q", ErrReplicaLimit, s.id)
	}
	if s.counter >= MaxCounter {
		return Dot{}, fmt.Errorf("%w: %s at %d", ErrCounterLimit, s.id, MaxCounter)
	}
	s.counter++
	d := Dot{Replica: s.id, Counter: s.counter}
	ctxAdd(s.ctx, d)
	m := s.active[element]
	if m == nil {
		m = map[Dot]struct{}{}
		s.active[element] = m
	}
	m[d] = struct{}{}
	return d, nil
}

// Remove deletes all currently (and therefore locally observed) active dots
// of element. Dots added concurrently on replicas this state has not yet
// observed remain untouched and will survive a later merge.
func (s *Set) Remove(element string) {
	delete(s.active, element)
}

// Contains reports whether element currently has at least one active dot.
func (s *Set) Contains(element string) bool {
	return len(s.active[element]) > 0
}

// Elements returns the currently visible elements in sorted order.
func (s *Set) Elements() []string {
	out := make([]string, 0, len(s.active))
	for e, dots := range s.active {
		if len(dots) > 0 {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Set) dotIndex() map[Dot]string {
	idx := map[Dot]string{}
	for elem, dots := range s.active {
		for d := range dots {
			idx[d] = elem
		}
	}
	return idx
}

// Merge folds the other state into s. It keeps:
//   - dots active on both sides for the same element;
//   - dots active on one side that the other side's context has not observed.
//
// Contexts are unioned. other is never mutated. An error is returned
// (without changing s) if the same dot is visibly active for two different
// elements, which no honest single-writer deployment can produce.
func (s *Set) Merge(other *Set) error {
	if other == nil || other == s {
		return nil
	}
	if unionReplicaCount(s.ctx, other.ctx) > MaxReplicas {
		return fmt.Errorf("%w: merge would exceed %d distinct replicas",
			ErrReplicaLimit, MaxReplicas)
	}
	sDots := s.dotIndex()
	oDots := other.dotIndex()
	for d, elem := range oDots {
		if e2, ok := sDots[d]; ok && e2 != elem {
			return fmt.Errorf("%w: (%s,%d) -> %q and %q",
				ErrDotConflict, d.Replica, d.Counter, e2, elem)
		}
	}

	keep := map[string]map[Dot]struct{}{}
	retain := func(elem string, d Dot) {
		m := keep[elem]
		if m == nil {
			m = map[Dot]struct{}{}
			keep[elem] = m
		}
		m[d] = struct{}{}
	}
	for d, elem := range sDots {
		if _, inOther := oDots[d]; inOther {
			retain(elem, d)
			continue
		}
		// Active only here: drop exactly when the other side observed
		// (and therefore removed) the dot.
		if !ctxContains(other.ctx, d) {
			retain(elem, d)
		}
	}
	for d, elem := range oDots {
		if _, inSelf := sDots[d]; inSelf {
			continue // handled above
		}
		if !ctxContains(s.ctx, d) {
			retain(elem, d)
		}
	}

	s.ctx = ctxUnion(s.ctx, other.ctx)
	s.active = keep
	return nil
}
