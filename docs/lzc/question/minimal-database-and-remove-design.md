# 最小数据库结构 + Remove 操作设计

## 一、最小数据库结构设计

### 1.1 字段分类

#### 必须字段（核心功能）

```go
type LVInfo struct {
    // ========== 核心标识（必须）==========
    Name         string    // LV 名称：devbox-{contentID}
    ContentID    string    // devbox content ID
    
    // ========== 状态管理（必须）==========
    State        string    // 当前状态：creating, created, mounting, mounted, unmounting, removing
    
    // ========== 挂载信息（必须）==========
    MountPoint   string    // 挂载点路径
    IsFormatted  bool      // 是否已格式化（避免重复格式化）
    
    // ========== LV 属性（必须）==========
    Capacity     string    // 容量
}
```

**说明**：
- `Name`: 必须，LV 的唯一标识
- `ContentID`: 必须，关联 devbox
- `State`: 必须，状态机的核心
- `MountPoint`: 必须，卸载时需要
- `IsFormatted`: 必须，区分首次挂载和后续挂载
- `Capacity`: 必须，扩容时需要

---

#### 推荐字段（调试和追踪）

```go
type LVInfo struct {
    // ... 必须字段 ...
    
    // ========== 容器追踪（推荐）==========
    CurrentKey   string    // 当前使用的 snapshot key
    
    // ========== 错误记录（推荐）==========
    LastError    string    // 最后的错误信息（用于调试和告警）
    ErrorTime    time.Time // 错误时间
}
```

**说明**：
- `CurrentKey`: 推荐，知道当前哪个容器在使用，方便调试
- `LastError`: 推荐，失败时记录错误信息，方便排查
- `ErrorTime`: 推荐，配合 LastError 使用

---

#### 可选字段（高级功能）

```go
type LVInfo struct {
    // ... 必须字段 + 推荐字段 ...
    
    // ========== 引用计数（可选，如果支持多容器复用）==========
    RefCount       int      // 引用计数
    LastKey        string   // 上一个 snapshot key
    
    // ========== 状态恢复（可选，启动时检测遗留状态）==========
    PrevState          string    // 前一个状态（用于回退）
    OperationStartTime time.Time // 操作开始时间（检测遗留状态）
    
    // ========== 重试控制（可选，避免无限重试）==========
    RetryCount     int       // 重试次数
    
    // ========== 时间戳（可选，审计和统计）==========
    CreatedTime    time.Time // 创建时间
    UpdatedTime    time.Time // 更新时间
    MountCount     int       // 挂载次数（统计）
}
```

**说明**：
- `RefCount`: 如果要支持多容器复用同一个 LV，必须要
- `PrevState`: 用于回退，但可以通过逻辑推断，不是必须
- `OperationStartTime`: 启动时检测遗留状态用，如果不需要自动恢复可以不要
- `RetryCount`: 避免无限重试，但可以通过其他方式控制

---

### 1.2 最小推荐结构（✅ 用户最终确认）

**Phase 1（最小可用）**：

```go
type LVInfo struct {
    Name         string    // LV 名称（必须）
    ContentID    string    // devbox content ID（必须）
    State        string    // 当前状态（必须）
    PrevState    string    // 前一个状态（✅ 用户确认：用于回退）
    MountPoint   string    // 挂载点（必须）
    IsFormatted  bool      // 是否已格式化（必须）
    Capacity     string    // 容量（必须）
    CurrentKey   string    // 当前 snapshot key（✅ 用户确认：方便调试）
    LastError    string    // 错误信息（推荐）
    CreatedTime  time.Time // 创建时间（推荐）
    UpdatedTime  time.Time // 更新时间（推荐）
}
```

**说明**：
- 共 11 个字段
- 8 个必须字段 + 3 个推荐字段
- `PrevState` 和 `CurrentKey` 由用户确认添加

**Phase 2（支持多容器复用）**：

```go
type LVInfo struct {
    // Phase 1 的所有字段
    
    RefCount     int       // 引用计数
    LastKey      string    // 上一个 snapshot key
}
```

**Phase 3（完善功能）**：

```go
type LVInfo struct {
    // Phase 1 + 2 的所有字段
    
    PrevState          string
    OperationStartTime time.Time
    RetryCount         int
    ErrorTime          time.Time
}
```

---

## 二、Remove 操作的完整设计

### 2.1 完整流程图

```
┌──────────────────────────────────────────────────────────────┐
│ Remove(ctx, key)                                              │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│ 1. 查询 snapshot 信息（metadata.db）                          │
│    ├─ contentID                                              │
│    └─ shouldDeleteLV (label)                                 │
│                                                              │
│ 2. 查询 LV 状态（devbox.db）                                  │
│    └─ currentState, lvInfo                                   │
│                                                              │
│ 3. 根据状态分发                                               │
│    ├─ mounted → unmountAndDelete / unmountOnly              │
│    ├─ unmounting → 恢复流程                                   │
│    ├─ created → deleteOnly / skipDelete                      │
│    └─ removing → 继续删除                                     │
└──────────────────────────────────────────────────────────────┘
```

---

### 2.2 场景 1：解挂载流程（不删除 LV）

```go
func (o *Snapshotter) Remove(ctx context.Context, key string) error {
    // ========== 步骤 1：查询 snapshot 信息 ==========
    var contentID string
    var shouldDeleteLV bool
    
    err := o.metaStore.WithTransaction(ctx, false, func(ctx) error {
        snap, err := storage.Get(ctx, key)
        if err != nil {
            if errors.Is(err, errdefs.ErrNotFound) {
                return nil  // Snapshot 不存在
            }
            return err
        }
        
        contentID = snap.Labels["devbox.content-id"]
        shouldDeleteLV = snap.Labels["devbox.delete-lv"] == "true"
        
        return nil
    })
    if err != nil {
        return err
    }
    
    if contentID == "" {
        // 非 devbox，直接删除 snapshot
        return o.removeSnapshotMetadata(ctx, key)
    }
    
    // ========== 步骤 2：查询 LV 状态 ==========
    lvName := "devbox-" + contentID
    currentState, lvInfo, err := o.getLVState(ctx, lvName)
    if err != nil {
        return fmt.Errorf("failed to get LV state: %w", err)
    }
    
    log.G(ctx).Infof("Remove: LV %s state=%s, shouldDelete=%v", 
        lvName, currentState, shouldDeleteLV)
    
    // ========== 步骤 3：根据状态和删除标志决定操作 ==========
    if currentState == LVStateMounted {
        if shouldDeleteLV {
            // 卸载 + 删除
            return o.unmountAndDeleteLV(ctx, key, lvName, lvInfo)
        } else {
            // 只卸载
            return o.unmountOnly(ctx, key, lvName, lvInfo)
        }
    }
    
    // 其他状态...
}
```

#### 只卸载的完整流程

```go
func (o *Snapshotter) unmountOnly(ctx, key, lvName, lvInfo) error {
    mountPoint := lvInfo.MountPoint
    
    log.G(ctx).Info("Remove: Unmounting LV (will be reused)")
    
    // ========== T1: devbox.db - 标记 unmounting ==========
    err := o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
        return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateUnmounting
            // lv.PrevState = LVStateMounted  // 可选
        })
    })
    if err != nil {
        return fmt.Errorf("failed to mark as unmounting: %w", err)
    }
    
    // ========== 物理操作：卸载（带重试）==========
    unmountErr := o.unmountWithRetry(ctx, mountPoint, 3)
    
    if unmountErr != nil {
        log.G(ctx).WithError(unmountErr).Error("Remove: Failed to unmount")
        
        // ========== 检查挂载点是否真的还在 ==========
        isMounted, checkErr := lvm.IsMountPoint(mountPoint)
        if checkErr != nil {
            log.G(ctx).WithError(checkErr).Warn("Failed to check mount point")
        }
        
        if !isMounted {
            // 挂载点不存在了，可能是并发操作或已经卸载
            log.G(ctx).Warn("Remove: Mount point not found, assuming unmounted")
            
            // 直接标记为 created
            o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
                return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                    lv.State = LVStateCreated
                    lv.CurrentKey = ""
                    lv.MountPoint = ""
                })
            })
            
            // 清理 snapshot metadata
            return o.removeSnapshotMetadata(ctx, key)
        }
        
        // ========== 挂载点还在，回退状态 ==========
        o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
            return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.State = LVStateMounted  // 回退
                lv.LastError = unmountErr.Error()
            })
        })
        
        return fmt.Errorf("failed to unmount: %w", unmountErr)
    }
    
    // ========== T2: devbox.db - 标记 created ==========
    err = o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
        return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateCreated  // 保留 LV
            lv.CurrentKey = ""          // 清除当前容器
            lv.MountPoint = ""          // 清除挂载点
            lv.LastError = ""           // 清除错误
        })
    })
    if err != nil {
        log.G(ctx).WithError(err).Warn("Failed to update LV state")
        // 不回退，继续
    }
    
    // ========== T3: metadata.db - 删除 snapshot ==========
    return o.metaStore.WithTransaction(ctx, true, func(ctx) error {
        return storage.Remove(ctx, key)
    })
}
```

---

### 2.3 场景 2：从 unmounting 状态恢复（Kubelet 重试）

```go
func (o *Snapshotter) Remove(ctx, key) error {
    // ... 查询状态 ...
    
    if currentState == LVStateUnmounting {
        // Kubelet 重试，上次卸载失败或被中断
        return o.removeFromUnmounting(ctx, key, lvName, lvInfo, shouldDeleteLV)
    }
}

func (o *Snapshotter) removeFromUnmounting(ctx, key, lvName, lvInfo, shouldDeleteLV) error {
    mountPoint := lvInfo.MountPoint
    
    log.G(ctx).Warn("Remove: Recovering from unmounting state")
    
    // ========== 检查是否真的还在挂载 ==========
    isMounted, err := lvm.IsMountPoint(mountPoint)
    if err != nil {
        log.G(ctx).WithError(err).Warn("Failed to check mount point")
    }
    
    if !isMounted {
        // 已经卸载了（可能是上次成功了但状态没更新）
        log.G(ctx).Info("Remove: Already unmounted, updating state")
        
        // 标记为 created
        o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
            return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.State = LVStateCreated
                lv.MountPoint = ""
            })
        })
        
        // 根据删除标志决定后续操作
        if shouldDeleteLV {
            return o.deleteLV(ctx, key, lvName)
        } else {
            return o.removeSnapshotMetadata(ctx, key)
        }
    }
    
    // ========== 还在挂载，重新尝试卸载 ==========
    log.G(ctx).Info("Remove: Still mounted, retrying unmount")
    
    unmountErr := o.unmountWithRetry(ctx, mountPoint, 3)
    if unmountErr != nil {
        // 重试失败，回退状态
        log.G(ctx).WithError(unmountErr).Error("Remove: Unmount retry failed")
        
        o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
            return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.State = LVStateMounted  // 回退
                lv.LastError = unmountErr.Error()
            })
        })
        
        return fmt.Errorf("unmount retry failed: %w", unmountErr)
    }
    
    // 卸载成功，标记为 created
    o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
        return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateCreated
            lv.MountPoint = ""
            lv.LastError = ""
        })
    })
    
    // 根据删除标志决定后续操作
    if shouldDeleteLV {
        return o.deleteLV(ctx, key, lvName)
    } else {
        return o.removeSnapshotMetadata(ctx, key)
    }
}
```

---

### 2.4 场景 3：删除 LV 流程

```go
func (o *Snapshotter) deleteLV(ctx, key, lvName) error {
    log.G(ctx).Infof("Remove: Deleting LV %s", lvName)
    
    // ========== T1: devbox.db - 标记 removing ==========
    err := o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
        return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateRemoving
        })
    })
    if err != nil {
        return fmt.Errorf("failed to mark as removing: %w", err)
    }
    
    // ========== 物理操作：删除 LV（带重试）==========
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{VolGroup: o.lvmVgName},
    }
    
    deleteErr := o.deleteLVWithRetry(ctx, vol, 3)
    
    if deleteErr != nil {
        log.G(ctx).WithError(deleteErr).Error("Remove: Failed to delete LV")
        
        // ========== 回退状态 ==========
        o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
            return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.State = LVStateCreated  // 回退到 created
                lv.LastError = deleteErr.Error()
            })
        })
        
        return fmt.Errorf("failed to delete LV: %w", deleteErr)
    }
    
    // ========== T2: devbox.db - 标记 removed ==========
    o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
        return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateRemoved
        })
    })
    
    // ========== T3: 清理所有 metadata ==========
    return o.cleanupAllMetadata(ctx, key, lvName)
}

func (o *Snapshotter) cleanupAllMetadata(ctx, key, lvName) error {
    // 1. 删除 snapshot（metadata.db）
    err1 := o.metaStore.WithTransaction(ctx, true, func(ctx) error {
        return storage.Remove(ctx, key)
    })
    if err1 != nil {
        log.G(ctx).WithError(err1).Warn("Failed to remove snapshot metadata")
    }
    
    // 2. 删除 LV 记录（devbox.db）
    err2 := o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
        return devboxStorage.RemoveLV(ctx, lvName)
    })
    if err2 != nil {
        log.G(ctx).WithError(err2).Warn("Failed to remove LV metadata")
    }
    
    if err1 != nil {
        return err1
    }
    return err2
}

func (o *Snapshotter) deleteLVWithRetry(ctx, vol, maxRetries) error {
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

### 2.5 场景 4：从 removing 状态恢复（删除被中断）

```go
func (o *Snapshotter) Remove(ctx, key) error {
    // ... 查询状态 ...
    
    if currentState == LVStateRemoving {
        // 上次删除被中断，继续删除
        return o.removeFromRemoving(ctx, key, lvName)
    }
}

func (o *Snapshotter) removeFromRemoving(ctx, key, lvName) error {
    log.G(ctx).Warn("Remove: Recovering from removing state")
    
    // ========== 检查 LV 是否还存在 ==========
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    lvExists := fileExists(devicePath)
    
    if !lvExists {
        // LV 已经删除了（上次可能成功了但状态没更新）
        log.G(ctx).Info("Remove: LV already deleted, cleaning up metadata")
        
        // 直接清理 metadata
        return o.cleanupAllMetadata(ctx, key, lvName)
    }
    
    // ========== LV 还在，继续删除 ==========
    log.G(ctx).Info("Remove: LV still exists, retrying delete")
    
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{VolGroup: o.lvmVgName},
    }
    
    deleteErr := o.deleteLVWithRetry(ctx, vol, 3)
    if deleteErr != nil {
        log.G(ctx).WithError(deleteErr).Error("Remove: Delete retry failed")
        
        // 回退状态
        o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
            return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.State = LVStateCreated
                lv.LastError = deleteErr.Error()
            })
        })
        
        return fmt.Errorf("delete retry failed: %w", deleteErr)
    }
    
    // 删除成功，清理 metadata
    return o.cleanupAllMetadata(ctx, key, lvName)
}
```

---

## 三、完整状态机

### 3.1 Remove 的状态转换

```
场景 1：只卸载（保留 LV）
mounted → unmounting → created
         ↓ 失败，检查挂载点
         ├─ 还在挂载 → mounted（回退）
         └─ 不在挂载 → created（继续）

场景 2：卸载并删除
mounted → unmounting → created → removing → removed → 清理 metadata
         ↓ 失败                    ↓ 失败
       mounted（回退）           created（回退）

场景 3：从 unmounting 恢复（Kubelet 重试）
unmounting → 检查挂载点
           ├─ 还在挂载 → 重试卸载 → created / mounted（失败回退）
           └─ 不在挂载 → created（更新状态）

场景 4：从 removing 恢复（删除被中断）
removing → 检查 LV 是否存在
         ├─ 还存在 → 重试删除 → removed / created（失败回退）
         └─ 不存在 → 清理 metadata
```

---

## 四、完整时间线对比

### 4.1 只卸载（容器停止）

```
正常流程：
T1: metadata.db 查询 → contentID, shouldDeleteLV=false
T2: devbox.db 查询 → state=mounted
T3: devbox.db 事务 → state=unmounting
T4: 物理操作 → unmount（成功）
T5: devbox.db 事务 → state=created
T6: metadata.db 事务 → 删除 snapshot

失败流程：
T1-T3: 同上
T4: 物理操作 → unmount（失败）
T5: 检查挂载点 → 还在挂载
T6: devbox.db 事务 → state=mounted（回退）
T7: 返回错误

Kubelet 重试：
T1: 查询 state=mounted → 重新执行卸载流程
```

---

### 4.2 卸载并删除（Devbox 删除）

```
正常流程：
T1: metadata.db 查询 → shouldDeleteLV=true
T2: devbox.db 查询 → state=mounted
T3: devbox.db 事务 → state=unmounting
T4: 物理操作 → unmount（成功）
T5: devbox.db 事务 → state=created
T6: devbox.db 事务 → state=removing
T7: 物理操作 → delete LV（成功）
T8: devbox.db 事务 → state=removed
T9: metadata.db 事务 → 删除 snapshot
T10: devbox.db 事务 → 删除 LV 记录

删除失败流程：
T1-T6: 同上
T7: 物理操作 → delete LV（失败，重试 3 次）
T8: devbox.db 事务 → state=created（回退）
T9: 返回错误

Kubelet 重试：
T1: 查询 state=created, shouldDeleteLV=true
T2: 直接执行删除流程（跳过卸载）
```

---

## 五、关键设计点总结

### 5.1 数据库结构

**最小推荐**（8 个字段）：

```go
type LVInfo struct {
    Name         string
    ContentID    string
    State        string
    MountPoint   string
    IsFormatted  bool
    Capacity     string
    CurrentKey   string  // 推荐
    LastError    string  // 推荐
}
```

### 5.2 Remove 流程

1. **查询两个数据库**
   - metadata.db: contentID, shouldDeleteLV
   - devbox.db: currentState, lvInfo

2. **根据状态分发**
   - mounted → 卸载流程
   - unmounting → 恢复卸载
   - created → 删除流程（如果需要）
   - removing → 恢复删除

3. **失败处理**
   - 卸载失败 → 检查挂载点 → 回退或继续
   - 删除失败 → 重试 3 次 → 回退状态

4. **状态回退**
   - unmounting 失败 → mounted
   - removing 失败 → created

### 5.3 与你的方案对比

| 你的方案 | 我的改进 | 原因 |
|---------|---------|------|
| "存在则报错返回，状态仍然为Unmounting" | 回退状态为 mounted | 避免系统长期处于中间状态 |
| "下次进来是仍然为错误状态时直接告警报错" | 检查状态，尝试恢复 | 自动恢复，不依赖人工 |
| 记录错误状态 | 记录 LastError，但状态回退 | 状态回退到稳定状态，错误单独记录 |

**核心差异**：
- 你的方案倾向于保持中间状态 + 记录错误
- 我的方案倾向于立即回退到稳定状态 + 错误单独记录

**我的方案的优势**：
- 系统始终在稳定状态
- 重试逻辑更清晰
- 不需要额外的"错误状态"

