# 自定义 Snapshotter 开发教程

## 目录
1. [理解 Snapshotter 接口](#1-理解-snapshotter-接口)
2. [实现方式选择](#2-实现方式选择)
3. [方法一：装饰器模式（推荐入门）](#3-方法一装饰器模式)
4. [方法二：完全重写（高级）](#4-方法二完全重写)
5. [集成到 Containerd](#5-集成到-containerd)
6. [实战案例对比](#6-实战案例对比)

---

## 1. 理解 Snapshotter 接口

### 1.1 核心概念

**Snapshotter 的职责**：管理容器的文件系统快照（rootfs）

```
镜像层 (只读)          容器运行时 (读写)
   ↓                      ↓
[layer1]             [prepare]
[layer2]      →      [mount] → 容器可以读写
[layer3]             [commit] → 保存为新镜像
   ↓                 [remove] → 清理
[committed]
```

### 1.2 关键方法说明

| 方法 | 用途 | 返回值 |
|------|------|--------|
| `Prepare(key, parent, opts)` | 创建**可写**快照（容器启动时） | `[]mount.Mount` |
| `View(key, parent, opts)` | 创建**只读**快照（检查镜像时） | `[]mount.Mount` |
| `Mounts(key)` | 获取已创建快照的挂载点 | `[]mount.Mount` |
| `Commit(name, key, opts)` | 将可写快照提交为只读快照（保存镜像） | `error` |
| `Remove(key)` | 删除快照（容器停止时） | `error` |
| `Stat(key)` | 获取快照信息（包含 Labels） | `Info` |

### 1.3 mount.Mount 结构

```go
type mount.Mount struct {
    Type    string   // 文件系统类型，如 "overlay", "bind"
    Source  string   // 源路径
    Options []string // 挂载选项
}

// overlayfs 的 Options 示例：
Options: []string{
    "lowerdir=/var/lib/containerd/snapshots/1/fs:/var/lib/containerd/snapshots/2/fs",
    "upperdir=/var/lib/containerd/snapshots/3/fs",
    "workdir=/var/lib/containerd/snapshots/3/work",
}
```

---

## 2. 实现方式选择

### 决策树

```
你的需求是？
├─ 只需修改 overlayfs 的挂载参数（添加 lower 层、修改选项）
│  └─ ✅ 使用装饰器模式（文档示例）
│
└─ 需要自定义存储后端（LVM、ZFS、自定义元数据）
   └─ ✅ 完全重写（devbox 方式）
```

---

## 3. 方法一：装饰器模式

### 3.1 架构设计

```
containerd
    ↓ (grpc)
你的 snapshotter 进程
    ↓
overlayCustomAddLowerSnapshotter (装饰器)
    ├─ Prepare()  → 调用 overlay.Prepare() → 修改 mounts
    ├─ View()     → 调用 overlay.View()    → 修改 mounts
    ├─ Mounts()   → 调用 overlay.Mounts()  → 修改 mounts
    └─ 其他方法    → 直接透传到 overlay
```

### 3.2 关键代码解析

#### Step 1: 定义常量

```go
const (
    // Label 必须以 "containerd.io/snapshot/" 开头才会被继承
    // 参见 snapshots/snapshotter.go@FilterInheritedLabels
    LabelCustomAddLowerPaths = "containerd.io/snapshot/overlay-custom-add-lower.paths"
)
```

**为什么必须这个前缀？** 看接口定义第 382-397 行：

```go
func FilterInheritedLabels(labels map[string]string) map[string]string {
    filtered := make(map[string]string)
    for k, v := range labels {
        if k == labelSnapshotRef || strings.HasPrefix(k, "containerd.io/snapshot/") {
            filtered[k] = v  // 只有这个前缀的 label 会被保留
        }
    }
    return filtered
}
```

#### Step 2: 嵌入式结构（装饰器核心）

```go
type overlayCustomAddLowerSnapshotter struct {
    snapshots.Snapshotter  // 嵌入接口，Go 会自动转发所有方法
}

// 只重写需要修改的方法
func (s *overlayCustomAddLowerSnapshotter) Prepare(ctx, key, parent string, opts ...Opt) ([]mount.Mount, error) {
    // 1. 调用原始实现
    mounts, err := s.Snapshotter.Prepare(ctx, key, parent, opts...)
    if err != nil {
        return nil, err
    }
    // 2. 修改返回值
    return s.tryAddLowers(ctx, key, mounts)
}
```

#### Step 3: 核心逻辑 - 修改 lowerdir

```go
func (s *overlayCustomAddLowerSnapshotter) tryAddLowers(ctx, key string, mounts []mount.Mount) ([]mount.Mount, error) {
    // 检查：只处理 overlay 类型的挂载
    if len(mounts) != 1 || mounts[0].Type != "overlay" {
        return mounts, nil
    }
    
    // 获取 snapshot 信息（包含 labels）
    info, err := s.Snapshotter.Stat(ctx, key)
    if err != nil {
        return nil, err
    }
    
    // 读取自定义 label
    lowerPathString, ok := info.Labels[LabelCustomAddLowerPaths]
    if !ok || lowerPathString == "" {
        return mounts, nil  // 没有自定义路径，直接返回
    }
    
    // 解析路径（支持 : 分隔多个路径）
    lowerPaths := strings.Split(lowerPathString, ":")
    
    // 确保目录存在
    for _, p := range lowerPaths {
        if p == "" {
            continue
        }
        err = os.MkdirAll(p, 0o755)
        if err != nil {
            return nil, fmt.Errorf("mkdir lower path %s error: %s", p, err)
        }
    }
    
    // 🔑 关键：修改 overlayfs 的 lowerdir 选项
    for i, o := range mounts[0].Options {
        if strings.HasPrefix(o, "lowerdir=") {
            // 原始: lowerdir=/镜像层1:/镜像层2
            // 修改后: lowerdir=/自定义路径:/镜像层1:/镜像层2
            originalLowerdir := strings.TrimPrefix(o, "lowerdir=")
            mounts[0].Options[i] = "lowerdir=" + lowerPathString + ":" + originalLowerdir
            break
        }
    }
    
    return mounts, nil
}
```

### 3.3 作为独立进程运行

containerd 支持两种插件模式：
- **内置插件**：编译到 containerd 二进制
- **外部插件（proxy plugin）**：独立进程，通过 gRPC 通信

文档采用的是**外部插件**方式：

```go
func main() {
    // 1. 创建 snapshotter 实例
    sn, err := snapshotter.NewSnapshotter(root, opts...)
    
    // 2. 包装成 gRPC service
    service := snapshotservice.FromSnapshotter(sn)
    
    // 3. 创建 gRPC server
    rpc := grpc.NewServer()
    snapshotsapi.RegisterSnapshotsServer(rpc, service)
    
    // 4. 监听 unix socket
    l, err := net.Listen("unix", "/path/to/grpc.sock")
    rpc.Serve(l)
}
```

### 3.4 Containerd 配置

```toml
version = 2

[proxy_plugins]
  [proxy_plugins.overlay-custom-add-lower-snapshotter]
    type = "snapshot"
    address = "/var/lib/containerd/.../grpc.sock"
```

### 3.5 使用示例

```bash
# 启动 snapshotter 进程
sudo ./overlay-custom-add-lower-snapshotter &

# 使用自定义 snapshotter 创建容器
sudo ctr run \
  --snapshotter overlay-custom-add-lower-snapshotter \
  --snapshotter-label containerd.io/snapshot/overlay-custom-add-lower.paths=/tmp/my-cache \
  docker.io/library/nginx:latest \
  my-container

# 现在容器内可以看到 /tmp/my-cache 的内容
# 但容器的修改不会影响 /tmp/my-cache（写到 upperdir）
```

---

## 4. 方法二：完全重写（Devbox 方式）

### 4.1 为什么需要完全重写？

Devbox 的需求：
1. ✅ **LVM 存储后端**：使用 LVM 逻辑卷而非普通目录
2. ✅ **存储配额**：每个容器限制存储大小
3. ✅ **内容 ID 管理**：多个容器共享同一个缓存层
4. ✅ **私有镜像层**：跳过父层，插入中间层

这些需求装饰器模式无法实现，必须控制整个快照生命周期。

### 4.2 核心实现要点

#### 4.2.1 自定义元数据存储

```go
// devbox/storage/bolt.go
const (
    DevboxKeyContentID = []byte("content_id")  // 内容 ID
    DevboxKeyPath      = []byte("path")        // 挂载路径
    DevboxKeyLvName    = []byte("lv_name")     // LVM 逻辑卷名
)

// 存储 snapshot → contentID → lvName 的映射
func SetDevboxContent(ctx, key, contentID, lvName, path string) error
```

#### 4.2.2 修改父层关系

```go
// devbox/devbox.go@createSnapshot
directParent := parent
if privateImageOk {  // 检查是否有 "devbox-init" label
    // 跳过直接父层，使用祖父层
    directParent, err = storage.GetParentID(ctx, parent)
}

s, err = storage.CreateSnapshot(ctx, kind, key, directParent, opts...)
```

**效果**：
```
正常:  container → image_layer1 → image_layer2
修改后: container → image_layer2  (layer1 变成额外的 lower)
```

#### 4.2.3 LVM 卷管理

```go
// 创建 LVM 逻辑卷
func (o *Snapshotter) createLVMVolume(lvName, size string) error {
    cmd := exec.Command("lvcreate", 
        "--thin", "-V", size, 
        "-n", lvName,
        fmt.Sprintf("%s/%s", o.lvmVgName, o.ThinPoolName))
    return cmd.Run()
}

// 挂载 LVM 卷
func (o *Snapshotter) mountLvm(ctx, lvName, path string) error {
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    return syscall.Mount(devicePath, path, "ext4", 0, "")
}
```

#### 4.2.4 多父层支持

```go
// devbox/devbox.go@mounts
parentPaths := make([]string, len(s.ParentIDs))
for i := range s.ParentIDs {
    parentPaths[i] = o.upperPath(s.ParentIDs[i])
}

options = append(options, 
    fmt.Sprintf("lowerdir=%s", strings.Join(parentPaths, ":")))
```

### 4.3 完全重写的文件结构

```
snapshots/devbox/
├── devbox.go           # 主实现（实现 Snapshotter 接口）
│   ├── type Snapshotter struct { ... }
│   ├── func (o *Snapshotter) Prepare() { ... }
│   ├── func (o *Snapshotter) Mounts() { ... }
│   └── func (o *Snapshotter) mounts(s storage.Snapshot) []mount.Mount
│
├── storage/
│   └── bolt.go         # 元数据存储（基于 BoltDB）
│
├── lvm/
│   └── lvm.go          # LVM 操作封装
│
└── plugin/
    └── plugin.go       # Containerd 插件注册
```

---

## 5. 集成到 Containerd

### 5.1 外部插件（文档方式）

**优点**：
- ✅ 独立进程，崩溃不影响 containerd
- ✅ 可以单独重启和升级
- ✅ 开发调试方便

**配置**：
```toml
[proxy_plugins.my-snapshotter]
  type = "snapshot"
  address = "/var/run/my-snapshotter.sock"
```

### 5.2 内置插件（Devbox 方式）

**优点**：
- ✅ 性能更好（无 gRPC 开销）
- ✅ 无需额外进程管理

**注册代码**（`snapshots/devbox/plugin/plugin.go`）：
```go
func init() {
    plugin.Register(&plugin.Registration{
        Type: plugin.SnapshotPlugin,
        ID:   "devbox",
        InitFn: func(ic *plugin.InitContext) (interface{}, error) {
            return devbox.NewSnapshotter(root, opts...)
        },
    })
}
```

**配置**：
```toml
[plugins."io.containerd.snapshotter.v1.devbox"]
  root_path = "/var/lib/containerd/io.containerd.snapshotter.v1.devbox"
```

---

## 6. 实战案例对比

### 案例 1：添加编译缓存层（装饰器方式）

**需求**：Go 项目编译时，希望容器能看到宿主的 `go mod cache`

**实现**：
```bash
# 宿主机准备缓存
sudo mkdir -p /opt/go-mod-cache
# ... 预先下载依赖到这个目录

# 启动容器
ctr run \
  --snapshotter my-snapshotter \
  --snapshotter-label containerd.io/snapshot/overlay-custom-add-lower.paths=/opt/go-mod-cache \
  golang:1.21 \
  build-container \
  go build ./...
```

**效果**：
```
容器视角:
/go/pkg/mod/  ← 包含宿主 /opt/go-mod-cache 的内容
              ← 容器新下载的依赖写到 upperdir
              ← 不会污染宿主的缓存目录
```

### 案例 2：多容器共享缓存（Devbox 方式）

**需求**：10 个开发容器共享同一个 node_modules 缓存，但各自的修改互不影响

**实现**：
```go
// 创建共享缓存快照
ctr snapshot prepare \
  --snapshotter devbox \
  --label containerd.io/snapshot/devbox-content-id=shared-node-modules \
  --label containerd.io/snapshot/devbox-storage-limit=10Gi \
  shared-cache

// ... 预填充 node_modules

ctr snapshot commit shared-cache shared-cache-v1

// 容器 1
ctr run \
  --snapshotter devbox \
  --snapshotter-label containerd.io/snapshot/devbox-init=true \
  node:18 dev1

// 容器 2（共享同一个 LVM 卷）
ctr run \
  --snapshotter devbox \
  --snapshotter-label containerd.io/snapshot/devbox-init=true \
  node:18 dev2
```

**效果**：
- ✅ 10 个容器共享同一个 10GB 的 LVM 逻辑卷（节省空间）
- ✅ 每个容器有独立的 upperdir（写隔离）
- ✅ 存储配额限制（LVM thin pool）

---

## 7. 常见问题

### Q1: Label 为什么必须是 `containerd.io/snapshot/` 前缀？

**A**: 见 `snapshots/snapshotter.go@FilterInheritedLabels`，containerd 会过滤 labels，只有这个前缀的才会从父快照继承到子快照。

### Q2: 装饰器模式能否支持 LVM？

**A**: 不能。`Prepare()` 返回的 mounts 只是挂载参数，无法控制底层存储的创建过程。LVM 卷必须在返回 mounts 之前创建好。

### Q3: 如何调试 snapshotter？

**A**: 
```bash
# 查看日志
journalctl -u containerd -f

# 查看 snapshot 信息
ctr snapshot --snapshotter my-snapshotter ls
ctr snapshot --snapshotter my-snapshotter info <key>

# 查看挂载参数
ctr snapshot --snapshotter my-snapshotter mounts <key>
```

### Q4: CRI 如何传递 labels？

**A**: 需要修改 `pkg/cri/server/container_create.go`：

```go
// 从 Pod annotations 读取配置
devboxOpt, err := devboxSnapshotterOpts(runtime, r.GetSandboxConfig())
sOpts = append(sOpts, devboxOpt)  // 传递给 snapshotter
```

---

## 8. 总结

| 选择 | 适用场景 | 难度 |
|------|---------|------|
| **装饰器模式** | 只需修改 overlayfs 参数 | ⭐ 简单 |
| **完全重写** | 需要自定义存储、元数据、复杂逻辑 | ⭐⭐⭐⭐⭐ 困难 |

**建议**：
1. 🎯 先用装饰器模式实现 MVP
2. 🎯 确认需求后，再考虑是否需要完全重写
3. 🎯 参考 devbox 代码学习高级技巧

---

**参考资源**：
- Containerd Plugin 文档: https://github.com/containerd/containerd/blob/main/docs/PLUGINS.md
- Overlay Snapshotter 源码: `snapshots/overlay/overlay.go`
- Devbox Snapshotter 源码: `snapshots/devbox/devbox.go`

