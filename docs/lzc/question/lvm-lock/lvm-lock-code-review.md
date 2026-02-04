# LVM Lock 代码审查报告

## 一、当前锁机制概述

### 1.1 锁的定义

**文件**: `snapshots/devbox/lvm/lvm.go:40`

```go
// lvmLock lvm global lock
var lvmLock sync.Mutex
```

这是一个**全局互斥锁**，用于保护所有 LVM 和文件系统操作。

### 1.2 锁覆盖的函数（lvm.go）

| 函数 | 行号 | 是否加锁 | 保护的操作 |
|------|------|---------|-----------|
| `CreateVolume` | 329-330 | ✅ 是 | LVM 创建 |
| `DestroyVolume` | 364-365 | ✅ 是 | LVM 删除 + 文件系统清理 |
| `ForceDestroyVolume` | 405-406 | ✅ 是 | 强制删除 LVM |
| `MountVolume` | 442-443 | ✅ 是 | 文件系统挂载 |
| `UnmountVolume` | 477-478 | ✅ 是 | 文件系统卸载 |
| `FindMountPointByDevice` | 504-505 | ✅ 是 | 读取 /proc/mounts |
| `ResizeLVMVolume` | 675-676 | ✅ 是 | LVM 扩容 + 文件系统扩容 |
| `ListLVMLogicalVolumeByVG` | 1173-1174 | ✅ 是 | 列出 LV |
| `IsMountPoint` | 561 | ❌ 否 | 检查是否挂载点 |
| `CheckVolumeExists` | 623 | ❌ 否 | 检查 LV 是否存在 |
| `GetVolumeDevPath` | 638 | ❌ 否 | 获取设备路径 |

---

## 二、发现的问题

### 问题 1：devbox.go 中的操作未加锁 ⚠️ 高危

#### 1.1 `mkfs` 函数未使用 lvmLock

**文件**: `snapshots/devbox/devbox.go:659-672`

```go
func (o *Snapshotter) mkfs(lvName string) error {
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    // Check if the device exists
    if _, err := os.Stat(devicePath); os.IsNotExist(err) {
        return fmt.Errorf("LVM logical volume %s does not exist: %w", devicePath, err)
    }

    cmd := exec.Command("mkfs.ext4", devicePath)
    output, err := cmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf("failed to create filesystem on %s: %w, output: %s", devicePath, err, string(output))
    }
    return nil
}
```

**问题**：
- `mkfs` 是文件系统操作，但没有使用 `lvmLock` 保护
- 如果在其他地方正在对这个 LV 进行其他操作（如 resize、delete），可能会冲突

**并发场景示例**：
```go
// Goroutine 1: 正在格式化 LV
go devbox.mkfs("devbox-123")  // ← 未加锁

// Goroutine 2: 同时在删除 LV
go lvm.DestroyVolume(ctx, vol)  // ← 已加锁，持有 lvmLock

// 结果：mkfs 可能访问正在被删除的设备
```

**影响**：
- 文件系统损坏
- 设备不存在错误
- 数据不一致

#### 1.2 `isMountPoint` 函数未使用 lvmLock

**文件**: `snapshots/devbox/devbox.go:642-657`

```go
func isMountPoint(dir string) (bool, error) {
    mounts, err := readProcMounts()
    if err != nil {
        return false, err
    }

    // check if the directory is in the mount list
    for _, fields := range mounts {
        mountPoint := fields[1]
        if mountPoint == dir {
            return true, nil
        }
    }

    return false, nil
}
```

**问题**：
- 读取 `/proc/mounts` 时没有加锁
- 但在 `lvm.go` 中，`FindMountPointByDevice` 读取 `/proc/mounts` 时是加锁的
- 可能出现读-写不一致

**并发场景示例**：
```go
// Goroutine 1: 检查是否挂载点（未加锁）
isMounted := devbox.isMountPoint("/path/to/mount")  // 读取 /proc/mounts

// Goroutine 2: 同时挂载设备（已加锁）
lvm.MountVolume(...)  // 修改 /proc/mounts（实际是修改内核状态）

// 结果：isMounted 可能读到旧值
```

---

### 问题 2：锁粒度过大，性能问题 ⚠️ 中危

#### 2.1 全局锁导致所有 LV 串行化

**当前实现**：
```go
var lvmLock sync.Mutex  // ← 全局锁

func CreateVolume(...) {
    lvmLock.Lock()
    defer lvmLock.Unlock()
    // 操作 LV-A
}

func MountVolume(...) {
    lvmLock.Lock()
    defer lvmLock.Unlock()
    // 操作 LV-B
}
```

**问题**：
- 所有 LV 的操作都互斥，即使操作不同的 LV
- 无法并发处理多个容器

**并发场景示例**：
```go
// 容器 1: 创建 devbox-abc 的 LV
lvm.CreateVolume(ctx, vol1)  // 持有 lvmLock，阻塞其他操作

// 容器 2: 同时创建 devbox-xyz 的 LV（完全独立的 LV）
lvm.CreateVolume(ctx, vol2)  // ← 被阻塞，等待容器 1 完成

// 结果：本可以并行的操作被串行化
```

**影响**：
- 性能严重下降
- 容器启动延迟增加
- 无法利用多核优势

#### 2.2 读操作也使用互斥锁

**文件**: `snapshots/devbox/lvm/lvm.go:504-505`

```go
func FindMountPointByDevice(devicePath string) ([]string, error) {
    lvmLock.Lock()
    defer lvmLock.Unlock()

    data, err := os.ReadFile("/proc/mounts")  // ← 纯读操作
    // ...
}
```

**问题**：
- 读取 `/proc/mounts` 是纯读操作
- 应该使用读写锁（`sync.RWMutex`），允许多个读者并发

**并发场景示例**：
```go
// Goroutine 1: 查询挂载点（读操作）
lvm.FindMountPointByDevice("/dev/vg/lv1")  // 持有 lvmLock

// Goroutine 2: 同时查询另一个挂载点（读操作）
lvm.FindMountPointByDevice("/dev/vg/lv2")  // ← 被阻塞

// 结果：两个读操作互相阻塞
```

---

### 问题 3：锁的生命周期管理问题 ⚠️ 高危

#### 3.1 prepareLvmDirectory 中的锁使用

**文件**: `snapshots/devbox/devbox.go:958-1025`

```go
func (o *Snapshotter) prepareLvmDirectory(ctx context.Context, snapshotDir string, contentKey string, useLimit string) (string, string, error) {
    lvName := "devbox-" + contentKey

    td, err := os.MkdirTemp(snapshotDir, "new-")
    // ...

    vol := &apis.LVMVolume{...}
    mounted := false

    defer func() {
        if err != nil {
            if mounted {
                o.unmountLvm(ctx, td)  // ← 在 lvm.go 中会获取 lvmLock
            }
            o.forceRemoveLv(ctx, lvName)  // ← 在 lvm.go 中会获取 lvmLock
        }
    }()

    err = lvm.CreateVolume(ctx, vol)  // ← 在 lvm.go 中会获取 lvmLock
    if err != nil {
        return td, lvName, fmt.Errorf("failed to create LVM logical volume %s: %w", lvName, err)
    }

    if err = o.mkfs(lvName); err != nil {  // ← mkfs 中没有获取 lvmLock
        return td, lvName, fmt.Errorf("failed to create filesystem on LVM logical volume %s: %w", lvName, err)
    }

    mounted = true
    if err = o.mountLvm(ctx, lvName, td); err != nil {  // ← mountLvm 会获取 lvmLock
        return td, lvName, fmt.Errorf("failed to mount LVM logical volume %s: %w", lvName, err)
    }

    // ...
}
```

**问题**：
- `lvm.CreateVolume`、`o.mountLvm` 都会获取 `lvmLock`
- `o.mkfs` **不会**获取 `lvmLock`
- 但 `defer` 清理时又调用 `o.unmountLvm`（会获取 `lvmLock`）
- 锁的使用不一致

**潜在风险**：
```go
// 时间线：
// T1: lvm.CreateVolume(ctx, vol)     ← 获取 lvmLock，创建 LV，释放锁
// T2: o.mkfs(lvName)                  ← 不获取 lvmLock，格式化
// T3: o.mountLvm(ctx, lvName, td)    ← 获取 lvmLock
// T4: (在 T2-T3 之间) 另一个 goroutine 调用 lvm.DestroyVolume(ctx, vol) ← 删除 LV
// T5: o.mountLvm 失败（设备不存在）
// T6: defer 清理：o.unmountLvm ← 尝试卸载不存在的设备

// 问题：T2 和 T3/T4 之间没有锁保护，LV 可能被其他操作删除
```

---

### 问题 4：devbox.go 中的 RemoveDir 存在竞态条件 ⚠️ 高危

**文件**: `snapshots/devbox/devbox.go:368-389`

```go
func (o *Snapshotter) RemoveDir(ctx context.Context, dir string) {
    isMounted, err := lvm.IsMountPoint(dir)  // ← lvm.go:561，未加锁
    if err != nil {
        log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to check if path is a mount point")
        return
    }
    if isMounted {
        if err1 := o.unmountLvm(ctx, dir); err1 != nil {  // ← lvm.go:685，会获取 lvmLock
            log.G(ctx).WithError(err1).WithField("path", dir).Warn("failed to unmount directory")
            return
        }
        if err1 := os.Remove(dir); err1 != nil {
            log.G(ctx).WithError(err1).WithField("path", dir).Warn("failed to remove directory")
            return
        }
    } else {
        if err1 := os.RemoveAll(dir); err1 != nil {  // ← 没有 lvmLock 保护
            log.G(ctx).WithError(err1).WithField("path", dir).Warn("failed to remove directory")
            return
        }
    }
}
```

**问题**：
- `lvm.IsMountPoint` 不加锁（lvm.go:561）
- `o.unmountLvm` 加锁（lvm.go:685）
- `os.Remove` 和 `os.RemoveAll` 不加锁

**竞态条件**：
```go
// 时间线（TOCTOU - Time-of-check to time-of-use）：

// T1: 检查挂载状态
isMounted := lvm.IsMountPoint("/path/to/dir")  // 返回 false（未加锁）

// T2: 另一个 goroutine 挂载了这个目录
lvm.MountVolume("/dev/vg/lv", "/path/to/dir", "ext4", 0, "")  // ← 成功挂载

// T3: 基于过时的检查结果执行操作
if !isMounted {
    os.RemoveAll("/path/to/dir")  // ← 删除已挂载的目录！数据损坏！
}
```

**影响**：
- **数据丢失**：删除正在使用的挂载点
- **文件系统损坏**：删除挂载点导致文件系统元数据不一致
- **设备忙错误**：后续操作失败

---

### 问题 5：CreateVolume 中检查后使用的竞态条件 ⚠️ 中危

**文件**: `snapshots/devbox/lvm/lvm.go:328-360`

```go
func CreateVolume(ctx context.Context, vol *apis.LVMVolume) error {
    lvmLock.Lock()
    defer lvmLock.Unlock()

    volume := vol.Spec.VolGroup + "/" + vol.Name

    volExists, err := CheckVolumeExists(ctx, vol)  // ← 检查 LV 是否存在
    if err != nil {
        return err
    }
    if volExists {
        klog.Infof("CreateVolume: volume (%s) already exists, skipping its creation", volume)
        err := resizeLVMVolumeInternal(ctx, vol, false)  // ← 已存在，resize
        if err != nil {
            return err
        }
        return nil
    }

    // ... 创建 LV 的代码
}
```

**虽然整个函数在锁内，但仍有一个问题**：
- `CheckVolumeExists` 调用 `os.Stat` 检查设备文件
- 但在返回 `volExists=true` 后，调用 `resizeLVMVolumeInternal`
- 在调用 `lvextend` 之前，设备可能被其他进程删除

**并发场景**（与外部进程）：
```go
// 时间线：

// T1: CreateVolume 检查设备
volExists := CheckVolumeExists(ctx, vol)  // 返回 true
// （仍在持有 lvmLock）

// T2: 外部进程（如 lvremove）删除了 LV
// （注意：lvmLock 只保护本进程，不保护外部进程）

// T3: CreateVolume 执行 resize
resizeLVMVolumeInternal(ctx, vol, false)  // ← 设备已不存在，失败
```

---

### 问题 6：事务与锁的交互问题 ⚠️ 高危

#### 6.1 Remove 函数中的锁使用

**文件**: `snapshots/devbox/devbox.go:394-450`

```go
func (o *Snapshotter) Remove(ctx context.Context, key string) (err error) {
    // ...
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // ... 事务内操作 ...

        mountPath, err = storage.RemoveDevbox(ctx, key)
        if mountPath != "" {
            if err = o.unmountLvm(ctx, mountPath); err != nil {  // ← 获取 lvmLock
                log.G(ctx).WithError(err).WithField("path", mountPath).Warn("failed to unmount directory")
            }
        }

        _, _, err = storage.Remove(ctx, key)
        // ...

        if !o.asyncRemove {
            removals, err = o.getCleanupDirectories(ctx)  // ← 不获取 lvmLock
            removedLvNames, err = o.getCleanupLvNames(ctx)  // ← 会获取 lvmLock
        }
        return nil
    })
}
```

**问题**：
- 事务内获取 `lvmLock`
- 如果 `lvmLock` 被其他操作持有，事务会长时间阻塞
- 导致数据库事务持锁时间过长

**并发场景示例**：
```go
// Goroutine 1: Remove 操作
o.ms.WithTransaction(ctx, true, func(ctx) error {
    // ...
    o.unmountLvm(ctx, mountPath)  // ← 等待 lvmLock，事务被阻塞
    // ...
    return nil
})

// Goroutine 2: 另一个慢速的 LVM 操作（持有 lvmLock）
lvm.CreateVolume(ctx, vol)  // ← 持有 lvmLock 1 分钟

// 结果：Goroutine 1 的事务被阻塞 1 分钟，阻塞其他所有事务
```

**影响**：
- 数据库事务吞吐量下降
- 其他操作（Prepare、Commit 等）被阻塞
- 整体性能下降

---

### 问题 7：cleanupDirectories 中的锁嵌套 ⚠️ 中危

**文件**: `snapshots/devbox/devbox.go:494-537`

```go
func (o *Snapshotter) cleanupDirectories(ctx context.Context) (_ []string, _ []string, err error) {
    // ...
    if err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // ...

        // Unmount any mounted LVs
        for _, lvName := range removedLvNames {
            devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
            mountPoints, err := lvm.FindMountPointByDevice(devicePath)  // ← 获取 lvmLock
            if err != nil {
                // ...
                continue
            }
            for _, mountPoint := range mountPoints {
                if err := o.unmountLvm(ctx, mountPoint); err != nil {  // ← 获取 lvmLock
                    // ...
                }
            }
        }

        return nil
    }); err != nil {
        return nil, nil, err
    }

    return cleanupDirs, removedLvNames, nil
}
```

**问题**：
- 事务内多次获取 `lvmLock`（`FindMountPointByDevice`、`unmountLvm`）
- 虽然不会死锁（因为每次都释放锁），但性能差

**性能问题**：
```go
// cleanupDirectories 执行流程：
事务开始 → 获取 lvmLock（查找挂载点） → 释放 lvmLock
       → 获取 lvmLock（卸载 LV1） → 释放 lvmLock
       → 获取 lvmLock（查找挂载点） → 释放 lvmLock
       → 获取 lvmLock（卸载 LV2） → 释放 lvmLock
       → ...
事务结束

// 如果有 10 个 LV 需要清理，事务内获取/释放 lvmLock 20 次
```

---

### 问题 8：FindMountPointByDevice 的锁粒度问题 ⚠️ 低危

**文件**: `snapshots/devbox/lvm/lvm.go:503-558`

```go
func FindMountPointByDevice(devicePath string) ([]string, error) {
    lvmLock.Lock()
    defer lvmLock.Unlock()

    data, err := os.ReadFile("/proc/mounts")  // ← 纯读操作
    if err != nil {
        return nil, fmt.Errorf("failed to read /proc/mounts: %w", err)
    }

    // 解析 /proc/mounts...
}
```

**问题**：
- 读取 `/proc/mounts` 是纯读操作
- 使用互斥锁导致所有读者串行化
- 应该使用读写锁（`sync.RWMutex`）

**当前性能**：
```go
// 3 个 goroutine 同时查询不同的挂载点：
go lvm.FindMountPointByDevice("/dev/vg/lv1")  // 阻塞
go lvm.FindMountPointByDevice("/dev/vg/lv2")  // 阻塞
go lvm.FindMountPointByDevice("/dev/vg/lv3")  // 阻塞

// 执行时间：3 × T（串行）
// 理想时间：T（并行）
```

---

## 三、潜在并发场景总结

### 场景 1：mkfs 与 Mount/Unmount 并发

```go
// Goroutine 1: 正在格式化设备
devbox.mkfs("devbox-123")  // ← 未加锁

// Goroutine 2: 同时卸载设备
lvm.UnmountVolume("/path/to/mount")  // ← 加锁

// 结果：
// - mkfs 可能访问正在被卸载的设备
// - 设备忙错误或文件系统损坏
```

### 场景 2：IsMountPoint 与 Mount/Unmount 并发

```go
// Goroutine 1: 检查挂载状态
isMounted := lvm.IsMountPoint("/path")  // ← 未加锁，返回 false

// Goroutine 2: 同时挂载
lvm.MountVolume("/dev/vg/lv", "/path", "ext4", 0, "")  // ← 加锁

// Goroutine 1: 基于过时检查结果删除目录
if !isMounted {
    os.RemoveAll("/path")  // ← 删除已挂载的目录
}

// 结果：
// - 删除正在使用的挂载点
// - 文件系统损坏
```

### 场景 3：RemoveDir 与 Mount 并发

```go
// Goroutine 1: RemoveDir 检查并删除
devbox.RemoveDir(ctx, "/path/to/mount")  // IsMountPoint 未加锁

// Goroutine 2: 同时挂载
lvm.MountVolume("/dev/vg/lv", "/path/to/mount", "ext4", 0, "")  // 加锁

// TOCTOU 竞态：
// T1: RemoveDir 检查 IsMountPoint（未加锁）→ false
// T2: MountVolume 挂载设备（加锁）
// T3: RemoveDir 删除目录（未加锁）

// 结果：
// - 删除已挂载的目录
// - 文件系统元数据损坏
```

### 场景 4：事务与锁的长时间阻塞

```go
// Goroutine 1: Remove 事务
o.ms.WithTransaction(ctx, true, func(ctx) error {
    // ...
    o.unmountLvm(ctx, mountPath)  // ← 等待 lvmLock
    // ...
    return nil
})

// Goroutine 2: 慢速 LVM 操作（在事务外）
lvm.CreateVolume(ctx, vol)  // ← 持有 lvmLock 1 分钟

// 结果：
// - 事务被阻塞 1 分钟
// - 其他事务也被阻塞（数据库事务互斥）
// - 系统吞吐量下降
```

---

## 四、改进建议

### 建议 1：为 mkfs 添加锁保护 🔴 高优先级

**修改**：`devbox.go:mkfs`

```go
func (o *Snapshotter) mkfs(lvName string) error {
    // 获取全局 LVM 锁
    lvm.LockLV()
    defer lvm.UnlockLV()

    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    // ... 现有代码 ...
}
```

**需要在 lvm.go 中导出锁函数**：
```go
// lvm.go
func LockLV() {
    lvmLock.Lock()
}

func UnlockLV() {
    lvmLock.Unlock()
}
```

---

### 建议 2：为 isMountPoint 添加锁保护 🔴 高优先级

**修改方案 1**：在 `devbox.go` 中使用锁

```go
func isMountPoint(dir string) (bool, error) {
    lvm.LockLV()
    defer lvm.UnlockLV()

    mounts, err := readProcMounts()
    // ... 现有代码 ...
}
```

**修改方案 2**：直接使用 `lvm.IsMountPoint`（已存在）

```go
// devbox.go
func (o *Snapshotter) RemoveDir(ctx context.Context, dir string) {
    // 改用 lvm.IsMountPoint（已经检查了目录存在性）
    isMounted, err := lvm.IsMountPoint(dir)  // ← 虽然未加锁，但逻辑与 IsMountPoint 一致
    if err != nil {
        log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to check if path is a mount point")
        return
    }
    // ...
}
```

**注意**：`lvm.IsMountPoint` 本身也需要加锁保护。

---

### 建议 3：改用读写锁，提升并发性能 🟡 中优先级

**修改**：`lvm.go:40`

```go
// lvmLock lvm global read-write lock
var lvmLock sync.RWMutex

// 为读操作添加 RLock
func FindMountPointByDevice(devicePath string) ([]string, error) {
    lvmLock.RLock()
    defer lvmLock.RUnlock()

    data, err := os.ReadFile("/proc/mounts")
    // ... 现有代码 ...
}
```

**性能提升**：
```go
// 修改前：所有操作串行
3 个读操作串行：3 × T

// 修改后：读操作可以并行
3 个读操作并行：max(T1, T2, T3)

// 性能提升：约 2-3 倍（读密集场景）
```

---

### 建议 4：细化锁粒度，按 LV 锁定 🟢 低优先级（复杂）

**当前问题**：全局锁导致所有 LV 操作串行化

**改进方案**：按 LV 名称加锁

```go
// lvm.go
type LVLockManager struct {
    locks map[string]*sync.Mutex
    global sync.Mutex  // 保护 locks map
}

var lvLockManager = &LVLockManager{
    locks: make(map[string]*sync.Mutex),
}

func (m *LVLockManager) Lock(lvName string) {
    m.global.Lock()
    lock, exists := m.locks[lvName]
    if !exists {
        lock = &sync.Mutex{}
        m.locks[lvName] = lock
    }
    m.global.Unlock()

    lock.Lock()
}

func (m *LVLockManager) Unlock(lvName string) {
    m.global.Lock()
    lock := m.locks[lvName]
    m.global.Unlock()

    lock.Unlock()
}

// 使用示例：
func CreateVolume(ctx context.Context, vol *apis.LVMVolume) error {
    lvLockManager.Lock(vol.Name)
    defer lvLockManager.Unlock(vol.Name)

    // ... 现有代码 ...
}

func MountVolume(...) {
    lvLockManager.Lock(extractLVName(devicePath))
    defer lvLockManager.Unlock(extractLVName(devicePath))

    // ... 现有代码 ...
}
```

**优势**：
- 不同 LV 的操作可以并发
- 提升并发性能

**劣势**：
- 实现复杂
- 需要处理锁的生命周期
- 可能引入新的 bug

---

### 建议 5：事务外执行 LVM 操作 🔴 高优先级

**当前问题**：事务内调用 `unmountLvm` 等，可能导致事务长时间阻塞

**改进方案**：参考你的状态机方案，将 LVM 操作移到事务外

```go
func (o *Snapshotter) Remove(ctx context.Context, key string) (err error) {
    var mountPath string
    var removedLvNames []string

    // ========== 事务内：只操作元数据 ==========
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        mountPath, err = storage.RemoveDevbox(ctx, key)
        if err != nil && err != errdefs.ErrNotFound {
            return fmt.Errorf("failed to remove devbox content for snapshot %s: %w", key, err)
        }

        _, _, err = storage.Remove(ctx, key)
        if err != nil {
            return fmt.Errorf("failed to remove snapshot %s: %w", key, err)
        }

        if !o.asyncRemove {
            removals, err = o.getCleanupDirectories(ctx)
            if err != nil {
                return fmt.Errorf("unable to get directories for removal: %w", err)
            }
            removedLvNames, err = o.getCleanupLvNames(ctx)
            if err != nil {
                return fmt.Errorf("failed to get LVM logical volume names for snapshot %s: %w", key, err)
            }
        }
        return nil
    })

    // ========== 事务外：执行 LVM 操作（不阻塞其他事务）==========
    if mountPath != "" {
        if err := o.unmountLvm(ctx, mountPath); err != nil {
            log.G(ctx).WithError(err).WithField("path", mountPath).Warn("failed to unmount directory")
        }
    }

    for _, dir := range removals {
        o.RemoveDir(ctx, dir)
    }

    for _, lvName := range removedLvNames {
        if err := o.removeLv(ctx, lvName); err != nil {
            log.G(ctx).WithError(err).WithField("lvName", lvName).Warn("Remove: failed to destroy LVM logical volume")
        }
    }

    return err
}
```

**优势**：
- 事务不被 LVM 操作阻塞
- 提升数据库事务吞吐量
- 与你的状态机方案一致

---

## 五、总结

### 5.1 问题严重程度统计

| 问题 | 严重程度 | 影响 | 优先级 |
|------|---------|------|--------|
| `mkfs` 未加锁 | 🔴 高危 | 数据损坏 | P0 |
| `isMountPoint` 未加锁 | 🔴 高危 | 数据丢失 | P0 |
| RemoveDir 竞态条件 | 🔴 高危 | 文件系统损坏 | P0 |
| 事务与锁交互 | 🔴 高危 | 性能下降 | P0 |
| 全局锁性能问题 | 🟡 中危 | 并发性能低 | P1 |
| 读操作未优化 | 🟢 低危 | 性能略差 | P2 |
| CreateVolume 竞态 | 🟡 中危 | 可能失败 | P1 |

### 5.2 建议的修复顺序

**Phase 1（必须修复）**：
1. ✅ 为 `mkfs` 添加锁保护
2. ✅ 为 `isMountPoint` 添加锁保护
3. ✅ 修复 RemoveDir 中的 TOCTOU 竞态条件

**Phase 2（性能优化）**：
4. ✅ 改用读写锁（`sync.RWMutex`）
5. ✅ 将 LVM 操作移到事务外

**Phase 3（长期优化）**：
6. ✅ 细化锁粒度（按 LV 锁定）

### 5.3 核心原则

1. **所有 LVM 和文件系统操作都必须加锁**
   - 包括 `mkfs`、`mount`、`unmount`、`rm` 等
   - 包括检查操作（`IsMountPoint`、`CheckVolumeExists`）

2. **避免长时间持有锁**
   - 事务内不要执行慢速 LVM 操作
   - 将 LVM 操作移到事务外

3. **读操作使用读写锁**
   - 查询操作使用 `RLock`/`RUnlock`
   - 写操作使用 `Lock`/`Unlock`

4. **避免 TOCTOU 竞态条件**
   - 检查和使用之间不能有其他操作插入
   - 使用原子操作或将检查和使用放在同一个锁内
