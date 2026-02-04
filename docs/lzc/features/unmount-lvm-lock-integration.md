# Unmount LVM 操作锁机制集成方案

## 问题背景

### 1. 核心问题

在生产环境中发现 containerd goroutine 阻塞 5 分钟的问题，堆栈信息显示：

```
goroutine 18603 [syscall, 5 minutes]:
syscall.Unmount(...)
  → snapshots/devbox/devbox.go:667 unmountLvm
    → snapshots/devbox/devbox.go:245 Update.func1
      → snapshots/devbox/storage/metastore.go:121 WithTransaction
        → go.etcd.io/bbolt.(*DB).Update
```

**问题根源**：
- `unmountLvm` 操作在 **BoltDB 事务内**执行
- `syscall.Unmount` 可能长时间阻塞（设备 busy、LVM 锁竞争等）
- 阻塞导致 BoltDB 写锁被长时间持有，整个 containerd 被阻塞

### 2. 当前状态

- ✅ **LVM 包已有全局锁**：保护 `CreateVolume`、`DestroyVolume`、`ResizeLVMVolume` 等 LVM 命令操作
- ❌ **unmountLvm 无锁保护**：文件系统操作（`syscall.Unmount`）没有锁机制
- ⚠️ **潜在冲突**：unmount 时可能与 LVM 操作（如 `lvremove`、`lvextend`）竞争同一设备，导致相互阻塞

### 3. 调用场景分析

`unmountLvm` 在以下场景被调用：

1. **Update 方法**（`devbox.go:245`）：在事务内调用，用于卸载 LVM 设备
2. **Remove 方法**（`devbox.go:429`）：在事务内调用，删除快照时卸载
3. **RemoveDir 方法**（`devbox.go:375`）：在 defer 中调用，清理目录时卸载
4. **prepareSnapshot 方法**（`devbox.go:831`）：在事务内调用，准备快照时卸载

**关键发现**：所有调用都在事务内或紧邻事务，存在阻塞风险。

## 需求分析

### 1. 核心需求

- **防止并发冲突**：unmount 操作需要与 LVM 操作（lvcreate、lvremove 等）串行执行
- **避免锁冗余**：不希望引入新的锁，复用现有的 LVM 全局锁
- **保持代码简洁**：方案实现要简单，易于维护

### 2. 约束条件

- ✅ 已有 `lvm` 包的全局锁 `lvmLock`（`sync.Mutex`）
- ✅ 所有 LVM 命令操作已受锁保护
- ❌ `unmountLvm` 目前是 snapshotter 层的方法，没有锁保护
- ❌ `mountLvm` 同样没有锁保护

### 3. 设计目标

1. **统一锁管理**：所有设备相关操作（LVM 命令 + mount/unmount）使用同一个锁
2. **封装清晰**：底层操作集中在 `lvm` 包，snapshotter 层只负责业务逻辑
3. **易于扩展**：未来添加其他设备操作（如 fsck、resize2fs）遵循相同模式

## 方案设计

### 方案概述

**将 mount/unmount 操作纳入 LVM 全局锁保护范围**，通过在 `lvm` 包中提供 `MountVolume` 和 `UnmountVolume` 函数，内部使用现有的 `lvmLock` 进行保护。

### 方案优势

1. ✅ **只用一个锁**：`lvmLock` 保护所有设备相关操作
2. ✅ **概念统一**：unmount 的是 LVM 设备上的文件系统，属于设备操作，应该和 LVM 命令统一管理
3. ✅ **封装性好**：所有设备操作都在 `lvm` 包，snapshotter 只管业务逻辑
4. ✅ **易于维护**：未来如果要加 mount 操作，也遵循同样的模式

### 架构设计

```
┌─────────────────────────────────────────┐
│         snapshotter 层                   │
│  (devbox.go: unmountLvm, mountLvm)      │
│         ↓ 调用                           │
└─────────────────────────────────────────┘
         ↓
┌─────────────────────────────────────────┐
│         lvm 包                           │
│  ┌─────────────────────────────────┐    │
│  │  全局锁: lvmLock (sync.Mutex)   │    │
│  └─────────────────────────────────┘    │
│         ↓ 保护                           │
│  ┌─────────────────────────────────┐    │
│  │  CreateVolume                   │    │
│  │  DestroyVolume                  │    │
│  │  ResizeLVMVolume                │    │
│  │  ForceDestroyVolume              │    │
│  │  ListLVMLogicalVolumeByVG        │    │
│  │  MountVolume      ← 新增         │    │
│  │  UnmountVolume    ← 新增         │    │
│  └─────────────────────────────────┘    │
└─────────────────────────────────────────┘
```

## 实现步骤

### 第一步：在 lvm 包中添加新函数

**文件**：`snapshots/devbox/lvm/lvm.go`

**添加位置**：建议在 `ForceDestroyVolume` 函数之后

**新增函数**：

1. **`MountVolume`**：挂载 LVM 设备到指定路径
   - 参数：`devicePath`, `mountPath`, `fsType`, `flags`, `options`
   - 功能：检查设备存在、创建挂载目录、执行 mount
   - 锁保护：使用 `lvmLock.Lock()/Unlock()`

2. **`UnmountVolume`**：卸载挂载点
   - 参数：`mountPath`
   - 功能：检查是否挂载、执行 unmount
   - 锁保护：使用 `lvmLock.Lock()/Unlock()`

3. **`isMountPoint`**：检查路径是否为挂载点（从 devbox.go 移过来）
   - 参数：`dir`
   - 功能：通过比较目录和父目录的设备号判断是否为挂载点
   - 注意：需要导入 `syscall` 包

**关键实现点**：

```go
// 示例：UnmountVolume 实现
func UnmountVolume(mountPath string) error {
    lvmLock.Lock()
    defer lvmLock.Unlock()

    // 1. 检查是否是挂载点
    isMounted, err := isMountPoint(mountPath)
    if err != nil {
        return fmt.Errorf("failed to check if %s is a mount point: %w", mountPath, err)
    }

    if !isMounted {
        klog.Infof("lvm: path %s is not mounted, skipping unmount", mountPath)
        return nil
    }

    // 2. 执行 unmount
    if err := syscall.Unmount(mountPath, 0); err != nil {
        klog.Warningf("lvm: unmount failed for %s: %v", mountPath, err)
        return fmt.Errorf("failed to unmount %s: %w", mountPath, err)
    }

    klog.Infof("lvm: successfully unmounted %s", mountPath)
    return nil
}
```

### 第二步：修改 snapshotter 层调用

**文件**：`snapshots/devbox/devbox.go`

#### 2.1 修改 `unmountLvm` 函数（line 658-672）

**原实现**：
```go
func (o *Snapshotter) unmountLvm(ctx context.Context, path string) error {
    isMounted, err := isMountPoint(path)
    // ... 检查并执行 unmount
}
```

**新实现**：
```go
func (o *Snapshotter) unmountLvm(ctx context.Context, path string) error {
    // 直接调用 lvm 包的 UnmountVolume，它内部有锁保护
    if err := lvm.UnmountVolume(path); err != nil {
        log.G(ctx).WithError(err).Errorf("failed to unmount %s", path)
        return err
    }
    log.G(ctx).Debugf("successfully unmounted %s", path)
    return nil
}
```

#### 2.2 修改 `mountLvm` 函数（line 641-656）

**原实现**：
```go
func (o *Snapshotter) mountLvm(ctx context.Context, lvName string, path string) error {
    // ... 检查路径、执行 mount
}
```

**新实现**：
```go
func (o *Snapshotter) mountLvm(ctx context.Context, lvName string, path string) error {
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    
    // 调用 lvm 包的 MountVolume，它内部有锁保护
    if err := lvm.MountVolume(devicePath, path, "ext4", 0, ""); err != nil {
        log.G(ctx).WithError(err).Errorf("failed to mount %s to %s", devicePath, path)
        return err
    }
    
    log.G(ctx).Debugf("successfully mounted %s to %s", devicePath, path)
    return nil
}
```

#### 2.3 删除 `isMountPoint` 函数（line 598-624）

**原因**：该函数已移到 `lvm` 包中，snapshotter 层不再需要。

#### 2.4 修改 `RemoveDir` 函数（line 368-389）

**原实现**：
```go
func (o *Snapshotter) RemoveDir(ctx context.Context, dir string) {
    isMounted, err := isMountPoint(dir)
    if isMounted {
        o.unmountLvm(ctx, dir)
    }
    // ...
}
```

**新实现**：
```go
func (o *Snapshotter) RemoveDir(ctx context.Context, dir string) {
    // 直接调用 unmountLvm，它内部会检查是否挂载
    if err1 := o.unmountLvm(ctx, dir); err1 != nil {
        log.G(ctx).WithError(err1).WithField("path", dir).Warn("failed to unmount directory")
        return
    }
    
    if err1 := os.Remove(dir); err1 != nil {
        log.G(ctx).WithError(err1).WithField("path", dir).Warn("failed to remove directory")
        return
    }
}
```

**注意**：如果 `unmountLvm` 返回"未挂载"（不是错误），可以继续删除目录。需要确认 `lvm.UnmountVolume` 在未挂载时的行为。

### 第三步：处理事务外调用（可选优化）

**问题**：虽然 mount/unmount 现在有锁保护，但如果它们在事务内调用，仍然可能阻塞事务。

**建议**：后续可以考虑将 mount/unmount 操作移到事务外执行，但这不在本次方案范围内。

**当前方案的价值**：
- ✅ 解决了 unmount 与 LVM 操作的并发冲突
- ✅ 即使 unmount 阻塞，也不会因为锁竞争导致 LVM 操作失败
- ⚠️ 事务内调用 unmount 仍然可能阻塞事务（这是另一个问题，需要单独处理）

## 关键点说明

### 1. 锁的粒度

- **锁范围**：所有 LVM 命令 + mount/unmount 操作
- **锁粒度**：全局锁，所有设备操作串行执行
- **锁类型**：`sync.Mutex`（互斥锁，非读写锁）

### 2. 错误处理

- **isMountPoint 失败**：返回错误，不继续执行 unmount
- **unmount 失败**：返回详细错误信息，记录警告日志
- **mount 失败**：返回详细错误信息，记录错误日志

### 3. 日志记录

- **成功操作**：记录 Info 级别日志
- **失败操作**：记录 Warning/Error 级别日志
- **跳过操作**：记录 Info 级别日志（如未挂载时跳过 unmount）

### 4. 向后兼容

- ✅ snapshotter 层的接口不变（`unmountLvm`、`mountLvm` 方法签名不变）
- ✅ 调用方代码不需要修改
- ✅ 只是底层实现从 snapshotter 移到 lvm 包

## 测试建议

### 1. 单元测试

- 测试 `MountVolume` 和 `UnmountVolume` 的锁机制
- 测试并发 mount/unmount 操作是否串行执行
- 测试 unmount 未挂载路径时的行为

### 2. 集成测试

- 测试 mount/unmount 与 LVM 操作（如 `lvremove`）的并发场景
- 测试在事务内调用 unmount 的行为（虽然可能阻塞，但不应死锁）

### 3. 压力测试

- 并发创建/删除多个快照，验证锁机制是否正常工作
- 验证长时间阻塞的 unmount 不会影响其他 LVM 操作

## 注意事项

### 1. 事务内调用问题

**当前方案解决了**：
- ✅ unmount 与 LVM 操作的并发冲突

**当前方案未解决**：
- ⚠️ 事务内调用 unmount 仍然可能阻塞事务（这是另一个问题）

**建议**：后续可以考虑将 mount/unmount 操作移到事务外执行，但这需要：
- 修改 `Update`、`Remove` 等方法的实现
- 处理事务提交后操作失败的回滚问题
- 可能需要异步重试机制

### 2. 锁的持有时间

- unmount 操作可能因为设备 busy 而长时间阻塞
- 锁持有时间 = unmount 执行时间
- 如果 unmount 阻塞 5 分钟，锁也会持有 5 分钟
- **但这比没有锁要好**：至少保证了操作的串行性，不会出现死锁

### 3. 强制 unmount

当前方案使用 `syscall.Unmount(path, 0)`（普通 unmount）。

如果遇到设备 busy 的情况，可以考虑：
- 添加重试机制
- 使用 `syscall.MNT_FORCE` 标志（需要谨慎，可能有数据丢失风险）

## 实施检查清单

- [ ] 在 `lvm.go` 中添加 `MountVolume` 函数
- [ ] 在 `lvm.go` 中添加 `UnmountVolume` 函数
- [ ] 在 `lvm.go` 中添加 `isMountPoint` 函数（从 devbox.go 移过来）
- [ ] 修改 `devbox.go` 的 `unmountLvm` 函数
- [ ] 修改 `devbox.go` 的 `mountLvm` 函数
- [ ] 删除 `devbox.go` 的 `isMountPoint` 函数
- [ ] 修改 `devbox.go` 的 `RemoveDir` 函数
- [ ] 添加必要的导入（如 `syscall`）
- [ ] 运行单元测试
- [ ] 运行集成测试
- [ ] 代码审查

## 相关文档

- [LVM 锁机制说明](./lvm_lock.md)
- [Zombie LV 问题分析](../question/devbox-zombie-lv-issue.md)
- [LVM 命令超时解决方案](../question/lvm-command-timeout-solution.md)

## 更新记录

- **2025-01-XX**：初始方案设计

