# LVM 命令超时机制实现方案

## 一、进程组（Process Group）详细解释

### 1.1 什么是进程组？

在 Linux 系统中，**进程组（Process Group）**是一组相关进程的集合，它们共享同一个进程组 ID（PGID）。进程组的主要作用是：

1. **统一管理**：可以将信号发送给整个进程组，而不是单个进程
2. **作业控制**：Shell 使用进程组来管理前台和后台作业
3. **父子关系**：子进程默认继承父进程的进程组 ID

### 1.2 为什么需要设置进程组？

**问题场景**：

当执行 `lvcreate` 这样的 LVM 命令时，它可能会：
- 启动多个子进程（例如：`lvcreate` → `dmsetup` → `kernel`）
- 调用其他工具（例如：`wipefs`, `mkfs` 等）
- 创建守护进程或后台任务

**如果不设置进程组**：

```
父进程 (containerd)
  └── lvcreate (PID: 1000, PGID: 1000)
      ├── 子进程1 (PID: 1001, PGID: 1000) ← 继承父进程的 PGID
      └── 子进程2 (PID: 1002, PGID: 1000) ← 继承父进程的 PGID
```

当你对 `lvcreate` (PID 1000) 发送 `SIGTERM` 或 `SIGKILL` 时：
- ✅ `lvcreate` 进程会被终止
- ❌ 但它的子进程可能**不会**被终止（取决于子进程如何处理信号）
- ❌ 子进程可能变成**僵尸进程**或**孤儿进程**

**如果设置了进程组**：

```go
cmd.SysProcAttr = &syscall.SysProcAttr{
    Setpgid: true,  // 让子进程创建新的进程组，PGID = 子进程的 PID
}
```

```
父进程 (containerd)
  └── lvcreate (PID: 1000, PGID: 1000) ← 创建新进程组，PGID = 自己的 PID
      ├── 子进程1 (PID: 1001, PGID: 1000) ← 继承父进程的 PGID
      └── 子进程2 (PID: 1002, PGID: 1000) ← 继承父进程的 PGID
```

当你对进程组发送信号时（使用负数 PID）：
```go
syscall.Kill(-pgid, syscall.SIGTERM)  // -pgid 表示进程组
```

- ✅ `lvcreate` 进程会被终止
- ✅ **所有子进程也会被终止**（因为它们在同一进程组）
- ✅ 不会留下僵尸进程

### 1.3 代码示例对比

**不设置进程组（有问题）**：

```go
cmd := exec.Command("lvcreate", args...)
cmd.Run()  // 如果超时，只 kill 主进程，子进程可能还在运行
```

**设置进程组（正确）**：

```go
cmd := exec.Command("lvcreate", args...)
cmd.SysProcAttr = &syscall.SysProcAttr{
    Setpgid: true,  // 创建新进程组
}
cmd.Run()  // 如果超时，可以 kill 整个进程组
```

### 1.4 实际应用场景

**场景 1：lvcreate 超时**

```bash
# lvcreate 命令执行流程
lvcreate
  ├── 检查 VG 空间
  ├── 调用 dmsetup 创建设备映射
  │   └── dmsetup 可能启动多个内核线程
  └── 调用 wipefs 清理设备
      └── wipefs 可能有自己的子进程
```

如果 `lvcreate` 超时，但只 kill 了 `lvcreate` 进程：
- `dmsetup` 进程可能还在运行
- 设备可能处于不一致状态
- 下次操作可能失败

**场景 2：使用进程组 kill**

```go
// 获取进程组 ID
pgid, _ := syscall.Getpgid(cmd.Process.Pid)

// 向整个进程组发送 SIGTERM
syscall.Kill(-pgid, syscall.SIGTERM)  // 负数表示进程组

// 所有进程（lvcreate, dmsetup, wipefs 等）都会收到信号
```

### 1.5 关键点总结

1. **`Setpgid: true`**：
   - 让子进程创建**新的进程组**
   - 新进程组的 PGID = 子进程的 PID
   - 所有子进程都会继承这个 PGID

2. **`syscall.Kill(-pgid, signal)`**：
   - 负数 PID 表示进程组
   - 会向进程组内**所有进程**发送信号
   - 确保所有相关进程都被终止

3. **为什么重要**：
   - LVM 命令可能启动多个子进程
   - 只 kill 主进程可能留下僵尸进程
   - 进程组确保**完整清理**

---

## 二、代码修改方案

### 2.1 添加必要的导入

在 `snapshots/devbox/lvm/lvm.go` 文件顶部添加：

```go
import (
    "bytes"
    "context"        // 新增：用于超时控制
    "encoding/json"
    "fmt"
    "os"
    "os/exec"
    "path/filepath"
    "strconv"
    "strings"
    "syscall"        // 新增：用于进程组管理
    "time"           // 新增：用于超时时间

    "github.com/pkg/errors"
    "k8s.io/apimachinery/pkg/api/resource"
    "k8s.io/klog/v2"

    apis "github.com/openebs/lvm-localpv/pkg/apis/openebs.io/lvm/v1alpha1"
)
```

### 2.2 添加超时常量

在 `snapshots/devbox/lvm/constants.go` 中添加：

```go
// LVM 命令超时配置
const (
    // CommandTimeout 所有 LVM 命令的统一超时时间
    CommandTimeout = 2 * time.Minute
)
```

### 2.3 修改 RunCommandSplit 函数

将 `snapshots/devbox/lvm/lvm.go` 中的 `RunCommandSplit` 函数替换为：

```go
// RunCommandSplit is a wrapper function to run a command with timeout and receive its
// STDERR and STDOUT streams in separate []byte vars.
func RunCommandSplit(command string, args ...string) ([]byte, []byte, error) {
    // 创建带超时的 context
    ctx, cancel := context.WithTimeout(context.Background(), CommandTimeout)
    defer cancel()

    var cmdStdout bytes.Buffer
    var cmdStderr bytes.Buffer

    // 使用 CommandContext 创建命令（支持超时）
    cmd := exec.CommandContext(ctx, command, args...)
    cmd.Stdout = &cmdStdout
    cmd.Stderr = &cmdStderr
    
    // 设置进程组，确保子进程也被终止
    // Setpgid: true 表示让子进程创建新的进程组
    // 这样在超时时，可以通过进程组 ID 来终止所有相关进程
    cmd.SysProcAttr = &syscall.SysProcAttr{
        Setpgid: true,  // 创建新进程组，PGID = 子进程的 PID
    }

    // 启动命令
    if err := cmd.Start(); err != nil {
        return nil, nil, fmt.Errorf("failed to start command %s: %w", command, err)
    }

    // 等待命令完成
    done := make(chan error, 1)
    go func() {
        done <- cmd.Wait()
    }()

    select {
    case err := <-done:
        // 命令正常完成（成功或失败）
        output := cmdStdout.Bytes()
        error_output := cmdStderr.Bytes()

        if len(error_output) > 0 {
            klog.Warningf("lvm: said into stderr: %s", error_output)
        }

        return output, error_output, err

    case <-ctx.Done():
        // 超时了
        if cmd.Process != nil {
            klog.Warningf("lvm: command %s %v timed out after %v, attempting to kill", 
                command, args, CommandTimeout)

            // 获取进程组 ID
            pgid, err := syscall.Getpgid(cmd.Process.Pid)
            if err != nil {
                klog.Warningf("lvm: failed to get process group ID for PID %d: %v", 
                    cmd.Process.Pid, err)
                // 如果获取进程组失败，直接 kill 进程
                cmd.Process.Signal(syscall.SIGTERM)
            } else {
                // 向整个进程组发送 SIGTERM（负数 PID 表示进程组）
                if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
                    klog.Warningf("lvm: failed to send SIGTERM to process group %d: %v", pgid, err)
                }
            }

            // 等待进程优雅退出（最多 2 秒）
            select {
            case <-done:
                // 进程已经退出
                klog.Infof("lvm: command %s terminated gracefully after SIGTERM", command)
            case <-time.After(2 * time.Second):
                // 2 秒后还没退出，发送 SIGKILL 强制终止
                klog.Warningf("lvm: command %s did not terminate after SIGTERM, sending SIGKILL", command)
                
                if pgid > 0 {
                    // 向整个进程组发送 SIGKILL
                    syscall.Kill(-pgid, syscall.SIGKILL)
                } else {
                    // 如果之前获取进程组失败，直接 kill 进程
                    cmd.Process.Kill()
                }
                
                // 等待进程被杀死
                <-done
            }
        }

        output := cmdStdout.Bytes()
        error_output := cmdStderr.Bytes()

        if len(error_output) > 0 {
            klog.Warningf("lvm: command %s stderr output: %s", command, error_output)
        }

        return output, error_output, fmt.Errorf("command %s timed out after %v", command, CommandTimeout)
    }
}
```

### 2.4 完整代码对比

**修改前**：

```go
func RunCommandSplit(command string, args ...string) ([]byte, []byte, error) {
    var cmdStdout bytes.Buffer
    var cmdStderr bytes.Buffer

    cmd := exec.Command(command, args...)
    cmd.Stdout = &cmdStdout
    cmd.Stderr = &cmdStderr
    err := cmd.Run()

    output := cmdStdout.Bytes()
    error_output := cmdStderr.Bytes()

    if len(error_output) > 0 {
        klog.Warningf("lvm: said into stderr: %s", error_output)
    }

    return output, error_output, err
}
```

**修改后**：

```go
func RunCommandSplit(command string, args ...string) ([]byte, []byte, error) {
    ctx, cancel := context.WithTimeout(context.Background(), CommandTimeout)
    defer cancel()

    var cmdStdout bytes.Buffer
    var cmdStderr bytes.Buffer

    cmd := exec.CommandContext(ctx, command, args...)
    cmd.Stdout = &cmdStdout
    cmd.Stderr = &cmdStderr
    cmd.SysProcAttr = &syscall.SysProcAttr{
        Setpgid: true,
    }

    if err := cmd.Start(); err != nil {
        return nil, nil, fmt.Errorf("failed to start command %s: %w", command, err)
    }

    done := make(chan error, 1)
    go func() {
        done <- cmd.Wait()
    }()

    select {
    case err := <-done:
        output := cmdStdout.Bytes()
        error_output := cmdStderr.Bytes()
        if len(error_output) > 0 {
            klog.Warningf("lvm: said into stderr: %s", error_output)
        }
        return output, error_output, err
    case <-ctx.Done():
        if cmd.Process != nil {
            klog.Warningf("lvm: command %s %v timed out after %v", command, args, CommandTimeout)
            pgid, err := syscall.Getpgid(cmd.Process.Pid)
            if err != nil {
                cmd.Process.Signal(syscall.SIGTERM)
            } else {
                syscall.Kill(-pgid, syscall.SIGTERM)
            }
            select {
            case <-done:
                klog.Infof("lvm: command %s terminated gracefully", command)
            case <-time.After(2 * time.Second):
                klog.Warningf("lvm: command %s did not terminate, sending SIGKILL", command)
                if pgid > 0 {
                    syscall.Kill(-pgid, syscall.SIGKILL)
                } else {
                    cmd.Process.Kill()
                }
                <-done
            }
        }
        output := cmdStdout.Bytes()
        error_output := cmdStderr.Bytes()
        if len(error_output) > 0 {
            klog.Warningf("lvm: command %s stderr: %s", command, error_output)
        }
        return output, error_output, fmt.Errorf("command %s timed out after %v", command, CommandTimeout)
    }
}
```

---

## 三、执行流程说明

### 3.1 正常执行流程

```
1. 创建 context，设置 2 分钟超时
2. 启动命令（lvcreate, lvremove 等）
3. 设置进程组（Setpgid: true）
4. 在 goroutine 中等待命令完成
5. 如果 2 分钟内完成 → 返回结果 ✅
```

### 3.2 超时处理流程

```
1. 命令执行超过 2 分钟
2. context 超时，触发 <-ctx.Done()
3. 获取进程组 ID（pgid）
4. 发送 SIGTERM 给整个进程组
   └── 所有子进程都会收到信号
5. 等待 2 秒，看进程是否退出
6. 如果还没退出 → 发送 SIGKILL 强制终止
7. 返回超时错误 ❌
```

### 3.3 信号发送流程

```
SIGTERM (优雅终止)
  ├── 进程可以捕获信号
  ├── 执行清理工作
  └── 正常退出

如果 2 秒后还没退出
  └── SIGKILL (强制终止)
      ├── 无法捕获
      ├── 立即终止
      └── 可能留下不完整状态
```

---

## 四、关键点总结

### 4.1 进程组设置的作用

1. **`Setpgid: true`**：
   - 让子进程创建新进程组
   - PGID = 子进程的 PID
   - 所有子进程继承这个 PGID

2. **`syscall.Kill(-pgid, signal)`**：
   - 负数 PID 表示进程组
   - 向进程组内所有进程发送信号
   - 确保完整清理

3. **为什么重要**：
   - LVM 命令可能启动多个子进程
   - 只 kill 主进程可能留下僵尸进程
   - 进程组确保所有相关进程都被终止

### 4.2 超时机制

1. **统一超时**：所有 LVM 命令都是 2 分钟
2. **优雅退出**：先 SIGTERM，等待 2 秒
3. **强制终止**：如果还没退出，发送 SIGKILL
4. **完整清理**：通过进程组确保所有子进程都被终止

### 4.3 错误处理

1. **超时错误**：返回明确的超时错误信息
2. **日志记录**：记录超时事件和强制终止事件
3. **stderr 输出**：即使超时也返回 stderr 输出，便于调试

---

## 五、测试建议

1. **正常场景**：验证命令在 2 分钟内完成时正常工作
2. **超时场景**：模拟长时间运行的命令，验证超时机制
3. **子进程清理**：验证所有子进程都被正确终止
4. **错误处理**：验证超时后返回的错误信息

---

## 六、注意事项

1. **向后兼容**：函数签名不变，现有调用代码无需修改
2. **资源清理**：确保超时后所有资源都被正确清理
3. **日志记录**：记录超时事件，便于问题排查
4. **性能影响**：超时机制使用 goroutine，对性能影响很小

