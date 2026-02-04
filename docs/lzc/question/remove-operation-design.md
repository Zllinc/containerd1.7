# Remove 操作的统一设计方案

## 一、当前架构的问题

### 1.1 旧架构的流程

```
步骤 1：Update() 中设置删除标记
  ├─ 设置 label: "devbox.remove" = "true"
  ├─ 设置 status: "removed"
  └─ 执行解挂载操作

步骤 2：Remove() 中删除资源
  ├─ 查询 status，发现是 "removed"
  ├─ 删除 metadata bucket
  └─ 由于 metadata 中找不到，认为是可清理的 LV，删除之
```

**问题**：
1. ❌ 操作分散：解挂载在 `Update`，删除在 `Remove`
2. ❌ 需要中间状态：需要先标记 "removed"
3. ❌ 逻辑复杂：通过"找不到 metadata"来判断可删除
4. ❌ 不直观：删除操作应该在 `Remove` 中完成

---

## 二、新架构的设计

### 2.1 核心思想

**所有删除相关操作都在 Remove() 中完成**：

```go
Remove(ctx, key) {
    // 1. 判断是否需要删除 LV（通过 snapshot 信息）
    shouldDeleteLV := checkIfShouldDeleteLV(key)
    
    // 2. 根据判断执行不同操作
    if shouldDeleteLV {
        // 卸载 + 删除 LV
        unmountAndDeleteLV()
    } else {
        // 只卸载，保留 LV
        unmountOnly()
    }
    
    // 3. 清理 snapshot metadata
    removeSnapshotMetadata()
}
```

### 2.2 如何判断是否删除 LV？

#### 方案 1：通过 Label 判断（推荐）

在删除 snapshot 时，通过 label 告知是否需要删除 LV：

```go
// 调用方（例如 kubelet）
func deleteSnapshot(key string, shouldDeleteLV bool) {
    // 设置 label
    labels := map[string]string{}
    if shouldDeleteLV {
        labels["devbox.delete-lv"] = "true"
    }
    
    // 调用 Remove
    snapshotter.Remove(ctx, key)
}
```

**Remove 实现**：

```go
func (o *Snapshotter) Remove(ctx context.Context, key string) error {
    log.G(ctx).Infof("Remove: Removing snapshot %s", key)
    
    // ========== 步骤 1：查询 snapshot 信息 ==========
    var contentID string
    var shouldDeleteLV bool
    
    err := o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
        snap, err := storage.Get(ctx, key)
        if err != nil {
            if errors.Is(err, errdefs.ErrNotFound) {
                return nil  // Snapshot 不存在，可能已被删除
            }
            return err
        }
        
        // 获取 content ID
        contentID = snap.Labels["devbox.content-id"]
        
        // 检查是否需要删除 LV
        shouldDeleteLV = snap.Labels["devbox.delete-lv"] == "true"
        
        return nil
    })
    if err != nil {
        return err
    }
    
    if contentID == "" {
        // 不是 devbox snapshot，直接删除 metadata
        return o.removeSnapshotMetadata(ctx, key)
    }
    
    lvName := "devbox-" + contentID
    
    // ========== 步骤 2：检查 LV 状态 ==========
    currentState, lvInfo, err := o.getLVState(ctx, lvName)
    if err != nil {
        return fmt.Errorf("failed to get LV state: %w", err)
    }
    
    log.G(ctx).Infof("Remove: LV %s state=%s, shouldDelete=%v", 
        lvName, currentState, shouldDeleteLV)
    
    // ========== 步骤 3：根据状态和删除标志决定操作 ==========
    if shouldDeleteLV {
        // 卸载并删除 LV
        return o.unmountAndDeleteLV(ctx, key, lvName, currentState, lvInfo)
    } else {
        // 只卸载，保留 LV（下个容器会用）
        return o.unmountOnly(ctx, key, lvName, currentState, lvInfo)
    }
}
```

---

#### 方案 2：通过引用计数判断（更智能）

自动判断是否是最后一个容器：

```go
type LVInfo struct {
    Name            string
    ContentID       string
    State           string
    
    // 引用计数
    RefCount        int      // 当前有多少个 snapshot 在使用
    TotalSnapshots  []string // 所有使用这个 LV 的 snapshot keys
}
```

**Remove 实现**：

```go
func (o *Snapshotter) Remove(ctx context.Context, key string) error {
    var contentID string
    var shouldDeleteLV bool
    
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        snap, err := storage.Get(ctx, key)
        if err != nil {
            return err
        }
        
        contentID = snap.Labels["devbox.content-id"]
        if contentID == "" {
            return nil
        }
        
        lvName := "devbox-" + contentID
        
        // 获取 LV 信息
        lv, err := storage.GetLV(ctx, lvName)
        if err != nil {
            return err
        }
        
        // 从引用列表中移除当前 snapshot
        lv.TotalSnapshots = removeFromSlice(lv.TotalSnapshots, key)
        lv.RefCount = len(lv.TotalSnapshots)
        
        // 如果引用计数为 0，说明是最后一个
        shouldDeleteLV = (lv.RefCount == 0)
        
        // 更新 LV 信息
        storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.RefCount = lv.RefCount - 1
            lv.TotalSnapshots = removeFromSlice(lv.TotalSnapshots, key)
        })
        
        return nil
    })
    
    if contentID == "" {
        return o.removeSnapshotMetadata(ctx, key)
    }
    
    lvName := "devbox-" + contentID
    currentState, lvInfo, _ := o.getLVState(ctx, lvName)
    
    // 根据引用计数决定操作
    if shouldDeleteLV {
        log.G(ctx).Info("Remove: Last container, will delete LV")
        return o.unmountAndDeleteLV(ctx, key, lvName, currentState, lvInfo)
    } else {
        log.G(ctx).Infof("Remove: Not last container (refCount=%d), will only unmount", 
            lvInfo.RefCount)
        return o.unmountOnly(ctx, key, lvName, currentState, lvInfo)
    }
}
```

---

## 三、Remove 的完整实现

### 3.1 主函数

```go
func (o *Snapshotter) Remove(ctx context.Context, key string) error {
    log.G(ctx).Infof("Remove: Removing snapshot %s", key)
    
    // ========== 步骤 1：查询 snapshot 和 LV 信息 ==========
    var contentID string
    var shouldDeleteLV bool
    
    err := o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
        snap, err := storage.Get(ctx, key)
        if err != nil {
            if errors.Is(err, errdefs.ErrNotFound) {
                log.G(ctx).Warn("Remove: Snapshot not found, may already be deleted")
                return nil
            }
            return err
        }
        
        // 获取 content ID（devbox 标识）
        contentID = snap.Labels["devbox.content-id"]
        
        // 方案 1：通过 label 判断
        shouldDeleteLV = snap.Labels["devbox.delete-lv"] == "true"
        
        // 方案 2：通过引用计数判断（可选，更智能）
        // if contentID != "" {
        //     lv, _ := storage.GetLV(ctx, "devbox-"+contentID)
        //     shouldDeleteLV = (lv.RefCount <= 1)
        // }
        
        return nil
    })
    if err != nil {
        return err
    }
    
    // 非 devbox snapshot，直接删除 metadata
    if contentID == "" {
        return o.removeSnapshotMetadata(ctx, key)
    }
    
    lvName := "devbox-" + contentID
    
    // ========== 步骤 2：检查 LV 当前状态 ==========
    currentState, lvInfo, err := o.getLVState(ctx, lvName)
    if err != nil {
        return fmt.Errorf("failed to get LV state: %w", err)
    }
    
    log.G(ctx).Infof("Remove: LV %s state=%s, shouldDelete=%v", 
        lvName, currentState, shouldDeleteLV)
    
    // ========== 步骤 3：根据状态和删除标志决定操作 ==========
    switch currentState {
    case LVStateNone:
        // LV 不存在，只清理 snapshot metadata
        return o.removeSnapshotMetadata(ctx, key)
        
    case LVStateMounted:
        if shouldDeleteLV {
            // 卸载并删除
            return o.unmountAndDeleteLV(ctx, key, lvName, lvInfo)
        } else {
            // 只卸载
            return o.unmountOnly(ctx, key, lvName, lvInfo)
        }
        
    case LVStateUnmounting:
        // 上次卸载被中断，检查实际状态
        return o.removeFromUnmounting(ctx, key, lvName, lvInfo, shouldDeleteLV)
        
    case LVStateCreated, LVStateUnmounted:
        if shouldDeleteLV {
            // 已卸载，直接删除
            return o.deleteLV(ctx, key, lvName)
        } else {
            // 已卸载，只清理 snapshot metadata
            return o.removeSnapshotMetadata(ctx, key)
        }
        
    case LVStateRemoving:
        // 上次删除被中断，继续删除
        return o.removeFromRemoving(ctx, key, lvName, lvInfo)
        
    default:
        return fmt.Errorf("unexpected LV state: %s", currentState)
    }
}
```

---

### 3.2 场景 1：只卸载（保留 LV 给下个容器）

```go
func (o *Snapshotter) unmountOnly(ctx context.Context, key, lvName string, lvInfo *LVInfo) error {
    log.G(ctx).Info("Remove: Unmounting LV (will be reused by next container)")
    
    mountPoint := lvInfo.MountPoint
    
    // ========== 步骤 1：标记为 unmounting ==========
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.PrevState = lv.State  // mounted
            lv.State = LVStateUnmounting
            lv.OperationStartTime = time.Now()
        })
    })
    if err != nil {
        return fmt.Errorf("failed to mark as unmounting: %w", err)
    }
    
    // ========== 步骤 2：卸载（带重试）==========
    unmountErr := o.unmountWithRetry(ctx, mountPoint, 3)
    if unmountErr != nil {
        log.G(ctx).WithError(unmountErr).Error("Remove: Failed to unmount")
        
        // 回退到 mounted
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.State = LVStateMounted
                lv.LastError = unmountErr.Error()
            })
        })
        
        return fmt.Errorf("failed to unmount: %w", unmountErr)
    }
    
    // ========== 步骤 3：标记为 created（保留 LV）==========
    log.G(ctx).Info("Remove: Unmount successful, marking as created")
    err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateCreated      // ← 回到 created，不是 removed
            lv.CurrentKey = ""              // 清除当前容器
            lv.LastKey = key                // 记录上一个容器
            lv.MountPoint = ""              // 清除挂载点
            lv.LastError = ""
        })
    })
    if err != nil {
        return fmt.Errorf("failed to update LV state: %w", err)
    }
    
    // ========== 步骤 4：清理 snapshot metadata ==========
    return o.removeSnapshotMetadata(ctx, key)
}
```

---

### 3.3 场景 2：卸载并删除 LV

```go
func (o *Snapshotter) unmountAndDeleteLV(ctx context.Context, key, lvName string, lvInfo *LVInfo) error {
    log.G(ctx).Info("Remove: Unmounting and deleting LV (last container)")
    
    // ========== 步骤 1：先卸载（如果已挂载）==========
    if lvInfo.State == LVStateMounted {
        err := o.unmountLVForDeletion(ctx, lvName, lvInfo)
        if err != nil {
            return fmt.Errorf("failed to unmount before delete: %w", err)
        }
        
        // 重新获取状态（应该是 created 或 unmounted）
        _, lvInfo, _ = o.getLVState(ctx, lvName)
    }
    
    // ========== 步骤 2：删除 LV ==========
    if lvInfo.State == LVStateCreated || lvInfo.State == LVStateUnmounted {
        return o.deleteLV(ctx, key, lvName)
    }
    
    return fmt.Errorf("unexpected state for deletion: %s", lvInfo.State)
}

func (o *Snapshotter) unmountLVForDeletion(ctx context.Context, lvName string, lvInfo *LVInfo) error {
    mountPoint := lvInfo.MountPoint
    
    // 标记 unmounting
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.PrevState = lv.State
            lv.State = LVStateUnmounting
        })
    })
    
    // 卸载（带重试）
    unmountErr := o.unmountWithRetry(ctx, mountPoint, 3)
    if unmountErr != nil {
        // 回退到 mounted
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.State = LVStateMounted
                lv.LastError = unmountErr.Error()
            })
        })
        return unmountErr
    }
    
    // 标记 created（临时状态，准备删除）
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateCreated
        })
    })
    
    return nil
}

func (o *Snapshotter) deleteLV(ctx context.Context, key, lvName string) error {
    log.G(ctx).Infof("Remove: Deleting LV %s", lvName)
    
    // ========== 步骤 1：标记为 removing ==========
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.PrevState = lv.State
            lv.State = LVStateRemoving
            lv.OperationStartTime = time.Now()
        })
    })
    if err != nil {
        return fmt.Errorf("failed to mark as removing: %w", err)
    }
    
    // ========== 步骤 2：删除 LV（带重试）==========
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{VolGroup: o.lvmVgName},
    }
    
    deleteErr := o.deleteLVWithRetry(ctx, vol, 3)
    if deleteErr != nil {
        log.G(ctx).WithError(deleteErr).Error("Remove: Failed to delete LV")
        
        // 回退到 created
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.State = LVStateCreated
                lv.LastError = deleteErr.Error()
            })
        })
        
        return fmt.Errorf("failed to delete LV: %w", deleteErr)
    }
    
    // ========== 步骤 3：标记为 removed ==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateRemoved
        })
    })
    
    // ========== 步骤 4：清理所有 metadata ==========
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 删除 snapshot metadata
        if err := storage.Remove(ctx, key); err != nil {
            log.G(ctx).WithError(err).Warn("Failed to remove snapshot metadata")
        }
        
        // 删除 LV metadata
        if err := storage.RemoveLV(ctx, lvName); err != nil {
            log.G(ctx).WithError(err).Warn("Failed to remove LV metadata")
        }
        
        return nil
    })
}
```

---

## 四、调用方的使用

### 4.1 容器停止（不删除 LV）

```go
// 调用方
func stopContainer(containerID string) {
    // 不设置 delete-lv label
    err := snapshotter.Remove(ctx, containerID)
    // LV 被保留，下个容器可以复用
}
```

### 4.2 Devbox 删除（删除 LV）

```go
// 调用方
func deleteDevbox(containerID string) {
    // 设置 delete-lv label
    err := snapshotter.Update(ctx, snapshots.Info{
        Name: containerID,
        Labels: map[string]string{
            "devbox.delete-lv": "true",
        },
    })
    
    // 调用 Remove
    err = snapshotter.Remove(ctx, containerID)
    // LV 被删除
}
```

---

## 五、完整时间线对比

### 5.1 旧架构（操作分散）

```
容器停止（不删除 LV）：
T1: Update → 解挂载
T2: Remove → 清理 snapshot metadata
    （LV 保留）

Devbox 删除（删除 LV）：
T1: Update → 设置 status="removed"
T2: Update → 解挂载
T3: Remove → 删除 bucket
T4: Remove → 由于找不到 metadata，删除 LV
```

### 5.2 新架构（操作集中）

```
容器停止（不删除 LV）：
T1: Remove → 判断 shouldDeleteLV=false
T2: Remove → unmountOnly()
    ├─ 标记 unmounting
    ├─ 执行卸载
    ├─ 标记 created（保留）
    └─ 清理 snapshot metadata

Devbox 删除（删除 LV）：
T1: Update → 设置 label "devbox.delete-lv"="true"（可选）
T2: Remove → 判断 shouldDeleteLV=true
T3: Remove → unmountAndDeleteLV()
    ├─ 标记 unmounting
    ├─ 执行卸载
    ├─ 标记 created
    ├─ 标记 removing
    ├─ 执行删除
    ├─ 标记 removed
    └─ 清理所有 metadata
```

---

## 六、优势对比

| 维度 | 旧架构 | 新架构 |
|------|--------|--------|
| **操作位置** | 分散在 Update 和 Remove | 集中在 Remove ✅ |
| **中间状态** | 需要 "removed" 状态 | 不需要 ✅ |
| **判断方式** | 通过"找不到 metadata" | 明确的 shouldDeleteLV ✅ |
| **清晰度** | 间接，不直观 | 直接，清晰 ✅ |
| **可维护性** | 低 | 高 ✅ |
| **可测试性** | 低 | 高 ✅ |

---

## 七、总结

### 7.1 核心改进

✅ **所有删除操作集中在 Remove 中**
- 卸载操作在 Remove
- 删除操作在 Remove
- 不需要在 Update 中预处理

✅ **明确的删除判断**
- 通过 label 或引用计数明确判断
- 不依赖"找不到 metadata"的间接方式

✅ **清晰的状态流转**
- 只卸载：`mounted → unmounting → created`
- 卸载+删除：`mounted → unmounting → created → removing → removed`

### 7.2 实施建议

**Phase 1**：
1. 实现基本的 `Remove` 函数（支持 shouldDeleteLV 判断）
2. 实现 `unmountOnly` 和 `unmountAndDeleteLV`
3. 移除 `Update` 中的卸载逻辑

**Phase 2**：
4. 实现引用计数机制（可选）
5. 添加状态恢复逻辑
6. 完善错误处理

**Phase 3**：
7. 优化性能
8. 添加监控指标
9. 完善日志

