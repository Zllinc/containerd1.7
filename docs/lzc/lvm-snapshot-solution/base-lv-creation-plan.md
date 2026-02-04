# 基准 LV 创建方案

**创建时间**: 2026-01-22  
**目的**: 详细说明如何创建和管理"只读层最上层的基准 LV"

---

## 一、方案概述

### 我们要实现的架构

```
只读层最上层（OverlayFS 合并视图 / merged view）
    ↓ 拷贝到
基准 LV (base-lv)
    ↓ 用于 diff 对比
可写层快照 LV (writable-snapshot)
```

### 关键问题

**Q: 基准 LV 从哪里来？**
A: Prepare 阶段创建，从只读层最上层拷贝数据

> 注意：当前 `devbox` snapshotter 的 `upperPath(id)` 是该 snapshot 的 `upperdir`（增量内容目录），**不是**多层的合并视图。
> 如果要拿到“只读层最上层的合并视图”，需要通过 snapshotter 返回的 `mounts`（overlay/bind）进行一次临时挂载后，从临时挂载点读取。

**Q: 什么时候创建？**
A: 第一次为该镜像创建容器时（懒加载）

**Q: 数据怎么进去？**
A: 挂载只读层 → 创建基准 LV → 拷贝数据 → 卸载

---

## 二、详细实现方案

### 2.1 基准 LV 的生命周期

```
┌────────────────────────────────────────────┐
│ containerd pull 镜像                       │
│ - 只读层被 unpack 到 snapshots/{id}/fs/   │
└────────────────────────────────────────────┘
              ↓
┌────────────────────────────────────────────┐
│ 第一次 Prepare（创建 active snapshot）     │
│ - 检查：base-lv 是否存在？                 │
│   - 如果不存在：创建 base-lv               │
│   - 如果存在：复用 base-lv                 │
└────────────────────────────────────────────┘
              ↓
┌────────────────────────────────────────────┐
│ 创建可写层 LV（空）                         │
│ - 不拷贝只读层数据                         │
│ - 用户直接在空 LV 上写入                   │
└────────────────────────────────────────────┘
              ↓
┌────────────────────────────────────────────┐
│ Commit 时                                   │
│ - 创建可写层快照                           │
│ - thin_send: base-lv vs writable-snapshot │
└────────────────────────────────────────────┘
```

### 2.2 Prepare 阶段的改造

#### 当前代码流程（devbox.go）

```go
func (o *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
    // 1. 创建可写层 LV
    td, lvName, err = o.prepareLvmDirectory(ctx, snapshotDir, contentID, useLimit)
    
    // 2. 如果是 private image，拷贝 parent 的 upperdir
    if privateImageOk {
        parentUpperdir := o.upperPath(parentID)
        cp.Copy(parentUpperdir, filepath.Join(td, "fs"), opt)
    }
    
    // 3. 挂载可写层 LV
    o.mountLvm(ctx, lvName, path)
    
    return o.mounts(s), nil
}
```

#### 改造后的流程

```go
func (o *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
    // 1. 获取或创建基准 LV（新增）
    baseLVName, err := o.getOrCreateBaseLV(ctx, parent, contentID)
    if err != nil {
        return nil, err
    }
    
    // 2. 创建可写层 LV（空，不拷贝数据）
    td, lvName, err = o.prepareLvmDirectory(ctx, snapshotDir, contentID, useLimit)
    
    // 3. 保存基准 LV 名称到 metadata
    storage.SetBaseLVName(ctx, contentID, baseLVName)
    
    // 4. 挂载可写层 LV
    o.mountLvm(ctx, lvName, path)
    
    return o.mounts(s), nil
}
```

### 2.3 基准 LV 创建函数

```go
// getOrCreateBaseLV 获取或创建基准 LV
// 基准 LV 用于保存只读层最上层的数据，作为 commit diff 的基准
func (o *Snapshotter) getOrCreateBaseLV(ctx context.Context, parentKey, contentID string) (string, error) {
    // 基准 LV 的命名：devbox-base-{contentID}
    baseLVName := fmt.Sprintf("devbox-base-%s", contentID)
    
    // 1. 检查基准 LV 是否已存在
    exists, err := lvm.CheckVolumeExists(ctx, &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{
            Name: baseLVName,
        },
        Spec: apis.VolumeInfo{
            VolGroup: o.lvmVgName,
        },
    })
    if err != nil {
        return "", fmt.Errorf("failed to check base LV existence: %w", err)
    }
    
    if exists {
        // 基准 LV 已存在，直接复用
        log.G(ctx).Infof("Reusing existing base LV: %s", baseLVName)
        return baseLVName, nil
    }
    
    // 2. 基准 LV 不存在，需要创建
    log.G(ctx).Infof("Creating base LV: %s", baseLVName)
    
    // 3. 获取只读层最上层的路径
    parentID, err := storage.GetID(ctx, parentKey)
    if err != nil {
        return "", fmt.Errorf("failed to get parent ID: %w", err)
    }
    
    // 只读层最上层（OverlayFS 已合并的视图）
    readonlyTopPath := o.upperPath(parentID)
    
    // 4. 计算只读层的大小（用于确定 LV 大小）
    readonlySize, err := o.calculateDirSize(readonlyTopPath)
    if err != nil {
        return "", fmt.Errorf("failed to calculate readonly layer size: %w", err)
    }
    
    // 留 20% 余量（文件系统元数据、对齐等）
    baseLVSize := uint64(float64(readonlySize) * 1.2)
    
    // 5. 创建基准 LV
    baseVol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{
            Name: baseLVName,
        },
        Spec: apis.VolumeInfo{
            Capacity:      fmt.Sprintf("%d", baseLVSize),
            VolGroup:      o.lvmVgName,
            ThinProvision: o.ThinPoolName,
        },
    }
    
    if err := lvm.CreateVolume(ctx, baseVol); err != nil {
        return "", fmt.Errorf("failed to create base LV: %w", err)
    }
    
    // 6. 格式化基准 LV
    if err := o.mkfs(baseLVName); err != nil {
        lvm.DestroyVolume(ctx, baseVol) // 清理
        return "", fmt.Errorf("failed to format base LV: %w", err)
    }
    
    // 7. 挂载基准 LV
    baseMountPoint := filepath.Join(o.root, "base-mounts", baseLVName)
    os.MkdirAll(baseMountPoint, 0755)
    
    if err := o.mountLvm(ctx, baseLVName, baseMountPoint); err != nil {
        lvm.DestroyVolume(ctx, baseVol) // 清理
        return "", fmt.Errorf("failed to mount base LV: %w", err)
    }
    
    // 8. 拷贝只读层最上层到基准 LV
    log.G(ctx).Infof("Copying readonly layer to base LV: %s → %s", readonlyTopPath, baseMountPoint)
    
    opt := cp.Options{
        OnSymlink: func(src string) cp.SymlinkAction {
            return cp.Shallow
        },
        PreserveTimes: true,
        PreserveOwner: true,
    }
    
    if err := cp.Copy(readonlyTopPath, baseMountPoint, opt); err != nil {
        o.unmountLvm(ctx, baseMountPoint) // 清理
        lvm.DestroyVolume(ctx, baseVol)   // 清理
        return "", fmt.Errorf("failed to copy readonly layer to base LV: %w", err)
    }
    
    // 9. 卸载基准 LV（保持只读状态）
    if err := o.unmountLvm(ctx, baseMountPoint); err != nil {
        log.G(ctx).WithError(err).Warn("Failed to unmount base LV, but continuing")
    }
    
    log.G(ctx).Infof("Base LV created successfully: %s", baseLVName)
    return baseLVName, nil
}
```

### 2.4 辅助函数

```go
// calculateDirSize 计算目录的总大小
func (o *Snapshotter) calculateDirSize(dir string) (uint64, error) {
    var size uint64
    
    err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }
        if !info.IsDir() {
            size += uint64(info.Size())
        }
        return nil
    })
    
    return size, err
}
```

---

## 三、数据流详解

### 3.1 第一次创建容器

```
Step 1: containerd pull 镜像
┌─────────────────────────────────────┐
│ 只读层 unpack 到 snapshots/         │
│ - snapshots/layer1/fs/              │
│ - snapshots/layer2/fs/              │
│ - snapshots/layer3/fs/ (最上层)     │
└─────────────────────────────────────┘

Step 2: Prepare 阶段
┌─────────────────────────────────────┐
│ 检查基准 LV 是否存在                │
│ → 不存在                            │
└─────────────────────────────────────┘
         ↓
┌─────────────────────────────────────┐
│ 创建基准 LV                         │
│ 1. lvcreate -V 5G -T vg/thinpool    │
│    -n devbox-base-xxx               │
│ 2. mkfs.ext4 /dev/vg/devbox-base-xxx│
│ 3. mount /dev/vg/devbox-base-xxx    │
│    /base-mounts/devbox-base-xxx     │
└─────────────────────────────────────┘
         ↓
┌─────────────────────────────────────┐
│ 拷贝只读层最上层到基准 LV           │
│ cp -a snapshots/layer3/fs/*         │
│       /base-mounts/devbox-base-xxx/ │
└─────────────────────────────────────┘
         ↓
┌─────────────────────────────────────┐
│ 卸载基准 LV                         │
│ umount /base-mounts/devbox-base-xxx │
│ → 基准 LV 保持只读，不再挂载        │
└─────────────────────────────────────┘
         ↓
┌─────────────────────────────────────┐
│ 创建可写层 LV（空）                 │
│ lvcreate -V 10G -T vg/thinpool      │
│    -n devbox-xxx                    │
│ → 不拷贝只读层数据                  │
└─────────────────────────────────────┘
```

### 3.2 第二次创建容器（同一镜像）

```
Step 1: Prepare 阶段
┌─────────────────────────────────────┐
│ 检查基准 LV 是否存在                │
│ → 存在：devbox-base-xxx             │
│ → 直接复用，跳过创建步骤            │
└─────────────────────────────────────┘
         ↓
┌─────────────────────────────────────┐
│ 创建新的可写层 LV（空）             │
│ lvcreate -V 10G -T vg/thinpool      │
│    -n devbox-yyy                    │
└─────────────────────────────────────┘

结果：
- 基准 LV 被多个容器共享
- 每个容器有自己的可写层 LV
- 节省存储空间和创建时间
```

---

## 四、Commit 流程

### 4.1 Commit 时使用基准 LV

```go
func (o *Snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
    // 1. 获取 active snapshot 的信息
    id, err := storage.GetID(ctx, key)
    if err != nil {
        return err
    }
    
    // 2. 获取可写层 LV 名称
    lvName, err := storage.GetDevboxLvName(ctx, contentID, "")
    if err != nil {
        return err
    }
    
    // 3. 创建可写层 LV 的快照
    snapshotPath, err := lvm.CreateDevboxLVSnapshot(ctx, o.lvmVgName, lvName)
    if err != nil {
        return err
    }
    defer lvm.DestroyDevboxLVSnapshot(ctx, o.lvmVgName, lvName)
    
    // 4. 获取基准 LV 名称
    baseLVName, err := storage.GetBaseLVName(ctx, contentID)
    if err != nil {
        return fmt.Errorf("base LV not found: %w", err)
    }
    
    // 5. 使用 thin_send 计算差异
    baseLV := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, baseLVName)
    snapshotLV := fmt.Sprintf("/dev/%s", snapshotPath)
    
    diffStream := "/tmp/thin-send-" + contentID + ".stream"
    err = lvm.ThinSendToFile(ctx, baseLV, snapshotLV, diffStream)
    if err != nil {
        return fmt.Errorf("thin_send failed: %w", err)
    }
    
    // 6. 后续处理：解析 diff、打包镜像层
    // TODO: 实现
    
    return nil
}
```

---

## 五、基准 LV 的管理

### 5.1 命名规范

```
基准 LV 名称: devbox-base-{contentID}
可写层 LV 名称: devbox-{contentID}
快照 LV 名称: devbox-{contentID}-snapshot
```

### 5.2 生命周期管理

**创建**：
- 第一次为该镜像创建容器时
- Prepare 阶段自动创建

**复用**：
- 同一镜像的所有容器共享基准 LV
- 通过 contentID 关联

**删除**：
```go
// 删除基准 LV 的时机：
// 1. 镜像被删除时
// 2. 所有使用该镜像的容器都被删除后
// 3. 手动清理

func (o *Snapshotter) RemoveBaseLV(ctx context.Context, contentID string) error {
    baseLVName := fmt.Sprintf("devbox-base-%s", contentID)
    
    // 检查是否还有容器在使用
    inUse, err := storage.IsBaseLVInUse(ctx, baseLVName)
    if err != nil {
        return err
    }
    
    if inUse {
        log.G(ctx).Infof("Base LV %s is still in use, skipping removal", baseLVName)
        return nil
    }
    
    // 删除基准 LV
    baseVol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{
            Name: baseLVName,
        },
        Spec: apis.VolumeInfo{
            VolGroup: o.lvmVgName,
        },
    }
    
    return lvm.DestroyVolume(ctx, baseVol)
}
```

### 5.3 元数据管理

```go
// 在 boltDB 中存储基准 LV 的信息

// 创建时保存
func SetBaseLVName(ctx context.Context, contentID, baseLVName string) error {
    // 保存 contentID → baseLVName 映射
}

// 查询时获取
func GetBaseLVName(ctx context.Context, contentID string) (string, error) {
    // 获取 contentID 对应的 baseLVName
}

// 引用计数
func IncrementBaseLVRefCount(ctx context.Context, baseLVName string) error {
    // 增加引用计数
}

func DecrementBaseLVRefCount(ctx context.Context, baseLVName string) error {
    // 减少引用计数
    // 如果引用计数为 0，标记为可删除
}
```

---

## 六、优势与考虑

### 6.1 优势

**1. 真正的增量 commit**
```
thin_send 输出 = 可写层快照 - 基准 LV
                = 用户写入的数据
                = 真正的增量
```

**2. 基准 LV 可复用**
- 同一镜像的所有容器共享
- 只创建一次
- 节省存储和时间

**3. 可写层为空**
- 不拷贝只读层数据
- Prepare 速度快
- OverlayFS 自动 COW

### 6.2 需要考虑的问题

**1. 存储开销**
- 每个镜像一个基准 LV
- 大小 = 只读层最上层的大小
- 需要 thin pool 有足够空间

**2. 创建时间**
- 第一次创建容器时需要拷贝数据
- 可能需要几秒到几十秒
- 后续创建复用，很快

**3. 基准 LV 的清理**
- 需要引用计数管理
- 避免删除正在使用的基准 LV
- 定期清理未使用的基准 LV

---

## 七、总结

### 基准 LV 的来源

**答案**：
1. **创建时机**：Prepare 阶段（第一次为该镜像创建容器时）
2. **数据来源**：从只读层最上层（OverlayFS 合并后）拷贝
3. **创建流程**：创建 LV → 格式化 → 挂载 → 拷贝数据 → 卸载
4. **复用机制**：同一镜像的所有容器共享同一个基准 LV

### 数据流总结

```
只读层最上层（snapshots/layer3/fs/）
    ↓ cp -a（第一次）
基准 LV（devbox-base-xxx）
    ↓ thin_send（commit 时）
可写层快照（devbox-xxx-snapshot）
    ↓ 解析、打包
容器镜像层（layer.tar）
```

---

**文档版本**: 1.0.0  
**最后更新**: 2026-01-22

