# ThinSend 模块实现文档

**模块路径**: `snapshots/devbox/lvm/thin_send_recv.go`  
**创建时间**: 2026-01-22  
**最后更新**: 2026-01-22  
**维护状态**: ✅ Active

---

## 一、模块概述

### 目的

ThinSend 模块是基于 LVM thin pool 的块级差异检测工具的 Go 语言封装。它通过调用 LINBIT 的 `thin_send` 命令，实现两个 thin volume/snapshot 之间的增量数据提取。

### 核心功能

1. **ThinSend**: 将块级差异流式输出到 `io.Writer`
2. **ThinSendToFile**: 将块级差异直接写入文件

### 设计原则

- **不侵入 Snapshotter**: 独立模块，不修改现有 Snapshotter 代码
- **thin-send-recv 集成**: 直接使用成熟的开源工具，不重复造轮子
- **流式处理**: 支持大数据量场景，避免内存溢出

---

## 二、实现思路详解

### 2.1 整体架构

```
┌─────────────────────────────────────────────┐
│  调用方（commit 流程）                       │
└─────────────────────────────────────────────┘
                  │
                  │ 调用 ThinSend/ThinSendToFile
                  ↓
┌─────────────────────────────────────────────┐
│  thin_send_recv.go (Go 封装层)              │
│  - 参数校验                                  │
│  - thin_send 工具检测                       │
│  - 命令执行与错误处理                        │
└─────────────────────────────────────────────┘
                  │
                  │ exec.CommandContext
                  ↓
┌─────────────────────────────────────────────┐
│  thin_send (外部工具)                        │
│  - 读取 thin pool COW 表                    │
│  - 计算块级差异                              │
│  - 输出增量数据流                            │
└─────────────────────────────────────────────┘
                  │
                  ↓
┌─────────────────────────────────────────────┐
│  输出（io.Writer 或文件）                    │
│  - 二进制数据流                              │
│  - 包含变化的块信息                          │
└─────────────────────────────────────────────┘
```

### 2.2 核心函数：ThinSend

#### 函数签名

```go
func ThinSend(ctx context.Context, baseLV, targetLV string, out io.Writer) error
```

#### 参数说明

- **ctx**: 上下文，用于控制命令超时和取消
- **baseLV**: 基准 LV 路径（如 `/dev/vg/devbox-xxx-base`）
- **targetLV**: 目标 LV 路径（如 `/dev/vg/devbox-xxx-snapshot`）
- **out**: 输出流，接收 thin_send 的二进制数据

#### 实现步骤

**步骤 1: 参数校验**

```go
if baseLV == "" || targetLV == "" {
    return fmt.Errorf("baseLV and targetLV must be non-empty")
}
if out == nil {
    return fmt.Errorf("output writer must be non-nil")
}
```

**目的**：
- 避免空参数导致的命令执行失败
- 提前发现调用方错误，提高可调试性

**步骤 2: 工具可用性检测**

```go
if _, err := exec.LookPath("thin_send"); err != nil {
    return fmt.Errorf("thin_send not found in PATH: %w", err)
}
```

**目的**：
- 确保 `thin_send` 工具已安装
- 提供清晰的错误信息（而不是模糊的 "command not found"）

**步骤 3: 构造并执行命令**

```go
cmd := exec.CommandContext(ctx, "thin_send", baseLV, targetLV)
var stderr bytes.Buffer
cmd.Stdout = out
cmd.Stderr = &stderr
```

**设计要点**：
- 使用 `CommandContext` 支持超时和取消
- `Stdout` 直接连接到 `out`，实现流式输出（不占用内存）
- `Stderr` 缓存到 `buffer`，用于错误诊断

**步骤 4: 错误处理**

```go
if err := cmd.Run(); err != nil {
    errMsg := stderr.String()
    if errMsg != "" {
        return fmt.Errorf("thin_send failed: %w: %s", err, errMsg)
    }
    return fmt.Errorf("thin_send failed: %w", err)
}
```

**目的**：
- 捕获 `thin_send` 的 stderr 输出，方便调试
- 包装错误，提供完整的上下文信息

### 2.3 辅助函数：ThinSendToFile

#### 函数签名

```go
func ThinSendToFile(ctx context.Context, baseLV, targetLV, outputPath string) error
```

#### 实现要点

**文件创建**：
```go
outFile, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
```

**权限选择**：
- `0600` (rw-------): 只有所有者可读写
- 原因：thin_send 输出可能包含敏感数据（文件系统块内容）

**文件同步**：
```go
if err := outFile.Sync(); err != nil {
    return fmt.Errorf("failed to sync output file: %w", err)
}
```

**目的**：
- 确保数据完全写入磁盘
- 避免系统崩溃导致的数据丢失

---

## 三、技术细节

### 3.1 为什么使用 thin_send？

**优势**：
1. **成熟稳定**: LINBIT 开源项目，生产级质量
2. **块级精确**: 直接读取 thin pool 的 COW 表，100% 准确
3. **高性能**: C 语言实现，比 Go 遍历文件系统快得多

**劣势**：
1. **外部依赖**: 需要安装 thin-send-recv 工具
2. **二进制输出**: 需要额外解析才能转换为文件列表

### 3.2 流式处理的重要性

**场景**：
- 可写层 LV 可能有 100GB+
- thin_send 输出可能有几个 GB

**如果不用流式处理**：
```go
// ❌ 错误示例
output, err := cmd.Output()  // 全部加载到内存
```

**问题**：
- 内存占用 = 输出大小（可能几 GB）
- 容易 OOM (Out of Memory)

**流式处理方案**：
```go
// ✅ 正确示例
cmd.Stdout = out  // 直接写入 io.Writer
```

**好处**：
- 内存占用 ≈ 内核缓冲区大小（几 KB）
- 可以处理任意大小的数据

### 3.3 Context 的作用

**超时控制**：
```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
defer cancel()
err := ThinSend(ctx, baseLV, targetLV, out)
```

**取消操作**：
```go
ctx, cancel := context.WithCancel(context.Background())
go func() {
    <-stopSignal
    cancel()  // 用户取消操作
}()
err := ThinSend(ctx, baseLV, targetLV, out)
```

---

## 四、使用示例

### 4.1 基本用法

```go
package main

import (
    "context"
    "os"
    "github.com/containerd/containerd/snapshots/devbox/lvm"
)

func main() {
    ctx := context.Background()
    baseLV := "/dev/devbox-vg/devbox-xxx-base"
    targetLV := "/dev/devbox-vg/devbox-xxx-snapshot"
    
    // 输出到文件
    err := lvm.ThinSendToFile(ctx, baseLV, targetLV, "/tmp/thin-send.stream")
    if err != nil {
        panic(err)
    }
}
```

### 4.2 带超时控制

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
defer cancel()

err := lvm.ThinSendToFile(ctx, baseLV, targetLV, "/tmp/thin-send.stream")
```

### 4.3 流式处理到管道

```go
reader, writer := io.Pipe()

go func() {
    defer writer.Close()
    err := lvm.ThinSend(ctx, baseLV, targetLV, writer)
    if err != nil {
        writer.CloseWithError(err)
    }
}()

// 在另一个 goroutine 中处理数据
processStream(reader)
```

---

## 五、错误处理

### 5.1 常见错误

| 错误信息 | 原因 | 解决方案 |
|---------|------|---------|
| `thin_send not found in PATH` | 未安装 thin-send-recv | 安装工具 |
| `baseLV and targetLV must be non-empty` | 参数为空 | 检查调用代码 |
| `failed to create output file` | 权限不足或路径不存在 | 检查文件路径和权限 |
| `thin_send failed: ... not a thin volume` | LV 不是 thin volume | 确认 LV 类型 |

### 5.2 调试建议

**查看 thin_send 原始输出**：
```bash
thin_send /dev/vg/base /dev/vg/target > /tmp/debug.stream
hexdump -C /tmp/debug.stream | head
```

**检查 LV 类型**：
```bash
lvs -o lv_name,segtype /dev/vg/base
# 应该显示 segtype = thin
```

---

## 六、性能考虑

### 6.1 性能基准

**测试场景**：
- 基准 LV: 10GB (ext4)
- 可写层变化: 500MB
- 硬件: SSD

**预期性能**：
- thin_send 执行时间: 1-3 秒
- 内存占用: < 10MB
- CPU 占用: 单核 20-40%

### 6.2 优化建议

1. **并行处理**: thin_send 本身是单线程，但可以并行处理多个 LV
2. **缓存基准 LV**: 如果基准 LV 不变，可以复用
3. **监控磁盘 I/O**: thin_send 是 I/O 密集型操作

---

## 七、未来优化方向

### 7.1 短期优化

- [ ] 添加进度回调（解析 thin_send 的进度输出）
- [ ] 支持压缩输出（pipe 到 gzip）
- [ ] 添加单元测试（mock thin_send）

### 7.2 长期优化

- [ ] 实现纯 Go 的 thin pool COW 表读取（去除外部依赖）
- [ ] 支持增量传输（类似 rsync）
- [ ] 集成到 Snapshotter 的 Commit 流程

---

## 八、维护记录

### 2026-01-22 (v1.0.0)

- **创建**: 初始实现
- **功能**: ThinSend, ThinSendToFile
- **测试**: 手动测试通过（见测试文档）

### 待办事项

- [ ] 添加集成测试
- [ ] 编写使用文档
- [ ] 性能基准测试

---

## 九、相关文档

- [thin-send-recv 分析](./thin-send-recv-analysis.md)
- [thin-send-recv 使用分析](./thin-send-recv-usage-analysis.md)
- [ThinSend 测试文档](./thin-send-testing.md)
- [Thin_send 块级 Diff 原理](./thin-send-block-level-diff.md)

---

## 十、FAQ

**Q: 为什么不直接用 Go 实现块级 diff？**  
A: thin_send 是成熟的生产级工具，重新实现需要理解 LVM thin pool 的内部结构，风险高且耗时。当前阶段先验证可行性，未来可考虑纯 Go 实现。

**Q: thin_send 输出的格式是什么？**  
A: 二进制格式，包含块地址和块数据。具体格式需要查看 thin-send-recv 源码或文档。

**Q: 如果 baseLV 和 targetLV 在不同的 thin pool？**  
A: thin_send 要求两个 LV 在同一个 thin pool 中，否则会报错。

**Q: 支持 regular snapshot 吗？**  
A: 不支持。thin_send 只支持 thin volume 和 thin snapshot。

---

**文档版本**: 1.0.0  
**作者**: AI Assistant  
**审核**: Pending

