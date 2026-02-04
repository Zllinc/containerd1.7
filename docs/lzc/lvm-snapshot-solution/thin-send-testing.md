# ThinSend 功能测试指南

**模块**: ThinSend (thin_send_recv.go)  
**测试目的**: 验证 thin_send 集成的正确性和可用性  
**最后更新**: 2026-01-22

---

## 一、测试环境准备

### 1.1 系统要求

- **操作系统**: Linux (内核 >= 3.10，支持 LVM thin pool)
- **LVM 工具**: lvm2 (>= 2.02)
- **thin-send-recv**: LINBIT thin-send-recv 工具
- **权限**: root 或 sudo

### 1.2 安装 thin-send-recv

#### 方法 1: 从源码编译（推荐）

```bash
# 克隆仓库
git clone https://github.com/LINBIT/thin-send-recv.git
cd thin-send-recv

# 安装依赖（Debian/Ubuntu）
sudo apt-get install -y build-essential liblvm2-dev

# 编译
make

# 安装到系统路径
sudo make install

# 验证安装
which thin_send
thin_send --help
```

#### 方法 2: 使用包管理器（如果有）

```bash
# Ubuntu/Debian (如果有打包版本)
sudo apt-get install thin-send-recv

# CentOS/RHEL
sudo yum install thin-send-recv
```

#### 验证安装

```bash
$ thin_send --version
# 或
$ thin_send --help
```

**预期输出**: 显示版本信息或帮助信息（不报错即可）

---

## 二、创建测试环境

### 2.1 创建测试用的 Volume Group

**注意**: 以下操作会创建 loop 设备和 VG，不会影响现有数据。

```bash
# 1. 创建测试用的 loop 设备（模拟块设备）
sudo dd if=/dev/zero of=/tmp/lvm-test.img bs=1G count=20
sudo losetup -f /tmp/lvm-test.img
LOOP_DEVICE=$(sudo losetup -a | grep lvm-test.img | cut -d: -f1)
echo "Loop device: $LOOP_DEVICE"

# 2. 创建 Physical Volume
sudo pvcreate $LOOP_DEVICE

# 3. 创建 Volume Group
sudo vgcreate test-vg $LOOP_DEVICE

# 4. 验证 VG 创建成功
sudo vgs test-vg
```

**预期输出**:
```
  VG      #PV #LV #SN Attr   VSize  VFree
  test-vg   1   0   0 wz--n- 20.00g 20.00g
```

### 2.2 创建 Thin Pool

```bash
# 1. 创建 thin pool（大小 15GB）
sudo lvcreate -L 15G -T test-vg/test-thinpool

# 2. 验证 thin pool 创建
sudo lvs -o lv_name,segtype,lv_size test-vg/test-thinpool
```

**预期输出**:
```
  LV            Type       LSize
  test-thinpool thin-pool  15.00g
```

### 2.3 创建基准 Thin Volume（模拟只读层最上层）

```bash
# 1. 创建基准 thin volume (5GB)
sudo lvcreate -V 5G -T test-vg/test-thinpool -n base-lv

# 2. 格式化为 ext4
sudo mkfs.ext4 /dev/test-vg/base-lv

# 3. 挂载并写入基准数据
sudo mkdir -p /mnt/base-lv
sudo mount /dev/test-vg/base-lv /mnt/base-lv

# 4. 写入一些基准文件（模拟只读层）
sudo bash -c 'echo "Base file 1" > /mnt/base-lv/file1.txt'
sudo bash -c 'echo "Base file 2" > /mnt/base-lv/file2.txt'
sudo mkdir -p /mnt/base-lv/dir1
sudo bash -c 'echo "Base file in dir" > /mnt/base-lv/dir1/file3.txt'

# 5. 卸载
sudo umount /mnt/base-lv

# 6. 验证基准 LV
sudo lvs -o lv_name,segtype,lv_size,pool_lv test-vg/base-lv
```

**预期输出**:
```
  LV      Type LSize Pool          
  base-lv thin 5.00g test-thinpool
```

**重要说明**：
> ⚠️ **卸载（umount）不会删除数据！**
> 
> - LV 是块设备，数据永久存储在 LV 上
> - 卸载只是断开挂载点和 LV 的关联
> - 数据仍然在 `/dev/test-vg/base-lv` 上
> 
> 验证数据还在：
> ```bash
> # 重新挂载，数据依然存在
> sudo mount /dev/test-vg/base-lv /mnt/base-lv
> ls /mnt/base-lv  # file1.txt, file2.txt, dir1 都在
> sudo umount /mnt/base-lv
> ```
> 
> 详细说明：[LV、文件系统、挂载的关系](./lv-filesystem-concepts.md)

### 2.4 创建可写层 Thin Volume（模拟用户写入）

```bash
# 1. 创建可写层 thin volume (5GB)
sudo lvcreate -V 5G -T test-vg/test-thinpool -n writable-lv

# 2. 格式化
sudo mkfs.ext4 /dev/test-vg/writable-lv

# 3. 拷贝基准层数据（模拟 Prepare 阶段）
sudo mkdir -p /mnt/writable-lv
sudo mount /dev/test-vg/writable-lv /mnt/writable-lv
sudo mount /dev/test-vg/base-lv /mnt/base-lv
sudo cp -a /mnt/base-lv/* /mnt/writable-lv/
sudo umount /mnt/base-lv

# 4. 写入用户数据（模拟用户修改）
sudo bash -c 'echo "User file 1" > /mnt/writable-lv/user-file1.txt'
sudo bash -c 'echo "Modified" > /mnt/writable-lv/file1.txt'  # 修改已有文件
sudo rm /mnt/writable-lv/file2.txt  # 删除文件

# 5. 卸载
sudo umount /mnt/writable-lv
```

**说明**：
> 卸载后，writable-lv 上的数据仍然存在（包括从 base-lv 拷贝的数据和用户写入的数据）。
> thin_send 可以直接读取卸载后的 LV，因为它工作在块设备级别，不需要挂载文件系统。

### 2.5 创建快照（模拟 Commit 时刻）

```bash
# 创建 thin snapshot（不指定 size）
sudo lvcreate -s test-vg/writable-lv -n writable-lv-snapshot

# 验证快照类型
sudo lvs -o lv_name,segtype,pool_lv test-vg/writable-lv-snapshot
```

**预期输出**:
```
  LV                    Type Pool          
  writable-lv-snapshot  thin test-thinpool
```

**关键**: `segtype` 应该是 `thin`，表示是 thin snapshot。

---

## 三、测试 ThinSend CLI

### 3.1 使用 CLI 工具生成 diff 流

```bash
# 编译 CLI 工具
cd /root/containerd1.7
go build -o /tmp/devbox-thin-send-recv ./cmd/devbox-thin-send-recv

# 运行 thin_send
sudo /tmp/devbox-thin-send-recv \
  -base /dev/test-vg/base-lv \
  -target /dev/test-vg/writable-lv-snapshot \
  -out /tmp/thin-send.stream

# 检查输出文件
ls -lh /tmp/thin-send.stream
file /tmp/thin-send.stream
```

**预期输出**:
```bash
$ ls -lh /tmp/thin-send.stream
-rw------- 1 root root 1.2M Jan 22 10:30 /tmp/thin-send.stream

$ file /tmp/thin-send.stream
/tmp/thin-send.stream: data
```

**成功标志**:
- 文件存在且大小 > 0
- 文件类型为 `data`（二进制）
- 权限为 `rw-------` (0600)

**关于输出文件大小**:
> ⚠️ **为什么输出文件这么大（可能 100MB+）？**
> 
> 这是正常的！thin_send 输出的是**块级差异**，不是文件级差异：
> - 包含所有 writable-lv 上已分配的块
> - 包含从 base-lv 拷贝的数据（即使内容相同）
> - 包含文件系统元数据（superblock、inode、目录等）
> 
> 详细说明：[Thin_send 块级 Diff 原理](./thin-send-block-level-diff.md)
> 
> **简单理解**：
> ```
> 输出大小 ≈ writable-lv 的实际使用量
> 不是 "用户写入的增量"
> 而是 "所有已使用的块的数据"
> ```

### 3.2 验证 diff 流内容（基础检查）

```bash
# 查看前 100 字节（十六进制）
sudo hexdump -C /tmp/thin-send.stream | head -20

# 检查文件大小是否合理（应该 < 5GB，因为只有部分变化）
du -h /tmp/thin-send.stream
```

**预期**:
- 文件内容是二进制数据（有可见字符和不可见字符混合）
- 文件大小远小于整个 LV 的大小（因为是增量）

---

## 四、测试 ThinSend Go API

### 4.1 创建测试程序

创建文件 `/tmp/test-thin-send.go`:

```go
package main

import (
    "context"
    "fmt"
    "os"
    "time"

    "github.com/containerd/containerd/snapshots/devbox/lvm"
)

func main() {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
    defer cancel()

    baseLV := "/dev/test-vg/base-lv"
    targetLV := "/dev/test-vg/writable-lv-snapshot"
    outputPath := "/tmp/thin-send-go-api.stream"

    fmt.Printf("Running ThinSend...\n")
    fmt.Printf("  Base LV: %s\n", baseLV)
    fmt.Printf("  Target LV: %s\n", targetLV)
    fmt.Printf("  Output: %s\n", outputPath)

    start := time.Now()
    err := lvm.ThinSendToFile(ctx, baseLV, targetLV, outputPath)
    duration := time.Since(start)

    if err != nil {
        fmt.Printf("❌ ThinSend failed: %v\n", err)
        os.Exit(1)
    }

    // 检查输出文件
    stat, err := os.Stat(outputPath)
    if err != nil {
        fmt.Printf("❌ Failed to stat output file: %v\n", err)
        os.Exit(1)
    }

    fmt.Printf("✅ ThinSend succeeded!\n")
    fmt.Printf("  Duration: %v\n", duration)
    fmt.Printf("  Output size: %d bytes (%.2f MB)\n", stat.Size(), float64(stat.Size())/1024/1024)
}
```

### 4.2 运行测试程序

```bash
cd /root/containerd1.7

# 运行测试（需要 sudo 权限访问 /dev）
sudo -E go run /tmp/test-thin-send.go
```

**预期输出**:
```
Running ThinSend...
  Base LV: /dev/test-vg/base-lv
  Target LV: /dev/test-vg/writable-lv-snapshot
  Output: /tmp/thin-send-go-api.stream
✅ ThinSend succeeded!
  Duration: 1.234s
  Output size: 1234567 bytes (1.18 MB)
```

**成功标志**:
- 输出 `✅ ThinSend succeeded!`
- Duration < 10秒（取决于硬件）
- Output size > 0

---

## 五、验证结果正确性

### 5.1 对比两个 diff 流

```bash
# 对比 CLI 和 Go API 的输出
sudo diff /tmp/thin-send.stream /tmp/thin-send-go-api.stream

# 如果相同，没有输出；如果不同，显示差异
```

**预期**: 无输出（两个文件完全相同）

### 5.2 手动验证 thin_send

```bash
# 直接使用 thin_send 命令
sudo thin_send /dev/test-vg/base-lv /dev/test-vg/writable-lv-snapshot > /tmp/thin-send-manual.stream

# 对比
sudo diff /tmp/thin-send.stream /tmp/thin-send-manual.stream
```

**预期**: 无输出（说明封装正确）

---

## 六、错误场景测试

### 6.1 测试错误处理：LV 不存在

```bash
sudo /tmp/devbox-thin-send-recv \
  -base /dev/test-vg/nonexistent-lv \
  -target /dev/test-vg/writable-lv-snapshot \
  -out /tmp/error-test.stream
```

**预期输出**:
```
Error: thin_send failed: ... (包含 "not found" 或类似错误)
```

### 6.2 测试错误处理：不是 thin volume

```bash
# 创建一个 regular LV
sudo lvcreate -L 1G -n regular-lv test-vg

# 尝试对 regular LV 执行 thin_send
sudo /tmp/devbox-thin-send-recv \
  -base /dev/test-vg/regular-lv \
  -target /dev/test-vg/writable-lv-snapshot \
  -out /tmp/error-test.stream
```

**预期输出**:
```
Error: thin_send failed: ... not a thin volume
```

### 6.3 测试错误处理：thin_send 未安装

```bash
# 临时重命名 thin_send
sudo mv /usr/local/bin/thin_send /usr/local/bin/thin_send.bak

# 运行测试
sudo /tmp/devbox-thin-send-recv \
  -base /dev/test-vg/base-lv \
  -target /dev/test-vg/writable-lv-snapshot \
  -out /tmp/error-test.stream

# 恢复
sudo mv /usr/local/bin/thin_send.bak /usr/local/bin/thin_send
```

**预期输出**:
```
Error: thin_send not found in PATH
```

---

## 七、性能测试

### 7.1 测试不同数据量

```bash
# 创建多个测试场景
for size in 100 500 1000 5000; do
    echo "Testing with ${size}MB changes..."
    
    # 清空可写层
    sudo mount /dev/test-vg/writable-lv /mnt/writable-lv
    sudo rm -rf /mnt/writable-lv/*
    sudo cp -a /mnt/base-lv/* /mnt/writable-lv/
    
    # 写入指定大小的数据
    sudo dd if=/dev/urandom of=/mnt/writable-lv/testfile bs=1M count=$size
    sudo umount /mnt/writable-lv
    
    # 重新创建快照
    sudo lvremove -f test-vg/writable-lv-snapshot
    sudo lvcreate -s test-vg/writable-lv -n writable-lv-snapshot
    
    # 测试性能
    time sudo /tmp/devbox-thin-send-recv \
        -base /dev/test-vg/base-lv \
        -target /dev/test-vg/writable-lv-snapshot \
        -out /tmp/thin-send-${size}mb.stream
    
    ls -lh /tmp/thin-send-${size}mb.stream
done
```

**记录结果**:
| 变化大小 | thin_send 时间 | 输出文件大小 |
|---------|---------------|-------------|
| 100MB   | ?s            | ?MB         |
| 500MB   | ?s            | ?MB         |
| 1000MB  | ?s            | ?MB         |
| 5000MB  | ?s            | ?MB         |

---

## 八、清理测试环境

### 8.1 删除测试 LV

```bash
# 删除所有测试 LV
sudo lvremove -f test-vg/writable-lv-snapshot
sudo lvremove -f test-vg/writable-lv
sudo lvremove -f test-vg/base-lv
sudo lvremove -f test-vg/test-thinpool
sudo lvremove -f test-vg/regular-lv  # 如果创建了

# 删除 VG
sudo vgremove -f test-vg

# 删除 PV
sudo pvremove $LOOP_DEVICE

# 删除 loop 设备
sudo losetup -d $LOOP_DEVICE

# 删除测试文件
sudo rm -f /tmp/lvm-test.img
sudo rm -f /tmp/thin-send*.stream
sudo rm -f /tmp/devbox-thin-send-recv
```

### 8.2 删除挂载点

```bash
sudo rmdir /mnt/base-lv /mnt/writable-lv
```

---

## 九、测试检查清单

### 必须通过的测试

- [ ] thin-send-recv 工具已安装
- [ ] 创建 thin pool 成功
- [ ] 创建 thin volume 成功
- [ ] 创建 thin snapshot 成功（segtype=thin）
- [ ] CLI 工具生成 diff 流成功
- [ ] Go API 生成 diff 流成功
- [ ] CLI 和 Go API 输出一致
- [ ] 错误处理正确（LV 不存在、不是 thin volume、工具未安装）

### 可选测试

- [ ] 性能测试（记录不同数据量的执行时间）
- [ ] 并发测试（同时对多个 LV 执行 thin_send）
- [ ] 超时测试（设置短超时时间，验证 Context 生效）

---

## 十、故障排查

### 问题 1: thin_send 命令找不到

**症状**: `thin_send not found in PATH`

**解决**:
```bash
# 检查 thin_send 是否安装
which thin_send

# 如果没有，重新安装（见 1.2 节）
```

### 问题 2: Permission denied

**症状**: `failed to open /dev/test-vg/base-lv: permission denied`

**解决**:
```bash
# 使用 sudo 运行
sudo /tmp/devbox-thin-send-recv ...
```

### 问题 3: 快照不是 thin snapshot

**症状**: `lvs` 显示 segtype 不是 `thin`

**解决**:
```bash
# 检查源 LV 是否是 thin volume
sudo lvs -o lv_name,segtype,pool_lv test-vg/writable-lv

# 如果不是 thin volume，需要重新创建为 thin volume
sudo lvcreate -V 5G -T test-vg/test-thinpool -n writable-lv
```

### 问题 4: thin pool 空间不足

**症状**: `thin_send failed: ... no space left`

**解决**:
```bash
# 检查 thin pool 使用情况
sudo lvs -o lv_name,data_percent test-vg/test-thinpool

# 如果接近 100%，扩展 thin pool
sudo lvextend -L +5G test-vg/test-thinpool
```

---

## 十一、测试报告模板

```markdown
# ThinSend 测试报告

**测试日期**: 2026-01-22  
**测试人**: [Your Name]  
**环境**: Linux x.x.x, LVM 2.02.xxx

## 测试结果

### 功能测试
- [x] CLI 工具测试通过
- [x] Go API 测试通过
- [x] 错误处理测试通过

### 性能测试
- 100MB 变化: 耗时 1.2s
- 500MB 变化: 耗时 3.5s
- 1000MB 变化: 耗时 6.8s

### 问题记录
- 无

### 结论
✅ ThinSend 模块功能正常，可以投入使用。
```

---

**文档版本**: 1.0.0  
**最后更新**: 2026-01-22

