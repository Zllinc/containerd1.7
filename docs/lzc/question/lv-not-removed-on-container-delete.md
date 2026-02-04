# 容器删除时 LV 未被删除的问题分析

## 一、问题描述

**现象**：
- 删除容器时，有时候没有触发 `snapshotter.Remove()` 接口
- 导致一些 LV 没有被删除，只能在 `Cleanup` 时被删除

**影响**：
- LV 资源泄漏
- 需要等待 GC 触发 `Cleanup` 才能清理
- 清理时间不固定（取决于 GC 调用频率）

---

## 二、问题根因分析

### 2.1 调用链路

**正常的删除流程**：

```
kubelet.RemoveContainer()
    ↓
criService.RemoveContainer()
    ↓
container.Container.Delete(ctx, containerd.WithSnapshotCleanup)
    ↓
WithSnapshotCleanup()
    ↓
snapshotter.Remove(ctx, snapshotKey)  // ✅ 应该被调用
    ↓
removeLv() → DestroyVolume()
```

### 2.2 问题点 1：WithSnapshotCleanup 的条件检查

**代码位置**：`container_opts.go:258-273`

```go
// WithSnapshotCleanup deletes the rootfs snapshot allocated for the container
func WithSnapshotCleanup(ctx context.Context, client *Client, c containers.Container) error {
	if c.SnapshotKey != "" {  // ⚠️ 关键：如果 SnapshotKey 为空，直接返回 nil
		if c.Snapshotter == "" {
			return fmt.Errorf("container.Snapshotter must be set to cleanup rootfs snapshot: %w", errdefs.ErrInvalidArgument)
		}
		s, err := client.getSnapshotter(ctx, c.Snapshotter)
		if err != nil {
			return err
		}
		if err := s.Remove(ctx, c.SnapshotKey); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	return nil  // ⚠️ SnapshotKey 为空时，直接返回，不调用 Remove
}
```

**问题**：
- 如果 `c.SnapshotKey == ""`，函数直接返回 `nil`，**不会调用 `snapshotter.Remove()`**
- 这会导致 LV 不会被删除

### 2.3 问题点 2：Container.Info() 返回 ErrNotFound

**代码位置**：`pkg/cri/server/container_remove.go:51-63`

```go
id := container.ID
i, err := container.Container.Info(ctx)
if err != nil {
	if !errdefs.IsNotFound(err) {
		return nil, fmt.Errorf("get container info: %w", id, err)
	}
	// Since containerd doesn't see the container and criservice's content store does,
	// we should try to recover from this state by removing entry for this container
	// from the container store as well and return successfully.
	log.G(ctx).WithError(err).Warn("get container info failed")
	c.containerStore.Delete(ctrID)
	c.containerNameIndex.ReleaseByKey(ctrID)
	return &runtime.RemoveContainerResponse{}, nil  // ⚠️ 直接返回，不会调用 Delete
}
```

**问题**：
- 如果 `container.Container.Info()` 返回 `ErrNotFound`，函数直接返回
- **不会调用 `container.Container.Delete()`**，因此不会触发 `WithSnapshotCleanup`
- 这会导致 LV 不会被删除

### 2.4 问题点 3：Container.Delete() 返回 ErrNotFound

**代码位置**：`pkg/cri/server/container_remove.go:106-111`

```go
// Delete containerd container.
if err := container.Container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
	if !errdefs.IsNotFound(err) {
		return nil, fmt.Errorf("failed to delete containerd container %q: %w", id, err)
	}
	log.G(ctx).Tracef("Remove called for containerd container %q that does not exist", id)
	// ⚠️ 如果是 ErrNotFound，继续执行，但 WithSnapshotCleanup 可能已经执行失败
}
```

**问题**：
- 如果 container 不存在，`Delete` 会返回 `ErrNotFound`
- 但 `WithSnapshotCleanup` 可能已经执行，也可能没有执行（取决于实现）
- 如果 `SnapshotKey` 为空，`WithSnapshotCleanup` 不会调用 `Remove`

---

## 三、什么情况下 SnapshotKey 会为空？

### 3.1 容器创建失败

**场景**：
- 容器创建过程中失败（如 snapshot 创建失败）
- `SnapshotKey` 可能没有被正确设置
- 容器 metadata 可能已经写入，但 `SnapshotKey` 为空

**代码位置**：`pkg/cri/server/container_create.go:203-212`

```go
opts := []containerd.NewContainerOpts{
	containerd.WithSnapshotter(c.runtimeSnapshotter(ctx, ociRuntime)),
	customopts.WithNewSnapshot(id, containerdImage, sOpts...),  // 如果这里失败
}
```

### 3.2 容器已被部分删除

**场景**：
- 容器已经被部分删除（如 metadata 已删除，但 snapshot 还在）
- 重新获取 container 时，`SnapshotKey` 可能为空

### 3.3 容器状态不一致

**场景**：
- 节点重启后，容器状态不一致
- `container.Container.Info()` 可能返回 `ErrNotFound`
- 或者返回的 container 的 `SnapshotKey` 为空

### 3.4 并发删除

**场景**：
- 多个 goroutine 同时删除同一个容器
- 第一个删除成功，第二个获取 container 时可能已经不存在
- 或者 `SnapshotKey` 已经被清空

---

## 四、解决方案

### 4.1 方案 1：在 RemoveContainer 中显式清理（推荐）

**思路**：即使 `WithSnapshotCleanup` 没有调用 `Remove`，也要确保 LV 被清理

**实现**：

```go
// pkg/cri/server/container_remove.go
func (c *criService) RemoveContainer(ctx context.Context, r *runtime.RemoveContainerRequest) (_ *runtime.RemoveContainerResponse, retErr error) {
	// ... 现有代码 ...
	
	// Delete containerd container.
	if err := container.Container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		if !errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("failed to delete containerd container %q: %w", id, err)
		}
		log.G(ctx).Tracef("Remove called for containerd container %q that does not exist", id)
	}
	
	// ✅ 新增：即使 Delete 失败或 SnapshotKey 为空，也要尝试清理 LV
	// 从 container store 中获取 snapshot key（如果存在）
	if i.SnapshotKey == "" {
		// 尝试从 devbox storage 中查找对应的 contentID
		// 通过 container ID 或其他方式查找
		log.G(ctx).Warnf("Container %q has empty SnapshotKey, attempting to cleanup LV by container ID", id)
		// 调用 devbox snapshotter 的清理方法
		if err := c.cleanupDevboxLVByContainerID(ctx, id); err != nil {
			log.G(ctx).WithError(err).Warnf("Failed to cleanup devbox LV for container %q", id)
		}
	}
	
	// ... 后续代码 ...
}
```

### 4.2 方案 2：增强 WithSnapshotCleanup

**思路**：即使 `SnapshotKey` 为空，也要尝试清理（通过其他方式查找）

**实现**：

```go
// container_opts.go
func WithSnapshotCleanup(ctx context.Context, client *Client, c containers.Container) error {
	if c.SnapshotKey != "" {
		// 原有逻辑
		if c.Snapshotter == "" {
			return fmt.Errorf("container.Snapshotter must be set to cleanup rootfs snapshot: %w", errdefs.ErrInvalidArgument)
		}
		s, err := client.getSnapshotter(ctx, c.Snapshotter)
		if err != nil {
			return err
		}
		if err := s.Remove(ctx, c.SnapshotKey); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	} else {
		// ✅ 新增：即使 SnapshotKey 为空，也要尝试清理
		// 对于 devbox snapshotter，可以通过 container ID 查找对应的 LV
		if c.Snapshotter == "devbox" {
			log.G(ctx).Warnf("Container %q has empty SnapshotKey, attempting to cleanup devbox LV", c.ID)
			s, err := client.getSnapshotter(ctx, "devbox")
			if err != nil {
				return err
			}
			// 调用 devbox snapshotter 的特殊清理方法
			if cleaner, ok := s.(interface {
				CleanupByContainerID(ctx context.Context, containerID string) error
			}); ok {
				if err := cleaner.CleanupByContainerID(ctx, c.ID); err != nil {
					log.G(ctx).WithError(err).Warnf("Failed to cleanup devbox LV for container %q", c.ID)
				}
			}
		}
	}
	return nil
}
```

### 4.3 方案 3：在 devbox snapshotter 中增强 Cleanup

**思路**：在 `Cleanup` 中更积极地清理残留的 LV

**实现**：

```go
// snapshots/devbox/devbox.go
func (o *Snapshotter) Cleanup(ctx context.Context) error {
	log.G(ctx).Infof("Cleanup called")
	
	// 原有逻辑：清理"存在但不在数据库中"的 LV
	cleanup, cleanupLv, err := o.cleanupDirectories(ctx)
	if err != nil {
		return err
	}
	
	// ✅ 新增：也清理"在数据库中但 container 已不存在的" LV
	orphanedLvs, err := o.getOrphanedLvNames(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Warn("Failed to get orphaned LV names")
	} else {
		cleanupLv = append(cleanupLv, orphanedLvs...)
	}
	
	// ... 清理逻辑 ...
}

func (o *Snapshotter) getOrphanedLvNames(ctx context.Context) ([]string, error) {
	// 获取所有 devbox content 记录
	allContents, err := storage.GetAllDevboxContents(ctx)
	if err != nil {
		return nil, err
	}
	
	orphaned := []string{}
	for contentID, contentInfo := range allContents {
		// 检查对应的 container 是否还存在
		// 如果不存在，说明是孤儿 LV
		if !o.containerExists(ctx, contentInfo.ContainerID) {
			lvName := "devbox-" + contentID
			orphaned = append(orphaned, lvName)
		}
	}
	
	return orphaned, nil
}
```

### 4.4 方案 4：在 RemoveContainer 中处理 ErrNotFound 情况

**思路**：即使 `container.Container.Info()` 返回 `ErrNotFound`，也要尝试清理 LV

**实现**：

```go
// pkg/cri/server/container_remove.go
func (c *criService) RemoveContainer(ctx context.Context, r *runtime.RemoveContainerRequest) (_ *runtime.RemoveContainerResponse, retErr error) {
	// ... 现有代码 ...
	
	id := container.ID
	i, err := container.Container.Info(ctx)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("get container info: %w", id, err)
		}
		// ✅ 修改：即使 container 不存在，也要尝试清理 LV
		log.G(ctx).WithError(err).Warn("get container info failed, attempting to cleanup LV")
		
		// 尝试通过 container ID 清理 devbox LV
		if err := c.cleanupDevboxLVByContainerID(ctx, ctrID); err != nil {
			log.G(ctx).WithError(err).Warnf("Failed to cleanup devbox LV for container %q", ctrID)
		}
		
		c.containerStore.Delete(ctrID)
		c.containerNameIndex.ReleaseByKey(ctrID)
		return &runtime.RemoveContainerResponse{}, nil
	}
	
	// ... 后续代码 ...
}
```

---

## 五、推荐方案

**建议采用方案 1 + 方案 3 的组合**：

1. **方案 1**：在 `RemoveContainer` 中显式清理，确保即使 `WithSnapshotCleanup` 失败也能清理
2. **方案 3**：增强 `Cleanup`，作为兜底机制，定期清理残留的 LV

**优势**：
- **双重保障**：立即清理 + 定期清理
- **兼容性好**：不需要修改 containerd 核心代码
- **易于实现**：主要在 devbox snapshotter 中实现

---

## 六、实现细节

### 6.1 在 devbox snapshotter 中添加 CleanupByContainerID

```go
// snapshots/devbox/devbox.go
func (o *Snapshotter) CleanupByContainerID(ctx context.Context, containerID string) error {
	// 1. 通过 container ID 查找对应的 contentID
	contentID, err := storage.GetContentIDByContainerID(ctx, containerID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil  // 没有找到，说明已经清理过了
		}
		return err
	}
	
	// 2. 查找对应的 LV
	lvName := "devbox-" + contentID
	
	// 3. 检查 LV 是否存在
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{Name: lvName},
		Spec:       apis.VolumeInfo{VolGroup: o.lvmVgName},
	}
	exists, err := lvm.CheckVolumeExists(ctx, vol)
	if err != nil {
		return err
	}
	if !exists {
		return nil  // LV 不存在，说明已经清理过了
	}
	
	// 4. 尝试卸载和删除 LV
	if err := o.removeLv(ctx, lvName); err != nil {
		log.G(ctx).WithError(err).WithField("lvName", lvName).
			Warn("CleanupByContainerID: failed to destroy LVM logical volume")
		return err
	}
	
	log.G(ctx).Infof("CleanupByContainerID: successfully removed LV %s for container %s", lvName, containerID)
	return nil
}
```

### 6.2 在 storage 中添加 GetContentIDByContainerID

```go
// snapshots/devbox/storage/bolt.go
func GetContentIDByContainerID(ctx context.Context, containerID string) (string, error) {
	// 遍历所有 devbox content，查找 containerID 匹配的记录
	// 返回对应的 contentID
}
```

---

## 七、测试建议

### 7.1 测试场景

1. **正常删除**：容器正常删除，验证 LV 被删除
2. **SnapshotKey 为空**：模拟 `SnapshotKey` 为空的情况，验证 LV 仍能被清理
3. **Container.Info() 返回 ErrNotFound**：模拟容器不存在的情况，验证 LV 仍能被清理
4. **并发删除**：多个 goroutine 同时删除，验证不会重复删除

### 7.2 验证方法

- 检查日志，确认 `Remove` 被调用
- 检查 LVM，确认 LV 被删除
- 检查数据库，确认 metadata 被清理

---

## 八、总结

**问题根因**：
1. `WithSnapshotCleanup` 在 `SnapshotKey` 为空时不会调用 `Remove`
2. `container.Container.Info()` 返回 `ErrNotFound` 时不会调用 `Delete`
3. 容器创建失败或状态不一致时，`SnapshotKey` 可能为空

**解决方案**：
- 在 `RemoveContainer` 中显式清理 LV
- 增强 `Cleanup` 作为兜底机制
- 添加 `CleanupByContainerID` 方法支持通过 container ID 清理

**参考代码**：
- `container_opts.go:258-273` - `WithSnapshotCleanup` 实现
- `pkg/cri/server/container_remove.go:51-111` - `RemoveContainer` 实现
- `snapshots/devbox/devbox.go:394-450` - `Remove` 实现

