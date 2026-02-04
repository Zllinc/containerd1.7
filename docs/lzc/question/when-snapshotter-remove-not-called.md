# 容器什么时候不会执行 Snapshotter 的 Remove 函数？

## 一、问题分析

### 1.1 调用链

```
RemoveContainer (container_remove.go:35)
  └─> container.Container.Delete(ctx, containerd.WithSnapshotCleanup) (line 106)
      └─> WithSnapshotCleanup (container_opts.go:259)
          └─> s.Remove(ctx, c.SnapshotKey) (line 268)
```

### 1.2 关键代码位置

**`pkg/cri/server/container_remove.go:106`**：
```go
if err := container.Container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
    if !errdefs.IsNotFound(err) {
        return nil, fmt.Errorf("failed to delete containerd container %q: %w", id, err)
    }
    log.G(ctx).Tracef("Remove called for containerd container %q that does not exist", id)
}
```

**`container_opts.go:258-273`**：
```go
func WithSnapshotCleanup(ctx context.Context, client *Client, c containers.Container) error {
    if c.SnapshotKey != "" {  // ← 条件 1：SnapshotKey 必须非空
        if c.Snapshotter == "" {  // ← 条件 2：Snapshotter 必须非空
            return fmt.Errorf("container.Snapshotter must be set to cleanup rootfs snapshot: %w", errdefs.ErrInvalidArgument)
        }
        s, err := client.getSnapshotter(ctx, c.Snapshotter)  // ← 条件 3：必须能获取 snapshotter
        if err != nil {
            return err
        }
        if err := s.Remove(ctx, c.SnapshotKey); err != nil && !errdefs.IsNotFound(err) {
            return err  // ← 条件 4：Remove 失败且不是 NotFound
        }
    }
    return nil
}
```

---

## 二、不会执行 Remove 的情况

### 情况 1：容器 metadata 不存在 ⭐ 最常见

**代码位置**：`container_remove.go:38-45`

```go
container, err := c.containerStore.Get(ctrID)
if err != nil {
    if !errdefs.IsNotFound(err) {
        return nil, fmt.Errorf("an error occurred when try to find container %q: %w", ctrID, err)
    }
    // Do not return error if container metadata doesn't exist.
    log.G(ctx).Tracef("RemoveContainer called for container %q that does not exist", ctrID)
    return &runtime.RemoveContainerResponse{}, nil  // ← 直接返回，不会执行到 Delete
}
```

**原因**：
- 容器在 CRI 的 containerStore 中不存在
- 可能已经被删除，或者从未创建成功

**影响**：
- ❌ 不会执行 `container.Container.Delete`
- ❌ 不会调用 `WithSnapshotCleanup`
- ❌ 不会调用 `s.Remove`

**场景**：
- 容器创建失败，但 kubelet 仍然尝试删除
- 容器已经被删除，但 kubelet 重试删除
- 数据库不一致

---

### 情况 2：Containerd 中容器不存在

**代码位置**：`container_remove.go:51-62`

```go
i, err := container.Container.Info(ctx)
if err != nil {
    if !errdefs.IsNotFound(err) {
        return nil, fmt.Errorf("get container info: %w", err)
    }
    // Since containerd doesn't see the container and criservice's content store does,
    // we should try to recover from this state by removing entry for this container
    // from the container store as well and return successfully.
    log.G(ctx).WithError(err).Warn("get container info failed")
    c.containerStore.Delete(ctrID)
    c.containerNameIndex.ReleaseByKey(ctrID)
    return &runtime.RemoveContainerResponse{}, nil  // ← 直接返回，不会执行到 Delete
}
```

**原因**：
- CRI 的 containerStore 中有容器，但 containerd 中不存在
- 状态不一致

**影响**：
- ❌ 不会执行 `container.Container.Delete`
- ❌ 不会调用 `WithSnapshotCleanup`
- ❌ 不会调用 `s.Remove`

**场景**：
- Containerd 重启后，容器状态丢失
- 手动删除了 containerd 中的容器，但 CRI 中还有记录

---

### 情况 3：强制停止容器失败

**代码位置**：`container_remove.go:65-72`

```go
state := container.Status.Get().State()
if state == runtime.ContainerState_CONTAINER_RUNNING ||
    state == runtime.ContainerState_CONTAINER_UNKNOWN {
    logrus.Infof("Forcibly stopping container %q", id)
    if err := c.stopContainer(ctx, container, 0); err != nil {
        return nil, fmt.Errorf("failed to forcibly stop container %q: %w", id, err)  // ← 返回错误，不会执行到 Delete
    }
}
```

**原因**：
- 容器正在运行或状态未知
- 尝试强制停止失败

**影响**：
- ❌ 不会执行 `container.Container.Delete`
- ❌ 不会调用 `WithSnapshotCleanup`
- ❌ 不会调用 `s.Remove`

**场景**：
- 容器进程卡死，无法停止
- 系统资源不足，无法执行停止操作

---

### 情况 4：设置 removing 状态失败

**代码位置**：`container_remove.go:78-79`

```go
if err := setContainerRemoving(container); err != nil {
    return nil, fmt.Errorf("failed to set removing state for container %q: %w", id, err)  // ← 返回错误，不会执行到 Delete
}
```

**`setContainerRemoving` 可能失败的情况**（`container_remove.go:142-159`）：
- 容器仍在运行（`CONTAINER_RUNNING`）
- 容器状态未知（`CONTAINER_UNKNOWN`）
- 容器正在启动（`Starting == true`）
- 容器已经在 removing 状态（`Removing == true`）

**影响**：
- ❌ 不会执行 `container.Container.Delete`
- ❌ 不会调用 `WithSnapshotCleanup`
- ❌ 不会调用 `s.Remove`

**场景**：
- 并发删除操作
- 状态竞争条件

---

### 情况 5：SnapshotKey 为空

**代码位置**：`container_opts.go:260`

```go
func WithSnapshotCleanup(ctx context.Context, client *Client, c containers.Container) error {
    if c.SnapshotKey != "" {  // ← 如果为空，直接返回，不调用 Remove
        // ...
    }
    return nil
}
```

**原因**：
- 容器没有 snapshot（例如：使用 `--keep-snapshot` 创建）
- 容器创建时没有分配 snapshot

**影响**：
- ✅ 会执行 `container.Container.Delete`
- ✅ 会调用 `WithSnapshotCleanup`
- ❌ **不会调用 `s.Remove`**（因为 `SnapshotKey` 为空）

**场景**：
- 使用 `ctr containers delete --keep-snapshot` 删除容器
- 容器创建失败，但 metadata 中仍有记录

---

### 情况 6：Snapshotter 为空

**代码位置**：`container_opts.go:261-263`

```go
if c.Snapshotter == "" {
    return fmt.Errorf("container.Snapshotter must be set to cleanup rootfs snapshot: %w", errdefs.ErrInvalidArgument)
}
```

**原因**：
- 容器的 `Snapshotter` 字段未设置
- 容器创建时没有指定 snapshotter

**影响**：
- ✅ 会执行 `container.Container.Delete`
- ✅ 会调用 `WithSnapshotCleanup`
- ❌ **不会调用 `s.Remove`**（因为返回错误）

**注意**：
- 如果 `WithSnapshotCleanup` 返回错误，`container.Delete` 的行为取决于实现
- 通常 `Delete` 会继续执行，但 snapshot 不会被清理

---

### 情况 7：无法获取 Snapshotter

**代码位置**：`container_opts.go:264-267`

```go
s, err := client.getSnapshotter(ctx, c.Snapshotter)
if err != nil {
    return err  // ← 返回错误，不会调用 Remove
}
```

**原因**：
- Snapshotter 插件未注册
- Snapshotter 名称错误
- Snapshotter 插件加载失败

**影响**：
- ✅ 会执行 `container.Container.Delete`
- ✅ 会调用 `WithSnapshotCleanup`
- ❌ **不会调用 `s.Remove`**（因为返回错误）

**场景**：
- Devbox snapshotter 未正确注册
- Snapshotter 名称拼写错误（如 `devbox` vs `Devbox`）

---

### 情况 8：Container.Delete 返回 NotFound 错误

**代码位置**：`container_remove.go:106-111`

```go
if err := container.Container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
    if !errdefs.IsNotFound(err) {
        return nil, fmt.Errorf("failed to delete containerd container %q: %w", id, err)
    }
    log.G(ctx).Tracef("Remove called for containerd container %q that does not exist", id)
    // ← 继续执行，但 WithSnapshotCleanup 可能已经执行失败
}
```

**原因**：
- Containerd 中容器不存在
- 但 `WithSnapshotCleanup` 可能已经执行

**影响**：
- ✅ 会执行 `container.Container.Delete`
- ✅ 会调用 `WithSnapshotCleanup`
- ⚠️ **`s.Remove` 可能执行，也可能不执行**（取决于 `WithSnapshotCleanup` 内部逻辑）

**注意**：
- 如果 `WithSnapshotCleanup` 在 `Delete` 之前执行，且 `SnapshotKey` 存在，`Remove` 会被调用
- 如果 `WithSnapshotCleanup` 在 `Delete` 之后执行，且容器已不存在，可能无法获取容器信息

---

## 三、总结

### 3.1 完全不会执行 Delete 的情况（不会调用 Remove）

1. ✅ **容器 metadata 不存在**（情况 1）
2. ✅ **Containerd 中容器不存在**（情况 2）
3. ✅ **强制停止失败**（情况 3）
4. ✅ **设置 removing 状态失败**（情况 4）

### 3.2 会执行 Delete，但不会调用 Remove 的情况

5. ✅ **SnapshotKey 为空**（情况 5）
6. ✅ **Snapshotter 为空**（情况 6）
7. ✅ **无法获取 Snapshotter**（情况 7）

### 3.3 可能执行 Remove 的情况

8. ⚠️ **Container.Delete 返回 NotFound**（情况 8）
   - 取决于 `WithSnapshotCleanup` 的执行时机和容器状态

---

## 四、对 Devbox Snapshotter 的影响

### 4.1 可能导致 LV 泄漏的情况

1. **情况 5：SnapshotKey 为空**
   - 容器 metadata 存在，但没有 snapshot
   - LV 可能已经创建，但不会被清理

2. **情况 6：Snapshotter 为空**
   - 容器创建时没有正确设置 snapshotter
   - LV 可能已经创建，但不会被清理

3. **情况 7：无法获取 Snapshotter**
   - Devbox snapshotter 未正确注册
   - LV 可能已经创建，但不会被清理

### 4.2 建议

1. **确保 SnapshotKey 和 Snapshotter 正确设置**
   - 在容器创建时验证
   - 在删除时检查

2. **添加 fallback 清理机制**
   - 即使 `WithSnapshotCleanup` 失败，也要尝试清理 LV
   - 使用 GC 作为兜底机制

3. **监控和告警**
   - 监控 `WithSnapshotCleanup` 的失败情况
   - 告警 LV 泄漏

---

## 五、验证方法

### 5.1 添加日志

**在 `WithSnapshotCleanup` 中添加日志**：
```go
func WithSnapshotCleanup(ctx context.Context, client *Client, c containers.Container) error {
    log.G(ctx).WithFields(log.Fields{
        "container_id": c.ID,
        "snapshot_key": c.SnapshotKey,
        "snapshotter": c.Snapshotter,
    }).Debug("WithSnapshotCleanup called")
    
    if c.SnapshotKey != "" {
        // ...
    } else {
        log.G(ctx).WithField("container_id", c.ID).Warn("WithSnapshotCleanup: SnapshotKey is empty")
    }
    return nil
}
```

**在 `RemoveContainer` 中添加日志**：
```go
log.G(ctx).WithFields(log.Fields{
    "container_id": ctrID,
    "exists_in_store": container != nil,
}).Debug("RemoveContainer called")

if container != nil {
    i, err := container.Container.Info(ctx)
    log.G(ctx).WithFields(log.Fields{
        "container_id": ctrID,
        "exists_in_containerd": err == nil,
        "snapshot_key": i.SnapshotKey,
        "snapshotter": i.Snapshotter,
    }).Debug("Container info")
}
```

### 5.2 检查容器状态

```bash
# 查看容器 metadata
crictl ps -a

# 查看 containerd 容器
ctr containers list

# 查看 snapshot
ctr snapshots list
```

---

## 附录：相关代码位置

- `pkg/cri/server/container_remove.go:35-138` - `RemoveContainer` 实现
- `container_opts.go:258-273` - `WithSnapshotCleanup` 实现
- `container.go:172-210` - `container.Delete` 实现
- `pkg/cri/server/container_remove.go:142-169` - `setContainerRemoving` 实现
















