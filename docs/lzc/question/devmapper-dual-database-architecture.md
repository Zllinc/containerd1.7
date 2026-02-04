# Devmapper 双数据库架构详解

## 一、核心问题：为什么两个 BoltDB 可以避免冲突？

**关键答案**：虽然都是 BoltDB，但是：
1. **不同的文件** = 不同的数据库实例
2. **不同的锁** = 互不干扰
3. **不同的职责** = 解耦设计

---

## 二、双数据库架构概览

### 2.1 数据库文件位置

```go
// snapshots/devmapper/snapshotter.go:84
store, err := storage.NewMetaStore(filepath.Join(config.RootPath, metadataFileName))
// 文件路径：/var/lib/containerd/devmapper/metadata.db

// snapshots/devmapper/pool_device.go:65
dbpath := filepath.Join(config.RootPath, config.PoolName+".db")
poolMetaStore, err := NewPoolMetadata(dbpath)
// 文件路径：/var/lib/containerd/devmapper/containerd-pool.db
```

**实际文件结构**：
```
/var/lib/containerd/devmapper/
├── metadata.db          # MetaStore（containerd 标准）
└── containerd-pool.db   # PoolMetadata（devmapper 专用）
```

### 2.2 数据库初始化

#### MetaStore（containerd 标准）

```go
// snapshots/devbox/storage/metastore.go:77-81
func NewMetaStore(dbfile string) (*MetaStore, error) {
    return &MetaStore{
        dbfile: dbfile,  // 只是保存路径，延迟打开
    }, nil
}

// 第一次使用时才打开数据库
func (ms *MetaStore) TransactionContext(ctx context.Context, writable bool) {
    ms.dbL.Lock()
    if ms.db == nil {
        db, err := bolt.Open(ms.dbfile, 0600, nil)  // 打开 metadata.db
        ms.db = db
    }
    ms.dbL.Unlock()
    // ...
}
```

#### PoolMetadata（devmapper 专用）

```go
// snapshots/devmapper/metadata.go:65-77
func NewPoolMetadata(dbfile string) (*PoolMetadata, error) {
    db, err := bolt.Open(dbfile, 0600, nil)  // 立即打开 poolname.db
    if err != nil {
        return nil, err
    }
    
    metadata := &PoolMetadata{db: db}
    if err := metadata.ensureDatabaseInitialized(); err != nil {
        return nil, fmt.Errorf("failed to initialize database: %w", err)
    }
    
    return metadata, nil
}
```

**关键区别**：
- `MetaStore`：延迟打开（第一次事务时）
- `PoolMetadata`：立即打开（初始化时）

---

## 三、数据库内容对比

### 3.1 MetaStore (metadata.db) - Snapshot 元数据

**存储内容**：
- Snapshot 的 key（用户可见的名称）
- Snapshot 的 ID（内部唯一标识）
- Parent 关系（快照层级）
- Labels（标签，如文件系统类型）
- Usage（使用量统计）

**数据结构**（由 containerd 的 `storage` 包定义）：

```go
// storage 包内部结构（简化）
type Snapshot struct {
    Kind      snapshots.Kind  // Active, Committed, View
    ID        string          // 唯一 ID，如 "1", "2", "3"
    ParentIDs []string        // 父快照 ID 列表
}

type Info struct {
    Name    string            // 用户可见的 key
    Kind    snapshots.Kind
    Parent  string            // 父快照的 key
    Labels  map[string]string // 标签
    Created time.Time
    Updated time.Time
}
```

**BoltDB 结构**：
```
metadata.db
└── snapshots/
    ├── keys/              # key -> ID 映射
    │   ├── "snapshot-1" -> "1"
    │   └── "snapshot-2" -> "2"
    ├── ids/               # ID -> Snapshot 结构
    │   ├── "1" -> {Kind: Active, ParentIDs: []}
    │   └── "2" -> {Kind: Committed, ParentIDs: ["1"]}
    └── info/              # ID -> Info 结构
        ├── "1" -> {Name: "snapshot-1", Labels: {...}}
        └── "2" -> {Name: "snapshot-2", Labels: {...}}
```

### 3.2 PoolMetadata (poolname.db) - 设备元数据

**存储内容**：
- 设备名称（如 `containerd-pool-snap-1`）
- 设备 ID（devmapper 内部的设备 ID，如 1, 2, 3）
- 设备状态（Creating, Activated, Removed 等）
- 父设备名称（用于快照）
- 错误信息（如果操作失败）

**数据结构**：

```go
// snapshots/devmapper/device_state.go
type DeviceInfo struct {
    Name       string      // 设备名称，如 "containerd-pool-snap-1"
    DeviceID   uint32      // devmapper 设备 ID，如 1, 2, 3
    Size       uint64      // 设备大小（字节）
    ParentName string      // 父设备名称（如果是快照）
    State      DeviceState // 设备状态
    Error      string      // 错误信息（如果失败）
}
```

**BoltDB 结构**：
```
containerd-pool.db
├── devices/              # 设备名称 -> DeviceInfo
│   ├── "containerd-pool-snap-1" -> {
│   │       Name: "containerd-pool-snap-1",
│   │       DeviceID: 1,
│   │       Size: 8589934592,
│   │       State: "Activated",
│   │       Error: ""
│   │   }
│   └── "containerd-pool-snap-2" -> {
│           Name: "containerd-pool-snap-2",
│           DeviceID: 2,
│           Size: 8589934592,
│           ParentName: "containerd-pool-snap-1",
│           State: "Activated",
│           Error: ""
│       }
└── device_ids/           # 设备 ID -> 状态（已用/空闲/故障）
    ├── "1" -> 1 (taken)
    ├── "2" -> 1 (taken)
    └── "3" -> 0 (free)
```

---

## 四、数据关联机制

### 4.1 通过 Snapshot ID 关联

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

### 4.2 完整的数据流示例

**场景：创建一个新的 snapshot**

```go
// 1. 在 MetaStore 中创建 snapshot 记录
func (s *Snapshotter) Prepare(ctx context.Context, key, parent string) {
    err = s.store.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 在 metadata.db 中创建记录
        snap, err := storage.CreateSnapshot(ctx, kind, key, parent, opts...)
        // snap.ID = "1"（自动生成）
        // 此时 metadata.db 中有：
        //   keys/"my-snapshot" -> "1"
        //   ids/"1" -> {Kind: Active, ParentIDs: []}
        
        // 2. 使用 snap.ID 创建设备
        deviceName := s.getDeviceName(snap.ID)  // "containerd-pool-snap-1"
        
        // 3. 在 PoolMetadata 中创建设备记录（独立事务）
        err := s.pool.CreateThinDevice(ctx, deviceName, size)
        // 此时 containerd-pool.db 中有：
        //   devices/"containerd-pool-snap-1" -> {
        //       DeviceID: 1,
        //       State: "Activated",
        //       ...
        //   }
        
        return nil
    })
}
```

**关键点**：
- `storage.CreateSnapshot` 在 **MetaStore 事务内**执行
- `pool.CreateThinDevice` 在 **PoolMetadata 事务内**执行
- **两个事务是独立的**，互不阻塞

---

## 五、为什么可以避免冲突？

### 5.1 BoltDB 的锁机制

**BoltDB 的锁是文件级别的**：

```go
// BoltDB 内部实现（简化）
type DB struct {
    file *os.File
    rwtx *Tx  // 读写事务
    // ...
}

// 打开数据库时获取文件锁
func Open(path string, mode os.FileMode, options *Options) (*DB, error) {
    file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, mode)
    // 每个文件有独立的锁
}
```

**关键理解**：
- `metadata.db` 和 `containerd-pool.db` 是**两个不同的文件**
- 每个文件有**独立的文件锁**
- 操作 `metadata.db` 不会阻塞 `containerd-pool.db`，反之亦然

### 5.2 实际执行流程对比

#### 场景 1：在事务内执行设备操作（Devmapper）

```go
// snapshots/devmapper/snapshotter.go:238-291
func (s *Snapshotter) Commit(ctx context.Context, name, key string) error {
    return s.store.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 1. MetaStore 事务开始（锁定 metadata.db）
        id, snapInfo, _, err := storage.GetInfo(ctx, key)
        
        // 2. 获取设备名称
        deviceName := s.getDeviceName(id)
        
        // 3. 执行设备操作（使用 PoolMetadata，独立事务）
        // ⚠️ 注意：这里会开启一个新的 PoolMetadata 事务
        // ⚠️ 但不会阻塞 MetaStore 事务，因为它们是不同的文件
        err = s.pool.SuspendDevice(ctx, deviceName)
        err = s.pool.ResumeDevice(ctx, deviceName)
        err = s.pool.DeactivateDevice(ctx, deviceName, true, false)
        
        // 4. MetaStore 事务提交（解锁 metadata.db）
        return nil
    })
}
```

**内部执行**：

```go
// pool.SuspendDevice 内部
func (p *PoolDevice) SuspendDevice(ctx context.Context, deviceName string) error {
    // 开启 PoolMetadata 事务（锁定 containerd-pool.db）
    return p.metadata.UpdateDevice(ctx, deviceName, func(deviceInfo *DeviceInfo) error {
        deviceInfo.State = Suspended
        return nil
    })
    // PoolMetadata 事务提交（解锁 containerd-pool.db）
}
```

**时间线**：
```
时间轴：
T1: MetaStore 事务开始（锁定 metadata.db）
T2: PoolMetadata 事务开始（锁定 containerd-pool.db）
T3: 执行 dmsetup suspend（快速，几毫秒）
T4: PoolMetadata 事务提交（解锁 containerd-pool.db）
T5: MetaStore 事务提交（解锁 metadata.db）

关键：T2-T4 期间，metadata.db 仍然被锁定
     但 PoolMetadata 操作很快，不会长时间阻塞
```

#### 场景 2：如果使用单一数据库（Devbox 当前架构）

```go
// 假设所有数据都在 metadata.db 中
func (o *Snapshotter) Update(ctx context.Context, info snapshots.Info) error {
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 1. MetaStore 事务开始（锁定 metadata.db）
        
        // 2. 执行 unmount（慢速操作，可能 5 分钟）
        err := o.unmountLvm(ctx, mountPath)
        // ⚠️ 问题：unmount 在事务内，metadata.db 被锁定 5 分钟！
        // ⚠️ 其他所有操作都无法进行（都在等待 metadata.db 解锁）
        
        // 3. MetaStore 事务提交（解锁 metadata.db）
        return nil
    })
}
```

**时间线**：
```
时间轴：
T1: MetaStore 事务开始（锁定 metadata.db）
T2: 执行 unmount（阻塞 5 分钟！）
T3: MetaStore 事务提交（解锁 metadata.db）

问题：
- T1-T3 期间，metadata.db 被锁定
- 所有其他操作（Stat, Prepare, Remove 等）都在等待
- 导致级联阻塞和死锁
```

---

## 六、双数据库的优势

### 6.1 解耦设备操作和 Snapshot 操作

**优势 1：设备操作不阻塞 Snapshot 操作**

```go
// 场景：同时执行两个操作

// Goroutine 1：删除设备（慢速操作）
go func() {
    pool.RemoveDevice(ctx, "device-1")  // 使用 PoolMetadata，锁定 containerd-pool.db
    // 可能需要 10 秒
}()

// Goroutine 2：查询 snapshot（快速操作）
go func() {
    snapshotter.Stat(ctx, "my-snapshot")  // 使用 MetaStore，锁定 metadata.db
    // 几毫秒完成
}()

// ✅ 两个操作可以并行执行，互不干扰
```

**优势 2：设备操作失败不影响 Snapshot 元数据**

```go
// 场景：设备删除失败，但 snapshot 元数据已删除

// 1. 删除 snapshot 元数据（成功）
storage.Remove(ctx, key)  // MetaStore 事务成功

// 2. 删除设备（失败，但可以重试）
pool.RemoveDevice(ctx, deviceName)  // PoolMetadata 事务失败
// 设备状态标记为 "Removed"，等待 Cleanup 重试

// ✅ Snapshot 元数据已删除（用户看不到这个 snapshot）
// ✅ 设备删除可以在后台重试（不影响其他操作）
```

### 6.2 独立的事务策略

**MetaStore 事务**：
- 快速操作（读写元数据）
- 必须原子性（要么全部成功，要么全部失败）
- 与 containerd 其他组件共享

**PoolMetadata 事务**：
- 设备状态更新
- 可以容忍部分失败（标记状态后重试）
- devmapper 专用，不影响其他组件

### 6.3 更好的错误恢复

**场景：进程崩溃后的恢复**

```go
// 启动时检查两个数据库的一致性
func (s *Snapshotter) ensureDeviceStates(ctx context.Context) error {
    // 1. 从 PoolMetadata 读取所有设备
    pool.WalkDevices(ctx, func(info *DeviceInfo) error {
        // 检查设备状态
        if info.State == Creating {
            // 中间状态 = 之前的操作被中断
            markAsFaulty(info)
        }
        return nil
    })
    
    // 2. 从 MetaStore 读取所有 snapshot
    store.Walk(ctx, func(ctx context.Context, info snapshots.Info) error {
        // 检查对应的设备是否存在
        deviceName := getDeviceName(info.ID)
        if !pool.DeviceExists(deviceName) {
            // snapshot 存在但设备不存在 = 数据不一致
            log.Warn("数据不一致：snapshot 存在但设备不存在")
        }
        return nil
    })
}
```

---

## 七、与 Devbox 单数据库架构对比

### 7.1 Devbox 当前架构

```
/var/lib/containerd/devbox/
└── metadata.db          # 唯一的数据库
    ├── snapshots/       # snapshot 元数据
    └── lvm_contents/    # LVM 元数据（在同一数据库中）
```

**问题**：
- 所有操作共享同一个 BoltDB 锁
- LVM 操作（unmount）阻塞 snapshot 操作
- 无法解耦

### 7.2 如果 Devbox 采用双数据库架构

```
/var/lib/containerd/devbox/
├── metadata.db          # MetaStore（containerd 标准）
│   └── snapshots/       # snapshot 元数据
└── lvm-metadata.db     # LVM 专用数据库
    ├── lvs/            # LV 元数据
    │   ├── "devbox-xxx" -> {
    │   │       Name: "devbox-xxx",
    │   │       State: "Mounted",
    │   │       MountPoint: "/tmp/xxx",
    │   │       ...
    │   │   }
    └── states/          # LV 状态
```

**优势**：
- LVM 操作使用独立的数据库
- unmount 操作不阻塞 snapshot 操作
- 可以异步清理失败的 LVM 操作

**实现示例**：

```go
// 1. 创建 LVM 元数据库
type LVMMetadata struct {
    db *bolt.DB
}

func NewLVMMetadata(dbfile string) (*LVMMetadata, error) {
    db, err := bolt.Open(dbfile, 0600, nil)
    return &LVMMetadata{db: db}, nil
}

// 2. 在 Update 中使用
func (o *Snapshotter) Update(ctx context.Context, info snapshots.Info) error {
    // 在 MetaStore 事务内只标记状态
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        if value, ok := info.Labels[unmountLvm]; ok && value == "true" {
            // 只更新 MetaStore（快速）
            storage.SetUnmountedWithKey(ctx, info.Name)
            return nil
        }
        return nil
    })
    
    // 在 LVM 元数据库事务内执行 unmount（独立）
    if needUnmount {
        return o.lvmMetadata.WithTransaction(ctx, true, func(ctx context.Context) error {
            // 更新 LVM 状态
            o.lvmMetadata.MarkUnmounting(ctx, lvName)
            // 执行 unmount（可能慢，但不阻塞 MetaStore）
            return o.unmountLvm(ctx, mountPath)
        })
    }
    
    return err
}
```

---

## 八、总结

### 8.1 核心要点

1. **两个独立的 BoltDB 文件** = 两个独立的数据库实例
2. **每个数据库有独立的锁** = 互不阻塞
3. **通过 Snapshot ID 关联** = 数据一致性
4. **职责分离** = 解耦设计

### 8.2 为什么 Devmapper 可以在事务内执行设备操作？

**答案**：
- 设备操作使用 **PoolMetadata**（独立的数据库）
- 虽然 MetaStore 事务持有 `metadata.db` 的锁
- 但设备操作锁定的是 `containerd-pool.db`
- **两个锁互不干扰**
- 而且 dmsetup 操作很快（几毫秒），不会长时间占用锁

### 8.3 Devbox 可以借鉴什么？

1. **双数据库架构**：
   - 创建独立的 LVM 元数据库
   - LVM 操作使用独立数据库，不阻塞 snapshot 操作

2. **异步清理机制**：
   - 在 MetaStore 事务内只标记状态
   - 在 LVM 元数据库事务内执行慢速操作
   - 失败后可以重试

3. **状态管理**：
   - 在 LVM 元数据库中追踪 LV 状态
   - 启动时检查并修复不一致状态

---

## 九、参考资料

- **BoltDB 文档**：https://github.com/etcd-io/bbolt
- **Containerd Storage 包**：`snapshots/storage/metastore.go`
- **Devmapper Metadata**：`snapshots/devmapper/metadata.go`
- **Devmapper Snapshotter**：`snapshots/devmapper/snapshotter.go`

