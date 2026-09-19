package orset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// Deterministic JSON snapshot encoding. All arrays are sorted: context
// entries by replica, exception dots ascending, elements by name, element
// dots by (replica, counter).

type snapshotJSON struct {
	ID       string        `json:"id"`
	Context  []ctxJSON     `json:"context"`
	Elements []elementJSON `json:"elements"`
}

type ctxJSON struct {
	Replica string   `json:"replica"`
	Max     uint64   `json:"max"`
	Dots    []uint64 `json:"dots"`
}

type elementJSON struct {
	Element string    `json:"element"`
	Dots    []dotJSON `json:"dots"`
}

type dotJSON struct {
	Replica string `json:"replica"`
	Counter uint64 `json:"counter"`
}

// Snapshot encodes the set as deterministic JSON. The receiver is not
// modified.
func (s *ORSet) Snapshot() []byte {
	snap := snapshotJSON{
		ID:       s.id,
		Context:  make([]ctxJSON, 0, len(s.ctx.entries)),
		Elements: make([]elementJSON, 0, len(s.els)),
	}
	for id, e := range s.ctx.entries {
		dots := make([]uint64, 0, len(e.dots))
		for d := range e.dots {
			dots = append(dots, d)
		}
		sort.Slice(dots, func(i, j int) bool { return dots[i] < dots[j] })
		snap.Context = append(snap.Context, ctxJSON{Replica: id, Max: e.max, Dots: dots})
	}
	sort.Slice(snap.Context, func(i, j int) bool { return snap.Context[i].Replica < snap.Context[j].Replica })
	for _, elem := range s.Elements() {
		dots := s.Dots(elem)
		dj := make([]dotJSON, 0, len(dots))
		for _, d := range dots {
			dj = append(dj, dotJSON{Replica: d.Replica, Counter: d.Counter})
		}
		snap.Elements = append(snap.Elements, elementJSON{Element: elem, Dots: dj})
	}
	data, err := json.Marshal(snap)
	if err != nil {
		panic(fmt.Sprintf("orset: snapshot marshal: %v", err)) // unreachable for this schema
	}
	return data
}

// Restore decodes and validates a snapshot. Validation checks replica IDs,
// positive bounded counters, canonical sorted encoding, that every active
// dot belongs to the causal context, and that no visible dot maps to two
// different elements. Any error rejects the whole snapshot; the input bytes
// are never modified. After a legitimate restore, the next local counter is
// one past the context maximum for this replica (exceptions included).
//
// Note: forgery of already-deleted dots cannot be detected from a snapshot
// alone and is not guarded against.
func Restore(data []byte) (*ORSet, error) {
	var snap snapshotJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		return nil, fmt.Errorf("orset: invalid snapshot JSON: %w", err)
	}
	var extra struct{}
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("orset: trailing data after snapshot")
	}
	if !validReplicaID(snap.ID) {
		return nil, fmt.Errorf("orset: invalid replica ID %q", snap.ID)
	}
	if len(snap.Context) > MaxReplicas {
		return nil, ErrTooManyReplicas
	}

	ctx := newContext()
	for i, ce := range snap.Context {
		if !validReplicaID(ce.Replica) {
			return nil, fmt.Errorf("orset: invalid context replica ID %q", ce.Replica)
		}
		if i > 0 && snap.Context[i-1].Replica >= ce.Replica {
			return nil, fmt.Errorf("orset: context replicas not sorted or duplicated at %q", ce.Replica)
		}
		if ce.Max > MaxCounter {
			return nil, fmt.Errorf("orset: context max %d for %q exceeds limit", ce.Max, ce.Replica)
		}
		if ce.Max == 0 && len(ce.Dots) == 0 {
			return nil, fmt.Errorf("orset: empty context entry for %q", ce.Replica)
		}
		e := &ctxEntry{max: ce.Max, dots: make(map[uint64]struct{}, len(ce.Dots))}
		for j, d := range ce.Dots {
			if d == 0 {
				return nil, fmt.Errorf("orset: non-positive exception counter for %q", ce.Replica)
			}
			if d > MaxCounter {
				return nil, fmt.Errorf("orset: exception counter %d for %q exceeds limit", d, ce.Replica)
			}
			if d <= ce.Max {
				return nil, fmt.Errorf("orset: exception %d for %q not above prefix max %d", d, ce.Replica, ce.Max)
			}
			if j > 0 && ce.Dots[j-1] >= d {
				return nil, fmt.Errorf("orset: exception dots for %q not sorted or duplicated", ce.Replica)
			}
			e.dots[d] = struct{}{}
		}
		ctx.entries[ce.Replica] = e
	}

	els := make(map[string]map[Dot]struct{}, len(snap.Elements))
	seen := make(map[Dot]string) // dot -> element, to catch visible-dot conflicts
	for i, ee := range snap.Elements {
		if i > 0 && snap.Elements[i-1].Element >= ee.Element {
			return nil, fmt.Errorf("orset: elements not sorted or duplicated at %q", ee.Element)
		}
		if len(ee.Dots) == 0 {
			return nil, fmt.Errorf("orset: element %q has no dots", ee.Element)
		}
		dots := make(map[Dot]struct{}, len(ee.Dots))
		for j, dj := range ee.Dots {
			if !validReplicaID(dj.Replica) {
				return nil, fmt.Errorf("orset: invalid dot replica ID %q", dj.Replica)
			}
			if dj.Counter == 0 {
				return nil, fmt.Errorf("orset: non-positive dot counter for element %q", ee.Element)
			}
			if dj.Counter > MaxCounter {
				return nil, fmt.Errorf("orset: dot counter %d for element %q exceeds limit", dj.Counter, ee.Element)
			}
			if j > 0 {
				prev := ee.Dots[j-1]
				if prev.Replica > dj.Replica || (prev.Replica == dj.Replica && prev.Counter >= dj.Counter) {
					return nil, fmt.Errorf("orset: dots for element %q not sorted or duplicated", ee.Element)
				}
			}
			d := Dot{Replica: dj.Replica, Counter: dj.Counter}
			if !ctx.Contains(d) {
				return nil, fmt.Errorf("orset: active dot %v not in causal context", d)
			}
			if other, ok := seen[d]; ok {
				return nil, fmt.Errorf("orset: visible dot %v maps to both %q and %q", d, other, ee.Element)
			}
			seen[d] = ee.Element
			dots[d] = struct{}{}
		}
		els[ee.Element] = dots
	}

	return &ORSet{id: snap.ID, ctx: ctx, els: els}, nil
}
