# Devbox Snapshotter 僵尸 LV 问题分析与解决方案

## 问题概述

在生产环境中，devbox snapshotter 在处理容器删除和清理时，出现大量重复的 `lstat` 错误日志，以及 LVM 逻辑卷（LV）无法被正常清理的问题，导致系统中存在"僵尸 LV"。

## 问题现象

### 1. 日志表现

系统日志中出现大量重复的错误，每 0.5 秒左右重复一次：

```
E1229 18:47:59.812 lvm.go:750] failed to resolve device mapper from lv path 
/dev/devbox-vg/devbox-561c5e7d-e071-48c3-853b-618bf07b6e62: 
lstat /dev/devbox-vg/devbox-561c5e7d-e071-48c3-853b-618bf07b6e62: no such file or directory

W1229 18:47:59.812 lvm.go:917] failed to get device name for LV 
devbox-561c5e7d-e071-48c3-853b-618bf07b6e62, skipping: 
lstat /dev/devbox-vg/devbox-561c5e7d-e071-48c3-853b-618bf07b6e62: no such file or directory
```

### 2. LV 状态异常

问题 LV 的状态特征：

```bash
# 问题 LV（僵尸 LV）
lvs | grep devbox-561c5e7d-e071-48c3-853b-618bf07b6e62
devbox-561c5e7d-e071-48c3-853b-618bf07b6e62  devbox-vg  Vwi-a-tz--  10.00g  devbox-vg-thinpool  0.00
                                                                      ^^^^^
                                                                      active 但没有 open

# 正常 LV
devbox-00147c97-48a3-45d4-98fa-c61a57f507d3  devbox-vg  Vwi-aotz--  10.00g  devbox-vg-thinpool  4.64
                                                                      ^^^^^^
                                                                      active 且 open
```

**关键差异：**
- 问题 LV：`Vwi-a-tz--` → active but NOT open（活跃但未打开/使用）
- 正常 LV：`Vwi-aotz--` → active AND open（活跃且已打开/使用）

### 3. 设备节点缺失

```bash
# LVM 元数据中存在
lvs devbox-vg/devbox-561c5e7d-e071-48c3-853b-618bf07b6e62  # 能查到

# 但设备节点不存在
ls -l /dev/devbox-vg/devbox-561c5e7d-e071-48c3-853b-618bf07b6e62  # 不存在
ls -l /dev/mapper/devbox--vg-devbox--561c5e7d...  # 也不存在
```

## 根本原因分析

### 1. LV 创建过程中断

LV 创建是一个多步骤过程，可能在任何阶段被中断：

```
lvcreate 执行流程：
1. 更新 LVM 元数据（写入磁盘）           ✓ 完成
2. 输出 "Logical volume created"        ✓ 完成
3. 通知内核创建 device-mapper 设备      ✗ 可能中断
4. 触发 udev 事件                       ✗ 未执行
5. udev 创建 /dev 下的符号链接          ✗ 未执行
6. 命令返回                             ✗ 进程被 kill

结果：LVM 元数据已创建，但设备节点未完成
```

**中断原因：**
- **Context timeout**：`lvcreate` 执行时间过长，超过 context 设置的超时时间
- **OOM Killer**：系统内存不足，进程被杀死
- **系统信号**：收到 SIGTERM/SIGKILL
- **LVM hang**：thin pool 满了、IO 慢等导致 LVM 操作卡住

相关日志证据：
```
E1227 20:11:58.762 lvm.go:308] lvm: could not create volume devbox-vg/devbox-667a27fc... 
error: Logical volume "devbox-667a27fc..." created.
                                           ^^^^^^^ 输出显示已创建

time="2025-12-27T20:12:08.351" level=error msg="CreateContainer failed" 
error="...failed to create LVM logical volume devbox-667a27fc...: 
Logical volume created.\n - signal: killed"
                             ^^^^^^^^^^^^^^^ 但收到 kill 信号
```

### 2. CheckVolumeExists 设计缺陷

`CheckVolumeExists` 函数只检查设备节点，不检查 LVM 元数据：

```go
// lvm.go:401-413
func CheckVolumeExists(ctx context.Context, vol *apis.LVMVolume) (bool, error) {
    devPath, err := GetVolumeDevPath(vol)
    if err != nil {
        return false, err
    }
    // 问题：只用 os.Stat 检查设备节点
    if _, err = os.Stat(devPath); err != nil {
        if os.IsNotExist(err) {
            return false, nil  // ← 设备节点不存在就认为 LV 不存在
        }
        return false, err
    }
    return true, nil
}
```

**问题：**
- 设备节点不存在 ≠ LV 不存在
- LVM 元数据可能已存在，但设备节点未创建
- 导致清理逻辑误判

### 3. DestroyVolume 清理失败

当 `CreateVolume` 失败时，会调用 `DestroyVolume` 清理，但清理逻辑存在缺陷：

```go
// lvm.go:363-398
func DestroyVolume(ctx context.Context, vol *apis.LVMVolume) error {
    // ...
    volExists, err := CheckVolumeExists(ctx, vol)  // ← 检查设备节点
    if err != nil {
        return err
    }
    if !volExists {
        klog.Infof("lvm: volume doesn't exists, skipping its deletion")
        return nil  // ← 直接返回，不删除 LVM 元数据！
    }
    
    err = removeVolumeFilesystem(vol)  // ← 也依赖设备节点
    if err != nil {
        return err  // ← 清理文件系统失败会阻止删除
    }
    
    // lvremove 实际上从未被执行
    args := buildLVMDestroyArgs(vol)
    out, _, err := RunCommandSplit(ctx, LVRemove, args...)
    // ...
}
```

**完整失败场景：**

```
T1: lvcreate 执行
    - LVM 元数据写入 ✓
    - 输出 "created" ✓
    - 进程被 kill ✗
    - 设备节点未创建 ✗

T2: CreateVolume 返回 err
    - 调用 DestroyVolume 清理

T3: DestroyVolume 执行
    - 调用 CheckVolumeExists(vol)
    - CheckVolumeExists 检查 /dev/devbox-vg/xxx
    - os.Stat() 返回 "no such file"
    - 返回 volExists = false

T4: DestroyVolume 跳过删除
    - 认为 LV 不存在
    - return nil (认为成功)
    - 但 LVM 元数据还在！

T5: 僵尸 LV 产生
    - lvs 能看到它（元数据存在）
    - /dev 下没有设备（节点缺失）
    - 永远无法被自动清理
```

### 4. 并发清理导致日志重复

`getCleanupLvNames` 被多个地方调用：

1. **`Remove()`** - 每次删除 snapshot 时（devbox.go:443）
2. **`Cleanup()`** - GC 触发时（devbox.go:508）
3. **多个容器并发删除** - 每个都独立调用

每次调用都会：
```go
// devbox.go:550-559
func (o *Snapshotter) getCleanupLvNames(ctx context.Context) ([]string, error) {
    nameMap, err := storage.GetDevboxLvNames(ctx)  // 从 DB 获取应该存在的 LV
    lvs, err := lvm.ListLVMLogicalVolumeByVG(...)  // 查询实际的 LV
    // 这里会对每个 LV 调用 getLvDeviceName → lstat
    // 僵尸 LV 每次都会失败并打印日志
}
```

**日志重复的原因：**
- 不是重试，而是多个独立的调用源在并发执行
- 每次调用都看到僵尸 LV
- 每次都尝试 lstat 并失败
- 没有去重或缓存机制

### 5. 数据库事务与物理操作的时序差

```go
// devbox.go:404-449
defer func() {
    if err == nil {
        // 物理删除在 defer 中，事务外执行
        for _, lvName := range removedLvNames {
            err := o.removeLv(ctx, lvName)
        }
    }
}()

return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
    // 在事务内获取要删除的 LV 列表
    removedLvNames, err = o.getCleanupLvNames(ctx)
    // ...
})
```

**并发竞态：**
```
进程 A                          进程 B
事务开始
查询 lvs（看到僵尸 LV）
事务提交                        事务开始
                               查询 lvs（也看到僵尸 LV）
defer: 尝试删除 LV              
lstat 失败                      defer: 尝试删除 LV
                               lstat 失败
```

## 解决方案

### 方案 1：改进 CheckVolumeExists（推荐）

**问题：** 只检查设备节点，不检查 LVM 元数据

**修复：** 使用 `lvs` 命令检查 LVM 元数据

```go
func CheckVolumeExists(ctx context.Context, vol *apis.LVMVolume) (bool, error) {
    volume := vol.Spec.VolGroup + "/" + vol.Name
    
    // 使用 lvs 命令检查 LVM 元数据，而不是检查设备节点
    args := []string{
        volume,
        "--noheadings",
        "-o", "lv_name",
    }
    out, _, err := RunCommandSplit(ctx, "lvs", args...)
    if err != nil {
        // lvs 失败说明 LV 不存在（或 VG 不存在）
        return false, nil
    }
    
    // 检查输出是否包含 LV 名称
    lvName := strings.TrimSpace(string(out))
    return lvName == vol.Name, nil
}
```

### 方案 2：DestroyVolume 强制删除（推荐）

**问题：** 设备节点不存在时跳过删除，文件系统清理失败会阻止删除

**修复：** 即使设备不可访问也强制删除元数据

```go
func DestroyVolume(ctx context.Context, vol *apis.LVMVolume) error {
    if vol.Spec.VolGroup == "" {
        klog.Infof("volGroup not set for lvm volume %v, skipping its deletion", vol.Name)
        return nil
    }

    volume := vol.Spec.VolGroup + "/" + vol.Name

    // 使用 lvs 检查 LVM 元数据是否存在
    volExists, err := checkLVMMetadataExists(ctx, vol)
    if err != nil {
        klog.Warningf("failed to check LVM metadata for %s: %v, attempting deletion anyway", volume, err)
    }
    if !volExists {
        klog.Infof("lvm: volume (%s) doesn't exist in LVM metadata", volume)
        return nil
    }

    // 尝试清理文件系统（可能失败，但不要阻止删除）
    err = removeVolumeFilesystem(vol)
    if err != nil {
        klog.Warningf("lvm: failed to remove filesystem for %s: %v, continuing with LV deletion", volume, err)
        // 不要 return，继续删除
    }

    // 使用 lvremove -f 强制删除，即使设备不可访问
    args := []string{"-f", DevPath + volume}
    out, _, err := RunCommandSplit(ctx, LVRemove, args...)

    if err != nil {
        klog.Errorf("lvm: could not destroy volume %v cmd %v error: %s", volume, args, string(out))
        return err
    }

    klog.Infof("lvm: destroyed volume %s", volume)
    return nil
}

// 新增辅助函数
func checkLVMMetadataExists(ctx context.Context, vol *apis.LVMVolume) (bool, error) {
    volume := vol.Spec.VolGroup + "/" + vol.Name
    args := []string{volume, "--noheadings", "-o", "lv_name"}
    out, _, err := RunCommandSplit(ctx, "lvs", args...)
    if err != nil {
        return false, nil
    }
    return strings.TrimSpace(string(out)) == vol.Name, nil
}
```

### 方案 3：改进 getLvDeviceName 容错性

**问题：** lstat 失败会打印错误日志，但这对僵尸 LV 是预期行为

**修复：** 降低日志级别，区分正常失败和异常失败

```go
func getLvDeviceName(path string) (string, error) {
    // 先检查符号链接是否存在
    if _, err := os.Lstat(path); err != nil {
        if os.IsNotExist(err) {
            // 设备节点不存在是预期的（僵尸 LV），不是错误
            return "", nil
        }
        klog.Errorf("failed to stat lv path %v: %v", path, err)
        return "", err
    }
    
    dmPath, err := filepath.EvalSymlinks(path)
    if err != nil {
        if os.IsNotExist(err) {
            // 符号链接目标不存在
            return "", nil
        }
        klog.Errorf("failed to resolve device mapper from lv path %v: %v", path, err)
        return "", err
    }
    
    _, file := filepath.Split(dmPath)
    if file == "" {
        return "", fmt.Errorf("invalid device path: %s", dmPath)
    }
    return file, nil
}
```

### 方案 4：在 getCleanupLvNames 中过滤僵尸 LV

**问题：** 僵尸 LV 每次都被发现并尝试处理，但总是失败

**修复：** 识别僵尸 LV 并特殊处理

```go
func (o *Snapshotter) getCleanupLvNames(ctx context.Context) ([]string, error) {
    nameMap, err := storage.GetDevboxLvNames(ctx)
    if err != nil {
        return nil, err
    }

    lvs, err := lvm.ListLVMLogicalVolumeByVG(ctx, o.lvmVgName, o.ThinPoolName)
    if err != nil {
        return nil, fmt.Errorf("failed to list LVM logical volumes: %w", err)
    }

    cleanup := []string{}
    for _, d := range lvs {
        if _, ok := nameMap[d.Name]; ok {
            continue
        }

        if strings.HasPrefix(d.Name, "devbox") {
            // 检查是否是僵尸 LV
            devPath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, d.Name)
            if _, err := os.Stat(devPath); err != nil && os.IsNotExist(err) {
                // 僵尸 LV：元数据存在但设备节点不存在
                log.G(ctx).Warnf("Found zombie LV %s (no device node), will be force removed", d.Name)
                // 仍然加入清理列表，但标记为僵尸（或直接删除）
            }
            cleanup = append(cleanup, d.Name)
        }
    }

    return cleanup, nil
}
```

### 方案 5：CreateVolume 中的智能清理

**问题：** 创建失败时的清理逻辑不够健壮

**修复：** 识别"created but killed"场景并强制清理

```go
func CreateVolume(ctx context.Context, vol *apis.LVMVolume) error {
    volume := vol.Spec.VolGroup + "/" + vol.Name

    volExists, err := CheckVolumeExists(ctx, vol)
    if err != nil {
        return err
    }
    if volExists {
        klog.Infof("lvm: volume (%s) already exists, skipping its creation", volume)
        err := ResizeLVMVolume(ctx, vol, false)
        if err != nil {
            return err
        }
        return nil
    }

    args := buildLVMCreateArgs(ctx, vol)
    out, _, err := RunCommandSplit(ctx, LVCreate, args...)

    if err != nil {
        klog.Errorf("lvm: could not create volume %v cmd %v error: %s", volume, args, string(out))
        
        // 检查是否是 "created but killed" 的情况
        outputStr := string(out)
        if strings.Contains(outputStr, "created") || strings.Contains(outputStr, "Created") {
            klog.Warningf("lvm: volume %s was created but command failed (likely killed), forcing cleanup", volume)
        }
        
        // 强制删除：直接使用 lvremove -f，不检查设备节点
        cleanupArgs := []string{"-f", DevPath + volume}
        cleanupOut, _, cleanupErr := RunCommandSplit(ctx, LVRemove, cleanupArgs...)
        if cleanupErr != nil {
            klog.Errorf("lvm: failed to force cleanup volume %s: %v, output: %s", 
                volume, cleanupErr, string(cleanupOut))
        } else {
            klog.Infof("lvm: successfully force cleaned up failed volume %s", volume)
        }
        
        return err
    }
    
    klog.Infof("lvm: created volume %s", volume)
    return nil
}
```

## 临时修复（生产环境）

### 手动清理僵尸 LV

```bash
# 1. 查找僵尸 LV（active 但没有 open 的）
lvs -o lv_name,lv_attr,vg_name | grep "Vwi-a-tz"

# 2. 强制删除僵尸 LV
lvremove -f devbox-vg/devbox-561c5e7d-e071-48c3-853b-618bf07b6e62
lvremove -f devbox-vg/devbox-2b28cf18-4cfc-4e94-922c-370d0a01e84e

# 3. 验证
lvs | grep devbox-561c5e7d  # 应该查不到
```

### 批量清理脚本

```bash
#!/bin/bash
# cleanup-zombie-lvs.sh

VG_NAME="devbox-vg"

# 查找所有 active 但没有 open 的 devbox LV
ZOMBIE_LVS=$(lvs --noheadings -o lv_name,lv_attr,vg_name | \
    awk '$2 ~ /^Vwi-a-tz/ && $3 == "'$VG_NAME'" && $1 ~ /^devbox-/ {print $1}')

if [ -z "$ZOMBIE_LVS" ]; then
    echo "No zombie LVs found"
    exit 0
fi

echo "Found zombie LVs:"
echo "$ZOMBIE_LVS"
echo ""

for lv in $ZOMBIE_LVS; do
    echo "Removing $VG_NAME/$lv"
    lvremove -f "$VG_NAME/$lv"
    if [ $? -eq 0 ]; then
        echo "  ✓ Removed successfully"
    else
        echo "  ✗ Failed to remove"
    fi
done
```

## 预防措施

### 1. 增加 LVM 命令超时

```go
// lvm.go 中的 CommandTimeout
const (
    CommandTimeout      = 120 * time.Second  // 从 30s 增加到 120s
    CommandGraceTimeout = 10 * time.Second
)
```

### 2. 监控 thin pool 使用率

```bash
# 定期检查 thin pool 使用率
lvs -o lv_name,data_percent,metadata_percent devbox-vg/devbox-vg-thinpool

# 告警阈值：data_percent > 80%
```

### 3. 定期清理僵尸 LV

添加 cron 任务：
```cron
# 每小时检查并清理僵尸 LV
0 * * * * /usr/local/bin/cleanup-zombie-lvs.sh >> /var/log/lvm-cleanup.log 2>&1
```

### 4. 添加 LV 状态监控

在 snapshotter 启动时检查并清理：

```go
func NewSnapshotter(root string, opts ...Opt) (snapshots.Snapshotter, error) {
    // ... 初始化代码 ...
    
    // 启动时清理僵尸 LV
    go func() {
        time.Sleep(10 * time.Second)  // 等待系统稳定
        ctx := context.Background()
        if err := o.cleanupZombieLVs(ctx); err != nil {
            log.G(ctx).WithError(err).Warn("failed to cleanup zombie LVs on startup")
        }
    }()
    
    return o, nil
}

func (o *Snapshotter) cleanupZombieLVs(ctx context.Context) error {
    // 实现僵尸 LV 清理逻辑
}
```

## 总结

### 问题核心

1. **LV 创建过程可能被中断**，导致 LVM 元数据存在但设备节点缺失
2. **CheckVolumeExists 设计缺陷**，只检查设备节点不检查元数据
3. **DestroyVolume 清理失败**，误认为 LV 不存在而跳过删除
4. **并发调用**导致僵尸 LV 被重复发现和尝试处理

### 修复优先级

1. **高优先级**（必须修复）：
   - 修复 `CheckVolumeExists`，使用 `lvs` 检查元数据
   - 修复 `DestroyVolume`，使用 `lvremove -f` 强制删除

2. **中优先级**（建议修复）：
   - 改进 `getLvDeviceName` 的日志级别
   - 在 `CreateVolume` 失败清理中使用强制删除

3. **低优先级**（优化）：
   - 在 `getCleanupLvNames` 中识别和标记僵尸 LV
   - 增加 LVM 命令超时时间
   - 添加定期清理机制

### 验证方法

修复后验证：
```bash
# 1. 创建测试容器并强制杀死
# 2. 检查是否有僵尸 LV
lvs -o lv_name,lv_attr | grep "Vwi-a-tz"

# 3. 触发清理
crictl rmi --prune
# 或重启 containerd

# 4. 再次检查，僵尸 LV 应该被清理
lvs | grep devbox-
```

## 参考资料

- LVM 属性字段说明：`man lvs`
- udev 同步问题：`man udevadm`
- 相关代码：
  - `snapshots/devbox/lvm/lvm.go:401-413` (CheckVolumeExists)
  - `snapshots/devbox/lvm/lvm.go:363-398` (DestroyVolume)
  - `snapshots/devbox/lvm/lvm.go:323-360` (CreateVolume)
  - `snapshots/devbox/devbox.go:550-575` (getCleanupLvNames)

