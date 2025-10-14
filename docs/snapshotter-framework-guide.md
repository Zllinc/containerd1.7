# Snapshotter 开发框架完全指南

## 🎯 完整流程图

### 1. 编译时（Build Time）

```
┌─────────────────────────────────────────────────────────────────┐
│                   开发者编写代码                                  │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  步骤 1: 创建 Snapshotter 实现                                   │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ snapshots/mysimple/mysimple.go                            │  │
│  │                                                           │  │
│  │ type snapshotter struct {                                │  │
│  │     snapshots.Snapshotter  // 嵌入或自己实现              │  │
│  │ }                                                         │  │
│  │                                                           │  │
│  │ func NewSnapshotter(root string, opts) (...) { ... }    │  │
│  │ func (s *snapshotter) Prepare(...) { ... }              │  │
│  │ func (s *snapshotter) View(...) { ... }                 │  │
│  │ func (s *snapshotter) Mounts(...) { ... }               │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  步骤 2: 创建插件注册文件                                        │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ snapshots/mysimple/plugin/plugin.go                       │  │
│  │                                                           │  │
│  │ func init() {                                            │  │
│  │     plugin.Register(&plugin.Registration{               │  │
│  │         Type: plugin.SnapshotPlugin,                     │  │
│  │         ID: "mysimple",           ← 插件ID               │  │
│  │         Config: &Config{},        ← 配置结构             │  │
│  │         InitFn: func(ic *plugin.InitContext) {           │  │
│  │             return mysimple.NewSnapshotter(...)          │  │
│  │         },                                               │  │
│  │     })                                                   │  │
│  │ }                                                        │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  步骤 3: 导入到 containerd builtins                              │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ cmd/containerd/builtins/mysimple_linux.go                 │  │
│  │                                                           │  │
│  │ import (                                                 │  │
│  │     _ "github.com/.../snapshots/mysimple/plugin"        │  │
│  │ )                                                        │  │
│  │                                                          │  │
│  │ ← 触发 plugin/plugin.go 的 init() 执行                   │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  步骤 4: 编译 containerd                                         │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ $ make                                                    │  │
│  │   或                                                      │  │
│  │ $ go build -o bin/containerd ./cmd/containerd            │  │
│  │                                                           │  │
│  │ ✅ 生成 bin/containerd (包含 mysimple 插件)               │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘

```

---

### 2. 启动时（Runtime - Containerd Startup）

```
┌─────────────────────────────────────────────────────────────────┐
│  用户启动 containerd                                             │
│  $ sudo systemctl start containerd                               │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  containerd 进程启动                                             │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ cmd/containerd/main.go                                    │  │
│  │   ↓                                                       │  │
│  │ import "...cmd/containerd/builtins"                       │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  Go 导入包，执行所有 init() 函数                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ builtins/mysimple_linux.go                                │  │
│  │   → import _ "...mysimple/plugin"                         │  │
│  │   → 执行 plugin/plugin.go 的 init()                       │  │
│  │   → plugin.Register(...) 被调用                           │  │
│  │   → mysimple 插件注册到全局注册表                          │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  读取配置文件                                                    │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ /etc/containerd/config.toml                               │  │
│  │                                                           │  │
│  │ [plugins."io.containerd.snapshotter.v1.mysimple"]        │  │
│  │   root_path = "/var/lib/mysimple"                        │  │
│  │   upperdir_label = true                                  │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  初始化已注册的插件                                              │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ 对于每个 Type=SnapshotPlugin 的插件：                      │  │
│  │   1. 解析配置到 Config 结构体                             │  │
│  │   2. 调用 InitFn(InitContext)                            │  │
│  │   3. InitFn 内部调用 NewSnapshotter(...)                 │  │
│  │   4. 返回 snapshots.Snapshotter 实例                      │  │
│  │   5. 存储到插件管理器                                     │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  containerd 就绪                                                 │
│  ✅ mysimple snapshotter 已加载并可用                            │
└─────────────────────────────────────────────────────────────────┘
```

---

### 3. 使用时（Runtime - Container Creation）

```
┌─────────────────────────────────────────────────────────────────┐
│  用户创建容器                                                    │
│  $ ctr run --snapshotter mysimple \                              │
│      --snapshotter-label containerd.io/snapshot/mysimple.xxx=... │
│      nginx:latest test                                           │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  CRI Server 处理请求                                             │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ pkg/cri/server/container_create.go                        │  │
│  │                                                           │  │
│  │ func (c *criService) CreateContainer(...)                │  │
│  │   ↓                                                       │  │
│  │ 1. 解析镜像                                               │  │
│  │ 2. 构建 snapshot options (labels)                        │  │
│  │ 3. 获取 snapshotter ("mysimple")                         │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  调用 Snapshotter.Prepare()                                      │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ snapshotter.Prepare(ctx, containerID, imageChainID, opts) │  │
│  │                                                           │  │
│  │ opts 包含:                                                │  │
│  │   - WithLabels({"containerd.io/snapshot/mysimple.xxx"})  │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  mysimple.snapshotter.Prepare() 执行                             │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ snapshots/mysimple/mysimple.go                            │  │
│  │                                                           │  │
│  │ func (s *snapshotter) Prepare(...) {                     │  │
│  │   // 1. 调用底层 overlay.Prepare()                       │  │
│  │   mounts, err := s.Snapshotter.Prepare(...)              │  │
│  │                                                           │  │
│  │   // 2. 读取 labels                                      │  │
│  │   info, _ := s.Snapshotter.Stat(ctx, key)                │  │
│  │   customLowers := info.Labels["containerd.io/..."]       │  │
│  │                                                           │  │
│  │   // 3. 修改 mounts                                      │  │
│  │   for i, opt := range mounts[0].Options {                │  │
│  │     if strings.HasPrefix(opt, "lowerdir=") {             │  │
│  │       // 添加自定义路径到 lowerdir                        │  │
│  │       mounts[0].Options[i] = "lowerdir=" +               │  │
│  │         customLowers + ":" + originalLowerdir            │  │
│  │     }                                                     │  │
│  │   }                                                       │  │
│  │                                                           │  │
│  │   // 4. 返回修改后的 mounts                              │  │
│  │   return mounts, nil                                     │  │
│  │ }                                                        │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  返回 mounts 给 CRI                                              │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ []mount.Mount{                                            │  │
│  │   {                                                       │  │
│  │     Type: "overlay",                                     │  │
│  │     Source: "overlay",                                   │  │
│  │     Options: [                                           │  │
│  │       "lowerdir=/custom/path:/镜像层1:/镜像层2",          │  │
│  │       "upperdir=/var/lib/.../容器层/fs",                 │  │
│  │       "workdir=/var/lib/.../容器层/work",                │  │
│  │     ],                                                   │  │
│  │   },                                                     │  │
│  │ }                                                        │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  Runtime 挂载文件系统                                            │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ $ mount -t overlay overlay \                              │  │
│  │     -o lowerdir=/custom/path:/镜像层1:/镜像层2,\          │  │
│  │        upperdir=/容器层/fs,\                              │  │
│  │        workdir=/容器层/work \                             │  │
│  │     /run/containerd/io.../rootfs                          │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────────┐
│  容器启动成功                                                    │
│  ✅ 容器内可以看到 /custom/path 的内容                           │
│  ✅ 容器的修改写入 upperdir，不影响 /custom/path                 │
└─────────────────────────────────────────────────────────────────┘
```

---

## 📂 关键文件的作用

### 文件 1: `snapshots/mysimple/mysimple.go`

**作用**: Snapshotter 的核心实现

**关键代码**:
```go
type snapshotter struct {
    snapshots.Snapshotter  // 嵌入或自己实现
}

func NewSnapshotter(root string, opts ...Opt) (snapshots.Snapshotter, error) {
    // 工厂函数，插件系统会调用
}

func (s *snapshotter) Prepare/View/Mounts(...) {
    // 实现核心逻辑
}
```

**调用时机**: 容器操作时（创建/查看快照）

---

### 文件 2: `snapshots/mysimple/plugin/plugin.go`

**作用**: 插件注册和配置

**关键代码**:
```go
func init() {
    plugin.Register(&plugin.Registration{
        ID: "mysimple",
        InitFn: func(ic *plugin.InitContext) (interface{}, error) {
            return mysimple.NewSnapshotter(...)
        },
    })
}
```

**调用时机**: containerd 启动时（自动执行）

---

### 文件 3: `cmd/containerd/builtins/mysimple_linux.go`

**作用**: 将插件链接到 containerd

**关键代码**:
```go
import _ "github.com/containerd/containerd/snapshots/mysimple/plugin"
```

**调用时机**: 编译时（导入）+ 运行时（执行 init）

---

## 🔍 与 Devbox 的实现对比

### mysimple (简化版)

```
mysimple/
├── mysimple.go           (~200 行)
│   └── 包装 overlay.Snapshotter
│       └── 只重写 Prepare/View/Mounts
│
└── plugin/
    └── plugin.go         (~100 行)
        └── 插件注册
```

### devbox (完整版)

```
devbox/
├── devbox.go            (~1000 行)
│   ├── 完全实现所有接口方法
│   ├── LVM 卷管理
│   ├── 自定义父层处理
│   └── mounts() 生成 overlayfs 选项
│
├── storage/
│   └── bolt.go          (~500 行)
│       └── 扩展的元数据存储
│           ├── contentID → lvName 映射
│           ├── 挂载状态跟踪
│           └── 多容器共享管理
│
├── lvm/
│   └── lvm.go           (~300 行)
│       ├── lvcreate
│       ├── lvresize
│       └── lvremove
│
└── plugin/
    └── plugin.go        (~100 行)
        └── 插件注册 + 复杂配置
```

---

## 🎓 学习路径

### 阶段 1: 理解框架（当前）

✅ 了解三个关键文件的作用  
✅ 理解编译时 → 启动时 → 使用时的流程  
✅ 能够创建最小化的 snapshotter  

**实践**:
```bash
cd /root/containerd1.7
make
sudo ./bin/ctr plugins ls | grep mysimple
```

---

### 阶段 2: 实现基础功能

✅ 实现 Prepare/View/Mounts  
✅ 读取和使用 labels  
✅ 修改 overlayfs 挂载选项  

**实践**:
```bash
# 测试自定义 lower 层
sudo ctr run \
  --snapshotter mysimple \
  --snapshotter-label containerd.io/snapshot/mysimple.custom-lowers=/tmp/test \
  nginx:latest test
```

---

### 阶段 3: 高级特性（参考 devbox）

✅ 自己管理元数据（不依赖 overlay）  
✅ LVM 集成  
✅ 多层父子关系处理  
✅ 存储配额  

**实践**: 逐步将 mysimple 改造为完全自实现

---

## 🐛 常见错误和调试

### 错误 1: 插件未注册

**症状**:
```bash
$ ctr plugins ls | grep mysimple
# 没有输出
```

**原因**: 
- ❌ 没有创建 `builtins/mysimple_linux.go`
- ❌ 没有导入 plugin 包

**解决**:
```bash
# 检查是否导入
grep -r "mysimple/plugin" cmd/containerd/builtins/

# 重新编译
make clean && make
```

---

### 错误 2: 配置不生效

**症状**: 配置的 `root_path` 等没有生效

**原因**:
- ❌ 配置文件路径错误
- ❌ TOML 语法错误

**解决**:
```bash
# 检查配置
sudo cat /etc/containerd/config.toml | grep -A 5 mysimple

# 验证配置
sudo containerd config dump | grep -A 5 mysimple
```

---

### 错误 3: Label 不生效

**症状**: 自定义的 label 没有传递到 snapshotter

**原因**:
- ❌ Label 没有 `containerd.io/snapshot/` 前缀

**解决**:
```bash
# ✅ 正确
containerd.io/snapshot/mysimple.custom-lowers

# ❌ 错误（会被过滤）
mysimple.custom-lowers
```

---

## 📚 参考资源

1. **containerd 插件文档**
   - https://github.com/containerd/containerd/blob/main/docs/PLUGINS.md

2. **Snapshotter 接口定义**
   - `snapshots/snapshotter.go`

3. **现有实现参考**
   - `snapshots/overlay/` - 标准实现
   - `snapshots/devbox/` - 高级实现
   - `snapshots/native/` - 最简单实现

4. **调试工具**
   ```bash
   # 查看所有插件
   ctr plugins ls
   
   # 查看 snapshot 操作
   ctr snapshot --snapshotter mysimple <command>
   
   # 查看日志
   journalctl -u containerd -f
   ```

---

## ✅ 检查清单

开发新 snapshotter 时，确保完成：

- [ ] 创建 `snapshots/<name>/<name>.go`
- [ ] 实现 `NewSnapshotter()` 工厂函数
- [ ] 实现或重写 `Prepare/View/Mounts` 方法
- [ ] 创建 `snapshots/<name>/plugin/plugin.go`
- [ ] 在 `init()` 中调用 `plugin.Register()`
- [ ] 创建 `cmd/containerd/builtins/<name>_linux.go`
- [ ] 导入 plugin 包 (`import _ "..."`)
- [ ] 编译 containerd
- [ ] 验证插件已注册 (`ctr plugins ls`)
- [ ] 配置 containerd (`config.toml`)
- [ ] 测试功能

完成所有步骤后，你就有了一个完整可用的 snapshotter！🎉

