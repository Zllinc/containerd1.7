# committed snapshots 的 LVM 化（方案 A）：从 Unpack 到 thin_send Diff 的端到端流程
**创建时间**: 2026-01-26  
**目的**: 在不偏离“base LV 从何而来/如何形成”的主题下，给出一条**语义正确、长期可演进**的方案：让 containerd 的 **committed snapshots** 直接落在 **LVM thin** 上，使得  
`base LV = chainID-final 对应的 committed LV`，并在 commit/diff 时用 `thin_send(base, writableSnapshot)` 做块级 diff，再映射为文件差异并复用现有 tar 流程生成 OCI layer。

---

## 1. 目标与非目标

### 1.1 目标
- **base LV 的语义正确**：base 必须代表“镜像只读层最终合并视图（最终 rootfs）”在块设备上的基线。
- **满足 thin_send 的前提**：
  - base/target 都是 **thin volume / thin snapshot**
  - 位于同一个 thinpool
  - target 最好与 base 存在 **lineage/共享块映射**（否则增量会退化）
- **对齐 containerd 模型**：遵循 `Prepare → Apply → Commit` 的 unpack 语义，以及容器创建时 `Prepare(containerKey, chainID-final)` 的语义。
- **最终 diff 产物是 OCI layer**：新增/修改文件写入完整内容，删除写 whiteout，复用 containerd 的 tar/ChangeWriter 逻辑。

### 1.2 非目标（本篇不展开）
- 不讨论“短期 MVP 旁路构建 base LV（拷贝 merged view / replay apply）”。
- 不解决“块→文件映射”的具体实现细节（debugfs/fiemap 等），只定义接口与数据流边界。
- 不讨论完整 GC/引用计数的落地实现细节（只给出必须的生命周期约束）。

---

## 2. 核心结论（先给答案）

在 committed snapshots LVM 化的终局里：

- **base LV 从哪里来？**
  - **base LV = 镜像 unpack 完成后，`chainID-final` 对应的 committed snapshot 的 LV（只读）**
- **write LV 从哪里来？**
  - 容器创建时：`write LV = thinSnapshot(base LV)`（可写）
- **diff 时 target 端怎么取？**
  - 对 `write LV` 再做一个**只读 thin snapshot**（保证一致性），记为 `writeSnap LV`
- **块级 diff 怎么做？**
  - `thin_send(base LV, writeSnap LV)` → 块差异流
- **怎么变成 OCI layer？**
  - 块差异 → 文件差异（Add/Modify/Delete）
  - 复用 `archive.ChangeWriter` 生成 layer tar（含 whiteout）

这条链路之所以“正统”，是因为它让块级 diff 的两端都代表 **完整 rootfs 的块设备视图**，不会出现“拿 upperdir 的块数据却想解释成 rootfs 变化”的语义错位。

---

## 3. 数据模型：chainID / key 与 LV 的映射

Snapshotter 需要维护两类映射（持久化到 metadata store，例如 bolt）：

- **committed 映射**：`chainID -> lvName`
  - `chainID` 是 containerd 用于标识“父链 + 当前层”的确定性 ID（最终 rootfs 的标识就是 `chainID-final`）
  - `lvName` 是对应的 LVM thin LV 名称（建议做短 hash，避免 LVM 名称长度限制）

- **active 映射**：`key -> lvName`
  - `key` 是临时标识（unpack 的 `extract-...` 或容器的 `containerID`）
  - `lvName` 是 active 阶段可写 LV（最终 commit 后会成为 committed 或被清理）

命名建议（示例）：
- committed LV：`img-<chainid-short>`
- unpack active LV：`unpack-<rand>-<chainid-short>`
- container active LV：`ctr-<containerid-short>`
- container diff snapshot LV：`ctr-<containerid-short>-diffsnap-<ts>`

---

## 4. 镜像拉取后的 Unpack：Prepare → Apply → Commit 如何落到 LVM

containerd 的 unpack 核心循环（简化）是：

```
for layer in layers:
  mounts = snapshotter.Prepare(key, parentChainID)
  applier.Apply(layer, mounts)
  snapshotter.Commit(chainID, key)
```

要实现 committed snapshots 的 LVM 化，本质是把每次 `Prepare/Commit` 对应到 “LVM thin snapshot 链的一步”。

### 4.1 Prepare(key, parentChainID)：创建本层 active LV

#### 情况 A：第一层（parentChainID == ""）
- 创建一个新的 thin volume：`lv = thinVolume()`（虚拟大小按策略）
- 在该 LV 上 `mkfs`（ext4/xfs 二选一）
- 返回 mounts：让 applier 把 layer tar 解到该文件系统根

#### 情况 B：后续层（parentChainID != ""）
- 查表得到 `parentLV = lvOf(parentChainID)`
- 创建 `lv = thinSnapshot(parentLV)`（关键：让本层继承父层的块视图）
  - 这里的 `parentLV` 既可能是“thin volume”（第一层 committed），也可能是“thin snapshot”（后续层 committed）。
  - **LVM thin 支持“快照套快照”**：thin snapshot 可以继续作为 origin 再创建 thin snapshot（会形成一条快照链）。
- **不要 mkfs**（因为 thin snapshot 继承父文件系统）
- 返回 mounts：让 applier 把本层 layer tar “应用”到这个 active LV

> 重要澄清（避免误解）：
> - Unpack 并不是“永远只有一个 active LV，然后每层都对同一个 LV 做快照再写回去”。
> - 正确模型是：**每处理一层，就创建一个新的 active LV（它是 parent committed LV 的 thin snapshot）**；Apply 只写当前 active LV；Commit 后该 active LV 变成该层的 committed LV，并作为下一层的 parent。

#### mounts 的形态（建议）
为了让 containerd 的通用 applier 通过 `mount.WithTempMount` 挂载并写入，你应该返回“可挂载块设备”的 mounts，例如：
- `Type: "ext4"`（或 `"xfs"`，取决于你的 mkfs）
- `Source: "/dev/<vg>/<lvName>"`
- `Options: ["rw"]`

> 关键点：这里不再需要 overlay lowerdir/upperdir/workdir。  
> 每个 committed LV 本身就是“完整视图 + 本层变更”的块级快照，挂载该 LV 看到的就是完整 rootfs。

### 4.2 Apply(layerBlob, mounts)：把本层 tar 解到 active LV

applier 的行为不需要你改：
- 它会把 mounts 挂到临时目录（`mount.WithTempMount`）
- 然后 `archive.Apply(ctx, root, tarStream, ...)` 写入文件系统

写入的结果：
- 只会在当前 `lv` 上产生 COW（thinpool 分配新块）
- 父 LV 不变

### 4.3 Commit(chainID, key)：把 active LV 固化为 committed

Commit 要做两件事：
- **元数据**：把 snapshot 从 KindActive 变为 KindCommitted，并记录 parent 关系（containerd 语义）
- **LVM 映射**：更新 `chainID -> lvName`（committed 映射），并清理 `key -> lvName`（active 映射）

是否需要把 LV 设为只读：
- 可以在 LVM 层设为只读（更强约束），也可以只在逻辑上保证 “committed 不再写”。

完成后，镜像的每个 `chainID` 都有一个对应的 committed thin LV。最后一层的 `chainID-final` 对应的 LV，就是你要的 base LV。

---

## 5. 容器创建：write LV 怎么来（必须满足 thin_send 的 lineage）

容器创建的关键动作是：

```
snapshotter.Prepare(containerKey, chainID-final)
```

在 LVM 化方案里：
- 查表得到 `baseLV = lvOf(chainID-final)`（只读 committed）
- 创建 `writeLV = thinSnapshot(baseLV)`（可写）
- 返回 mounts 指向 `writeLV`（Type=ext4/xfs, Source=/dev/vg/writeLV）
- 记录 `containerKey -> writeLV` 映射（active）

为什么必须是 `thinSnapshot(baseLV)`：
- 这是让 `writeLV` 与 `baseLV` 共享大量块映射的唯一“干净方式”
- 后续 `thin_send(baseLV, writeSnapLV)` 才会真正输出“增量块”

---

## 6. Diff/Commit：thin_send 到 OCI layer 的完整流程

这里的“diff/commit”指 containerd 的 diff-service / `diff.Comparer` 语义（即 `Comparer.Compare(lower, upper)` 生成 layer），而不是 snapshotter 的 `Commit()`（它主要是元数据状态转换）。

### 6.1 containerd diff 的输入：lower mounts 与 upper mounts

在 containerd 的调用链里（参考 `containerd-diff-mechanism.md`）：
- `lower` 来自 `sn.View(parentKey, parent)`（父层视图）
- `upper` 来自 `sn.Mounts(activeKey)`（当前层）

在 LVM 化方案中：
- `lower mounts` 指向 **baseLV（chainID-final 的 committed LV）**
- `upper mounts` 指向 **writeLV（容器可写 thin snapshot）**

于是 LVM differ 可以从 mounts 反推出两端设备路径：
- base: `/dev/<vg>/<baseLV>`
- write: `/dev/<vg>/<writeLV>`

### 6.2 为一致性创建只读快照：writeSnap LV

diff/commit 时容器可能仍在写入（你也希望“不停容器 commit”）。  
因此需要：
- `writeSnapLV = thinSnapshot(writeLV)`（并标记只读或只读挂载）
- diff 全程只读取 `writeSnapLV`
- 完成后删除 `writeSnapLV`

### 6.3 生成块级差异流：thin_send(base, writeSnap)

执行：
```
thin_send /dev/<vg>/<baseLV> /dev/<vg>/<writeSnapLV>  > stream
```

输出是二进制块级增量流（包含变化块的元数据与数据内容），不是文件级差异。

### 6.4 块差异 → 文件差异（边界定义）

你最终需要一个“文件变化列表”：
- Add/Modify：提供路径 + 读取新内容的入口（通常来自挂载后的 `writeSnapLV`）
- Delete：提供路径（写 whiteout）

建议把这一步做成两个子阶段（接口化）：

1) **解析 thin_send 流**：得到“变化块集合/范围”
2) **块→文件映射**：在 `writeSnapLV` 的文件系统中，把“物理块变化”映射到 inode，再映射到路径

> 注意：删除语义不能只依赖块变化（删除可能只改元数据，路径解析也会困难）。最终一定要能产出 Delete 列表，写入 whiteout。

### 6.5 复用 containerd 的 tar 流程：ChangeWriter

产出文件变化列表后，复用现有逻辑写 OCI layer：
- Add/Modify：把 `writeSnap` 挂载为 `upperRoot`，对每个变化调用 `ChangeWriter.HandleChange(Add/Modify, path, info, nil)` 写入完整文件内容
- Delete：调用 `HandleChange(Delete, path, info, nil)` 让 ChangeWriter 写 whiteout

最终：
- 写入 content store
- 计算 digest
- 设置 `containerd.io/uncompressed` label
- 返回 `ocispec.Descriptor`

---

## 7. 为什么这条路线能把“base LV 从哪里来”彻底讲清楚

因为在这条路线里：

- **镜像最终 rootfs 不是一个“目录合并视图”，而是一个“可挂载块设备视图”**
- 这个块设备视图由 containerd 的 unpack 语义自然生成：最后一层 committed snapshot 的 LV
- 因此 base LV 不需要“复制 merged view”、也不需要“replay apply 旁路构建”，它就是 committed snapshots 的自然产物

换句话说：  
> base LV 从“镜像的 committed snapshot（chainID-final）”来，而 committed snapshot 从 containerd 的 `Prepare→Apply→Commit` 流程来；要让 base LV 存在，就必须让 committed snapshots 本身落在 LVM thin LV 上。

---

## 8. 必须提前评估的工程影响（不偏离主题，但必须提醒）

### 8.1 快照链长度
每层都是 thin snapshot 会形成链；读取可能变慢。需要评估：
- 是否需要定期 flatten（把链顶端“重基线”成一个新 thin volume）
- 或者在某些场景做 merge/重打基线

### 8.2 文件系统选择
ext4/xfs 影响后续“块→文件映射”的工具链（debugfs vs xfs_db/fiemap）。

### 8.3 GC 与引用关系
必须保证：
- committed LVs 的生命周期跟随镜像内容引用（content/lease/GC）
- container write LVs 跟随容器生命周期
- diff 临时快照 LV（writeSnap）必须在一次 diff 完成后删除

---

## 9. 总结（一句话）

**committed snapshots 的 LVM 化**的本质是：把 containerd 的 “每层 committed snapshot” 变成 “父 LV 的 thin snapshot + 本层 Apply 写入”，从而让最后一层 `chainID-final` 自然成为 **base LV**；容器 write LV 从 base LV 派生，diff 时对 write LV 再打只读快照，`thin_send(base, writeSnap)` 得到块级增量，再映射为文件变化并复用 tar 流程生成 OCI layer。

---

## 10. 对 containerd 其它模块/生命周期的影响与需要改动点（全流程评估）

你问的核心是：**如果 committed snapshots 全部落在 LVM 上，那么 containerd 的 snapshot 目录里是否就没有 overlayfs 那种“解压后的目录树数据”了？会不会影响其它模块？**

### 10.1 `snapshots/` 目录里会变成什么样？

结论：
- **不会再有 overlayfs snapshotter 那种“每层一棵可直接访问的 upperdir 目录树”**（因为不再使用 overlay 的 upperdir/lowerdir 目录模型）。
- 但 snapshotter 仍然会维护 `root/snapshots/<id>/` 这样的目录结构作为**挂载点**（mountpoint）与元数据管理的锚点：
  - 当 LV 挂载在 `root/snapshots/<id>/` 时，你“看到的文件树”来自 LV 的 ext4/xfs 文件系统。
  - 当卸载后，这个目录可能是空的（只剩目录本身）。

因此，“解压后的数据”仍然存在，但**不再以普通目录树的形式常驻在宿主机文件系统**，而是存在于 **LV 的块设备**里。

### 10.2 对 containerd 核心生命周期的影响（Pull/Unpack → Run → Diff/Commit → Stop/Restart → Remove/GC）

下面按阶段给出影响与改动点：

#### A) Pull/Unpack（Prepare → Apply → Commit）
- **原 overlayfs 行为**：
  - `Prepare` 返回 overlay mounts（含 upperdir/workdir/lowerdir）。
  - `Apply` 走 overlay 特化路径：直接解到 upperdir（普通目录）。
- **LVM 化后的行为**：
  - `Prepare` 返回“可挂载块设备”的 mounts（ext4/xfs，Source=/dev/vg/lv）。
  - `Apply` 会走通用路径：`mount.WithTempMount(mounts, ...)` 后 `archive.Apply(root, ...)`。
  - 每层会创建一个新的 active LV（thinSnapshot(parentLV) 或 thinVolume），Apply 只写该 LV，Commit 固化为 committed LV。

**影响点**：
- containerd 的 applier/diff/apply 不需要改（通用路径天然支持），但失去 overlay 特化的“直写 upperdir”优化。
- snapshotter 必须严格实现 containerd 的 parent/chainID 语义，否则镜像层复用与父链解析会出错。

#### B) 容器创建/运行（基于 chainID-final）
- **原 overlayfs 行为**：运行时根文件系统是 overlay 合并视图。
- **LVM 化后的行为**：
  - 容器 active snapshot 的 LV 是 `thinSnapshot(baseLV)`，容器直接在该 LV 文件系统上读写（不需要 overlay 合并）。

**影响点**：
- 容器 runtime/CRI 的“拿 mounts 挂 rootfs”逻辑不变：依旧消费 snapshotter 返回的 mounts。
- 如果你之前依赖 overlay 的 whiteout/upperdir 语义做增量（比如 overlayfs differ），那条路径不再适用。

#### C) Diff/Commit（生成 OCI layer）
- **原 overlayfs differ**：
  - 依赖 upperdir + whiteout 的目录语义生成 layer（并对 lower 做临时挂载用于判定）。
- **LVM 化后的目标行为**：
  - 以 `baseLV` vs `writeSnapLV` 做 `thin_send`，再映射为文件变化，复用 ChangeWriter 写 layer。

**影响点**：
- diff-service 需要有一个“LVM differ”（或更通用：能识别你的 mounts 并选择 LVM 路径的 comparer）。
- snapshotter 的 `Commit()` 仍然主要是元数据转换；“生成 layer”属于 diff-service 的职责。

#### D) Stop/Restart（容器停止与再次启动）
- **LVM 化后的直觉变化**：
  - 如果容器停止后你选择卸载 LV，目录挂载点会变空；但 LV 数据依旧存在（块设备）。
  - 再次启动时重新挂载同一个 active LV 即可恢复。

**影响点**：
- snapshotter 需要可靠的“挂载点恢复”能力：`Mounts(key)` 应该能在进程重启后重建 mounts（通过 metadata 找回 lvName）。

#### E) Remove/GC（镜像删除、容器删除、空间回收）
这是 LVM 化最容易踩坑的部分：以前 overlayfs 的“目录删除”很直接；现在需要对 LVs 做引用管理。

必须满足：
- **committed LVs（镜像层）**：
  - 生命周期跟随 content/镜像引用（通过 containerd 的 metadata/lease/GC 体系）。
  - 当某个 chainID 不再被任何镜像引用且没有 active 从它派生时，才能删除对应 LV。
- **active LVs（容器可写层）**：
  - 跟随容器生命周期删除。
- **临时 diff 快照 LVs（writeSnap）**：
  - 必须在一次 diff 完成后删除，避免泄漏。

实现上需要：
- 在 snapshotter metadata 中维护 `chainID -> lvName` 与引用关系/引用计数（或可推导的引用图）。
- `Remove(key)` 与 `Cleanup()` 需要能扫描“元数据已无引用但 LV 仍存在”的孤儿 LV 并回收。

### 10.3 对 containerd 其它模块的影响（按模块列举）

#### 10.3.1 Content Store
- **无影响**：仍然存储压缩 layer blobs（tar.gz/zstd），与 snapshotter 存储介质无关。

#### 10.3.2 Metadata / Leases / GC
- **接口不变，但你的 snapshotter 必须正确配合**：
  - unpack 时 containerd 依赖 lease 防 GC；
  - snapshotter 必须确保 committed snapshots 的 parent/chainID 正确，GC 才能正确判断可回收对象。

#### 10.3.3 Applier（解包）
- **逻辑无影响**，但从 overlay 特化回到通用挂载路径，可能带来性能差异（多一次挂载步骤）。

#### 10.3.4 Diff service（Comparer）
- **需要新增/替换 differ**：overlayfs differ 不再适用，需要实现 LVM differ（或让 walking differ 作为兜底，但它是文件级全遍历，不符合你要的块级路线）。

#### 10.3.5 snapshotter 自身（需要改动最多）
需要实现/修改：
- `Prepare/View/Mounts/Commit/Remove/Cleanup` 的语义从“overlay 目录层叠”切换为“LVM thin snapshot 链”。
- overlay 相关的 mount options（`userxattr/index=off` 等）不再是核心；相应地要关注块设备挂载选项与 fs 类型（ext4/xfs）。
- `Usage` 统计方式：不再是扫描 upperdir 目录；更适合从 LVM（如 `lvs data_percent`）或挂载后 `statfs` 获取。

### 10.4 需要改动的点清单（落地检查表）

如果你把当前 devbox snapshotter 演进到“committed snapshots LVM 化”，至少需要做这些改动：

- **Snapshotter mounts 形态改造**：
  - committed 与 active 的 `Mounts()` 都返回 block-device mount（ext4/xfs），不再返回 overlay mount。
- **Prepare/Commit 逻辑改造（unpack 路径）**：
  - `Prepare(key, parentChainID)`：创建 `thinVolume` 或 `thinSnapshot(parentLV)`，挂载点返回给 applier。
  - `Commit(chainID, key)`：固化映射 `chainID -> lvName`，并保证 committed 只读语义。
- **容器 active 创建改造**：
  - `Prepare(containerKey, chainID-final)`：`writeLV = thinSnapshot(baseLV)`。
- **Diff/Comparer 新实现**：
  - 识别 base/write 的 LV 路径；
  - diff 时创建 `writeSnapLV`；
  - `thin_send(base, writeSnap)`；
  - 块→文件→ChangeWriter→OCI layer。
- **GC/Remove/Cleanup 完整闭环**：
  - 跟随 snapshot 元数据删除/回收 LV；
  - 扫描孤儿 LV；
  - 确保临时 diff snapshot 不泄漏。

---

## 11. 替代方案：单 thin LV + Flatten Unpack（只产出 `chainID-final` 的 base LV）

> 背景：你希望减少 LVM 操作次数（避免线上阻塞），并且业务上不强调“镜像层复用/保留每层 committed snapshot”。  
> 因此选择把镜像只读层 **flatten** 成“最终 rootfs 的一个 base LV”，容器只保留两层：`baseLV` + `writeLV`。

### 11.1 方案目标与关键取舍

- **目标**：
  - 镜像 unpack 阶段只创建 **1 个 active snapshot**（对应 1 个 thin LV）；
  - 将镜像所有层按顺序 `Apply` 到同一个挂载点（同一个 LV 文件系统根）；
  - 最终只 `Commit(chainID-final, key)`，只保证 `chainID-final` 这个 committed snapshot 存在；
  - 容器运行时只派生 `writeLV = thinSnapshot(baseLV)`；
  - diff 时 `thin_send(baseLV, writeSnapLV)`。
- **取舍**（必须接受）：
  - 不再物化/保留 `chainID-1..chainID-(n-1)` 对应的 committed snapshots（等价于放弃层级复用与中间层可挂载调试能力）。

### 11.2 为什么“只改 snapshotter”不够？（必须改 unpack/apply 流程）

containerd 默认 unpack 是“逐层 committed”的：每层都会 `Prepare→Apply→Commit(chainID-i)`，并且下一层 parent 指向上一层 `chainID-(i-1)`。

如果你“始终只写同一个 LV”，那早先已经 `Commit(chainID-1/2/...)` 的 committed 内容会被后续层继续写入而改变，破坏 **committed snapshot 不可变** 的基本语义。  
因此要实现真正的 Flatten，必须让 unpack 流程变成“只在最后 commit 一次（chainID-final）”。

### 11.3 Flatten Unpack 的端到端流程（我们需要对齐的每一步）

下面按“镜像 pull/unpack → 容器创建/运行 → diff/commit → GC”顺序描述。

#### A) Pull/Unpack（Flatten 模式）

**输入**：
- 镜像 config（含 `rootfs.diff_ids`）
- layers blobs（压缩 tar）
- snapshotter（devbox 或未来 LVM snapshotter）

**步骤**：
1. 读取 image config，拿到 `diffIDs := config.RootFS.DiffIDs`（未压缩 digest 列表）。
2. 预计算：
   - `finalChainID := identity.ChainID(diffIDs).String()`
3. 复用判断：
   - `if sn.Stat(finalChainID) exists`：说明最终 base 已存在，**直接返回**（跳过整个 apply）。
4. 创建唯一临时 key（防并发冲突）：
   - `key := "extract-<rand>-<finalChainID>"`
5. `mounts := sn.Prepare(ctx, key, parent="")`：
   - snapshotter 为这个 key 创建 **一个 thin LV**（`baseLV_tmp`），`mkfs`，返回 block-device mounts；
   - 这一步之后，我们会反复把所有 layer 依次 apply 到同一个 mounts 指向的 rootfs。
6. 对每一层按顺序执行：
   - `a.Apply(ctx, layerBlob, mounts, ...)`
   - containerd 会在 apply 后校验该层解压得到的 diffID 与 config 里的 diffID 一致（正确性校验）。
7. 所有层成功后：
   - `sn.Commit(ctx, name=finalChainID, key)`：
   - 把这个 active snapshot 固化为 committed snapshot；
   - **最终产物只有一个 committed snapshot：`finalChainID`**，对应 `baseLV`。

**输出**：
- `chainID-final` 对应的 committed snapshot（base LV）
- content store 里的每层 blob 仍会写 `containerd.io/uncompressed = diffID` label（与是否 flatten 无冲突）

#### B) 容器创建/运行（只有 baseLV + writeLV）

**输入**：
- `parent = finalChainID`（镜像最终 rootfs）
- `containerKey`

**步骤**：
1. `sn.Prepare(ctx, containerKey, parent=finalChainID)`：
   - snapshotter 查到 `baseLV = lvOf(finalChainID)`；
   - 创建 `writeLV = thinSnapshot(baseLV)`（可写）；
   - 返回 mounts 指向 `writeLV`（ext4/xfs, Source=/dev/vg/writeLV）。
2. runtime 将 `writeLV` 挂载为容器 rootfs，容器在其上读写。

**输出**：
- 容器 rootfs 只挂载一个文件系统（writeLV），没有 overlay lower/upper/work。

#### C) Diff/Commit（生成 OCI layer）

**输入**：
- `lower = baseLV`（finalChainID）
- `upper = writeLV`（容器可写层）

**步骤**：
1. 为一致性创建只读快照：
   - `writeSnapLV = thinSnapshot(writeLV)`（只读）
2. 块级 diff：
   - `thin_send(baseLV, writeSnapLV)` → 块差异流
3. 块→文件：
   - 把块差异映射为文件变更列表（Add/Modify/Delete）
4. 写 OCI layer：
   - 用 `archive.ChangeWriter` 写入 tar（含 whiteout）
5. 清理：
   - 删除 `writeSnapLV`（临时快照必须不泄漏）

#### D) GC/Remove/Cleanup（最小集合）

Flatten 模式的引用关系更简单：
- 镜像层：只需要管理 `finalChainID -> baseLV`；
- 容器层：`containerKey -> writeLV`；
- diff 临时：`writeSnapLV` 生命周期仅限一次 diff。

### 11.4 为什么 writeLV 必须从 baseLV 的快照派生？（回答你问的 2/3）

在“只有两层（base + write）”的世界里，你需要满足两个性质：

1) **读路径能看到 base 的数据**（当 write 没有覆盖某些块/文件时）  
2) **写入不会改坏 base**（COW：写时复制）

`writeLV = thinSnapshot(baseLV)` 同时满足这两点：
- **共享读取**：writeLV 初始时块映射指向 baseLV 的物理块；读取未修改块时直接读 base 的块。
- **写时复制**：当容器写入某个块时，thinpool 分配新块，writeLV 的映射指向新块；baseLV 不变。

如果 writeLV 不是 baseLV 的快照（例如一个独立空 LV），那么：
- 你要想“读到 base 里存在但 write 里不存在的数据”，就必须引入额外机制：
  - 要么 overlay/union（回到 overlayfs 思路）；
  - 要么在 writeLV 里预先复制 base 的全部数据（等价于 full copy，I/O 极大且失去 thin_send 的 lineage 优势）。

因此，在“只剩两层”且你希望保持高效的前提下，**writeLV 从 baseLV 派生是最自然、也是几乎唯一的正确连接方式**。

---

## 12. 实现约束（按当前对齐）

为避免后续偏离主题，代码实现阶段需要遵守以下约束：

1) **Diff service**  
   - 重新实现一套 `Compare` / `Apply`，不修改现有 diff 实现（overlayfs/walking 等）。  
   - 新实现应以 **独立 diff plugin** 的方式接入，符合 containerd 插件规范（可在配置中启用）。

2) **Snapshotter**  
   - 基于现有 **devbox snapshotter** 修改，不新写一个 LVM snapshotter。  
   - 仅在必要时扩展 devbox 的逻辑与元数据，不引入额外无关能力。

3) **最小功能原则**  
   - 仅实现当前方案所需的功能与数据流，不提前加入未验证/无需求的功能。




