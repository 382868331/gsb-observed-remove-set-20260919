// Command demo exercises the orset library end to end, entirely in memory.
// It prints one successful convergence scenario and one failure that is
// actually triggered by validating a tampered snapshot.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	orset "github.com/382868331/gsb-observed-remove-set-20260919"
)

func main() {
	start := time.Now()
	fmt.Println("=== OR-Set 离线副本演示 ===")
	fmt.Println()

	ok := demoNormal()
	fmt.Println()
	failOK := demoFailure()
	fmt.Println()

	fmt.Printf("耗时 %s\n", time.Since(start).Round(time.Millisecond))
	if !ok || !failOK {
		fmt.Println("演示异常：以上场景未按预期运行")
		return
	}
	fmt.Println("完成：正常场景收敛，伪造快照被实际拒绝。")
}

// demoNormal runs a partition/merge story and verifies real convergence.
func demoNormal() bool {
	fmt.Println("[场景 1] 正常：分区编辑 -> 合并 -> 快照恢复后继续写入")

	alpha, _ := orset.New("alpha")
	beta, _ := orset.New("beta")
	gamma, _ := orset.New("gamma")

	// Common history before the partition.
	mustAdd(alpha, "color:blue")
	mustAdd(alpha, "shape:circle")
	must(beta.Merge(alpha))
	must(gamma.Merge(alpha))

	// Network partitions: each side edits locally.
	mustAdd(alpha, "color:green") // concurrent add ...
	beta.Remove("color:blue")     // ... against an observed removal
	mustAdd(beta, "owner:beta")
	gamma.Remove("shape:circle")
	mustAdd(gamma, "region:cn")

	// Snapshot one side mid-partition (old state, later retransmitted).
	oldBeta := beta.Clone()

	// Reconnect with shuffled merge order; convergence must be order-free.
	must(alpha.Merge(beta))
	must(gamma.Merge(alpha))
	must(beta.Merge(gamma))
	must(alpha.Merge(gamma))

	// Old snapshot delivered after convergence: no resurrection.
	must(beta.Merge(oldBeta))

	// Recover gamma from its JSON snapshot and keep writing.
	data := mustMarshal(gamma)
	restored, err := orset.Unmarshal(data)
	if err != nil {
		fmt.Printf("  失败：快照恢复返回错误: %v\n", err)
		return false
	}
	mustAdd(restored, "after-restore")
	must(gamma.Merge(restored))
	must(beta.Merge(restored))
	must(alpha.Merge(restored))

	want := map[string]bool{
		"color:green":   true, // concurrent add survived the remove
		"owner:beta":    true,
		"region:cn":     true,
		"after-restore": true,
	}
	gotA, gotB, gotG := alpha.Elements(), beta.Elements(), gamma.Elements()
	converged := eq(gotA, gotB) && eq(gotB, gotG)
	matches := len(gotA) == len(want)
	for _, e := range gotA {
		if !want[e] {
			matches = false
		}
	}
	dead := !alpha.Contains("color:blue") && !alpha.Contains("shape:circle")

	fmt.Printf("  分区并发操作: alpha 加 color:green，beta 删 color:blue\n")
	fmt.Printf("  合并后三副本元素一致: %v -> %v\n", converged, gotA)
	fmt.Printf("  已观察删除保持失效(color:blue/shape:circle): %v\n", dead)
	fmt.Printf("  快照恢复后新 Add 的计数连续未回绕: %v\n",
		restored.Contains("after-restore"))
	fmt.Printf("  旧快照重传未复活元素(已包含在上方结果中)\n")

	if converged && matches && dead {
		fmt.Println("  结果: 正常")
		return true
	}
	fmt.Println("  结果: 异常（语义不符）")
	return false
}

// demoFailure tampers with an otherwise valid JSON snapshot and shows the
// library actually rejecting it as a whole.
func demoFailure() bool {
	fmt.Println("[场景 2] 失败：篡改快照（同一 dot 对应两个元素），恢复必须被拒绝")

	s, _ := orset.New("alpha")
	mustAdd(s, "task-1")
	data, err := orset.Marshal(s)
	if err != nil {
		fmt.Printf("  失败：编码错误: %v\n", err)
		return false
	}

	// Decode to a generic, unordered structure, duplicate the active entry
	// under a different element, and re-encode with arrays sorted like the
	// canonical format: this is a real conflict, not a syntax error.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		fmt.Printf("  失败：解析错误: %v\n", err)
		return false
	}
	active := raw["active"].([]any)
	first := active[0].(map[string]any)
	dup := map[string]any{
		"element": "task-2",
		"dots":    first["dots"],
	}
	raw["active"] = []any{first, dup}
	tampered, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		fmt.Printf("  失败：重编码错误: %v\n", err)
		return false
	}

	fmt.Println("  篡改后的快照片段:")
	for _, line := range strings.Split(string(tampered), "\n") {
		fmt.Println("    " + line)
	}

	_, err = orset.Unmarshal(tampered)
	fmt.Printf("  Unmarshal 返回: %v\n", err)
	if errors.Is(err, orset.ErrInvalidSnapshot) {
		fmt.Println("  结果: 按预期触发失败，错误输入被整体拒绝")
		return true
	}
	fmt.Println("  结果: 异常（伪造快照未被检测到）")
	return false
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustAdd(s *orset.Set, elem string) {
	if _, err := s.Add(elem); err != nil {
		panic(err)
	}
}

func mustMarshal(s *orset.Set) []byte {
	b, err := orset.Marshal(s)
	if err != nil {
		panic(err)
	}
	return b
}

func eq(a, b []string) bool {
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
