# LV 删除失败后的重试机制详解

## 一、问题回顾

从日志可以看到：

```
2026-01-06 10:10:44.939 E0106 10:10:44.939471 36624 lvm.go:382] 
    DestroyVolume: could not destroy volume devbox-vg/devbox-342df430-2ee0-4fc3-9ee7-cee319c1f039

2026-01-06 10:10:45.596 I0106 10:10:45.595997 36624 lvm.go:388] 
    DestroyVolume: destroyed volume devbox-vg/devbox-342df430-2ee0-4fc3-9ee7-cee319c1f039
```

**问题**：
1. 第一次删除失败（因为设备还在使用）
2. 约 0.6 秒后成功删除
3. **谁执行了重试？如何重试的？**

---

## 二、重试机制的核心：Cleanup 函数

### 2.1 Cleanup 的调用链

**Containerd 的 GC（垃圾回收）机制**：

```
Containerd GC (metadata/db.go)
    ↓
GarbageCollect() 周期性调用
    ↓
cleanupSnapshotter() 对每个 snapshotter 调用
    ↓
snapshotter.Cleanup() 调用 snapshotter 的 Cleanup 方法
    ↓
devbox.Cleanup() 执行清理
```

**代码位置**：

```go
// metadata/db.go:420-436
if len(m.dirtySS) > 0 {
    for snapshotterName := range m.dirtySS {
        log.G(ctx).WithField("snapshotter", snapshotterName).Debug("schedule snapshotter cleanup")
        go func(snapshotterName string) {
            m.cleanupSnapshotter(ctx, snapshotterName)  // 调用 snapshotter.Cleanup()
            wg.Done()
        }(snapshotterName)
    }
}

// metadata/snapshot.go:846-863
func (s *snapshotter) garbageCollect(ctx context.Context) (d time.Duration, err error) {
    // ... 扫描和删除未使用的 snapshot ...
    
    defer func() {
        if err == nil {
            if c, ok := s.Snapshotter.(snapshots.Cleaner); ok {
                err = c.Cleanup(ctx)  // ✅ 调用 devbox.Cleanup()
            }
        }
    }()
}
```

**关键点**：
- `Cleanup` 是 **周期性调用**的（由 containerd GC 触发）
- 不是立即重试，而是**下次 GC 时重试**

### 2.2 Cleanup 如何找出需要清理的 LV？

**核心函数：`getCleanupLvNames`**：

```go
// snapshots/devbox/devbox.go:569-594
func (o *Snapshotter) getCleanupLvNames(ctx context.Context) ([]string, error) {
    // 1. 从数据库获取所有已记录的 LV 名称
    nameMap, err := storage.GetDevboxLvNames(ctx)
    // nameMap = {"devbox-xxx": "/path/to/mount", ...}
    
    // 2. 从 LVM 获取所有实际存在的 LV
    lvs, err := lvm.ListLVMLogicalVolumeByVG(ctx, o.lvmVgName, o.ThinPoolName)
    // lvs = [{"Name": "devbox-xxx", ...}, {"Name": "devbox-yyy", ...}, ...]
    
    cleanup := []string{}
    for _, d := range lvs {
        // 3. 如果 LV 在数据库中不存在，说明需要清理
        if _, ok := nameMap[d.Name]; ok {
            continue  // LV 在数据库中，跳过
        }
        
        // 4. 只清理以 "devbox" 开头的 LV
        if strings.HasPrefix(d.Name, "devbox") {
            cleanup = append(cleanup, d.Name)
        }
    }
    
    return cleanup, nil
}
```

**关键逻辑**：
1. **扫描数据库**：获取所有已记录的 LV 名称
2. **扫描 LVM**：获取所有实际存在的 LV
3. **对比差异**：找出"存在但不在数据库中"的 LV
4. **这些 LV 就是需要清理的**（包括之前删除失败的）

**为什么能找出之前删除失败的 LV？**
- 第一次删除失败时，LV 仍然存在（`lvs` 命令能查到）
- 但数据库中的记录已被删除（`nameMap` 中没有）
- 所以 `getCleanupLvNames` 会把它找出来

---

## 三、完整的删除和重试流程

### 3.1 第一次删除（Remove 函数）

```go
// snapshots/devbox/devbox.go:394-450
func (o *Snapshotter) Remove(ctx context.Context, key string) (err error) {
    // 1. 在事务内删除元数据
    err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 删除 devbox content 记录
        mountPath, err = storage.RemoveDevbox(ctx, key)
        
        // 卸载文件系统
        if mountPath != "" {
            if err = o.unmountLvm(ctx, mountPath); err != nil {
                log.Warn("failed to unmount directory")
            }
        }
        
        // 删除 snapshot 元数据
        _, _, err = storage.Remove(ctx, key)
        
        // 获取需要清理的 LV 名称
        if !o.asyncRemove {
            removedLvNames, err = o.getCleanupLvNames(ctx)
        }
        return nil
    })
    
    // 2. 在 defer 中删除 LV（事务外）
    defer func() {
        if err == nil {
            for _, lvName := range removedLvNames {
                err := o.removeLv(ctx, lvName)  // 调用 DestroyVolume
                if err != nil {
                    // ⚠️ 删除失败，只记录警告，不返回错误
                    log.Warn("Remove: failed to destroy LVM logical volume")
                    continue
                }
                log.Infof("Remove: LVM logical volume %s removed successfully", lvName)
            }
        }
    }()
}
```

**时间线**：

```
T1: Remove() 调用
    ↓
T2: 事务内删除数据库记录（成功）
    ↓
T3: unmountLvm() 调用 syscall.Unmount（成功）
    ↓
T4: defer 执行，调用 removeLv()
    ↓
T5: DestroyVolume() 执行 lvremove（失败：设备还在使用）
    ↓
T6: 记录警告，函数返回（不重试）
    ↓
T7: LV 仍然存在，但数据库记录已删除
```

### 3.2 重试（Cleanup 函数）

```go
// snapshots/devbox/devbox.go:472-492
func (o *Snapshotter) Cleanup(ctx context.Context) error {
    log.Infof("Cleanup called")
    
    // 1. 获取需要清理的目录和 LV
    cleanup, cleanupLv, err := o.cleanupDirectories(ctx)
    
    // 2. 清理目录
    for _, dir := range cleanup {
        o.RemoveDir(ctx, dir)
    }
    
    // 3. 清理 LV（包括之前删除失败的）
    for _, lvName := range cleanupLv {
        if err := o.removeLv(ctx, lvName); err != nil {
            // ⚠️ 删除失败，记录警告，继续处理其他 LV
            log.Warn("Cleanup: failed to destroy LVM logical volume")
            continue
        }
        log.Infof("Cleanup: LVM logical volume %s removed successfully", lvName)
    }
    
    return nil
}
```

**时间线**：

```
T1: Containerd GC 触发（周期性，可能几分钟后）
    ↓
T2: cleanupSnapshotter() 调用 devbox.Cleanup()
    ↓
T3: cleanupDirectories() 扫描需要清理的 LV
    ↓
T4: getCleanupLvNames() 找出"存在但不在数据库中"的 LV
    ↓
T5: 找到之前删除失败的 LV（仍然存在）
    ↓
T6: 再次调用 removeLv() 尝试删除
    ↓
T7: 如果内核已完成清理，删除成功 ✅
    ↓
T8: 如果内核仍在清理，删除失败，下次 Cleanup 再重试
```

---

## 四、为什么会有多次重试？（即使 GC 配置是默认的）

### 4.1 实际日志分析

从用户提供的日志可以看到：

```
2026-01-09 14:53:34.626070000 - 第一次删除失败
2026-01-09 14:53:34.142208000 - 第二次删除失败（约 0.48 秒前）
2026-01-09 14:53:33.734317000 - 第三次删除失败（约 0.41 秒前）
2026-01-09 14:53:33.318321000 - 第四次删除失败（约 0.42 秒前）
2026-01-09 14:53:33.038235000 - 第五次删除失败（约 0.28 秒前）
2026-01-09 14:53:32.742325000 - 第六次删除失败（约 0.30 秒前）
2026-01-09 14:53:32.418436000 - 第七次删除失败（约 0.32 秒前）
2026-01-09 14:53:31.530464000 - 第八次删除失败（约 0.89 秒前）
```

**关键观察**：
- 在约 3 秒内有 8 次删除尝试
- 都是同一个 LV：`devbox-a9852d24-877e-40ef-be20-2a88ad520479`
- 错误信息：`Logical volume contains a filesystem in use`
- GC 配置是默认的，不太可能在几秒内多次触发

### 4.2 可能的原因分析

#### 原因 1：WithSnapshotCleanup 同步调用 + kubelet 重试（最可能）

**触发链路**：

```go
// pkg/cri/server/container_remove.go:106
container.Container.Delete(ctx, containerd.WithSnapshotCleanup)
    ↓
// container_opts.go:258-273
WithSnapshotCleanup() 同步调用
    ↓
snapshotter.Remove(ctx, snapshotKey)  // 同步执行
    ↓
removeLv() → DestroyVolume()  // 如果失败，返回错误
```

**关键点**：
1. **`WithSnapshotCleanup` 是同步调用**：每次 `RemoveContainer` 都会立即调用 `Remove`
2. **如果删除失败，会返回错误给 kubelet**
3. **kubelet 可能有重试机制**：当 `RemoveContainer` 返回错误时，kubelet 可能会重试

**验证方法**：
- 查看 kubelet 日志，看是否有多次 `RemoveContainer` 调用
- 检查 kubelet 的重试配置

#### 原因 2：多个容器共享同一个 LV（devbox 场景）

**Devbox 的使用模式**：

```
Devbox (contentID: a9852d24-877e-40ef-be20-2a88ad520479)
  └─ LV: devbox-a9852d24-877e-40ef-be20-2a88ad520479
      ├─ Container-1 (snapshot key: container-1)
      ├─ Container-2 (snapshot key: container-2)
      └─ Container-3 (snapshot key: container-3)
```

**问题场景**：
1. 多个容器使用同一个 contentID（共享同一个 LV）
2. 当多个容器同时被删除时，每个容器都会调用 `Remove`
3. 每个 `Remove` 都会尝试删除同一个 LV
4. 第一个删除失败（设备在使用），后续删除也会失败

**代码证据**：

```go
// snapshots/devbox/devbox.go:394-450
func (o *Snapshotter) Remove(ctx context.Context, key string) (err error) {
    // ...
    // 在事务内删除元数据（只删除当前 key 的记录）
    mountPath, err = storage.RemoveDevbox(ctx, key)
    
    // 获取需要清理的 LV（可能包含共享的 LV）
    removedLvNames, err = o.getCleanupLvNames(ctx)
    
    // 在 defer 中尝试删除 LV
    for _, lvName := range removedLvNames {
        err := o.removeLv(ctx, lvName)  // 多个容器可能同时删除同一个 LV
    }
}
```

**关键问题**：
- `getCleanupLvNames` 会找出"存在但不在数据库中"的 LV
- 如果第一个容器删除时，数据库记录已删除，但 LV 还在使用
- 后续容器删除时，也会尝试删除同一个 LV
- **没有并发控制**：多个 goroutine 可能同时尝试删除同一个 LV

#### 原因 3：GC 和 WithSnapshotCleanup 同时触发

**并发场景**：

```
时间线：
T1: kubelet 调用 RemoveContainer
    ↓
T2: WithSnapshotCleanup 同步调用 Remove() → 删除失败
    ↓
T3: 同时，GC 触发 Cleanup() → 也尝试删除同一个 LV → 删除失败
    ↓
T4: kubelet 重试 RemoveContainer → 再次调用 Remove() → 删除失败
    ↓
T5: GC 再次触发（如果配置了 deletion_threshold=1）→ 再次尝试删除
```

**代码证据**：

```go
// metadata/db.go:420-436
// GC 会并发调用多个 snapshotter 的 Cleanup
for snapshotterName := range m.dirtySS {
    go func(snapshotterName string) {
        m.cleanupSnapshotter(ctx, snapshotterName)  // 并发执行
    }(snapshotterName)
}
```

#### 原因 4：LVM 锁竞争

**LVM 操作是串行的**：
- LVM 命令（如 `lvremove`）需要获取 LVM 锁
- 如果多个进程同时执行 `lvremove`，会排队等待
- 第一个进程失败后，后续进程会依次尝试
- 这可能导致多次重试

**验证方法**：
- 检查是否有多个 containerd 进程
- 查看 LVM 锁的状态

### 4.3 当前代码没有自动重试机制

**查看 `DestroyVolume` 代码**：

```go
// snapshots/devbox/lvm/lvm.go:356-391
func DestroyVolume(ctx context.Context, vol *apis.LVMVolume) error {
    // ... 检查 LV 是否存在 ...
    
    // 擦除文件系统签名
    err = removeVolumeFilesystem(vol)
    
    // 删除 LV（没有重试）
    args := buildLVMDestroyArgs(vol)
    out, _, err := RunCommandSplit(ctx, LVRemove, args...)
    
    if err != nil {
        klog.Errorf("DestroyVolume: could not destroy volume %v", volume)
        return err  // ⚠️ 直接返回错误，不重试
    }
    
    return nil
}
```

**结论**：
- `DestroyVolume` **没有重试机制**
- 如果删除失败，直接返回错误
- **重试依赖于 Cleanup 的周期性调用**

---

## 五、重试机制总结

### 5.1 重试的执行者

**Containerd GC（垃圾回收）**：
- 周期性调用 `snapshotter.Cleanup()`
- 不是立即重试，而是**下次 GC 时重试**

### 5.2 重试的触发条件

**`getCleanupLvNames` 的逻辑**：
1. 扫描数据库，获取所有已记录的 LV
2. 扫描 LVM，获取所有实际存在的 LV
3. **找出"存在但不在数据库中"的 LV**
4. 这些 LV 就是需要清理的（包括之前删除失败的）

### 5.3 重试的时间间隔

**不固定**：
- 取决于 containerd GC 的调用频率
- 通常是几分钟到几十分钟
- **不是立即重试**

### 5.4 为什么会有多次重试？（即使 GC 配置是默认的）

**最可能的解释**：

1. **WithSnapshotCleanup 同步调用 + kubelet 重试**：
   - 每次 `RemoveContainer` 都会同步调用 `Remove`
   - 如果删除失败，kubelet 可能会重试 `RemoveContainer`
   - 导致多次删除尝试

2. **多个容器共享同一个 LV**：
   - Devbox 场景中，多个容器可能共享同一个 contentID（同一个 LV）
   - 当多个容器同时被删除时，每个都会尝试删除同一个 LV
   - 没有并发控制，导致多次删除尝试

3. **GC 和 WithSnapshotCleanup 同时触发**：
   - GC 的异步清理和 `WithSnapshotCleanup` 的同步清理可能同时进行
   - 如果配置了 `deletion_threshold=1`，每次删除都会触发 GC

4. **LVM 锁竞争**：
   - 多个进程同时执行 `lvremove` 会排队等待
   - 第一个失败后，后续进程依次尝试

---

## 六、改进建议

### 6.1 在 DestroyVolume 中添加重试机制（推荐）

**当前问题**：
- 删除失败后，需要等待 GC 触发 Cleanup 才能重试
- 重试时间不固定（几分钟到几十分钟）

**改进方案**：

```go
// snapshots/devbox/lvm/lvm.go
func DestroyVolume(ctx context.Context, vol *apis.LVMVolume) error {
    volume := vol.Spec.VolGroup + "/" + vol.Name
    
    volExists, err := CheckVolumeExists(ctx, vol)
    if err != nil {
        return err
    }
    if !volExists {
        klog.Infof("DestroyVolume: volume (%s) doesn't exist, skipping its deletion", volume)
        return nil
    }
    
    // ✅ 添加重试机制
    maxRetries := 10
    retryDelay := 100 * time.Millisecond
    
    for attempt := 0; attempt < maxRetries; attempt++ {
        // 1. 擦除文件系统签名
        if err = removeVolumeFilesystem(vol); err != nil {
            klog.Warningf("DestroyVolume: failed to wipe filesystem (attempt %d/%d): %v", 
                attempt+1, maxRetries, err)
        }
        
        // 2. 尝试删除 LV
        args := buildLVMDestroyArgs(vol)
        out, stderr, err := RunCommandSplit(ctx, LVRemove, args...)
        
        if err == nil {
            klog.Infof("DestroyVolume: destroyed volume %s", volume)
            return nil
        }
        
        // 3. 检查是否是"设备在使用"错误
        if strings.Contains(string(stderr), "contains a filesystem in use") {
            if attempt < maxRetries-1 {
                klog.Infof("DestroyVolume: volume %s still in use, retrying in %v (attempt %d/%d)", 
                    volume, retryDelay, attempt+1, maxRetries)
                time.Sleep(retryDelay)
                continue
            }
        }
        
        // 4. 其他错误，立即返回
        klog.Errorf("DestroyVolume: could not destroy volume %v cmd %v error: %s", 
            volume, args, string(out))
        return err
    }
    
    return fmt.Errorf("failed to destroy volume %s after %d attempts", volume, maxRetries)
}
```

**优势**：
- **立即重试**：不需要等待 GC
- **固定时间间隔**：最多等待 1 秒（10 次 × 100ms）
- **自动处理时间差**：内核清理完成后自动成功

### 6.2 保留 Cleanup 作为兜底机制

**即使添加了重试机制，Cleanup 仍然有用**：
- 处理进程崩溃后的遗留 LV
- 处理网络问题导致的删除失败
- 处理其他异常情况

---

## 七、总结

### 7.1 重试机制

1. **执行者**：Containerd GC（周期性调用）
2. **触发方式**：`Cleanup()` 函数扫描需要清理的 LV
3. **识别方法**：`getCleanupLvNames()` 找出"存在但不在数据库中"的 LV
4. **时间间隔**：不固定，取决于 GC 调用频率（通常几分钟）

### 7.2 为什么会有多次重试？（即使 GC 配置是默认的）

**最可能的解释**：

1. **WithSnapshotCleanup 同步调用 + kubelet 重试**：
   - `RemoveContainer` 会同步调用 `Remove`，失败时 kubelet 可能重试

2. **多个容器共享同一个 LV**：
   - Devbox 场景中，多个容器共享同一个 contentID
   - 每个容器删除时都会尝试删除同一个 LV
   - 没有并发控制

3. **GC 和 WithSnapshotCleanup 同时触发**：
   - 异步 GC 和同步清理可能同时进行

4. **LVM 锁竞争**：
   - 多个进程同时执行 `lvremove` 会排队等待

### 7.3 改进建议

**在 `DestroyVolume` 中添加重试机制**：
- 立即重试，不需要等待 GC
- 最多等待 1 秒（10 次 × 100ms）
- 自动处理内核清理的时间差

**保留 Cleanup 作为兜底**：
- 处理异常情况
- 处理进程崩溃后的遗留 LV

---

## 八、解决方案建议

### 8.1 添加并发控制（推荐）

**问题**：多个 goroutine 可能同时尝试删除同一个 LV

**解决方案**：使用 sync.Map 或 mutex 保护删除操作

```go
// snapshots/devbox/devbox.go
var deletingLVs sync.Map  // 正在删除的 LV 集合

func (o *Snapshotter) removeLv(ctx context.Context, lvName string) error {
    // 检查是否正在删除
    if _, deleting := deletingLVs.LoadOrStore(lvName, struct{}{}); deleting {
        log.G(ctx).Infof("LV %s is already being deleted, skipping", lvName)
        return nil
    }
    defer deletingLVs.Delete(lvName)
    
    // 执行删除
    return lvm.DestroyVolume(ctx, vol)
}
```

### 8.2 在 DestroyVolume 中添加重试机制

**问题**：删除失败后需要等待 GC 才能重试

**解决方案**：在 `DestroyVolume` 中添加重试机制（见 6.1 节）

### 8.3 检查 kubelet 重试配置

**问题**：kubelet 可能在删除失败时重试

**解决方案**：
- 检查 kubelet 日志，确认是否有多次 `RemoveContainer` 调用
- 如果确认是 kubelet 重试，需要优化错误处理，避免返回错误

### 8.4 优化错误处理

**问题**：删除失败时返回错误，导致 kubelet 重试

**解决方案**：对于"设备在使用"错误，可以记录警告但不返回错误

```go
// snapshots/devbox/devbox.go:410-416
for _, lvName := range removedLvNames {
    err := o.removeLv(ctx, lvName)
    if err != nil {
        // 检查是否是"设备在使用"错误
        if strings.Contains(err.Error(), "contains a filesystem in use") {
            log.G(ctx).WithError(err).WithField("lvName", lvName).
                Warn("Remove: LV still in use, will be cleaned up by GC")
            continue  // 不返回错误，让 GC 处理
        }
        // 其他错误才返回
        return err
    }
}
```

---

## 九、参考资料

- **Containerd GC**：`metadata/db.go:353-440`
- **Snapshotter Cleanup**：`metadata/snapshot.go:846-863`
- **Devbox Cleanup**：`snapshots/devbox/devbox.go:472-492`
- **Get Cleanup LV Names**：`snapshots/devbox/devbox.go:569-594`
- **WithSnapshotCleanup**：`container_opts.go:258-273`
- **RemoveContainer**：`pkg/cri/server/container_remove.go:34-135`

