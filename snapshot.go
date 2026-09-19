package orset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
)

// snapshotVersion is the only accepted on-disk format version.
const snapshotVersion = 1

var idRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func validID(id string) bool { return idRE.MatchString(id) }

// Snapshot is the canonical, deterministic JSON projection of a Set.
// Array contents are sorted, so equal states always encode byte-identically.
// The writer counter is not stored: on restore it is derived as the largest
// observed counter of the owning replica (including exception points).
type Snapshot struct {
	Version int             `json:"version"`
	Replica string          `json:"replica"`
	Active  []ActiveElement `json:"active"`
	Context []ContextEntry  `json:"context"`
}

// ActiveElement lists the dots currently keeping Element visible.
type ActiveElement struct {
	Element string `json:"element"`
	Dots    []Dot  `json:"dots"`
}

// ContextEntry is one compressed per-replica causal context: counters
// 1..Prefix observed contiguously, plus sorted Exceptions above Prefix.
type ContextEntry struct {
	Replica    string `json:"replica"`
	Prefix     int    `json:"prefix"`
	Exceptions []int  `json:"exceptions,omitempty"`
}

// Snapshot returns an independent, canonical copy of the state. The caller
// may mutate the returned value; it never aliases the Set's internals.
func (s *Set) Snapshot() Snapshot {
	snap := Snapshot{Version: snapshotVersion, Replica: s.id}

	elems := make([]string, 0, len(s.active))
	for e, dots := range s.active {
		if len(dots) > 0 {
			elems = append(elems, e)
		}
	}
	sortStrings(elems)
	for _, e := range elems {
		dots := sortedDots(s.active[e])
		snap.Active = append(snap.Active, ActiveElement{Element: e, Dots: dots})
	}

	ids := make([]string, 0, len(s.ctx))
	for id := range s.ctx {
		ids = append(ids, id)
	}
	sortStrings(ids)
	for _, id := range ids {
		r := s.ctx[id]
		entry := ContextEntry{Replica: id, Prefix: r.prefix}
		if len(r.exc) > 0 {
			entry.Exceptions = append([]int(nil), r.exc...)
		}
		snap.Context = append(snap.Context, entry)
	}
	return snap
}

func sortedDots(m map[Dot]struct{}) []Dot {
	out := make([]Dot, 0, len(m))
	for d := range m {
		out = append(out, d)
	}
	sortDotsSlice(out)
	return out
}

func sortDotsSlice(d []Dot) {
	sort.Slice(d, func(i, j int) bool { return lessDot(d[i], d[j]) })
}

func lessDot(a, b Dot) bool {
	if a.Replica != b.Replica {
		return a.Replica < b.Replica
	}
	return a.Counter < b.Counter
}

func sortStrings(x []string) { sort.Strings(x) }

// Marshal encodes a Set to canonical JSON.
func Marshal(s *Set) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s.Snapshot()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Unmarshal validates JSON and restores a Set from it. Malformed input is
// rejected as a whole. The input byte slice is never modified.
func Unmarshal(data []byte) (*Set, error) {
	var snap Snapshot
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&snap); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSnapshot, err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%w: unexpected trailing data", ErrInvalidSnapshot)
	}
	return Restore(snap)
}

// Restore validates snap and builds a Set from it. The snapshot is never
// mutated and never retained: the result owns fully independent storage.
func Restore(snap Snapshot) (*Set, error) {
	if err := validate(snap); err != nil {
		return nil, err
	}

	ctx := make(map[string]*replicaCtx, len(snap.Context))
	for _, c := range snap.Context {
		r := &replicaCtx{prefix: c.Prefix}
		if len(c.Exceptions) > 0 {
			r.exc = append([]int(nil), c.Exceptions...)
		}
		ctx[c.Replica] = r
	}

	active := map[string]map[Dot]struct{}{}
	seen := map[Dot]string{}
	for _, ae := range snap.Active {
		m := map[Dot]struct{}{}
		for _, d := range ae.Dots {
			m[d] = struct{}{}
			seen[d] = ae.Element
		}
		active[ae.Element] = m
	}

	counter := 0
	if r := ctx[snap.Replica]; r != nil {
		counter = r.prefix
		if len(r.exc) > 0 {
			counter = r.exc[len(r.exc)-1]
		}
	}

	return &Set{id: snap.Replica, counter: counter, active: active, ctx: ctx}, nil
}

func validate(snap Snapshot) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidSnapshot, fmt.Sprintf(format, args...))
	}
	if snap.Version != snapshotVersion {
		return bad("unsupported version %d", snap.Version)
	}
	if !validID(snap.Replica) {
		return bad("invalid owner replica id %q", snap.Replica)
	}

	known := map[string]struct{}{}
	for _, c := range snap.Context {
		if !validID(c.Replica) {
			return bad("invalid context replica id %q", c.Replica)
		}
		if _, dup := known[c.Replica]; dup {
			return bad("duplicate context entry for %q", c.Replica)
		}
		known[c.Replica] = struct{}{}
		if c.Prefix < 0 || c.Prefix > MaxCounter {
			return bad("prefix out of range for %q: %d", c.Replica, c.Prefix)
		}
		prev := c.Prefix
		for _, n := range c.Exceptions {
			if n <= 0 || n > MaxCounter {
				return bad("exception counter out of range for %q: %d", c.Replica, n)
			}
			if n <= prev {
				return bad("exceptions not strictly increasing above prefix for %q", c.Replica)
			}
			prev = n
		}
	}
	if len(known) > MaxReplicas {
		return bad("replica limit %d exceeded: %d", MaxReplicas, len(known))
	}
	if _, ok := known[snap.Replica]; !ok && len(known)+1 > MaxReplicas {
		return bad("replica limit %d exceeded including owner", MaxReplicas)
	}

	global := map[Dot]string{}
	seenElem := map[string]struct{}{}
	for _, ae := range snap.Active {
		if _, dup := seenElem[ae.Element]; dup {
			return bad("duplicate active element entry for %q", ae.Element)
		}
		seenElem[ae.Element] = struct{}{}
		local := map[Dot]struct{}{}
		for _, d := range ae.Dots {
			if !validID(d.Replica) {
				return bad("invalid dot replica id %q", d.Replica)
			}
			if d.Counter <= 0 || d.Counter > MaxCounter {
				return bad("dot counter out of range: (%s,%d)", d.Replica, d.Counter)
			}
			if _, dup := local[d]; dup {
				return bad("duplicate active dot (%s,%d) under %q", d.Replica, d.Counter, ae.Element)
			}
			local[d] = struct{}{}
			if _, ok := known[d.Replica]; !ok {
				return bad("active dot (%s,%d) missing from context", d.Replica, d.Counter)
			}
			if !ctxEntryContains(snap, d) {
				return bad("active dot (%s,%d) not contained in context", d.Replica, d.Counter)
			}
			if other, clash := global[d]; clash {
				return bad("dot (%s,%d) active for both %q and %q", d.Replica, d.Counter, other, ae.Element)
			}
			global[d] = ae.Element
		}
	}
	return nil
}

func ctxEntryContains(snap Snapshot, d Dot) bool {
	for _, c := range snap.Context {
		if c.Replica != d.Replica {
			continue
		}
		if d.Counter <= c.Prefix && d.Counter >= 1 {
			return true
		}
		for _, n := range c.Exceptions {
			if n == d.Counter {
				return true
			}
			if n > d.Counter {
				break
			}
		}
		return false
	}
	return false
}

// Clone returns an independent deep copy of s, suitable for simulating
// retransmitted old states.
func (s *Set) Clone() *Set {
	out := &Set{
		id:      s.id,
		counter: s.counter,
		active:  map[string]map[Dot]struct{}{},
		ctx:     map[string]*replicaCtx{},
	}
	for e, dots := range s.active {
		m := make(map[Dot]struct{}, len(dots))
		for d := range dots {
			m[d] = struct{}{}
		}
		out.active[e] = m
	}
	for id, r := range s.ctx {
		out.ctx[id] = &replicaCtx{prefix: r.prefix, exc: append([]int(nil), r.exc...)}
	}
	return out
}
