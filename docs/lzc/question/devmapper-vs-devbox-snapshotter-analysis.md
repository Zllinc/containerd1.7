# Devmapper Snapshotter 与 Devbox Snapshotter 架构对比分析

## 一、核心架构差异

### 1.1 元数据存储策略

#### Devmapper Snapshotter（双数据库架构）
```
snapshots/devmapper/
├── MetaStore (metadata.db)          # containerd 的 storage.MetaStore
│   └── 存储 snapshot 元数据（key, parent, labels等）
└── PoolMetadata (poolname.db)       # devmapper 专用的设备元数据库
    └── 存储设备信息（DeviceID, State, ParentName等）
```

**关键点**：
- 使用**两个独立的 BoltDB 数据库**
- `MetaStore`：containerd 标准元数据，用于 snapshot 信息
- `PoolMetadata`：设备级元数据，用于设备管理和状态跟踪
- **两者互不干扰**，设备操作使用独立的数据库事务

#### Devbox Snapshotter（单数据库架构）
```
snapshots/devbox/
└── MetaStore (metadata.db)          # 唯一的数据库
    └── 存储所有元数据（snapshot + LVM content）
```

**关键点**：
- 使用**一个 BoltDB 数据库**
- 所有元数据（snapshot 和 LVM）都在同一个数据库中
- LVM 操作和 snapshot 操作**共享同一个事务锁**

### 1.2 块设备操作与事务的关系

#### Devmapper：块设备操作**在事务内**但速度快

查看 `snapshotter.go` 的 `Commit` 函数：

```go
// snapshots/devmapper/snapshotter.go:238-291
func (s *Snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
    return s.store.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 1. 元数据操作
        _, err = storage.CommitActive(ctx, key, name, usage, opts...)
        
        // 2. 设备操作（在事务内！）
        err = s.pool.SuspendDevice(ctx, deviceName)   // dmsetup suspend（快速）
        err = s.pool.ResumeDevice(ctx, deviceName)    // dmsetup resume（快速）
        return s.pool.DeactivateDevice(ctx, deviceName, true, false) // dmsetup remove（快速）
    })
}
```

**为什么可以在事务内执行？**
1. **dmsetup 命令非常快**：只是向内核发送消息，几乎不阻塞
2. **没有文件系统操作**：不涉及 mount/unmount
3. **有重试机制**：对 `EBUSY` 错误自动重试（pool_device.go:99-123）

#### Devbox：块设备操作**在事务内**且速度慢

查看 `devbox.go` 的 `Update` 函数：

```go
// snapshots/devbox/devbox.go (当前版本)
func (o *Snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) error {
    err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        if value, ok := info.Labels[unmountLvm]; ok && value == "true" {
            // ...
            return o.unmountLvm(ctx, mountPath) // ⚠️ unmount 在事务内！
        }
        // ...
    })
}
```

**为什么会导致问题？**
1. **unmount 操作可能卡住 5 分钟**：等待僵尸进程释放文件句柄
2. **阻塞整个数据库**：事务持有写锁，其他操作无法进行
3. **级联死锁**：其他 goroutine 等待事务释放，导致系统卡死

### 1.3 异步清理机制

#### Devmapper：支持异步删除（AsyncRemove）

查看 `snapshotter.go` 的 `Remove` 函数：

```go
// snapshots/devmapper/snapshotter.go:303-329
func (s *Snapshotter) removeDevice(ctx context.Context, key string) error {
    snapID, _, err := storage.Remove(ctx, key)
    deviceName := s.getDeviceName(snapID)
    
    if !s.config.AsyncRemove {
        // 同步删除：立即删除设备（可能失败）
        if err := s.pool.RemoveDevice(ctx, deviceName); err != nil {
            return errdefs.ErrFailedPrecondition
        }
    } else {
        // 异步删除：只标记状态，真正的删除在 Cleanup 中执行
        if err := s.pool.MarkDeviceState(ctx, deviceName, Removed); err != nil {
            return err
        }
    }
    return nil
}
```

查看 `snapshotter.go` 的 `Cleanup` 函数：

```go
// snapshots/devmapper/snapshotter.go:534-566
func (s *Snapshotter) Cleanup(ctx context.Context) error {
    if !s.config.AsyncRemove {
        return nil
    }
    
    // 1. 查询标记为 Removed 的设备（不在事务内）
    var removedDevices []*DeviceInfo
    s.pool.WalkDevices(ctx, func(info *DeviceInfo) error {
        if info.State == Removed {
            removedDevices = append(removedDevices, info)
        }
        return nil
    })
    
    // 2. 真正删除设备（不在 containerd MetaStore 事务内）
    for _, dev := range removedDevices {
        if err := s.pool.RemoveDevice(ctx, dev.Name); err != nil {
            // 记录错误但继续处理其他设备
        }
    }
    return nil
}
```

**关键优势**：
- **在事务内只标记状态**：快速完成，不阻塞
- **真正的删除在 Cleanup 中执行**：在事务外，不影响数据库
- **失败可重试**：Cleanup 周期性调用，自动重试失败的删除

#### Devbox：当前没有异步机制

查看 `devbox.go` 的 `Cleanup` 函数：

```go
// snapshots/devbox/devbox.go:494-541 (修改后的版本)
func (o *Snapshotter) cleanupDirectories(ctx context.Context) (_ []string, _ []string, err error) {
    // 1. 在事务内查询需要清理的 LV
    if err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        cleanupDirs, err = o.getCleanupDirectories(ctx)
        removedLvNames, err = o.getCleanupLvNames(ctx)
        return nil
    }); err != nil {
        return nil, nil, err
    }
    
    // 2. 在事务外执行 unmount（已修复）
    for _, lvName := range removedLvNames {
        devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
        mountPoints, err := findMountPointByDevice(devicePath)
        for _, mountPoint := range mountPoints {
            if err := o.unmountLvm(ctx, mountPoint); err != nil {
                // ⚠️ unmount 失败只记录日志，下次 Cleanup 重试
            }
        }
    }
    return cleanupDirs, removedLvNames, nil
}
```

**改进点**：
- ✅ unmount 已移到事务外
- ❌ 但没有明确的状态标记机制
- ❌ `Update` 函数中的 unmount 仍在事务内

---

## 二、设备状态管理

### 2.1 Devmapper：完整的状态机

查看 `pool_device.go` 的状态定义：

```go
// snapshots/devmapper/device_state.go
type DeviceState string

const (
    Unknown      DeviceState = ""
    Creating     DeviceState = "Creating"     // 正在创建
    Created      DeviceState = "Created"      // 已创建
    Activating   DeviceState = "Activating"   // 正在激活
    Activated    DeviceState = "Activated"    // 已激活
    Suspending   DeviceState = "Suspending"   // 正在暂停
    Suspended    DeviceState = "Suspended"    // 已暂停
    Resuming     DeviceState = "Resuming"     // 正在恢复
    Resumed      DeviceState = "Resumed"      // 已恢复
    Deactivating DeviceState = "Deactivating" // 正在停用
    Deactivated  DeviceState = "Deactivated"  // 已停用
    Removing     DeviceState = "Removing"     // 正在删除
    Removed      DeviceState = "Removed"      // 已删除
    Faulty       DeviceState = "Faulty"       // 故障
)
```

**状态转换机制**（`pool_device.go:177-213`）：

```go
func (p *PoolDevice) transition(ctx context.Context, deviceName string, 
    tryingState DeviceState, successState DeviceState, updateStateFn func() error) error {
    
    // 1. 先设置"正在尝试"状态
    p.metadata.UpdateDevice(ctx, deviceName, func(deviceInfo *DeviceInfo) error {
        deviceInfo.State = tryingState
        return nil
    })
    
    // 2. 执行真正的设备操作
    err := updateStateFn()
    
    // 3. 根据结果更新最终状态
    p.metadata.UpdateDevice(ctx, deviceName, func(deviceInfo *DeviceInfo) error {
        if err == nil {
            deviceInfo.State = successState
            deviceInfo.Error = ""
        } else {
            deviceInfo.Error = err.Error() // 记录错误信息
        }
        return nil
    })
    
    return errors.Join(result...)
}
```

**使用示例**：

```go
// 创建设备
func (p *PoolDevice) createDevice(ctx context.Context, info *DeviceInfo) error {
    return p.transition(ctx, info.Name, Creating, Created, func() error {
        return dmsetup.CreateDevice(p.poolName, info.DeviceID)
    })
}

// 删除设备
func (p *PoolDevice) deleteDevice(ctx context.Context, info *DeviceInfo) error {
    return p.transition(ctx, info.Name, Removing, Removed, func() error {
        return retry(ctx, func() error {
            return dmsetup.DeleteDevice(p.poolName, info.DeviceID)
        })
    })
}
```

**优势**：
1. **清晰的状态追踪**：可以知道设备处于哪个阶段
2. **错误信息持久化**：失败时保存错误详情
3. **故障恢复**：启动时通过 `ensureDeviceStates` 检查并修复不一致状态

### 2.2 Devbox：简单的状态管理

当前 devbox snapshotter 使用 containerd 的 `storage` 包，状态管理相对简单：

```go
// 快照状态
type Kind int

const (
    KindUnknown Kind = iota
    KindView         // 只读视图
    KindActive       // 活动快照
    KindCommitted    // 已提交
)
```

**不足**：
- ❌ 没有中间状态（如"正在创建"、"正在删除"）
- ❌ 没有错误信息持久化
- ❌ 无法区分"删除失败"和"正在删除"

---

## 三、错误恢复机制

### 3.1 Devmapper：启动时自动恢复

查看 `pool_device.go:130-172` 的 `ensureDeviceStates` 函数：

```go
func (p *PoolDevice) ensureDeviceStates(ctx context.Context) error {
    var faultyDevices []*DeviceInfo
    var activatedDevices []*DeviceInfo
    
    // 遍历所有设备
    p.WalkDevices(ctx, func(info *DeviceInfo) error {
        switch info.State {
        case Suspended, Resumed, Deactivated, Removed, Faulty:
            // 这些是正常的最终状态
        case Activated:
            // 检查设备是否真的处于激活状态
            activatedDevices = append(activatedDevices, info)
        default:
            // 其他状态（Creating, Activating, Removing等）都是中间状态
            // 表示之前的操作可能被中断了，标记为 Faulty
            faultyDevices = append(faultyDevices, info)
        }
        return nil
    })
    
    // 重新激活标记为 Activated 但实际未激活的设备
    for _, dev := range activatedDevices {
        if !p.IsActivated(dev.Name) {
            log.Warn("设备标记为 Activated 但未激活，重新激活")
            p.activateDevice(ctx, dev)
        }
    }
    
    // 将处于中间状态的设备标记为 Faulty
    for _, dev := range faultyDevices {
        log.Warn("设备处于中间状态，标记为 Faulty")
        p.metadata.MarkFaulty(ctx, dev.Name)
    }
    
    return nil
}
```

**关键功能**：
1. **检测中断的操作**：如果设备处于"Creating"、"Activating"等中间状态，说明之前的操作被中断
2. **自动修复**：重新激活应该激活但未激活的设备
3. **标记故障设备**：将无法恢复的设备标记为 Faulty

### 3.2 Devbox：缺少恢复机制

当前 devbox snapshotter：
- ❌ 没有启动时的状态检查
- ❌ 僵尸 LV 会一直存在并重复扫描
- ❌ unmount 失败后没有明确的重试机制

---

## 四、重试机制

### 4.1 Devmapper：内置重试

查看 `pool_device.go:99-123` 的 `retry` 函数：

```go
func retry(ctx context.Context, f func() error) error {
    var (
        maxRetries = 100
        retryDelay = 100 * time.Millisecond
        retryErr   error
    )
    
    for attempt := 1; attempt <= maxRetries; attempt++ {
        retryErr = f()
        
        // 如果不是 EBUSY 错误，不重试
        if retryErr == nil || !errors.Is(retryErr, unix.EBUSY) {
            return retryErr
        }
        
        // 每 10 次打印一次日志，避免刷屏
        if attempt%10 == 0 {
            log.Warnf("retrying... (%d of %d)", attempt, maxRetries)
        }
        
        time.Sleep(retryDelay)
    }
    
    return retryErr
}
```

**应用场景**：
- 设备删除时的 `EBUSY` 错误
- 设备停用时的资源竞争

### 4.2 Devbox：全局锁串行化

当前 devbox snapshotter 使用全局锁（`lvm/lvm.go`）：

```go
var lvmLock sync.Mutex

func CreateVolume(ctx context.Context, vol *apis.LVMVolume) error {
    lvmLock.Lock()
    defer lvmLock.Unlock()
    // ... lvcreate 操作
}

func UnmountVolume(mountPath string) error {
    lvmLock.Lock()
    defer lvmLock.Unlock()
    // ... unmount 操作
}
```

**问题**：
- ❌ 锁粒度太粗，所有 LVM 操作都串行化
- ❌ unmount 卡住时会阻塞所有其他 LVM 操作
- ✅ 但避免了并发冲突

---

## 五、配置选项对比

### 5.1 Devmapper 配置

```toml
[plugins."io.containerd.snapshotter.v1.devmapper"]
  root_path = "/var/lib/containerd/devmapper"
  pool_name = "containerd-pool"
  base_image_size = "8192MB"
  async_remove = true                    # ⭐ 异步删除选项
  discard_blocks = true                  # 删除时回收磁盘空间
  fs_type = "ext4"                       # 文件系统类型
  fs_options = "nodiscard,lazy_itable_init=0"
```

### 5.2 Devbox 配置

```toml
[plugins."io.containerd.snapshotter.v1.devbox"]
  root_path = "/var/lib/containerd/devbox"
  lvm_vg_name = "devbox-vg"
  thin_pool_name = "devbox-vg-thinpool"
  # ❌ 缺少 async_remove 选项
  # ❌ 缺少错误恢复选项
```

---

## 六、核心问题总结

### 6.1 为什么 Devmapper 可以在事务内执行块设备操作？

1. **dmsetup 命令快速**：
   - `dmsetup create`、`dmsetup remove` 只是向内核发送消息
   - 通常在几毫秒内完成
   - 不涉及文件系统操作

2. **mkfs 在事务内但有完善的回滚**：
   ```go
   if err := mkfs(...); err != nil {
       // 立即删除设备并回滚
       return errors.Join(err, s.pool.RemoveDevice(ctx, deviceName))
   }
   ```

3. **提供异步删除选项**：
   - 慢速的删除操作可以异步化
   - 在 `Cleanup` 中执行，不阻塞事务

4. **独立的设备元数据库**：
   - 设备操作使用 `PoolMetadata` 数据库
   - 不与 containerd 的 `MetaStore` 共享锁

### 6.2 为什么 Devbox 会出现死锁问题？

1. **unmount 操作太慢**：
   - LVM unmount 可能等待僵尸进程（5 分钟超时）
   - 文件系统 umount 比 dmsetup remove 慢得多

2. **unmount 在事务内执行**：
   - 事务持有写锁
   - 其他操作无法进行
   - 导致级联阻塞

3. **单一数据库架构**：
   - 所有元数据在一个数据库中
   - LVM 操作和 snapshot 操作共享同一个锁

4. **缺少异步机制**：
   - 没有"标记后删除"的模式
   - 必须立即完成操作

---

## 七、可借鉴的设计要点

### 7.1 立即可应用的改进

#### 1. 异步清理机制（类似 AsyncRemove）

**实现方案**：

```go
// 1. 在 Update 中只标记状态，不真正 unmount
func (o *Snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) error {
    err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        if value, ok := info.Labels[unmountLvm]; ok && value == "true" {
            // ✅ 只标记为"待卸载"，不真正执行 unmount
            _, err := storage.SetUnmountedWithKey(ctx, info.Name)
            return err
        }
        // ...
    })
}

// 2. 在 Cleanup 中真正执行 unmount（已实现）
func (o *Snapshotter) cleanupDirectories(ctx context.Context) {
    // 在事务内查询需要清理的 LV
    // 在事务外执行 unmount
}
```

#### 2. 完善的状态管理

**建议添加的状态**：

```go
type LVState string

const (
    LVStateUnknown      LVState = ""
    LVStateCreating     LVState = "creating"     // 正在创建
    LVStateActive       LVState = "active"       // 活动中
    LVStateMounted      LVState = "mounted"      // 已挂载
    LVStateUnmounting   LVState = "unmounting"   // 正在卸载
    LVStateUnmounted    LVState = "unmounted"    // 已卸载
    LVStateRemoving     LVState = "removing"     // 正在删除
    LVStateRemoved      LVState = "removed"      // 已删除（待清理）
    LVStateFaulty       LVState = "faulty"       // 故障（僵尸 LV）
)
```

#### 3. 启动时的状态检查

```go
func (o *Snapshotter) ensureLVStates(ctx context.Context) error {
    // 扫描所有 LV
    lvList, _ := lvm.ListVolumes(ctx, o.lvmVgName)
    
    for _, lv := range lvList {
        // 检查是否是僵尸 LV（元数据存在但设备节点不存在）
        if !deviceExists(lv.Name) {
            log.Warn("检测到僵尸 LV，标记为 Faulty")
            markLVAsFaulty(lv.Name)
            continue
        }
        
        // 检查 mount 状态是否与元数据一致
        // ...
    }
}
```

### 7.2 架构级改进（需要更大改动）

#### 1. 双数据库架构

**优点**：
- LVM 操作使用独立的数据库
- 不与 containerd 事务冲突
- 可以有独立的锁策略

**缺点**：
- 增加复杂度
- 需要处理两个数据库的一致性

#### 2. 任务队列模式（之前提到的方案）

```go
type LVMTaskQueue struct {
    tasks chan LVMTask
}

func (q *LVMTaskQueue) SubmitUnmount(lvName string) error {
    q.tasks <- LVMTask{Type: "unmount", LVName: lvName}
    return nil // 立即返回
}

func (q *LVMTaskQueue) worker() {
    for task := range q.tasks {
        // 在后台执行 unmount
        unmountLVM(task.LVName)
    }
}
```

---

## 八、最佳实践总结

### 8.1 事务内操作的黄金法则

**✅ 应该在事务内执行**：
- 快速的元数据操作（读写数据库）
- 快速的设备命令（如 `dmsetup` 消息）
- 状态标记操作

**❌ 不应该在事务内执行**：
- 慢速的文件系统操作（mount/unmount）
- 可能长时间阻塞的系统调用
- 外部进程调用（除非保证快速返回）

### 8.2 异步处理的适用场景

**适合异步处理的操作**：
1. **删除操作**：失败后可以重试
2. **清理操作**：不影响正常功能
3. **状态同步**：可以延迟执行

**不适合异步处理的操作**：
1. **创建操作**：需要立即知道结果
2. **激活操作**：后续操作依赖
3. **关键路径**：影响用户体验

### 8.3 状态管理的关键要素

1. **中间状态**：区分"正在执行"和"已完成"
2. **错误信息**：持久化错误详情，便于调试
3. **故障标记**：区分"失败"和"可重试"
4. **启动检查**：验证状态一致性并修复

### 8.4 错误处理策略

1. **立即失败 vs 延迟重试**：
   - 创建操作：立即失败并回滚
   - 删除操作：标记后延迟重试

2. **部分失败处理**：
   - devmapper：删除失败返回 `ErrFailedPrecondition`，允许 GC 继续
   - 避免一个失败导致整体停止

3. **错误分类**：
   - 临时错误（EBUSY）：自动重试
   - 永久错误（设备不存在）：标记为 Faulty
   - 致命错误：返回并停止

---

## 九、结论与建议

### 9.1 核心发现

**Devmapper Snapshotter 成功的关键**：
1. ✅ 双数据库架构，设备操作独立
2. ✅ dmsetup 命令快速，不阻塞事务
3. ✅ 提供异步删除选项
4. ✅ 完善的状态管理和错误恢复
5. ✅ 内置重试机制

**Devbox Snapshotter 的瓶颈**：
1. ❌ unmount 操作慢且在事务内
2. ❌ 单一数据库，共享事务锁
3. ❌ 缺少异步清理机制
4. ❌ 状态管理简单，无法追踪中间状态
5. ❌ 僵尸 LV 没有自动恢复

### 9.2 优先改进建议

#### 短期（已完成或易于实现）
1. ✅ **将 unmount 移到事务外**（已在 `cleanupDirectories` 中完成）
2. ⚠️ **将 `Update` 中的 unmount 改为异步**（待实现）
3. ⚠️ **添加启动时的状态检查**（检测并标记僵尸 LV）

#### 中期（需要一定工作量）
1. **完善状态管理**：添加中间状态和错误信息持久化
2. **实现异步清理**：类似 `AsyncRemove` 的机制
3. **优化锁策略**：考虑细粒度锁或读写锁

#### 长期（架构级改动）
1. **双数据库架构**：分离设备元数据
2. **任务队列模式**：完全解耦慢速操作
3. **外部 LVM 服务**：高度隔离（如果性能要求很高）

### 9.3 立即行动项

1. **修复 `Update` 函数中的 unmount 事务问题**：
   ```go
   // 当前：unmount 在事务内
   // 改为：只标记状态，在 Cleanup 中执行
   ```

2. **添加僵尸 LV 检测**：
   ```go
   func (o *Snapshotter) detectZombieLVs() {
       // 扫描 lvs 输出
       // 检查设备节点是否存在
       // 标记僵尸 LV 以避免重复扫描
   }
   ```

3. **改进日志**：
   ```go
   // 记录状态转换
   log.Infof("LV %s: %s -> %s", lvName, oldState, newState)
   ```

---

## 十、参考资料

- **Devmapper Snapshotter 源码**：`snapshots/devmapper/`
- **Containerd Issue #4234**：设备暂停/恢复的竞态条件
- **Device Mapper 文档**：
  - [Thin Provisioning](https://www.kernel.org/doc/Documentation/device-mapper/thin-provisioning.txt)
  - [Snapshot Support](https://www.kernel.org/doc/Documentation/device-mapper/snapshot.txt)
- **Devbox Snapshotter 问题文档**：`docs/lzc/question/unmount-lvm-lock-integration.md`

