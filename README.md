# 带因果上下文的 Observed-Remove Set

离线副本之间可合并的字符串集合：支持本地添加、观察删除、状态合并与 JSON 快照恢复。
状态式 OR-Set，每个 Add 生成唯一 dot `(replicaID, counter)`，因果上下文采用
「每副本连续前缀 + 离散异常点」压缩，空洞不会被最大计数吞掉。

仅用 Go 1.26.5 标准库，全程内存模拟，不联网、无第三方依赖，可在 Windows 原生运行。

## 语义要点

- **Add**：生成本副本新 dot（计数严格递增），记入上下文并挂到元素上。
- **Remove**：仅移除本地已观察到的该元素全部活跃 dot；尚未观察到的并发 Add 保留。
- **Merge**：保留双方共有的活跃 dot，以及单侧活跃、且未被另一侧上下文观察到的 dot；
  上下文取并集。因此已删除 dot 不会因旧状态重传复活，合并满足交换律、结合律、幂等律。
- **上下文压缩**：每副本记录连续前缀 `1..prefix` 与严格大于前缀、升序的异常点；
  乱序到达只产生异常点，空洞显式保留，补齐后自动并入前缀。
- **快照恢复**：写者计数取本副本上下文最大值（含异常点），不回绕；
  校验副本 ID（1–32 字符 `[A-Za-z0-9_-]`）、正计数（≤10000）、
  活跃 dot 必须属于上下文、同一可见 dot 不能对应不同元素；任何错误整体拒绝。
- **限制**：最多 20 个副本，每副本计数 ≤ 10000。
- 不保证检测已删除 dot 的全部历史伪造（规格明确排除）；不做墓碑回收与动态成员管理。

## 接口

包根目录即包 `orset`（module `github.com/382868331/gsb-observed-remove-set-20260919`）。

| 签名 | 说明 |
| --- | --- |
| `New(replicaID string) (*Set, error)` | 创建属于某副本的空集合（单写者，禁止同 ID 并发或回退旧快照写入） |
| `(*Set) Add(element string) (Dot, error)` | 添加元素，返回新 dot |
| `(*Set) Remove(element string)` | 删除当前已观察的该元素活跃 dot |
| `(*Set) Contains(element string) bool` | 是否可见 |
| `(*Set) Elements() []string` | 可见元素，升序 |
| `(*Set) Merge(other *Set) error` | 将 other 并入接收者；不改写 other；可见 dot 冲突时返回 `ErrDotConflict` 且不改写接收者 |
| `(*Set) Snapshot() Snapshot` | 独立的规范快照结构（数组全部排序） |
| `Marshal(s *Set) ([]byte, error)` | 规范 JSON 编码（确定性字节） |
| `Unmarshal(data []byte) (*Set, error)` | 校验并恢复；错误返回 `ErrInvalidSnapshot`，不改写输入 |
| `Restore(snap Snapshot) (*Set, error)` | 同语义，直接从结构体恢复，不保留入参引用 |
| `(*Set) Clone() *Set` | 深拷贝（模拟旧状态重传） |

错误哨兵：`ErrInvalidReplica`、`ErrCounterLimit`、`ErrReplicaLimit`、
`ErrDotConflict`、`ErrInvalidSnapshot`，配合 `errors.Is` 使用。

### 快照格式

```json
{
  "version": 1,
  "replica": "alpha",
  "active": [
    { "element": "color:green", "dots": [ { "replica": "alpha", "counter": 3 } ] }
  ],
  "context": [
    { "replica": "alpha", "prefix": 2, "exceptions": [5] }
  ]
}
```

`exceptions` 可省略；所有数组按副本 ID、元素、计数排序，故相等状态字节相同。

## 运行

```bash
go run ./cmd/demo
go test ./... -count=1 -timeout=60s
```

演示约 2–3 秒（含编译），输出一个真实计算的正常收敛场景（分区并发增删、
旧快照重传、快照恢复后续写），以及一个实际触发的失败：篡改快照让同一 dot
对应两个元素，`Unmarshal` 返回 `ErrInvalidSnapshot` 整体拒绝。

## 测试覆盖

- 并发 Add 与 Remove（两种投递方向收敛）、观察后删除、旧状态重传不复活
- 上下文空洞（乱序观察、前缀补齐不吞空洞）、异常点高计数恢复（计数不回绕）
- 可见 dot 冲突（Merge 与快照两条路径）、输入不被改写
- 快照各类非法输入整体拒绝、副本/计数上限、规范编码确定性
- 固定种子（20260920）随机性质测试：与「未压缩 dot 集合」参考模型逐步比对，
  含稀疏乱序快照、重复快照，并在随机存活状态上核对交换/结合/幂等律与最终收敛
