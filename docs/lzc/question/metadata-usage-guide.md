# Devmapper 和 Devbox 的 Metadata 使用指南

## 一、Devmapper 的 Metadata 使用规则

### 1.1 MetaStore（containerd 标准）- 必须使用

**使用场景**：所有 containerd snapshotter 接口方法

| 方法 | 使用 MetaStore | 说明 |
|------|---------------|------|
| `Stat` | ✅ | 查询 snapshot 信息 |
| `Update` | ✅ | 更新 snapshot 信息 |
| `Usage` | ✅ | 查询 snapshot 使用量（从 MetaStore 读取，但会调用 pool.GetUsage 获取实际大小） |
| `Mounts` | ✅ | 获取挂载信息（从 MetaStore 读取 snapshot 信息） |
| `Prepare` | ✅ | 创建 active snapshot（在 MetaStore 中创建记录） |
| `View` | ✅ | 创建 view snapshot（在 MetaStore 中创建记录） |
| `Commit` | ✅ | 提交 snapshot（在 MetaStore 中更新状态） |
| `Remove` | ✅ | 删除 snapshot（从 MetaStore 中删除记录） |
| `Walk` | ✅ | 遍历所有 snapshot（从 MetaStore 读取） |

**存储内容**：
- Snapshot 的 key（用户可见的名称）
- Snapshot 的 ID（内部唯一标识）
- Parent 关系（快照层级）
- Labels（标签）
- Usage（使用量统计）

**代码示例**：

```go
// snapshots/devmapper/snapshotter.go:107-121
func (s *Snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
    err = s.store.WithTransaction(ctx, false, func(ctx context.Context) error {
        _, info, _, err = storage.GetInfo(ctx, key)  // ✅ 使用 MetaStore
        return err
    })
    return info, err
}

// snapshots/devmapper/snapshotter.go:202-216
func (s *Snapshotter) Prepare(ctx context.Context, key, parent string) ([]mount.Mount, error) {
    err = s.store.WithTransaction(ctx, true, func(ctx context.Context) error {
        mounts, err = s.createSnapshot(ctx, snapshots.KindActive, key, parent, opts...)
        // 内部会调用 storage.CreateSnapshot，使用 MetaStore
        return err
    })
    return mounts, err
}
```

### 1.2 PoolMetadata（devmapper 专用）- 设备操作时使用

**使用场景**：所有设备级别的操作

| 操作 | 使用 PoolMetadata | 说明 |
|------|------------------|------|
| `CreateThinDevice` | ✅ | 创建设备时保存设备元数据 |
| `CreateSnapshotDevice` | ✅ | 创建快照设备时保存设备元数据 |
| `RemoveDevice` | ✅ | 删除设备时更新设备状态 |
| `SuspendDevice` | ✅ | 暂停设备时更新设备状态 |
| `ResumeDevice` | ✅ | 恢复设备时更新设备状态 |
| `DeactivateDevice` | ✅ | 停用设备时更新设备状态 |
| `GetUsage` | ❌ | 直接从 dmsetup 查询，不使用数据库 |
| `MarkDeviceState` | ✅ | 标记设备状态（用于异步删除） |
| `WalkDevices` | ✅ | 遍历所有设备（用于 Cleanup） |

**存储内容**：
- 设备名称（如 `containerd-pool-snap-1`）
- 设备 ID（devmapper 内部的设备 ID）
- 设备状态（Creating, Activated, Removed 等）
- 父设备名称（用于快照）
- 错误信息（如果操作失败）

**代码示例**：

```go
// snapshots/devmapper/pool_device.go:215-264
func (p *PoolDevice) CreateThinDevice(ctx context.Context, deviceName string, size uint64) error {
    // 1. 在 PoolMetadata 中保存设备元数据
    metaErr = p.metadata.AddDevice(ctx, info)  // ✅ 使用 PoolMetadata
    
    // 2. 创建 devmapper 设备
    devErr = p.createDevice(ctx, info)
    
    // 3. 激活设备
    activeErr = p.activateDevice(ctx, info)
    
    return nil
}

// snapshots/devmapper/pool_device.go:500-521
func (p *PoolDevice) RemoveDevice(ctx context.Context, deviceName string) error {
    // 1. 从 PoolMetadata 读取设备信息
    info, err := p.metadata.GetDevice(ctx, deviceName)  // ✅ 使用 PoolMetadata
    
    // 2. 停用设备
    if err := p.DeactivateDevice(ctx, deviceName, false, true); err != nil {
        return err
    }
    
    // 3. 删除设备
    if err := p.deleteDevice(ctx, info); err != nil {
        return err
    }
    
    // 4. 从 PoolMetadata 删除记录
    if err := p.metadata.RemoveDevice(ctx, deviceName); err != nil {  // ✅ 使用 PoolMetadata
        return err
    }
    
    return nil
}
```

### 1.3 数据关联机制

**关键函数**：

```go
// snapshots/devmapper/snapshotter.go:500-503
func (s *Snapshotter) getDeviceName(snapID string) string {
    // 将 snapshot ID 转换为设备名称
    // 例如：snapID="1" -> "containerd-pool-snap-1"
    return fmt.Sprintf("%s-snap-%s", s.config.PoolName, snapID)
}
```

**关联流程**：

```
用户请求：key = "my-snapshot"
    ↓
MetaStore 查询：key -> ID = "1"
    ↓
getDeviceName("1") -> "containerd-pool-snap-1"
    ↓
PoolMetadata 查询：设备名称 -> DeviceInfo
    ↓
获取设备信息：DeviceID=1, State=Activated, ...
```

**完整示例**：

```go
// snapshots/devmapper/snapshotter.go:238-291
func (s *Snapshotter) Commit(ctx context.Context, name, key string) error {
    return s.store.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 1. 从 MetaStore 获取 snapshot ID
        id, snapInfo, _, err := storage.GetInfo(ctx, key)  // ✅ 使用 MetaStore
        
        // 2. 将 snapshot ID 转换为设备名称
        deviceName := s.getDeviceName(id)  // "containerd-pool-snap-1"
        
        // 3. 从设备获取使用量（直接查询 dmsetup，不使用数据库）
        size, err := s.pool.GetUsage(deviceName)
        
        // 4. 在 MetaStore 中提交 snapshot
        _, err = storage.CommitActive(ctx, key, name, usage, opts...)  // ✅ 使用 MetaStore
        
        // 5. 操作设备（使用 PoolMetadata）
        err = s.pool.SuspendDevice(ctx, deviceName)      // ✅ 内部使用 PoolMetadata
        err = s.pool.ResumeDevice(ctx, deviceName)       // ✅ 内部使用 PoolMetadata
        return s.pool.DeactivateDevice(ctx, deviceName, true, false)  // ✅ 内部使用 PoolMetadata
    })
}
```

---

## 二、Devbox 的 Metadata 使用规则

### 2.1 重要结论：Devbox 必须同时使用两种 Metadata

**❌ 错误理解**：
> "devbox 只需要记录自己的 metadata，不需要管 containerd 的 metadata"

**✅ 正确理解**：
> "devbox 必须使用 containerd 的 MetaStore（标准接口），同时可以创建自己的 LVM Metadata 数据库（用于 LVM 操作）"

### 2.2 为什么必须使用 MetaStore？

#### 原因 1：Containerd 接口要求

**Containerd 的 snapshotter 接口要求使用 MetaStore**：

```go
// containerd/snapshots/snapshotter.go
type Snapshotter interface {
    Stat(ctx context.Context, key string) (Info, error)
    Update(ctx context.Context, info Info, fieldpaths ...string) (Info, error)
    Usage(ctx context.Context, key string) (Usage, error)
    Mounts(ctx context.Context, key string) ([]mount.Mount, error)
    Prepare(ctx context.Context, key, parent string, opts ...Opt) ([]mount.Mount, error)
    View(ctx context.Context, key, parent string, opts ...Opt) ([]mount.Mount, error)
    Commit(ctx context.Context, name, key string, opts ...Opt) error
    Remove(ctx context.Context, key string) error
    Walk(ctx context.Context, fn WalkFunc, fs ...string) error
}
```

**这些接口的实现必须使用 MetaStore**，因为：
- `storage` 包提供了标准的实现
- 其他 containerd 组件依赖这些数据
- GC（垃圾回收）需要从 MetaStore 读取 snapshot 信息

#### 原因 2：其他组件依赖

**Containerd 的 GC（垃圾回收）依赖 MetaStore**：

```go
// containerd/services/snapshots/snapshotter.go
func (s *service) List(ctx context.Context, filters ...string) ([]snapshots.Info, error) {
    // GC 需要遍历所有 snapshot
    return s.snapshotter.Walk(ctx, func(ctx context.Context, info snapshots.Info) error {
        // 从 MetaStore 读取 snapshot 信息
        // ...
    })
}
```

**如果 devbox 不使用 MetaStore**：
- GC 无法找到 snapshot
- 其他 containerd 组件无法工作
- 系统无法正常运行

#### 原因 3：数据一致性

**MetaStore 存储的是 snapshot 的元数据**：
- Snapshot 的 key（用户可见的名称）
- Snapshot 的 ID（内部唯一标识）
- Parent 关系（快照层级）
- Labels（标签）

**这些数据是 containerd 的标准**，所有 snapshotter 都必须使用。

### 2.3 Devbox 当前的使用情况

**查看 devbox.go 的代码**：

```go
// snapshots/devbox/devbox.go
type Snapshotter struct {
    ms            MetaStore  // ✅ 使用 containerd 的 MetaStore
    // ...
}

// 所有接口方法都使用 MetaStore
func (o *Snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
    err = o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
        _, info, _, err = storage.GetInfo(ctx, key)  // ✅ 使用 MetaStore
        return err
    })
    return info, err
}
```

**当前问题**：
- ✅ 正确使用了 MetaStore（必须的）
- ❌ 没有独立的 LVM Metadata 数据库
- ❌ LVM 相关数据混在 MetaStore 中（通过 labels 或自定义存储）

---

## 三、Devbox 应该如何使用 Metadata？

### 3.1 推荐架构：双数据库（类似 Devmapper）

```
/var/lib/containerd/devbox/
├── metadata.db          # MetaStore（containerd 标准，必须）
│   └── snapshots/       # snapshot 元数据
└── lvm-metadata.db     # LVM Metadata（devbox 专用，新增）
    ├── lvs/            # LV 元数据
    │   ├── "devbox-xxx" -> {
    │   │       Name: "devbox-xxx",
    │   │       State: "Mounted",
    │   │       MountPoint: "/tmp/xxx",
    │   │       ContentKey: "xxx",
    │   │       ...
    │   │   }
    └── states/          # LV 状态
```

### 3.2 实现方案

#### 步骤 1：创建 LVM Metadata 数据库

```go
// snapshots/devbox/lvm_metadata.go
package devbox

import (
    "context"
    "encoding/json"
    "fmt"
    bolt "go.etcd.io/bbolt"
)

type LVState string

const (
    LVStateUnknown    LVState = ""
    LVStateCreating   LVState = "creating"
    LVStateActive     LVState = "active"
    LVStateMounted    LVState = "mounted"
    LVStateUnmounting LVState = "unmounting"
    LVStateUnmounted  LVState = "unmounted"
    LVStateRemoving   LVState = "removing"
    LVStateRemoved    LVState = "removed"
    LVStateFaulty     LVState = "faulty"
)

type LVInfo struct {
    Name        string   // LV 名称，如 "devbox-xxx"
    ContentKey  string   // 关联的 content key
    State       LVState  // LV 状态
    MountPoint  string   // 挂载点（如果有）
    Size        uint64   // LV 大小（字节）
    ParentName  string   // 父 LV 名称（如果是快照）
    Error       string   // 错误信息（如果失败）
}

type LVMMetadata struct {
    db *bolt.DB
}

var (
    lvsBucketName = []byte("lvs")
)

func NewLVMMetadata(dbfile string) (*LVMMetadata, error) {
    db, err := bolt.Open(dbfile, 0600, nil)
    if err != nil {
        return nil, err
    }
    
    metadata := &LVMMetadata{db: db}
    if err := metadata.ensureDatabaseInitialized(); err != nil {
        return nil, fmt.Errorf("failed to initialize database: %w", err)
    }
    
    return metadata, nil
}

func (m *LVMMetadata) ensureDatabaseInitialized() error {
    return m.db.Update(func(tx *bolt.Tx) error {
        if _, err := tx.CreateBucketIfNotExists(lvsBucketName); err != nil {
            return err
        }
        return nil
    })
}

func (m *LVMMetadata) AddLV(ctx context.Context, info *LVInfo) error {
    return m.db.Update(func(tx *bolt.Tx) error {
        bucket := tx.Bucket(lvsBucketName)
        data, err := json.Marshal(info)
        if err != nil {
            return err
        }
        return bucket.Put([]byte(info.Name), data)
    })
}

func (m *LVMMetadata) GetLV(ctx context.Context, lvName string) (*LVInfo, error) {
    var info LVInfo
    err := m.db.View(func(tx *bolt.Tx) error {
        bucket := tx.Bucket(lvsBucketName)
        data := bucket.Get([]byte(lvName))
        if data == nil {
            return fmt.Errorf("LV %q not found", lvName)
        }
        return json.Unmarshal(data, &info)
    })
    return &info, err
}

func (m *LVMMetadata) UpdateLVState(ctx context.Context, lvName string, state LVState) error {
    return m.db.Update(func(tx *bolt.Tx) error {
        bucket := tx.Bucket(lvsBucketName)
        data := bucket.Get([]byte(lvName))
        if data == nil {
            return fmt.Errorf("LV %q not found", lvName)
        }
        
        var info LVInfo
        if err := json.Unmarshal(data, &info); err != nil {
            return err
        }
        
        info.State = state
        newData, err := json.Marshal(info)
        if err != nil {
            return err
        }
        
        return bucket.Put([]byte(lvName), newData)
    })
}

func (m *LVMMetadata) RemoveLV(ctx context.Context, lvName string) error {
    return m.db.Update(func(tx *bolt.Tx) error {
        bucket := tx.Bucket(lvsBucketName)
        return bucket.Delete([]byte(lvName))
    })
}

func (m *LVMMetadata) WalkLVs(ctx context.Context, cb func(info *LVInfo) error) error {
    return m.db.View(func(tx *bolt.Tx) error {
        bucket := tx.Bucket(lvsBucketName)
        return bucket.ForEach(func(key, value []byte) error {
            var info LVInfo
            if err := json.Unmarshal(value, &info); err != nil {
                return err
            }
            return cb(&info)
        })
    })
}

func (m *LVMMetadata) Close() error {
    return m.db.Close()
}
```

#### 步骤 2：在 Snapshotter 中集成

```go
// snapshots/devbox/devbox.go
type Snapshotter struct {
    ms            MetaStore      // ✅ containerd 标准 MetaStore（必须）
    lvmMetadata   *LVMMetadata  // ✅ devbox 专用 LVM Metadata（新增）
    // ...
}

func NewSnapshotter(ctx context.Context, root string, opts ...Opt) (*Snapshotter, error) {
    // 1. 创建 MetaStore（必须）
    ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
    if err != nil {
        return nil, err
    }
    
    // 2. 创建 LVM Metadata（新增）
    lvmMetadata, err := NewLVMMetadata(filepath.Join(root, "lvm-metadata.db"))
    if err != nil {
        return nil, err
    }
    
    return &Snapshotter{
        ms:          ms,
        lvmMetadata: lvmMetadata,
        // ...
    }, nil
}
```

#### 步骤 3：在操作中使用

```go
// 创建 LV 时
func (o *Snapshotter) prepareLvmDirectory(ctx context.Context, ...) (string, string, error) {
    lvName := "devbox-" + contentKey
    
    // 1. 在 MetaStore 事务内创建 snapshot（必须）
    err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        snap, err := storage.CreateSnapshot(ctx, kind, key, parent, opts...)
        // ...
        return nil
    })
    
    // 2. 在 LVM Metadata 事务内记录 LV 信息（独立）
    lvInfo := &LVInfo{
        Name:       lvName,
        ContentKey: contentKey,
        State:      LVStateCreating,
    }
    if err := o.lvmMetadata.AddLV(ctx, lvInfo); err != nil {
        return "", lvName, err
    }
    
    // 3. 创建 LV
    err = lvm.CreateVolume(ctx, vol)
    if err != nil {
        o.lvmMetadata.UpdateLVState(ctx, lvName, LVStateFaulty)
        return "", lvName, err
    }
    
    // 4. 更新状态
    o.lvmMetadata.UpdateLVState(ctx, lvName, LVStateActive)
    
    // 5. 挂载
    if err = o.mountLvm(ctx, lvName, td); err != nil {
        return "", lvName, err
    }
    
    // 6. 更新状态
    o.lvmMetadata.UpdateLVState(ctx, lvName, LVStateMounted)
    
    return td, lvName, nil
}

// 卸载 LV 时（在事务外）
func (o *Snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) error {
    var needUnmount bool
    var lvName string
    
    // 1. 在 MetaStore 事务内只标记状态（快速）
    err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        if value, ok := info.Labels[unmountLvm]; ok && value == "true" {
            // 只更新 MetaStore（快速）
            storage.SetUnmountedWithKey(ctx, info.Name)
            needUnmount = true
            lvName = getLVNameFromContentKey(...)
        }
        return nil
    })
    
    // 2. 在 LVM Metadata 事务内执行 unmount（独立，不阻塞 MetaStore）
    if needUnmount {
        o.lvmMetadata.UpdateLVState(ctx, lvName, LVStateUnmounting)
        if err := o.unmountLvm(ctx, mountPath); err != nil {
            o.lvmMetadata.UpdateLVState(ctx, lvName, LVStateFaulty)
            return err
        }
        o.lvmMetadata.UpdateLVState(ctx, lvName, LVStateUnmounted)
    }
    
    return err
}
```

---

## 四、总结

### 4.1 Devmapper 的 Metadata 使用规则

| 操作类型 | 使用 MetaStore | 使用 PoolMetadata |
|---------|---------------|------------------|
| Snapshot 元数据 | ✅ | ❌ |
| 设备元数据 | ❌ | ✅ |
| Snapshotter 接口方法 | ✅ | ❌ |
| 设备操作 | ❌ | ✅ |

### 4.2 Devbox 的 Metadata 使用规则（推荐）

| 操作类型 | 使用 MetaStore | 使用 LVM Metadata |
|---------|---------------|------------------|
| Snapshot 元数据 | ✅（必须） | ❌ |
| LV 元数据 | ❌ | ✅（新增） |
| Snapshotter 接口方法 | ✅（必须） | ❌ |
| LVM 操作 | ❌ | ✅（新增） |

### 4.3 关键要点

1. **MetaStore 是必须的**：
   - containerd 接口要求
   - 其他组件依赖
   - 数据一致性保证

2. **LVM Metadata 是可选的但推荐**：
   - 解耦 LVM 操作和 snapshot 操作
   - 避免事务冲突
   - 更好的状态管理

3. **数据关联**：
   - 通过 content key 或 snapshot ID 关联
   - 两个数据库独立，但数据一致

4. **实现建议**：
   - 模仿 devmapper 的双数据库架构
   - MetaStore 用于 snapshot 元数据
   - LVM Metadata 用于 LV 元数据
   - 两个数据库独立事务，互不阻塞

---

## 五、参考资料

- **Devmapper Snapshotter**：`snapshots/devmapper/snapshotter.go`
- **Devmapper Metadata**：`snapshots/devmapper/metadata.go`
- **Containerd Storage**：`snapshots/storage/metastore.go`
- **Devbox Snapshotter**：`snapshots/devbox/devbox.go`

