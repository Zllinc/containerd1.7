# 事件溯源方案：完整的失败处理与资源清理

## 一、问题分析

### 1.1 OverlayFS Snapshotter 的成功经验

```go
// OverlayFS Snapshotter 的做法（在事务内）
func (o *Snapshotter) Prepare(ctx, key, parent string, opts) ([]mount.Mount, error) {
    return o.ms.WithTransaction(ctx, true, func(ctx) error {
        // 1. 在元数据中创建 snapshot 记录
        snap, err := storage.CreateSnapshot(ctx, KindActive, key, parent, opts...)

        // 2. 创建临时目录（在事务内）
        tmpDir := filepath.Join(o.root, "snapshots", "tmp-"+snap.ID)
        if err := os.MkdirAll(tmpDir, 0755); err != nil {
            return err  // ← 事务回退，元数据删除
        }

        // 3. 执行一些快速操作（创建目录等）
        ...

        // 4. 重命名到最终位置（在事务内）
        finalDir := filepath.Join(o.root, "snapshots", snap.ID)
        if err := os.Rename(tmpDir, finalDir); err != nil {
            return err  // ← 事务回退，临时目录会被 GC 清理
        }

        return nil
    })
    // ← 事务提交，所有变更生效
}
```

**优势**：
- ✅ 所有操作都在事务内
- ✅ 失败时事务自动回退，元数据一致
- ✅ 使用临时目录，失败后 GC 会自动清理
- ✅ 状态一致性保证强

**为什么我们能这样做**：
- 创建目录是**快速操作**（毫秒级）
- 所有操作都可以**在事务内**完成
- 事务持锁时间很短

### 1.2 我们场景的特殊性

```go
// ❌ 如果我们照搬 OverlayFS 的做法
func (o *Snapshotter) Prepare(ctx, key, parent string, opts) ([]mount.Mount, error) {
    return o.ms.WithTransaction(ctx, true, func(ctx) error {
        // 1. 创建 snapshot 元数据（快速，几毫秒）
        snap, err := storage.CreateSnapshot(...)

        // 2. 创建 LV（慢速，可能数秒到数分钟！）
        if err := lvm.CreateVolume(ctx, vol); err != nil {
            return err  // ← 但此时已经持锁数分钟了！
        }

        // 3. 格式化 LV（慢速，可能数十秒）
        if err := mkfs(devicePath); err != nil {
            return err  // ← 继续持锁！
        }

        // 4. 挂载 LV（慢速，可能数十秒）
        if err := mount(...); err != nil {
            return err  // ← 继续持锁！
        }

        return nil
    })
}
```

**问题**：
- ❌ 事务持锁时间过长（数分钟）
- ❌ 其他操作被阻塞
- ❌ 并发性能极差
- ❌ 违背了事务的设计初衷（快速提交）

### 1.3 核心矛盾

| 机制 | 要求 | 我们的场景 | 矛盾 |
|------|------|-----------|------|
| ACID 事务 | 快速提交（毫秒级） | LVM/文件系统操作慢（秒到分钟级） | ❌ 不匹配 |
| 事务回退 | 自动、原子 | 慢速操作在事务外，无法自动回退 | ❌ 不支持 |
| 资源清理 | 失败后自动清理 | 僵尸资源需要手动清理 | ❌ 复杂 |

---

## 二、解决方案：补偿事务 + 自动清理

### 2.1 核心思想

**关键洞察**：既然无法在事务内完成所有操作，那就：
1. **事务内完成快速操作**（元数据、事件追加）
2. **事务外执行慢速操作**（LVM、文件系统）
3. **失败时手动回退**（补偿事务）
4. **清理机制兜底**（Cleanup 定期扫描僵尸资源）

### 2.2 完整的 Prepare 实现（带回退）

```go
func (o *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
    log.G(ctx).Infof("Prepare: key=%s, parent=%s", key, parent)

    // ========== 步骤 1：解析 labels ==========
    base := snapshots.Info{}
    for _, opt := range opts {
        if err := opt(&base); err != nil {
            return nil, err
        }
    }

    contentID := base.Labels["devbox.content-id"]
    capacity := base.Labels["devbox.capacity"]

    if contentID == "" {
        return o.prepareNormalSnapshot(ctx, key, parent, opts)
    }

    lvName := "devbox-" + contentID

    // ========== 步骤 2：metadata.db 事务 - 创建 snapshot ==========
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

    mountPoint := filepath.Join(o.root, "snapshots", snapID)

    // ========== 步骤 3：EventStore 事务 - 追加初始事件 ==========
    err = o.eventStore.WithTransaction(ctx, true, func(ctx context.Context) error {
        requestEvent := Event{
            ID:        generateEventID(),
            LVName:    lvName,
            Type:      EventLVRequested,
            Timestamp: time.Now(),
            Status:    EventStatusPending,
            Data: encodeJSON(map[string]interface{}{
                "content_id":  contentID,
                "capacity":    capacity,
                "mount_point": mountPoint,
                "key":         key,  // ← 记录 snapshot key，用于回退
            }),
        }
        return o.eventStore.Append(ctx, requestEvent)
    })
    if err != nil {
        // 回退：删除 snapshot（补偿事务）
        log.G(ctx).WithError(err).Warn("Failed to append event, rolling back snapshot")
        o.metaStore.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.Remove(ctx, key)
        })
        return nil, fmt.Errorf("failed to append event: %w", err)
    }

    // ========== 步骤 4：同步创建 LV（事务外）==========
    // 使用 defer 确保失败时清理
    var lvCreated bool
    var lvFormatted bool
    var lvMounted bool

    defer func() {
        if err != nil {
            // 失败了，执行清理
            log.G(ctx).WithError(err).Error("Prepare failed, cleaning up resources")

            // 清理顺序：先卸载，再删除 LV，最后删除元数据
            if lvMounted {
                // 卸载 LV
                if unmountErr := syscall.Unmount(mountPoint, 0); unmountErr != nil {
                    log.G(ctx).WithError(unmountErr).Warn("Failed to unmount during cleanup")
                }
            }

            if lvFormatted || lvCreated {
                // 删除 LV
                vol := &apis.LVMVolume{
                    ObjectMeta: metav1.ObjectMeta{Name: lvName},
                    Spec:       apis.VolumeInfo{Capacity: capacity, VolGroup: o.lvmVgName},
                }
                if removeErr := lvm.ForceDestroyVolume(ctx, vol); removeErr != nil {
                    log.G(ctx).WithError(removeErr).Warn("Failed to remove LV during cleanup")
                    // 追加清理事件，让 Cleanup 处理
                    o.eventStore.Append(context.Background(), Event{
                        ID:        generateEventID(),
                        LVName:    lvName,
                        Type:      EventCleanupRequested, // 新事件类型
                        Timestamp: time.Now(),
                        Status:    EventStatusPending,
                        Data: encodeJSON(map[string]interface{}{
                            "reason": "prepare failed",
                            "key":    key,
                        }),
                    })
                }
            }

            // 删除 snapshot 元数据（补偿事务）
            if removeErr := o.metaStore.WithTransaction(context.Background(), true, func(ctx context.Context) error {
                return storage.Remove(ctx, key)
            }); removeErr != nil {
                log.G(ctx).WithError(removeErr).Error("Failed to remove snapshot during cleanup")
            }

            // 追加失败事件
            o.eventStore.Append(context.Background(), Event{
                ID:        generateEventID(),
                LVName:    lvName,
                Type:      EventPrepareFailed, // 新事件类型
                Timestamp: time.Now(),
                Status:    EventStatusCompleted,
                Data: encodeJSON(map[string]interface{}{
                    "error": err.Error(),
                    "key":   key,
                }),
            })
        }
    }()

    // ========== Phase 1: 创建 LV ==========
    log.G(ctx).Infof("Prepare: Creating LV %s", lvName)

    o.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    lvName,
        Type:      EventLVRequested,
        Timestamp: time.Now(),
        Status:    EventStatusExecuting,
    })

    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{Capacity: capacity, VolGroup: o.lvmVgName},
    }

    maxRetries := 3
    for i := 0; i < maxRetries; i++ {
        err = lvm.CreateVolume(ctx, vol)
        if err == nil {
            lvCreated = true  // ← 标记 LV 已创建
            break
        }

        if !isRetriableError(err) {
            break  // 不可重试，直接退出
        }

        log.G(ctx).Warnf("LV creation failed (attempt %d/%d): %v", i+1, maxRetries, err)
        time.Sleep(time.Second * time.Duration(i+1))
    }

    if err != nil {
        err = fmt.Errorf("failed to create LV after %d retries: %w", maxRetries, err)
        o.eventStore.Append(ctx, Event{
            ID:        generateEventID(),
            LVName:    lvName,
            Type:      EventLVCreateFailed,
            Timestamp: time.Now(),
            Status:    EventStatusCompleted,
            Data: encodeJSON(map[string]interface{}{
                "error": err.Error(),
            }),
        })
        return nil, err  // ← defer 会清理资源
    }

    o.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    lvName,
        Type:      EventLVCreated,
        Timestamp: time.Now(),
        Status:    EventStatusCompleted,
    })

    // ========== Phase 2: 格式化 LV ==========
    log.G(ctx).Infof("Prepare: Formatting LV %s", lvName)

    o.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    lvName,
        Type:      EventFormatRequested,
        Timestamp: time.Now(),
        Status:    EventStatusExecuting,
    })

    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)

    for i := 0; i < maxRetries; i++ {
        err = mkfs(devicePath)
        if err == nil {
            lvFormatted = true  // ← 标记 LV 已格式化
            break
        }

        if !isRetriableError(err) {
            break
        }

        log.G(ctx).Warnf("Format failed (attempt %d/%d): %v", i+1, maxRetries, err)
        time.Sleep(time.Second * time.Duration(i+1))
    }

    if err != nil {
        err = fmt.Errorf("failed to format LV after %d retries: %w", maxRetries, err)
        o.eventStore.Append(ctx, Event{
            ID:        generateEventID(),
            LVName:    lvName,
            Type:      EventFormatFailed,
            Timestamp: time.Now(),
            Status:    EventStatusCompleted,
            Data: encodeJSON(map[string]interface{}{
                "error": err.Error(),
            }),
        })
        return nil, err  // ← defer 会清理资源（包括 LV）
    }

    o.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    lvName,
        Type:      EventFormatted,
        Timestamp: time.Now(),
        Status:    EventStatusCompleted,
    })

    // ========== Phase 3: 挂载 LV ==========
    log.G(ctx).Infof("Prepare: Mounting LV %s to %s", lvName, mountPoint)

    o.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    lvName,
        Type:      EventMountRequested,
        Timestamp: time.Now(),
        Status:    EventStatusExecuting,
    })

    // 创建挂载点目录
    if err := os.MkdirAll(mountPoint, 0755); err != nil {
        err = fmt.Errorf("failed to create mount directory: %w", err)
        o.eventStore.Append(ctx, Event{
            ID:        generateEventID(),
            LVName:    lvName,
            Type:      EventMountFailed,
            Timestamp: time.Now(),
            Status:    EventStatusCompleted,
            Data: encodeJSON(map[string]interface{}{
                "error": err.Error(),
            }),
        })
        return nil, err  // ← defer 会清理资源（包括 LV）
    }

    // 挂载
    for i := 0; i < maxRetries; i++ {
        err = syscall.Mount(devicePath, mountPoint, "ext4", 0, "")
        if err == nil {
            lvMounted = true  // ← 标记 LV 已挂载
            break
        }

        if !isRetriableError(err) {
            break
        }

        log.G(ctx).Warnf("Mount failed (attempt %d/%d): %v", i+1, maxRetries, err)
        time.Sleep(time.Second * time.Duration(i+1))
    }

    if err != nil {
        err = fmt.Errorf("failed to mount LV after %d retries: %w", maxRetries, err)
        o.eventStore.Append(ctx, Event{
            ID:        generateEventID(),
            LVName:    lvName,
            Type:      EventMountFailed,
            Timestamp: time.Now(),
            Status:    EventStatusCompleted,
            Data: encodeJSON(map[string]interface{}{
                "error": err.Error(),
            }),
        })
        return nil, err  // ← defer 会清理资源（包括挂载点、LV）
    }

    o.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    lvName,
        Type:      EventMounted,
        Timestamp: time.Now(),
        Status:    EventStatusCompleted,
        Data: encodeJSON(map[string]interface{}{
            "mount_point": mountPoint,
        }),
    })

    // ========== 步骤 5：使 Projection 失效 ==========
    o.projection.Invalidate(lvName)

    log.G(ctx).Infof("Prepare: LV %s created and mounted successfully", lvName)

    // 成功了，defer 不会执行清理
    return []mount.Mount{{
        Type:    "bind",
        Source:  mountPoint,
        Options: []string{"rbind"},
    }}, nil
}
```

---

## 三、各失败场景的详细处理

### 3.1 场景1：LV 创建失败

**失败时状态**：
- 元数据：snapshot 已创建
- 事件：lv_requested (executing) → lv_create_failed (completed)
- 物理资源：可能存在僵尸 LV（部分创建）

**清理逻辑**（defer 中执行）：

```go
// lvCreated = false（创建失败）

defer func() {
    if err != nil {
        // 1. 卸载：不需要（lvMounted = false）

        // 2. 删除 LV：尝试删除僵尸 LV（如果有）
        if lvCreated {
            vol := &apis.LVMVolume{...}
            if removeErr := lvm.ForceDestroyVolume(ctx, vol); removeErr != nil {
                // 删除失败，追加清理事件
                o.eventStore.Append(context.Background(), Event{
                    Type: EventCleanupRequested,
                    Data: encodeJSON(map[string]interface{}{
                        "reason": "lv create failed, cleanup needed",
                        "key":    key,
                    }),
                })
            }
        }

        // 3. 删除 snapshot 元数据（补偿事务）
        o.metaStore.WithTransaction(context.Background(), true, func(ctx) error {
            return storage.Remove(ctx, key)
        })

        // 4. 追加失败事件
        o.eventStore.Append(context.Background(), Event{
            Type: EventPrepareFailed,
            Data: encodeJSON(map[string]interface{}{
                "error": err.Error(),
                "key":   key,
            }),
        })
    }
}()
```

**结果**：
- ✅ 元数据已清理（snapshot 已删除）
- ✅ 僵尸 LV 已删除（或标记为待清理）
- ✅ 事件日志完整（可以追溯失败原因）

### 3.2 场景2：格式化失败

**失败时状态**：
- 元数据：snapshot 已创建
- 事件：lv_requested → lv_created → format_requested → format_failed
- 物理资源：LV 已创建但未格式化

**清理逻辑**（defer 中执行）：

```go
// lvCreated = true, lvFormatted = false

defer func() {
    if err != nil {
        // 1. 卸载：不需要（lvMounted = false）

        // 2. 删除 LV（已创建但未格式化，直接删除）
        if lvCreated {
            vol := &apis.LVMVolume{...}
            if removeErr := lvm.ForceDestroyVolume(ctx, vol); removeErr != nil {
                // 删除失败，追加清理事件
                o.eventStore.Append(context.Background(), Event{
                    Type: EventCleanupRequested,
                    Data: encodeJSON(map[string]interface{}{
                        "reason": "format failed, need to remove LV",
                        "key":    key,
                    }),
                })
            }
        }

        // 3. 删除 snapshot 元数据
        o.metaStore.WithTransaction(context.Background(), true, func(ctx) error {
            return storage.Remove(ctx, key)
        })

        // 4. 追加失败事件
        o.eventStore.Append(context.Background(), Event{
            Type: EventPrepareFailed,
            Data: encodeJSON(map[string]interface{}{
                "error": err.Error(),
                "key":   key,
            }),
        })
    }
}()
```

**结果**：
- ✅ 元数据已清理
- ✅ 未格式化的 LV 已删除
- ✅ 事件日志完整

### 3.3 场景3：挂载失败

**失败时状态**：
- 元数据：snapshot 已创建
- 事件：lv_requested → lv_created → format_requested → formatted → mount_requested → mount_failed
- 物理资源：LV 已创建并格式化，挂载点目录已创建，但未挂载

**清理逻辑**（defer 中执行）：

```go
// lvCreated = true, lvFormatted = true, lvMounted = false

defer func() {
    if err != nil {
        // 1. 卸载：不需要（lvMounted = false）

        // 2. 删除 LV（已格式化，直接删除）
        if lvFormatted {
            vol := &apis.LVMVolume{...}
            if removeErr := lvm.ForceDestroyVolume(ctx, vol); removeErr != nil {
                // 删除失败，追加清理事件
                o.eventStore.Append(context.Background(), Event{
                    Type: EventCleanupRequested,
                    Data: encodeJSON(map[string]interface{}{
                        "reason": "mount failed, need to remove LV",
                        "key":    key,
                    }),
                })
            }
        }

        // 3. 删除挂载点目录
        os.RemoveAll(mountPoint)

        // 4. 删除 snapshot 元数据
        o.metaStore.WithTransaction(context.Background(), true, func(ctx) error {
            return storage.Remove(ctx, key)
        })

        // 5. 追加失败事件
        o.eventStore.Append(context.Background(), Event{
            Type: EventPrepareFailed,
            Data: encodeJSON(map[string]interface{}{
                "error": err.Error(),
                "key":   key,
            }),
        })
    }
}()
```

**结果**：
- ✅ 元数据已清理
- ✅ 已格式化的 LV 已删除
- ✅ 挂载点目录已删除
- ✅ 事件日志完整

### 3.4 场景4：进程崩溃（defer 未执行）

**崩溃时状态**：
- 元数据：snapshot 已创建
- 事件：lv_requested → lv_created → format_requested → formatted → mount_requested → (崩溃)
- 物理资源：LV 已创建并格式化，可能已挂载（取决于崩溃时机）

**恢复逻辑**（启动时）：

```go
func (o *Snapshotter) recoverIncompleteOperations(ctx context.Context) error {
    // 查找所有 pending 或 executing 状态的事件
    events, _ := o.eventStore.GetEventsByStatus(ctx, EventStatusPending)
    executingEvents, _ := o.eventStore.GetEventsByStatus(ctx, EventStatusExecuting)
    events = append(events, executingEvents...)

    for _, event := range events {
        // 检查事件类型
        switch event.Type {
        case EventLVRequested, EventFormatRequested, EventMountRequested:
            // 获取事件关联的 key
            var data map[string]interface{}
            json.Unmarshal(event.Data, &data)
            key := data["key"].(string)

            // 检查 snapshot 是否还存在
            _, err := o.metaStore.WithTransaction(ctx, false, func(ctx) error {
                snap, err := storage.GetSnapshot(ctx, key)
                if err != nil {
                    return err  // snapshot 不存在
                }
                snapID := snap.ID
                return nil
            })

            if err != nil {
                // snapshot 不存在，说明已经清理过了
                continue
            }

            // snapshot 还在，需要恢复
            log.G(ctx).Warnf("Found incomplete operation for LV %s, key %s", event.LVName, key)

            // 检查实际状态
            devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, event.LVName)
            deviceExists := checkDeviceExists(devicePath)
            mountPoint := getMountPointFromEvent(event)
            isMounted := checkIsMounted(mountPoint)

            // 根据实际状态决定恢复策略
            if isMounted {
                // 已挂载，补充成功事件
                o.eventStore.Append(ctx, Event{
                    Type:   EventMounted,
                    Status: EventStatusCompleted,
                    Data:   encodeJSON(map[string]interface{}{"mount_point": mountPoint}),
                })
                log.G(ctx).Infof("Recovered: LV %s was already mounted", event.LVName)

            } else if deviceExists {
                // LV 存在但未挂载，需要补充格式化事件（如果需要）并挂载
                log.G(ctx).Infof("Recovered: LV %s exists but not mounted, will mount", event.LVName)

                // 补充 formatted 事件（如果还没有）
                if event.Type == EventMountRequested {
                    o.eventStore.Append(ctx, Event{
                        Type:   EventFormatted,
                        Status: EventStatusCompleted,
                    })
                }

                // 尝试挂载
                if err := mountLV(ctx, devicePath, mountPoint); err != nil {
                    // 挂载失败，清理资源
                    log.G(ctx).WithError(err).Errorf("Failed to mount LV %s during recovery", event.LVName)
                    o.cleanupLV(ctx, event.LVName, key, mountPoint)
                } else {
                    // 挂载成功，补充成功事件
                    o.eventStore.Append(ctx, Event{
                        Type:   EventMounted,
                        Status: EventStatusCompleted,
                        Data:   encodeJSON(map[string]interface{}{"mount_point": mountPoint}),
                    })
                }

            } else {
                // LV 不存在，操作失败，清理资源
                log.G(ctx).Warnf("Recovered: LV %s does not exist, cleaning up", event.LVName)
                o.cleanupLV(ctx, event.LVName, key, "")
            }

        default:
            // 其他类型的事件，暂时忽略
        }
    }

    return nil
}

func (o *Snapshotter) cleanupLV(ctx context.Context, lvName, key, mountPoint string) {
    // 1. 卸载
    if mountPoint != "" {
        if isMounted(mountPoint) {
            syscall.Unmount(mountPoint, 0)
        }
        os.RemoveAll(mountPoint)
    }

    // 2. 删除 LV
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{VolGroup: o.lvmVgName},
    }
    lvm.ForceDestroyVolume(ctx, vol)

    // 3. 删除 snapshot 元数据
    o.metaStore.WithTransaction(context.Background(), true, func(ctx) error {
        return storage.Remove(ctx, key)
    })

    // 4. 追加失败事件
    o.eventStore.Append(context.Background(), Event{
        Type:   EventPrepareFailed,
        Status: EventStatusCompleted,
        Data:   encodeJSON(map[string]interface{}{
            "reason": "crashed during operation",
            "key":    key,
        }),
    })
}
```

**结果**：
- ✅ 启动时自动检测未完成的操作
- ✅ 根据实际状态恢复或清理
- ✅ 元数据和物理资源保持一致

---

## 四、Cleanup：兜底清理机制

### 4.1 Cleanup 函数实现

```go
func (o *Snapshotter) Cleanup(ctx context.Context) error {
    log.G(ctx).Info("Cleanup: Starting periodic cleanup")

    // ========== 1. 处理清理请求事件 ==========
    cleanupEvents, _ := o.eventStore.GetEventsByType(ctx, EventCleanupRequested)
    for _, event := range cleanupEvents {
        var data map[string]interface{}
        json.Unmarshal(event.Data, &data)
        key := data["key"].(string)
        lvName := event.LVName

        log.G(ctx).Infof("Cleanup: Processing cleanup request for LV %s, key %s", lvName, key)

        // 检查 snapshot 是否还存在
        snapExists := false
        o.metaStore.WithTransaction(ctx, false, func(ctx) error {
            _, err := storage.GetSnapshot(ctx, key)
            if err == nil {
                snapExists = true
            }
            return nil
        })

        if snapExists {
            // snapshot 还在，需要删除
            o.metaStore.WithTransaction(ctx, true, func(ctx) error {
                return storage.Remove(ctx, key)
            })
        }

        // 检查 LV 是否存在
        devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
        if checkDeviceExists(devicePath) {
            // 删除 LV
            vol := &apis.LVMVolume{
                ObjectMeta: metav1.ObjectMeta{Name: lvName},
                Spec:       apis.VolumeInfo{VolGroup: o.lvmVgName},
            }
            if err := lvm.ForceDestroyVolume(ctx, vol); err != nil {
                log.G(ctx).WithError(err).Errorf("Failed to remove LV %s during cleanup", lvName)
                // 保留事件，下次重试
                continue
            }
        }

        // 追加清理完成事件
        o.eventStore.Append(ctx, Event{
            Type:   EventCleanupCompleted,
            Status: EventStatusCompleted,
            Data:   encodeJSON(map[string]interface{}{"key": key}),
        })

        log.G(ctx).Infof("Cleanup: Successfully cleaned up LV %s", lvName)
    }

    // ========== 2. 检测僵尸 LV（物理 LV 存在但元数据中没有）==========
    allLVs := lvm.ListAllVolumes(o.lvmVgName)
    for _, lv := range allLVs {
        if !strings.HasPrefix(lv.Name, "devbox-") {
            continue  // 不是 devbox LV，跳过
        }

        // 检查事件日志中是否有这个 LV
        events, _ := o.eventStore.GetEvents(ctx, lv.Name)
        if len(events) == 0 {
            // 没有事件，可能是僵尸 LV
            log.G(ctx).Warnf("Cleanup: Found zombie LV %s (no events)", lv.Name)

            // 检查是否挂载
            mountPoint := findMountPointForLV(lv.Name)
            if mountPoint != "" {
                syscall.Unmount(mountPoint, 0)
                os.RemoveAll(mountPoint)
            }

            // 删除 LV
            if err := lvm.ForceDestroyVolume(ctx, &lv); err != nil {
                log.G(ctx).WithError(err).Errorf("Failed to remove zombie LV %s", lv.Name)
            } else {
                log.G(ctx).Infof("Cleanup: Successfully removed zombie LV %s", lv.Name)
            }
        }
    }

    // ========== 3. 检测僵尸 snapshot（元数据存在但 LV 不存在）==========
    // （这通常不应该发生，但在崩溃后可能出现）
    o.metaStore.WithTransaction(ctx, false, func(ctx) error {
        return storage.WalkSnapshots(ctx, func(info snapshots.Info) error {
            // 检查是否是 devbox snapshot
            contentID := info.Labels["devbox.content-id"]
            if contentID == "" {
                return nil  // 不是 devbox snapshot
            }

            lvName := "devbox-" + contentID

            // 检查 LV 是否存在
            devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
            if !checkDeviceExists(devicePath) {
                // LV 不存在，这是僵尸 snapshot
                log.G(ctx).Warnf("Cleanup: Found zombie snapshot %s (LV %s does not exist)", info.Name, lvName)

                // 删除 snapshot
                return storage.Remove(ctx, info.Name)
            }

            return nil
        })
    })

    log.G(ctx).Info("Cleanup: Completed")
    return nil
}
```

### 4.2 定期执行 Cleanup

```go
func (o *Snapshotter) StartCleanupDaemon(ctx context.Context) {
    ticker := time.NewTicker(5 * time.Minute)  // 每 5 分钟执行一次
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            if err := o.Cleanup(ctx); err != nil {
                log.G(ctx).WithError(err).Error("Cleanup failed")
            }
        }
    }
}

func (o *Snapshotter) New(ctx context.Context, root string, config *Config) (*Snapshotter, error) {
    s := &Snapshotter{...}

    // 启动清理守护进程
    go s.StartCleanupDaemon(ctx)

    return s, nil
}
```

---

## 五、完整的状态转换图（带失败处理）

```
                    [首次创建容器 - 同步执行，带回退]
                         Prepare(key="container1")
                              ↓
              ┌──────────────────────────────────┐
              │  T1: metadata.db 事务             │
              │  - 创建 snapshot 元数据            │
              └────────────┬─────────────────────┘
                           │ 失败
                           ├────────→ 返回错误，无资源残留
                           │ 成功
                           ↓
              ┌──────────────────────────────────┐
              │  T2: EventStore 事务             │
              │  - 追加事件 lv_requested         │
              └────────────┬─────────────────────┘
                           │ 失败
                           ├────────→ 补偿事务：删除 snapshot
                           │         返回错误
                           │ 成功
                           ↓
              ┌──────────────────────────────────┐
              │  物理操作：创建 LV                 │
              │  - lvm.CreateVolume()             │
              │  - 失败则重试 3 次                 │
              └────────────┬─────────────────────┘
                           │
                  ┌────────┴────────┐
                  │                 │
             成功 ✅            失败 ❌
                  │                 │
                  ↓                 ↓
    ┌──────────────────┐   ┌──────────────────────────────────┐
    │ 追加事件：        │   │ defer 清理：                      │
    │ lv_created       │   │ 1. 尝试删除僵尸 LV                │
    │ (completed)      │   │ 2. 补偿事务：删除 snapshot 元数据  │
    └─────────┬────────┘   │ 3. 追加事件：prepare_failed        │
              │            │ 4. 返回错误                        │
              ↓            └──────────────────────────────────┘
    ┌──────────────────┐
    │ 物理操作：格式化 LV│
    │ - mkfs()          │
    │ - 失败则重试 3 次   │
    └─────────┬─────────┘
              │
      ┌───────┴────────┐
      │                │
 成功 ✅          失败 ❌
      │                │
      ↓                ↓
┌──────────────┐  ┌──────────────────────────────────┐
│追加事件：      │  │ defer 清理：                      │
│formatted     │  │ 1. 删除 LV（已创建但未格式化）       │
│(completed)   │  │ 2. 补偿事务：删除 snapshot 元数据  │
└──────┬───────┘  │ 3. 追加事件：prepare_failed        │
       │         │ 4. 返回错误                        │
       ↓         └──────────────────────────────────┘
┌──────────────┐
│物理操作：挂载 LV│
│ - mount()     │
│ - 失败则重试 3次│
└──────┬───────┘
       │
 ┌─────┴────┐
 │         │
成功 ✅  失败 ❌
 │         │
 ↓         ↓
┌────────┐ ┌──────────────────────────────────┐
│追加事件：│ │ defer 清理：                      │
│mounted │ │ 1. 删除 LV（已格式化）              │
│(completed)│ │ 2. 删除挂载点目录                  │
└───┬────┘ │ 3. 补偿事务：删除 snapshot 元数据  │
    │      │ 4. 追加事件：prepare_failed        │
    ↓      │ 5. 返回错误                        │
状态 =    └──────────────────────────────────┘
"mounted"
    │
    ↓
┌──────────────────────────┐
│  使 Projection 失效       │
│  Prepare 返回 mountInfo   │
│  容器可以立即使用         │
└──────────────────────────┘
```

---

## 六、与 OverlayFS 方案的对比

### 6.1 事务使用对比

| 方面 | OverlayFS | Devbox（混合方案） |
|------|-----------|-------------------|
| 事务内操作 | 元数据 + 快速文件操作 | 元数据 + 事件追加 |
| 事务外操作 | 无 | LVM/文件系统操作 |
| 事务持锁时间 | 毫秒级 | 毫秒级（只操作元数据和事件） |
| 失败回退 | 自动（事务回退） | 手动（defer + 补偿事务） |
| 资源清理 | 自动（GC 清理临时目录） | 手动（defer 清理 + Cleanup 兜底） |

### 6.2 复杂度对比

| 方面 | OverlayFS | Devbox（混合方案） |
|------|-----------|-------------------|
| 代码复杂度 | 低（自动回退） | 中（手动回退） |
| 测试复杂度 | 低 | 高（需要测试各种失败场景） |
| 调试难度 | 低 | 中（有完整的事件日志） |
| 维护难度 | 低 | 中 |

### 6.3 为什么我们不能像 OverlayFS 那样？

**原因**：慢速操作的存在

| 操作 | OverlayFS | Devbox |
|------|-----------|--------|
| 创建目录 | 毫秒级，可在事务内 | - |
| 创建 LV | - | 秒到分钟级，不能在事务内 |
| 格式化 | - | 秒级，不能在事务内 |
| 挂载 | 毫秒级（bind mount） | 秒级（ext4 mount） |

**结论**：
- OverlayFS 的所有操作都是快速的，可以在事务内完成
- Devbox 有慢速操作，必须在事务外执行
- 因此需要手动处理回退和清理

---

## 七、总结

### 7.1 核心设计原则

1. **快慢分离**
   - 快速操作（元数据、事件追加）在事务内
   - 慢速操作（LVM、文件系统）在事务外

2. **失败处理**
   - 正常流程：使用 defer 确保失败时清理
   - 崩溃场景：启动时恢复 + Cleanup 兜底

3. **资源清理**
   - 主清理：defer 函数（立即清理）
   - 辅助清理：Cleanup 定期扫描（兜底）
   - 事件日志：记录清理失败，后续重试

4. **元数据一致性**
   - 补偿事务：手动回退元数据
   - 事件日志：记录失败，可追溯

### 7.2 与双数据库方案的对比

| 方面 | 双数据库方案 | 混合事件溯源方案 |
|------|-------------|----------------|
| 元数据存储 | metadata.db + devbox.db | metadata.db + EventStore |
| 状态来源 | 直接存储 | 从事件推导 |
| 失败处理 | 补偿事务（复杂） | defer + 补偿事务（相对简单） |
| 可追溯性 | 弱 | 强（完整事件日志） |
| 资源清理 | 需要手动实现 | 内置（defer + Cleanup） |
| 代码复杂度 | 高（~500行） | 中（~400行） |

### 7.3 实施建议

**Phase 1（必须）**：
1. 实现 EventStore
2. 实现带 defer 清理的 Prepare
3. 实现启动时恢复

**Phase 2（重要）**：
4. 实现 Cleanup
5. 实现定期清理守护进程

**Phase 3（可选）**：
6. 添加监控和告警
7. 优化重试策略
8. 添加性能指标

这个方案在保证元数据一致性和资源清理的前提下，最大程度简化了错误处理逻辑。
