# observed-remove set（带因果上下文，可离线合并）

状态式 observed-remove set（ORSet），元素为字符串。断连副本各自在内存中维护集合并交换 JSON 快照，合并后收敛且不复活已观察的删除。仅标准库，无网络、无第三方依赖。

## 模型

- 每次 `Add` 生成唯一 dot = `(replicaID, counter)`；计数取本副本因果上下文最大值（含异常点）+1，不回绕。
- 状态 = `element -> 活跃 dots` + 已观察因果上下文。上下文按副本存「连续前缀 max + 离散异常点」，空洞不会被 max 吞掉。
- `Remove` 只移除当前已观察的该元素 dots；并发未见的 `Add` 保留。
- `Merge` 保留：双方共有活跃 dot，以及单侧活跃且未被另一侧上下文观察的 dot；上下文取并。被删除 dot 不会因旧状态重传复活。不做墓碑回收与动态副本成员管理。
- 约束：固定唯一副本 ID（每 ID 单写者，不得并发运行或从旧快照回退写者）；最多 20 副本；每副本计数 ≤ 10000。

## 接口（包 `orset`）

```go
s, err := orset.New("alpha")        // 新建副本（校验副本 ID）
d, err := s.Add("milk")             // 添加元素，返回生成的 Dot；计数超限返回 ErrCounterExhausted
s.Remove("milk")                    // 移除已观察的 dots
ok := s.Contains("milk")            // 是否可见
elems := s.Elements()               // 排序后的可见元素
dots := s.Dots("milk")              // 该元素的活跃 dots
err = s.Merge(other)                // 合并 other 到 s；不修改 other
snap := s.Snapshot()                // 确定性 JSON 快照（数组排序编码）
s2, err := orset.Restore(snap)      // 校验并整体恢复；任何错误整体拒绝，不改写输入
eq := s.Equal(s2)                   // 状态等价比较
c := s.Clone()                      // 深拷贝
```

快照校验：副本 ID 合法、计数为正且 ≤ 10000、数组按规范排序、活跃 dot 属于因果上下文、同一可见 dot 不得对应两个不同元素。无法检测已删除 dot 的历史伪造，不作该保证。

## 运行

Windows 原生 Go 1.26.5，离线：

```
go run ./cmd/demo                       # 约 8 秒内：一个正常收敛结果 + 一个实际触发的校验失败
go test ./... -count=1 -timeout=60s     # 全部测试
```

测试覆盖：并发 Add 与 Remove、观察后删除、旧状态重传、上下文空洞、异常点高计数恢复、可见 dot 冲突、输入不变性，以及用未压缩 dot 集合参考模型核对压缩上下文与合并的交换/结合/幂等律（固定种子、小样本，含乱序重复快照回放）。
