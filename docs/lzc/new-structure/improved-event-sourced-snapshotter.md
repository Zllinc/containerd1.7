# 改进的事件溯源方案：混合同步/异步模型

## 一、问题回顾

### 1.1 原方案的问题

```go
// ❌ 有问题的设计
func Prepare(...) {
    // 1. metadata.db 事务：创建 snapshot
    // 2. EventStore 事务：追加事件 lv_requested
    return mountPoint  // 立即返回，但 LV 还没创建！
    // 后台异步创建 LV
}

// 问题：
// 1. mountPoint 目录可能是空的或不存在
// 2. 容器立即启动，往 rootfs 写数据，但 LV 还没挂载
// 3. 数据丢失或容器启动失败
```

**根本原因**：containerd 的 Prepare 接口要求返回的 snapshot 必须**立即可用**，不能异步准备。

### 1.2 核心矛盾

| 机制 | 要求 | 矛盾 |
|------|------|------|
| Event Sourcing | 异步执行，追加事件后立即返回 | Prepare 需要同步等待 |
| Snapshotter 接口 | Prepare 返回后 snapshot 必须可用 | 异步执行导致 snapshot 未就绪 |

## 二、改进方案：混合同步/异步模型

### 2.1 核心思想

**关键洞察**：Prepare 的性能瓶颈主要在于**首次创建 LV**，后续容器的挂载操作很快。

**改进策略**：
- **首次创建**：同步执行（LV 创建 + 格式化 + 挂载），但用事件驱动简化错误处理
- **后续挂载**：直接返回已存在的 LV（非常快）
- **卸载/删除**：异步执行（不阻塞 Remove 接口）

### 2.2 三种执行模式

```go
type ExecutionMode int

const (
    // 同步执行：等待操作完成后才返回
    // 适用于：必须立即可用的操作（如 Prepare）
    ModeSync ExecutionMode = iota

    // 异步执行：追加事件后立即返回，后台执行
    // 适用于：可以延后的操作（如 Remove, Cleanup）
    ModeAsync

    // 快速路径：不需要执行任何操作，直接返回
    // 适用于：LV 已经存在的情况
    ModeFastPath
)
```

---

## 三、完整的 Prepare 实现

### 3.1 Prepare 主函数

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

    // 非 devbox，使用原来的逻辑
    if contentID == "" {
        return o.prepareNormalSnapshot(ctx, key, parent, opts)
    }

    lvName := "devbox-" + contentID

    // ========== 步骤 2：检查 LV 状态（从 Projection）==========
    lvState, err := o.projection.GetLVState(ctx, lvName)
    if err != nil && err != ErrLVNotFound {
        return nil, fmt.Errorf("failed to get LV state: %w", err)
    }

    log.G(ctx).Infof("Prepare: LV %s state: %s", lvName, lvState.CurrentStatus)

    // ========== 步骤 3：根据状态决定执行模式 ==========
    switch lvState.CurrentStatus {
    case "none", "":
        // LV 不存在：首次创建，同步执行
        return o.prepareFirstContainerSync(ctx, key, parent, contentID, capacity, opts)

    case "created", "formatted":
        // LV 已创建但未挂载：恢复挂载，同步执行
        return o.prepareRemountSync(ctx, key, lvName, lvState, opts)

    case "mounted":
        // LV 已挂载：直接使用，快速路径
        return o.prepareExistingFast(ctx, key, lvName, lvState, opts)

    case "failed":
        // LV 处于失败状态：尝试恢复
        return o.prepareFailedRecovery(ctx, key, lvName, lvState, opts)

    default:
        return nil, fmt.Errorf("unexpected LV state: %s", lvState.CurrentStatus)
    }
}
```

### 3.2 场景 1：首次创建（同步执行）

```go
func (o *Snapshotter) prepareFirstContainerSync(
    ctx context.Context,
    key, parent, contentID, capacity string,
    opts []snapshots.Opt,
) ([]mount.Mount, error) {

    lvName := "devbox-" + contentID
    log.G(ctx).Info("Prepare: First container, creating LV synchronously")

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

    mountPoint := filepath.Join(o.root, "snapshots", snapID)

    // ========== 步骤 2：EventStore 事务 - 追加初始事件 ==========
    err = o.eventStore.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 追加请求事件
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
                "sync":        true, // 标记为同步执行
            }),
        }
        return o.eventStore.Append(ctx, requestEvent)
    })
    if err != nil {
        // 回退：删除 snapshot
        o.metaStore.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.Remove(ctx, key)
        })
        return nil, fmt.Errorf("failed to append event: %w", err)
    }

    // ========== 步骤 3：同步执行 LV 创建（带重试）==========
    if err := o.createLVSync(ctx, lvName, contentID, capacity, mountPoint); err != nil {
        // 清理：删除 snapshot 和事件
        o.metaStore.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.Remove(ctx, key)
        })
        return nil, fmt.Errorf("failed to create LV: %w", err)
    }

    // ========== 步骤 4：返回挂载信息 ==========
    log.G(ctx).Infof("Prepare: LV %s created and mounted successfully", lvName)

    return []mount.Mount{{
        Type:    "bind",
        Source:  mountPoint,
        Options: []string{"rbind"},
    }}, nil
}
```

### 3.3 同步创建 LV 的实现

```go
func (o *Snapshotter) createLVSync(
    ctx context.Context,
    lvName, contentID, capacity, mountPoint string,
) error {

    // ========== Phase 1: 创建 LV ==========
    log.G(ctx).Infof("createLVSync: Creating LV %s", lvName)

    // 追加执行中事件
    o.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    lvName,
        Type:      EventLVRequested,
        Timestamp: time.Now(),
        Status:    EventStatusExecuting,
    })

    // 创建 LV（带重试）
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{Capacity: capacity, VolGroup: o.lvmVgName},
    }

    maxRetries := 3
    var err error
    for i := 0; i < maxRetries; i++ {
        err = lvm.CreateVolume(ctx, vol)
        if err == nil {
            break
        }
        log.G(ctx).Warnf("createLVSync: LV creation failed (attempt %d/%d): %v", i+1, maxRetries, err)
        time.Sleep(time.Second * time.Duration(i+1))
    }

    if err != nil {
        // 追加失败事件
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
        return fmt.Errorf("failed to create LV after %d retries: %w", maxRetries, err)
    }

    // 追加成功事件
    o.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    lvName,
        Type:      EventLVCreated,
        Timestamp: time.Now(),
        Status:    EventStatusCompleted,
    })

    // ========== Phase 2: 格式化 LV ==========
    log.G(ctx).Infof("createLVSync: Formatting LV %s", lvName)

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
            break
        }
        log.G(ctx).Warnf("createLVSync: Format failed (attempt %d/%d): %v", i+1, maxRetries, err)
        time.Sleep(time.Second * time.Duration(i+1))
    }

    if err != nil {
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
        return fmt.Errorf("failed to format LV after %d retries: %w", maxRetries, err)
    }

    o.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    lvName,
        Type:      EventFormatted,
        Timestamp: time.Now(),
        Status:    EventStatusCompleted,
    })

    // ========== Phase 3: 挂载 LV ==========
    log.G(ctx).Infof("createLVSync: Mounting LV %s to %s", lvName, mountPoint)

    o.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    lvName,
        Type:      EventMountRequested,
        Timestamp: time.Now(),
        Status:    EventStatusExecuting,
    })

    // 创建挂载点目录
    if err := os.MkdirAll(mountPoint, 0755); err != nil {
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
        return fmt.Errorf("failed to create mount directory: %w", err)
    }

    // 挂载
    for i := 0; i < maxRetries; i++ {
        err = syscall.Mount(devicePath, mountPoint, "ext4", 0, "")
        if err == nil {
            break
        }
        log.G(ctx).Warnf("createLVSync: Mount failed (attempt %d/%d): %v", i+1, maxRetries, err)
        time.Sleep(time.Second * time.Duration(i+1))
    }

    if err != nil {
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
        return fmt.Errorf("failed to mount LV after %d retries: %w", maxRetries, err)
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

    // ========== Phase 4: 使 Projection 失效，强制重新推导状态 ==========
    o.projection.Invalidate(lvName)

    log.G(ctx).Infof("createLVSync: LV %s created, formatted and mounted successfully", lvName)
    return nil
}
```

### 3.4 场景 2：后续容器（快速路径）

```go
func (o *Snapshotter) prepareExistingFast(
    ctx context.Context,
    key, lvName string,
    lvState *LVState,
    opts []snapshots.Opt,
) ([]mount.Mount, error) {

    log.G(ctx).Infof("Prepare: LV %s already mounted, using fast path", lvName)

    // ========== 步骤 1：metadata.db 事务 - 创建 snapshot ==========
    err := o.metaStore.WithTransaction(ctx, true, func(ctx context.Context) error {
        _, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, lvState.ParentKey, opts...)
        return err
    })
    if err != nil {
        return nil, fmt.Errorf("failed to create snapshot: %w", err)
    }

    // ========== 步骤 2：追加使用事件（用于审计）==========
    o.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    lvName,
        Type:      EventSnapshotCreated, // 新事件类型：表示 snapshot 已创建
        Timestamp: time.Now(),
        Status:    EventStatusCompleted,
        Data: encodeJSON(map[string]interface{}{
            "key": key,
        }),
    })

    // ========== 步骤 3：返回现有的挂载点 ==========
    log.G(ctx).Infof("Prepare: Returning existing mount point: %s", lvState.MountPoint)

    return []mount.Mount{{
        Type:    "bind",
        Source:  lvState.MountPoint,
        Options: []string{"rbind"},
    }}, nil
}
```

### 3.5 场景 3：恢复未完成的挂载（同步执行）

```go
func (o *Snapshotter) prepareRemountSync(
    ctx context.Context,
    key, lvName string,
    lvState *LVState,
    opts []snapshots.Opt,
) ([]mount.Mount, error) {

    log.G(ctx).Infof("Prepare: LV %s exists but not mounted, mounting now", lvName)

    // LV 已创建但未挂载（可能是之前的 Prepare 失败了）
    // 需要完成挂载操作

    snapID := generateSnapID()
    mountPoint := filepath.Join(o.root, "snapshots", snapID)

    // ========== 步骤 1：metadata.db 事务 - 创建 snapshot ==========
    err := o.metaStore.WithTransaction(ctx, true, func(ctx context.Context) error {
        _, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, lvState.ParentKey, opts...)
        return err
    })
    if err != nil {
        return nil, fmt.Errorf("failed to create snapshot: %w", err)
    }

    // ========== 步骤 2：同步挂载 LV ==========
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)

    // 追加挂载请求事件
    o.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    lvName,
        Type:      EventMountRequested,
        Timestamp: time.Now(),
        Status:    EventStatusExecuting,
        Data: encodeJSON(map[string]interface{}{
            "mount_point": mountPoint,
        }),
    })

    // 创建挂载点并挂载
    if err := os.MkdirAll(mountPoint, 0755); err != nil {
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
        return nil, fmt.Errorf("failed to create mount directory: %w", err)
    }

    if err := syscall.Mount(devicePath, mountPoint, "ext4", 0, ""); err != nil {
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
        return nil, fmt.Errorf("failed to mount LV: %w", err)
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

    // 使 Projection 失效
    o.projection.Invalidate(lvName)

    log.G(ctx).Infof("Prepare: LV %s mounted successfully", lvName)

    return []mount.Mount{{
        Type:    "bind",
        Source:  mountPoint,
        Options: []string{"rbind"},
    }}, nil
}
```

---

## 四、Remove 接口：使用异步执行

与 Prepare 不同，Remove 可以使用异步执行，因为删除操作不阻塞容器使用。

### 4.1 Remove 实现

```go
func (o *Snapshotter) Remove(ctx context.Context, key string) error {
    log.G(ctx).Infof("Remove: key=%s", key)

    // ========== 步骤 1：metadata.db 事务 - 删除 snapshot ==========
    err := o.metaStore.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.Remove(ctx, key)
    })
    if err != nil {
        return fmt.Errorf("failed to remove snapshot: %w", err)
    }

    // ========== 步骤 2：检查是否需要卸载和删除 LV ==========
    // 从 Projection 获取 LV 状态
    lvName := getLVNameFromKey(key)
    lvState, err := o.projection.GetLVState(ctx, lvName)
    if err != nil {
        // LV 不存在或已删除，直接返回
        return nil
    }

    // ========== 步骤 3：检查引用计数 ==========
    if lvState.RefCount > 1 {
        // 还有其他 snapshot 引用这个 LV，只删除 snapshot，不删除 LV
        log.G(ctx).Infof("Remove: LV %s still referenced by %d snapshots, skipping", lvName, lvState.RefCount)
        return nil
    }

    // ========== 步骤 4：追加卸载和删除事件（异步执行）==========
    log.G(ctx).Infof("Remove: Scheduling async cleanup for LV %s", lvName)

    o.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    lvName,
        Type:      EventUnmountRequested,
        Timestamp: time.Now(),
        Status:    EventStatusPending, // ← 异步，pending 状态
    })

    // 执行器会在后台处理这个事件

    return nil
}
```

### 4.2 执行器处理异步事件

```go
func (e *LVExecutor) processPendingEvents(ctx context.Context) {
    events, _ := e.eventStore.GetPendingEvents(ctx)

    for _, event := range events {
        if err := e.Execute(ctx, event); err != nil {
            log.G(ctx).WithError(err).Errorf("Failed to execute event %s", event.ID)
            // 事件已标记为 failed，稍后会重试
        }
    }
}

func (e *LVExecutor) executeUnmount(ctx context.Context, event Event) error {
    var data map[string]interface{}
    json.Unmarshal(event.Data, &data)
    mountPoint := data["mount_point"].(string)

    // 卸载
    if err := syscall.Unmount(mountPoint, 0); err != nil {
        e.eventStore.Append(ctx, Event{
            ID:     generateEventID(),
            LVName: event.LVName,
            Type:   EventUnmountFailed,
            Status: EventStatusCompleted,
            Data:   encodeError(err),
        })
        return err
    }

    // 追加成功事件
    e.eventStore.Append(ctx, Event{
        ID:     generateEventID(),
        LVName: event.LVName,
        Type:   EventUnmounted,
        Status: EventStatusCompleted,
    })

    // 追加删除请求事件
    e.eventStore.Append(ctx, Event{
        ID:     generateEventID(),
        LVName: event.LVName,
        Type:   EventRemoveRequested,
        Status: EventStatusPending,
    })

    return nil
}
```

---

## 五、状态转换详解

### 5.1 完整的状态转换图

```
                    [首次创建容器 - 同步执行]
                         Prepare(key="container1")
                              ↓
              ┌──────────────────────────────────┐
              │  Event: lv_requested (pending)    │
              └────────────┬─────────────────────┘
                           ↓
              ┌──────────────────────────────────┐
              │  执行器：创建 LV                   │
              │  - lvm.CreateVolume()             │
              │  - 失败则重试 3 次                 │
              └────────────┬─────────────────────┘
                           ↓
                  ┌────────┴────────┐
                  │                 │
             成功 ✅            失败 ❌
                  │                 │
                  ↓                 ↓
    ┌──────────────────┐   ┌──────────────────┐
    │ Event: lv_created │   │ Event: lv_failed  │
    │ (completed)      │   │ (completed)      │
    └─────────┬────────┘   └─────────┬────────┘
              │                      │
              ↓                      ↓
    ┌──────────────────┐   ┌──────────────────┐
    │ 执行器：格式化 LV   │   │ 状态 = "failed"  │
    │ - mkfs()          │   │ 返回错误给调用者  │
    │ - 失败则重试 3 次   │   │ Prepare 返回错误 │
    └─────────┬────────┘   └──────────────────┘
              │
              ↓
      ┌───────┴────────┐
      │                │
 成功 ✅          失败 ❌
      │                │
      ↓                ↓
┌──────────────┐  ┌──────────────┐
│Event: formatted│  │Event: failed │
│ (completed)  │  │ (completed) │
└──────┬───────┘  └──────┬───────┘
       │                 │
       ↓                 ↓
┌──────────────┐  ┌──────────────┐
│ 执行器：挂载 LV │  │ 状态 = "failed"│
│ - mount()     │  │ 返回错误      │
│ - 失败则重试 3次│  │              │
└──────┬───────┘  └──────────────┘
       │
       ↓
 ┌────┴────┐
 │         │
成功 ✅  失败 ❌
 │         │
 ↓         ↓
┌────────┐ ┌────────┐
│Event:  │ │Event:  │
│mounted │ │failed  │
└───┬────┘ └───┬────┘
    │          │
    ↓          ↓
状态 =      状态 = "failed"
"mounted"   返回错误
    │
    ↓
┌──────────────────────────┐
│  Prepare 返回 mountInfo   │
│  容器可以立即使用         │
└──────────────────────────┘



                    [后续容器 - 快速路径]
                    Prepare(key="container2")
                              ↓
              ┌──────────────────────────────────┐
              │  从 Projection 读取状态：mounted  │
              └────────────┬─────────────────────┘
                           ↓
              ┌──────────────────────────────────┐
              │  创建 snapshot 元数据            │
              │  返回现有的 mountPoint            │
              │  立即返回（毫秒级）               │
              └──────────────────────────────────┘
                           ↓
              ┌──────────────────────────────────┐
              │  追加事件：snapshot_created      │
              │  （仅用于审计）                   │
              └──────────────────────────────────┘



                    [卸载和删除 - 异步执行]
                    Remove(key="container1")
                              ↓
              ┌──────────────────────────────────┐
              │  删除 snapshot 元数据            │
              │  检查引用计数（RefCount = 0）    │
              └────────────┬─────────────────────┘
                           ↓
              ┌──────────────────────────────────┐
              │  追加事件：unmount_requested     │
              │  Status = pending                │
              └────────────┬─────────────────────┘
                           ↓
                   Remove 立即返回
                           ↓
              ┌──────────────────────────────────┐
              │  后台执行器处理 pending 事件      │
              │  1. unmount_requested → unmounted│
              │  2. remove_requested → removed   │
              └──────────────────────────────────┘
```

### 5.2 状态定义

| 状态 | 含义 | 如何到达 | 可执行操作 |
|------|------|---------|-----------|
| `none` | LV 不存在 | 初始状态 | 创建 LV |
| `creating` | LV 正在创建 | lv_requested 事件，status=executing | 等待创建完成 |
| `created` | LV 已创建，未格式化 | lv_created 事件 | 格式化 |
| `formatting` | 正在格式化 | format_requested 事件，status=executing | 等待格式化完成 |
| `formatted` | 已格式化，未挂载 | formatted 事件 | 挂载 |
| `mounting` | 正在挂载 | mount_requested 事件，status=executing | 等待挂载完成 |
| `mounted` | 已挂载，可用 | mounted 事件 | 使用、卸载 |
| `unmounting` | 正在卸载 | unmount_requested 事件，status=executing | 等待卸载完成 |
| `unmounted` | 已卸载 | unmounted 事件 | 删除或重新挂载 |
| `removing` | 正在删除 | remove_requested 事件，status=executing | 等待删除完成 |
| `removed` | 已删除 | removed 事件 | 清理元数据 |
| `failed` | 操作失败 | 任何 *_failed 事件 | 查看错误、手动恢复 |

### 5.3 Projection：从事件推导状态

```go
type LVState struct {
    Name       string
    ContentID  string
    Capacity   string

    // 从事件推导出来的当前状态
    CurrentStatus string // "none", "created", "mounted", "failed", etc.

    // 挂载信息
    MountPoint  string
    IsMounted   bool

    // 引用计数
    RefCount    int

    // 错误信息（如果处于 failed 状态）
    LastError   string
    FailedAt    time.Time

    // 元数据
    CreatedAt   time.Time
    UpdatedAt   time.Time
}

func GetLVState(events []Event) *LVState {
    state := &LVState{
        CurrentStatus: "none",
        RefCount:      0,
    }

    for _, event := range events {
        switch event.Type {
        case EventLVRequested:
            state.Name = event.LVName
            state.CurrentStatus = "creating"
            state.CreatedAt = event.Timestamp

            var data map[string]interface{}
            json.Unmarshal(event.Data, &data)
            state.ContentID = data["content_id"].(string)
            state.Capacity = data["capacity"].(string)

        case EventLVCreated:
            state.CurrentStatus = "created"

        case EventFormatted:
            state.CurrentStatus = "formatted"

        case EventMounted:
            state.CurrentStatus = "mounted"
            state.IsMounted = true
            var data map[string]interface{}
            json.Unmarshal(event.Data, &data)
            state.MountPoint = data["mount_point"].(string)

        case EventUnmounted:
            state.CurrentStatus = "unmounted"
            state.IsMounted = false
            state.MountPoint = ""

        case EventRemoved:
            state.CurrentStatus = "removed"

        case EventLVCreateFailed, EventFormatFailed, EventMountFailed:
            state.CurrentStatus = "failed"
            var data map[string]interface{}
            json.Unmarshal(event.Data, &data)
            state.LastError = data["error"].(string)
            state.FailedAt = event.Timestamp

        case EventSnapshotCreated:
            // 增加 snapshot 引用计数
            state.RefCount++
        }

        state.UpdatedAt = event.Timestamp
    }

    return state
}
```

---

## 六、错误处理与重试

### 6.1 同步操作的重试策略

```go
func (o *Snapshotter) createLVSync(...) error {
    maxRetries := 3
    retryDelay := time.Second

    // 创建 LV
    for i := 0; i < maxRetries; i++ {
        err = lvm.CreateVolume(ctx, vol)
        if err == nil {
            break
        }

        // 检查是否是可重试的错误
        if !isRetriableError(err) {
            // 不可重试，直接返回
            appendFailureEvent(...)
            return err
        }

        if i < maxRetries-1 {
            log.Warnf("LV creation failed (attempt %d/%d), retrying...", i+1, maxRetries)
            time.Sleep(retryDelay * time.Duration(i+1)) // 指数退避
        }
    }

    if err != nil {
        appendFailureEvent(...)
        return err
    }

    appendSuccessEvent(...)
    return nil
}

func isRetriableError(err error) bool {
    // 临时错误，可以重试
    return strings.Contains(err.Error(), "device busy") ||
           strings.Contains(err.Error(), "resource temporarily unavailable") ||
           strings.Contains(err.Error(), "timeout")
}
```

### 6.2 异步操作的重试

```go
func (e *LVExecutor) processPendingEvents(ctx context.Context) {
    events, _ := e.eventStore.GetPendingEvents(ctx)

    for _, event := range events {
        // 检查失败次数
        if event.RetryCount >= 5 {
            log.Errorf("Event %s failed %d times, giving up", event.ID, event.RetryCount)
            continue
        }

        // 执行事件
        if err := e.Execute(ctx, event); err != nil {
            // 增加重试计数
            event.RetryCount++
            event.LastRetryTime = time.Now()
            e.eventStore.UpdateEvent(ctx, event)
        }
    }
}
```

---

## 七、方案对比总结

### 7.1 与纯异步方案的对比

| 方面 | 纯异步方案（有问题） | 混合方案（改进） |
|------|-------------------|----------------|
| Prepare 首次创建 | 异步，立即返回 | 同步，等待完成 |
| 数据一致性 | ❌ 有问题 | ✅ 保证 |
| Prepare 后续容器 | - | 快速路径（毫秒级） |
| Remove | 异步 | 异步 |
| 错误处理 | 简单 | 稍复杂但可控 |
| 性能 | 首次快，但不可用 | 首次慢（秒级），后续快 |

### 7.2 与旧方案的对比

| 方面 | 旧方案（状态机 + 补偿事务） | 新方案（混合事件溯源） |
|------|---------------------------|----------------------|
| Prepare 事务数 | 5个 | 2个（首次） |
| Prepare 首次性能 | 慢（同步，无优化） | 慢（同步，带重试） |
| Prepare 后续性能 | 慢 | 快（快速路径） |
| 错误处理 | 复杂（补偿事务） | 简单（追加事件） |
| 代码复杂度 | 高（~500行） | 中（~400行） |
| 可追溯性 | 弱 | 强（完整事件日志） |
| 启动时恢复 | 复杂（状态机检查） | 简单（事件重放） |

### 7.3 核心优势

1. **保证数据一致性**：Prepare 时同步等待 LV 就绪，确保容器启动时 snapshot 可用
2. **简化错误处理**：失败只追加事件，不需要复杂的补偿事务
3. **性能优化**：后续容器使用快速路径，毫秒级响应
4. **完整可追溯性**：事件日志记录所有操作历史
5. **幂等性**：支持重试和恢复

---

## 八、实施建议

**Phase 1（1-2周）**：
1. 实现 EventStore
2. 实现同步创建 LV（带重试）
3. 实现 Prepare（首次 + 后续）

**Phase 2（1周）**：
4. 实现 Projection
5. 实现异步 Remove
6. 实现执行器后台处理

**Phase 3（可选）**：
7. 启动时恢复
8. 监控和工具
9. 性能优化

这个方案在保证正确性的前提下，最大程度保留了事件溯源的优势。
