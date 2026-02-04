# Devbox Snapshotter 代码分析与优化建议

## 一、当前代码架构分析

### 1.1 核心数据结构

**BoltDB 存储结构**：
```
bucketKeyStorageVersion (v1)
  └─ k8s.io
      ├─ snapshots (bucketKeySnapshot)
      │   └─ "<snapshot-key>" (如 "sha256:abc123...")
      │       ├─ id: 容器 ID
      │       ├─ kind: snapshot 类型
      │       ├─ parent: 父 snapshot
      │       ├─ DevboxKeyContentID: content hash
      │       └─ DevboxKeyPath: 挂载路径 ❌ 问题：从不删除
      │
      └─ devbox_storage_path (DevboxStoragePathBucket)
          └─ "<content-id>" (如 "sha256:abc123...")
              ├─ DevboxKeyLvName: LV 名称
              ├─ DevboxKeySnapshotKey: snapshot key ❌ 问题：在 Remove 时删除
              ├─ DevboxKeyStatus: active/removed
              └─ DevboxKeyPath: (历史遗留，不再使用)
```

### 1.2 状态流转

```
创建阶段：
  Prepare() → createSnapshot()
    ├─ 创建 LV (如果有 contentID 且 LV 存在，复用)
    ├─ 格式化文件系统
    ├─ 挂载到 mountPath
    └─ SetDevboxContent(key, contentID, lvName, mountPath)
        ├─ 写入 DevboxKeyContentID
        ├─ 写入 DevboxKeyPath ❌ 问题：从不删除
        ├─ 写入 DevboxKeyLvName
        ├─ 写入 DevboxKeySnapshotKey
        └─ 写入 DevboxKeyStatus = "active"

使用阶段：
  Mounts() → 返回挂载信息

卸载阶段（正常流程）：
  StopContainer/Exit → Update(unmountLvm="true")
    └─ SetUnmountedWithKey(key)
        ├─ 读取 DevboxKeyPath ✅ 能读取到
        ├─ 删除 DevboxKeySnapshotKey
        └─ 调用 unmountLvm(mountPath) ✅ 执行卸载

删除阶段（异常流程）：
  RemoveContainer → Remove(key)
    └─ RemoveDevbox(key)
        ├─ 读取 DevboxKeyPath
        ├─ 根据 DevboxKeyStatus 决定操作
        │   ├─ if "removed": 删除 content bucket
        │   └─ else: 删除 DevboxKeySnapshotKey
        └─ 返回 mountPath (可能为空 ❌)
            └─ if mountPath != "": unmountLvm() ❌ 可能不执行

清理阶段（兜底）：
  Cleanup()
    ├─ getCleanupLvNames(): 找出 "存在但不在数据库中" 的 LV
    ├─ 扫描 /proc/mounts，卸载所有残留的 LV
    └─ 删除 LV
```

---

## 二、核心问题分析

### 2.1 问题 1：职责不清，逻辑分散 ❌

**现象**：
- 卸载 LV 的逻辑分散在 3 个地方：
  1. `Update()` → `SetUnmountedWithKey()` → `unmountLvm()`
  2. `Remove()` → `RemoveDevbox()` → `unmountLvm()`
  3. `Cleanup()` → 扫描 `/proc/mounts` → `unmountLvm()`

**问题**：
- 没有明确的生命周期管理
- 每个函数都试图做"完整的事情"，导致逻辑重复
- `RemoveDevbox()` 既读取状态，又修改状态，职责混乱

**影响**：
- 代码难以理解和维护
- 容易出现遗漏（如 containerd 重启场景）
- 难以定位问题

---

### 2.2 问题 2：DevboxKeyPath 从不删除 ❌❌❌

**现象**：
```go
// SetUnmountedWithKey() 只删除 DevboxKeySnapshotKey
func SetUnmountedWithKey(ctx context.Context, key string) (string, error) {
    // ...
    sdbkt.Delete(DevboxKeySnapshotKey)  // ✅ 删除这个
    // ❌ 但是 DevboxKeyPath 没有被删除！
    return mountPath, nil
}
```

**影响**：
- Containerd 重启后，`DevboxKeyPath` 仍然存在
- 但无法判断 LV 是否还在挂载
- `Remove()` 函数无法通过 `mountPath` 判断是否需要卸载

**为什么这是核心问题**：
- 在正常流程中，`Update(unmountLvm="true")` 已经卸载了 LV
- 但 `DevboxKeyPath` 仍然保留
- Containerd 重启后，`Remove()` 再次被调用
- 此时 `mountPath` 不为空，但 LV 已经被卸载了
- 或者：`mountPath` 为空（因为 snapshot bucket 已被删除），LV 还在挂载

---

### 2.3 问题 3：状态管理不一致 ❌

**现象**：
```go
// RemoveDevbox 中的状态处理
if status := sdbkt.Get(DevboxKeyStatus); status != nil {
    if string(status) == string(DevboxStatusRemoved) {
        dbkt.DeleteBucket([]byte(contentID))
    } else {
        sdbkt.Delete(DevboxKeySnapshotKey)
    }
}
```

**问题**：
- `DevboxKeyStatus` 有 `active` 和 `removed` 两种状态
- 但 `removed` 状态从未被设置（`SetDevboxContentStatusRemoved` 函数存在，但从未被调用）
- 状态检查的逻辑在 `RemoveDevbox` 中，而不是在调用方

**影响**：
- 状态检查逻辑无效（永远不会进入 `if string(status) == "removed"` 分支）
- 状态管理混乱，不知道何时应该删除什么

---

### 2.4 问题 4：Remove 函数逻辑顺序错误 ⚠️

**当前逻辑**：
```go
func (o *Snapshotter) Remove(ctx context.Context, key string) error {
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 1. 获取 mountPath
        mountPath, err = storage.RemoveDevbox(ctx, key)

        // 2. 如果 mountPath 不为空，卸载
        if mountPath != "" {
            o.unmountLvm(ctx, mountPath)
        }

        // 3. 删除 snapshot metadata
        storage.Remove(ctx, key)

        // 4. 获取需要删除的 LV
        removedLvNames, err = o.getCleanupLvNames(ctx)

        return nil
    })()

    // 5. 在 defer 中删除 LV（事务外）
    for _, lvName := range removedLvNames {
        o.removeLv(ctx, lvName)
    }
}
```

**问题**：
1. **卸载和删除 LV 的时机不对**：
   - 卸载在事务内执行（可能阻塞事务）
   - 删除 LV 在事务外的 defer 中执行（无法回滚）

2. **状态检查在错误的函数中**：
   - `RemoveDevbox()` 中检查 `DevboxKeyStatus`
   - 但应该是 `Remove()` 来决定何时卸载、何时删除

3. **缺少关键的中间状态**：
   - 没有 "已卸载但未删除" 的状态
   - 无法区分 "正在使用"、"已卸载"、"已删除"

---

### 2.5 问题 5：getCleanupLvNames 的逻辑不完整 ⚠️

**当前逻辑**：
```go
func (o *Snapshotter) getCleanupLvNames(ctx context.Context) ([]string, error) {
    nameMap, err := storage.GetDevboxLvNames(ctx)  // 从 BoltDB 获取
    lvs, err := lvm.ListLVMLogicalVolumeByVG(ctx, o.lvmVgName, o.ThinPoolName)  // 从 LVM 获取

    cleanup := []string{}
    for _, d := range lvs {
        if _, ok := nameMap[d.Name]; ok {
            continue  // 在 BoltDB 中存在，跳过
        }
        if strings.HasPrefix(d.Name, "devbox") {
            cleanup = append(cleanup, d.Name)
        }
    }
    return cleanup, nil
}
```

**问题**：
- 只能清理"存在但不在数据库中"的 LV
- 无法清理"在数据库中，但容器已不存在"的 LV
- Containerd 重启后，BoltDB 中的 metadata 还在，但容器已经不存在

---

## 三、优化建议

### 3.1 建议 1：重新设计状态机 ✅✅✅

**目标**：明确的生命周期管理，清晰的状态流转

**新状态定义**：
```go
const (
    DevboxStatusActive   = "active"    // 使用中：LV 已挂载
    DevboxStatusMounted  = "mounted"   // 已挂载：LV 已挂载，容器已停止
    DevboxStatusUnmounted = "unmounted" // 已卸载：LV 已卸载，但未删除
    DevboxStatusRemoved  = "removed"   // 已删除：LV 已删除
)
```

**状态流转图**：
```
创建
  ↓
Active (使用中)
  ↓
容器停止 → Mounted (已挂载，等待卸载)
  ↓
Update(unmountLvm="true") → Unmounted (已卸载，等待删除)
  ↓
Remove() → Removed (已删除)
  ↓
Cleanup() → 物理删除 LV
```

---

### 3.2 建议 2：重构 Remove 函数 ✅✅✅

**新的逻辑顺序**：
```go
func (o *Snapshotter) Remove(ctx context.Context, key string) error {
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 1. 读取当前状态和 mountPath
        info, mountPath, err := storage.GetDevboxSnapshotInfo(ctx, key)
        if err != nil {
            return err
        }

        // 2. 根据状态决定操作
        switch info.Status {
        case DevboxStatusActive:
            // 容器还在使用中，不应该调用 Remove
            return fmt.Errorf("snapshot is still active")

        case DevboxStatusMounted:
            // LV 还在挂载，需要先卸载
            if mountPath != "" {
                if err := o.unmountLvm(ctx, mountPath); err != nil {
                    log.G(ctx).WithError(err).Warn("failed to unmount")
                }
            }
            // 更新状态为 Unmounted
            storage.SetDevboxStatus(ctx, key, DevboxStatusUnmounted)

        case DevboxStatusUnmounted:
            // 已卸载，删除 metadata
            storage.Remove(ctx, key)
            storage.SetDevboxStatus(ctx, key, DevboxStatusRemoved)

        case DevboxStatusRemoved:
            // 已删除，无需操作
            return nil
        }

        return nil
    })()
}
```

**优势**：
1. 状态驱动，逻辑清晰
2. 每个状态对应明确的操作
3. 可以从任意状态恢复（如 containerd 重启后）

---

### 3.3 建议 3：简化 RemoveDevbox 函数 ✅

**新的职责**：只读取信息，不修改状态

```go
// GetDevboxSnapshotInfo 获取 snapshot 的完整信息
func GetDevboxSnapshotInfo(ctx context.Context, key string) (*SnapshotInfo, string, error) {
    var info SnapshotInfo
    var mountPath string

    err := withDevboxBucket(ctx, func(ctx context.Context, bkt *bolt.Bucket, dbkt *bolt.Bucket) error {
        sbkt := bkt.Bucket([]byte(key))
        if sbkt == nil {
            return errdefs.ErrNotFound
        }

        // 读取基本信息
        info.ID = key
        info.ContentID = string(sbkt.Get(DevboxKeyContentID))
        info.Status = string(sbkt.Get(DevboxKeyStatus))
        mountPath = string(sbkt.Get(DevboxKeyPath))

        // 读取 LV 名称
        if len(info.ContentID) > 0 {
            sdbkt := dbkt.Bucket([]byte(info.ContentID))
            if sdbkt != nil {
                info.LvName = string(sdbkt.Get(DevboxKeyLvName))
            }
        }

        return nil
    })

    return &info, mountPath, err
}

type SnapshotInfo struct {
    ID        string
    ContentID string
    LvName    string
    Status    string
}
```

**优势**：
1. 单一职责：只读取信息
2. 调用方决定如何处理
3. 易于测试和理解

---

### 3.4 建议 4：在 Remove 前先检查并卸载 ✅

**逻辑**：
```go
func (o *Snapshotter) Remove(ctx context.Context, key string) error {
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 1. 获取 snapshot 信息
        info, mountPath, err := storage.GetDevboxSnapshotInfo(ctx, key)
        if err != nil {
            if errdefs.IsNotFound(err) {
                // Snapshot 不存在，直接返回
                return nil
            }
            return err
        }

        // 2. 检查 LV 是否还在挂载（通过 /proc/mounts）
        if info.LvName != "" {
            devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, info.LvName)
            mountPoints, _ := findMountPointByDevice(devicePath)
            if len(mountPoints) > 0 {
                // LV 还在挂载，先卸载
                for _, mp := range mountPoints {
                    if err := o.unmountLvm(ctx, mp); err != nil {
                        log.G(ctx).WithError(err).Warn("failed to unmount")
                    }
                }
            }
        }

        // 3. 删除 snapshot metadata
        if _, _, err := storage.Remove(ctx, key); err != nil {
            return err
        }

        // 4. 收集需要删除的 LV
        removedLvNames, err := o.getCleanupLvNames(ctx)
        if err != nil {
            return err
        }

        // 5. 在事务内删除 LV（或返回列表，在事务外删除）
        for _, lvName := range removedLvNames {
            if err := o.removeLv(ctx, lvName); err != nil {
                log.G(ctx).WithError(err).Warn("failed to remove LV")
            }
        }

        return nil
    })()
}
```

**优势**：
1. 先检查挂载状态，再决定是否卸载
2. 不依赖 `mountPath` 是否为空
3. 通过 `/proc/mounts` 获取真实状态

---

### 3.5 建议 5：增强 Cleanup 函数 ✅

**目标**：处理所有残留的 LV

```go
func (o *Snapshotter) Cleanup(ctx context.Context) error {
    log.G(ctx).Infof("Cleanup called")

    // 1. 清理不在数据库中的 LV
    orphanedLVs, err := o.getOrphanedLVs(ctx)
    if err != nil {
        log.G(ctx).WithError(err).Warn("failed to get orphaned LVs")
    } else {
        for _, lvName := range orphanedLVs {
            // 先卸载
            if err := o.unmountLvByName(ctx, lvName); err != nil {
                log.G(ctx).WithError(err).Warn("failed to unmount")
            }
            // 再删除
            if err := o.removeLv(ctx, lvName); err != nil {
                log.G(ctx).WithError(err).Warn("failed to remove LV")
            }
        }
    }

    // 2. 清理残留的 snapshot metadata
    if err := o.cleanupStaleSnapshots(ctx); err != nil {
        log.G(ctx).WithError(err).Warn("failed to cleanup stale snapshots")
    }

    // 3. 清理残留的目录
    cleanupDirs, _, err := o.cleanupDirectories(ctx)
    if err != nil {
        return err
    }
    for _, dir := range cleanupDirs {
        o.RemoveDir(ctx, dir)
    }

    return nil
}

// getOrphanedLVs 获取所有孤立的 LV（不在数据库中的 LV）
func (o *Snapshotter) getOrphanedLVs(ctx context.Context) ([]string, error) {
    // 1. 从数据库获取所有 LV
    nameMap, err := storage.GetDevboxLvNames(ctx)
    if err != nil {
        return nil, err
    }

    // 2. 从 LVM 获取所有 devbox LV
    lvs, err := lvm.ListLVMLogicalVolumeByVG(ctx, o.lvmVgName, o.ThinPoolName)
    if err != nil {
        return nil, err
    }

    // 3. 找出孤立 LV
    orphaned := []string{}
    for _, lv := range lvs {
        if strings.HasPrefix(lv.Name, "devbox") {
            if _, ok := nameMap[lv.Name]; !ok {
                orphaned = append(orphaned, lv.Name)
            }
        }
    }

    return orphaned, nil
}

// cleanupStaleSnapshots 清理残留的 snapshot metadata
func (o *Snapshotter) cleanupStaleSnapshots(ctx context.Context) error {
    // 遍历所有 devbox snapshot，如果对应的容器不存在，删除 metadata
    return storage.WalkDevboxSnapshots(ctx, func(key string, info *SnapshotInfo) error {
        // TODO: 检查容器是否还存在
        // 如果容器不存在，删除 metadata
        return nil
    })
}

// unmountLvByName 通过 LV 名称卸载
func (o *Snapshotter) unmountLvByName(ctx context.Context, lvName string) error {
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    mountPoints, err := findMountPointByDevice(devicePath)
    if err != nil {
        return err
    }

    for _, mp := range mountPoints {
        if err := o.unmountLvm(ctx, mp); err != nil {
            return err
        }
    }

    return nil
}
```

**优势**：
1. 主动扫描所有 devbox LV
2. 不依赖 metadata 是否完整
3. 兜底机制，确保不会有残留

---

### 3.6 建议 6：在 Prepare 时检查状态 ✅

**目标**：处理 containerd 重启后，LV 已经挂载的情况

```go
func (o *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
    // 1. 检查 snapshot 是否已存在
    info, mountPath, err := storage.GetDevboxSnapshotInfo(ctx, key)
    if err == nil {
        // Snapshot 已存在，检查状态
        if info.Status == DevboxStatusActive || info.Status == DevboxStatusMounted {
            // LV 应该已经挂载，检查是否真的在挂载
            if info.LvName != "" {
                devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, info.LvName)
                mountPoints, _ := findMountPointByDevice(devicePath)
                if len(mountPoints) > 0 {
                    // LV 已经挂载，直接返回
                    log.G(ctx).Infof("Snapshot %s already mounted at %s", key, mountPoints[0])
                    return o.mounts(storage.Snapshot{
                        ID:        info.ID,
                        ParentIDs: []string{parent},
                        Kind:      snapshots.KindActive,
                    }), nil
                }
            }
        }
    }

    // 2. 正常创建流程
    // ...
}
```

**优势**：
1. 可以处理重启后的恢复
2. 避免重复创建 LV
3. 幂等性保证

---

## 四、重构后的完整流程

### 4.1 创建流程

```
Prepare(key)
  ↓
检查 snapshot 是否已存在
  ├─ 不存在 → 创建 LV → 挂载 → 设置状态 = Active
  └─ 存在 → 检查 LV 是否挂载
      ├─ 已挂载 → 直接返回
      └─ 未挂载 → 重新挂载 → 设置状态 = Active
```

### 4.2 停止流程

```
StopContainer/Exit → Update(unmountLvm="true")
  ↓
读取 mountPath
  ↓
卸载 LV
  ↓
设置状态 = Unmounted
```

### 4.3 删除流程

```
Remove(key)
  ↓
读取 snapshot 信息
  ↓
根据状态决定操作：
  ├─ Active → 错误（不应该删除）
  ├─ Mounted → 卸载 → 设置状态 = Unmounted
  ├─ Unmounted → 删除 metadata → 设置状态 = Removed
  └─ Removed → 无操作
  ↓
收集需要删除的 LV
  ↓
删除 LV
```

### 4.4 清理流程

```
Cleanup()
  ↓
扫描所有 devbox LV
  ↓
对于每个 LV：
  ├─ 检查是否在数据库中
  │   ├─ 在 → 跳过
  │   └─ 不在 → 孤立 LV，卸载并删除
  ↓
扫描所有 snapshot metadata
  ↓
对于每个 snapshot：
  ├─ 检查容器是否还存在
  │   ├─ 在 → 跳过
  │   └─ 不在 → 删除 metadata，卸载并删除 LV
```

---

## 五、总结

### 5.1 核心问题

1. **职责不清**：逻辑分散在多个函数中
2. **状态管理混乱**：没有明确的状态机
3. **依赖不正确的信息**：`DevboxKeyPath` 从不删除
4. **缺少兜底机制**：Cleanup 不够完善

### 5.2 优化方向

1. **引入明确的状态机**：Active → Mounted → Unmounted → Removed
2. **职责分离**：读取信息和修改状态分离
3. **先检查再操作**：不依赖 metadata，而是检查实际状态（/proc/mounts）
4. **增强 Cleanup**：扫描所有 LV，清理残留
5. **幂等性保证**：每个操作都可以安全地重试

### 5.3 实施建议

**优先级**：
1. **高优先级**：建议 3（简化 RemoveDevbox）+ 建议 4（先检查再卸载）
2. **中优先级**：建议 5（增强 Cleanup）
3. **低优先级**：建议 1（重新设计状态机，需要大的重构）

**渐进式重构**：
1. 先修复当前的问题（Remove 函数中的卸载逻辑）
2. 再增强 Cleanup 作为兜底
3. 最后考虑引入完整的状态机

---

## 六、具体代码示例

### 6.1 修复 Remove 函数（最小改动）

```go
func (o *Snapshotter) Remove(ctx context.Context, key string) (err error) {
    var (
        removals       []string
        removedLvNames []string
    )

    log.G(ctx).Warnf("Remove called with key: %s", key)

    defer func() {
        if err == nil {
            for _, dir := range removals {
                o.RemoveDir(ctx, dir)
            }
            for _, lvName := range removedLvNames {
                err := o.removeLv(ctx, lvName)
                if err != nil {
                    log.G(ctx).WithError(err).WithField("lvName", lvName).Warn("Remove: failed to destroy LVM logical volume")
                    continue
                }
                log.G(ctx).Infof("Remove: LVM logical volume %s removed successfully", lvName)
            }
        }
    }()

    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 1. 获取 snapshot 信息
        info, mountPath, err := storage.GetDevboxSnapshotInfo(ctx, key)
        if err != nil && err != errdefs.ErrNotFound {
            return fmt.Errorf("failed to get devbox snapshot info: %w", err)
        }

        log.G(ctx).WithFields(logrus.Fields{
            "key": key,
            "mountPath": mountPath,
            "lvName": info.LvName,
            "contentID": info.ContentID,
        }).Warnf("Remove: got snapshot info")

        // 2. 如果 LV 存在，检查是否还在挂载
        if info.LvName != "" {
            devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, info.LvName)
            mountPoints, merr := findMountPointByDevice(devicePath)
            if merr != nil {
                log.G(ctx).WithError(merr).WithField("devicePath", devicePath).Warn("Remove: failed to find mount point")
            } else if len(mountPoints) > 0 {
                // LV 还在挂载，先卸载
                log.G(ctx).WithFields(logrus.Fields{
                    "lvName": info.LvName,
                    "mountPoints": mountPoints,
                }).Warnf("Remove: LV still mounted, unmounting")
                for _, mp := range mountPoints {
                    if err = o.unmountLvm(ctx, mp); err != nil {
                        log.G(ctx).WithError(err).WithField("path", mp).Warn("Remove: failed to unmount")
                    } else {
                        log.G(ctx).WithField("path", mp).Infof("Remove: successfully unmounted")
                    }
                }
            } else {
                log.G(ctx).WithField("lvName", info.LvName).Infof("Remove: LV not mounted")
            }
        }

        // 3. 删除 snapshot metadata
        _, _, err = storage.Remove(ctx, key)
        if err != nil {
            return fmt.Errorf("failed to remove snapshot %s: %w", key, err)
        }

        // 4. 收集需要删除的 LV
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
    })()
}
```

**关键改进**：
1. 不依赖 `mountPath` 是否为空
2. 通过 LV 名称检查实际挂载状态
3. 先卸载，再删除 metadata
4. 添加详细日志

### 6.2 新增 GetDevboxSnapshotInfo 函数

```go
// SnapshotInfo 包含 snapshot 的完整信息
type SnapshotInfo struct {
    ID        string
    ContentID string
    LvName    string
    Status    string
}

// GetDevboxSnapshotInfo 获取 snapshot 的完整信息和挂载路径
func GetDevboxSnapshotInfo(ctx context.Context, key string) (*SnapshotInfo, string, error) {
    var info SnapshotInfo
    var mountPath string

    err := withDevboxBucket(ctx, func(ctx context.Context, bkt *bolt.Bucket, dbkt *bolt.Bucket) error {
        sbkt := bkt.Bucket([]byte(key))
        if sbkt == nil {
            return errdefs.ErrNotFound
        }

        // 读取基本信息
        info.ID = key
        contentID := sbkt.Get(DevboxKeyContentID)
        if contentID != nil {
            info.ContentID = string(contentID)
        }

        status := sbkt.Get(DevboxKeyStatus)
        if status != nil {
            info.Status = string(status)
        } else {
            info.Status = DevboxStatusActive // 默认状态
        }

        mountPathBytes := sbkt.Get(DevboxKeyPath)
        if mountPathBytes != nil {
            mountPath = string(mountPathBytes)
        }

        // 读取 LV 名称
        if len(info.ContentID) > 0 {
            sdbkt := dbkt.Bucket([]byte(info.ContentID))
            if sdbkt != nil {
                lvName := sdbkt.Get(DevboxKeyLvName)
                if lvName != nil {
                    info.LvName = string(lvName)
                }
            }
        }

        return nil
    })

    if err != nil {
        return nil, "", err
    }

    return &info, mountPath, nil
}
```

---

## 七、验证方法

### 7.1 单元测试

```go
func TestRemoveWithMountedLV(t *testing.T) {
    // 1. 创建 snapshot 并挂载 LV
    // 2. 模拟 containerd 重启（不卸载 LV）
    // 3. 调用 Remove
    // 4. 验证 LV 被卸载和删除
}

func TestRemoveWithUnmountedLV(t *testing.T) {
    // 1. 创建 snapshot 并挂载 LV
    // 2. 卸载 LV
    // 3. 调用 Remove
    // 4. 验证 LV 被删除（无需重复卸载）
}

func TestRemoveWithOrphanedLV(t *testing.T) {
    // 1. 创建 LV 并挂载
    // 2. 删除 snapshot metadata（模拟 metadata 损坏）
    // 3. 调用 Cleanup
    // 4. 验证 LV 被卸载和删除
}
```

### 7.2 集成测试

```bash
# 1. 创建容器
crictl run <pod-config>

# 2. 重启 containerd
systemctl restart containerd

# 3. 删除容器
crictl stop <container-id>
crictl rm <container-id>

# 4. 检查 LV 是否被删除
lvs | grep devbox

# 5. 检查挂载点
mount | grep devbox
```

---

## 八、参考资料

- 当前代码：`/root/containerd1.7/snapshots/devbox/devbox.go`
- 存储层：`/root/containerd1.7/snapshots/devbox/storage/bolt.go`
- 文档：`/root/containerd1.7/docs/lzc/`
