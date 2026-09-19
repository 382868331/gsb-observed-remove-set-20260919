// Demo of the observed-remove set library: one normal convergent run and
// one genuinely triggered validation failure. All results are computed by
// the library at runtime; everything is in-memory and finishes in well
// under 8 seconds.
package main

import (
	"fmt"
	"os"

	"github.com/382868331/gsb-observed-remove-set-20260919/orset"
)

func main() {
	fmt.Println("=== observed-remove set demo (offline, in-memory) ===")
	normal()
	if !failure() {
		fmt.Println("FAILURE CASE DID NOT TRIGGER: forged snapshot was accepted")
		os.Exit(1)
	}
	fmt.Println("=== demo done ===")
}

// normal: two replicas concurrently add/remove, exchange snapshots, and
// converge; a retransmitted old state must not resurrect the removal.
func normal() {
	alpha, err := orset.New("alpha")
	must(err)
	beta, err := orset.New("beta")
	must(err)

	if _, err := alpha.Add("milk"); err != nil {
		must(err)
	}
	if _, err := alpha.Add("eggs"); err != nil {
		must(err)
	}
	must(beta.Merge(alpha)) // first sync: beta observes milk and eggs

	stale := alpha.Clone() // old state captured before the remove

	beta.Remove("milk") // observed remove on beta
	if _, err := alpha.Add("bread"); err != nil {
		must(err)
	} // concurrent add on alpha, unseen by beta

	must(alpha.Merge(beta))
	must(beta.Merge(alpha))
	fmt.Printf("[normal] converged elements:        %v\n", alpha.Elements())
	fmt.Printf("[normal] replicas equal:            %v\n", alpha.Equal(beta))

	// Old-state retransmission: the stale copy still holds "milk".
	must(beta.Merge(stale))
	must(alpha.Merge(beta))
	fmt.Printf("[normal] after stale retransmission: %v (milk stays removed)\n", alpha.Elements())

	// Snapshot round-trip through the deterministic JSON encoding.
	snap := alpha.Snapshot()
	restored, err := orset.Restore(snap)
	must(err)
	fmt.Printf("[normal] snapshot %d bytes, restored equal: %v\n", len(snap), restored.Equal(alpha))
}

// failure: a forged snapshot maps the same visible dot to two different
// elements; Restore must reject it as a whole.
func failure() bool {
	forged := []byte(`{"id":"gamma","context":[{"replica":"gamma","max":1,"dots":[]}],"elements":[` +
		`{"element":"x","dots":[{"replica":"gamma","counter":1}]},` +
		`{"element":"y","dots":[{"replica":"gamma","counter":1}]}]}`)
	_, err := orset.Restore(forged)
	if err == nil {
		return false
	}
	fmt.Printf("[failure] forged snapshot rejected:  %v\n", err)
	return true
}

func must(err error) {
	if err != nil {
		fmt.Println("unexpected error:", err)
		os.Exit(1)
	}
}
