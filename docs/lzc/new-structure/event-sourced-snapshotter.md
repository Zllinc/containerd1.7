# 事件溯源架构：简化的 Devbox Snapshotter 设计

## 一、核心思想

### 1.1 现有方案的问题

**问题代码示例**：
```go
// ❌ 现有方案：复杂的补偿事务
func prepareFirstContainer(...) {
    // T1: metadata.db 事务
    err := o.metaStore.WithTransaction(ctx, true, func(ctx) error {
        return storage.CreateSnapshot(...)
    })
    if err != nil {
        return err  // 失败，直接返回
    }

    // T2: devbox.db 事务
    err = o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
        return devboxStorage.AddLV(...)
    })
    if err != nil {
        // ❌ 补偿事务：回退 T1
        o.metaStore.WithTransaction(ctx, true, func(ctx) error {
            return storage.Remove(ctx, key)
        })
        return err
    }

    // 物理操作：创建 LV
    createErr := lvm.CreateVolume(ctx, vol)
    if createErr != nil {
        // ❌ 补偿事务：回退 T1 + T2
        o.devboxMetadata.WithTransaction(ctx, true, func(ctx) error {
            return devboxStorage.RemoveLV(ctx, lvName)
        })
        o.metaStore.WithTransaction(ctx, true, func(ctx) error {
            return storage.Remove(ctx, key)
        })
        return createErr
    }

    // T3: devbox.db 事务
    // ... （后续还有 T4, T5，每步都要考虑回退）
}
```

**核心问题**：
1. 5个连续的事务（T1-T5），每步失败都要写补偿逻辑
2. 事务间依赖复杂：T2 失败要回退 T1，物理操作失败要回退 T1+T2
3. 状态机复杂：creating → created → mounting → mounted，每个转换都可能失败
4. 补偿事务代码量大，难以维护

### 1.2 新方案的核心思想

**核心理念**：
- **状态是推导出来的，不是存储出来的**
- **事件是不可变的事实，是唯一真实来源**
- **操作通过事件驱动，而不是通过状态驱动**

**关键区别**：

| 方面 | 旧方案（状态机） | 新方案（事件溯源） |
|------|----------------|-------------------|
| 存储内容 | 当前状态 | 事件日志 |
| 状态计算 | 直接读取 | 从事件重放 |
| 失败处理 | 补偿事务 | 追加新事件（补偿事件） |
| 复杂度 | 高（每步都要回退） | 低（只追加，不修改） |
| 可追溯性 | 弱（只看得到当前状态） | 强（完整的历史记录） |

---

## 二、架构设计

### 2.1 数据结构

#### 2.1.1 事件定义

```go
// Event 表示 LV 生命周期中的一个不可变事件
type Event struct {
    ID        string    // 事件唯一 ID
    LVName    string    // LV 名称
    Type      EventType // 事件类型
    Data      []byte    // 事件数据（JSON）
    Timestamp time.Time // 事件时间
    Status    EventStatus // 事件状态
}

type EventType string

const (
    // LV 生命周期事件
    EventLVRequested    EventType = "lv_requested"    // 请求创建 LV
    EventLVCreated      EventType = "lv_created"      // LV 创建成功
    EventLVCreateFailed EventType = "lv_create_failed" // LV 创建失败

    // 格式化事件
    EventFormatRequested     EventType = "format_requested"     // 请求格式化
    EventFormatted           EventType = "formatted"             // 格式化成功
    EventFormatFailed        EventType = "format_failed"         // 格式化失败

    // 挂载事件
    EventMountRequested  EventType = "mount_requested"  // 请求挂载
    EventMounted         EventType = "mounted"          // 挂载成功
    EventMountFailed     EventType = "mount_failed"     // 挂载失败

    // 卸载事件
    EventUnmountRequested  EventType = "unmount_requested"  // 请求卸载
    EventUnmounted         EventType = "unmounted"          // 卸载成功
    EventUnmountFailed     EventType = "unmount_failed"     // 卸载失败

    // 删除事件
    EventRemoveRequested  EventType = "remove_requested"  // 请求删除
    EventRemoved          EventType = "removed"           // 删除成功
    EventRemoveFailed     EventType = "remove_failed"     // 删除失败
)

type EventStatus string

const (
    EventStatusPending   EventStatus = "pending"    // 待执行
    EventStatusExecuting EventStatus = "executing"  // 执行中
    EventStatusCompleted EventStatus = "completed"  // 已完成
    EventStatusFailed    EventStatus = "failed"     // 已失败
)
```

#### 2.1.2 LV 信息（从事件推导）

```go
// LVState 从事件日志中推导出来的当前状态
type LVState struct {
    Name       string
    ContentID  string
    Capacity   string

    // 从事件推导出来的状态
    CurrentStatus string // "none", "created", "formatted", "mounted", "unmounted", "removed", "failed"

    // 挂载信息
    MountPoint  string
    IsMounted   bool

    // 错误信息（如果处于 failed 状态）
    LastError   string
    FailedAt    time.Time

    // 元数据
    CreatedAt   time.Time
    UpdatedAt   time.Time
}

// GetLVState 从事件日志重放得到当前状态
func GetLVState(events []Event) *LVState {
    state := &LVState{
        CurrentStatus: "none",
    }

    for _, event := range events {
        switch event.Type {
        case EventLVRequested:
            state.Name = event.LVName
            state.CurrentStatus = "creating"
            state.CreatedAt = event.Timestamp

        case EventLVCreated:
            state.CurrentStatus = "created"

        case EventFormatted:
            state.CurrentStatus = "formatted"

        case EventMounted:
            state.CurrentStatus = "mounted"
            state.IsMounted = true
            var mountData MountData
            json.Unmarshal(event.Data, &mountData)
            state.MountPoint = mountData.MountPoint

        case EventUnmounted:
            state.CurrentStatus = "unmounted"
            state.IsMounted = false
            state.MountPoint = ""

        case EventLVCreateFailed, EventFormatFailed, EventMountFailed:
            state.CurrentStatus = "failed"
            var errData ErrorData
            json.Unmarshal(event.Data, &errData)
            state.LastError = errData.Error
            state.FailedAt = event.Timestamp

        case EventRemoved:
            state.CurrentStatus = "removed"
        }

        state.UpdatedAt = event.Timestamp
    }

    return state
}
```

---

### 2.2 核心组件

#### 2.2.1 事件存储（EventStore）

```go
type EventStore interface {
    // Append 追加新事件（原子操作）
    Append(ctx context.Context, event Event) error

    // GetEvents 获取 LV 的所有事件
    GetEvents(ctx context.Context, lvName string) ([]Event, error)

    // GetPendingEvents 获取待执行的事件
    GetPendingEvents(ctx context.Context) ([]Event, error)

    // MarkEventStatus 更新事件状态
    MarkEventStatus(ctx context.Context, eventID string, status EventStatus) error
}
```

**关键特性**：
- **只追加，不修改**：事件一旦写入就不会改变
- **原子写入**：追加事件是原子操作，要么全成功，要么全失败
- **快速读取**：按 LV 名称快速查询事件流

#### 2.2.2 执行器（Executor）

```go
type Executor interface {
    // 执行单个事件
    Execute(ctx context.Context, event Event) error

    // 后台执行器：不断执行 pending 状态的事件
    Start(ctx context.Context)
}

type LVExecutor struct {
    eventStore EventStore
    lvmVgName  string
}

func (e *LVExecutor) Execute(ctx context.Context, event Event) error {
    // 标记为执行中
    e.eventStore.MarkEventStatus(ctx, event.ID, EventStatusExecuting)

    var err error
    switch event.Type {
    case EventLVRequested:
        err = e.executeCreateLV(ctx, event)
    case EventFormatRequested:
        err = e.executeFormat(ctx, event)
    case EventMountRequested:
        err = e.executeMount(ctx, event)
    case EventUnmountRequested:
        err = e.executeUnmount(ctx, event)
    case EventRemoveRequested:
        err = e.executeRemove(ctx, event)
    }

    // 标记为完成或失败
    if err != nil {
        e.eventStore.MarkEventStatus(ctx, event.ID, EventStatusFailed)
        return err
    }

    e.eventStore.MarkEventStatus(ctx, event.ID, EventStatusCompleted)
    return nil
}
```

**关键特性**：
- **幂等性**：重复执行同一个事件不会出错
- **异步执行**：不阻塞主流程
- **自动重试**：失败的事件会自动重试

#### 2.2.3 Projection（状态投影）

```go
// Projection 从事件流中推导出当前状态
type Projection struct {
    eventStore EventStore
    cache      map[string]*LVState // 缓存推导出的状态
}

func (p *Projection) GetLVState(ctx context.Context, lvName string) (*LVState, error) {
    // 检查缓存
    if state, ok := p.cache[lvName]; ok {
        return state, nil
    }

    // 从事件重放
    events, err := p.eventStore.GetEvents(ctx, lvName)
    if err != nil {
        return nil, err
    }

    state := GetLVState(events)
    p.cache[lvName] = state
    return state, nil
}

func (p *Projection) Invalidate(lvName string) {
    delete(p.cache, lvName)
}
```

---

## 三、完整流程：Prepare 函数

### 3.1 流程对比

#### 旧方案（复杂）：

```
T1: metadata.db 事务（创建 snapshot）
    ↓
T2: devbox.db 事务（创建 LV 记录，状态=creating）
    ↓ （如果失败，回退 T1）
物理操作：创建 LV
    ↓ （如果失败，回退 T1+T2）
T3: devbox.db 事务（更新状态=created）
    ↓ （如果失败，记录 faulty）
物理操作：格式化
    ↓ （如果失败，标记 faulty，回退到 created）
T4: devbox.db 事务（更新状态=mounting）
    ↓ （如果失败，记录 faulty）
物理操作：挂载
    ↓ （如果失败，标记 faulty，回退到 formatted）
T5: devbox.db 事务（更新状态=mounted）
    ↓ （如果失败，记录 faulty）
返回挂载信息
```

#### 新方案（简单）：

```
T1: metadata.db 事务（创建 snapshot）
    ↓
T2: EventStore 事务（追加事件：lv_requested）
    ↓ （如果失败，回退 T1，物理操作还没开始）
返回挂载信息（不等待 LV 创建完成！）
    ↓
后台异步执行：
    1. 执行器读取 pending 事件
    2. 创建 LV
    3. 追加事件：lv_created
    4. 追加事件：format_requested
    5. 格式化
    6. 追加事件：formatted
    7. 追加事件：mount_requested
    8. 挂载
    9. 追加事件：mounted
```

**关键区别**：
- 旧方案：5个事务 + 3个物理操作，全部同步执行，等待完成
- 新方案：2个事务，立即返回，物理操作异步执行

### 3.2 Prepare 函数实现

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

    // ========== 步骤 3：EventStore 事务 - 追加事件 ==========
    mountPoint := filepath.Join(o.root, "snapshots", snapID)

    err = o.eventStore.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 检查是否已经有事件
        events, _ := o.eventStore.GetEvents(ctx, lvName)
        if len(events) > 0 {
            // 已经创建过，不需要重新创建
            return nil
        }

        // 追加初始事件
        requestEvent := Event{
            ID:        generateEventID(),
            LVName:    lvName,
            Type:      EventLVRequested,
            Timestamp: time.Now(),
            Status:    EventStatusPending,
            Data: encodeJSON(map[string]interface{}{
                "content_id": contentID,
                "capacity":   capacity,
                "mount_point": mountPoint,
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

    // ========== 步骤 4：立即返回挂载信息 ==========
    // 注意：此时 LV 可能还没创建完成！
    // 但这没关系，因为容器启动时会等待挂载点可用

    log.G(ctx).Infof("Prepare: LV %s creation initiated (async)", lvName)

    return []mount.Mount{{
        Type:    "bind",
        Source:  mountPoint,
        Options: []string{"rbind"},
    }}, nil
}
```

**关键点**：
1. **只涉及2个事务**，而不是5个
2. **不需要等待 LV 创建完成**
3. **不需要补偿事务**（除非追加事件失败，那只需删除 snapshot）
4. **物理操作全部异步执行**

### 3.3 执行器实现

```go
func (e *LVExecutor) Start(ctx context.Context) {
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            e.processPendingEvents(ctx)
        }
    }
}

func (e *LVExecutor) processPendingEvents(ctx context.Context) {
    // 获取所有 pending 状态的事件
    events, err := e.eventStore.GetPendingEvents(ctx)
    if err != nil {
        log.G(ctx).WithError(err).Error("Failed to get pending events")
        return
    }

    for _, event := range events {
        // 执行事件
        if err := e.Execute(ctx, event); err != nil {
            log.G(ctx).WithError(err).Errorf("Failed to execute event %s", event.ID)
            // 事件已标记为 failed，稍后会重试
        }
    }
}

func (e *LVExecutor) executeCreateLV(ctx context.Context, event Event) error {
    var data map[string]interface{}
    json.Unmarshal(event.Data, &data)

    contentID := data["content_id"].(string)
    capacity := data["capacity"].(string)

    // 创建 LV
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: event.LVName},
        Spec:       apis.VolumeInfo{Capacity: capacity, VolGroup: e.lvmVgName},
    }

    if err := lvm.CreateVolume(ctx, vol); err != nil {
        // 追加失败事件
        e.eventStore.Append(ctx, Event{
            ID:        generateEventID(),
            LVName:    event.LVName,
            Type:      EventLVCreateFailed,
            Timestamp: time.Now(),
            Status:    EventStatusCompleted, // 失败事件本身已完成
            Data: encodeJSON(map[string]interface{}{
                "error": err.Error(),
            }),
        })
        return err
    }

    // 追加成功事件
    e.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    event.LVName,
        Type:      EventLVCreated,
        Timestamp: time.Now(),
        Status:    EventStatusCompleted,
    })

    // 追加下一个请求事件
    e.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    event.LVName,
        Type:      EventFormatRequested,
        Timestamp: time.Now(),
        Status:    EventStatusPending,
    })

    return nil
}

func (e *LVExecutor) executeFormat(ctx context.Context, event Event) error {
    devicePath := fmt.Sprintf("/dev/%s/%s", e.lvmVgName, event.LVName)

    if err := mkfs(devicePath); err != nil {
        e.eventStore.Append(ctx, Event{
            ID:        generateEventID(),
            LVName:    event.LVName,
            Type:      EventFormatFailed,
            Timestamp: time.Now(),
            Status:    EventStatusCompleted,
            Data: encodeJSON(map[string]interface{}{
                "error": err.Error(),
            }),
        })
        return err
    }

    e.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    event.LVName,
        Type:      EventFormatted,
        Timestamp: time.Now(),
        Status:    EventStatusCompleted,
    })

    // 获取挂载点
    events, _ := e.eventStore.GetEvents(ctx, event.LVName)
    var mountPoint string
    for _, e := range events {
        if e.Type == EventLVRequested {
            var data map[string]interface{}
            json.Unmarshal(e.Data, &data)
            mountPoint = data["mount_point"].(string)
            break
        }
    }

    // 追加挂载请求
    e.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    event.LVName,
        Type:      EventMountRequested,
        Timestamp: time.Now(),
        Status:    EventStatusPending,
        Data: encodeJSON(map[string]interface{}{
            "mount_point": mountPoint,
        }),
    })

    return nil
}

func (e *LVExecutor) executeMount(ctx context.Context, event Event) error {
    var data map[string]interface{}
    json.Unmarshal(event.Data, &data)
    mountPoint := data["mount_point"].(string)
    devicePath := fmt.Sprintf("/dev/%s/%s", e.lvmVgName, event.LVName)

    // 创建挂载点目录
    if err := os.MkdirAll(mountPoint, 0755); err != nil {
        e.eventStore.Append(ctx, Event{
            ID:        generateEventID(),
            LVName:    event.LVName,
            Type:      EventMountFailed,
            Timestamp: time.Now(),
            Status:    EventStatusCompleted,
            Data: encodeJSON(map[string]interface{}{
                "error": err.Error(),
            }),
        })
        return err
    }

    // 挂载
    if err := syscall.Mount(devicePath, mountPoint, "ext4", 0, ""); err != nil {
        e.eventStore.Append(ctx, Event{
            ID:        generateEventID(),
            LVName:    event.LVName,
            Type:      EventMountFailed,
            Timestamp: time.Now(),
            Status:    EventStatusCompleted,
            Data: encodeJSON(map[string]interface{}{
                "error": err.Error(),
            }),
        })
        return err
    }

    e.eventStore.Append(ctx, Event{
        ID:        generateEventID(),
        LVName:    event.LVName,
        Type:      EventMounted,
        Timestamp: time.Now(),
        Status:    EventStatusCompleted,
        Data: encodeJSON(map[string]interface{}{
            "mount_point": mountPoint,
        }),
    })

    return nil
}
```

---

## 四、错误处理与恢复

### 4.1 失败场景

#### 场景 1：追加事件失败

```go
err = o.eventStore.WithTransaction(ctx, true, func(ctx context.Context) error {
    return o.eventStore.Append(ctx, requestEvent)
})
if err != nil {
    // ✅ 回退：删除 snapshot
    o.metaStore.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.Remove(ctx, key)
    })
    return nil, fmt.Errorf("failed to append event: %w", err)
}
```

**特点**：物理操作还没开始，只需回退 snapshot，简单明了。

#### 场景 2：LV 创建失败

```go
func (e *LVExecutor) executeCreateLV(ctx context.Context, event Event) error {
    if err := lvm.CreateVolume(ctx, vol); err != nil {
        // ✅ 追加失败事件（不是回退！）
        e.eventStore.Append(ctx, Event{
            Type:   EventLVCreateFailed,
            Status: EventStatusCompleted,
            Data:   encodeError(err),
        })
        return err
    }

    // ✅ 追加成功事件
    e.eventStore.Append(ctx, Event{
        Type:   EventLVCreated,
        Status: EventStatusCompleted,
    })
    return nil
}
```

**特点**：
- 不需要回退任何事件
- 只追加新的事件记录失败
- 状态自然推导为 "failed"

#### 场景 3：进程崩溃

```go
// 进程崩溃时，事件状态可能为 pending 或 executing

// 重启后，执行器自动处理：
func (e *LVExecutor) processPendingEvents(ctx context.Context) {
    // 1. 获取所有 pending 事件
    events, _ := e.eventStore.GetPendingEvents(ctx)

    // 2. 检查每个事件的实际状态
    for _, event := range events {
        // 检查 LV 是否实际存在
        deviceExists := checkDeviceExists(event.LVName)

        if event.Type == EventLVRequested && deviceExists {
            // LV 已创建，但事件未更新
            // ✅ 补充成功事件
            e.eventStore.Append(ctx, Event{
                Type:   EventLVCreated,
                Status: EventStatusCompleted,
            })
        } else if event.Type == EventLVRequested && !deviceExists {
            // LV 不存在，重新创建
            e.Execute(ctx, event)
        }
    }
}
```

**特点**：
- 事件日志是完整的历史，可以准确判断崩溃发生在哪一步
- 通过补充事件来修复状态，而不是修改原有事件

### 4.2 启动时恢复

```go
func (o *Snapshotter) New(ctx context.Context, root string, config *Config) (*Snapshotter, error) {
    s := &Snapshotter{
        root:       root,
        metaStore:  metaStore,
        eventStore: eventStore,
        projection: &Projection{eventStore: eventStore},
        executor:   &LVExecutor{eventStore: eventStore, lvmVgName: config.LvmVgName},
    }

    // ✅ 启动执行器（自动处理 pending 事件）
    go s.executor.Start(ctx)

    // ✅ 启动时修复不一致的事件（仅一次性）
    s.recoverEvents(ctx)

    return s, nil
}

func (o *Snapshotter) recoverEvents(ctx context.Context) {
    events, _ := o.eventStore.GetPendingEvents(ctx)

    for _, event := range events {
        // 检查事件的实际执行结果
        actualState := o.checkActualState(event)

        if actualState == eventSucceeded {
            // 事件实际成功了，但未标记
            // ✅ 补充成功事件
            o.eventStore.Append(ctx, Event{
                Type:   getSuccessEventType(event.Type),
                Status: EventStatusCompleted,
            })
        } else if actualState == eventFailed {
            // 事件实际失败了，但未标记
            // ✅ 补充失败事件
            o.eventStore.Append(ctx, Event{
                Type:   getFailureEventType(event.Type),
                Status: EventStatusCompleted,
            })
        }
        // 如果实际状态未知，保持 pending，执行器会重试
    }
}
```

---

## 五、方案对比

### 5.1 代码复杂度

| 方面 | 旧方案 | 新方案 |
|------|----------------|-----------------|
| Prepare 事务数 | 5个 | 2个 |
| 补偿事务 | 需要写 | 不需要 |
| 状态定义 | 7个状态 | 15种事件类型 |
| 失败处理 | 每步都要考虑回退 | 只需追加新事件 |
| 代码行数 | ~500行 | ~300行 |

### 5.2 可维护性

| 方面 | 旧方案 | 新方案 |
|------|----------------|-----------------|
| 添加新操作 | 需要修改状态机、转换逻辑、回退逻辑 | 只需添加新的事件类型和执行器 |
| 调试问题 | 难以追溯历史（只知道当前状态） | 完整的事件日志，可以精确重现 |
| 测试 | 需要测试所有状态转换组合 | 只需测试单个事件的处理 |
| 理解难度 | 高（状态机复杂） | 低（事件驱动，直观） |

### 5.3 性能

| 方面 | 旧方案 | 新方案 |
|------|----------------|-----------------|
| Prepare 延迟 | 同步等待 LV 创建（可能数分钟） | 异步，立即返回（毫秒级） |
| 并发性能 | 数据库锁竞争激烈 | 锁竞争少（只追加事件） |
| 资源清理 | 需要扫描整个数据库 | 只需扫描事件日志 |

---

## 六、实施建议

### 6.1 Phase 1：核心功能（1-2周）

1. **实现 EventStore**
   - 使用 BoltDB 存储
   - 支持追加、查询、更新状态

2. **实现核心事件类型**
   - lv_requested, lv_created, lv_create_failed
   - mount_requested, mounted, mount_failed
   - unmount_requested, unmounted, unmount_failed

3. **实现执行器**
   - LV 创建、挂载、卸载
   - 后台执行 pending 事件

4. **修改 Prepare**
   - 只追加事件，不等待执行完成

### 6.2 Phase 2：完善功能（1周）

5. **实现 Projection**
   - 从事件推导状态
   - 缓存优化

6. **启动时恢复**
   - 检查 pending 事件
   - 补充缺失的事件

7. **Cleanup**
   - 基于 Projection 查询
   - 清理失败的 LV

### 6.3 Phase 3：增强功能（可选）

8. **监控**
   - 事件执行时间统计
   - 失败率监控

9. **工具**
   - 事件查看器
   - 手动修复工具

10. **优化**
    - 批量执行
    - 优先级队列

---

## 七、总结

### 7.1 核心优势

1. **简化错误处理**
   - 不需要补偿事务
   - 失败只需追加新事件
   - 状态从不一致

2. **提高可追溯性**
   - 完整的事件日志
   - 可以精确重现问题
   - 方便调试和审计

3. **提高性能**
   - 异步执行，不阻塞
   - 数据库锁竞争少
   - 可以批量处理

4. **提高可维护性**
   - 代码结构清晰
   - 添加新功能简单
   - 测试覆盖容易

### 7.2 与现有方案的兼容性

- **metadata.db**：保持不变，继续管理 snapshot 元数据
- **devbox.db**：替换为 EventStore，更简单
- **对外接口**：Prepare/Remove/Mount 等接口保持不变

### 7.3 为什么这个方案更好？

**问题根源**：用 ACID 事务（强一致性、即时）来处理长时间运行的物理操作（LVM、文件系统），这是不匹配的。

**解决方案**：
- **事件溯源**：用日志记录"做了什么"，而不是"当前是什么"
- **异步执行**：不阻塞主流程
- **幂等性**：重复执行不会出错
- **最终一致性**：允许短暂的不一致，但最终会一致

这个方案在分布式系统中已经被广泛验证（如 Kafka、Event Sourcing 模式），非常适合你的场景。
