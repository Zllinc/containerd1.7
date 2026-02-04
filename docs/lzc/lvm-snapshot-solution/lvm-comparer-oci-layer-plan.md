# LVM Comparer: 生成 OCI layer 的方案

**创建时间**: 2026-01-23  
**目标**: 实现 `diff.Comparer`（LVM 版），将两个 LV 的差异打包为 OCI layer（tar/tar.gz）。

---

## 1. 目标与约束

**目标**  
实现一个 LVM 版 `diff.Comparer`，输入 `lower` 与 `upper` 的 mounts，输出符合 OCI 规范的 layer（tar 或 tar.gz），并写入 containerd content store。

**约束**  
1. **暂不修改 Snapshotter**，仅实现 Comparer。  
2. **diff 数据来源**：使用 `thin_send` 输出（块级 diff）。  
3. **OCI layer 格式**：需要新增/修改文件完整内容 + whiteout 标记删除。  

---

## 2. 现有实现参考（要对齐的行为）

`diff/walking/differ.go` 与 `diff/overlayfs/differ.go` 是参考模板：

核心模式统一：
1. **打开 content store writer**
2. **确定压缩格式**
3. **写入 tar**
4. **计算 digest**
5. **设置 label `containerd.io/uncompressed`**
6. **Commit descriptor**

---

## 3. LVM Comparer 总体流程（端到端）

```
Compare(ctx, lower, upper, opts...)
    ↓
1. 解析 config (mediaType, compressor, labels, sourceDateEpoch)
2. 从 mounts 提取 base LV 与 writable LV
3. 对 writable LV 创建只读 snapshot (writable-snap)
4. 使用 thin_send(baseLV, writable-snap) 获取块级变化流
5. 解析块级变化 → 得到“变化块列表”
6. 块映射到文件 → 得到“文件变化列表”
7. 将文件变化列表写入 OCI layer (tar/tar.gz)
8. 写入 content store, 计算 digest, 设置 labels
9. 返回 ocispec.Descriptor
```

关键步骤：**5 与 6 是 LVM 特有差异**，其余步骤可以复用 `walking/overlayfs` 的内容写入逻辑。

---

## 4. LVM Comparer 需要实现的核心点

### 4.1 从 mounts 提取 LV 信息

**目标**：解析 `lower` 与 `upper` 的 `[]mount.Mount`，提取出 LV 路径。

示例：
```
upper mount:
Type: "bind"
Source: "/var/lib/containerd/devbox/mnt/writable-lv-123"

lower mount:
Type: "bind"
Source: "/var/lib/containerd/devbox/mnt/base-lv-abc"
```

**处理方式**：
1. 如果 mount.Source 是“已经挂载的目录”，则反查其 block device。
2. 如果 mount.Source 直接是 `/dev/vg/lv`，直接使用。

（此处应结合 Devbox snapshotter 当前挂载路径规则）

---

### 4.1.1 你的提议：基于 lowdir 预构建 base LV（可行）

**思路**  
在镜像 unpack 时，将低层（lowdir 的合并视图或最上层只读层）写入一个 base LV，使其成为稳定的“基准层”。之后 commit 时直接用 `base LV` 与 `writable LV snapshot` 做 diff。

**可行性**  
可行，但需要明确两个点：
1. **base LV 必须代表 lowdir 的完整内容**（不是增量）。
2. **base LV 的创建必须与镜像层版本绑定**（同一镜像可复用）。

**建议的创建时机与流程**  
```
镜像 unpack 阶段:
1) 选择一个临时目录 tempLower
2) 将 lowdir 的合并视图挂载到 tempLower
3) 创建 base LV，并格式化（ext4/xfs）
4) 挂载 base LV 到 tempBase
5) cp -a tempLower/. -> tempBase/
6) umount tempLower, umount tempBase
7) base LV 完成，可复用
```

**注意**  
- 必须使用“合并视图”，否则只拷贝最上层增量会丢失 lower 层内容。  
- 如果只拷贝“最上层只读层”而不是完整合并视图，那么 base LV 无法代表 lowdir，会导致 diff 结果错误。

---

### 4.1.2 Unpack / Apply 的真实调用流程（为什么可行）

**结论**：你的方案“unpack 时直接把 layer apply 到 base LV”是**可行**的，但需要在 snapshotter + applier 的边界上正确接入。下面是实际的调用链说明：

#### (1) Unpack 入口：逐层 apply

在 `image.Unpack` 中，containerd 会遍历 layers 并调用 `rootfs.ApplyLayerWithOpts`：  
```
for _, layer := range layers {
    unpacked, err = rootfs.ApplyLayerWithOpts(ctx, layer, chain, sn, a, ...)
}
```
这是每层 apply 的入口。（对应 `image.go` 的 `Unpack` 实现）

#### (2) rootfs.ApplyLayerWithOpts → applyLayers

在 `rootfs/apply.go` 中，会为每一层创建一个临时 snapshot（`sn.Prepare`），然后调用 applier：
```
mounts, err = sn.Prepare(ctx, key, parent.String(), opts...)
diff, err = a.Apply(ctx, layer.Blob, mounts, applyOpts...)
sn.Commit(ctx, chainID.String(), key, opts...)
```
这意味着：**apply 的实际写入目标就是 snapshotter 返回的 mounts**。

#### (3) diff/apply: 真实写入路径

在 `diff/apply/apply_linux.go`：
- 对于 overlay/aufs，有专门的优化路径（直接写 upperdir）。
- 其他情况走 `mount.WithTempMount`，把 mounts 挂载到临时目录后调用 `archive.Apply`。

关键逻辑：
```
return mount.WithTempMount(ctx, mounts, func(root string) error {
    _, err := archive.Apply(ctx, root, r)
    return err
})
```
所以**写入目标始终由 mounts 决定**。

---

### 4.1.3 评估你的方案是否可行

你的方案描述：
```
unpack 开始时:
1) 创建 base LV
2) 挂载 base LV 到临时目录
3) 直接把 lowdir apply 到该目录
```

**可行性结论**：可行，但需要满足以下条件：

1. **你要让 applier 直接写入 base LV 的挂载点**  
   - apply 的入口只接受 `mounts []mount.Mount`  
   - 也就是说你需要在 unpack 阶段让 snapshotter 返回“指向 base LV 的 mounts”

2. **必须确保 base LV 对应的是 lowdir 的完整合并视图**  
   - apply 是逐层叠加  
   - 如果每一层都 apply 到同一个 base LV，最终结果就等价于完整 lowdir

3. **关键风险：需要改动 unpack/prepare 的 mount 语义**  
   - 当前流程是 `sn.Prepare` 为每层创建一个独立 snapshot  
   - 你如果让它直接写 base LV，就等于改变 snapshotter 行为  
   - 这意味着你需要在 snapshotter 或 unpack 逻辑中引入“base LV 模式”

**更稳妥的落地方式（推荐）**：
```
方案 A（安全）：
1) 正常 unpack（保持 containerd 原流程）
2) 在 unpack 结束后，挂载 merged view
3) 拷贝到 base LV（当前文档中的方式）

方案 B（侵入）：
1) 修改 snapshotter: 在 unpack 时直接返回 base LV mounts
2) 让 applier 直接写 base LV
3) 需要保证对其他 snapshotter 行为无副作用
```

**结论**：你的方案 **逻辑上可行**，但需要侵入 snapshotter 或 unpack 流程；如果你暂不改 snapshotter，那么仍建议先使用“unpack 后拷贝”的方式。

### 4.2 生成稳定的 diff 视图（快照）

**目标**：确保 diff 过程中 `upper` 数据稳定不变。

流程：
```
upper LV (writable-lv)
    ↓ create thin snapshot
writable-lv-snap (readonly)
    ↓ 作为 diff 的 upper 端
```

**原因**：提交时容器仍可能写入，否则 diff 结果不一致。

---

### 4.3 生成块级 diff（thin_send）

```
thin_send /dev/vg/base-lv /dev/vg/writable-lv-snap > stream
```

输出是 block-level 的增量数据流，不是文件级变化。

---

### 4.4 块 → 文件映射（关键难点）

**输入**：变化块列表  
**输出**：文件变化列表（Add/Modify/Delete）

必须解决的两个问题：

1. **哪些文件被修改/新增？**  
   - 通过块变化 → 文件映射获取。
2. **哪些文件被删除？**  
   - block diff 无法直接告诉删除。  
   - 需要补充策略（见下文 4.5）。

**可行方案**：

**方案 A: 基于文件系统元数据解析**
- 挂载 snapshot 到临时目录（只读）。  
- 使用 `debugfs` / `xfs_db` / `filefrag` 将块号映射到 inode。  
- 再从 inode → 文件路径。  

**方案 B: 结合目录全量遍历**
- 使用块映射筛选“可能变化的文件”  
- 对这些文件做内容对比或 hash 校验  

> 当前待完成任务：
> - “解析 thin_send 输出（二进制流 → 块列表）”
> - “块到文件映射（debugfs/xfs_db）”

---

### 4.5 删除检测策略

**问题**：`thin_send` 只反映“块被写入”，但删除文件时可能不写块（或只修改元数据）。

**必须补充的删除检测手段**：

**策略 1: 目录树对比**
- 挂载 base LV 与 writable snapshot  
- 做目录树对比（类似 `archive.WriteDiff` 的 walking 模式）  
- 只做删除检测，不做完整 diff  

**策略 2: 增量索引**
- 在 LV 层维护元数据索引（例如 inode list + hash）  
- 提交时根据索引判断删除（目前还未实现）  

**推荐**：先使用 **策略 1**，成本低且稳定。

---

### 4.5.1 你的问题：能否直接用 base LV vs writable LV 做删除检测？

**答案**：可以，但必须满足两个条件：
1. base LV **确实包含 lowdir 的完整内容**（见 4.1.1）。  
2. compare 的上层必须是 **writable LV 的 snapshot**（保证稳定）。  

**操作方式（删除检测专用）**：
```
lowerRoot = mount(base LV)
upperRoot = mount(writable-snapshot LV)

// 只检测删除，不做完整 diff:
filepath.Walk(lowerRoot) {
    if path 不在 upperRoot:
        emit Delete (whiteout)
}
```

**为什么不能只依赖 thin_send**  
删除文件可能只修改目录项或 inode 元数据，并不一定触发数据块写入。  
因此必须通过“目录级对比”或“元数据索引”补充删除检测。

---

## 5. 生成 OCI layer（tar / tar.gz）

### 5.1 复用 ChangeWriter（关键）

`archive.NewChangeWriter` 已封装：
- whiteout 生成  
- tar header  
- 硬链接处理  
- 父目录补齐  
- 路径标准化  

**使用方式**：
```go
cw := archive.NewChangeWriter(writer, upperRoot, opts...)
cw.HandleChange(kind, path, fileInfo, nil)
cw.Close()
```

**因此 LVM Comparer 只需要提供**：
```
[]Change{Kind, Path, FileInfo}
```

---

### 5.2 层打包的标准流程（对齐 walking/overlayfs）

流程和 `walking/overlayfs` 保持一致：

```
1. 打开 content store writer
2. 按 mediaType 选择压缩 (gzip / zstd / none)
3. io.MultiWriter(compressed, dgstr.Hash())
4. 写 tar 内容
5. 记录 labels.LabelUncompressed
6. Commit content store
7. 返回 ocispec.Descriptor
```

**关键点**：
- `labels.LabelUncompressed` 必须记录未压缩层的 digest  
- `SourceDateEpoch` 用于可重复构建（whiteout 时间戳上界）  
- `MediaType` 需要和压缩格式匹配  

---

## 6. LVM Comparer 的实现骨架（伪代码）

```go
type lvmDiff struct {
    store content.Store
}

func (d *lvmDiff) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (ocispec.Descriptor, error) {
    // 1. 解析 config
    var config diff.Config
    for _, opt := range opts { opt(&config) }

    // 2. 解析 LV 路径
    baseLV := extractLV(lower)
    writableLV := extractLV(upper)

    // 3. 创建 snapshot
    snapLV := createSnapshot(writableLV)
    defer removeSnapshot(snapLV)

    // 4. 生成块级 diff
    stream := runThinSend(baseLV, snapLV)

    // 5. 解析块变化
    blocks := parseThinSend(stream) // TODO

    // 6. 块 → 文件映射
    changes := mapBlocksToFiles(blocks, snapLV) // TODO

    // 7. 补充删除检测（必要）
    deletions := detectDeletes(baseLV, snapLV) // walking-only for deletes
    changes = merge(changes, deletions)

    // 8. 创建 content store writer + tar writer
    cw := store.Writer(...)
    writer := maybeCompress(cw, config.MediaType)
    changeWriter := archive.NewChangeWriter(writer, snapMount, opts...)

    // 9. 写入变化
    for _, ch := range changes {
        changeWriter.HandleChange(ch.Kind, ch.Path, ch.FileInfo, nil)
    }
    changeWriter.Close()

    // 10. Commit
    commitToStore(...)
    return descriptor, nil
}
```

---

## 7. OCI layer 的关键规范要点

**新增/修改文件**  
OCI layer 不做二进制 patch，必须写入**完整文件内容**。

**删除文件（whiteout）**  
删除文件需要写入一个空文件 `.wh.<name>`：  
```
/etc/hosts 被删除
→ layer 中包含 /etc/.wh.hosts
```

**删除整个目录（opaque）**  
写入 `.wh..wh..opq` 标记目录为 opaque（禁止继承 lower）：
```
删除目录 /var/log/*
→ layer 中包含 /var/log/.wh..wh..opq
```

**路径规范**  
`ChangeWriter` 会自动将路径规范化成 POSIX 风格，并保证目录以 `/` 结尾。

**时间戳**  
使用 `SourceDateEpoch` 约束时间戳，确保可重复构建（和 walking/overlayfs 一致）。

---

## 8. Tar.gz 的生成方式（对齐 walking/overlayfs）

**压缩选择**：
```
MediaTypeImageLayer       → 无压缩
MediaTypeImageLayerGzip   → gzip
MediaTypeImageLayerZstd   → zstd
```

**实现要点**：
1. 对压缩流进行 digest 计算  
2. 记录未压缩 digest 到 label `containerd.io/uncompressed`  

**参考实现**：`diff/overlayfs/differ.go`  
它的核心逻辑与 LVM Comparer 应保持一致。

---

## 9. 推荐的实现拆分（模块化）

```
snapshots/devbox/lvm/
├── lvm_diff.go                // LVM Comparer 入口（Compare）
├── thin_send_recv.go          // thin_send wrapper（已有）
├── thin_send_parser.go        // 解析 thin_send stream -> block list
├── block_to_file.go           // block -> file 映射
├── delete_detector.go         // 删除检测（walking-only for deletes）
└── layer_writer.go            // 复用 ChangeWriter 写 tar
```

**职责清晰**：
- Comparer 负责调度整体流程  
- 解析与映射逻辑独立，方便替换实现  

---

## 10. 测试建议（关键点）

1. **基础 diff**
   - base LV 创建文件 A
   - writable LV 修改文件 A、新增文件 B
   - LVM comparer 输出 layer
   - 解 tar 检查 A/B 内容

2. **删除测试**
   - base LV 有文件 C
   - writable LV 删除 C
   - layer 中应该出现 `.wh.C`

3. **目录 opaque 测试**
   - base LV 有目录 /d/ (多个文件)
   - writable LV 删除整个目录
   - layer 中应出现 `/d/.wh..wh..opq`

4. **digest 与 label**
   - 验证 layer digest
   - 验证 label `containerd.io/uncompressed`

---

## 11. 下一步

当前差的两个关键模块是：
1. **解析 thin_send 输出** → 变化块列表  
2. **块 → 文件映射** → 文件变化列表  

完成这两步后，就可以直接复用 `archive.ChangeWriter` 生成 OCI layer。

