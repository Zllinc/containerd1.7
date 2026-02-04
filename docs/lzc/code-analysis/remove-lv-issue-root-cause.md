# Devbox Snapshotter 删除流程完整分析

## 一、日志分析

### 1.1 问题日志解读

```
案例 1:
  key=k8s.io/89381/34af6d227eb9a9fbb386d81fe0325c1906d491895c7b2ede40574e84a9eec328
  contentID=52361581-098f-4918-836a-ccced16afe21
  mountPath=/var/lib/containerd/io.containerd.snapshotter.v1.devbox/snapshots/69847
  ✅ contentID 不为空
  ✅ mountPath 不为空

案例 2:
  key=k8s.io/89383/91af4656aec49b97ee186d5b8e11df61c26cd7ebf3c228a2ff691cbd084bbb9b
  contentID=52361581-098f-4918-836a-ccced16afe21 (同一个 contentID！)
  mountPath=/var/lib/containerd/io.containerd.snapshotter.v1.devbox/snapshots/69849
  ✅ contentID 不为空
  ✅ mountPath 不为空

错误日志:
  time="2026-01-18T03:03:51.046059214+08:00" level=error
  msg="Failed to update devbox snapshot: failed to set devbox content status to unmounted:
       snapshot key for content ID 52361581-098f-4918-836a-ccced16afe21 is not set,
       cannot set status to unmounted: not found"

关键发现：**同一个 contentID 被两个不同的容器使用（不同的 snapshot key，不同的 mountPath）**
```

---

## 二、关键代码流程分析

### 2.1 容器停止流程的两种路径

#### 路径 1：StopContainer（主动停止）

**调用链路**：
```
StopContainer (container_stop.go:40)
  ↓
stopContainer() (container_stop.go:100)
  ↓
stopContainer() (container_stop.go:57)
  ↓
UpdateDevboxSnapshot(unmountLvm, "true") (container_stop.go:90)
  ↓
snapshotter.Update() (devbox.go:237-270)
  ↓
SetUnmountedWithKey() → unmountLvm() ✅ 执行卸载
```

**代码位置**：`container_stop.go:86-94`
```go
snapshotter := c.runtimeSnapshotter(ctx, ociRuntime)
log.G(ctx).Infof("Check snapshotter: %s", snapshotter)

// Check if the snapshotter is devbox and update the devbox snapshot
if snapshotter == "devbox" {
    err = c.client.UpdateDevboxSnapshot(ctx, snapshotter, i.ID, unmountLvm, "true")
    if err != nil {
        log.G(ctx).WithError(err).Errorf("Failed to update devbox snapshot: %s", err)
    }
}
```

#### 路径 2：handleContainerExit（容器退出）

**调用链路**：
```
容器退出 → TaskExit 事件
  ↓
handleContainerExit (events.go:367-474)
  ↓
UpdateDevboxSnapshot(unmountLvm, "true") (events.go:452)
  ↓
snapshotter.Update() (devbox.go:237-270)
  ↓
SetUnmountedWithKey() → unmountLvm() ✅ 执行卸载
```

**代码位置**：`events.go:446-456`
```go
container, err := c.client.ContainerService().Get(ctx, cntr.Container.ID())
if err != nil {
    return status, err
}
fmt.Println("Container snapshotter:", container.Snapshotter, "ID:", cntr.Container.ID())
if container.Snapshotter == "devbox" {
    err = c.client.UpdateDevboxSnapshot(ctx, container.Snapshotter, container.ID, unmountLvm, "true")
    if err != nil {
        logrus.WithError(err).Errorf("Failed to update devbox snapshot for container %s", cntr.Container.ID())
    }
}
```

---

### 2.2 Update 函数的逻辑（devbox.go:237-270）

```go
func (o *Snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (newInfo snapshots.Info, err error) {
    err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 检查是否是卸载请求
        if value, ok := info.Labels[unmountLvm]; ok && value == "true" {
            mountPath, err := storage.SetUnmountedWithKey(ctx, info.Name)
            if err != nil {
                return fmt.Errorf("failed to set devbox content status to unmounted: %w", err)
            }
            return o.unmountLvm(ctx, mountPath)  // ✅ 执行卸载
        }

        // 检查是否是移除 contentID 的请求
        if value, ok := info.Labels[removeContentIDKey]; ok {
            return storage.SetDevboxContentStatusRemoved(ctx, value)
        }

        // 其他 label 的处理...
        newInfo, err = storage.UpdateInfo(ctx, info, fieldpaths...)
        // ...
    })
    return newInfo, err
}
```

---

### 2.3 SetUnmountedWithKey 的逻辑（bolt.go:823-859）

```go
func SetUnmountedWithKey(ctx context.Context, key string) (string, error) {
    var mountPath string

    err := withDevboxBucket(ctx, func(ctx context.Context, bkt *bolt.Bucket, dbkt *bolt.Bucket) error {
        // 1. 获取 snapshot bucket
        sbkt := bkt.Bucket([]byte(key))
        if sbkt == nil {
            return fmt.Errorf("devbox storage path bucket for key %s does not exist: %w", key, errdefs.ErrNotFound)
        }

        // 2. 获取 mountPath
        mountPath = string(sbkt.Get(DevboxKeyPath))
        if mountPath == "" {
            return fmt.Errorf("mount path for key %s is empty: %w", key, errdefs.ErrNotFound)
        }

        // 3. 获取 contentID
        contentID := sbkt.Get(DevboxKeyContentID)
        if len(contentID) == 0 {
            return fmt.Errorf("content ID for key %s is empty: %w", key, errdefs.ErrNotFound)
        }

        // 4. 获取 content bucket
        sdbkt := dbkt.Bucket([]byte(contentID))
        if sdbkt == nil {
            return fmt.Errorf("devbox storage path bucket for content ID %s does not exist: %w", string(contentID), errdefs.ErrNotFound)
        }

        // 5. ⚠️ 关键检查：是否存在 snapshotKey
        if snapshotKey := sdbkt.Get(DevboxKeySnapshotKey); snapshotKey != nil {
            // 删除 snapshotKey 关联
            if err := sdbkt.Delete(DevboxKeySnapshotKey); err != nil {
                return fmt.Errorf("failed to delete snapshot key for content ID %s: %w", string(contentID), err)
            }
            return nil  // ✅ 成功删除 snapshotKey
        }

        // ⚠️ 如果没有 snapshotKey，返回错误！
        return fmt.Errorf("snapshot key for content ID %s is not set, cannot set status to unmounted: %w", string(contentID), errdefs.ErrNotFound)
    })

    return mountPath, nil
}
```

---

## 三、问题根因分析

### 3.1 核心问题：同一个 contentID 被多个容器共享

从日志中可以看到：

```
contentID=52361581-098f-4918-836a-ccced16afe21
  ├─ key1: k8s.io/89381/34af6d227eb9a9fbb386d81fe0325c1906d491895c7b2ede40574e84a9eec328
  │   mountPath: .../snapshots/69847
  │
  └─ key2: k8s.io/89383/91af4656aec49b97ee186d5b8e11df61c26cd7ebf3c228a2ff691cbd084bbb9b
      mountPath: .../snapshots/69849
```

**这意味着**：
- 同一个镜像层（contentID）被两个不同的容器使用
- 每个容器有自己的 snapshot（key）和挂载点（mountPath）
- **两个容器共享同一个 LV**

### 3.2 问题 1：先删除的容器会删除 snapshotKey 关联

**第一个容器被删除时（比如 k8s.io/89381/34af...）**：

1. StopContainer 或 handleContainerExit 被调用
2. 调用 `Update(unmountLvm="true")`
3. `SetUnmountedWithKey()` 被调用：
   - 读取 snapshot bucket 中的 `DevboxKeyPath`
   - 读取 contentID
   - 获取 content bucket
   - **删除 DevboxKeySnapshotKey**
4. `unmountLvm()` 执行卸载 ✅
5. **但 LV 没有被删除！**（只删除了关联）

**此时状态**：
- `snapshots["k8s.io/89381/34af..."]` bucket 还在（还没调用 Remove）
- `devbox_storage_path["52361581-..."]` bucket 还在
- **DevboxKeySnapshotKey 已被删除** ⚠️
- LV 还在使用中（被第二个容器使用）

### 3.3 问题 2：后删除的容器无法删除 snapshotKey

**第二个容器被删除时（比如 k8s.io/89383/91af...）**：

1. StopContainer 或 handleContainerExit 被调用
2. 调用 `Update(unmountLvm="true")`
3. `SetUnmountedWithKey()` 被调用：
   - 读取 snapshot bucket 中的 `DevboxKeyPath`
   - 读取 contentID
   - 获取 content bucket
   - ⚠️ **DevboxKeySnapshotKey 已经被第一个容器删除了**
4. **返回错误**：
   ```
   snapshot key for contentID 52361581-098f-4918-836a-ccced16afe21 is not set, cannot set status to unmounted: not found
   ```

**此时状态**：
- `DevboxKeySnapshotKey` 已经不存在（被第一个容器删除）
- 无法判断 LV 是否还挂载
- 无法删除关联
- **但 LV 还在使用中（无法删除）**

### 3.4 问题 3：为什么没有删除 LV？

**关键发现**：当前的删除流程中，**Remove 函数没有直接删除 LV**

**Remove 函数的删除流程**（devbox.go:394-450）：

```go
func (o *Snapshotter) Remove(ctx context.Context, key string) (err error) {
    // ...
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 1. 获取 mountPath
        mountPath, err = storage.RemoveDevbox(ctx, key)

        // 2. 如果 mountPath 不为空，卸载
        if mountPath != "" {
            o.unmountLvm(ctx, mountPath)
        }

        // 3. 删除 snapshot metadata
        storage.Remove(ctx, key)

        // 4. 收集需要删除的 LV
        removedLvNames, err = o.getCleanupLvNames(ctx)

        // ⚠️ 关键：getCleanupLvNames 只会清理"存在但不在数据库中"的 LV
        // 不会删除"在数据库中但还在使用"的 LV！
        return nil
    })()

    // 5. 在 defer 中删除 LV（事务外）
    for _, lvName := range removedLvNames {
        o.removeLv(ctx, lvName)  // ❌ 不会执行，因为 removedLvNames 为空
    }
}
```

**`getCleanupLvNames` 的逻辑**（devbox.go:582-594）：

```go
func (o *Snapshotter) getCleanupLvNames(ctx context.Context) ([]string, error) {
    // 1. 从 BoltDB 获取所有"在数据库中"的 LV
    nameMap, err := storage.GetDevboxLvNames(ctx)

    // 2. 从 LVM 获取所有 LV（包括使用中和未使用的）
    lvs, err := lvm.ListLVMLogicalVolumeByVG(ctx, o.lvmVgName, o.ThinPoolName)

    // 3. 找出"存在但不在数据库中"的 LV
    cleanup := []string{}
    for _, d := range lvs {
        if _, ok := nameMap[d.Name]; ok {
            continue  // 在 BoltDB 中存在，跳过
        }
        if strings.HasPrefix(d.Name, "devbox") {
            cleanup = append(cleanup, d.Name)
        }
    }

    // ⚠️ 不会删除"在数据库中但还在使用中"的 LV！
    return cleanup, nil
}
```

---

## 四、完整的问题链路

### 4.1 时间线重建

#### T1: 容器 A 和容器 B 使用同一个 contentID

```
k8s.io/89381/34af.../mountPath/69847  ← 容器 A (业务容器)
k8s.io/89383/91af.../mountPath/69849  ← 容器 B (可能是 Pod 重建后的容器)
  │
  └─ contentID: 52361581-098f-4918-836a-ccced16afe21 (镜像层 hash)
        │
        └─ LV: devbox-52361581-098f-4918-836a-ccced16afe21
```

#### T2: 容器 A 被删除（或停止/退出）

```
StopContainer/Exit (容器 A)
  ↓
UpdateDevboxSnapshot(unmountLvm="true")
  ↓
SetUnmountedWithKey(snapshot_key_1)
  ├─ 读取 DevboxKeyPath (有值)
  ├─ 读取 contentID
  ├─ 删除 DevboxKeySnapshotKey (关联已被删除) ✅
  └─ unmountLvm(mountPath) ✅ 执行卸载
```

#### T3: 容器 B 被删除（或停止/退出）

```
StopContainer/Exit (容器 B)
  ↓
UpdateDevboxSnapshot(unmountLvm="true")
  ↓
SetUnmountedWithKey(snapshot_key_2)
  ├─ 读取 DevboxKeyPath (有值)
  ├─ 读取 contentID
  ├─ 获取 content bucket
  ├─ ⚠️ DevboxKeySnapshotKey 不存在（已被容器 A 删除）
  │   └─ 返回错误："snapshot key for contentID xxx is not set" ❌
  └─ 无法删除关联和卸载
```

#### T4: 最后删除时 Remove 函数被调用

```
Remove(snapshot_key_2)
  ↓
RemoveDevbox(snapshot_key_2)
  ├─ 读取 DevboxKeyPath (有值)
  ├─ 读取 contentID (有值)
  ├─ 调用 getCleanupLvNames()
  │   ├─ 从 BoltDB 获取所有在数据库中的 LV
  │   └─ 从 LVM 获取所有 LV（包括使用中和未使用的）
  │   └─ 返回需要删除的 LV 数组
  │
  ↓
  defer 函数中删除 LV
  └─ for lvName in removedLvNames:
      o.removeLv(ctx, lvName)  ❌ 不会执行，因为 removedLvNames 为空
```

---

## 五、问题的根本原因

### 5.1 核心问题：devbox snapshotter 的数据结构设计问题

**当前设计**：
- `snapshots["<snapshot_key>"]`：存储容器快照的元数据
- `devbox_storage_path["<contentID>"]`：存储 contentID 相关的 LV 信息
- `devbox_storage_path["<contentID>"]["DevboxKeySnapshotKey"]`：**反向关联，记录这个 contentID 被哪个 snapshot key 使用**
- **只有一个 `DevboxKeySnapshotKey` 字段**

**问题**：
- 当多个容器共享一个 contentID 时
- 第一个删除的容器会删除 `DevboxKeySnapshotKey`
- 后续容器无法判断 LV 是否还在使用

### 5.2 为什么没有删除 LV？

**原因 1：getCleanupLvNames 只清理孤儿 LV**

```go
func (o *Snapshoter) getCleanupLvNames(ctx context.Context) ([]string, error) {
    // 只清理"存在但不在数据库中"的 LV
    nameMap, err := storage.GetDevboxLvNames(ctx)
    lvs, err := lvm.ListLVMLogicalVolumeByVG(ctx, o.lvmVgName, o.ThinPoolName)

    for _, d := range lvs {
        if _, ok := nameMap[d.Name]; ok {
            continue  // 在数据库中存在，跳过
        }
        // 删除不在数据库中的 LV
        if strings.HasPrefix(d.Name, "devbox") {
            cleanup = append(cleanup, d.Name)
        }
    }
    // ❌ 不会删除"在数据库中"的 LV
    return cleanup, nil
}
```

**原因 2：没有主动扫描并卸载残留的 LV**

当前的 Cleanup 函数**不够完善**，没有主动扫描所有 devbox LV 并卸载残留挂载。

---

## 六、解决方案

### 6.1 解决方案 1：为每个容器创建独立的 LV（长期方案）

**原理**：
- 不再共享 contentID
- 每个容器有独立的 LV
- 需要大量存储空间

### 6.2 解决方案 2：改进 getCleanupLvNames（当前推荐）✅

**原理**：
- 主动扫描所有 devbox LV
- 检查每个 LV 是否还在挂载
- 卸载并删除所有残留的 LV

**代码示例**：

```go
func (o *Snapshotter) getCleanupLvNames(ctx context.Context) ([]string, error) {
    // 1. 从 BoltDB 获取所有在数据库中的 LV
    nameMap, err := storage.GetDevboxLvNames(ctx)

    // 2. 从 LVM 获取所有 devbox LV
    lvs, err := lvm.ListLVMLogicalVolumeByVG(ctx, o.lvmVgName, o.ThinPoolName)

    // 3. 找出需要删除的 LV：
    //   - 不在数据库中（孤儿 LV）
    //   - 或者在数据库中但容器已不存在（孤儿容器）
    //   - 或者在数据库中但容器已删除
    for _, lv := range lvs {
        if !strings.HasPrefix(lv.Name, "devbox") {
            continue
        }

        // 检查 LV 是否还在使用
        if _, ok := nameMap[lv.Name]; !ok {
            // 不在数据库中，肯定是孤儿 LV，需要删除
            cleanup = append(cleanup, lv.Name)
        } else {
            // 在数据库中，检查容器是否还存在
            // TODO: 需要检查容器状态，如果容器已删除，也加入清理列表
        }
    }

    return cleanup, nil
}
```

### 6.3 解决方案 3：在 Remove 前先卸载所有 LV（临时方案）

**原理**：
- 不依赖 metadata 是否完整
- 直接扫描所有 devbox LV
- 检查并卸载所有挂载点
- 然后调用 removeLv

---

## 七、修复建议

### 7.1 立即修复（最简单）

**修改 `getCleanupLvNames` 函数**：

```go
func (o *Snapshotter) getCleanupLvNames(ctx context.Context) ([]string, error) {
    // 1. 获取所有 devbox LV
    lvs, err := lvm.ListLVMLogicalVolumeByVG(ctx, o.lvmVgName, o.ThinPoolName)
    if err != nil {
        return nil, err
    }

    // 2. 获取数据库中的 LV（用于过滤）
    nameMap, err := storage.GetDevboxLvNames(ctx)
    if err != nil {
        return nil, err
    }

    // 3. 找出所有需要删除的 LV
    cleanup := []string{}
    for _, lv := range lvs {
        if strings.HasPrefix(lv.Name, "devbox") {
            // 检查是否在数据库中
            if _, ok := nameMap[lv.Name]; !ok {
                // 不在数据库中，肯定是孤儿 LV，需要删除
                cleanup = append(cleanup, lv.Name)
            } else {
                // 在数据库中，但需要检查 LV 是否还在挂载
                devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lv.Name)
                mountPoints, _ := findMountPointByDevice(devicePath)
                if len(mountPoints) > 0 {
                    // LV 还在挂载，需要卸载并删除
                    cleanup = append(cleanup, lv.Name)
                }
            }
        }
    }

    return cleanup, nil
}
```

### 7.2 长期优化（推荐）✅✅✅

**重新设计数据结构**：

**方案 A**：为每个容器创建独立的 LV（最彻底）

**方案 B**：为每个容器创建独立的 LV 复制（平衡方案）

**方案 C**：在 Remove 时主动扫描并清理（当前方案 + 增强）

---

## 八、总结

### 8.1 问题的本质

**根本原因**：devbox snapshotter 设计上允许多个容器共享一个 contentID（镜像层），但：
1. 没有使用引用计数
2. 只有一个 `DevboxKeySnapshotKey` 字段
3. 第一个删除的容器会删除 snapshotKey 关联
4. 后续容器无法判断 LV 是否还在使用
5. `getCleanupLvNames` 只清理孤儿 LV，不会删除"在数据库中但还在使用"的 LV

### 8.2 为什么没有删除 LV？

**原因**：
- `getCleanupLvNames()` 只清理孤儿 LV（不在数据库中的）
- 两个容器都还在数据库中，所以 LV 不会被删除
- Remove 函数的 defer 函数中，`removedLvNames` 为空，不会执行删除

### 8.3 立即行动

**修改 `getCleanupLvNames` 函数**，添加主动扫描并卸载所有残留的 LV。

---

## 九、验证方法

```bash
# 1. 检查这个 contentID 的 LV 是否还存在
lvs | grep 52361581

# 2. 检查是否有残留的挂载点
mount | grep 52361581

# 3. 查看容器日志
crictl logs k8s.io 89383 89383 | tail -100
journalctl -u containerd | grep "89383" | tail -100

# 4. 检查是否有残留的 devbox LV
lvs | grep devbox | wc -l
```

---

## 十、最终建议

### 修复 getCleanupLvNames 的完整代码

```go
func (o *Snapshotter) getCleanupLvNames(ctx context.Context) ([]string, error) {
    // 1. 从 LVM 获取所有 devbox LV
    lvs, err := lvm.ListLVMLogicalVolumeByVG(ctx, o.lvmVgName, o.ThinPoolName)
    if err != nil {
        return nil, err
    }

    // 2. 获取数据库中的 LV
    nameMap, err := storage.GetDevboxLvNames(ctx)
    if err != nil {
        return nil, err
    }

    // 3. 找出需要删除的 LV
    cleanup := []string{}
    for _, lv := range lvs {
        if !strings.HasPrefix(lv.Name, "devbox") {
            continue
        }

        // 如果不在数据库中，加入清理列表（孤儿 LV）
        if _, ok := nameMap[lv.Name]; !ok {
            log.G(ctx).WithField("lvName", lv.Name).Infof("[CLEANUP-RECOMMEND] Orphaned LV found: %s", lv.Name)
            cleanup = append(cleanup, lv.Name)
        } else {
            // 在数据库中，检查 LV 是否还在挂载
            devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lv.Name)
            mountPoints, err := findMountPointByDevice(devicePath)
            if err != nil {
                log.G(ctx).WithError(err).WithField("lvName", lv.Name).Warnf("[CLEANUP-RECOMMEND] Failed to find mount point for LV")
                continue
            }

            if len(mountPoints) > 0 {
                // LV 还在挂载，需要卸载
                log.G(ctx).WithFields(logrus.Fields{
                    "lvName": lv.Name,
                    "mountPoints": mountPoints,
                }).Warnf("[CLEANUP-RECOMMEND] LV still mounted, adding to cleanup list")
                cleanup = append(cleanup, lv.Name)
            } else {
                log.G(ctx).WithField("lvName", lv.Name).Debugf("[CLEANUP-RECOMMEND] LV exists in database but not mounted")
            }
        }
    }

    return cleanup, nil
}
```

这个修复会**主动扫描所有 devbox LV**，检查是否还在挂载，并加入清理列表。

