# Snapshotter 状态机实现方案

## 一、简化的状态定义

### 1.1 去掉 formatting/formatted 状态

**原因**：
- 格式化操作很快（几秒），不需要单独状态
- 格式化和挂载可以合并为一个原子操作
- 简化状态机，降低复杂度

### 1.2 最终状态定义

#### 稳定状态

```go
const (
    // LVStateNone 表示 LV 不存在（无记录）
    LVStateNone = ""
    
    // LVStateCreated 表示 LV 已创建（设备存在，可能已格式化或未格式化）
    LVStateCreated = "created"
    
    // LVStateMounted 表示 LV 已挂载（可以使用）
    LVStateMounted = "mounted"
    
    // LVStateUnmounted 表示 LV 已卸载（设备存在但未挂载）
    LVStateUnmounted = "unmounted"
    
    // LVStateRemoved 表示 LV 已删除（待清理元数据）
    LVStateRemoved = "removed"
)
```

#### 中间状态

```go
const (
    // LVStateCreating 表示正在创建 LV
    LVStateCreating = "creating"
    
    // LVStateMounting 表示正在挂载 LV（包括格式化）
    LVStateMounting = "mounting"
    
    // LVStateUnmounting 表示正在卸载 LV
    LVStateUnmounting = "unmounting"
    
    // LVStateRemoving 表示正在删除 LV
    LVStateRemoving = "removing"
)
```

#### 错误状态

```go
const (
    // LVStateFaulty 表示 LV 处于错误状态（需要人工介入）
    LVStateFaulty = "faulty"
)
```

### 1.3 状态转换图（简化版）

```
完整生命周期：
none → creating → created → mounting → mounted → unmounting → unmounted → removing → removed
  ↑       ↓失败       ↑       ↓失败       ↑          ↓失败          ↑          ↓失败
  └───────┴───────────┴───────┴───────────┴──────────┴──────────────┴──────────┴────→ faulty
                                                                                    (仅在无法回退时)
```

---

## 二、Snapshotter 接口与状态的对应

### 2.1 Prepare() - 创建活动快照

**目标**：创建并挂载 LV，供容器使用

```go
func (o *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error)
```

**状态转换**：
```
none → creating → created → mounting → mounted
```

**可能遇到的状态**：

| 当前状态 | 说明 | 处理方式 |
|---------|------|---------|
| none | LV 不存在（正常） | 从头开始创建 |
| creating | 上次创建被中断 | 检查 LV 是否存在，决定继续或重新创建 |
| created | 上次创建完成但挂载失败 | 跳过创建，直接挂载 |
| mounting | 上次挂载被中断 | 检查是否已挂载，决定继续或重新挂载 |
| mounted | 上次挂载成功（或未清理） | 检查挂载点，直接返回 |
| faulty | 之前失败且无法恢复 | 尝试修复或返回错误 |

---

### 2.2 Remove() - 删除快照

**目标**：卸载并删除 LV

```go
func (o *Snapshotter) Remove(ctx context.Context, key string) error
```

**状态转换**：
```
mounted → unmounting → unmounted → removing → removed
```

**可能遇到的状态**：

| 当前状态 | 说明 | 处理方式 |
|---------|------|---------|
| mounted | LV 已挂载（正常） | 执行卸载和删除 |
| unmounting | 上次卸载被中断 | 检查是否已卸载，决定继续或重新卸载 |
| unmounted | 上次卸载成功但删除失败 | 跳过卸载，直接删除 |
| removing | 上次删除被中断 | 检查 LV 是否存在，决定继续或完成 |
| created | LV 从未挂载 | 跳过卸载，直接删除 |
| faulty | 之前失败且无法恢复 | 尝试强制删除 |

---

## 三、具体实现代码

### 3.1 LVInfo 结构体

```go
// LVInfo 存储 LV 的元数据
type LVInfo struct {
    Name         string    // LV 名称
    ContentKey   string    // 对应的 snapshot key
    State        string    // 当前状态
    PrevState    string    // 前一个状态（用于回退）
    MountPoint   string    // 挂载点
    Capacity     string    // 容量
    Error        string    // 最后的错误信息
    ErrorTime    time.Time // 错误时间
    RetryCount   int       // 重试次数
    CreatedTime  time.Time // 创建时间
    UpdatedTime  time.Time // 更新时间
}
```

### 3.2 状态查询和更新

```go
// getLVState 查询 LV 的当前状态
func (o *Snapshotter) getLVState(ctx context.Context, lvName string) (string, *LVInfo, error) {
    var state string
    var info *LVInfo
    
    err := o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
        lv, err := storage.GetLV(ctx, lvName)
        if err != nil {
            if errors.Is(err, errdefs.ErrNotFound) {
                state = LVStateNone
                return nil
            }
            return err
        }
        state = lv.State
        info = lv
        return nil
    })
    
    return state, info, err
}

// setLVState 更新 LV 状态
func (o *Snapshotter) setLVState(ctx context.Context, lvName string, newState string) error {
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLVState(ctx, lvName, newState, time.Now())
    })
}

// rollbackLVState 回退 LV 状态（失败时使用）
func (o *Snapshotter) rollbackLVState(ctx context.Context, lvName string, prevState string, err error) error {
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        storage.UpdateLVState(ctx, lvName, prevState, time.Now())
        storage.SaveLVError(ctx, lvName, err.Error(), time.Now())
        storage.IncrementRetryCount(ctx, lvName)
        return nil
    })
}
```

### 3.3 Prepare() 的状态机实现

```go
func (o *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
    lvName := "devbox-" + key
    
    // ========== 步骤 1：检查当前状态 ==========
    currentState, lvInfo, err := o.getLVState(ctx, lvName)
    if err != nil {
        return nil, fmt.Errorf("failed to get LV state: %w", err)
    }
    
    log.G(ctx).Infof("Prepare: LV %s current state: %s", lvName, currentState)
    
    // ========== 步骤 2：根据状态决定操作 ==========
    switch currentState {
    case LVStateNone:
        // 正常流程：从头开始创建
        return o.prepareFromScratch(ctx, key, parent, opts...)
        
    case LVStateCreating:
        // 上次创建被中断，检查 LV 是否实际存在
        return o.prepareFromCreating(ctx, key, parent, lvInfo, opts...)
        
    case LVStateCreated:
        // 上次创建完成但挂载失败，直接挂载
        return o.prepareFromCreated(ctx, key, lvInfo, opts...)
        
    case LVStateMounting:
        // 上次挂载被中断，检查是否已挂载
        return o.prepareFromMounting(ctx, key, lvInfo, opts...)
        
    case LVStateMounted:
        // 已挂载（可能是重复调用或未清理），检查挂载点
        return o.prepareFromMounted(ctx, key, lvInfo, opts...)
        
    case LVStateFaulty:
        // 之前失败，尝试修复
        return o.prepareFromFaulty(ctx, key, parent, lvInfo, opts...)
        
    default:
        return nil, fmt.Errorf("unexpected LV state: %s", currentState)
    }
}
```

### 3.4 各状态的具体处理

#### 从头开始创建（none → mounted）

```go
func (o *Snapshotter) prepareFromScratch(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
    lvName := "devbox-" + key
    log.G(ctx).Info("Prepare: Starting from scratch")
    
    // ========== 创建 snapshot 元数据 ==========
    var snapID string
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        snap, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, parent, opts...)
        if err != nil {
            return err
        }
        snapID = snap.ID
        
        // 同时创建 LV 元数据，标记为 creating
        return storage.AddLV(ctx, &LVInfo{
            Name:        lvName,
            ContentKey:  key,
            State:       LVStateCreating,
            PrevState:   LVStateNone,
            Capacity:    getCapacity(opts...),
            CreatedTime: time.Now(),
        })
    })
    if err != nil {
        return nil, fmt.Errorf("failed to create snapshot metadata: %w", err)
    }
    
    // ========== 创建 LV（事务外，带回退） ==========
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{Capacity: getCapacity(opts...), VolGroup: o.lvmVgName},
    }
    
    createErr := lvm.CreateVolume(ctx, vol)
    if createErr != nil {
        log.G(ctx).WithError(createErr).Error("Prepare: Failed to create LV")
        
        // 回退：删除 metadata
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            storage.Remove(ctx, key)
            storage.RemoveLV(ctx, lvName)
            return nil
        })
        
        // 尝试清理可能的残留
        _ = lvm.ForceDestroyVolume(ctx, vol)
        
        return nil, fmt.Errorf("failed to create LV: %w", createErr)
    }
    
    // ========== 标记为 created ==========
    o.setLVState(ctx, lvName, LVStateCreated)
    
    // ========== 继续挂载 ==========
    return o.mountLV(ctx, key, lvName, snapID)
}
```

#### 从 creating 状态恢复

```go
func (o *Snapshotter) prepareFromCreating(ctx context.Context, key, parent string, lvInfo *LVInfo, opts ...snapshots.Opt) ([]mount.Mount, error) {
    lvName := lvInfo.Name
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    
    log.G(ctx).Warn("Prepare: Recovering from creating state")
    
    // 检查 LV 是否实际存在
    deviceExists := fileExists(devicePath)
    
    if deviceExists {
        // LV 已存在，更新状态为 created，继续挂载
        log.G(ctx).Info("Prepare: LV exists, updating state to created")
        o.setLVState(ctx, lvName, LVStateCreated)
        
        var snapID string
        o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
            snap, _ := storage.Get(ctx, key)
            snapID = snap.ID
            return nil
        })
        
        return o.mountLV(ctx, key, lvName, snapID)
    } else {
        // LV 不存在，清理 metadata，重新创建
        log.G(ctx).Warn("Prepare: LV doesn't exist, cleaning up and recreating")
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            storage.Remove(ctx, key)
            storage.RemoveLV(ctx, lvName)
            return nil
        })
        
        return o.prepareFromScratch(ctx, key, parent, opts...)
    }
}
```

#### 从 created 状态恢复

```go
func (o *Snapshotter) prepareFromCreated(ctx context.Context, key string, lvInfo *LVInfo, opts ...snapshots.Opt) ([]mount.Mount, error) {
    lvName := lvInfo.Name
    
    log.G(ctx).Info("Prepare: Recovering from created state, will mount")
    
    var snapID string
    o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
        snap, _ := storage.Get(ctx, key)
        snapID = snap.ID
        return nil
    })
    
    // 直接挂载
    return o.mountLV(ctx, key, lvName, snapID)
}
```

#### 从 mounting 状态恢复

```go
func (o *Snapshotter) prepareFromMounting(ctx context.Context, key string, lvInfo *LVInfo, opts ...snapshots.Opt) ([]mount.Mount, error) {
    lvName := lvInfo.Name
    mountPoint := o.getMountPoint(key)
    
    log.G(ctx).Warn("Prepare: Recovering from mounting state")
    
    // 检查是否已经挂载
    isMounted, _ := lvm.IsMountPoint(mountPoint)
    
    if isMounted {
        // 已挂载，更新状态为 mounted
        log.G(ctx).Info("Prepare: Already mounted, updating state")
        o.setLVState(ctx, lvName, LVStateMounted)
        
        return []mount.Mount{{
            Type:    "bind",
            Source:  mountPoint,
            Options: []string{"rbind"},
        }}, nil
    } else {
        // 未挂载，回退到 created，重新挂载
        log.G(ctx).Warn("Prepare: Not mounted, rolling back to created")
        o.setLVState(ctx, lvName, LVStateCreated)
        
        var snapID string
        o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
            snap, _ := storage.Get(ctx, key)
            snapID = snap.ID
            return nil
        })
        
        return o.mountLV(ctx, key, lvName, snapID)
    }
}
```

#### 从 mounted 状态恢复

```go
func (o *Snapshotter) prepareFromMounted(ctx context.Context, key string, lvInfo *LVInfo, opts ...snapshots.Opt) ([]mount.Mount, error) {
    mountPoint := o.getMountPoint(key)
    
    log.G(ctx).Warn("Prepare: Already mounted (possible duplicate call)")
    
    // 检查挂载点是否真的已挂载
    isMounted, _ := lvm.IsMountPoint(mountPoint)
    
    if isMounted {
        // 确实已挂载，直接返回
        log.G(ctx).Info("Prepare: Mount point valid, returning")
        return []mount.Mount{{
            Type:    "bind",
            Source:  mountPoint,
            Options: []string{"rbind"},
        }}, nil
    } else {
        // 状态不一致，回退到 created，重新挂载
        log.G(ctx).Warn("Prepare: Mount point invalid, remounting")
        o.setLVState(ctx, lvName, LVStateCreated)
        
        var snapID string
        o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
            snap, _ := storage.Get(ctx, key)
            snapID = snap.ID
            return nil
        })
        
        return o.mountLV(ctx, key, lvName, snapID)
    }
}
```

#### 从 faulty 状态恢复

```go
func (o *Snapshotter) prepareFromFaulty(ctx context.Context, key, parent string, lvInfo *LVInfo, opts ...snapshots.Opt) ([]mount.Mount, error) {
    lvName := lvInfo.Name
    
    log.G(ctx).Warnf("Prepare: Attempting to recover from faulty state (retry count: %d)", lvInfo.RetryCount)
    
    // 检查重试次数
    if lvInfo.RetryCount >= 5 {
        return nil, fmt.Errorf("LV %s has failed too many times (%d), manual intervention required", lvName, lvInfo.RetryCount)
    }
    
    // 检查实际状态
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    deviceExists := fileExists(devicePath)
    mountPoint := o.getMountPoint(key)
    isMounted, _ := lvm.IsMountPoint(mountPoint)
    
    // 根据实际状态决定从哪里恢复
    if isMounted {
        // 实际已挂载，更新状态
        log.G(ctx).Info("Prepare: Faulty LV is actually mounted, updating state")
        o.setLVState(ctx, lvName, LVStateMounted)
        return []mount.Mount{{
            Type:    "bind",
            Source:  mountPoint,
            Options: []string{"rbind"},
        }}, nil
    } else if deviceExists {
        // LV 存在但未挂载，标记为 created 并挂载
        log.G(ctx).Info("Prepare: Faulty LV exists but not mounted, will mount")
        o.setLVState(ctx, lvName, LVStateCreated)
        
        var snapID string
        o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
            snap, _ := storage.Get(ctx, key)
            snapID = snap.ID
            return nil
        })
        
        return o.mountLV(ctx, key, lvName, snapID)
    } else {
        // LV 不存在，清理后重新创建
        log.G(ctx).Warn("Prepare: Faulty LV doesn't exist, recreating")
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            storage.Remove(ctx, key)
            storage.RemoveLV(ctx, lvName)
            return nil
        })
        
        return o.prepareFromScratch(ctx, key, parent, opts...)
    }
}
```

### 3.5 挂载 LV 的统一函数

```go
func (o *Snapshotter) mountLV(ctx context.Context, key, lvName, snapID string) ([]mount.Mount, error) {
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    mountPoint := o.getMountPoint(key)
    
    // ========== 标记为 mounting ==========
    err := o.setLVState(ctx, lvName, LVStateMounting)
    if err != nil {
        return nil, fmt.Errorf("failed to mark as mounting: %w", err)
    }
    
    // ========== 确保挂载点存在 ==========
    if err := os.MkdirAll(mountPoint, 0755); err != nil {
        o.rollbackLVState(ctx, lvName, LVStateCreated, err)
        return nil, fmt.Errorf("failed to create mount point: %w", err)
    }
    
    // ========== 格式化（如果需要）==========
    needFormat, err := o.needsFormat(devicePath)
    if err != nil {
        o.rollbackLVState(ctx, lvName, LVStateCreated, err)
        return nil, fmt.Errorf("failed to check filesystem: %w", err)
    }
    
    if needFormat {
        log.G(ctx).Info("Prepare: Formatting LV")
        formatErr := o.formatWithRetry(ctx, devicePath, 3)
        if formatErr != nil {
            log.G(ctx).WithError(formatErr).Error("Prepare: Failed to format LV")
            o.rollbackLVState(ctx, lvName, LVStateCreated, formatErr)
            return nil, fmt.Errorf("failed to format LV: %w", formatErr)
        }
    }
    
    // ========== 挂载（带重试）==========
    mountErr := o.mountWithRetry(ctx, devicePath, mountPoint, 3)
    if mountErr != nil {
        log.G(ctx).WithError(mountErr).Error("Prepare: Failed to mount LV")
        o.rollbackLVState(ctx, lvName, LVStateCreated, mountErr)
        return nil, fmt.Errorf("failed to mount LV: %w", mountErr)
    }
    
    // ========== 标记为 mounted ==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        storage.UpdateLVState(ctx, lvName, LVStateMounted, time.Now())
        storage.SetLVMountPoint(ctx, lvName, mountPoint)
        storage.ClearLVError(ctx, lvName)  // 清除之前的错误
        return nil
    })
    
    log.G(ctx).Infof("Prepare: Successfully mounted LV %s to %s", lvName, mountPoint)
    
    return []mount.Mount{{
        Type:    "bind",
        Source:  mountPoint,
        Options: []string{"rbind"},
    }}, nil
}

// needsFormat 检查是否需要格式化
func (o *Snapshotter) needsFormat(devicePath string) (bool, error) {
    // 使用 blkid 检查是否已有文件系统
    cmd := exec.Command("blkid", devicePath)
    output, err := cmd.CombinedOutput()
    if err != nil {
        // 如果 blkid 返回错误，通常意味着没有文件系统
        return true, nil
    }
    // 如果有输出，说明已有文件系统
    return len(output) == 0, nil
}

// formatWithRetry 格式化设备（带重试）
func (o *Snapshotter) formatWithRetry(ctx context.Context, devicePath string, maxRetries int) error {
    var lastErr error
    for i := 0; i < maxRetries; i++ {
        cmd := exec.CommandContext(ctx, "mkfs.ext4", "-F", devicePath)
        output, err := cmd.CombinedOutput()
        if err == nil {
            return nil
        }
        lastErr = fmt.Errorf("%w: %s", err, string(output))
        log.G(ctx).Warnf("Format failed (attempt %d/%d): %v", i+1, maxRetries, lastErr)
        time.Sleep(time.Second * time.Duration(i+1))
    }
    return lastErr
}

// mountWithRetry 挂载设备（带重试）
func (o *Snapshotter) mountWithRetry(ctx context.Context, devicePath, mountPoint string, maxRetries int) error {
    var lastErr error
    for i := 0; i < maxRetries; i++ {
        lastErr = syscall.Mount(devicePath, mountPoint, "ext4", 0, "")
        if lastErr == nil {
            return nil
        }
        log.G(ctx).Warnf("Mount failed (attempt %d/%d): %v", i+1, maxRetries, lastErr)
        time.Sleep(time.Second * time.Duration(i+1))
    }
    return lastErr
}
```

---

## 四、Remove() 的状态机实现

```go
func (o *Snapshotter) Remove(ctx context.Context, key string) error {
    lvName := "devbox-" + key
    
    // ========== 步骤 1：检查当前状态 ==========
    currentState, lvInfo, err := o.getLVState(ctx, lvName)
    if err != nil {
        return fmt.Errorf("failed to get LV state: %w", err)
    }
    
    log.G(ctx).Infof("Remove: LV %s current state: %s", lvName, currentState)
    
    // ========== 步骤 2：根据状态决定操作 ==========
    switch currentState {
    case LVStateNone:
        // LV 不存在，可能已被删除，只需清理 snapshot 元数据
        return o.removeSnapshotMetadata(ctx, key)
        
    case LVStateMounted:
        // 正常流程：卸载 -> 删除
        return o.removeFromMounted(ctx, key, lvInfo)
        
    case LVStateUnmounting:
        // 上次卸载被中断，检查是否已卸载
        return o.removeFromUnmounting(ctx, key, lvInfo)
        
    case LVStateUnmounted, LVStateCreated:
        // 已卸载或从未挂载，直接删除
        return o.removeFromUnmounted(ctx, key, lvInfo)
        
    case LVStateRemoving:
        // 上次删除被中断，检查 LV 是否存在
        return o.removeFromRemoving(ctx, key, lvInfo)
        
    case LVStateFaulty:
        // 强制删除
        return o.removeFromFaulty(ctx, key, lvInfo)
        
    default:
        return fmt.Errorf("unexpected LV state: %s", currentState)
    }
}
```

### 4.1 各状态的删除处理

```go
func (o *Snapshotter) removeFromMounted(ctx context.Context, key string, lvInfo *LVInfo) error {
    lvName := lvInfo.Name
    mountPoint := lvInfo.MountPoint
    
    // ========== 标记为 unmounting ==========
    o.setLVState(ctx, lvName, LVStateUnmounting)
    
    // ========== 卸载（带重试）==========
    unmountErr := o.unmountWithRetry(ctx, mountPoint, 3)
    if unmountErr != nil {
        log.G(ctx).WithError(unmountErr).Error("Remove: Failed to unmount LV")
        o.rollbackLVState(ctx, lvName, LVStateMounted, unmountErr)
        return fmt.Errorf("failed to unmount LV: %w", unmountErr)
    }
    
    // ========== 标记为 unmounted ==========
    o.setLVState(ctx, lvName, LVStateUnmounted)
    
    // ========== 继续删除 ==========
    return o.deleteLV(ctx, key, lvName)
}

func (o *Snapshotter) removeFromUnmounted(ctx context.Context, key string, lvInfo *LVInfo) error {
    log.G(ctx).Info("Remove: LV already unmounted, will delete")
    return o.deleteLV(ctx, key, lvInfo.Name)
}

func (o *Snapshotter) deleteLV(ctx context.Context, key, lvName string) error {
    // ========== 标记为 removing ==========
    o.setLVState(ctx, lvName, LVStateRemoving)
    
    // ========== 删除 LV（带重试）==========
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{VolGroup: o.lvmVgName},
    }
    
    deleteErr := o.deleteLVWithRetry(ctx, vol, 3)
    if deleteErr != nil {
        log.G(ctx).WithError(deleteErr).Error("Remove: Failed to delete LV")
        o.rollbackLVState(ctx, lvName, LVStateUnmounted, deleteErr)
        return fmt.Errorf("failed to delete LV: %w", deleteErr)
    }
    
    // ========== 标记为 removed ==========
    o.setLVState(ctx, lvName, LVStateRemoved)
    
    // ========== 清理元数据 ==========
    return o.cleanupMetadata(ctx, key, lvName)
}

func (o *Snapshotter) deleteLVWithRetry(ctx context.Context, vol *apis.LVMVolume, maxRetries int) error {
    var lastErr error
    for i := 0; i < maxRetries; i++ {
        lastErr = lvm.DestroyVolume(ctx, vol)
        if lastErr == nil {
            return nil
        }
        log.G(ctx).Warnf("Delete LV failed (attempt %d/%d): %v", i+1, maxRetries, lastErr)
        time.Sleep(time.Second * time.Duration(i+1))
    }
    return lastErr
}
```

---

## 五、总结

### 5.1 简化后的优势

1. **状态更少，更易理解**
   - 去掉 formatting/formatted 状态
   - 格式化合并到挂载流程中

2. **重试场景处理清晰**
   - 检查当前状态
   - 根据实际情况决定从哪里继续
   - 避免重复操作

3. **回退机制完善**
   - 每个操作失败都立即回退
   - 不依赖重启或 Cleanup

### 5.2 关键设计点

1. **状态检查优先**
   - 每个接口调用先检查当前状态
   - 根据状态选择处理路径

2. **实际状态验证**
   - 不仅看 metadata 状态
   - 还要检查实际状态（LV 是否存在、是否已挂载）

3. **幂等性保证**
   - 相同的调用多次执行结果一致
   - 对于已完成的状态，直接返回

4. **错误恢复**
   - 立即重试（3 次）
   - 立即回退到稳定状态
   - faulty 状态仅用于无法自动恢复的情况

### 5.3 实施建议

**优先级 1**（必须）：
1. 实现基本状态转换（Prepare, Remove）
2. 实现重试机制（3 次）
3. 实现回退机制

**优先级 2**（重要）：
4. 实现状态恢复（从中间状态恢复）
5. 实现 faulty 状态处理
6. 添加日志和监控

**优先级 3**（可选）：
7. 优化重试策略（指数退避）
8. 添加超时控制
9. 实现人工介入工具

