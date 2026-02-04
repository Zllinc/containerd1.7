# Containerd Diff 机制详解

**创建时间**: 2026-01-22  
**目的**: 深入理解 containerd 的 diff/apply 机制，为 LVM snapshot commit 集成提供参考

---

## 一、Diff 机制概述

### 1.1 什么是 Diff？

在 containerd 中，**Diff** 是将容器的文件系统变化打包成 OCI 镜像层的核心机制。

```
容器运行时的文件系统状态
    ↓ Diff (比较变化)
差异数据（新增、修改、删除的文件）
    ↓ 打包
OCI 镜像层（layer.tar.gz）
    ↓ 推送
镜像 Registry
```

### 1.2 核心接口

Containerd 定义了两个核心接口：

```go
// diff/diff.go

// Comparer: 计算两个 mount 之间的差异
type Comparer interface {
    // lower: 父层（只读）
    // upper: 当前层（可写）
    // 返回: OCI 层描述符（包含 digest、size、media type）
    Compare(ctx context.Context, lower, upper []mount.Mount, opts ...Opt) (ocispec.Descriptor, error)
}

// Applier: 将镜像层应用到 mount
type Applier interface {
    // desc: 镜像层描述符
    // mount: 目标 mount
    // 返回: 应用后的描述符（包含 uncompressed digest）
    Apply(ctx context.Context, desc ocispec.Descriptor, mount []mount.Mount, opts ...ApplyOpt) (ocispec.Descriptor, error)
}
```

**关键概念**：
- **Comparer**: Commit 时使用，计算差异
- **Applier**: Pull/Unpack 时使用，解压层

---

## 二、Diff 的调用时机

### 2.1 完整流程图

```
┌─────────────────────────────────────────────────────────┐
│                  容器生命周期                            │
└─────────────────────────────────────────────────────────┘

1. Pull 镜像 (Apply)
   ┌─────────────┐
   │ Registry    │
   └──────┬──────┘
          │ Pull layer.tar.gz
          ↓
   ┌─────────────┐
   │ ContentStore│  存储压缩层
   └──────┬──────┘
          │ Applier.Apply()
          ↓
   ┌─────────────┐
   │ Snapshotter │  解压到 snapshot
   └─────────────┘

2. 运行容器 (Read/Write)
   ┌─────────────┐
   │ Container   │
   └──────┬──────┘
          │ 读取文件（from lower）
          │ 写入文件（to upper）
          ↓
   ┌─────────────┐
   │ OverlayFS   │  lower（只读）+ upper（可写）
   └─────────────┘

3. Commit 容器 (Compare)
   ┌─────────────┐
   │ Snapshotter │
   └──────┬──────┘
          │ GetMounts(lower, upper)
          ↓
   ┌─────────────┐
   │ Comparer    │  diff.Compare(lower, upper)
   └──────┬──────┘
          │ 计算差异 → layer.tar
          ↓
   ┌─────────────┐
   │ ContentStore│  存储新层 digest
   └──────┬──────┘
          │ Push
          ↓
   ┌─────────────┐
   │ Registry    │
   └─────────────┘
```

### 2.2 Commit 时的具体调用链

```
用户触发 commit (例如 ctr snapshot diff)
    ↓
rootfs.CreateDiff()  ← 入口函数
    ↓
├─ sn.Stat(snapshotID)  ← 获取 snapshot 信息
│   - info.Parent: 父层 ID
│   - info.Kind: KindActive 或 KindCommitted
│
├─ sn.View(parentKey, info.Parent)  ← 创建父层的只读视图
│   - 返回 lower mounts
│
├─ sn.Mounts(snapshotID)  ← 获取当前层的 mounts
│   - 如果 KindActive: 直接返回可写层 mounts
│   - 如果 KindCommitted: 创建 View
│   - 返回 upper mounts
│
│   ★ 这些 mounts 的作用 ★
│   1. Walking Differ: 需要挂载到临时目录才能访问文件系统
│   2. OverlayFS Differ: 从 mount options 提取 upperdir 路径
│   3. 本质：mounts 描述了如何访问这个层的文件系统
│
└─ d.Compare(ctx, lower, upper, opts...)  ← 调用 Comparer
    ↓
    选择 Differ 实现:
    ├─ overlayfs.overlayfsDiff.Compare()  ← OverlayFS 优化版
    ├─ walking.walkingDiff.Compare()      ← 通用版
    └─ windows.windowsDiff.Compare()      ← Windows 版
```

---

## 三、Mounts 的作用详解

### 3.1 什么是 Mount？

`Mount` 是 containerd 中描述文件系统挂载的抽象结构：

```go
// mount/mount.go
type Mount struct {
    Type    string   // 文件系统类型："overlay", "bind", "lvm" 等
    Source  string   // 挂载源路径或设备
    Options []string // 挂载选项
}
```

**示例 1: OverlayFS Mount**
```go
mount.Mount{
    Type: "overlay",
    Source: "overlay",
    Options: []string{
        "lowerdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/1/fs:/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/2/fs",
        "upperdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/3/fs",
        "workdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/3/work",
    },
}
```

**示例 2: LVM Bind Mount**
```go
mount.Mount{
    Type: "bind",
    Source: "/var/lib/containerd/devbox/mnt/writable-lv",
    Options: []string{"rbind", "rw"},
}
```

### 3.2 为什么需要 Mounts？

Diff 过程需要访问文件系统的内容，但在 containerd 的抽象层次上：

```
Snapshotter 层（抽象）
    ↓ 返回 mounts
    ↓ (不关心具体文件在哪)
Differ 层
    ↓ 使用 mounts
    ↓ (根据 mounts 访问文件系统)
实际文件系统
```

**问题**：Snapshotter 怎么告诉 Differ "这个层的文件在哪里"？  
**答案**：通过返回 `[]mount.Mount`，描述如何挂载/访问这个层。

### 3.3 不同 Differ 如何使用 Mounts

#### 方式 1: Walking Differ - 真实挂载

**步骤**：

```go
// 1. 获取 lower 和 upper 的 mounts
lower := sn.View(parentKey, info.Parent)   // 父层 mounts
upper := sn.Mounts(snapshotID)             // 当前层 mounts

// 2. 挂载到临时目录
mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
    // lowerRoot 例如: /tmp/containerd-mount-123456
    // 现在可以访问 lowerRoot 下的所有文件
    
    return mount.WithTempMount(ctx, upper, func(upperRoot string) error {
        // upperRoot 例如: /tmp/containerd-mount-789012
        // 现在可以访问 upperRoot 下的所有文件
        
        // 3. 遍历两个目录，比较文件
        archive.WriteDiff(ctx, writer, lowerRoot, upperRoot)
        //                               ↑         ↑
        //                               真实的文件系统路径
        //                               可以用 os.Open, filepath.Walk 等操作
    })
})
```

**实际挂载过程**：

```bash
# 假设 upper mount 是：
Type: "overlay"
Options: ["lowerdir=/a:/b", "upperdir=/upper", "workdir=/work"]

# WithTempMount 会执行：
mkdir -p /tmp/containerd-mount-789012
mount -t overlay overlay -o lowerdir=/a:/b,upperdir=/upper,workdir=/work /tmp/containerd-mount-789012

# 现在可以访问：
/tmp/containerd-mount-789012/etc/hosts
/tmp/containerd-mount-789012/usr/bin/bash
...

# diff 完成后自动 umount：
umount /tmp/containerd-mount-789012
rm -rf /tmp/containerd-mount-789012
```

**为什么需要挂载？为什么不能直接访问 snapshot 目录？**

**常见误解**：
```
误解：snapshot 目录（如 /var/lib/containerd/.../snapshots/3/fs）不就是文件系统吗？
      Differ 直接访问这个目录不就行了？

实际情况：这个目录只是"增量数据"，不是完整的文件系统视图！
```

**关键原因 1: OverlayFS 的特殊性（最重要）**

```
OverlayFS snapshot 目录结构：

/var/lib/containerd/.../snapshots/
├── 1/fs/          ← 基础层（例如 Ubuntu 基础文件系统）
│   ├── bin/
│   ├── etc/
│   └── usr/
├── 2/fs/          ← 第二层（例如安装了 nginx）
│   ├── etc/nginx/
│   └── usr/bin/nginx
└── 3/fs/          ← 第三层（容器的可写层，只有修改）
    ├── etc/
    │   └── hosts      ← 修改的文件
    ├── tmp/
    │   └── test.txt   ← 新增的文件
    └── .wh.oldfile    ← whiteout（删除标记）

问题：如果 Differ 直接访问 /snapshots/3/fs/
❌ 只能看到：etc/hosts, tmp/test.txt, .wh.oldfile
❌ 看不到：bin/, usr/, etc/nginx/ 等来自 lower 层的文件
❌ 无法判断哪些文件是新增的，哪些是修改的

解决方案：挂载 overlay 得到合并视图
✅ mount -t overlay overlay \
     -o lowerdir=/snapshots/1/fs:/snapshots/2/fs,\
        upperdir=/snapshots/3/fs,\
        workdir=/snapshots/3/work \
     /tmp/merged

✅ 现在 /tmp/merged/ 包含完整的文件系统视图：
   - bin/            ← 来自 layer 1
   - etc/hosts       ← 来自 layer 3 (修改)
   - etc/nginx/      ← 来自 layer 2
   - tmp/test.txt    ← 来自 layer 3 (新增)
   - usr/bin/nginx   ← 来自 layer 2
   - (oldfile 不可见，被 whiteout 删除)

✅ Differ 可以对比两个合并后的视图，得到准确的 diff
```

**关键原因 2: Mount 可能未实际挂载**

```
Snapshotter 返回的是"mount 描述"，不是"已挂载的路径"：

例如 LVM Snapshotter:
mount.Mount{
    Type: "ext4",           ← 文件系统类型
    Source: "/dev/vg/lv",   ← 块设备路径（未挂载）
    Options: []string{},
}

这个 mount 描述：
❌ /dev/vg/lv 是块设备，不能直接访问文件
❌ 必须先 mount /dev/vg/lv /tmp/xxx 才能访问

如果 Differ 直接访问 /dev/vg/lv：
❌ 只会读取到二进制块数据，不是文件系统
```

**关键原因 3: 隔离性和安全性**

```
直接访问 snapshot 目录的风险：

1. 污染风险
   - Differ 可能会创建临时文件（如 .tmp）
   - 直接在 snapshot 目录操作可能污染快照数据

2. 权限问题
   - snapshot 目录可能有特殊权限要求
   - 临时挂载可以控制只读/读写权限

3. 清理问题
   - 挂载到临时目录，结束后自动 umount 和删除
   - 不留痕迹

4. 并发安全
   - 多个 diff 操作可以挂载到不同的临时目录
   - 互不干扰
```

**关键原因 4: 统一接口**

```
不同 Snapshotter 的实现差异：

OverlayFS Snapshotter:
- 返回 overlay mount（需要挂载才能看到合并视图）

LVM Snapshotter:
- 返回 bind mount 或 ext4 mount（可能未挂载）

ZFS Snapshotter:
- 返回 zfs mount（可能未挂载）

BTRFS Snapshotter:
- 返回 btrfs subvolume mount

Walking Differ 的统一处理：
✅ 不管什么类型的 mount，都挂载到临时目录
✅ 统一的访问接口：filepath.Walk(tempDir, ...)
✅ 不需要为每种 Snapshotter 写特殊逻辑
```

#### 方式 2: OverlayFS Differ - 解析路径（特殊优化）

**步骤**：

```go
// 1. 获取 upper mounts
upper := sn.Mounts(snapshotID)
// 返回：
// mount.Mount{
//     Type: "overlay",
//     Options: [
//         "lowerdir=/snapshots/1/fs:/snapshots/2/fs",
//         "upperdir=/snapshots/3/fs",
//         "workdir=/snapshots/3/work",
//     ],
// }

// 2. 从 mount options 提取 upperdir（不需要真实挂载）
func overlayMountsToLayer(mounts []mount.Mount) (string, error) {
    for _, mnt := range mounts {
        if mnt.Type != "overlay" {
            continue
        }
        
        for _, opt := range mnt.Options {
            if strings.HasPrefix(opt, "upperdir=") {
                upperdir := strings.TrimPrefix(opt, "upperdir=")
                // upperdir = "/var/lib/containerd/.../snapshots/3/fs"
                return upperdir, nil
            }
        }
    }
}

// 3. 直接访问 upperdir（OverlayFS 特性）
upperdir := "/var/lib/containerd/.../snapshots/3/fs"

// ★ 关键：直接遍历 upperdir，不需要挂载 overlay ★
filepath.Walk(upperdir, func(path string, info os.FileInfo, err error) {
    // upperdir 已经是真实的目录，可以直接访问
    // 包含了所有修改（新增、修改、删除标记）
    
    if strings.HasPrefix(info.Name(), ".wh.") {
        // whiteout 文件，表示删除
        addWhiteoutToTar(...)
    } else {
        // 普通文件，表示新增或修改
        addFileToTar(...)
    }
})
```

**为什么 OverlayFS Differ 可以不挂载？**

```
关键理解：upperdir 是真实存在的目录，不需要挂载！

OverlayFS 的存储结构：

物理存储（真实目录）：
/var/lib/containerd/.../snapshots/
├── 1/fs/              ← 真实目录，可以直接访问
│   ├── bin/
│   └── etc/
├── 2/fs/              ← 真实目录，可以直接访问
│   └── etc/nginx/
└── 3/fs/              ← 真实目录，可以直接访问（upperdir）
    ├── etc/
    │   └── hosts          ← 修改的文件（完整内容）
    ├── tmp/
    │   └── test.txt       ← 新增的文件
    └── .wh.oldfile        ← whiteout（删除标记）

OverlayFS 挂载后的视图（虚拟的合并视图）：
/var/lib/containerd/.../snapshots/3/merged/
├── bin/               ← 透明地读取自 1/fs/bin/
├── etc/
│   ├── hosts          ← 读取自 3/fs/etc/hosts (修改)
│   └── nginx/         ← 读取自 2/fs/etc/nginx/
├── tmp/
│   └── test.txt       ← 读取自 3/fs/tmp/test.txt (新增)
└── usr/               ← 读取自 1/fs/usr/
    (oldfile 不存在，被 3/fs/.wh.oldfile 删除)

对比 Walking Differ 和 OverlayFS Differ：

Walking Differ 需要合并视图：
  1. 挂载 lower 到 /tmp/lower/
  2. 挂载 upper 到 /tmp/upper/ (overlay mount，包含 lowerdir)
  3. 遍历 /tmp/upper/，看到完整的文件系统
  4. 遍历 /tmp/lower/，看到父层的文件系统
  5. 对比两个完整视图，生成 diff

OverlayFS Differ 直接读取增量：
  1. 解析 mount options，得到 upperdir 路径
  2. 直接访问 upperdir 目录（已经存在，不需要挂载）
  3. upperdir 只包含增量（新增、修改、删除标记）
  4. 直接将增量打包成 tar，无需对比

优势：
✅ 不需要挂载（节省系统资源）
✅ 不需要遍历 lower 层（更快）
✅ 直接读取增量数据（更高效）

前提：
⚠️ 只适用于 OverlayFS snapshotter
⚠️ upperdir 必须是真实的目录（OverlayFS 的实现保证）
```

**OverlayFS 的存储原理**

```
关键理解：OverlayFS 的 upperdir 是真实的文件系统目录

创建 OverlayFS snapshot 时发生了什么：

$ ctr snapshot prepare mycontainer image-layer

Snapshotter 做的事情：
1. mkdir -p /snapshots/3/fs        ← 创建 upperdir（真实目录）
2. mkdir -p /snapshots/3/work      ← 创建 workdir（OverlayFS 需要）
3. 记录父层: parent = "image-layer"

容器运行时修改文件：

$ 容器内: echo "hello" > /etc/hosts

OverlayFS 的 COW 机制：
1. 检测到写入 /etc/hosts
2. 从 lowerdir 复制原始 /etc/hosts 到 upperdir
3. 在 upperdir 修改文件
4. 结果：/snapshots/3/fs/etc/hosts 是真实存在的文件

$ ls -la /snapshots/3/fs/etc/hosts
-rw-r--r-- 1 root root 6 Jan 23 10:00 /snapshots/3/fs/etc/hosts

容器内删除文件：

$ 容器内: rm /usr/share/doc/README

OverlayFS 的删除机制：
1. 不能真的删除 lowerdir 的文件（只读）
2. 在 upperdir 创建 whiteout 文件
3. 结果：/snapshots/3/fs/usr/share/doc/.wh.README

$ ls -la /snapshots/3/fs/usr/share/doc/
drwxr-xr-x 2 root root 4096 Jan 23 10:00 .
-r--r--r-- 1 root root    0 Jan 23 10:01 .wh.README

结论：
upperdir 是真实的目录，包含了容器的所有修改！
Differ 可以直接访问这个目录，不需要挂载！
```

### 3.4 总结：什么时候需要挂载？什么时候不需要？

```
┌─────────────────────────────────────────────────────────────────┐
│ 场景分析：是否需要挂载到临时目录                                │
└─────────────────────────────────────────────────────────────────┘

场景 1: OverlayFS snapshot 的 upperdir（已经是真实目录）
├─ Mount 描述:
│  Type: "overlay"
│  Options: ["upperdir=/snapshots/3/fs", "lowerdir=...", "workdir=..."]
├─ OverlayFS Differ 的做法:
│  ✅ 直接访问 /snapshots/3/fs（已经存在的目录）
│  ✅ 不需要挂载（优化）
└─ Walking Differ 的做法:
   ⚠️ 挂载 overlay 到临时目录（统一处理）
   ⚠️ 看到的是合并视图（包含 lower 层）

场景 2: LVM 块设备（未挂载）
├─ Mount 描述:
│  Type: "ext4"
│  Source: "/dev/vg/lv"
├─ 问题:
│  ❌ /dev/vg/lv 是块设备，不能直接访问文件
│  ❌ 必须挂载才能访问文件系统
└─ 必须挂载:
   ✅ mount /dev/vg/lv /tmp/xxx
   ✅ 然后访问 /tmp/xxx

场景 3: LVM 已挂载的目录（bind mount）
├─ Mount 描述:
│  Type: "bind"
│  Source: "/var/lib/containerd/devbox/mnt/lv"
│  Options: ["rbind", "rw"]
├─ 理论上:
│  ⚠️ 可以直接访问 /var/lib/.../mnt/lv（已经挂载）
│  ⚠️ 但实际上仍然会 bind mount 到临时目录
└─ 为什么还要挂载?
   1. 统一接口（Walking Differ 统一处理）
   2. 隔离性（不污染原挂载点）
   3. 权限控制（可以指定只读）
   4. 清理方便（自动 umount）

场景 4: 需要合并视图的情况
├─ 例子: Walking Differ 对比 lower 和 upper
├─ Mount 描述:
│  lower: overlay mount (lowerdir=/1/fs:/2/fs, upperdir=/3/fs)
│  upper: overlay mount (lowerdir=/1/fs:/2/fs:/3/fs, upperdir=/4/fs)
├─ 问题:
│  ❌ 如果直接访问 /3/fs 和 /4/fs，只能看到增量
│  ❌ 无法判断哪些文件来自 lower 层
└─ 必须挂载:
   ✅ 挂载两个 overlay，得到完整的合并视图
   ✅ 对比两个完整视图，准确计算 diff
```

**决策树：Differ 该怎么处理 mount？**

```
收到 []mount.Mount
    ↓
    判断：我是什么 Differ？
    ├─ OverlayFS Differ
    │  ├─ 检查 mount.Type == "overlay"？
    │  │  ├─ Yes → 提取 upperdir，直接访问（优化）
    │  │  └─ No  → 回退到 Walking Differ
    │  └─ 只需要增量数据（upperdir）
    │
    ├─ Walking Differ
    │  ├─ 不管什么类型，统一挂载到临时目录
    │  ├─ 挂载 lower 到 /tmp/lower-xxx
    │  ├─ 挂载 upper 到 /tmp/upper-xxx
    │  ├─ 遍历两个目录，对比生成 diff
    │  └─ 清理：umount + 删除临时目录
    │
    └─ LVM Differ (你未来要实现的)
       ├─ 从 mount.Source 提取 LV 路径或挂载点
       ├─ 找到对应的块设备 /dev/vg/xxx
       ├─ 不需要访问文件系统，直接块级 diff
       └─ 使用 thin_send 生成增量数据
```

**为什么 OverlayFS Differ 是特例？**

```
OverlayFS 的独特性：

1. upperdir 是真实的、可以直接访问的目录
   /var/lib/containerd/.../snapshots/3/fs  ← 真实目录
   
2. upperdir 只包含增量（新增、修改、删除标记）
   - 新增文件：直接存储在 upperdir
   - 修改文件：COW 复制到 upperdir
   - 删除文件：创建 .wh.* whiteout 文件
   
3. 这正是 OCI 镜像层的格式！
   - 镜像层 = tar(新增文件 + 修改文件 + whiteout)
   - upperdir = 新增文件 + 修改文件 + whiteout
   - 完美匹配！

4. 因此可以直接打包 upperdir，无需对比
   tar -C /snapshots/3/fs -czf layer.tar.gz .
   
其他文件系统不具备这个特性：
- LVM: 块设备，必须挂载才能访问
- ZFS: subvolume，包含完整数据，不是增量
- BTRFS: subvolume，包含完整数据，不是增量
- 它们都需要挂载后对比两个完整视图
```

### 3.5 Mounts 在 LVM Snapshotter 中的意义

对于你的 LVM-based snapshotter：

```go
// Devbox Snapshotter 的 Mounts 返回

func (s *DevboxSnapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
    // 当前实现：返回 LV 的 bind mount
    return []mount.Mount{{
        Type:   "bind",
        Source: "/var/lib/containerd/devbox/mnt/writable-lv-123",
        Options: []string{"rbind", "rw"},
    }}, nil
}

// Differ 会怎么用？

// Walking Differ:
mount.WithTempMount(ctx, mounts, func(root string) error {
    // 执行 bind mount:
    // mount --bind /var/lib/containerd/devbox/mnt/writable-lv-123 /tmp/xxx
    
    // 然后遍历 /tmp/xxx
    filepath.Walk(root, ...)
})

// 未来的 LVM Differ:
func (d *lvmDiffer) Compare(ctx, lower, upper []mount.Mount) {
    // 从 mount.Source 提取 LV 路径
    baseLVPath := extractLVPath(lower)     // /dev/vg/base-lv
    writableLVPath := extractLVPath(upper) // /dev/vg/writable-lv
    
    // 创建快照
    snapshot := createSnapshot(writableLVPath)
    
    // 使用 thin_send
    thin_send(baseLVPath, snapshot)
    
    // 解析 + 打包...
}
```

### 3.5 总结：Mounts 的本质

```
Mounts 是一个"文件系统访问协议"：

1. 抽象性
   - Snapshotter 不暴露内部实现细节
   - 只告诉 Differ "如何"访问文件系统
   - 不是"在哪里"（具体路径可能是实现细节）

2. 灵活性
   - 不同的 Snapshotter 可以返回不同类型的 mounts
     * OverlayFS: overlay mount
     * Devbox: bind mount
     * ZFS: zfs mount
   - Differ 根据 mount 类型选择最优策略

3. 延迟绑定
   - Snapshotter 返回 mounts 时，文件系统可能还没有挂载
   - Differ 决定何时、如何使用这些 mounts
   - 例如：OverlayFS Differ 不实际挂载，Walking Differ 挂载

4. 统一接口
   - 所有 Snapshotter 返回统一的 []mount.Mount
   - 所有 Differ 接受统一的 []mount.Mount
   - 解耦了 Snapshotter 和 Differ 的具体实现
```

**类比**：
```
Mounts 就像餐厅的菜单：

- 菜单（Mount）告诉你"如何点菜"
  * Type: "中餐"
  * Source: "川菜馆"
  * Options: ["辣", "不要香菜"]

- 不同的厨师（Differ）根据菜单决定怎么做：
  * 标准厨师（Walking）: 严格按菜单做菜
  * 快手厨师（OverlayFS）: 发现菜单是外卖单，直接去拿外卖

- 核心：菜单是抽象层，解耦了点菜（Snapshotter）和做菜（Differ）
```

---

## 四、两种 Differ 实现详解

### 3.1 Walking Differ (通用实现)

**文件**: `diff/walking/differ.go`

#### 核心思路

```
通用的文件系统 diff，适用于任何文件系统类型。
通过挂载 lower 和 upper，遍历目录树进行对比。
```

#### 实现步骤

```go
func (s *walkingDiff) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (ocispec.Descriptor, error) {
    // Step 1: 解析配置
    var config diff.Config
    for _, opt := range opts {
        opt(&config)
    }
    
    // Step 2: 确定压缩格式
    // - ocispec.MediaTypeImageLayerGzip (默认)
    // - ocispec.MediaTypeImageLayer (无压缩)
    // - ocispec.MediaTypeImageLayerZstd
    
    // Step 3: 挂载 lower 和 upper
    mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
        return mount.WithReadonlyTempMount(ctx, upper, func(upperRoot string) error {
            
            // Step 4: 打开 content writer
            cw, err := s.store.Writer(ctx, 
                content.WithRef(config.Reference),
                content.WithDescriptor(ocispec.Descriptor{
                    MediaType: config.MediaType,
                }))
            
            // Step 5: 创建压缩流（如果需要）
            if isCompressed {
                compressed := compression.CompressStream(cw, compression.Gzip)
                // 同时写入压缩流和 digest 计算器
                writer = io.MultiWriter(compressed, dgstr.Hash())
            }
            
            // Step 6: 遍历目录，写入 diff
            // ★ 核心函数 ★
            archive.WriteDiff(ctx, writer, lowerRoot, upperRoot, writeDiffOpts...)
            
            // Step 7: 提交到 content store
            cw.Commit(ctx, 0, dgst, commitopts...)
            
            // Step 8: 返回 OCI descriptor
            return ocispec.Descriptor{
                MediaType: config.MediaType,
                Size:      info.Size,
                Digest:    info.Digest,
            }
        })
    })
}
```

#### archive.WriteDiff 做了什么？

```go
// archive/diff.go (简化)

func WriteDiff(ctx, w io.Writer, lowerRoot, upperRoot string, opts...) error {
    // 遍历 upperRoot 的所有文件
    filepath.Walk(upperRoot, func(path string, info os.FileInfo, err error) error {
        relPath := strings.TrimPrefix(path, upperRoot)
        lowerPath := filepath.Join(lowerRoot, relPath)
        
        // 1. 检查文件是否在 lower 存在
        lowerInfo, err := os.Stat(lowerPath)
        
        if os.IsNotExist(err) {
            // 新增文件
            addFileToTar(w, path, info)
        } else if fileChanged(info, lowerInfo) {
            // 修改的文件
            addFileToTar(w, path, info)
        }
        // 相同文件不处理
        
        return nil
    })
    
    // 2. 检查删除的文件（在 lower 但不在 upper）
    filepath.Walk(lowerRoot, func(path string, info os.FileInfo, err error) error {
        relPath := strings.TrimPrefix(path, lowerRoot)
        upperPath := filepath.Join(upperRoot, relPath)
        
        if _, err := os.Stat(upperPath); os.IsNotExist(err) {
            // 删除的文件：写入 whiteout
            addWhiteoutToTar(w, relPath)
        }
        return nil
    })
}
```

**Whiteout 文件**：
```bash
# 如果删除了 /etc/hosts
# tar 包中会包含：
.wh.hosts  # whiteout 标记文件

# 如果删除了整个目录 /var/log
.wh..wh..opq  # opaque whiteout
```

---

### 3.2 OverlayFS Differ (优化实现)

**文件**: `diff/overlayfs/differ.go`

#### 核心思路

```
针对 OverlayFS 特性优化的 diff 实现。
直接读取 OverlayFS 的 upperdir，无需挂载 lower。
```

#### 关键优化点

**1. 提取 upperdir**

```go
func overlayMountsToLayer(mounts []mount.Mount) (string, error) {
    // 解析 mount options
    // overlay mount 示例:
    // Type: "overlay"
    // Options: ["lowerdir=/a:/b:/c", "upperdir=/upper", "workdir=/work"]
    
    for _, o := range mnt.Options {
        if k, v, ok := strings.Cut(o, "="); ok {
            switch k {
            case "upperdir":
                layer = filepath.Dir(v)  // 提取 upper 层路径
            case "lowerdir":
                dir, _, _ := strings.Cut(v, ":")
                topLower = filepath.Dir(dir)  // 提取最顶层 lower
            }
        }
    }
    
    // 如果有 upperdir，使用 upperdir
    // 否则使用 topLower（只读层的 diff）
    return layer, nil
}
```

**2. 使用 OverlayFS 的 COW 特性**

```go
func (s *overlayfsDiff) Compare(ctx, lower, upper []mount.Mount, opts...) (ocispec.Descriptor, error) {
    // 提取 upper 层路径
    layer, err := overlayMountsToLayer(upper)
    upperRoot := filepath.Join(layer, "fs")  // upper 层的文件系统根目录
    
    // ★ 关键：只遍历 upperdir，无需挂载 lower ★
    // OverlayFS 的 upperdir 已经包含了所有变化：
    // - 新增的文件：直接存在
    // - 修改的文件：COW 后存在
    // - 删除的文件：whiteout 文件存在
    
    writeDiff(ctx, w, lower, upperRoot, config.SourceDateEpoch)
}

func writeDiff(ctx, w io.Writer, lower []mount.Mount, upperRoot string, ...) error {
    // 挂载 lower（用于对比）
    mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
        // 使用 OverlayFS 特定的 diff 函数
        cw := archive.NewChangeWriter(w, upperRoot, opts...)
        
        // ★ fs.DiffDirChanges 是关键 ★
        // 它知道如何处理 OverlayFS 的 whiteout 文件
        fs.DiffDirChanges(ctx, lowerRoot, upperRoot, fs.DiffSourceOverlayFS, cw.HandleChange)
        
        return cw.Close()
    })
}
```

**3. OverlayFS vs Walking 的区别**

| 特性 | Walking Differ | OverlayFS Differ |
|-----|---------------|-----------------|
| **适用范围** | 任何文件系统 | 仅 OverlayFS |
| **lower 处理** | 需要挂载并遍历 | 仅挂载用于对比 |
| **upper 处理** | 需要挂载并遍历 | 直接读取 upperdir |
| **whiteout** | 手动检测删除 | 直接读取 .wh.* 文件 |
| **性能** | 较慢（两次遍历） | 较快（一次遍历 + 对比） |

---

## 四、Diff 输出格式

### 4.1 OCI Layer Tar 格式

```
layer.tar (或 layer.tar.gz)
├─ etc/
│  └─ hosts           # 新增或修改的文件
├─ var/
│  └─ log/
│     └─ app.log      # 新增或修改的文件
└─ usr/
   └─ .wh.old-file    # whiteout: 删除了 /usr/old-file
```

### 4.2 完整示例

假设容器运行时的变化：
```bash
# 新增文件
echo "hello" > /app/new.txt

# 修改文件
echo "modified" >> /etc/config

# 删除文件
rm /var/log/old.log
```

生成的 tar 包内容：
```
app/
  new.txt              # 内容: "hello"
etc/
  config               # 完整文件内容（不是 diff）
var/
  log/
    .wh.old.log        # whiteout 标记
```

### 4.3 Descriptor 示例

```go
ocispec.Descriptor{
    MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
    Digest:    "sha256:abc123...",
    Size:      1024567,  // 压缩后的大小
}

// ContentStore 中的 labels:
labels: {
    "containerd.io/uncompressed": "sha256:def456...",  // 未压缩的 digest
}
```

---

## 五、Walking Differ 代码详解

### 5.1 核心结构

```go
// diff/walking/differ.go

type walkingDiff struct {
    store content.Store  // 用于存储生成的 layer
}

func NewWalkingDiff(store content.Store) diff.Comparer {
    return &walkingDiff{store: store}
}
```

### 5.2 Compare 方法分步解析

```go
func (s *walkingDiff) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (d ocispec.Descriptor, err error) {
    // ===== Step 1: 配置解析 =====
    var config diff.Config
    for _, opt := range opts {
        if err := opt(&config); err != nil {
            return emptyDesc, err
        }
    }
    
    // SOURCE_DATE_EPOCH: 用于可重现构建
    if tm := epoch.FromContext(ctx); tm != nil && config.SourceDateEpoch == nil {
        config.SourceDateEpoch = tm
    }
    
    // ===== Step 2: 确定压缩格式 =====
    var isCompressed bool
    if config.MediaType == "" {
        config.MediaType = ocispec.MediaTypeImageLayerGzip  // 默认 gzip
    }
    
    switch config.MediaType {
    case ocispec.MediaTypeImageLayer:
        // 不压缩
        isCompressed = false
    case ocispec.MediaTypeImageLayerGzip:
        // gzip 压缩
        isCompressed = true
    default:
        return emptyDesc, fmt.Errorf("unsupported diff media type: %v", config.MediaType)
    }
    
    // ===== Step 3: 挂载并计算 diff =====
    var ocidesc ocispec.Descriptor
    
    // 挂载 lower (只读层)
    err := mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
        // 挂载 upper (可写层)
        return mount.WithReadonlyTempMount(ctx, upper, func(upperRoot string) error {
            
            // ===== Step 4: 生成唯一引用 =====
            var newReference bool
            if config.Reference == "" {
                newReference = true
                config.Reference = uniqueRef()  // 例如: "1705900800000000000-aB3cD"
            }
            
            // ===== Step 5: 打开 content writer =====
            cw, err := s.store.Writer(ctx,
                content.WithRef(config.Reference),
                content.WithDescriptor(ocispec.Descriptor{
                    MediaType: config.MediaType,
                }))
            if err != nil {
                return fmt.Errorf("failed to open writer: %w", err)
            }
            
            // ===== Step 6: 错误处理（cleanup） =====
            var errOpen error
            defer func() {
                if errOpen != nil {
                    cw.Close()
                    if newReference {
                        s.store.Abort(ctx, config.Reference)  // 清理未完成的上传
                    }
                }
            }()
            
            // ===== Step 7: 写入 diff =====
            if isCompressed {
                // 压缩场景
                dgstr := digest.SHA256.Digester()
                compressed, errOpen := compression.CompressStream(cw, compression.Gzip)
                if errOpen != nil {
                    return fmt.Errorf("failed to get compressed stream: %w", errOpen)
                }
                
                // ★ 核心：写入差异 ★
                // 同时写入：
                // 1. compressed → cw (content store)
                // 2. dgstr.Hash() → 计算未压缩的 digest
                errOpen = archive.WriteDiff(ctx, 
                    io.MultiWriter(compressed, dgstr.Hash()), 
                    lowerRoot, upperRoot, 
                    writeDiffOpts...)
                compressed.Close()
                
                if errOpen != nil {
                    return fmt.Errorf("failed to write compressed diff: %w", errOpen)
                }
                
                // 保存未压缩的 digest
                if config.Labels == nil {
                    config.Labels = map[string]string{}
                }
                config.Labels[labels.LabelUncompressed] = dgstr.Digest().String()
            } else {
                // 无压缩场景
                errOpen = archive.WriteDiff(ctx, cw, lowerRoot, upperRoot, writeDiffOpts...)
                if errOpen != nil {
                    return fmt.Errorf("failed to write diff: %w", errOpen)
                }
            }
            
            // ===== Step 8: 提交到 content store =====
            var commitopts []content.Opt
            if config.Labels != nil {
                commitopts = append(commitopts, content.WithLabels(config.Labels))
            }
            
            dgst := cw.Digest()  // 计算压缩后的 digest
            if errOpen = cw.Commit(ctx, 0, dgst, commitopts...); errOpen != nil {
                if !errdefs.IsAlreadyExists(errOpen) {
                    return fmt.Errorf("failed to commit: %w", errOpen)
                }
                errOpen = nil
            }
            
            // ===== Step 9: 获取最终信息 =====
            info, err := s.store.Info(ctx, dgst)
            if err != nil {
                return fmt.Errorf("failed to get info from content store: %w", err)
            }
            
            // 确保 uncompressed label 存在
            if info.Labels == nil {
                info.Labels = make(map[string]string)
            }
            if _, ok := info.Labels[labels.LabelUncompressed]; !ok {
                info.Labels[labels.LabelUncompressed] = config.Labels[labels.LabelUncompressed]
                s.store.Update(ctx, info, "labels."+labels.LabelUncompressed)
            }
            
            // ===== Step 10: 返回 descriptor =====
            ocidesc = ocispec.Descriptor{
                MediaType: config.MediaType,
                Size:      info.Size,       // 压缩后的大小
                Digest:    info.Digest,     // 压缩后的 digest
            }
            return nil
        })
    })
    
    if err != nil {
        return emptyDesc, err
    }
    return ocidesc, nil
}
```

### 5.3 关键辅助函数

```go
func uniqueRef() string {
    t := time.Now()
    var b [3]byte
    rand.Read(b[:])
    return fmt.Sprintf("%d-%s", t.UnixNano(), base64.URLEncoding.EncodeToString(b[:]))
    // 示例: "1705900800123456789-aB3cDe"
}
```

---

## 六、OverlayFS Differ 代码详解

### 6.1 核心优化

```go
// diff/overlayfs/differ.go

func (s *overlayfsDiff) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (d ocispec.Descriptor, err error) {
    // ★ 关键优化 1：提取 overlay layer 路径 ★
    layer, err := overlayMountsToLayer(upper)
    if err != nil {
        return emptyDesc, fmt.Errorf("failed to get overlay layer: %w", err)
    }
    
    // layer 示例: "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/123"
    // upperRoot: "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/123/fs"
    upperRoot := filepath.Join(layer, "fs")
    
    // ... (配置解析、压缩格式确定，与 walking differ 相同) ...
    
    // ★ 关键优化 2：直接使用 upperRoot，无需挂载 upper ★
    errOpen = writeDiff(ctx, writer, lower, upperRoot, config.SourceDateEpoch)
    
    // ... (提交到 content store，与 walking differ 相同) ...
}
```

### 6.2 overlayMountsToLayer 详解

```go
func overlayMountsToLayer(mounts []mount.Mount) (string, error) {
    if len(mounts) == 0 {
        return "", errors.New("no mounts provided")
    }
    
    // 检查是否是 overlay mount
    if mounts[0].Type != "overlay" {
        return "", fmt.Errorf("expected overlay mount type, got %s", mounts[0].Type)
    }
    
    mnt := mounts[0]
    var layer string
    var topLower string
    
    // 解析 mount options
    // 示例 options:
    // ["lowerdir=/a:/b:/c", "upperdir=/upper", "workdir=/work"]
    for _, o := range mnt.Options {
        if k, v, ok := strings.Cut(o, "="); ok {
            switch k {
            case "upperdir":
                // upperdir=/var/lib/.../snapshots/123/fs
                // 取父目录: /var/lib/.../snapshots/123
                layer = filepath.Dir(v)
            case "lowerdir":
                // lowerdir=/a:/b:/c
                // 取第一个（最上层）: /a
                dir, _, _ := strings.Cut(v, ":")
                topLower = filepath.Dir(dir)
            }
        }
    }
    
    // 优先返回 upperdir（可写层）
    // 如果没有 upperdir，返回 topLower（只读层的 diff）
    if layer == "" {
        if topLower == "" {
            return "", fmt.Errorf("unsupported overlay layer for overlayfs differ")
        }
        layer = topLower
    }
    return layer, nil
}
```

### 6.3 writeDiff 详解

```go
func writeDiff(ctx context.Context, w io.Writer, lower []mount.Mount, upperRoot string, sourceDateEpoch *time.Time) error {
    var opts []archive.ChangeWriterOpt
    if sourceDateEpoch != nil {
        opts = append(opts, archive.WithModTimeUpperBound(*sourceDateEpoch))
    }
    
    // ★ 只挂载 lower，用于对比 ★
    return mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
        // 创建 ChangeWriter
        cw := archive.NewChangeWriter(w, upperRoot, opts...)
        
        // ★ 使用 OverlayFS 特定的 diff 函数 ★
        // fs.DiffSourceOverlayFS: 告诉它这是 OverlayFS，会特殊处理 whiteout
        if err := fs.DiffDirChanges(ctx, lowerRoot, upperRoot, fs.DiffSourceOverlayFS, cw.HandleChange); err != nil {
            return fmt.Errorf("failed to calculate diff changes: %w", err)
        }
        return cw.Close()
    })
}
```

**fs.DiffDirChanges 的特殊处理**：

```
OverlayFS upperdir 的特殊文件：
1. .wh.filename      → 标记 filename 被删除
2. .wh..wh..opq      → 标记整个目录被删除（opaque whiteout）
3. 字符设备 (0/0)    → 也表示 whiteout

DiffDirChanges 会：
- 识别这些特殊文件
- 转换为 OCI 标准的 whiteout 格式
- 写入 tar 包
```

---

## 七、Diff Plugin 注册机制

### 7.1 Walking Differ 注册

```go
// diff/walking/plugin/plugin.go

func init() {
    plugin.Register(&plugin.Registration{
        Type: plugin.DiffPlugin,
        ID:   "walking",
        Requires: []plugin.Type{
            plugin.MetadataPlugin,  // 需要 metadata DB
        },
        InitFn: func(ic *plugin.InitContext) (interface{}, error) {
            // 获取 metadata plugin
            md, err := ic.Get(plugin.MetadataPlugin)
            if err != nil {
                return nil, err
            }
            
            // 获取 content store
            cs := md.(*metadata.DB).ContentStore()
            
            // 返回 diffPlugin（实现了 Comparer 和 Applier）
            return diffPlugin{
                Comparer: walking.NewWalkingDiff(cs),
                Applier:  apply.NewFileSystemApplier(cs),
            }, nil
        },
    })
}

// diffPlugin 组合了 Comparer 和 Applier
type diffPlugin struct {
    diff.Comparer
    diff.Applier
}
```

### 7.2 Differ 选择优先级

```go
// services/diff/local.go

type config struct {
    // Order: differ 的优先级
    // 默认: ["walking", "overlayfs"]
    // 第一个支持的 differ 会被使用
    Order []string `toml:"default"`
}

func (l *local) Diff(ctx, dr *diffapi.DiffRequest, ...) (*diffapi.DiffResponse, error) {
    var ocidesc ocispec.Descriptor
    var err error
    
    // 按优先级尝试每个 differ
    for _, differ := range l.differs {
        ocidesc, err = differ.Compare(ctx, aMounts, bMounts, opts...)
        if !errdefs.IsNotImplemented(err) {
            break  // 找到支持的 differ
        }
    }
    
    // ...
}
```

**配置示例**：

```toml
# /etc/containerd/config.toml

[plugins."io.containerd.differ.v1.walking"]
  # walking differ 总是支持

[plugins."io.containerd.differ.v1.overlayfs"]
  # overlayfs differ 只在 snapshotter 是 overlayfs 时支持
```

---

## 八、Diff 在 Commit 场景的完整调用链

### 8.1 示例：ctr snapshot diff

```bash
# 用户命令
ctr snapshot diff my-snapshot

# 生成的 tar 输出到 stdout
```

### 8.2 调用链

```
1. cmd/ctr/commands/snapshots/snapshots.go
   └─ diffCommand.Action()
       │
       ├─ client.SnapshotService(snapshotter)
       │   └─ 获取 snapshotter 实例
       │
       ├─ client.DiffService()
       │   └─ 获取 diff 服务
       │
       └─ rootfs.CreateDiff(ctx, snapshotID, snapshotter, diffService, opts...)
           ↓

2. rootfs/diff.go
   └─ CreateDiff(ctx, snapshotID, sn, d, opts...)
       │
       ├─ sn.Stat(ctx, snapshotID)
       │   └─ 获取 snapshot 信息
       │       - info.Parent: "parent-snapshot-id"
       │       - info.Kind: snapshots.KindActive
       │
       ├─ sn.View(ctx, lowerKey, info.Parent)
       │   └─ 创建父层的只读视图
       │       返回: lower = []mount.Mount{
       │           {Type: "overlay", Source: "overlay", Options: [...]},
       │       }
       │
       ├─ sn.Mounts(ctx, snapshotID)
       │   └─ 获取当前层的 mounts
       │       返回: upper = []mount.Mount{
       │           {Type: "overlay", Source: "overlay", Options: [...]},
       │       }
       │
       └─ d.Compare(ctx, lower, upper, opts...)
           ↓

3. services/diff/local.go
   └─ local.Diff(ctx, &diffapi.DiffRequest{...})
       │
       └─ 遍历 l.differs (按优先级)
           ├─ differ[0].Compare(ctx, lower, upper, opts...)
           │   └─ overlayfs.overlayfsDiff.Compare()
           │       或
           │   └─ walking.walkingDiff.Compare()
           │
           └─ 返回 ocispec.Descriptor
               ↓

4. diff/overlayfs/differ.go 或 diff/walking/differ.go
   └─ Compare(ctx, lower, upper, opts...)
       │
       ├─ overlayMountsToLayer(upper)
       │   └─ 提取 upperRoot 路径
       │
       ├─ mount.WithTempMount(ctx, lower, func(lowerRoot) {
       │       mount.WithReadonlyTempMount(ctx, upper, func(upperRoot) {
       │           // 挂载目录
       │       })
       │   })
       │
       ├─ s.store.Writer(ctx, content.WithRef(...))
       │   └─ 打开 content writer
       │
       ├─ archive.WriteDiff(ctx, writer, lowerRoot, upperRoot, opts...)
       │   └─ 遍历目录，写入 tar
       │       ├─ 新增文件 → tar entry
       │       ├─ 修改文件 → tar entry
       │       └─ 删除文件 → whiteout tar entry
       │
       ├─ cw.Commit(ctx, 0, dgst, commitopts...)
       │   └─ 提交到 content store
       │
       └─ 返回 ocispec.Descriptor{
               MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
               Digest:    "sha256:abc123...",
               Size:      1024567,
           }
           ↓

5. cmd/ctr/commands/snapshots/snapshots.go
   └─ client.ContentStore().ReaderAt(ctx, desc)
       │
       └─ io.Copy(os.Stdout, content.NewReader(ra))
           └─ 输出 tar.gz 到 stdout
```

---

## 九、关键数据结构

### 9.1 mount.Mount

```go
// mount/mount.go

type Mount struct {
    Type    string   // 文件系统类型，例如 "overlay", "bind", "ext4"
    Source  string   // 源路径或设备
    Options []string // 挂载选项
}

// OverlayFS mount 示例:
mount.Mount{
    Type: "overlay",
    Source: "overlay",
    Options: []string{
        "lowerdir=/var/lib/containerd/.../snapshots/1/fs:/var/lib/.../snapshots/2/fs",
        "upperdir=/var/lib/containerd/.../snapshots/3/fs",
        "workdir=/var/lib/containerd/.../snapshots/3/work",
    },
}
```

### 9.2 ocispec.Descriptor

```go
// OCI 镜像层描述符

type Descriptor struct {
    MediaType   string            // "application/vnd.oci.image.layer.v1.tar+gzip"
    Digest      digest.Digest     // "sha256:abc123..."
    Size        int64             // 1024567 (压缩后的字节数)
    Annotations map[string]string // 可选的注解
}
```

### 9.3 diff.Config

```go
// diff/diff.go

type Config struct {
    MediaType        string                  // layer media type
    Reference        string                  // content store reference
    Labels           map[string]string       // metadata labels
    Compressor       func(...) io.WriteCloser // 自定义压缩器
    SourceDateEpoch  *time.Time              // 可重现构建的时间戳
}
```

---

## 十、对你项目的启示

### 10.1 你需要实现的接口

如果要在 Devbox Snapshotter 中集成 LVM diff：

```go
// 实现 diff.Comparer 接口
type LVMDiffer struct {
    store content.Store
}

func (d *LVMDiffer) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (ocispec.Descriptor, error) {
    // 1. 从 mount options 提取 LV 信息
    baseLV := extractBaseLV(lower)
    writableLV := extractWritableLV(upper)
    
    // 2. 创建可写层的快照
    snapshotLV := lvm.CreateDevboxLVSnapshot(ctx, vgName, writableLV)
    
    // 3. 使用 thin_send 生成块级 diff
    var buf bytes.Buffer
    lvm.ThinSend(ctx, baseLV, snapshotLV, &buf)
    
    // 4. 解析 thin_send 输出，转换为文件级 diff
    changes := parseThinSendOutput(&buf)
    
    // 5. 生成 OCI tar layer
    cw, _ := d.store.Writer(ctx, ...)
    err := writeTarFromChanges(cw, changes)
    
    // 6. 提交到 content store
    cw.Commit(ctx, ...)
    
    return ocispec.Descriptor{...}, nil
}
```

### 10.2 集成到 Devbox Snapshotter

```go
// snapshots/devbox/plugin.go

func init() {
    plugin.Register(&plugin.Registration{
        Type: plugin.DiffPlugin,
        ID:   "devbox-lvm",
        Requires: []plugin.Type{
            plugin.MetadataPlugin,
        },
        InitFn: func(ic *plugin.InitContext) (interface{}, error) {
            md, _ := ic.Get(plugin.MetadataPlugin)
            cs := md.(*metadata.DB).ContentStore()
            
            return diffPlugin{
                Comparer: lvm.NewLVMDiffer(cs),  // 你的 LVM differ
                Applier:  apply.NewFileSystemApplier(cs),
            }, nil
        },
    })
}
```

### 10.3 配置优先级

```toml
# /etc/containerd/config.toml

[plugins."io.containerd.service.v1.diff-service"]
  default = ["devbox-lvm", "walking"]  # 优先使用 LVM differ
```

### 10.4 需要解决的问题

基于前面的 TODO，你需要：

1. ✅ **thin_send 集成** (已完成)
2. ⏳ **解析 thin_send 输出**
   - 块级别的变化 → 文件级别的变化
3. ⏳ **块到文件映射**
   - 使用 debugfs/xfs_db 或文件系统元数据
4. ⏳ **生成 OCI tar layer**
   - 符合 OCI 规范的 tar 格式
   - 正确处理 whiteout 文件
5. ⏳ **集成到 commit 流程**
   - 实现 diff.Comparer 接口
   - 注册为 diff plugin

---

## 十一、参考资源

### 11.1 相关代码文件

- `diff/diff.go`: 接口定义
- `diff/walking/differ.go`: 通用 differ 实现
- `diff/overlayfs/differ.go`: OverlayFS differ 实现
- `diff/apply/apply.go`: Layer applier 实现
- `rootfs/diff.go`: Commit 入口函数
- `archive/diff.go`: Tar 生成逻辑

### 11.2 OCI 规范

- [OCI Image Spec - Layer](https://github.com/opencontainers/image-spec/blob/main/layer.md)
- [OCI Image Spec - Descriptor](https://github.com/opencontainers/image-spec/blob/main/descriptor.md)

### 11.3 你的项目文档

- [ThinSend 实现文档](./thin-send-implementation.md)
- [ThinSend 测试指南](./thin-send-testing.md)
- [基准 LV 创建方案](./base-lv-creation-plan.md)

---

**文档版本**: 1.0.0  
**最后更新**: 2026-01-22  
**下一步**: 实现 thin_send 输出解析 → 文件映射 → OCI tar 生成

