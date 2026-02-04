# base LV 从哪里来：除了“复制 merged view”之外的替代方案

**创建时间**: 2026-01-23  
**目的**: 针对“`thin_send(baseLV, targetLV)` 做块级 diff → 解析变化块 → 映射到文件差异 → 复用 tar 流程生成 OCI layer”的目标，说明 base LV 的可行来源、各自优缺点与在 containerd 语义下的落地方式。

---

## 0. 先把目标说清楚：base LV 必须是什么（块级 diff 视角）

你要的 `base LV`（作为 `thin_send` 的 base 端）本质上必须满足：

- **语义**：它代表“镜像只读层的最终合并视图”（即容器启动时看到的只读根文件系统内容）在**块设备**上的基线。
- **thinpool 约束**：`baseLV` 与 `targetLV` 必须都是 **thin volume / thin snapshot**，并且位于同一个 thinpool（否则 `thin_send` 无法工作或没有意义）。
- **anecstry/共享块约束（非常关键）**：为了让 `thin_send` 真正“增量”，`targetLV` 应该是从 `baseLV` 派生出来的 thin snapshot（或至少两者共享大量块映射）。  
  - 如果 `targetLV` 是“独立空 LV + cp/rsync 全量写入”，那么从 thinpool 视角看它的大量块地址都与 `baseLV` 不同，`thin_send` 很容易退化成“输出几乎整个 target 的已使用块”（你之前看到 148MB 的原因本质就在这里）。
- **稳定性**：`targetLV` diff 时要用只读快照（避免写入导致视图变化）。

> 关键结论：我们最终要把“块变化”还原成“文件变化”，因此仍然需要能挂载/遍历 base/target 的文件树；但**diff 的输入必须是两个 thin LV**，并且最好存在 ancestry/共享块关系，否则增量意义会大幅下降。

---

## 1. 方案 A（最推荐/最正统）：base LV 就是 “镜像的 committed snapshot（chainID-final）” 对应的 LV

### 1.1 思路

让 snapshotter 的 **镜像层（committed snapshots）本身就落在 LVM thin LV 上**。

这样：

- **base LV = chainID-final 对应的 committed LV**（只读）
- **writable LV = 基于 base LV 的 thin snapshot**（容器 active）
- 你要的 diff 就是：`base LV` vs `writable LV snapshot`

### 1.2 为什么这是“最干净”的方案

- **完全符合 containerd 模型**：unpack 逐层 `Prepare(tempKey,parent)` → `Apply(layer)` → `Commit(chainID,tempKey)`。
- **天然得到 base**：最后一层的 `Commit` 产物（chainID-final）在 containerd 语义里代表“镜像最终 rootfs 的基准”，因为它携带了完整的 parent 链信息，**通过 snapshotter 挂载（mounts）后得到的视图就是完整 rootfs**。
- **不用额外复制 merged view**：merged view 的来源应该是“mount 这个 chainID-final snapshot（或基于它 View 出一个只读快照）”，而不是拿某个 `upperdir` 目录直接当 merged view。

> 重要澄清（避免误解）：
> - 对 **overlay snapshotter** 来说，磁盘上的每个 `upperdir` 目录确实只包含“该层的增量内容”，**单独看最后一层 upperdir 并不是完整 rootfs**。
> - 但 **snapshot(对象)** 的身份是 `chainID-final`，它通过 parent 链在挂载时组合出完整视图：`lowerdir=parents...`（以及容器自己的 `upperdir`）。
> - 对 **LVM thin snapshot** 这种后端来说，如果 committed snapshot 真的是“父 LV 的 thin snapshot”，那么“最后一层 committed LV”在块层面就已经包含完整视图（因为块设备快照天然包含父数据）。

### 1.3 落地要点（与 “你说的每层直接 Unpack 到 base LV” 的关系）

实际上这等价于：

- 每一层都 Apply 到一个“新 active LV”（它是父 committed LV 的 thin snapshot）
- Apply 完成后把这个 active LV Commit 成该层的 chainID（只读）
- 最后一层 committed LV 就是你要的 base

> 换句话说：你说的“每层直接 Unpack 到 base LV”，在 containerd 的语义里更自然的实现不是“永远写同一个 base LV”，而是“每层写一个新的 active LV（thin snapshot），Commit 后形成 chainID 链”。  
> 这样才能同时满足：层复用/缓存、正确的 parent 语义、容器创建依赖 chainID 的行为。

### 1.4 代价

- 你需要让 snapshotter 的 **committed 层也在 LVM 上**（比“只把容器可写层放 LVM”改动更大）。

### 1.5 方案 A 具体怎么实现（可执行步骤，块级 diff 对齐）

这一节把“方案 A”落到 **containerd 的 snapshotter 接口**上：你需要让 `Prepare/View/Commit/Mounts` 的行为具备“LVM thin snapshot 层叠”的语义，从而使：

- **镜像 unpack 结束后**：`chainID-final` 对应一个 **只读 committed LV**（它是父 LV 的 thin snapshot 链顶端）
- **容器创建时**：`Prepare(containerID, chainID-final)` 创建一个 **可写 LV（thin snapshot of chainID-final LV）**
- **commit/diff 时**：`base = chainID-final LV`，`target = writable LV 的只读快照`（thin snapshot），先做 `thin_send(base, target)` 得到块变化流，再把块变化映射成文件变化并打 OCI layer

> 核心理解：在 LVM thin snapshot 模型里，“最后一层也是变更层”没错，但它是 **块级快照**意义上的变更层——挂载该 LV 时看到的是“完整文件系统视图”，未改动的块会从父 LV 映射读取，所以不需要把 lowdir 的数据“拷贝进最后一层”。

#### 1.5.1 数据模型：chainID ↔ LV 的映射

你需要维护一张映射表（放在 snapshotter 的 metadata store 里，或单独的 KV/bolt bucket）：

- `chainID (string) -> lvName (string)`
- `activeKey (string) -> lvName (string)`（active 期间的临时 key，比如 unpack 的 `extract-...` 或容器 `containerID`）

建议命名：

- **committed（镜像层）LV**：`img-<chainid-short>`（或直接 `sha256-...` 做缩写，避免超过 LVM 名称限制）
- **active（unpack 临时）LV**：`unpack-<random>-<chainid-short>`
- **active（容器）LV**：`ctr-<containerid-short>`

#### 1.5.2 Unpack 阶段：每层如何落到 LVM（Prepare/Apply/Commit）

containerd unpack 的核心循环是（简化）：`Prepare(key,parent)` → `Apply(layer, mounts)` → `Commit(chainID, key)`。

你要做的就是把这三步映射成 LVM thin snapshot 链：

**(A) Prepare(key, parentChainID)**：

- 如果 `parentChainID == ""`（第一层）：
  - 创建一个新的 thin LV：`lvKey`（空文件系统）
  - `mkfs`（ext4/xfs）并挂载到 snapshotter 的 `.../snapshots/<id>/fs`
- 如果 `parentChainID != ""`（后续层）：
  - 从映射表查到 `parentLV`
  - 创建 `lvKey = thinSnapshot(parentLV)`（这一步很关键：让新层“继承父层的块视图”）
  - 挂载 `lvKey` 到 `.../snapshots/<id>/fs`

返回的 `mounts` 对 applier 来说可以是：

- `bind` 到挂载点（最简单）：applier 直接往目录写，数据落到 LV

**(B) Apply(layerBlob, mounts)**：

- 这一步不需要你改 containerd：applier 会把 layer tar 解出来写到 `mounts` 指向的目录里
- 写入只会触发当前 `lvKey` 的 COW（改动块从 thinpool 分配），父 LV 不变

**(C) Commit(chainID, key)**：

- 将 active key 对应的 LV **标记为只读（committed）**：
  - LVM 层面：可选做法是把 LV 设置成只读（或保持逻辑只读，仅由 snapshotter 保证不再写）
  - metadata 层面：把该 snapshot 从 KindActive 改成 KindCommitted（containerd 的 snapshot 元数据）
- 更新映射表：`chainID -> lvKey`
- 删除 `key -> lvKey` 的 active 映射（或保留做追踪）

> 这样做完后：镜像每一层的 `chainID` 都对应一个“父层 thin snapshot + 本层改动”的 committed LV。  
> 最后一层 `chainID-final` 对应的 LV，挂载视图就是最终 rootfs（通过快照链读取父块）。

#### 1.5.3 容器创建：如何基于 chainID-final 得到 writable LV

containerd 创建容器 rootfs 的关键是：`Prepare(containerID, chainID-final)`（见 `WithNewSnapshot`）。

你的 snapshotter 在这里应该：

- 从映射表拿到 `baseLV = lvOf(chainID-final)`
- 创建 `writableLV = thinSnapshot(baseLV)`（**容器可写层**）
- 挂载 `writableLV` 到容器的 `.../snapshots/<id>/fs`，返回 `mounts`（bind 或 block mount）

这样容器看到的 rootfs 就是 `writableLV` 的文件系统视图：

- 未改动文件读取：沿 thin snapshot 映射链回到 `baseLV`（以及更上层）
- 改动文件写入：写入 `writableLV` 自己分配的新块

#### 1.5.4 commit/diff：base LV 和 writable LV（或其快照）怎么取（符合 thin_send 增量条件）

你最终想做的是：`base` vs `write`（或其快照）→ 文件级 diff → OCI layer。

在方案 A 里，推荐这样取（同时满足“两个 thin LV + ancestry”）：

- **base**：容器创建时使用的 `parentChainID` 对应的 LV（即 `chainID-final` 的 committed LV）
- **write**：容器的 `writableLV` 先创建一个 **只读快照**（thin snapshot，命名如 `ctr-<id>-snap`），避免 diff 过程中容器写入导致视图变化

然后：

- `thin_send(baseLV, writeSnapLV)` → 得到变化块数据流
- 解析变化块 → 块到文件映射 → 得到文件变化列表（Add/Modify/Delete）
- 复用 containerd 的 tar 写入逻辑（ChangeWriter/whiteout）生成 OCI layer

#### 1.5.5 关键注意事项（否则实现会“看似能跑、实际不对”）

- **快照链长度与性能**：每层都是 thin snapshot，会形成链。读性能可能受影响，需要评估（可做 flatten/merge/定期重基线）。
- **文件系统与 mkfs**：每个 LV 都是可挂载的文件系统视图；mkfs 只在第一层创建时做一次，后续层是对父 LV 的 snapshot，不要重复 mkfs。
- **一致性（diff 时）**：一定对 `writableLV` 做只读快照后再 diff（你前面关心的“快照会不会变”就在这里解决）。
- **增量前提（必须再次强调）**：容器 writable LV 必须是从 base LV 派生的 thin snapshot（或同 lineage），否则 thin_send 往往会输出接近 target 已用块的大小，后续“块→文件”会非常难做且收益很低。
- **GC/生命周期**：镜像 committed LVs 应跟随内容/镜像引用生命周期；容器 writable LV 跟随容器删除；快照 LV 跟随一次 commit 操作结束后删除。

---

## 2. 方案 B（你提议的核心）：逐层 replay Apply 到 base LV（“把每层直接 Unpack 到 base LV”）

### 2.1 思路

为某个镜像的 chain（layer descriptors 列表）创建一个空的 base LV，然后：

- 从 **content store** 读取第 1..N 层的 layer blob（tar/gzip/zstd）
- 按顺序对 base LV 的挂载点执行 `archive.Apply`（等价于 containerd unpack 时的 apply）
- 处理 whiteout/opaque 语义（`archive.WithConvertWhiteout(...)` 等）
- 最终 base LV 上的文件树 = 镜像只读层 merged view

### 2.2 这个方案能不能替代“复制 merged view”？

**能**。它和“复制 merged view”的区别在于：  
你不是先得到一个合并挂载点再 `cp -a`，而是**直接重放(replay) unpack/apply**，把 layer tar 里的变更按顺序写进 base LV。

但注意：

- **它依然是全量 I/O（一次“物化”）**：镜像有多少文件、多少字节，base LV 最终都得落盘一次（本质无法避免）。
- **正确性依赖 whiteout/opaque 处理**：必须使用和 containerd 相同/兼容的 whiteout 转换规则。

### 2.3 在 containerd 语义下怎么落地（两种落点）

#### 落点 B1：完全“旁路”containerd unpack（外部/后台构建 base LV）

适用于你当前“先不改 Snapshotter 主流程、先把 diff/commit 跑通”的阶段。

- **输入**：镜像的 layer descriptors（按顺序），content store 可读
- **输出**：`chainID-final → baseLVName` 映射（你自己维护）
- **构建流程**：
  - 创建 base LV + mkfs + mount 到临时目录
  - 依次读取每层 blob（解压）→ `archive.Apply(ctx, mountPoint, tarStream, opts...)`
  - umount，标记 base LV 只读（或仅逻辑只读）

优点：对 snapshotter 侵入小。  
缺点：containerd 并不知道你构建了这个 base LV（需要你在 commit/diff 自己用）。

#### 落点 B2：挂到 unpack 流程里（在 Snapshotter / Applier 层面集成）

如果你希望“拉镜像时就顺便把 base LV 建好”，有两种做法：

- **改 Snapshotter**：在最后一层 Commit 后，额外触发一次“replay apply”构建 base LV（实现简单但会重复 I/O）。
- **改/自定义 Applier**：让 Apply 同时写入两个目标（writable snapshot + base LV）。这能减少一次重复 I/O，但侵入更大。

> 现实建议：除非你确定要做到“极致优化”，否则先选 B1 或 “Commit 后异步 build” 最稳。

### 2.4 方案 B 的优缺点

- **优点**：
  - 不依赖 overlay merged 挂载点（不需要 mount lowerdir/upperdir 去拼合视图）
  - 语义最贴近 OCI layer（就是把 layer tar 依次 apply）
  - 便于做“懒构建”（只对需要 commit 的镜像构建 base LV）
- **缺点**：
  - 仍然需要全量写入一次 base LV（不可避免）
  - 需要处理白化(whiteout)/opaque 细节（否则会和容器视图不一致）
  - 如果要“层复用/缓存”，最终还是会回到方案 A 的“每层 committed 都要可引用”

---

## 3. 方案 C（现有方案）：拿到合并视图（merged view）→ 全量复制到 base LV

这就是你现在文档 `base-lv-creation-plan.md` 的主线思路（修正点：merged view 不能用 `upperPath` 直接当）。

### 3.1 正确的“merged view”来源

- **来自 snapshotter 的 mounts**：用 `mount.WithTempMount(ctx, mounts, func(root string){ ... })` 获取合并后的 rootfs 视图
- **而不是**：直接读取某个 snapshot 的 `upperdir` 目录

### 3.2 优缺点

- **优点**：实现最简单（`cp -a` / `rsync` 就能干）
- **缺点**：
  - 需要一次 overlay 合并挂载（你的 snapshotter 当前对 committed mounts 的实现还需要仔细确认）
  - 同样是全量 I/O（一次“物化”）
  - 复制过程对 xattrs、hardlink、设备文件、白化语义要非常谨慎（否则和 OCI 语义不一致）

> 关于“B 比 C 少一次全量拷贝”的澄清：  
> B 和 C **都需要做一次“把镜像 rootfs 物化到 base LV”的写入**；差异主要在“读路径/成本结构”：
> - B：读 content store 的压缩 layer blob → 解压 → apply 写入 base LV（CPU 较多，避免 overlay 合并挂载）
> - C：读本地已 unpack 的 snapshot 文件树（通过 merged 挂载点暴露）→ copy/rsync 写入 base LV（CPU 较少，但需要一次合并挂载并遍历复制）

---

## 4. 有没有“完全不复制/不重放”的方法？

如果你的目标是 **文件级 diff + OCI layer（包含删除）**，基本不存在“零物化”的捷径：

- **thin_send** 给你的是块级变化，不提供“文件名/路径/删除”语义
- 删除检测要么来自：
  - 基准文件树（base LV/merged view）
  - 或者 overlay upperdir 的 whiteout 元数据（前提：你的 writable 层真的是 overlay upperdir，而不是一个独立 ext4/xfs）

因此，“base LV 从哪里来”本质就是：你要在某个时刻把镜像 rootfs 变成一个可遍历的目录树。

---

## 5. 推荐结论（结合你现在的架构）

### 5.1 如果你愿意把镜像层也迁移到 LVM（长期/最终形态）

选 **方案 A**：

- base LV 直接等于 `chainID-final` 对应的 committed LV
- 容器 writable LV 是它的 thin snapshot
- commit diff：`base LV` vs `writable LV snapshot`

这是最符合 containerd 的方式，后续你实现 `diff.Comparer`/计费/快照 commit 都会更顺。

### 5.2 如果你短期不想大改 Snapshotter（先跑通 diff/commit MVP）

选 **方案 B1**（旁路 replay apply 构建 base LV）：

- 不动 snapshotter 主流程
- 对需要 commit 的场景再构建 base LV（懒构建）
- 等 MVP 跑通后，再考虑是否演进到方案 A


