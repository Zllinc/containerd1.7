# LVM 命令超时机制验证方案

## 一、快速验证方法（推荐）

### 方法 1: 使用 sleep 命令模拟阻塞

创建一个简单的测试脚本：

```bash
#!/bin/bash
# 测试超时机制

# 测试 1: 正常命令（应该快速完成）
echo "测试 1: 正常命令"
time echo "hello"

# 测试 2: 阻塞命令（应该超时）
echo "测试 2: 阻塞命令（sleep 130 秒，超过 2 分钟）"
echo "注意: 这个会等待约 2 分钟来验证超时"
# 实际测试时，可以通过 Go 测试来验证
```

### 方法 2: 运行 Go 单元测试

```bash
# 在项目根目录执行
cd /root/containerd1.7

# 运行超时测试（需要等待约 2 分钟）
go test -v -run TestRunCommandSplitTimeout ./snapshots/devbox/lvm/ -timeout 5m

# 或者只运行快速测试
go test -v -run TestRunCommandSplitTimeoutShortTimeout ./snapshots/devbox/lvm/
```

### 方法 3: 使用测试脚本

```bash
# 给脚本执行权限
chmod +x test_lvm_timeout.sh

# 运行测试脚本
./test_lvm_timeout.sh
```

---

## 二、详细验证步骤

### 步骤 1: 验证正常命令不超时

```bash
# 创建一个简单的 Go 测试程序
cat > /tmp/test_timeout.go << 'EOF'
package main

import (
    "fmt"
    "time"
    "github.com/containerd/containerd/snapshots/devbox/lvm"
)

func main() {
    start := time.Now()
    stdout, stderr, err := lvm.RunCommandSplit("echo", "hello")
    duration := time.Since(start)
    
    fmt.Printf("Duration: %v\n", duration)
    fmt.Printf("Output: %s\n", string(stdout))
    fmt.Printf("Error: %v\n", err)
    
    if duration < 5*time.Second {
        fmt.Println("✓ Test passed: Command completed quickly")
    } else {
        fmt.Println("✗ Test failed: Command took too long")
    }
}
EOF

# 编译并运行（需要设置正确的 import 路径）
```

### 步骤 2: 验证超时机制

```bash
# 创建一个会阻塞的命令
cat > /tmp/test_blocking.sh << 'EOF'
#!/bin/bash
# 这个脚本会阻塞超过 2 分钟
sleep 130
echo "This should not be printed if timeout works"
EOF

chmod +x /tmp/test_blocking.sh

# 通过 Go 测试来验证
go test -v -run TestRunCommandSplitTimeout ./snapshots/devbox/lvm/
```

### 步骤 3: 验证进程组清理

```bash
# 创建一个会启动子进程的脚本
cat > /tmp/test_children.sh << 'EOF'
#!/bin/bash
# 启动子进程
(sleep 200) &
CHILD_PID=$!
echo "Child PID: $CHILD_PID"
# 主进程也阻塞
sleep 200
wait $CHILD_PID
EOF

chmod +x /tmp/test_children.sh

# 运行测试，检查进程是否被正确清理
go test -v -run TestRunCommandSplitTimeout ./snapshots/devbox/lvm/
```

---

## 三、实际场景测试

### 测试真实的 lvcreate 命令（需要 LVM 环境）

```bash
# 1. 创建一个会阻塞的 lvcreate 命令
# 可以通过创建非常大的卷来模拟阻塞
# 或者使用 iowait 来模拟 IO 阻塞

# 2. 监控进程
watch -n 1 'ps aux | grep lvcreate'

# 3. 在另一个终端触发超时
# 通过 containerd 调用 lvcreate，观察是否在 2 分钟后被终止
```

### 模拟 IO 阻塞

```bash
# 创建一个会阻塞 IO 的脚本
cat > /tmp/test_io_block.sh << 'EOF'
#!/bin/bash
# 模拟 IO 阻塞：尝试写入一个被挂起的设备
while true; do
    dd if=/dev/zero of=/tmp/test_block bs=1M count=100 2>/dev/null
    sleep 1
done
EOF

chmod +x /tmp/test_io_block.sh
```

---

## 四、验证检查清单

### ✅ 功能验证

- [ ] 正常命令（< 2 分钟）能正常完成
- [ ] 阻塞命令（> 2 分钟）会在 2 分钟后超时
- [ ] 超时后返回明确的超时错误
- [ ] 超时后进程被正确终止
- [ ] 子进程也被正确终止（进程组清理）

### ✅ 日志验证

检查日志中应该看到：

```
lvm: command sleep [130] timed out after 2m0s, attempting to kill
lvm: command sleep terminated gracefully after SIGTERM
# 或者
lvm: command sleep did not terminate after SIGTERM, sending SIGKILL
```

### ✅ 进程验证

```bash
# 在超时期间，检查进程是否被清理
ps aux | grep sleep
# 应该看不到相关进程

# 检查进程组
ps -eo pid,pgid,comm | grep sleep
# 应该看不到相关进程组
```

---

## 五、快速测试命令

### 最简单的验证方法

```bash
# 1. 运行单元测试（最快）
cd /root/containerd1.7
go test -v -run TestRunCommandSplitTimeout ./snapshots/devbox/lvm/ -timeout 5m

# 2. 如果测试通过，你会看到：
# ✓ Command completed in < 5s (expected < 2m0s)
# ✓ Command timed out after ~2m0s (expected ~2m0s)
# ✓ Command with children timed out after ~2m0s
```

### 手动验证（不需要 Go 环境）

```bash
# 1. 创建一个会阻塞的脚本
cat > /tmp/test_timeout.sh << 'EOF'
#!/bin/bash
echo "Starting..."
sleep 130
echo "This should not print if timeout works"
EOF

chmod +x /tmp/test_timeout.sh

# 2. 使用 timeout 命令模拟（验证超时概念）
timeout 2m /tmp/test_timeout.sh
echo "Exit code: $?"

# 3. 检查进程是否被清理
ps aux | grep test_timeout.sh
```

---

## 六、预期结果

### 成功场景

1. **正常命令**：
   - 执行时间 < 5 秒
   - 返回正常输出
   - 无错误

2. **超时命令**：
   - 执行时间 ≈ 2 分钟（CommandTimeout）
   - 返回超时错误
   - 进程被终止
   - 日志显示超时信息

3. **带子进程的命令**：
   - 执行时间 ≈ 2 分钟
   - 主进程和子进程都被终止
   - 无僵尸进程

### 失败场景（需要修复）

1. 命令执行超过 2 分钟但未超时
2. 超时后进程仍在运行
3. 子进程未被清理（僵尸进程）
4. 没有返回超时错误

---

## 七、调试技巧

### 查看详细日志

```bash
# 设置 klog 级别
export KLOG_V=4

# 运行测试
go test -v -run TestRunCommandSplitTimeout ./snapshots/devbox/lvm/
```

### 监控进程

```bash
# 在另一个终端监控进程
watch -n 1 'ps aux | grep -E "sleep|lvcreate" | grep -v grep'
```

### 检查进程组

```bash
# 查看进程组信息
ps -eo pid,pgid,comm,args | grep -E "sleep|lvcreate"
```

---

## 八、注意事项

1. **测试时间**：超时测试需要等待约 2 分钟，请耐心等待
2. **环境要求**：某些测试需要 root 权限或 LVM 环境
3. **资源清理**：测试后确保所有进程都被清理
4. **日志级别**：设置合适的日志级别以便观察超时行为

