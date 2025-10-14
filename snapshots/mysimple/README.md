# MySimple Snapshotter - 完整开发教程

这是一个用于学习如何开发 containerd snapshotter 的完整示例。

## 📚 目录

1. [项目结构](#项目结构)
2. [从零开始的实现步骤](#从零开始的实现步骤)
3. [编译 containerd](#编译-containerd)
4. [配置和使用](#配置和使用)
5. [调试和验证](#调试和验证)
6. [进阶：完全重写](#进阶完全重写)

---

## 项目结构

```
containerd1.7/
├── snapshots/
│   └── mysimple/                    # ← 我们的新 snapshotter
│       ├── mysimple.go              # 核心实现
│       ├── plugin/
│       │   └── plugin.go            # 插件注册
│       └── README.md                # 本文档
│
└── cmd/containerd/builtins/
    └── mysimple_linux.go            # 导入插件到 containerd
```

---

## 从零开始的实现步骤

### 步骤 1: 创建目录结构

```bash
cd /root/containerd1.7

# 创建 snapshotter 包目录
mkdir -p snapshots/mysimple/plugin

# 创建 builtins 导入文件目录（如果不存在）
mkdir -p cmd/containerd/builtins
```

### 步骤 2: 实现 Snapshotter 接口

创建 `snapshots/mysimple/mysimple.go`，核心要点：

```go
package mysimple

import (
    "github.com/containerd/containerd/snapshots"
    "github.com/containerd/containerd/snapshots/overlay"
)

// 定义结构体
type snapshotter struct {
    snapshots.Snapshotter  // 嵌入底层实现
    root string
}

// 工厂函数（插件会调用）
func NewSnapshotter(root string, opts ...Opt) (snapshots.Snapshotter, error) {
    baseSnapshotter, err := overlay.NewSnapshotter(root, opts...)
    if err != nil {
        return nil, err
    }
    
    return &snapshotter{
        Snapshotter: baseSnapshotter,
        root: root,
    }, nil
}

// 重写关键方法
func (s *snapshotter) Prepare(ctx, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
    // 1. 调用底层实现
    mounts, err := s.Snapshotter.Prepare(ctx, key, parent, opts...)
    if err != nil {
        return nil, err
    }
    
    // 2. 自定义逻辑
    return s.customizeMounts(ctx, key, mounts)
}

// 自定义逻辑
func (s *snapshotter) customizeMounts(ctx, key string, mounts []mount.Mount) ([]mount.Mount, error) {
    // 根据 labels 修改 mounts
    // 详见完整代码
}
```

### 步骤 3: 创建插件注册文件

创建 `snapshots/mysimple/plugin/plugin.go`：

```go
package overlay  // ⚠️ 注意：包名必须是 overlay

import (
    "github.com/containerd/containerd/plugin"
    "github.com/containerd/containerd/snapshots/mysimple"
)

type Config struct {
    RootPath      string `toml:"root_path"`
    UpperdirLabel bool   `toml:"upperdir_label"`
}

func init() {
    plugin.Register(&plugin.Registration{
        Type:   plugin.SnapshotPlugin,
        ID:     "mysimple",  // ← 插件 ID
        Config: &Config{},
        InitFn: func(ic *plugin.InitContext) (interface{}, error) {
            config := ic.Config.(*Config)
            root := ic.Root
            if config.RootPath != "" {
                root = config.RootPath
            }
            
            var opts []mysimple.Opt
            if config.UpperdirLabel {
                opts = append(opts, mysimple.WithUpperdirLabel)
            }
            
            return mysimple.NewSnapshotter(root, opts...)
        },
    })
}
```

**关键点**：
- ✅ 包名必须是 `overlay`（containerd 的约定）
- ✅ `ID` 是插件的唯一标识符
- ✅ `InitFn` 返回 `snapshots.Snapshotter` 接口

### 步骤 4: 注册到 containerd builtins

创建 `cmd/containerd/builtins/mysimple_linux.go`：

```go
//go:build linux

package builtins

import (
    // 导入插件包，触发 init() 执行
    _ "github.com/containerd/containerd/snapshots/mysimple/plugin"
)
```

**作用**：containerd 启动时会导入这个文件，从而执行插件的 `init()` 函数。

---

## 编译 containerd

### 方法 1: 完整编译

```bash
cd /root/containerd1.7

# 编译 containerd（包含所有插件）
make clean
make

# 编译产物
ls -lh bin/
# containerd
# ctr
# containerd-shim-runc-v2
```

### 方法 2: 仅编译 containerd

```bash
cd /root/containerd1.7

# 只编译 containerd 二进制
go build -o bin/containerd ./cmd/containerd

# 验证编译成功
./bin/containerd --version
```

### 验证插件是否包含

```bash
# 启动 containerd（测试模式）
sudo ./bin/containerd --config /dev/null &

# 查看已注册的插件
sudo ./bin/ctr plugins ls | grep snapshotter

# 应该能看到：
# io.containerd.snapshotter.v1.mysimple    linux/amd64    ok
```

---

## 配置和使用

### 1. 配置 containerd

编辑 `/etc/containerd/config.toml`：

```toml
version = 2

# mysimple snapshotter 的配置
[plugins."io.containerd.snapshotter.v1.mysimple"]
  root_path = "/var/lib/containerd/io.containerd.snapshotter.v1.mysimple"
  upperdir_label = true
  sync_remove = false

# 设置为默认 snapshotter（可选）
[plugins."io.containerd.grpc.v1.cri".containerd]
  snapshotter = "mysimple"
```

### 2. 重启 containerd

```bash
# 停止旧的 containerd
sudo systemctl stop containerd

# 替换二进制（可选）
sudo cp bin/containerd /usr/local/bin/containerd

# 启动新的 containerd
sudo systemctl start containerd

# 检查状态
sudo systemctl status containerd
```

### 3. 使用 mysimple snapshotter

#### 方式 1: 命令行指定

```bash
# 拉取镜像
sudo ctr images pull docker.io/library/nginx:latest

# 准备自定义 lower 层目录
sudo mkdir -p /opt/my-cache
sudo touch /opt/my-cache/test.txt
echo "Hello from custom lower layer" | sudo tee /opt/my-cache/test.txt

# 使用 mysimple snapshotter 启动容器
sudo ctr run \
  --snapshotter mysimple \
  --snapshotter-label containerd.io/snapshot/mysimple.custom-lowers=/opt/my-cache \
  --rm -t \
  docker.io/library/nginx:latest \
  test-container \
  bash

# 在容器内验证
root@container# ls -la /opt/my-cache/test.txt
root@container# cat /opt/my-cache/test.txt
```

#### 方式 2: 默认使用（已配置为默认 snapshotter）

```bash
# 直接创建容器，自动使用 mysimple
sudo ctr run --rm -t docker.io/library/nginx:latest test bash
```

---

## 调试和验证

### 1. 查看日志

```bash
# 查看 containerd 日志
sudo journalctl -u containerd -f

# 查看 mysimple 相关日志
sudo journalctl -u containerd -f | grep mysimple
```

### 2. 查看 snapshot 信息

```bash
# 列出所有 snapshots
sudo ctr snapshot --snapshotter mysimple ls

# 查看特定 snapshot 的详细信息
sudo ctr snapshot --snapshotter mysimple info <key>

# 查看挂载信息
sudo ctr snapshot --snapshotter mysimple mounts <key>
```

### 3. 验证自定义功能

```bash
# 创建测试目录
sudo mkdir -p /opt/test-lower
sudo touch /opt/test-lower/from-host.txt

# 使用自定义 lower 创建容器
sudo ctr run \
  --snapshotter mysimple \
  --snapshotter-label containerd.io/snapshot/mysimple.custom-lowers=/opt/test-lower \
  --rm -t nginx:latest test bash

# 容器内应该能看到 /opt/test-lower/from-host.txt
```

### 4. 查看实际的挂载参数

```bash
# 创建快照
sudo ctr snapshot --snapshotter mysimple prepare \
  --label containerd.io/snapshot/mysimple.custom-lowers=/tmp/lower1:/tmp/lower2 \
  test-snap sha256:xxxxx

# 查看挂载参数
sudo ctr snapshot --snapshotter mysimple mounts test-snap

# 输出示例：
# overlay
# overlay
# lowerdir=/tmp/lower1:/tmp/lower2:/var/lib/.../snapshots/xxx/fs:...
# upperdir=/var/lib/.../snapshots/yyy/fs
# workdir=/var/lib/.../snapshots/yyy/work
```

---

## 进阶：完全重写

如果需要像 devbox 那样完全控制（LVM、自定义元数据等），需要：

### 1. 不依赖 overlay，自己实现所有方法

```go
type snapshotter struct {
    root string
    ms   MetaStore  // 自己的元数据存储
}

func (s *snapshotter) Stat(ctx, key) (Info, error) {
    // 从元数据存储读取
}

func (s *snapshotter) Prepare(ctx, key, parent string, opts) ([]mount.Mount, error) {
    // 1. 创建 snapshot 目录
    // 2. 处理父子关系
    // 3. 生成 overlayfs mounts
    // 4. 保存元数据
}

// ... 实现其他所有方法
```

### 2. 自定义元数据存储

参考 `snapshots/overlay/overlay.go` 和 `snapshots/storage/`：

```go
import "github.com/containerd/containerd/snapshots/storage"

type snapshotter struct {
    ms *storage.MetaStore
}

func (s *snapshotter) Prepare(ctx, key, parent string, opts) ([]mount.Mount, error) {
    var snapshot storage.Snapshot
    
    err := s.ms.WithTransaction(ctx, true, func(ctx) error {
        // 使用事务创建 snapshot
        snapshot, err = storage.CreateSnapshot(ctx, kind, key, parent, opts...)
        return err
    })
    
    return s.mounts(snapshot), nil
}
```

### 3. LVM 集成（参考 devbox）

```go
func (s *snapshotter) Prepare(ctx, key, parent string, opts) ([]mount.Mount, error) {
    // 1. 解析 labels（存储大小等）
    // 2. 创建 LVM 逻辑卷
    cmd := exec.Command("lvcreate", "-L", size, "-n", lvName, vgName)
    
    // 3. 格式化
    cmd = exec.Command("mkfs.ext4", devicePath)
    
    // 4. 挂载
    syscall.Mount(devicePath, mountPoint, "ext4", 0, "")
    
    // 5. 返回 mounts
}
```

---

## 与现有 Snapshotter 的对比

| 特性 | mysimple | overlay | devbox |
|------|----------|---------|--------|
| **实现方式** | 包装 overlay | 完全实现 | 完全实现 |
| **代码量** | ~300 行 | ~800 行 | ~1500 行 |
| **存储后端** | overlay 目录 | overlay 目录 | LVM |
| **元数据** | overlay 的 BoltDB | 自己的 BoltDB | 扩展的 BoltDB |
| **自定义功能** | 添加 lower 层 | 标准 overlayfs | LVM + 多层 + 配额 |
| **适用场景** | 学习/简单定制 | 生产环境 | 复杂场景 |

---

## 常见问题

### Q1: 为什么 plugin 包的包名必须是 `overlay`？

**A**: containerd 的插件加载机制的历史原因。实际上包名可以是任何名字，但约定俗成使用 `overlay`。

### Q2: 如何在 Kubernetes 中使用？

**A**: 修改 CRI 配置：

```toml
[plugins."io.containerd.grpc.v1.cri".containerd]
  snapshotter = "mysimple"

# 或者为特定 runtime 配置
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.myruntime]
  snapshotter = "mysimple"
```

然后在 Pod 中使用：
```yaml
spec:
  runtimeClassName: myruntime
```

### Q3: Label 传递到容器怎么实现？

**A**: 需要修改 `pkg/cri/server/container_create.go`，将 Pod annotations 转换为 snapshot labels：

```go
// container_create.go
sOpts, err := snapshotterOpts(...)

// 添加自定义逻辑
if customPath := sandboxConfig.Annotations["mysimple.io/custom-lowers"]; customPath != "" {
    sOpts = append(sOpts, snapshots.WithLabels(map[string]string{
        "containerd.io/snapshot/mysimple.custom-lowers": customPath,
    }))
}
```

### Q4: 如何测试插件？

**A**: 
```bash
# 单元测试
cd snapshots/mysimple
go test -v ./...

# 集成测试
sudo ctr snapshot --snapshotter mysimple prepare test-snap ""
sudo ctr snapshot --snapshotter mysimple ls
sudo ctr snapshot --snapshotter mysimple remove test-snap
```

---

## 总结

这个 mysimple snapshotter 展示了：

✅ **最小化的完整实现**
- 核心代码 ~300 行
- 完整的插件注册流程
- 可实际运行和测试

✅ **清晰的架构**
- mysimple.go: 核心逻辑
- plugin/plugin.go: 插件注册
- builtins/mysimple_linux.go: 导入到 containerd

✅ **可扩展的设计**
- 包装模式便于快速开发
- 可逐步演进为完全重写
- 详细注释便于学习

**下一步**：
1. 编译和测试这个实现
2. 根据需求添加自定义功能
3. 参考 devbox 学习高级特性（LVM、多层等）

Happy Coding! 🚀

