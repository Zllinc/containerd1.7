# MySimple Snapshotter - 快速入门指南

## 📦 已创建的完整学习体系

我已经为你创建了一个**完整可运行的 snapshotter 示例**，包含所有必要的代码、文档和测试：

### ✅ 核心实现（3个关键文件）

```
snapshots/mysimple/
├── mysimple.go                    # 核心实现 (~200 行)
│   ├── type snapshotter struct
│   ├── func NewSnapshotter()
│   └── func Prepare/View/Mounts()
│
├── plugin/
│   └── plugin.go                  # 插件注册 (~100 行)
│       ├── type Config struct
│       └── func init() + plugin.Register()
│
├── README.md                       # 详细使用文档
└── test-mysimple.sh               # 自动化测试脚本 ✅

cmd/containerd/builtins/
└── mysimple_linux.go              # 导入到 containerd
```

### 📚 学习文档（3个教程）

```
docs/
├── BUILD-YOUR-OWN-SNAPSHOTTER.md  # 🎯 主教程（从零构建）
├── snapshotter-framework-guide.md # 🔍 框架深度解析
└── custom-snapshotter-tutorial.md # 📖 开发参考手册
```

---

## 🚀 30秒快速开始

### 步骤 1: 编译 containerd（包含 mysimple）

```bash
cd /root/containerd1.7

# 完整编译
make clean && make

# 或快速编译（仅 containerd）
go build -o bin/containerd ./cmd/containerd
```

### 步骤 2: 验证插件已注册

```bash
# 临时启动 containerd（测试）
sudo ./bin/containerd --config /dev/null &

# 查看插件列表
sudo ./bin/ctr plugins ls | grep mysimple

# 期望输出：
# io.containerd.snapshotter.v1.mysimple    linux/amd64    ok
```

### 步骤 3: 运行自动化测试

```bash
cd /root/containerd1.7/snapshots/mysimple

# 运行完整测试套件
sudo ./test-mysimple.sh

# 测试包括：
# ✓ 插件注册验证
# ✓ 基本 snapshot 功能
# ✓ 自定义 lower 层功能
# ✓ 容器运行验证
# ✓ 写隔离验证
```

---

## 📖 学习路径（按顺序）

### 🎯 路径 1: 快速理解框架（30分钟）

```bash
# 1. 阅读主教程
cat docs/BUILD-YOUR-OWN-SNAPSHOTTER.md

# 2. 理解三个核心文件
cat snapshots/mysimple/mysimple.go          # 核心逻辑
cat snapshots/mysimple/plugin/plugin.go     # 插件注册
cat cmd/containerd/builtins/mysimple_linux.go  # 导入

# 3. 查看流程图
cat docs/snapshotter-framework-guide.md
```

### 🔍 路径 2: 深度学习原理（2小时）

```bash
# 1. 学习 Snapshotter 接口
cat snapshots/snapshotter.go

# 2. 对比现有实现
cat snapshots/overlay/overlay.go     # 标准实现
cat snapshots/devbox/devbox.go       # 高级实现
cat snapshots/mysimple/mysimple.go   # 简化实现

# 3. 理解插件系统
grep -r "plugin.Register" snapshots/*/plugin/plugin.go

# 4. 阅读详细教程
cat docs/custom-snapshotter-tutorial.md
```

### 🛠️ 路径 3: 动手实践（4小时）

**任务 1: 修改 mysimple**
```bash
# 添加一个新功能：支持多个 lower 路径
# 编辑 snapshots/mysimple/mysimple.go
# 修改 customizeMounts() 函数
```

**任务 2: 创建自己的 snapshotter**
```bash
# 复制 mysimple 为模板
cp -r snapshots/mysimple snapshots/mysnap

# 修改包名和功能
sed -i 's/mysimple/mysnap/g' snapshots/mysnap/*.go
sed -i 's/mysimple/mysnap/g' snapshots/mysnap/plugin/*.go

# 创建 builtins 导入
cat > cmd/containerd/builtins/mysnap_linux.go << 'EOF'
//go:build linux
package builtins
import _ "github.com/containerd/containerd/snapshots/mysnap/plugin"
EOF

# 编译测试
make
```

**任务 3: 完全重写（参考 devbox）**
```bash
# 学习 devbox 的完整实现
cat snapshots/devbox/devbox.go
cat snapshots/devbox/storage/bolt.go

# 实现自己的元数据管理
# 实现自己的 mounts() 逻辑
# 添加 LVM 支持（可选）
```

---

## 🎨 核心概念详解

### 概念 1: 三个关键文件的作用

```
┌─────────────────────────────────────────────────────────┐
│ mysimple.go                                              │
│ • 实现 Snapshotter 接口                                  │
│ • 提供 NewSnapshotter() 工厂函数                         │
│ • 实现核心业务逻辑                                       │
└─────────────────────────────────────────────────────────┘
                         ↓ 被调用
┌─────────────────────────────────────────────────────────┐
│ plugin/plugin.go                                         │
│ • init() 函数自动执行                                    │
│ • plugin.Register() 注册插件                            │
│ • InitFn 调用 NewSnapshotter()                          │
└─────────────────────────────────────────────────────────┘
                         ↓ 被导入
┌─────────────────────────────────────────────────────────┐
│ builtins/mysimple_linux.go                               │
│ • import _ "...plugin"                                   │
│ • 触发 plugin.go 的 init() 执行                          │
└─────────────────────────────────────────────────────────┘
```

### 概念 2: 编译时 vs 运行时

**编译时 (Build Time)**:
```bash
make
  → 编译所有 Go 代码
  → builtins/mysimple_linux.go 被包含
  → 生成 bin/containerd（包含 mysimple 代码）
```

**运行时 (Runtime)**:
```bash
containerd 启动
  → 导入 builtins 包
  → 执行所有 init() 函数
  → mysimple 插件注册
  → 读取 config.toml
  → 调用 InitFn 创建实例
  → mysimple 可用
```

### 概念 3: Label 的传递流程

```
用户命令
  ctr run --snapshotter-label containerd.io/snapshot/mysimple.xxx=value

     ↓

CRI Server
  opts = [..., WithLabels({"containerd.io/snapshot/mysimple.xxx": "value"})]

     ↓

mysimple.Prepare(ctx, key, parent, opts)
  1. 调用 overlay.Prepare() → 保存 labels 到元数据
  2. 调用 s.Stat(key) → 读取 labels
  3. 根据 labels 修改 mounts
  4. 返回修改后的 mounts

     ↓

Runtime 挂载文件系统
  mount -t overlay ... -o lowerdir=自定义路径:镜像层...
```

---

## 🔧 实际使用示例

### 示例 1: 基本使用

```bash
# 1. 启动 containerd
sudo systemctl start containerd

# 2. 拉取镜像
sudo ctr images pull docker.io/library/nginx:latest

# 3. 使用 mysimple snapshotter
sudo ctr run \
  --snapshotter mysimple \
  --rm -t \
  docker.io/library/nginx:latest \
  test bash
```

### 示例 2: 使用自定义 lower 层

```bash
# 1. 准备自定义目录
sudo mkdir -p /opt/my-cache
echo "Hello from custom layer" | sudo tee /opt/my-cache/README.txt

# 2. 启动容器（带自定义 lower）
sudo ctr run \
  --snapshotter mysimple \
  --snapshotter-label containerd.io/snapshot/mysimple.custom-lowers=/opt/my-cache \
  --rm -t \
  docker.io/library/nginx:latest \
  test bash

# 3. 在容器内验证
root@container# cat /opt/my-cache/README.txt
# 输出: Hello from custom layer

# 4. 测试写隔离
root@container# echo "Modified" > /opt/my-cache/test.txt
root@container# exit

# 5. 宿主机验证（写隔离）
cat /opt/my-cache/test.txt
# 文件不存在（容器的修改写到了 upperdir）
```

### 示例 3: 配置为默认 snapshotter

```bash
# 编辑配置文件
sudo tee /etc/containerd/config.toml << 'EOF'
version = 2

[plugins."io.containerd.snapshotter.v1.mysimple"]
  root_path = "/var/lib/containerd/io.containerd.snapshotter.v1.mysimple"
  upperdir_label = true

[plugins."io.containerd.grpc.v1.cri".containerd]
  snapshotter = "mysimple"
EOF

# 重启 containerd
sudo systemctl restart containerd

# 现在所有容器默认使用 mysimple
sudo ctr run --rm -t nginx:latest test bash
```

---

## 🐛 常见问题解决

### Q1: 插件未找到

**症状**: `ctr plugins ls | grep mysimple` 没有输出

**解决方案**:
```bash
# 检查是否编译了新版本
./bin/containerd --version

# 检查 builtins 导入
cat cmd/containerd/builtins/mysimple_linux.go

# 重新编译
make clean && make

# 确保使用新编译的 containerd
sudo systemctl stop containerd
sudo cp bin/containerd /usr/local/bin/
sudo systemctl start containerd
```

### Q2: Label 不生效

**问题**: 设置了 label 但没有效果

**原因**: Label 必须以 `containerd.io/snapshot/` 开头

**正确写法**:
```bash
# ✅ 正确
--snapshotter-label containerd.io/snapshot/mysimple.custom-lowers=/path

# ❌ 错误（会被过滤）
--snapshotter-label mysimple.custom-lowers=/path
```

### Q3: 编译错误

**错误**: `package github.com/containerd/containerd/snapshots/mysimple/plugin: unrecognized import path`

**原因**: Go module 缓存问题

**解决**:
```bash
cd /root/containerd1.7
go mod tidy
go clean -modcache
make
```

---

## 📊 与 Devbox 的对比

| 特性 | mysimple | devbox |
|------|----------|--------|
| **代码量** | ~300 行 | ~1500 行 |
| **实现方式** | 包装 overlay | 完全重写 |
| **元数据** | overlay 的 BoltDB | 扩展的 BoltDB |
| **存储** | 目录 | LVM 逻辑卷 |
| **功能** | 添加 lower 层 | LVM + 配额 + 多层 |
| **学习难度** | ⭐ 简单 | ⭐⭐⭐⭐⭐ 复杂 |
| **适用场景** | 学习/简单定制 | 生产环境 |

**建议学习路径**:
1. 先掌握 mysimple（理解框架）
2. 再学习 devbox（高级特性）

---

## ✅ 下一步行动

### 立即开始（5分钟）

```bash
# 1. 编译
cd /root/containerd1.7
make

# 2. 测试
cd snapshots/mysimple
sudo ./test-mysimple.sh

# 3. 查看结果
```

### 深入学习（1小时）

```bash
# 阅读三个核心文件
cat snapshots/mysimple/mysimple.go
cat snapshots/mysimple/plugin/plugin.go  
cat cmd/containerd/builtins/mysimple_linux.go

# 阅读主教程
cat docs/BUILD-YOUR-OWN-SNAPSHOTTER.md
```

### 动手实践（4小时）

```bash
# 创建自己的 snapshotter
cp -r snapshots/mysimple snapshots/mysnap
# 修改代码，添加自己的功能
# 编译测试
```

---

## 📚 学习资源总结

### 必读文档（按优先级）

1. **`docs/BUILD-YOUR-OWN-SNAPSHOTTER.md`** ⭐⭐⭐⭐⭐
   - 完整的开发流程
   - 三个核心文件详解
   - 实战步骤

2. **`snapshots/mysimple/README.md`** ⭐⭐⭐⭐
   - 项目结构
   - 使用说明
   - 配置示例

3. **`docs/snapshotter-framework-guide.md`** ⭐⭐⭐⭐
   - 流程图
   - 原理深度解析
   - 调试技巧

4. **`docs/custom-snapshotter-tutorial.md`** ⭐⭐⭐
   - 接口说明
   - 开发参考
   - 高级话题

### 代码示例（按复杂度）

1. **native** (~300 行) - 最简单
2. **mysimple** (~300 行) - 本教程
3. **overlay** (~800 行) - 标准实现
4. **devbox** (~1500 行) - 高级实现

### 测试工具

```bash
# 自动化测试
snapshots/mysimple/test-mysimple.sh

# 手动测试命令
ctr snapshot --snapshotter mysimple <command>
ctr plugins ls | grep mysimple
journalctl -u containerd -f | grep mysimple
```

---

## 🎉 总结

你现在拥有：

✅ **完整可运行的代码** (mysimple snapshotter)  
✅ **三个核心文件** (实现/注册/导入)  
✅ **详细的文档** (4个教程)  
✅ **自动化测试** (test-mysimple.sh)  
✅ **学习路径** (从简单到复杂)  

**现在开始**:
```bash
cd /root/containerd1.7
make
cd snapshots/mysimple
sudo ./test-mysimple.sh
```

祝学习顺利！🚀

