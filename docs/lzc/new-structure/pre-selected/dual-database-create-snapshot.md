# 双数据库架构下的 CreateSnapshot 完整实现

## 一、数据库职责划分

### 1.1 metadata.db（MetaStore）

**职责**：Containerd 标准的 snapshot 元数据

```go
// 存储内容
type Snapshot struct {
    ID        string    // snapshot ID
    Kind      string    // Active/Committed
    Key       string    // snapshot key
    Parent    string    // 父 snapshot
    Labels    map[string]string
    CreatedAt time.Time
    UpdatedAt time.Time
}

// 操作
- CreateSnapshot()
- GetSnapshot()
- UpdateSnapshot()
- RemoveSnapshot()
- WalkSnapshots()
```

### 1.2 devbox.db（LVM Metadata 或 DevboxMetadata）

**职责**：LV 相关的状态和信息

```go
// 存储内容
type LVInfo struct {
    Name            string    // LV 名称
    ContentID       string    // devbox content ID
    State           string    // LV 状态（creating, created, mounting, mounted 等）
    PrevState       string    // 前一个状态（用于回退）
    
    // 挂载信息
    MountPoint      string
    IsFormatted     bool
    
    // 容器信息
    CurrentKey      string    // 当前 snapshot key
    RefCount        int       // 引用计数
    
    // 操作信息
    OperationStartTime time.Time
    LastError       string
    RetryCount      int
    
    // 时间戳
    CreatedTime     time.Time
    UpdatedTime     time.Time
    
    // LV 属性
    Capacity        string
}

// 操作
- AddLV()
- GetLV()
- UpdateLV()
- RemoveLV()
- WalkLVs()
```

---

## 二、Prepare() 的完整实现

### 2.1 主函数（包含状态机）

```go
func (o *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
    log.G(ctx).Infof("Prepare: key=%s, parent=%s", key, parent)
    
    // ========== 步骤 1：解析 labels，判断是否是 devbox ==========
    base := snapshots.Info{}
    for _, opt := range opts {
        if err := opt(&base); err != nil {
            return nil, err
        }
    }
    
    contentID := base.Labels["devbox.content-id"]
    capacity := base.Labels["devbox.capacity"]
    
    // 非 devbox，使用原来的逻辑
    if contentID == "" {
        return o.prepareNormalSnapshot(ctx, key, parent, opts)
    }
    
    // Devbox snapshot
    lvName := "devbox-" + contentID
    
    // ========== 步骤 2：检查 LV 状态（从 devbox.db）==========
    currentState, lvInfo, err := o.getLVState(ctx, lvName)
    if err != nil {
        return nil, fmt.Errorf("failed to get LV state: %w", err)
    }
    
    log.G(ctx).Infof("Prepare: LV %s current state: %s", lvName, currentState)
    
    // ========== 步骤 3：根据状态决定操作 ==========
    switch currentState {
    case LVStateNone:
        // 第一个容器：创建 LV
        return o.prepareFirstContainer(ctx, key, parent, contentID, capacity, opts)
        
    case LVStateCreated:
        // 后续容器：LV 已存在，直接挂载
        return o.prepareSubsequentContainer(ctx, key, parent, contentID, lvInfo, opts)
        
    case LVStateMounting:
        // 上次挂载被中断，恢复
        return o.prepareFromMounting(ctx, key, lvInfo, opts)
        
    case LVStateMounted:
        // 已挂载（重复调用或未清理）
        return o.prepareFromMounted(ctx, key, lvInfo, opts)
        
    default:
        return nil, fmt.Errorf("unexpected LV state: %s", currentState)
    }
}
```

---

### 2.2 场景 1：第一个容器（创建 LV）

**核心流程**：
```
1. metadata.db 事务：创建 snapshot 记录
2. devbox.db 事务：创建 LV 记录，标记 creating
3. 物理操作：创建 LV（事务外）
4. devbox.db 事务：标记 created
5. 物理操作：格式化（事务外）
6. devbox.db 事务：标记 mounting
7. 物理操作：挂载（事务外）
8. devbox.db 事务：标记 mounted
```

**完整代码**：

```go
func (o *Snapshotter) prepareFirstContainer(ctx context.Context, key, parent, contentID, capacity string, opts []snapshots.Opt) ([]mount.Mount, error) {
    lvName := "devbox-" + contentID
    snapshotDir := filepath.Join(o.root, "snapshots")
    
    log.G(ctx).Info("Prepare: First container, creating LV from scratch")
    
    // ========== 步骤 1：metadata.db 事务 - 创建 snapshot ==========
    var snapID string
    err := o.metaStore.WithTransaction(ctx, true, func(ctx context.Context) error {
        snap, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, parent, opts...)
        if err != nil {
            return err
        }
        snapID = snap.ID
        return nil
    })
    if err != nil {
        return nil, fmt.Errorf("failed to create snapshot in metadata.db: %w", err)
    }
    log.G(ctx).Infof("Prepare: Created snapshot in metadata.db, snapID=%s", snapID)
    
    // ========== 步骤 2：devbox.db 事务 - 创建 LV 记录，标记 creating ==========
    err = o.devboxMetadata.WithTransaction(ctx, true, func(ctx context.Context) error {
        return devboxStorage.AddLV(ctx, &LVInfo{
            Name:               lvName,
            ContentID:          contentID,
            State:              LVStateCreating,
            PrevState:          LVStateNone,
            CurrentKey:         key,
            Capacity:           capacity,
            IsFormatted:        false,
            RefCount:           1,
            OperationStartTime: time.Now(),
            CreatedTime:        time.Now(),
        })
    })
    if err != nil {
        // ========== 回退：删除 snapshot（补偿事务）==========
        log.G(ctx).WithError(err).Error("Failed to create LV record, rolling back snapshot")
        o.metaStore.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.Remove(ctx, key)
        })
        return nil, fmt.Errorf("failed to create LV record in devbox.db: %w", err)
    }
    log.G(ctx).Info("Prepare: Created LV record in devbox.db, state=creating")
    
    // ========== 步骤 3：物理操作 - 创建 LV（事务外）==========
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{Capacity: capacity, VolGroup: o.lvmVgName},
    }
    
    createErr := lvm.CreateVolume(ctx, vol)
    if createErr != nil {
        log.G(ctx).WithError(createErr).Error("Failed to create LV")
        
        // ========== 回退：删除两个数据库的记录 ==========
        // 1. 删除 devbox.db 记录
        o.devboxMetadata.WithTransaction(ctx, true, func(ctx context.Context) error {
            return devboxStorage.RemoveLV(ctx, lvName)
        })
        
        // 2. 删除 metadata.db 记录
        o.metaStore.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.Remove(ctx, key)
        })
        
        // 3. 尝试清理可能的僵尸 LV
        _ = lvm.ForceDestroyVolume(ctx, vol)
        
        return nil, fmt.Errorf("failed to create LV: %w", createErr)
    }
    log.G(ctx).Info("Prepare: LV created successfully")
    
    // ========== 步骤 4：devbox.db 事务 - 标记 created ==========
    err = o.devboxMetadata.WithTransaction(ctx, true, func(ctx context.Context) error {
        return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateCreated
            lv.UpdatedTime = time.Now()
        })
    })
    if err != nil {
        log.G(ctx).WithError(err).Warn("Failed to update LV state to created")
        // 不回退，继续执行（状态更新失败不影响功能）
    }
    
    // ========== 步骤 5：格式化并挂载 ==========
    return o.formatAndMountLV(ctx, key, lvName, snapID, true)
}
```

---

### 2.3 格式化并挂载的实现

```go
func (o *Snapshotter) formatAndMountLV(ctx context.Context, key, lvName, snapID string, needFormat bool) ([]mount.Mount, error) {
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    snapshotDir := filepath.Join(o.root, "snapshots")
    tempDir := filepath.Join(snapshotDir, "temp-"+snapID)
    finalDir := filepath.Join(snapshotDir, snapID)
    
    // ========== 步骤 1：devbox.db 事务 - 标记 mounting ==========
    err := o.devboxMetadata.WithTransaction(ctx, true, func(ctx context.Context) error {
        return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.PrevState = lv.State  // created
            lv.State = LVStateMounting
            lv.MountPoint = tempDir
            lv.OperationStartTime = time.Now()
        })
    })
    if err != nil {
        return nil, fmt.Errorf("failed to mark as mounting: %w", err)
    }
    log.G(ctx).Info("Prepare: Marked LV as mounting")
    
    // ========== 步骤 2：创建临时挂载目录 ==========
    if err := os.MkdirAll(tempDir, 0755); err != nil {
        o.rollbackLVState(ctx, lvName, LVStateCreated, err)
        return nil, fmt.Errorf("failed to create temp dir: %w", err)
    }
    
    // ========== 步骤 3：物理操作 - 格式化（如果需要）==========
    if needFormat {
        log.G(ctx).Info("Prepare: Formatting LV (first container)")
        formatErr := o.formatWithRetry(ctx, devicePath, 3)
        if formatErr != nil {
            log.G(ctx).WithError(formatErr).Error("Failed to format LV")
            
            // 回退到 created
            o.devboxMetadata.WithTransaction(ctx, true, func(ctx context.Context) error {
                return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                    lv.State = LVStateCreated
                    lv.LastError = formatErr.Error()
                })
            })
            
            os.RemoveAll(tempDir)
            return nil, fmt.Errorf("failed to format LV: %w", formatErr)
        }
        
        // 标记已格式化
        o.devboxMetadata.WithTransaction(ctx, true, func(ctx context.Context) error {
            return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.IsFormatted = true
            })
        })
    }
    
    // ========== 步骤 4：物理操作 - 挂载到临时目录 ==========
    mountErr := o.mountWithRetry(ctx, devicePath, tempDir, 3)
    if mountErr != nil {
        log.G(ctx).WithError(mountErr).Error("Failed to mount LV")
        
        // 回退到 created
        o.devboxMetadata.WithTransaction(ctx, true, func(ctx context.Context) error {
            return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.State = LVStateCreated
                lv.LastError = mountErr.Error()
            })
        })
        
        os.RemoveAll(tempDir)
        return nil, fmt.Errorf("failed to mount LV: %w", mountErr)
    }
    log.G(ctx).Info("Prepare: LV mounted to temp dir")
    
    // ========== 步骤 5：创建 overlayfs 结构 ==========
    fsDir := filepath.Join(tempDir, "fs")
    workDir := filepath.Join(tempDir, "work")
    if err := os.MkdirAll(fsDir, 0755); err != nil {
        o.unmountWithRetry(ctx, tempDir, 1)
        o.rollbackLVState(ctx, lvName, LVStateCreated, err)
        os.RemoveAll(tempDir)
        return nil, fmt.Errorf("failed to create fs dir: %w", err)
    }
    if err := os.MkdirAll(workDir, 0755); err != nil {
        o.unmountWithRetry(ctx, tempDir, 1)
        o.rollbackLVState(ctx, lvName, LVStateCreated, err)
        os.RemoveAll(tempDir)
        return nil, fmt.Errorf("failed to create work dir: %w", err)
    }
    
    // ========== 步骤 6：卸载临时目录，重命名，重新挂载到最终位置 ==========
    if err := o.unmountWithRetry(ctx, tempDir, 3); err != nil {
        log.G(ctx).WithError(err).Error("Failed to unmount from temp dir")
        o.rollbackLVState(ctx, lvName, LVStateCreated, err)
        os.RemoveAll(tempDir)
        return nil, fmt.Errorf("failed to unmount: %w", err)
    }
    
    if err := os.Rename(tempDir, finalDir); err != nil {
        o.rollbackLVState(ctx, lvName, LVStateCreated, err)
        os.RemoveAll(tempDir)
        return nil, fmt.Errorf("failed to rename: %w", err)
    }
    
    // 挂载到最终位置
    mountErr = o.mountWithRetry(ctx, devicePath, finalDir, 3)
    if mountErr != nil {
        log.G(ctx).WithError(mountErr).Error("Failed to mount to final dir")
        
        // 回退到 created
        o.devboxMetadata.WithTransaction(ctx, true, func(ctx context.Context) error {
            return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.State = LVStateCreated
                lv.LastError = mountErr.Error()
            })
        })
        
        os.RemoveAll(finalDir)
        return nil, fmt.Errorf("failed to mount to final dir: %w", mountErr)
    }
    
    // ========== 步骤 7：devbox.db 事务 - 标记 mounted ==========
    err = o.devboxMetadata.WithTransaction(ctx, true, func(ctx context.Context) error {
        return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateMounted
            lv.MountPoint = finalDir
            lv.LastError = ""
            lv.RetryCount = 0
            lv.UpdatedTime = time.Now()
        })
    })
    if err != nil {
        log.G(ctx).WithError(err).Warn("Failed to update LV state to mounted")
        // 不回退，继续返回（状态更新失败不影响功能）
    }
    
    log.G(ctx).Infof("Prepare: Successfully mounted LV to %s", finalDir)
    
    // ========== 步骤 8：返回挂载信息 ==========
    return o.mounts(snapID), nil
}

func (o *Snapshotter) mounts(id string) []mount.Mount {
    snapshotDir := filepath.Join(o.root, "snapshots", id)
    return []mount.Mount{{
        Type:    "bind",
        Source:  snapshotDir,
        Options: []string{"rbind"},
    }}
}
```

---

### 2.4 场景 2：后续容器（LV 已存在）

```go
func (o *Snapshotter) prepareSubsequentContainer(ctx context.Context, key, parent, contentID string, lvInfo *LVInfo, opts []snapshots.Opt) ([]mount.Mount, error) {
    lvName := lvInfo.Name
    
    log.G(ctx).Infof("Prepare: Subsequent container, LV already exists (refCount=%d)", lvInfo.RefCount)
    
    // ========== 步骤 1：metadata.db 事务 - 创建 snapshot ==========
    var snapID string
    err := o.metaStore.WithTransaction(ctx, true, func(ctx context.Context) error {
        snap, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, parent, opts...)
        if err != nil {
            return err
        }
        snapID = snap.ID
        return nil
    })
    if err != nil {
        return nil, fmt.Errorf("failed to create snapshot: %w", err)
    }
    
    // ========== 步骤 2：devbox.db 事务 - 更新 LV 记录 ==========
    err = o.devboxMetadata.WithTransaction(ctx, true, func(ctx context.Context) error {
        return devboxStorage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.LastKey = lv.CurrentKey
            lv.CurrentKey = key
            lv.RefCount++
            lv.UpdatedTime = time.Now()
        })
    })
    if err != nil {
        // 回退：删除 snapshot
        log.G(ctx).WithError(err).Error("Failed to update LV record")
        o.metaStore.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.Remove(ctx, key)
        })
        return nil, fmt.Errorf("failed to update LV record: %w", err)
    }
    
    // ========== 步骤 3：挂载（不需要格式化）==========
    return o.formatAndMountLV(ctx, key, lvName, snapID, false)  // needFormat=false
}
```

---

## 三、事务顺序总结

### 3.1 创建第一个容器的完整事务序列

```
┌─────────────────────────────────────────────────────────────┐
│ Phase 1: 准备阶段（在事务内）                                  │
├─────────────────────────────────────────────────────────────┤
│ T1: metadata.db 事务                                          │
│     └─ CreateSnapshot(key, parent, opts)                     │
│                                                              │
│ T2: devbox.db 事务                                            │
│     └─ AddLV(lvName, state=creating)                         │
│                                                              │
│     如果失败 → 回退 T1（删除 snapshot）                         │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│ Phase 2: 物理操作（在事务外）                                  │
├─────────────────────────────────────────────────────────────┤
│ LVM 操作：创建 LV                                             │
│                                                              │
│     如果失败 → 回退 T1 + T2（删除两个数据库的记录）              │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│ Phase 3: 状态更新（在事务内）                                  │
├─────────────────────────────────────────────────────────────┤
│ T3: devbox.db 事务                                            │
│     └─ UpdateLV(lvName, state=created)                       │
│                                                              │
│     如果失败 → 记录日志，继续（不影响功能）                      │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│ Phase 4: 格式化和挂载（交替：事务 + 物理操作）                    │
├─────────────────────────────────────────────────────────────┤
│ T4: devbox.db 事务                                            │
│     └─ UpdateLV(lvName, state=mounting)                      │
│                                                              │
│ 物理操作：格式化（如果需要）                                    │
│                                                              │
│     如果失败 → T5: devbox.db 事务                             │
│                └─ UpdateLV(lvName, state=created) [回退]     │
│                                                              │
│ 物理操作：挂载                                                │
│                                                              │
│     如果失败 → T6: devbox.db 事务                             │
│                └─ UpdateLV(lvName, state=created) [回退]     │
│                                                              │
│ T7: devbox.db 事务                                            │
│     └─ UpdateLV(lvName, state=mounted)                       │
└─────────────────────────────────────────────────────────────┘
```

### 3.2 关键原则

1. **metadata.db 优先**
   - 先创建 snapshot 记录（T1）
   - 再创建 LV 记录（T2）
   - 原因：snapshot 是必须的，LV 是可选的

2. **失败回退顺序**
   - T2 失败 → 回退 T1
   - 物理操作失败 → 回退 T1 + T2
   - 状态更新失败 → 不回退（记录日志）

3. **事务快速提交**
   - 每个事务只做元数据操作（毫秒级）
   - 物理操作在事务外（秒级或分钟级）
   - 避免长时间持有锁

4. **状态更新失败的处理**
   - 状态更新失败不影响功能
   - 启动时检查中间状态并恢复
   - 不回退已成功的物理操作

---

## 四、回退策略详解

### 4.1 各阶段的回退处理

| 失败阶段 | 已完成操作 | 回退操作 | 最终状态 |
|---------|-----------|---------|---------|
| T1 失败 | 无 | 直接返回错误 | 无记录 |
| T2 失败 | T1 | 删除 snapshot（T1 回退） | 无记录 |
| 创建 LV 失败 | T1, T2 | 删除 snapshot + LV 记录 | 无记录 |
| T3 失败 | T1, T2, LV | 记录日志，继续 | LV 创建成功但状态是 creating |
| 格式化失败 | T1-T4, LV | 状态回退到 created | LV 存在，未格式化 |
| 挂载失败 | T1-T4, LV, 格式化 | 状态回退到 created | LV 存在，已格式化 |

### 4.2 补偿事务模式

```go
// 标准的补偿事务模式
func (o *Snapshotter) prepareFirstContainer(...) {
    // Phase 1: 事务 1
    err := o.metaStore.WithTransaction(ctx, true, func(ctx) error {
        // 创建 snapshot
    })
    if err != nil {
        return err  // 失败，直接返回
    }
    
    // Phase 2: 事务 2
    err = o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
        // 创建 LV 记录
    })
    if err != nil {
        // ========== 补偿事务：回退 Phase 1 ==========
        o.metaStore.WithTransaction(ctx, true, func(ctx) error {
            return storage.Remove(ctx, key)  // 删除 snapshot
        })
        return err
    }
    
    // Phase 3: 物理操作
    createErr := lvm.CreateVolume(ctx, vol)
    if createErr != nil {
        // ========== 补偿事务：回退 Phase 1 + 2 ==========
        o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
            return devboxStorage.RemoveLV(ctx, lvName)  // 删除 LV 记录
        })
        o.metaStore.WithTransaction(ctx, true, func(ctx) error {
            return storage.Remove(ctx, key)  // 删除 snapshot
        })
        return createErr
    }
    
    // 继续...
}
```

---

## 五、完整代码示例（精简版）

```go
// Snapshotter 结构体
type Snapshotter struct {
    root            string
    metaStore       *storage.MetaStore        // metadata.db
    devboxMetadata  *devbox.DevboxMetadata    // devbox.db
    lvmVgName       string
}

// Prepare 主函数
func (o *Snapshotter) Prepare(ctx, key, parent string, opts) ([]mount.Mount, error) {
    // 1. 解析 labels
    contentID, capacity := parseLabels(opts)
    if contentID == "" {
        return o.prepareNormalSnapshot(ctx, key, parent, opts)
    }
    
    // 2. 检查 LV 状态
    lvName := "devbox-" + contentID
    state, lvInfo, _ := o.getLVState(ctx, lvName)
    
    // 3. 根据状态分发
    switch state {
    case LVStateNone:
        return o.prepareFirstContainer(ctx, key, parent, contentID, capacity, opts)
    case LVStateCreated:
        return o.prepareSubsequentContainer(ctx, key, parent, contentID, lvInfo, opts)
    // ... 其他状态
    }
}

// 第一个容器
func (o *Snapshotter) prepareFirstContainer(ctx, key, parent, contentID, capacity, opts) {
    lvName := "devbox-" + contentID
    
    // T1: metadata.db - 创建 snapshot
    var snapID string
    o.metaStore.WithTransaction(ctx, true, func(ctx) {
        snap, _ := storage.CreateSnapshot(ctx, snapshots.KindActive, key, parent, opts)
        snapID = snap.ID
    })
    
    // T2: devbox.db - 创建 LV 记录
    err := o.devboxMetadata.WithTransaction(ctx, true, func(ctx) {
        devboxStorage.AddLV(ctx, &LVInfo{
            Name: lvName, State: LVStateCreating, CurrentKey: key,
        })
    })
    if err != nil {
        // 回退 T1
        o.metaStore.WithTransaction(ctx, true, func(ctx) {
            storage.Remove(ctx, key)
        })
        return err
    }
    
    // 创建 LV（事务外）
    createErr := lvm.CreateVolume(ctx, vol)
    if createErr != nil {
        // 回退 T1 + T2
        o.devboxMetadata.WithTransaction(ctx, true, func(ctx) {
            devboxStorage.RemoveLV(ctx, lvName)
        })
        o.metaStore.WithTransaction(ctx, true, func(ctx) {
            storage.Remove(ctx, key)
        })
        return createErr
    }
    
    // T3: devbox.db - 标记 created
    o.devboxMetadata.WithTransaction(ctx, true, func(ctx) {
        devboxStorage.UpdateLV(ctx, lvName, func(lv) {
            lv.State = LVStateCreated
        })
    })
    
    // 格式化并挂载
    return o.formatAndMountLV(ctx, key, lvName, snapID, true)
}
```

---

## 六、总结

### 6.1 双数据库的优势

1. **职责分离**
   - metadata.db: containerd 标准元数据
   - devbox.db: LV 状态和管理信息

2. **互不阻塞**
   - 两个数据库独立
   - 事务互不影响

3. **状态独立管理**
   - LV 状态在 devbox.db
   - 不污染 metadata.db

### 6.2 事务顺序的关键点

1. **创建顺序**：metadata.db → devbox.db → 物理操作
2. **回退顺序**：反向回退（后创建的先删除）
3. **状态更新**：失败不影响功能，启动时恢复

### 6.3 实施建议

**Phase 1**：
1. 实现 DevboxMetadata（devbox.db）
2. 修改 Prepare 使用双数据库
3. 实现基本的回退逻辑

**Phase 2**：
4. 实现状态恢复（启动时检查）
5. 完善错误处理
6. 添加引用计数

**Phase 3**：
7. 性能优化
8. 监控和日志
9. 完善测试

