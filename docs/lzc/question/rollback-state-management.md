# 回退操作的状态管理策略

## 一、问题分析

### 1.1 问题场景

在创建容器过程中，各个环节都可能失败并需要回退：

```
场景 1：创建 LV 失败
none → creating → (失败) → 需要删除僵尸 LV

场景 2：格式化失败
created → mounting → 格式化失败 → 需要回退到 created

场景 3：挂载失败
created → mounting → 挂载失败 → 需要回退到 created
```

### 1.2 核心疑问

**回退操作是否需要标记状态？**

```go
// 方案 A：回退时标记状态
if createErr != nil {
    o.setLVState(ctx, lvName, LVStateRemoving)  // 标记
    forceDeleteLV(lvName)
    o.setLVState(ctx, lvName, LVStateNone)
    return createErr
}

// 方案 B：回退时不标记状态
if createErr != nil {
    forceDeleteLV(lvName)  // 直接清理，不标记
    return createErr
}
```

**如果每个回退都标记状态，会导致：**
- 状态数量翻倍（正常操作 + 回退操作）
- 管理复杂度大大增加
- 状态机难以理解和维护

---

## 二、推荐方案：回退操作不标记状态

### 2.1 核心原则

**正常操作** vs **回退操作**：

| 维度 | 正常操作 | 回退操作 |
|------|---------|---------|
| **是否标记状态** | ✅ 是 | ❌ 否 |
| **可能被中断** | ✅ 是（进程崩溃） | ⚠️ 概率很低 |
| **需要恢复** | ✅ 是（下次调用） | ❌ 否（尽力清理） |
| **执行时间** | 可能很长 | 通常很短 |
| **是否外部可见** | ✅ 是 | ❌ 否 |

**关键区别**：
- **正常操作**：需要状态追踪，因为可能被中断，下次调用需要知道进度
- **回退操作**：不需要状态，尽力清理即可，失败了后续 Cleanup 会处理

---

### 2.2 具体实现

#### 场景 1：创建 LV 失败

```go
func (o *Snapshotter) prepareFirstContainer(ctx, key, parent, contentID, opts) {
    lvName := "devbox-" + contentID
    
    // ========== 步骤 1：标记 creating（正常操作）==========
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.AddLV(ctx, &LVInfo{
            Name:      lvName,
            State:     LVStateCreating,  // ← 标记正常操作状态
            PrevState: LVStateNone,
        })
    })
    
    // ========== 步骤 2：创建 LV ==========
    vol := &apis.LVMVolume{...}
    createErr := lvm.CreateVolume(ctx, vol)
    
    if createErr != nil {
        // ❌ 创建失败
        
        // ========== 回退：清理僵尸 LV ==========
        // ✅ 不标记状态，直接清理
        
        // 删除 metadata（如果有）
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            storage.Remove(ctx, key)
            storage.RemoveLV(ctx, lvName)  // 直接删除记录，不标记 removing
            return nil
        })
        
        // 强制删除 LV（不检查状态）
        _ = lvm.ForceDestroyVolume(ctx, vol)  // 尽力清理，失败也不管
        
        return nil, fmt.Errorf("failed to create LV: %w", createErr)
    }
    
    // ========== 步骤 3：标记 created（正常操作）==========
    o.setLVState(ctx, lvName, LVStateCreated)
    
    // 继续后续操作...
}
```

**关键点**：
1. **正常操作标记状态**：`creating` → `created`
2. **回退操作不标记**：直接删除 metadata 和 LV
3. **失败了也不管**：`ForceDestroyVolume` 失败不影响返回错误

---

#### 场景 2：格式化失败

```go
func (o *Snapshotter) formatAndMountLV(ctx, key, lvName, snapID, needFormat) {
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    mountPoint := o.getMountPoint(key)
    
    // ========== 步骤 1：标记 mounting（正常操作）==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.PrevState = lv.State  // created
            lv.State = LVStateMounting  // ← 标记正常操作状态
        })
    })
    
    // ========== 步骤 2：格式化 ==========
    if needFormat {
        formatErr := o.formatWithRetry(ctx, devicePath, 3)
        if formatErr != nil {
            // ❌ 格式化失败
            
            // ========== 回退：标记回 created ==========
            // ✅ 这里只是状态回退，不涉及删除
            o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
                return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                    lv.State = LVStateCreated  // 回退到稳定状态
                    lv.LastError = formatErr.Error()
                })
            })
            
            return nil, fmt.Errorf("failed to format: %w", formatErr)
        }
    }
    
    // ========== 步骤 3：挂载 ==========
    mountErr := o.mountWithRetry(ctx, devicePath, mountPoint, 3)
    if mountErr != nil {
        // ❌ 挂载失败
        
        // ========== 回退：标记回 created ==========
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.State = LVStateCreated  // 回退到稳定状态
                lv.LastError = mountErr.Error()
            })
        })
        
        return nil, fmt.Errorf("failed to mount: %w", mountErr)
    }
    
    // ========== 步骤 4：标记 mounted（正常操作）==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateMounted
        })
    })
    
    return mounts, nil
}
```

**关键点**：
1. **正常操作标记状态**：`mounting` → `mounted`
2. **回退只是状态回退**：`mounting` → `created`（不涉及物理操作）
3. **不需要标记回退状态**：直接从 `mounting` 变为 `created`

---

#### 场景 3：卸载失败（Remove 时）

```go
func (o *Snapshotter) unmountAndKeepLV(ctx, key, lvName, lvInfo) error {
    mountPoint := lvInfo.MountPoint
    
    // ========== 步骤 1：标记 unmounting（正常操作）==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.PrevState = lv.State  // mounted
            lv.State = LVStateUnmounting  // ← 标记正常操作状态
        })
    })
    
    // ========== 步骤 2：卸载 ==========
    unmountErr := o.unmountWithRetry(ctx, mountPoint, 3)
    if unmountErr != nil {
        // ❌ 卸载失败
        
        // ========== 回退：标记回 mounted ==========
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.State = LVStateMounted  // 回退到稳定状态
                lv.LastError = unmountErr.Error()
            })
        })
        
        return fmt.Errorf("failed to unmount: %w", unmountErr)
    }
    
    // ========== 步骤 3：标记 created（正常操作）==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateCreated
        })
    })
    
    return nil
}
```

---

### 2.3 ForceDestroyVolume 实现

**用于回退清理的强制删除函数**：

```go
// ForceDestroyVolume 强制删除 LV，用于回退清理
// 不检查状态，不检查设备节点，直接删除 LVM 元数据
func ForceDestroyVolume(ctx context.Context, vol *apis.LVMVolume) error {
    if vol.Spec.VolGroup == "" {
        return nil
    }
    
    volume := vol.Spec.VolGroup + "/" + vol.Name
    
    // 不检查 LV 是否存在，直接尝试删除
    // 使用 lvremove -f 强制删除
    args := []string{"-f", DevPath + volume}
    out, _, err := RunCommandSplit(ctx, LVRemove, args...)
    
    if err != nil {
        // 即使失败也只记录日志，不返回错误
        klog.Warningf("ForceDestroyVolume: failed to remove %s: %v, output: %s", 
            volume, err, string(out))
        return nil  // ← 关键：失败不返回错误
    }
    
    klog.Infof("ForceDestroyVolume: successfully removed %s", volume)
    return nil
}
```

**关键特点**：
1. 不检查 LV 是否存在
2. 不检查设备节点
3. 使用 `-f` 强制删除
4. **失败不返回错误**（尽力清理）

---

## 三、状态对比

### 3.1 如果回退也标记状态（❌ 不推荐）

```
正常流程：
none → creating → created → mounting → mounted

失败流程 1：创建失败
none → creating → (失败) → removing → none
                            ^^^^^^^^
                            回退状态（多余）

失败流程 2：挂载失败
created → mounting → (失败) → unmounting → created
                               ^^^^^^^^^^
                               回退状态（多余）
```

**问题**：
- 状态数量：6 个正常状态 + 3 个回退状态 = 9 个状态
- 管理复杂度高
- 回退状态几乎用不到（因为在同一个函数内）

---

### 3.2 回退不标记状态（✅ 推荐）

```
正常流程：
none → creating → created → mounting → mounted

失败流程 1：创建失败
none → creating → (失败，清理) → none
                  ↑
                  直接清理，不标记 removing

失败流程 2：挂载失败
created → mounting → (失败) → created
                      ↑
                      直接回退状态，不标记 unmounting
```

**优势**：
- 状态数量：6 个状态（没有增加）
- 管理简单
- 回退是原子的紧急清理

---

## 四、详细对比表

### 4.1 各操作的状态管理

| 操作类型 | 是否标记状态 | 原因 | 示例 |
|---------|------------|------|------|
| **正常创建** | ✅ 是 | 可能被中断，需要恢复 | `creating` → `created` |
| **正常挂载** | ✅ 是 | 可能被中断，需要恢复 | `mounting` → `mounted` |
| **正常卸载** | ✅ 是 | 可能被中断，需要恢复 | `unmounting` → `created` |
| **正常删除** | ✅ 是 | 可能被中断，需要恢复 | `removing` → `removed` |
| **创建失败回退** | ❌ 否 | 尽力清理，失败也不管 | 直接删除，不标记 |
| **格式化失败回退** | ❌ 否 | 只是状态回退，无物理操作 | `mounting` → `created` |
| **挂载失败回退** | ❌ 否 | 只是状态回退，无物理操作 | `mounting` → `created` |
| **卸载失败回退** | ❌ 否 | 只是状态回退，无物理操作 | `unmounting` → `mounted` |

### 4.2 状态数量对比

| 方案 | 稳定状态 | 中间状态（正常） | 中间状态（回退） | 总计 |
|------|---------|----------------|----------------|------|
| **回退标记状态** | 4 | 4 | 3 | **11 个** ⚠️ |
| **回退不标记** | 4 | 4 | 0 | **8 个** ✅ |

```
回退标记状态的方案：
稳定：none, created, mounted, removed
正常：creating, mounting, unmounting, removing
回退：rollback_creating, rollback_mounting, rollback_unmounting

回退不标记的方案：
稳定：none, created, mounted, removed
正常：creating, mounting, unmounting, removing
```

---

## 五、特殊情况处理

### 5.1 回退操作被中断怎么办？

**场景**：创建失败，正在清理僵尸 LV 时进程崩溃

```go
createErr := lvm.CreateVolume(ctx, vol)
if createErr != nil {
    // 删除 metadata
    storage.RemoveLV(ctx, lvName)
    
    // 强制删除 LV
    lvm.ForceDestroyVolume(ctx, vol)  // ← 这里进程崩溃
    
    return createErr
}
```

**结果**：
- Metadata 已删除 ✅
- 僵尸 LV 可能还在 ⚠️

**处理**：
- **下次调用 Prepare**：会检测到 `state=none`，重新创建（覆盖僵尸 LV）
- **Cleanup**：会扫描并删除没有 metadata 的 LV

**关键**：即使回退被中断，系统仍然是**自愈的**

---

### 5.2 Cleanup 如何处理僵尸 LV

```go
func (o *Snapshotter) Cleanup(ctx context.Context) error {
    // 获取应该存在的 LV（从 metadata）
    expectedLVs, _ := storage.GetAllLVs(ctx)
    
    // 获取实际存在的 LV（从 lvs 命令）
    actualLVs, _ := lvm.ListLVMLogicalVolumeByVG(ctx, vgName)
    
    // 找出孤儿 LV（没有 metadata 的）
    for _, actualLV := range actualLVs {
        if _, exists := expectedLVs[actualLV.Name]; !exists {
            // 僵尸 LV，强制删除
            log.G(ctx).Warnf("Found orphan LV %s, will force remove", actualLV.Name)
            vol := &apis.LVMVolume{Name: actualLV.Name, Spec: apis.VolumeInfo{VolGroup: vgName}}
            _ = lvm.ForceDestroyVolume(ctx, vol)
        }
    }
    
    return nil
}
```

---

## 六、完整代码示例

### 6.1 Prepare 的完整实现（带回退）

```go
func (o *Snapshotter) prepareFirstContainer(ctx, key, parent, contentID, opts) ([]mount.Mount, error) {
    lvName := "devbox-" + contentID
    
    // ========== 步骤 1：创建 metadata，标记 creating ==========
    var snapID string
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        snap, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, parent, opts...)
        if err != nil {
            return err
        }
        snapID = snap.ID
        
        return storage.AddLV(ctx, &LVInfo{
            Name:               lvName,
            State:              LVStateCreating,  // ← 正常操作，标记状态
            PrevState:          LVStateNone,
            IsFormatted:        false,
            OperationStartTime: time.Now(),
        })
    })
    if err != nil {
        return nil, fmt.Errorf("failed to create metadata: %w", err)
    }
    
    // ========== 步骤 2：创建 LV ==========
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{Capacity: capacity, VolGroup: vgName},
    }
    
    createErr := lvm.CreateVolume(ctx, vol)
    if createErr != nil {
        log.G(ctx).WithError(createErr).Error("Failed to create LV")
        
        // ========== 回退：删除 metadata 和僵尸 LV ==========
        // ✅ 不标记 removing 状态，直接清理
        
        // 1. 删除 metadata
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            storage.Remove(ctx, key)
            storage.RemoveLV(ctx, lvName)  // 直接删除，不标记状态
            return nil
        })
        
        // 2. 强制删除 LV（尽力清理）
        _ = lvm.ForceDestroyVolume(ctx, vol)  // 失败也不管
        
        return nil, fmt.Errorf("failed to create LV: %w", createErr)
    }
    
    // ========== 步骤 3：标记 created ==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateCreated  // ← 正常操作，更新状态
        })
    })
    
    // ========== 步骤 4：格式化并挂载 ==========
    return o.formatAndMountLV(ctx, key, lvName, snapID, true)
}
```

---

## 七、总结

### 7.1 推荐方案

✅ **回退操作不标记状态**

**原因**：
1. 回退是原子的紧急清理，通常在同一个函数内完成
2. 回退被中断的概率极低
3. 即使回退失败，系统有 Cleanup 机制兜底
4. 避免状态爆炸（从 8 个增加到 11 个）
5. 简化管理和理解

### 7.2 状态管理规则

```
正常操作（需要标记状态）：
1. 创建 LV：none → creating → created
2. 挂载 LV：created → mounting → mounted
3. 卸载 LV：mounted → unmounting → created
4. 删除 LV：created → removing → removed

回退操作（不标记状态）：
1. 创建失败：直接删除 metadata + ForceDestroyVolume
2. 格式化失败：mounting → created（状态回退）
3. 挂载失败：mounting → created（状态回退）
4. 卸载失败：unmounting → mounted（状态回退）
```

### 7.3 关键实现

```go
// 正常操作：标记状态
o.setLVState(ctx, lvName, LVStateMounting)  // ✅ 标记
mountErr := mount(...)
o.setLVState(ctx, lvName, LVStateMounted)   // ✅ 标记

// 回退操作：不标记状态
if mountErr != nil {
    o.setLVState(ctx, lvName, LVStateCreated)  // ✅ 只回退到稳定状态
    return err  // ❌ 不标记 unmounting
}

// 创建失败回退：直接清理
if createErr != nil {
    storage.RemoveLV(ctx, lvName)  // ❌ 不标记 removing
    lvm.ForceDestroyVolume(ctx, vol)  // ❌ 不标记 removing
    return err
}
```

### 7.4 优势

- ✅ 状态数量少（8 个而不是 11 个）
- ✅ 管理简单
- ✅ 易于理解
- ✅ 回退逻辑清晰
- ✅ 系统自愈能力强（Cleanup 兜底）

