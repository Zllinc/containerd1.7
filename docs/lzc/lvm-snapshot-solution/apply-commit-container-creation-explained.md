# Apply、Commit、容器创建流程详解

**创建时间**: 2026-01-23  
**目的**: 回答关于 apply 的输出、Commit 的作用、容器创建流程的核心问题

---

## 目录

1. [问题 1: Apply 之后得到什么？](#一问题-1-apply-之后得到什么)
2. [问题 2: Commit 函数的作用](#二问题-2-commit-函数的作用)
3. [问题 3: 容器创建流程](#三问题-3-容器创建流程)
4. [如果将每层 Apply 到 LVM 会怎样？](#四如果将每层-apply-到-lvm-会怎样)
5. [总结：Snapshot 的层级与命名](#五总结snapshot-的层级与命名)

---

## 一、问题 1: Apply 之后得到什么？

### 1.1 简短回答

**Apply 之后得到的是当前层的 upperdir（可写层），而不是所有 lowdir 的合并视图。**

但是，**通过 OverlayFS 的挂载配置，可以访问到合并视图（merged view）。**

### 1.2 详细解释

#### 场景：三层镜像 Unpack

```
镜像层:
  Layer 1: base OS (Ubuntu)
  Layer 2: 安装 nginx
  Layer 3: 添加配置文件

Unpack 流程:
```

**Layer 1 (第一层)**:

```
Snapshotter.Prepare("extract-123", "")
  → 返回 mounts:
    [{
      Type: "bind",
      Source: "/snapshots/1/fs"
    }]

Apply(layer1.tar.gz, mounts)
  → 解压到 /snapshots/1/fs
  → 写入内容:
      /snapshots/1/fs/
        ├── bin/
        ├── etc/
        ├── usr/
        └── var/

Snapshotter.Commit(chainID-1, "extract-123")
  → 快照名: chainID-1
  → /snapshots/1/ 变为只读
```

**关键点**: Apply 直接写入 `/snapshots/1/fs`，这就是 **layer 1 的所有内容**。

---

**Layer 2 (增量层)**:

```
Snapshotter.Prepare("extract-456", chainID-1)
  → 返回 mounts:
    [{
      Type: "overlay",
      Options: [
        "lowerdir=/snapshots/1/fs",     ← layer 1 (只读)
        "upperdir=/snapshots/2/fs",     ← layer 2 (可写)
        "workdir=/snapshots/2/work"
      ]
    }]

Apply(layer2.tar.gz, mounts)
  → 提取 upperdir = /snapshots/2/fs
  → 只写入 /snapshots/2/fs (不写 lowerdir)
  → 写入内容:
      /snapshots/2/fs/
        ├── usr/
        │   └── sbin/nginx          ← 新增
        └── etc/
            └── nginx/              ← 新增
                └── nginx.conf

Snapshotter.Commit(chainID-2, "extract-456")
  → 快照名: chainID-2
  → /snapshots/2/ 变为只读
```

**关键点**: Apply **只写入 upperdir**（`/snapshots/2/fs`），这里只有 **layer 2 的增量内容**。

**但是**：如果挂载 overlay，就能看到合并视图：

```bash
# 挂载 overlay
mount -t overlay overlay \
  -o lowerdir=/snapshots/1/fs \
  -o upperdir=/snapshots/2/fs \
  -o workdir=/snapshots/2/work \
  /tmp/merged

# 在 /tmp/merged 中能看到:
/tmp/merged/
  ├── bin/              ← 来自 layer 1
  ├── etc/
  │   └── nginx/        ← 来自 layer 2
  ├── usr/
  │   └── sbin/nginx    ← 来自 layer 2
  └── var/              ← 来自 layer 1
```

---

**Layer 3 (最终层)**:

```
Snapshotter.Prepare("extract-789", chainID-2)
  → 返回 mounts:
    [{
      Type: "overlay",
      Options: [
        "lowerdir=/snapshots/1/fs:/snapshots/2/fs",  ← 多个 lowdir
        "upperdir=/snapshots/3/fs",
        "workdir=/snapshots/3/work"
      ]
    }]

Apply(layer3.tar.gz, mounts)
  → 只写入 /snapshots/3/fs
  → 写入内容:
      /snapshots/3/fs/
        └── etc/
            └── nginx/
                └── site.conf    ← 新增

Snapshotter.Commit(chainID-3, "extract-789")
  → 快照名: chainID-3 (最终快照)
```

**关键点**: `/snapshots/3/fs` 只有 **layer 3 的增量内容**。

---

### 1.3 回答你的问题

> **Apply 之后是得到 rootfs 吗？**

不完全是。Apply 之后得到的是：
- **第一层**：完整的基础 rootfs
- **后续层**：只有增量内容（upperdir）

> **这个 rootfs 就是所有 lowdir 的合并视图吗？**

不是。每个快照的 `fs/` 目录只包含该层的内容：
- `/snapshots/1/fs` = layer 1 的内容
- `/snapshots/2/fs` = layer 2 的增量
- `/snapshots/3/fs` = layer 3 的增量

**合并视图需要通过 OverlayFS 挂载才能看到**：

```
lowerdir=/snapshots/1/fs:/snapshots/2/fs + upperdir=/snapshots/3/fs
→ 合并后的视图 = 完整的 rootfs
```

---

## 二、问题 2: Commit 函数的作用

### 2.1 Commit 做了什么？

**文件**: `snapshots/storage/bolt.go:CommitActive()`

```go
func CommitActive(ctx, key, name string, usage, opts ...) (string, error)
```

**核心操作**：

```
1. 将 active 快照重命名为 committed 快照
2. 改变快照状态: KindActive → KindCommitted
3. 删除旧的 key（临时名称）
4. 创建新的 name（永久名称）
5. 更新父子关系
```

### 2.2 为什么要改名字？

#### 改名前：临时 key（用于 Prepare）

```
key = "extract-sha256:abc123-456789"
     或 "extract-123" (临时 ID)

特点：
- 随机生成的临时标识
- 用于 Apply 期间的可写访问
- 不代表快照的最终身份
```

#### 改名后：chainID（永久 name）

```
name = "sha256:111aaa..." (chainID)

chainID 计算:
  Layer 1: ChainID([diffID-1])
  Layer 2: ChainID([diffID-1, diffID-2])
  Layer 3: ChainID([diffID-1, diffID-2, diffID-3])

特点：
- 根据层内容确定性计算
- 全局唯一
- 代表快照的真实身份
- 用于容器创建时引用
```

### 2.3 Commit 的详细步骤

**输入**：
```
key = "extract-sha256:abc123-temp"  (Active snapshot)
name = "sha256:111aaa..."           (ChainID)
```

**步骤 1: 读取 active 快照**
```go
sbkt := bkt.Bucket([]byte(key))  // 从 bolt DB 读取
si.Kind = snapshots.KindActive
si.Parent = "sha256:parent-chainid"
```

**步骤 2: 创建 committed bucket**
```go
dbkt, err := bkt.CreateBucket([]byte(name))
// 在 bolt DB 中创建新的 bucket: "sha256:111aaa..."
```

**步骤 3: 标记为 committed**
```go
si.Kind = snapshots.KindCommitted
si.Created = time.Now().UTC()
si.Updated = si.Created
```

**步骤 4: 删除 active bucket**
```go
bkt.DeleteBucket([]byte(key))
// 删除临时 key: "extract-sha256:abc123-temp"
```

**步骤 5: 更新父快照的子引用**
```go
if si.Parent != "" {
    // 父快照的子列表: "extract-sha256:abc123-temp" → "sha256:111aaa..."
    pbkt.Put(parentKey(pid, id), []byte(name))
}
```

**输出**：
```
Committed snapshot: sha256:111aaa...
  - Kind: KindCommitted (只读)
  - Parent: sha256:parent-chainid
  - 可以通过 chainID 被容器引用
```

### 2.4 为什么不能直接用 chainID 作为 Prepare 的 key？

**问题**：为什么不能 `Prepare(chainID, parent)` 而要 `Prepare(tempKey, parent)` + `Commit(chainID, tempKey)`？

**原因 1: chainID 需要 diffID，而 diffID 来自 Apply**

```
Prepare 时:
  - 还没有 Apply
  - 还不知道 diffID (未压缩的 digest)
  - 无法计算 chainID

Apply 后:
  - 返回 diffID
  - 计算 chainID = ChainID(parent_chain + [diffID])
  - 然后 Commit
```

**原因 2: Prepare 可能失败**

```
如果直接用 chainID:
  Prepare(chainID, parent)
    → 如果 Apply 失败
    → chainID 快照已创建但内容错误
    → 无法清理（chainID 已被占用）

使用临时 key:
  Prepare(tempKey, parent)
    → 如果 Apply 失败
    → Remove(tempKey)  ← 清理简单
    → chainID 未被污染
```

### 2.5 Commit 的实际效果（文件系统）

**Commit 不改变文件系统内容**，只改变元数据：

```
Before Commit:
/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/
└── 123/
    ├── fs/         ← upperdir (内容不变)
    └── work/       ← workdir (会被删除)

Bolt DB:
  Bucket: "extract-sha256:abc123-temp"
    Kind: Active
    Parent: "sha256:parent"

After Commit(chainID, "extract-sha256:abc123-temp"):
/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/
└── 123/
    └── fs/         ← 只读 (内容不变)

Bolt DB:
  Bucket: "sha256:111aaa..." (chainID)
    Kind: Committed
    Parent: "sha256:parent"
  删除: "extract-sha256:abc123-temp"
```

---

## 三、问题 3: 容器创建流程

### 3.1 容器创建完整流程

```
用户命令: ctr run docker.io/library/nginx:latest my-nginx

整体流程:
┌─────────────────────────────────────────────┐
│ 1. 解析镜像引用                             │
│    nginx:latest → image record             │
└─────────────────────────────────────────────┘
                ↓
┌─────────────────────────────────────────────┐
│ 2. 获取 rootfs chain                        │
│    i.RootFS() → [diffID-1, diffID-2, ...]  │
│    ChainID(diffIDs) → chainID-final        │
└─────────────────────────────────────────────┘
                ↓
┌─────────────────────────────────────────────┐
│ 3. 创建容器的可写快照                       │
│    s.Prepare(containerID, chainID-final)   │
│    → 基于镜像的最终快照创建可写层           │
└─────────────────────────────────────────────┘
                ↓
┌─────────────────────────────────────────────┐
│ 4. 创建容器记录                             │
│    containers.Create(id, image, ...)       │
│    - SnapshotKey = containerID             │
│    - Image = nginx:latest                  │
└─────────────────────────────────────────────┘
                ↓
┌─────────────────────────────────────────────┐
│ 5. 创建任务（Task）                         │
│    container.NewTask(...)                  │
│    - s.Mounts(containerID) → mounts        │
│    - 传递给 runtime (runc)                 │
└─────────────────────────────────────────────┘
                ↓
┌─────────────────────────────────────────────┐
│ 6. Runtime 启动容器                         │
│    runc create + runc start                │
│    - 挂载 rootfs                           │
│    - 启动进程                              │
└─────────────────────────────────────────────┘
```

### 3.2 关键代码分析

#### 步骤 1-2: 获取镜像的 chainID

**文件**: `container_opts.go:WithNewSnapshot()`

```go
func WithNewSnapshot(id string, i Image, opts ...) NewContainerOpts {
    return func(ctx, client, c *containers.Container) error {
        // 获取镜像的 diffID 链
        diffIDs, err := i.RootFS(ctx)
        // diffIDs = [diffID-1, diffID-2, diffID-3]
        
        // 计算 chainID
        parent := identity.ChainID(diffIDs).String()
        // parent = "sha256:111aaa..." (最终层的 chainID)
        
        // 获取 snapshotter
        s, err := client.getSnapshotter(ctx, c.Snapshotter)
        
        // ★ 关键：基于镜像的最终 chainID 创建容器的可写快照
        if _, err := s.Prepare(ctx, id, parent, opts...); err != nil {
            return err
        }
        
        // 记录 snapshot key
        c.SnapshotKey = id
        c.Image = i.Name()
        return nil
    }
}
```

**关键点**：
```
parent = ChainID([diffID-1, diffID-2, diffID-3])
       = 镜像最终层的 chainID
       = unpack 时 Commit 的 name

s.Prepare(containerID, parent)
  → 基于 parent 创建可写快照
  → containerID 是容器的唯一标识
```

#### 步骤 3: Prepare 容器快照

**OverlayFS 实现**：

```go
func (o *snapshotter) Prepare(ctx, key, parent string, opts ...) ([]mount.Mount, error) {
    // key = "my-nginx" (容器 ID)
    // parent = "sha256:111aaa..." (镜像的 chainID-3)
    
    // 创建容器快照目录
    snapshotDir := "/snapshots/999/"  // 新的 ID
    os.MkdirAll(snapshotDir+"/fs", 0700)
    os.MkdirAll(snapshotDir+"/work", 0700)
    
    // 构造 lowerdir (镜像的所有层)
    lowerDirs := o.constructLowerDirs(parent)
    // lowerDirs = ["/snapshots/1/fs", "/snapshots/2/fs", "/snapshots/3/fs"]
    
    // 返回 mounts
    return []mount.Mount{{
        Type: "overlay",
        Options: []string{
            "lowerdir=/snapshots/1/fs:/snapshots/2/fs:/snapshots/3/fs",
            "upperdir=/snapshots/999/fs",   ← 容器的可写层
            "workdir=/snapshots/999/work",
        },
    }}, nil
}
```

**关键点**：
```
lowerdir = 镜像的所有层（只读）
  - /snapshots/1/fs (layer 1)
  - /snapshots/2/fs (layer 2)
  - /snapshots/3/fs (layer 3)

upperdir = 容器的可写层
  - /snapshots/999/fs (容器修改的内容)
```

#### 步骤 4-5: 创建任务并获取 mounts

**文件**: `container.go:NewTask()`

```go
func (c *container) NewTask(ctx, ioCreate, opts ...) (Task, error) {
    // 获取容器记录
    r, err := c.get(ctx)
    // r.SnapshotKey = "my-nginx"
    // r.Snapshotter = "overlayfs"
    
    if r.SnapshotKey != "" {
        // 获取 snapshotter
        s, err := c.client.getSnapshotter(ctx, r.Snapshotter)
        
        // ★ 获取容器快照的 mounts
        mounts, err := s.Mounts(ctx, r.SnapshotKey)
        // mounts = [{
        //   Type: "overlay",
        //   Options: ["lowerdir=...", "upperdir=/snapshots/999/fs", ...]
        // }]
        
        // 传递给 runtime
        for _, m := range mounts {
            request.Rootfs = append(request.Rootfs, &types.Mount{
                Type:    m.Type,
                Source:  m.Source,
                Options: m.Options,
            })
        }
    }
    
    // 调用 runtime 创建任务
    response, err := c.client.TaskService().Create(ctx, request)
    return task, nil
}
```

#### 步骤 6: Runtime 挂载并启动

**runc 接收到的 mounts**：

```json
{
  "type": "overlay",
  "source": "overlay",
  "options": [
    "lowerdir=/snapshots/1/fs:/snapshots/2/fs:/snapshots/3/fs",
    "upperdir=/snapshots/999/fs",
    "workdir=/snapshots/999/work"
  ]
}
```

**runc 执行**：

```bash
# 1. 创建 rootfs 挂载点
mkdir -p /run/containerd/io.containerd.runtime.v2.task/.../rootfs

# 2. 挂载 overlay
mount -t overlay overlay \
  -o lowerdir=/snapshots/1/fs:/snapshots/2/fs:/snapshots/3/fs \
  -o upperdir=/snapshots/999/fs \
  -o workdir=/snapshots/999/work \
  /run/containerd/.../rootfs

# 3. 在 rootfs 中启动容器进程
chroot /run/containerd/.../rootfs /bin/sh
```

### 3.3 关键点总结

```
镜像 Unpack:
  chainID-1 (layer 1) ← Committed
  chainID-2 (layer 2) ← Committed
  chainID-3 (layer 3) ← Committed (最终层)

容器创建:
  Prepare(containerID, chainID-3)
    → 基于 chainID-3 创建可写快照
    → lowerdir = chainID-3 的所有父层
    → upperdir = 容器的可写层

容器运行:
  s.Mounts(containerID) → overlay mounts
    → runtime 挂载
    → 容器看到合并的 rootfs
```

---

## 四、如果将每层 Apply 到 LVM 会怎样？

### 4.1 你的想法

```
原始方案（OverlayFS）:
  Layer 1 → /snapshots/1/fs
  Layer 2 → /snapshots/2/fs (增量)
  Layer 3 → /snapshots/3/fs (增量)
  容器挂载: overlay (lowerdir=1:2:3, upperdir=999)

你的 LVM 方案:
  Layer 1 → Apply 到 LV-1 (完整)
  Layer 2 → Apply 到 LV-2 (完整 = LV-1 + layer 2)
  Layer 3 → Apply 到 LV-3 (完整 = LV-1 + layer 2 + layer 3)
  容器: 直接使用 LV-3 的快照
```

### 4.2 这样做的影响

#### 影响 1: 没有 containerd 的 snapshot 概念

**问题**：

```
containerd 的容器创建依赖 snapshot:
  WithNewSnapshot(containerID, image)
    → i.RootFS() → chainID
    → s.Prepare(containerID, chainID)  ← 需要 chainID 存在
    
如果每层 Apply 到 LVM:
  - LV-1, LV-2, LV-3 存在
  - 但 containerd 不知道它们的 chainID
  - Prepare(containerID, chainID) 找不到 parent
  → 容器创建失败！
```

**解决方案**：必须在 Snapshotter 中注册快照

```go
// 在 Apply 到 LVM 后，注册到 containerd
func (s *lvmSnapshotter) Commit(ctx, name, key string, opts ...) error {
    // 1. 实际的 LVM 操作
    s.commitLV(key, name)
    
    // 2. 注册到 containerd 的 metadata store
    _, err := storage.CommitActive(ctx, key, name, usage, opts...)
    // 这样 containerd 才知道这个快照存在
    
    return err
}
```

#### 影响 2: 每层都是完整的 rootfs

**OverlayFS 方案**：
```
/snapshots/1/fs: 100MB (layer 1 完整)
/snapshots/2/fs: 10MB  (layer 2 增量)
/snapshots/3/fs: 5MB   (layer 3 增量)
总计: 115MB
```

**你的 LVM 方案**（如果每层都是完整的）：
```
LV-1: 100MB (layer 1)
LV-2: 110MB (layer 1 + layer 2 合并)
LV-3: 115MB (layer 1 + layer 2 + layer 3 合并)
总计: 325MB  ← 3 倍空间！
```

**问题**：空间浪费巨大

#### 影响 3: 容器启动不需要 overlay 挂载

**优点**：

```
OverlayFS 方案:
  s.Mounts(containerID) → overlay
    lowerdir=/snapshots/1/fs:/snapshots/2/fs:/snapshots/3/fs
  → 需要内核 overlay driver
  → 有性能开销

LVM 方案:
  s.Mounts(containerID) → bind
    source=/dev/vg/container-lv (thin snapshot of LV-3)
  → 直接使用 LV，无需 overlay
  → 更好的性能
```

### 4.3 正确的 LVM 方案

#### 方案 A: 模拟 OverlayFS 的增量模式

```
Layer 1:
  Prepare(temp-1, "") → 创建 LV-1
  Apply(layer1, LV-1) → 写入 LV-1
  Commit(chainID-1, temp-1)
  
Layer 2:
  Prepare(temp-2, chainID-1) → 创建 LV-2 (thin snapshot of LV-1)
  Apply(layer2, LV-2) → 增量写入 LV-2
  Commit(chainID-2, temp-2)
  
Layer 3:
  Prepare(temp-3, chainID-2) → 创建 LV-3 (thin snapshot of LV-2)
  Apply(layer3, LV-3) → 增量写入 LV-3
  Commit(chainID-3, temp-3)

容器:
  Prepare(containerID, chainID-3) → 创建 container-LV (thin snapshot of LV-3)
```

**优点**：
- 增量存储（类似 OverlayFS）
- 兼容 containerd 的 snapshot 模型
- Thin snapshot 很高效

**缺点**：
- 快照链很长（LV-3 → LV-2 → LV-1）
- 读性能可能下降

#### 方案 B: 只保留最终层（你的 base LV 方案）

```
Layer 1:
  Prepare → 临时 overlay upperdir
  Apply → 写入 overlay
  Commit(chainID-1) → 只在 metadata 注册

Layer 2:
  Prepare(parent=chainID-1) → 临时 overlay
  Apply → 写入 overlay
  Commit(chainID-2) → 只在 metadata 注册

Layer 3 (最终层):
  Prepare(parent=chainID-2) → 创建 base LV + writable LV
  Apply → 写入 base LV 和 writable LV
  Commit(chainID-3) → 注册，标记 base LV 路径

容器:
  Prepare(containerID, chainID-3)
    → 创建 container-LV (thin snapshot of base LV)
```

**优点**：
- 只有最终层用 LVM
- 中间层用 overlay（节省空间）
- 容器启动快（直接用 LV）
- 支持 base LV diff

**缺点**：
- 混合模式（复杂度）
- 中间层无法用 thin_send

---

## 五、总结：Snapshot 的层级与命名

### 5.1 名称体系

```
临时 key (Active):
  - "extract-sha256:abc123-temp-456"
  - "my-container-id"
  - 用途：Prepare 时的可写访问
  - 生命周期：Apply 完成后被 Commit 重命名或删除

chainID (Committed):
  - "sha256:111aaa..." (确定性计算)
  - ChainID([diffID-1, diffID-2, ...])
  - 用途：镜像层的永久标识
  - 生命周期：只要镜像存在就存在

containerID (Active):
  - "my-nginx" 或 UUID
  - 用途：容器的可写快照
  - 生命周期：容器删除时被 Remove
```

### 5.2 快照类型

```
KindActive:
  - 可写快照
  - 用于 Prepare 期间
  - 用于容器运行
  - 可以被修改

KindCommitted:
  - 只读快照
  - 用于镜像层
  - 不可修改
  - 可以作为 parent

KindView:
  - 只读快照（临时）
  - 用于只读容器
  - 不参与 chainID 计算
```

### 5.3 数据流总结

```
Registry → Content Store (压缩 blob)
  ↓
Unpack: For each layer
  ├─ Prepare(tempKey, parent) → Active snapshot
  ├─ Apply(blob, mounts) → 写入数据
  └─ Commit(chainID, tempKey) → Committed snapshot
  ↓
镜像层: chainID-1, chainID-2, chainID-3 (Committed)
  ↓
容器创建:
  ├─ WithNewSnapshot(containerID, image)
  ├─ i.RootFS() → chainID-final
  └─ Prepare(containerID, chainID-final) → Active snapshot
  ↓
容器运行:
  ├─ Mounts(containerID) → mounts
  └─ runtime 挂载 → rootfs
```

---

## 六、回答你的核心问题

### 问题 1: Apply 之后是得到 rootfs 吗？

**回答**：
- **第一层**：Apply 后得到完整的 base rootfs
- **后续层**：Apply 后只得到增量内容（upperdir）
- **合并视图**：需要通过 OverlayFS 挂载才能看到完整的 rootfs

### 问题 2: Commit 有啥用？

**回答**：
- 将临时的 Active 快照转为永久的 Committed 快照
- 重命名：临时 key → chainID
- 标记为只读
- 使快照可以被容器创建时引用

### 问题 3: 每层都 Apply 到 LVM 会怎样？

**回答**：
- **必须**在 containerd metadata 中注册快照（通过 `storage.CommitActive`）
- 否则容器创建时找不到 chainID parent
- 推荐使用 thin snapshot 链（方案 A）或混合模式（方案 B）
- 避免每层都存储完整 rootfs（空间浪费）

---

**文档版本**: 1.0.0  
**最后更新**: 2026-01-23  
**相关文档**:
- [镜像拉取 Unpack Apply 流程](./image-pull-unpack-apply-flow.md)
- [LVM Comparer OCI Layer 方案](./lvm-comparer-oci-layer-plan.md)

