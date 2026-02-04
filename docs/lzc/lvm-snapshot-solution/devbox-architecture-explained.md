# Devbox Snapshotter 架构详解

**创建时间**: 2026-01-22  
**目的**: 解答只读层、LV、OverlayFS 的关系和数据存储位置

---

## 一、问题1：只读层什么时候挂载到 LV？数据在哪里？

### 1.1 核心架构

```
┌─────────────────────────────────────────────────────────────┐
│  容器视图（OverlayFS 合并后）                                │
│  /var/lib/containerd/io.containerd.snapshotter.v1.devbox/   │
│  snapshots/{active-snapshot-id}/merged                       │
│                                                               │
│  包含：所有只读层 + 可写层的合并视图                          │
└─────────────────────────────────────────────────────────────┘
                            ↑
                            │ OverlayFS 挂载
                            │
┌─────────────────────────────────────────────────────────────┐
│  OverlayFS 配置                                              │
│  - lowerdir: 只读层1:只读层2:...:只读层N                     │
│  - upperdir: 可写层（在 LV 上）                              │
│  - workdir: 工作目录（在 LV 上）                             │
└─────────────────────────────────────────────────────────────┘
         ↓                           ↓
    只读层（目录）              可写层（LV 挂载点）
┌──────────────────┐      ┌──────────────────────┐
│ snapshots/       │      │ snapshots/           │
│  {parent1}/fs/   │      │  {active}/fs/        │
│  {parent2}/fs/   │      │  ↓                   │
│  {parentN}/fs/   │      │ /dev/vg/devbox-xxx   │
│                  │      │  (LV 挂载在这里)     │
│ 数据在普通文件系统│      │ 数据在 LV 块设备上   │
└──────────────────┘      └──────────────────────┘
```

### 1.2 详细流程

#### Prepare 阶段（创建 active snapshot）

```go
// 代码位置：devbox.go:857-942

// 1. 创建临时目录
td := os.MkdirTemp(snapshotDir, "new-")
// td = /snapshots/new-xxxxx/

// 2. 创建 LV
lvName := "devbox-" + contentID
lvm.CreateVolume(ctx, vol)  // 创建 thin volume

// 3. 格式化 LV
mkfs.ext4 /dev/vg/devbox-xxx

// 4. 挂载 LV 到临时目录
mount /dev/vg/devbox-xxx /snapshots/new-xxxxx/

// 5. 在 LV 上创建目录结构
mkdir /snapshots/new-xxxxx/fs    # upperdir
mkdir /snapshots/new-xxxxx/work  # workdir

// 6. 卸载并重命名
umount /snapshots/new-xxxxx/
mv /snapshots/new-xxxxx/ /snapshots/{snapshot-id}/

// 7. 重新挂载到最终位置
mount /dev/vg/devbox-xxx /snapshots/{snapshot-id}/
```

**关键点**：
- LV 挂载到 `snapshots/{snapshot-id}/` 目录
- `fs` 和 `work` 目录在 LV 上（不是普通文件系统）
- 只读层**不**挂载到 LV，它们在普通文件系统上

#### 容器运行时（OverlayFS 挂载）

```go
// 代码位置：devbox.go:1105-1159

func (o *Snapshotter) mounts(s storage.Snapshot) []mount.Mount {
    // ...
    options = append(options,
        fmt.Sprintf("workdir=%s", o.workPath(s.ID)),     // {snapshot-id}/work
        fmt.Sprintf("upperdir=%s", o.upperPath(s.ID)),   // {snapshot-id}/fs
    )
    
    parentPaths := make([]string, len(s.ParentIDs))
    for i := range s.ParentIDs {
        parentPaths[i] = o.upperPath(s.ParentIDs[i])  // 只读层的路径
    }
    
    options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(parentPaths, ":")))
    
    return []mount.Mount{
        {
            Type:    "overlay",
            Source:  "overlay",
            Options: options,
        },
    }
}
```

**实际挂载命令**：
```bash
mount -t overlay overlay \
  -o lowerdir=/snapshots/{parent1}/fs:/snapshots/{parent2}/fs,\
     upperdir=/snapshots/{active}/fs,\
     workdir=/snapshots/{active}/work \
  /run/containerd/.../rootfs
```

### 1.3 数据存储位置总结

| 数据类型 | 存储位置 | 说明 |
|---------|---------|------|
| **只读层（镜像层）** | `snapshots/{parent-id}/fs/` | 普通文件系统目录 |
| **可写层（upperdir）** | `snapshots/{active-id}/fs/` → LV 挂载点 | 数据实际在 LV 上 |
| **工作目录（workdir）** | `snapshots/{active-id}/work/` → LV 挂载点 | 数据实际在 LV 上 |
| **容器视图** | OverlayFS 合并后的目录 | 虚拟文件系统，合并上述所有层 |

**答案**：
> ✅ **只读层从不挂载到 LV**
> - 只读层是 containerd 在 pull 镜像时 unpack 到 `snapshots/{id}/fs/` 的普通目录
> - **只有可写层挂载到 LV**
> - 数据在两个地方：
>   - 只读层数据：在普通文件系统（snapshots 目录）
>   - 可写层数据：在 LV 块设备上（通过 `snapshots/{active-id}/` 挂载点访问）

---

## 二、问题2：OverlayFS 的 COW vs 测试中的拷贝

### 2.1 两种拷贝的区别

#### 场景 A：测试中的拷贝（cp -a）

```bash
# 测试流程
sudo mount /dev/vg/base-lv /mnt/base-lv
sudo mount /dev/vg/writable-lv /mnt/writable-lv
sudo cp -a /mnt/base-lv/* /mnt/writable-lv/
```

**特点**：
- **一次性全量拷贝**
- 拷贝所有文件和目录
- 发生时间：Prepare 时（容器启动前）
- 目的：为了测试 thin_send

#### 场景 B：OverlayFS 的 COW（Copy-on-Write）

```bash
# OverlayFS 配置
mount -t overlay overlay \
  -o lowerdir=/lower,upperdir=/upper,workdir=/work \
  /merged

# 修改只读层的文件时
echo "modified" > /merged/file.txt  # file.txt 原本在 lower
```

**特点**：
- **按需复制**（只在修改时复制）
- 只复制被修改的文件
- 发生时间：容器运行时（用户修改文件时）
- 目的：实现 COW 语义

### 2.2 详细对比

| 特性 | 测试中的 cp -a | OverlayFS 的 COW |
|-----|---------------|-----------------|
| **触发时机** | 手动执行 | 自动触发（修改文件时） |
| **复制范围** | 全部文件 | 只复制被修改的文件 |
| **复制时间** | Prepare 时 | 运行时 |
| **目的** | 测试验证 | 实现可写语义 |
| **性能** | 慢（全量复制） | 快（按需复制） |
| **空间占用** | 大（所有文件） | 小（只有变化的文件） |

### 2.3 OverlayFS COW 的工作原理

```
初始状态：
┌─────────────────┐     ┌─────────────────┐
│ lowerdir        │     │ upperdir        │
│ - file1.txt     │     │ (空)            │
│ - file2.txt     │     │                 │
│ - dir1/         │     │                 │
└─────────────────┘     └─────────────────┘
         ↓
┌─────────────────────────┐
│ merged (容器视图)        │
│ - file1.txt (来自 lower)│
│ - file2.txt (来自 lower)│
│ - dir1/     (来自 lower)│
└─────────────────────────┘

用户修改 file1.txt：
┌─────────────────┐     ┌─────────────────┐
│ lowerdir        │     │ upperdir        │
│ - file1.txt     │     │ - file1.txt     │ ← COW 复制
│ - file2.txt     │     │   (修改后的)    │
│ - dir1/         │     │                 │
└─────────────────┘     └─────────────────┘
         ↓
┌─────────────────────────┐
│ merged (容器视图)        │
│ - file1.txt (来自 upper)│ ← 现在读取 upper 的版本
│ - file2.txt (来自 lower)│
│ - dir1/     (来自 lower)│
└─────────────────────────┘
```

**COW 步骤**：
1. 用户在 merged 中修改 file1.txt
2. OverlayFS 检测到 file1.txt 在 lower 中
3. OverlayFS 将 file1.txt 从 lower **复制到 upper**
4. 用户的修改写入 upper 中的副本
5. 后续读取时，OverlayFS 优先读取 upper 的版本

### 2.4 OverlayFS COW 导致的块变化

```
假设场景：
- 容器基于镜像启动（镜像有 1000 个文件）
- 用户只修改了 10 个文件

OverlayFS COW 后：
┌─────────────────────────────────────┐
│ lowerdir (只读层)                   │
│ - 1000 个文件（在普通文件系统）     │
└─────────────────────────────────────┘
┌─────────────────────────────────────┐
│ upperdir (可写层 LV)                │
│ - 10 个文件（从 lower 复制并修改）  │
│ - 文件系统元数据                    │
└─────────────────────────────────────┘

thin_send 输出：
- upperdir LV 的所有块
- 包含：10 个文件 + 元数据
- 大小：几 MB 到几十 MB（取决于文件大小）
```

**与测试场景的对比**：

| 场景 | 可写层内容 | thin_send 输出 |
|------|-----------|---------------|
| **测试（cp -a）** | 1000 个文件 + 元数据 | 148MB（所有文件） |
| **实际（OverlayFS COW）** | 10 个文件 + 元数据 | 几 MB（只有修改的文件） |

### 2.5 为什么测试要用 cp -a？

**测试目的**：
- 模拟"可写层包含只读层数据"的场景
- 验证 thin_send 的正确性
- 确认输出包含所有数据

**实际场景**：
- 容器运行时，可写层只包含用户修改的文件（OverlayFS COW）
- thin_send 输出会小得多
- 更接近"增量 commit"的预期

---

## 三、实际 Commit 流程

### 3.1 当前架构下的 Commit

```
容器运行中：
┌─────────────────────────────────────┐
│ OverlayFS                            │
│ - lowerdir: 只读层（普通目录）       │
│ - upperdir: 可写层（LV 挂载点）      │
└─────────────────────────────────────┘
          ↓ 用户修改文件
┌─────────────────────────────────────┐
│ upperdir (LV)                        │
│ - 只包含被修改的文件（OverlayFS COW）│
│ - 文件系统元数据                     │
│ - 数据量：几 MB 到几十 MB            │
└─────────────────────────────────────┘

Commit 时：
1. 创建可写层 LV 的快照
   lvcreate -s /dev/vg/devbox-xxx -n devbox-xxx-snapshot
   
2. 挂载快照
   mount /dev/vg/devbox-xxx-snapshot /mnt/snapshot
   
3. 对比 snapshot 和只读层最上层
   # 方案 A：文件系统 diff
   rsync -n --itemize-changes /mnt/snapshot/ /mnt/readonly-top/
   
   # 方案 B：块级 diff（我们正在实现）
   thin_send /dev/vg/base-lv /dev/vg/devbox-xxx-snapshot
   
4. 打包变化的文件为镜像层
   tar -czf layer.tar.gz <changed-files>
```

### 3.2 thin_send 在实际场景的输出

**实际场景（OverlayFS COW）**：
```bash
# upperdir (可写层 LV) 只包含修改的文件
thin_send /dev/vg/base-lv /dev/vg/devbox-xxx-snapshot

# 输出大小 = upperdir 的实际使用
# 预期：几 MB 到几十 MB（取决于用户修改的数据量）
```

**测试场景（cp -a）**：
```bash
# upperdir 包含所有文件（全量拷贝）
thin_send /dev/vg/base-lv /dev/vg/devbox-xxx-snapshot

# 输出大小 = upperdir 的实际使用
# 结果：148MB（包含所有拷贝的文件）
```

**结论**：
- 测试中的 148MB 是因为 `cp -a` 全量拷贝
- 实际场景中，thin_send 输出会小得多（只有 OverlayFS COW 的文件）
- **这是好事**：说明实际 commit 的数据量会更小

---

## 四、优化方向（讨论中的方案）

### 当前方案的问题

**问题**：
- 如果 Prepare 时拷贝只读层最上层到可写层 LV
- thin_send 输出 = 所有数据（很大）
- 不是真正的"增量"

**解决方案**（你们讨论的）：
```
方案：不拷贝只读层到可写层
- Prepare 时：可写层 LV 为空
- 运行时：OverlayFS 自动 COW（按需复制）
- Commit 时：thin_send 只输出用户修改的数据
```

**结果**：
- thin_send 输出 = 用户修改的数据（几 MB）
- 真正的增量 commit
- 性能更好，输出更小

---

## 五、总结

### 问题 1 的答案

**只读层什么时候挂载到 LV？**
> ❌ 从不挂载！
> - 只读层是普通目录（containerd unpack 镜像时创建）
> - **只有可写层挂载到 LV**
> - OverlayFS 将它们组合起来

**数据在哪里？**
> - 只读层数据：在 `snapshots/{parent-id}/fs/` 普通目录
> - 可写层数据：在 LV 上（通过 `snapshots/{active-id}/` 访问）
> - 容器看到：OverlayFS 合并后的视图

### 问题 2 的答案

**OverlayFS 的 COW vs 测试的拷贝**

| 特性 | 测试（cp -a） | 实际（OverlayFS COW） |
|------|--------------|---------------------|
| **时机** | 手动，Prepare 时 | 自动，运行时 |
| **范围** | 全部文件 | 只有被修改的文件 |
| **目的** | 测试验证 | 实现可写语义 |
| **输出** | 148MB（所有文件） | 几 MB（只有修改） |

**结论**：
- **不一样**：测试是全量拷贝，实际是按需复制
- **实际更好**：OverlayFS COW 只复制修改的文件，输出更小
- **测试是模拟**：为了验证 thin_send 的正确性

---

**文档版本**: 1.0.0  
**最后更新**: 2026-01-22

