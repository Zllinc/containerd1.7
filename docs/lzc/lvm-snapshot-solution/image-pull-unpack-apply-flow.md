# containerd 镜像拉取、Unpack、Apply 完整流程详解

**创建时间**: 2026-01-23  
**目的**: 详细解释 containerd 从 registry 拉取镜像到解压到 snapshotter 的完整流程

---

## 目录

1. [总体流程概览](#一总体流程概览)
2. [阶段 1: Pull - 镜像拉取](#二阶段-1-pull---镜像拉取)
3. [阶段 2: Unpack - 解压准备](#三阶段-2-unpack---解压准备)
4. [阶段 3: Apply - 层应用](#四阶段-3-apply---层应用)
5. [核心组件交互](#五核心组件交互)
6. [数据流转图](#六数据流转图)
7. [关键路径代码解析](#七关键路径代码解析)
8. [对 LVM Snapshotter 的意义](#八对-lvm-snapshotter-的意义)

---

## 一、总体流程概览

```
用户命令: ctr image pull docker.io/library/nginx:latest

整体流程:
┌─────────────────────────────────────────────────────────────┐
│ 1. Pull (拉取)                                              │
│    Registry → Content Store                                 │
│    - Resolve manifest                                       │
│    - Fetch layers (blobs)                                   │
│    - Store in content store                                 │
└─────────────────────────────────────────────────────────────┘
                        ↓
┌─────────────────────────────────────────────────────────────┐
│ 2. Unpack (解压)                                            │
│    Content Store → Snapshotter                              │
│    - For each layer:                                        │
│      * Prepare snapshot                                     │
│      * Apply layer                                          │
│      * Commit snapshot                                      │
└─────────────────────────────────────────────────────────────┘
                        ↓
┌─────────────────────────────────────────────────────────────┐
│ 3. 容器运行                                                 │
│    Snapshotter → Container Runtime                          │
│    - Create active snapshot from committed snapshots        │
│    - Mount for container use                                │
└─────────────────────────────────────────────────────────────┘
```

---

## 二、阶段 1: Pull - 镜像拉取

### 2.1 入口：`Client.Pull()`

**文件**: `pull.go`

```go
func (c *Client) Pull(ctx context.Context, ref string, opts ...RemoteOpt) (Image, error)
```

**调用栈**：
```
Client.Pull()
  ↓
1. defaultRemoteContext()          // 初始化拉取配置
2. c.WithLease(ctx)                // 创建租约（GC 保护）
3. 创建 Unpacker (如果需要 unpack)
4. c.fetch()                       // 实际拉取
5. unpacker.Wait()                 // 等待 unpack 完成
6. c.createNewImage()              // 创建镜像记录
```

### 2.2 核心步骤详解

#### 步骤 1: Resolve 镜像引用

```go
name, desc, err := rCtx.Resolver.Resolve(ctx, ref)
// 例如: "docker.io/library/nginx:latest"
// 返回: manifest descriptor
```

**作用**：  
- 连接到 registry（例如 docker.io）
- 解析镜像引用（tag → digest）
- 获取 manifest descriptor

**输出**：
```
descriptor:
  MediaType: application/vnd.docker.distribution.manifest.v2+json
  Digest: sha256:abc123...
  Size: 1234
```

#### 步骤 2: Fetch 内容

```go
fetcher, err := rCtx.Resolver.Fetcher(ctx, name)
handler = images.Handlers(...)
images.Dispatch(ctx, handler, nil, desc)
```

**Handler 链**：
```
images.Dispatch
  ↓
images.ChildrenHandler        // 递归获取子对象
  ↓
remotes.FetchHandler          // 下载 blob
  ↓
images.FilterHandler          // 过滤平台
  ↓
unpacker.Unpack (如果启用)   // 同时触发 unpack
```

**Fetch 过程**：
```
Manifest
  ↓ 解析
Config blob + Layer blobs
  ↓ 下载
Content Store
  - /var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/abc123...
```

### 2.3 Content Store 存储

**结构**：
```
/var/lib/containerd/io.containerd.content.v1.content/
├── blobs/
│   └── sha256/
│       ├── abc123... (manifest)
│       ├── def456... (config)
│       ├── 111222... (layer 1)
│       ├── 333444... (layer 2)
│       └── 555666... (layer 3)
└── ingest/       (临时下载目录)
```

**存储特点**：
- 按 digest 寻址（内容寻址存储 CAS）
- 不可变（immutable）
- 自动去重（相同 digest 只存一份）
- 压缩格式（tar.gz / tar.zstd）

---

## 三、阶段 2: Unpack - 解压准备

### 3.1 Unpacker 初始化

**文件**: `pkg/unpack/unpacker.go`

```go
unpacker, err = unpack.NewUnpacker(ctx, c.ContentStore(), uopts...)
```

**Unpacker 职责**：
- 管理并发 unpack
- 去重（避免重复 unpack）
- 与 Snapshotter + Applier 交互

### 3.2 Unpack 流程

```go
func (u *Unpacker) unpack(h images.Handler, config, layers []ocispec.Descriptor)
```

**核心循环**（逐层处理）：
```go
for i, desc := range layers {
    // 1. 准备快照
    mounts, err = sn.Prepare(ctx, key, parent.String(), opts...)
    
    // 2. 应用层
    diff, err = a.Apply(ctx, desc, mounts, applyOpts...)
    
    // 3. 提交快照
    err = sn.Commit(ctx, chainID, key, opts...)
    
    chain = append(chain, layer.Diff.Digest)
}
```

**关键概念**：
- **chain**: 累积的 layer digest 链
- **chainID**: `identity.ChainID(chain)` - 唯一标识快照
- **parent**: 父快照的 chainID（第一层为空）

### 3.3 逐层处理详解

#### Layer 1 (基础层)

```
输入:
  desc: layer 1 blob (sha256:111222...)
  parent: "" (空)

步骤 1: Prepare
  sn.Prepare(ctx, "extract-temp-123", "", opts)
    → 返回 mounts

步骤 2: Apply
  a.Apply(ctx, desc, mounts)
    → 从 content store 读取 layer 1 blob
    → 解压到 mounts 指向的目录
    → 返回 uncompressed digest

步骤 3: Commit
  chainID = ChainID([diffID-1])
  sn.Commit(ctx, chainID, "extract-temp-123")
    → 快照名: chainID-1

输出:
  - Snapshot: chainID-1 (已提交, 只读)
  - chain: [diffID-1]
```

#### Layer 2 (增量层)

```
输入:
  desc: layer 2 blob (sha256:333444...)
  parent: chainID-1

步骤 1: Prepare
  sn.Prepare(ctx, "extract-temp-456", chainID-1, opts)
    → 基于 chainID-1 创建可写快照
    → 返回 mounts (包含 chainID-1 作为 lowerdir)

步骤 2: Apply
  a.Apply(ctx, desc, mounts)
    → 从 content store 读取 layer 2 blob
    → 解压到 mounts 指向的目录
    → 增量内容叠加到 chainID-1 之上

步骤 3: Commit
  chainID = ChainID([diffID-1, diffID-2])
  sn.Commit(ctx, chainID, "extract-temp-456")
    → 快照名: chainID-2

输出:
  - Snapshot: chainID-2 (已提交, 只读)
  - chain: [diffID-1, diffID-2]
```

#### Layer 3 (最终层)

```
输入:
  desc: layer 3 blob (sha256:555666...)
  parent: chainID-2

步骤 1: Prepare
  sn.Prepare(ctx, "extract-temp-789", chainID-2, opts)

步骤 2: Apply
  a.Apply(ctx, desc, mounts)

步骤 3: Commit
  chainID = ChainID([diffID-1, diffID-2, diffID-3])
  sn.Commit(ctx, chainID, "extract-temp-789")
    → 快照名: chainID-3 (最终快照)

输出:
  - Snapshot: chainID-3 (已提交, 只读)
  - chain: [diffID-1, diffID-2, diffID-3]
```

---

## 四、阶段 3: Apply - 层应用

### 4.1 Apply 入口

**文件**: `diff/apply/apply.go`

```go
func (s *fsApplier) Apply(ctx, desc ocispec.Descriptor, mounts []mount.Mount, opts) (ocispec.Descriptor, error)
```

**Apply 职责**：
- 从 content store 读取 blob
- 解压缩（gzip / zstd）
- 解包 tar
- 写入到 mounts 指向的位置

### 4.2 Apply 核心流程

```go
// 步骤 1: 从 content store 获取 reader
ra, err := s.store.ReaderAt(ctx, desc)

// 步骤 2: 解压缩（如果需要）
processor := diff.NewProcessorChain(desc.MediaType, content.NewReader(ra))
// MediaType: application/vnd.oci.image.layer.v1.tar+gzip
// → 创建 gzip decompressor

// 步骤 3: 计算 digest（未压缩）
digester := digest.Canonical.Digester()
rc := io.TeeReader(processor, digester.Hash())

// 步骤 4: 调用底层 apply
err := apply(ctx, mounts, rc, config.SyncFs)

// 步骤 5: 返回未压缩 descriptor
return ocispec.Descriptor{
    MediaType: ocispec.MediaTypeImageLayer,
    Size:      rc.c,
    Digest:    digester.Digest(),  // 未压缩的 digest (diffID)
}
```

### 4.3 底层 apply 实现（Linux）

**文件**: `diff/apply/apply_linux.go`

```go
func apply(ctx, mounts []mount.Mount, r io.Reader, sync bool) error
```

**特化路径（OverlayFS 优化）**：

```go
case len(mounts) == 1 && mounts[0].Type == "overlay":
    // 提取 upperdir
    path, parents, err := getOverlayPath(mounts[0].Options)
    // path = "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/123/fs"
    
    // 直接 apply 到 upperdir
    opts := []archive.ApplyOpt{
        archive.WithConvertWhiteout(archive.OverlayConvertWhiteout),
    }
    _, err = archive.Apply(ctx, path, r, opts...)
    return err
```

**通用路径（挂载后 apply）**：

```go
default:
    return mount.WithTempMount(ctx, mounts, func(root string) error {
        _, err := archive.Apply(ctx, root, r)
        return err
    })
```

### 4.4 archive.Apply 详解

**文件**: `archive/tar.go`

```go
func Apply(ctx, dir string, r io.Reader, opts ...) (int64, error)
```

**核心逻辑**：
```go
tr := tar.NewReader(r)
for {
    hdr, err := tr.Next()
    if err == io.EOF {
        break
    }
    
    // 处理 whiteout
    if strings.HasPrefix(hdr.Name, ".wh.") {
        // 删除对应文件
        name := strings.TrimPrefix(hdr.Name, ".wh.")
        os.Remove(filepath.Join(dir, name))
        continue
    }
    
    // 创建文件/目录
    path := filepath.Join(dir, hdr.Name)
    switch hdr.Typeflag {
    case tar.TypeDir:
        os.MkdirAll(path, hdr.FileInfo().Mode())
    case tar.TypeReg:
        f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, hdr.FileInfo().Mode())
        io.Copy(f, tr)
        f.Close()
    case tar.TypeSymlink:
        os.Symlink(hdr.Linkname, path)
    // ... 其他类型
    }
}
```

**Whiteout 处理**（OverlayFS 特化）：
```go
// OverlayConvertWhiteout 将 .wh.* 文件转换为 overlayfs 的字符设备
func OverlayConvertWhiteout(w io.Writer, hdr *tar.Header, path string) error {
    if strings.HasPrefix(filepath.Base(path), ".wh.") {
        // mknod c 0 0
        unix.Mknod(path, unix.S_IFCHR, 0)
        return nil
    }
    return nil
}
```

---

## 五、核心组件交互

### 5.1 Content Store（内容存储）

**职责**：
- 存储不可变内容（manifest, config, layers）
- 按 digest 寻址
- 支持并发读取

**接口**：
```go
type Store interface {
    Info(ctx, dgst digest.Digest) (Info, error)
    ReaderAt(ctx, desc ocispec.Descriptor) (ReaderAt, error)
    Writer(ctx, opts ...WriterOpt) (Writer, error)
}
```

**存储位置**：
```
/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/
```

### 5.2 Snapshotter（快照管理）

**职责**：
- 管理文件系统快照
- 提供层叠加（layering）
- 支持 COW（Copy-on-Write）

**核心接口**：
```go
type Snapshotter interface {
    // 准备可写快照（用于 unpack）
    Prepare(ctx, key, parent string, opts ...) ([]mount.Mount, error)
    
    // 提交为只读快照
    Commit(ctx, name, key string, opts ...) error
    
    // 查看只读快照（用于容器启动）
    View(ctx, key, parent string, opts ...) ([]mount.Mount, error)
    
    // 获取快照的 mounts
    Mounts(ctx, key string) ([]mount.Mount, error)
}
```

**OverlayFS Snapshotter 存储**：
```
/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/
├── 1/                     ← chainID-1
│   ├── fs/                ← upperdir
│   └── work/
├── 2/                     ← chainID-2
│   ├── fs/
│   └── work/
└── 3/                     ← chainID-3
    ├── fs/
    └── work/
```

### 5.3 Applier（层应用器）

**职责**：
- 解压缩层
- 解包 tar
- 写入文件系统
- 处理 whiteout

**接口**：
```go
type Applier interface {
    Apply(ctx, desc ocispec.Descriptor, mounts []mount.Mount, opts ...) (ocispec.Descriptor, error)
}
```

---

## 六、数据流转图

### 6.1 Pull 阶段数据流

```
Registry (docker.io)
    ↓ HTTP GET
Manifest (json)
    ↓ 解析
Config blob + Layer blobs
    ↓ 并发下载
Content Store (本地存储)
    /var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/
        ├── abc123... (manifest)
        ├── def456... (config)
        ├── 111222... (layer 1 - compressed)
        ├── 333444... (layer 2 - compressed)
        └── 555666... (layer 3 - compressed)
```

### 6.2 Unpack 阶段数据流（单层）

```
Content Store
    ↓ ReaderAt
Compressed tar.gz blob
    ↓ gzip decompressor
Uncompressed tar stream
    ↓ tar reader
Individual files
    ↓ archive.Apply
Snapshotter mounts (写入目标)
    /var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/1/fs/
        ├── bin/
        ├── etc/
        └── usr/
```

### 6.3 多层 Unpack 流程

```
Layer 1:
Content Store (layer 1 blob)
    → Decompress → Untar
    → Snapshotter.Prepare("temp-1", "")
    → Apply to mounts
    → Snapshotter.Commit(chainID-1, "temp-1")

Layer 2:
Content Store (layer 2 blob)
    → Decompress → Untar
    → Snapshotter.Prepare("temp-2", chainID-1)  ← 基于 layer 1
    → Apply to mounts (增量叠加)
    → Snapshotter.Commit(chainID-2, "temp-2")

Layer 3:
Content Store (layer 3 blob)
    → Decompress → Untar
    → Snapshotter.Prepare("temp-3", chainID-2)  ← 基于 layer 1+2
    → Apply to mounts (增量叠加)
    → Snapshotter.Commit(chainID-3, "temp-3")

最终: chainID-3 包含完整镜像
```

---

## 七、关键路径代码解析

### 7.1 OverlayFS Snapshotter 的 Prepare

**文件**: `snapshots/overlay/overlay.go`

```go
func (o *snapshotter) Prepare(ctx, key, parent string, opts ...) ([]mount.Mount, error) {
    // 步骤 1: 创建快照目录
    snapshotDir := filepath.Join(o.root, "snapshots", id)
    os.MkdirAll(snapshotDir, 0700)
    
    // 步骤 2: 创建 fs 和 work 目录
    upperDir := filepath.Join(snapshotDir, "fs")
    workDir := filepath.Join(snapshotDir, "work")
    os.MkdirAll(upperDir, 0700)
    os.MkdirAll(workDir, 0700)
    
    // 步骤 3: 构造 mount 选项
    var options []string
    if parent != "" {
        // 有父层：构造 lowerdir
        lowerDirs := o.constructLowerDirs(parent)
        options = append(options, "lowerdir="+strings.Join(lowerDirs, ":"))
    }
    options = append(options, "upperdir="+upperDir)
    options = append(options, "workdir="+workDir)
    
    // 步骤 4: 返回 mount 描述
    return []mount.Mount{{
        Type:    "overlay",
        Source:  "overlay",
        Options: options,
    }}, nil
}
```

**返回的 mounts 示例**：
```go
[]mount.Mount{{
    Type: "overlay",
    Source: "overlay",
    Options: []string{
        "lowerdir=/snapshots/1/fs:/snapshots/2/fs",
        "upperdir=/snapshots/3/fs",
        "workdir=/snapshots/3/work",
    },
}}
```

### 7.2 Apply 如何使用 mounts

**特化路径（OverlayFS）**：
```go
// 不需要真实挂载 overlay
// 直接解析 upperdir 并写入
path := extractUpperDir(mounts[0].Options)  // "/snapshots/3/fs"
archive.Apply(ctx, path, tarReader)
```

**通用路径（其他 FS）**：
```go
// 需要真实挂载
mount.WithTempMount(ctx, mounts, func(root string) error {
    // root = /tmp/containerd-mount-xxx (overlay 已挂载)
    archive.Apply(ctx, root, tarReader)
})
```

### 7.3 OverlayFS 的 Commit

```go
func (o *snapshotter) Commit(ctx, name, key string, opts ...) error {
    // 步骤 1: 标记为只读
    info := o.getSnapshot(key)
    info.Kind = snapshots.KindCommitted
    info.Name = name
    
    // 步骤 2: 删除 workdir（只读快照不需要）
    workDir := filepath.Join(o.root, "snapshots", id, "work")
    os.RemoveAll(workDir)
    
    // 步骤 3: 持久化元数据
    o.saveMetadata(name, info)
    
    return nil
}
```

---

## 八、对 LVM Snapshotter 的意义

### 8.1 你的 base LV 方案在 Unpack 流程中的位置

```
原始 OverlayFS 流程:
Layer 1 → Prepare → upperdir: /snapshots/1/fs
Layer 2 → Prepare → upperdir: /snapshots/2/fs, lowerdir: /snapshots/1/fs
Layer 3 → Prepare → upperdir: /snapshots/3/fs, lowerdir: /snapshots/1/fs:/snapshots/2/fs
                                                        ↑
                                                 完整的 lowerdir

你的 base LV 方案:
在最后一层 unpack 时：
1) Prepare 不仅准备 writable LV
2) 同时创建 base LV
3) 将 lowerdir 的完整内容 apply 到 base LV
4) 以后 diff 时用 base LV vs writable LV snapshot
```

### 8.2 可以介入的点

**方案 A: 在 Prepare 时创建 base LV**

修改 `DevboxSnapshotter.Prepare()`：
```go
func (s *DevboxSnapshotter) Prepare(ctx, key, parent string, opts ...) ([]mount.Mount, error) {
    // 正常创建 writable LV
    writableLV := s.createWritableLV(key)
    
    // 如果这是最后一层（检测是否是 unpack）
    if isLastLayer {
        // 创建 base LV
        baseLV := s.createBaseLV(parent)
        
        // 挂载 parent (lowerdir 合并视图)
        parentMounts := s.getParentMounts(parent)
        mount.WithTempMount(ctx, parentMounts, func(lowerRoot string) {
            // 挂载 base LV
            baseMountPoint := s.mountBaseLV(baseLV)
            
            // 拷贝数据
            exec.Command("cp", "-a", lowerRoot+"/.", baseMountPoint).Run()
            
            s.umountBaseLV(baseLV)
        })
        
        // 记录 baseLV 与 parent 的映射
        s.saveBaseLVMapping(parent, baseLV)
    }
    
    // 返回 writable LV 的 mounts
    return s.getWritableLVMounts(writableLV), nil
}
```

**方案 B: 在 Commit 后创建 base LV**

修改 `DevboxSnapshotter.Commit()`:
```go
func (s *DevboxSnapshotter) Commit(ctx, name, key string, opts ...) error {
    // 正常提交
    s.commitSnapshot(name, key)
    
    // 如果这是镜像的最终层（chainID-final）
    if isFinalLayer(name) {
        // 创建 base LV
        baseLV := s.createBaseLV(name)
        
        // 挂载当前已提交的快照（完整视图）
        mounts := s.Mounts(ctx, name)
        mount.WithTempMount(ctx, mounts, func(root string) {
            baseMountPoint := s.mountBaseLV(baseLV)
            exec.Command("cp", "-a", root+"/.", baseMountPoint).Run()
            s.umountBaseLV(baseLV)
        })
        
        s.saveBaseLVMapping(name, baseLV)
    }
    
    return nil
}
```

### 8.3 Apply 直接写入 base LV（你的优化方案）

**挑战**：需要 Apply 同时写入两个目标（writable LV + base LV）

```go
func (s *DevboxSnapshotter) Prepare(ctx, key, parent string, opts ...) ([]mount.Mount, error) {
    writableLV := s.createWritableLV(key)
    
    // 如果是最后一层
    if isLastLayer {
        baseLV := s.createBaseLV(parent)
        
        // 返回特殊的 mounts：同时写入两个 LV
        return []mount.Mount{
            {Type: "bind", Source: s.mountWritableLV(writableLV)},
            {Type: "bind", Source: s.mountBaseLV(baseLV)},
        }, nil
    }
    
    return s.getWritableLVMounts(writableLV), nil
}
```

**问题**：Applier 只支持单个 mount set，需要自定义 Applier。

### 8.4 推荐的实现策略

```
阶段 1（当前可行）:
  - 保持 Prepare/Apply 不变
  - 在 Commit 后创建 base LV（方案 B）
  - 优点：无侵入性，易实现
  - 缺点：需要拷贝

阶段 2（优化）:
  - 修改 Prepare 直接创建 base LV（方案 A）
  - 在 Prepare 时同步拷贝
  - 优点：统一流程
  - 缺点：需要识别"最后一层"

阶段 3（终极优化）:
  - 自定义 Applier
  - Apply 时同时写入 base LV 和 writable LV
  - 优点：避免拷贝
  - 缺点：需要深度定制
```

---

## 九、关键要点总结

### 9.1 数据流总结

```
Registry
  ↓ Fetch (并发下载)
Content Store (压缩 blob)
  ↓ For each layer:
    ├─ Snapshotter.Prepare(key, parent) → mounts
    ├─ Applier.Apply(blob, mounts) → 解压 + 写入
    └─ Snapshotter.Commit(chainID, key)
Snapshotter (只读快照)
  ↓ 容器启动时:
    └─ Snapshotter.Prepare(containerKey, chainID-final)
Container (可写快照)
```

### 9.2 Mounts 的作用

```
mounts 是 Snapshotter 告诉 Applier "写到哪里" 的协议：

OverlayFS:
  mounts = [overlay + upperdir + lowerdir + workdir]
  → Apply 直接写 upperdir

LVM (你的实现):
  mounts = [bind + /dev/vg/lv 挂载点]
  → Apply 写入挂载点
  → 数据进入 LV
```

### 9.3 base LV 的创建时机

```
选项 1: Prepare 时
  - 可以同步拷贝
  - 需要识别"最后一层"
  - 阻塞 unpack 流程

选项 2: Commit 后
  - 不阻塞 unpack
  - 可以异步拷贝
  - 实现简单

选项 3: Apply 时（直接写入）
  - 需要自定义 Applier
  - 避免拷贝
  - 侵入性最大
```

---

## 十、参考代码路径

```
Pull:
  - pull.go: Client.Pull()
  - pull.go: Client.fetch()

Unpack:
  - pkg/unpack/unpacker.go: Unpacker.unpack()
  - rootfs/apply.go: applyLayers()

Apply:
  - diff/apply/apply.go: fsApplier.Apply()
  - diff/apply/apply_linux.go: apply()
  - archive/tar.go: Apply()

Snapshotter:
  - snapshots/overlay/overlay.go: Prepare(), Commit()
  - snapshots/snapshotter.go: 接口定义

Content Store:
  - content/local/store.go: 实现
```

---

**文档版本**: 1.0.0  
**最后更新**: 2026-01-23  
**相关文档**:
- [LVM Comparer OCI Layer 方案](./lvm-comparer-oci-layer-plan.md)
- [containerd Diff 机制详解](./containerd-diff-mechanism.md)
- [OverlayFS writeDiff 详解](./overlayfs-writediff-explained.md)

