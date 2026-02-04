# 容器生命周期完整实现（基于状态机）

## 一、场景说明

### 1.1 Devbox 的使用模式

**一个 Devbox → 一个 LV → 多个容器（顺序使用）**

```
Devbox-A (contentID: abc123)
  └─ LV: devbox-abc123
      ├─ Container-1: 创建 → 使用 → 停止
      ├─ Container-2: 创建 → 使用 → 停止  (复用同一个 LV)
      └─ Container-3: 创建 → 使用 → 停止  (复用同一个 LV)
```

### 1.2 两种稳定状态

| 状态 | 含义 | LV 状态 | 容器状态 | 何时出现 |
|------|------|--------|---------|---------|
| `created` | LV 已创建但未挂载 | 设备存在，未挂载 | 无容器使用 | 1. LV 刚创建完<br>2. 上个容器停止，卸载后 |
| `mounted` | LV 已挂载 | 设备已挂载 | 有容器使用 | 容器运行中 |

### 1.3 完整生命周期

```
初始状态：none (LV 不存在)

第一个容器启动：
none → creating → created → mounting → mounted
                             ↑
                      第一次挂载，需要格式化

第一个容器停止：
mounted → unmounting → unmounted (实际变为 created 状态)

第二个容器启动（复用 LV）：
created → mounting → mounted
           ↑
     不需要格式化，直接挂载

第二个容器停止：
mounted → unmounting → created

... 循环往复 ...

最终删除（Devbox 删除）：
created → removing → removed → (清理 metadata)
```

---

## 二、核心数据结构

### 2.1 LVInfo

```go
type LVInfo struct {
    // 基本信息
    Name         string    // LV 名称：devbox-{contentID}
    ContentID    string    // Content ID（多个容器共享同一个 content）
    DevboxID     string    // Devbox ID
    
    // 状态信息
    State        string    // 当前状态：created, mounting, mounted, unmounting, removing
    PrevState    string    // 前一个稳定状态（用于回退）
    
    // 挂载信息
    MountPoint   string    // 当前挂载点（可能随容器变化）
    MountCount   int       // 挂载次数（用于统计）
    
    // 容器信息
    CurrentKey   string    // 当前使用的 snapshot key（容器标识）
    LastKey      string    // 上一个使用的 snapshot key
    
    // 操作信息
    OperationStartTime time.Time // 当前操作开始时间
    RetryCount         int       // 当前操作重试次数
    
    // 错误信息
    LastError    string    // 最后的错误信息
    ErrorTime    time.Time // 错误时间
    
    // 时间戳
    CreatedTime  time.Time // LV 创建时间
    UpdatedTime  time.Time // 最后更新时间
    
    // LV 属性
    Capacity     string    // 容量
    IsFormatted  bool      // 是否已格式化（重要！）
}
```

### 2.2 关键字段说明

**`IsFormatted`**：
- `false`：LV 刚创建，从未格式化（第一个容器）
- `true`：LV 已格式化过（后续容器可以直接挂载）

**`CurrentKey`**：
- 记录当前使用这个 LV 的容器 key
- 用于追踪和调试

**`MountCount`**：
- 记录这个 LV 被挂载过多少次
- 用于统计和监控

---

## 三、Prepare() - 创建容器的完整实现

### 3.1 接口说明

```go
// Prepare 为活动快照准备挂载点
// 场景 1：第一个容器（LV 不存在）
// 场景 2：后续容器（LV 已存在，状态为 created）
func (o *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error)
```

### 3.2 完整实现

```go
func (o *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
    // ========== 第 1 步：获取 Content ID ==========
    contentID := getContentID(opts...)  // 从 opts 中提取
    lvName := "devbox-" + contentID
    
    log.G(ctx).Infof("Prepare: key=%s, parent=%s, contentID=%s", key, parent, contentID)
    
    // ========== 第 2 步：检查 LV 当前状态 ==========
    currentState, lvInfo, err := o.getLVState(ctx, lvName)
    if err != nil {
        return nil, fmt.Errorf("failed to get LV state: %w", err)
    }
    
    log.G(ctx).Infof("Prepare: LV %s current state: %s", lvName, currentState)
    
    // ========== 第 3 步：根据状态决定操作 ==========
    switch currentState {
    
    case LVStateNone:
        // 场景：第一个容器，LV 不存在
        log.G(ctx).Info("Prepare: First container, creating LV from scratch")
        return o.prepareFirstContainer(ctx, key, parent, contentID, opts...)
        
    case LVStateCreated:
        // 场景：后续容器，LV 已存在但未挂载（上个容器刚停止）
        log.G(ctx).Info("Prepare: Subsequent container, LV already exists")
        return o.prepareSubsequentContainer(ctx, key, contentID, lvInfo)
        
    case LVStateMounting:
        // 场景：上次挂载被中断（可能是进程崩溃）
        log.G(ctx).Warn("Prepare: Recovering from mounting state")
        return o.prepareFromMounting(ctx, key, contentID, lvInfo)
        
    case LVStateMounted:
        // 场景：重复调用或未正确清理
        log.G(ctx).Warn("Prepare: LV already mounted")
        return o.prepareFromMounted(ctx, key, contentID, lvInfo)
        
    case LVStateUnmounting:
        // 场景：上个容器正在停止（或停止被中断）
        log.G(ctx).Warn("Prepare: Previous container is unmounting")
        return o.prepareFromUnmounting(ctx, key, contentID, lvInfo)
        
    default:
        return nil, fmt.Errorf("unexpected LV state: %s", currentState)
    }
}
```

---

## 四、场景 1：第一个容器（LV 不存在）

### 4.1 完整流程

```go
func (o *Snapshotter) prepareFirstContainer(ctx context.Context, key, parent, contentID string, opts ...snapshots.Opt) ([]mount.Mount, error) {
    lvName := "devbox-" + contentID
    
    log.G(ctx).Infof("Prepare: Creating first container for devbox %s", contentID)
    
    // ========== 步骤 1：创建 snapshot 元数据 ==========
    var snapID string
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        snap, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, parent, opts...)
        if err != nil {
            return err
        }
        snapID = snap.ID
        
        // 同时创建 LV 元数据，标记为 creating
        return storage.AddLV(ctx, &LVInfo{
            Name:               lvName,
            ContentID:          contentID,
            DevboxID:           getDevboxID(opts...),
            State:              LVStateCreating,
            PrevState:          LVStateNone,
            CurrentKey:         key,
            Capacity:           getCapacity(opts...),
            IsFormatted:        false,  // 关键：标记未格式化
            MountCount:         0,
            OperationStartTime: time.Now(),
            CreatedTime:        time.Now(),
            UpdatedTime:        time.Now(),
        })
    })
    if err != nil {
        return nil, fmt.Errorf("failed to create snapshot metadata: %w", err)
    }
    
    // ========== 步骤 2：创建 LV（事务外）==========
    log.G(ctx).Info("Prepare: Creating LV")
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{
            Capacity: getCapacity(opts...),
            VolGroup: o.lvmVgName,
        },
    }
    
    createErr := lvm.CreateVolume(ctx, vol)
    if createErr != nil {
        log.G(ctx).WithError(createErr).Error("Prepare: Failed to create LV")
        
        // 回退：删除所有 metadata
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            storage.Remove(ctx, key)
            storage.RemoveLV(ctx, lvName)
            return nil
        })
        
        // 尝试清理可能的残留
        _ = lvm.ForceDestroyVolume(ctx, vol)
        
        return nil, fmt.Errorf("failed to create LV: %w", createErr)
    }
    
    // ========== 步骤 3：更新状态为 created ==========
    log.G(ctx).Info("Prepare: LV created successfully")
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateCreated
            lv.UpdatedTime = time.Now()
        })
    })
    
    // ========== 步骤 4：格式化并挂载 ==========
    return o.formatAndMountLV(ctx, key, lvName, snapID, true)  // needFormat=true
}
```

---

## 五、场景 2：后续容器（LV 已存在，状态为 created）

### 5.1 完整流程

```go
func (o *Snapshotter) prepareSubsequentContainer(ctx context.Context, key, contentID string, lvInfo *LVInfo) ([]mount.Mount, error) {
    lvName := lvInfo.Name
    
    log.G(ctx).Infof("Prepare: Preparing subsequent container for devbox %s (mount count: %d)", 
        contentID, lvInfo.MountCount)
    
    // ========== 步骤 1：创建 snapshot 元数据 ==========
    var snapID string
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 创建新的 snapshot
        snap, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, "", nil)
        if err != nil {
            return err
        }
        snapID = snap.ID
        
        // 更新 LV 元数据
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.LastKey = lv.CurrentKey      // 保存上一个 key
            lv.CurrentKey = key             // 更新当前 key
            lv.UpdatedTime = time.Now()
        })
    })
    if err != nil {
        return nil, fmt.Errorf("failed to create snapshot metadata: %w", err)
    }
    
    // ========== 步骤 2：直接挂载（无需格式化）==========
    // 关键：lvInfo.IsFormatted 应该为 true
    if !lvInfo.IsFormatted {
        log.G(ctx).Warn("Prepare: LV should be formatted but isn't, will format")
    }
    
    return o.formatAndMountLV(ctx, key, lvName, snapID, !lvInfo.IsFormatted)  // needFormat 取决于实际状态
}
```

---

## 六、统一的挂载函数

### 6.1 formatAndMountLV - 处理格式化和挂载

```go
func (o *Snapshotter) formatAndMountLV(ctx context.Context, key, lvName, snapID string, needFormat bool) ([]mount.Mount, error) {
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    mountPoint := o.getMountPoint(key)  // 每个容器有独立的挂载点
    
    log.G(ctx).Infof("Prepare: Mounting LV %s to %s (needFormat=%v)", lvName, mountPoint, needFormat)
    
    // ========== 步骤 1：标记为 mounting ==========
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.PrevState = lv.State         // 保存当前状态（created）
            lv.State = LVStateMounting
            lv.MountPoint = mountPoint
            lv.OperationStartTime = time.Now()
            lv.RetryCount = 0
        })
    })
    if err != nil {
        return nil, fmt.Errorf("failed to mark as mounting: %w", err)
    }
    
    // ========== 步骤 2：创建挂载点目录 ==========
    if err := os.MkdirAll(mountPoint, 0755); err != nil {
        o.rollbackLVState(ctx, lvName, LVStateCreated, err)
        return nil, fmt.Errorf("failed to create mount point: %w", err)
    }
    
    // ========== 步骤 3：格式化（如果需要）==========
    if needFormat {
        log.G(ctx).Info("Prepare: Formatting LV (first container)")
        
        formatErr := o.formatWithRetry(ctx, devicePath, 3)
        if formatErr != nil {
            log.G(ctx).WithError(formatErr).Error("Prepare: Failed to format LV")
            o.rollbackLVState(ctx, lvName, LVStateCreated, formatErr)
            return nil, fmt.Errorf("failed to format LV: %w", formatErr)
        }
        
        // 标记已格式化
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.IsFormatted = true
            })
        })
    } else {
        log.G(ctx).Info("Prepare: Skipping format (LV already formatted)")
    }
    
    // ========== 步骤 4：挂载（带重试）==========
    mountErr := o.mountWithRetry(ctx, devicePath, mountPoint, 3)
    if mountErr != nil {
        log.G(ctx).WithError(mountErr).Error("Prepare: Failed to mount LV")
        o.rollbackLVState(ctx, lvName, LVStateCreated, mountErr)
        return nil, fmt.Errorf("failed to mount LV: %w", mountErr)
    }
    
    // ========== 步骤 5：标记为 mounted ==========
    log.G(ctx).Info("Prepare: Mount successful")
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateMounted
            lv.MountPoint = mountPoint
            lv.MountCount++              // 增加挂载次数
            lv.LastError = ""            // 清除错误
            lv.RetryCount = 0            // 清除重试次数
            lv.UpdatedTime = time.Now()
        })
    })
    
    log.G(ctx).Infof("Prepare: Successfully mounted LV %s (total mounts: %d)", 
        lvName, lvInfo.MountCount+1)
    
    return []mount.Mount{{
        Type:    "bind",
        Source:  mountPoint,
        Options: []string{"rbind"},
    }}, nil
}
```

---

## 七、Remove() - 容器停止的完整实现

### 7.1 接口说明

```go
// Remove 删除快照（容器停止时调用）
// 注意：对于 devbox，我们只卸载 LV，不删除它（下个容器还会用）
func (o *Snapshotter) Remove(ctx context.Context, key string) error
```

### 7.2 完整实现

```go
func (o *Snapshotter) Remove(ctx context.Context, key string) error {
    log.G(ctx).Infof("Remove: Removing snapshot %s", key)
    
    // ========== 步骤 1：查询 snapshot 和 LV 信息 ==========
    var contentID string
    var lvName string
    var shouldDeleteLV bool
    
    err := o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
        // 获取 snapshot 信息
        snap, err := storage.Get(ctx, key)
        if err != nil {
            return err
        }
        
        // 获取 content ID
        contentID = getContentIDFromSnapshot(snap)
        lvName = "devbox-" + contentID
        
        // 检查是否需要删除 LV（通过 label 判断）
        shouldDeleteLV = snap.Labels["devbox.delete"] == "true"
        
        return nil
    })
    if err != nil {
        if errors.Is(err, errdefs.ErrNotFound) {
            // Snapshot 不存在，可能已被删除
            return nil
        }
        return fmt.Errorf("failed to get snapshot: %w", err)
    }
    
    // ========== 步骤 2：检查 LV 状态 ==========
    currentState, lvInfo, err := o.getLVState(ctx, lvName)
    if err != nil {
        return fmt.Errorf("failed to get LV state: %w", err)
    }
    
    log.G(ctx).Infof("Remove: LV %s state=%s, shouldDelete=%v", lvName, currentState, shouldDeleteLV)
    
    // ========== 步骤 3：根据状态和删除标志决定操作 ==========
    if shouldDeleteLV {
        // 最后一个容器，需要删除 LV
        return o.removeAndDeleteLV(ctx, key, lvName, currentState, lvInfo)
    } else {
        // 非最后一个容器，只卸载 LV
        return o.removeAndUnmountLV(ctx, key, lvName, currentState, lvInfo)
    }
}
```

### 7.3 场景 1：容器停止，保留 LV

```go
func (o *Snapshotter) removeAndUnmountLV(ctx context.Context, key, lvName string, currentState string, lvInfo *LVInfo) error {
    log.G(ctx).Info("Remove: Unmounting LV (will be reused by next container)")
    
    // 根据当前状态决定操作
    switch currentState {
    case LVStateMounted:
        // 正常流程：卸载
        return o.unmountAndKeepLV(ctx, key, lvName, lvInfo)
        
    case LVStateUnmounting:
        // 上次卸载被中断，检查实际状态
        return o.removeFromUnmounting(ctx, key, lvName, lvInfo)
        
    case LVStateCreated:
        // 已经卸载了（可能是重复调用）
        log.G(ctx).Info("Remove: LV already unmounted")
        return o.removeSnapshotMetadata(ctx, key)
        
    default:
        return fmt.Errorf("unexpected LV state for unmount: %s", currentState)
    }
}

func (o *Snapshotter) unmountAndKeepLV(ctx context.Context, key, lvName string, lvInfo *LVInfo) error {
    mountPoint := lvInfo.MountPoint
    
    // ========== 步骤 1：标记为 unmounting ==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.PrevState = lv.State  // mounted
            lv.State = LVStateUnmounting
            lv.OperationStartTime = time.Now()
            lv.RetryCount = 0
        })
    })
    
    // ========== 步骤 2：卸载（带重试）==========
    unmountErr := o.unmountWithRetry(ctx, mountPoint, 3)
    if unmountErr != nil {
        log.G(ctx).WithError(unmountErr).Error("Remove: Failed to unmount LV")
        
        // 回退到 mounted 状态
        o.rollbackLVState(ctx, lvName, LVStateMounted, unmountErr)
        return fmt.Errorf("failed to unmount LV: %w", unmountErr)
    }
    
    // ========== 步骤 3：标记为 created（而不是 unmounted）==========
    // 关键：保持 created 状态，下个容器可以直接挂载
    log.G(ctx).Info("Remove: Unmount successful, marking as created")
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateCreated   // 关键：回到 created 状态
            lv.LastKey = lv.CurrentKey   // 保存容器 key
            lv.CurrentKey = ""           // 清除当前容器
            lv.MountPoint = ""           // 清除挂载点
            lv.LastError = ""
            lv.RetryCount = 0
            lv.UpdatedTime = time.Now()
        })
    })
    
    // ========== 步骤 4：清理 snapshot 元数据 ==========
    return o.removeSnapshotMetadata(ctx, key)
}
```

### 7.4 场景 2：最后一个容器停止，删除 LV

```go
func (o *Snapshotter) removeAndDeleteLV(ctx context.Context, key, lvName string, currentState string, lvInfo *LVInfo) error {
    log.G(ctx).Info("Remove: Last container, will delete LV")
    
    // ========== 步骤 1：先卸载（如果已挂载）==========
    if currentState == LVStateMounted {
        err := o.unmountAndKeepLV(ctx, key, lvName, lvInfo)
        if err != nil {
            return fmt.Errorf("failed to unmount before delete: %w", err)
        }
        
        // 重新获取状态（应该是 created）
        currentState, lvInfo, _ = o.getLVState(ctx, lvName)
    }
    
    // ========== 步骤 2：删除 LV ==========
    if currentState == LVStateCreated {
        return o.deleteLV(ctx, key, lvName)
    }
    
    return fmt.Errorf("unexpected LV state for delete: %s", currentState)
}

func (o *Snapshotter) deleteLV(ctx context.Context, key, lvName string) error {
    // ========== 步骤 1：标记为 removing ==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.PrevState = lv.State  // created
            lv.State = LVStateRemoving
            lv.OperationStartTime = time.Now()
            lv.RetryCount = 0
        })
    })
    
    // ========== 步骤 2：删除 LV（带重试）==========
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{VolGroup: o.lvmVgName},
    }
    
    deleteErr := o.deleteLVWithRetry(ctx, vol, 3)
    if deleteErr != nil {
        log.G(ctx).WithError(deleteErr).Error("Remove: Failed to delete LV")
        
        // 回退到 created 状态
        o.rollbackLVState(ctx, lvName, LVStateCreated, deleteErr)
        return fmt.Errorf("failed to delete LV: %w", deleteErr)
    }
    
    // ========== 步骤 3：标记为 removed ==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateRemoved
            lv.UpdatedTime = time.Now()
        })
    })
    
    // ========== 步骤 4：清理所有 metadata ==========
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 删除 snapshot
        storage.Remove(ctx, key)
        // 删除 LV 记录
        storage.RemoveLV(ctx, lvName)
        return nil
    })
}
```

---

## 八、完整时间线示例

### 8.1 场景：3 个容器顺序使用同一个 Devbox

```
初始状态：none

=== 容器 1 启动 ===
T1:  Prepare(key=container-1)
     → LV 不存在 (state=none)
     → 创建 LV: none → creating → created
     → 格式化: IsFormatted=false → 执行 mkfs → IsFormatted=true
     → 挂载: created → mounting → mounted
     → CurrentKey=container-1, MountCount=1

=== 容器 1 运行中 ===
T2:  state=mounted, CurrentKey=container-1

=== 容器 1 停止 ===
T3:  Remove(key=container-1, shouldDelete=false)
     → 卸载: mounted → unmounting → created (保留 LV)
     → CurrentKey="", LastKey=container-1
     → IsFormatted=true (保持)

=== 容器 2 启动 ===
T4:  Prepare(key=container-2)
     → LV 已存在 (state=created, IsFormatted=true)
     → 跳过创建和格式化
     → 直接挂载: created → mounting → mounted
     → CurrentKey=container-2, LastKey=container-1, MountCount=2

=== 容器 2 运行中 ===
T5:  state=mounted, CurrentKey=container-2

=== 容器 2 停止 ===
T6:  Remove(key=container-2, shouldDelete=false)
     → 卸载: mounted → unmounting → created (保留 LV)
     → CurrentKey="", LastKey=container-2
     → IsFormatted=true (保持)

=== 容器 3 启动 ===
T7:  Prepare(key=container-3)
     → LV 已存在 (state=created, IsFormatted=true)
     → 跳过创建和格式化
     → 直接挂载: created → mounting → mounted
     → CurrentKey=container-3, LastKey=container-2, MountCount=3

=== 容器 3 运行中 ===
T8:  state=mounted, CurrentKey=container-3

=== 容器 3 停止（最后一个，删除 Devbox）===
T9:  Remove(key=container-3, shouldDelete=true)
     → 卸载: mounted → unmounting → created
     → 删除 LV: created → removing → removed
     → 清理 metadata
     → LV 记录被删除

最终状态：none（LV 已删除）
```

### 8.2 关键日志示例

```
# 容器 1 启动（首次）
[INFO] Prepare: key=container-1, contentID=abc123
[INFO] Prepare: LV devbox-abc123 current state: none
[INFO] Prepare: First container, creating LV from scratch
[INFO] Prepare: Creating LV
[INFO] Prepare: LV created successfully
[INFO] Prepare: Formatting LV (first container)
[INFO] Prepare: Mounting LV devbox-abc123 to /mnt/container-1 (needFormat=true)
[INFO] Prepare: Mount successful
[INFO] Prepare: Successfully mounted LV devbox-abc123 (total mounts: 1)

# 容器 1 停止
[INFO] Remove: Removing snapshot container-1
[INFO] Remove: LV devbox-abc123 state=mounted, shouldDelete=false
[INFO] Remove: Unmounting LV (will be reused by next container)
[INFO] Remove: Unmount successful, marking as created

# 容器 2 启动（复用）
[INFO] Prepare: key=container-2, contentID=abc123
[INFO] Prepare: LV devbox-abc123 current state: created
[INFO] Prepare: Subsequent container, LV already exists
[INFO] Prepare: Preparing subsequent container for devbox abc123 (mount count: 1)
[INFO] Prepare: Skipping format (LV already formatted)
[INFO] Prepare: Mounting LV devbox-abc123 to /mnt/container-2 (needFormat=false)
[INFO] Prepare: Mount successful
[INFO] Prepare: Successfully mounted LV devbox-abc123 (total mounts: 2)

# 容器 2 停止
[INFO] Remove: Removing snapshot container-2
[INFO] Remove: LV devbox-abc123 state=mounted, shouldDelete=false
[INFO] Remove: Unmounting LV (will be reused by next container)
[INFO] Remove: Unmount successful, marking as created

# 容器 3 启动（复用）
[INFO] Prepare: key=container-3, contentID=abc123
[INFO] Prepare: LV devbox-abc123 current state: created
[INFO] Prepare: Subsequent container, LV already exists
[INFO] Prepare: Preparing subsequent container for devbox abc123 (mount count: 2)
[INFO] Prepare: Skipping format (LV already formatted)
[INFO] Prepare: Mounting LV devbox-abc123 to /mnt/container-3 (needFormat=false)
[INFO] Prepare: Mount successful
[INFO] Prepare: Successfully mounted LV devbox-abc123 (total mounts: 3)

# 容器 3 停止（删除 Devbox）
[INFO] Remove: Removing snapshot container-3
[INFO] Remove: LV devbox-abc123 state=mounted, shouldDelete=true
[INFO] Remove: Last container, will delete LV
[INFO] Remove: Unmounting LV (will be reused by next container)
[INFO] Remove: Unmount successful, marking as created
[INFO] Remove: Deleting LV devbox-abc123
[INFO] Remove: LV deleted successfully
```

---

## 九、总结

### 9.1 关键设计点

1. **`IsFormatted` 标志**
   - 首次创建 LV：`IsFormatted=false` → 需要格式化
   - 后续容器：`IsFormatted=true` → 跳过格式化

2. **`created` 状态的双重含义**
   - 首次创建：LV 刚创建，未格式化
   - 容器间：LV 已格式化，已卸载，等待下个容器

3. **容器停止时不删除 LV**
   - 只卸载，状态回到 `created`
   - 保留 `IsFormatted=true`
   - 下个容器可以直接挂载

4. **挂载点独立**
   - 每个容器有独立的挂载点：`/mnt/container-{key}`
   - 同一个 LV 可以挂载到不同位置

5. **统计信息**
   - `MountCount`：记录挂载次数
   - `CurrentKey`/`LastKey`：追踪容器

### 9.2 优势

✅ **效率**：后续容器无需重新创建和格式化 LV  
✅ **清晰**：状态转换明确，两种稳定状态  
✅ **可靠**：失败立即回退，系统始终稳定  
✅ **可追踪**：记录容器使用历史  
✅ **灵活**：支持删除标志，决定是否保留 LV

### 9.3 实施建议

**Phase 1**：
1. 实现基本的 Prepare() 和 Remove()
2. 支持 `IsFormatted` 标志
3. 实现回退机制

**Phase 2**：
4. 添加容器追踪（CurrentKey, LastKey）
5. 添加统计（MountCount）
6. 实现删除标志（shouldDelete）

**Phase 3**：
7. 优化格式化检测
8. 添加监控指标
9. 完善错误处理

