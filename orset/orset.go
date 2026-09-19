// Package orset implements a state-based observed-remove set (ORSet) with a
// compact causal context, mergeable across offline replicas.
//
// Each replica has a fixed unique ID and a single writer. Callers must not
// run writers concurrently under the same ID, nor roll a writer back to an
// old snapshot. The simulation is in-memory only.
package orset

import (
	"errors"
	"fmt"
	"sort"
)

const (
	// MaxReplicas bounds the number of distinct replica IDs in a context.
	MaxReplicas = 20
	// MaxCounter bounds the per-replica dot counter.
	MaxCounter = 10000
	// maxIDLen bounds replica ID length.
	maxIDLen = 32
)

// ErrCounterExhausted is returned by Add when the replica's counter would
// exceed MaxCounter. Counters never wrap.
var ErrCounterExhausted = errors.New("orset: replica counter exhausted")

// ErrTooManyReplicas is returned when a context would exceed MaxReplicas.
var ErrTooManyReplicas = errors.New("orset: too many replicas")

// Dot identifies a single Add event: (replicaID, counter).
type Dot struct {
	Replica string
	Counter uint64
}

// ctxEntry is the compact per-replica causal record: a contiguous prefix
// [1..max] plus discrete exception dots above max. Holes below an exception
// are never swallowed by max.
type ctxEntry struct {
	max  uint64
	dots map[uint64]struct{} // exceptions, all > max
}

// compact absorbs exception dots that extend the contiguous prefix.
func (e *ctxEntry) compact() {
	for {
		if _, ok := e.dots[e.max+1]; ok {
			delete(e.dots, e.max+1)
			e.max++
		} else {
			return
		}
	}
}

func (e *ctxEntry) clone() *ctxEntry {
	out := &ctxEntry{max: e.max, dots: make(map[uint64]struct{}, len(e.dots))}
	for d := range e.dots {
		out.dots[d] = struct{}{}
	}
	return out
}

// Context is the causal context: the set of dots a replica has observed,
// stored compactly per replica as prefix + exceptions.
type Context struct {
	entries map[string]*ctxEntry
}

func newContext() *Context {
	return &Context{entries: make(map[string]*ctxEntry)}
}

// Contains reports whether the dot has been observed.
func (c *Context) Contains(d Dot) bool {
	e, ok := c.entries[d.Replica]
	if !ok {
		return false
	}
	if d.Counter <= e.max {
		return true
	}
	_, ok = e.dots[d.Counter]
	return ok
}

// Add records a dot as observed. Idempotent.
func (c *Context) Add(d Dot) {
	e, ok := c.entries[d.Replica]
	if !ok {
		e = &ctxEntry{dots: make(map[uint64]struct{})}
		c.entries[d.Replica] = e
	}
	if d.Counter <= e.max {
		return
	}
	if d.Counter == e.max+1 {
		e.max++
		e.compact()
		return
	}
	e.dots[d.Counter] = struct{}{}
}

// Max returns the highest observed counter for a replica, including
// exception dots. Returns 0 if the replica is unknown.
func (c *Context) Max(replica string) uint64 {
	e, ok := c.entries[replica]
	if !ok {
		return 0
	}
	m := e.max
	for d := range e.dots {
		if d > m {
			m = d
		}
	}
	return m
}

// ReplicaCount returns the number of distinct replicas in the context.
func (c *Context) ReplicaCount() int {
	return len(c.entries)
}

// Union returns a new context observing everything either input observes,
// without mutating either input. Holes are preserved: exceptions below the
// merged prefix are absorbed only when actually observed.
func (c *Context) Union(o *Context) *Context {
	out := &Context{entries: make(map[string]*ctxEntry, len(c.entries))}
	for id, e := range c.entries {
		out.entries[id] = e.clone()
	}
	for id, e := range o.entries {
		t, ok := out.entries[id]
		if !ok {
			out.entries[id] = e.clone()
			continue
		}
		if e.max > t.max {
			for d := range t.dots {
				if d <= e.max {
					delete(t.dots, d)
				}
			}
			t.max = e.max
		}
		for d := range e.dots {
			if d > t.max {
				t.dots[d] = struct{}{}
			}
		}
		t.compact()
	}
	return out
}

func (c *Context) clone() *Context {
	out := &Context{entries: make(map[string]*ctxEntry, len(c.entries))}
	for id, e := range c.entries {
		out.entries[id] = e.clone()
	}
	return out
}

// equal reports whether two contexts observe exactly the same dots.
func (c *Context) equal(o *Context) bool {
	if len(c.entries) != len(o.entries) {
		return false
	}
	for id, e := range c.entries {
		oe, ok := o.entries[id]
		if !ok || oe.max != e.max || len(oe.dots) != len(e.dots) {
			return false
		}
		for d := range e.dots {
			if _, ok := oe.dots[d]; !ok {
				return false
			}
		}
	}
	return true
}

// ORSet is a state-based observed-remove set of strings.
type ORSet struct {
	id  string
	ctx *Context
	els map[string]map[Dot]struct{} // element -> active dots
}

// New creates an empty set for the given replica ID.
func New(id string) (*ORSet, error) {
	if !validReplicaID(id) {
		return nil, fmt.Errorf("orset: invalid replica ID %q", id)
	}
	return &ORSet{
		id:  id,
		ctx: newContext(),
		els: make(map[string]map[Dot]struct{}),
	}, nil
}

// ID returns the replica ID of this set.
func (s *ORSet) ID() string { return s.id }

// Add inserts an element, generating a fresh dot (replicaID, counter). The
// counter is one past the maximum observed for this replica (including
// exception dots) and never wraps.
func (s *ORSet) Add(elem string) (Dot, error) {
	next := s.ctx.Max(s.id) + 1
	if next > MaxCounter {
		return Dot{}, ErrCounterExhausted
	}
	d := Dot{Replica: s.id, Counter: next}
	s.ctx.Add(d)
	dots, ok := s.els[elem]
	if !ok {
		dots = make(map[Dot]struct{})
		s.els[elem] = dots
	}
	dots[d] = struct{}{}
	return d, nil
}

// Remove deletes all currently observed dots for the element. Concurrent
// adds not yet observed are unaffected.
func (s *ORSet) Remove(elem string) {
	delete(s.els, elem)
}

// Contains reports whether the element has at least one active dot.
func (s *ORSet) Contains(elem string) bool {
	return len(s.els[elem]) > 0
}

// Elements returns the visible elements in sorted order.
func (s *ORSet) Elements() []string {
	out := make([]string, 0, len(s.els))
	for e, dots := range s.els {
		if len(dots) > 0 {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

// Dots returns the active dots for an element in sorted order.
func (s *ORSet) Dots(elem string) []Dot {
	dots := s.els[elem]
	out := make([]Dot, 0, len(dots))
	for d := range dots {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Replica != out[j].Replica {
			return out[i].Replica < out[j].Replica
		}
		return out[i].Counter < out[j].Counter
	})
	return out
}

// Context returns a copy of the causal context.
func (s *ORSet) Context() *Context { return s.ctx.clone() }

// Clone returns a deep copy of the set.
func (s *ORSet) Clone() *ORSet {
	out := &ORSet{id: s.id, ctx: s.ctx.clone(), els: make(map[string]map[Dot]struct{}, len(s.els))}
	for e, dots := range s.els {
		nd := make(map[Dot]struct{}, len(dots))
		for d := range dots {
			nd[d] = struct{}{}
		}
		out.els[e] = nd
	}
	return out
}

// Merge folds other into s. A dot survives if it is active on both sides, or
// active on one side and not yet observed by the other side's context. The
// merged context is the union of both, so dots deleted on either side are
// not resurrected by retransmission of old states. other is not modified.
func (s *ORSet) Merge(other *ORSet) error {
	newCtx := s.ctx.Union(other.ctx)
	if newCtx.ReplicaCount() > MaxReplicas {
		return ErrTooManyReplicas
	}
	keep := make(map[string]map[Dot]struct{})
	put := func(elem string, d Dot) {
		dots, ok := keep[elem]
		if !ok {
			dots = make(map[Dot]struct{})
			keep[elem] = dots
		}
		dots[d] = struct{}{}
	}
	for elem, dots := range s.els {
		otherDots := other.els[elem]
		for d := range dots {
			if _, shared := otherDots[d]; shared {
				put(elem, d) // active on both sides
			} else if !other.ctx.Contains(d) {
				put(elem, d) // active here, unseen by the other side
			}
		}
	}
	for elem, dots := range other.els {
		for d := range dots {
			if _, shared := s.els[elem][d]; shared {
				continue // already kept above
			}
			if !s.ctx.Contains(d) {
				put(elem, d) // active there, unseen by this side
			}
		}
	}
	s.ctx = newCtx
	s.els = keep
	return nil
}

// equal reports whether two sets have identical visible elements, active
// dots, and causal contexts. Used by tests and the demo.
func (s *ORSet) equal(o *ORSet) bool {
	if len(s.els) != len(o.els) || !s.ctx.equal(o.ctx) {
		return false
	}
	for e, dots := range s.els {
		od, ok := o.els[e]
		if !ok || len(od) != len(dots) {
			return false
		}
		for d := range dots {
			if _, ok := od[d]; !ok {
				return false
			}
		}
	}
	return true
}

// Equal reports whether two sets have identical state.
func (s *ORSet) Equal(o *ORSet) bool { return s.equal(o) }

func validReplicaID(id string) bool {
	if len(id) == 0 || len(id) > maxIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		b := id[i]
		if !('a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' || b == '-' || b == '_') {
			return false
		}
	}
	return true
}
