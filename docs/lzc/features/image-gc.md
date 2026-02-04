# Kubelet Image GC 触发机制与 Content Blob 丢失分析

## 一、Image GC 的所有触发条件

根据 Kubernetes 源码分析，Image GC 有以下三种触发方式：

### 1. 周期性定时触发

**代码位置**: `pkg/kubelet/kubelet.go`

```1605:1627:pkg/kubelet/kubelet.go
	prevImageGCFailed := false
	beganGC := time.Now()
	go wait.Until(func() {
		ctx := context.Background()
		if err := kl.imageManager.GarbageCollect(ctx, beganGC); err != nil {
			if prevImageGCFailed {
				klog.ErrorS(err, "Image garbage collection failed multiple times in a row")
				// Only create an event for repeated failures
				kl.recorder.Event(kl.nodeRef, v1.EventTypeWarning, events.ImageGCFailed, err.Error())
			} else {
				klog.ErrorS(err, "Image garbage collection failed once. Stats initialization may not have completed yet")
			}
			prevImageGCFailed = true
		} else {
			var vLevel klog.Level = 4
			if prevImageGCFailed {
				vLevel = 1
				prevImageGCFailed = false
			}

			klog.V(vLevel).InfoS("Image garbage collection succeeded")
		}
	}, ImageGCPeriod, wait.NeverStop)
```

**触发条件**：
| 条件 | 默认值 | 说明 |
|------|--------|------|
| `ImageGCPeriod` | 5 分钟 | GC 执行周期 |
| `ImageGCHighThresholdPercent` | 85% | 磁盘使用率超过此值时触发清理 |
| `ImageGCLowThresholdPercent` | 80% | 清理目标：将使用率降到此值以下 |
| `ImageMinimumGCAge` | 2 分钟 | 镜像存活最小时间，新拉取的镜像不会被立即删除 |

**核心逻辑** (`pkg/kubelet/images/image_gc_manager.go`):

```390:414:pkg/kubelet/images/image_gc_manager.go
	// If over the max threshold, free enough to place us at the lower threshold.
	usagePercent := 100 - int(available*100/capacity)
	if usagePercent >= im.policy.HighThresholdPercent {
		amountToFree := capacity*int64(100-im.policy.LowThresholdPercent)/100 - available
		logger.Info("Disk usage on image filesystem is over the high threshold, trying to free bytes down to the low threshold", "usage", usagePercent, "highThreshold", im.policy.HighThresholdPercent, "amountToFree", amountToFree, "lowThreshold", im.policy.LowThresholdPercent)
		remainingImages, freed, err := im.freeSpace(ctx, amountToFree, freeTime, images)
		if err != nil {
			// Failed to delete images, eg due to a read-only filesystem.
			return err
		}

		im.runPostGCHooks(ctx, remainingImages, freeTime)

		if freed < amountToFree {
			// ...
			im.recorder.Eventf(im.nodeRef, v1.EventTypeWarning, events.FreeDiskSpaceFailed, "%s", message)
			return fmt.Errorf("%s", message)
		}
	}
```

---

### 2. 镜像最大年龄触发（MaxAge）

**代码位置**: `pkg/kubelet/images/image_gc_manager.go`

```425:454:pkg/kubelet/images/image_gc_manager.go
func (im *realImageGCManager) freeOldImages(ctx context.Context, images []evictionInfo, freeTime, beganGC time.Time) ([]evictionInfo, error) {
	if im.policy.MaxAge == 0 {
		return images, nil
	}

	// Wait until the MaxAge has passed since the Kubelet has started,
	// or else we risk prematurely garbage collecting images.
	if freeTime.Sub(beganGC) <= im.policy.MaxAge {
		return images, nil
	}
	var deletionErrors []error
	logger := klog.FromContext(ctx)
	remainingImages := make([]evictionInfo, 0)
	for _, image := range images {
		logger.V(5).Info("Evaluating image ID for possible garbage collection based on image age", "imageID", image.id)
		// Evaluate whether image is older than MaxAge.
		if freeTime.Sub(image.lastUsed) > im.policy.MaxAge {
			if err := im.freeImage(ctx, image, ImageGarbageCollectedTotalReasonAge); err != nil {
				deletionErrors = append(deletionErrors, err)
				remainingImages = append(remainingImages, image)
				continue
			}
			continue
		}
		remainingImages = append(remainingImages, image)
	}
	// ...
}
```

**触发条件**：
| 条件 | 默认值 | 说明 |
|------|--------|------|
| `ImageMaximumGCAge` | 0（禁用） | 镜像未使用超过此时间后被删除，需开启 `ImageMaximumGCAge` Feature Gate |

---

### 3. 驱逐管理器（Eviction Manager）触发

**代码位置**: `pkg/kubelet/eviction/eviction_manager.go` 和 `pkg/kubelet/eviction/helpers.go`

当节点资源压力达到阈值时，Eviction Manager 会调用 `DeleteUnusedImages` 删除所有未使用的镜像。

**触发信号类型** (`pkg/kubelet/eviction/api/types.go`):

```31:52:pkg/kubelet/eviction/api/types.go
	// SignalNodeFsAvailable is amount of storage available on filesystem that kubelet uses for volumes, daemon logs, etc.
	SignalNodeFsAvailable Signal = "nodefs.available"
	// SignalNodeFsInodesFree is amount of inodes available on filesystem that kubelet uses for volumes, daemon logs, etc.
	SignalNodeFsInodesFree Signal = "nodefs.inodesFree"
	// SignalImageFsAvailable is amount of storage available on filesystem that container runtime uses for storing images layers.
	SignalImageFsAvailable Signal = "imagefs.available"
	// SignalImageFsInodesFree is amount of inodes available on filesystem that container runtime uses for storing images layers.
	SignalImageFsInodesFree Signal = "imagefs.inodesFree"
	// SignalContainerFsAvailable is amount of storage available on filesystem that container runtime uses for container writable layers.
	SignalContainerFsAvailable Signal = "containerfs.available"
	// SignalContainerFsInodesFree is amount of inodes available on filesystem that container runtime uses for container writable layers.
	SignalContainerFsInodesFree Signal = "containerfs.inodesFree"
```

**信号与 GC 函数映射** (`pkg/kubelet/eviction/helpers.go`):

```1203:1237:pkg/kubelet/eviction/helpers.go
// buildSignalToNodeReclaimFuncs returns reclaim functions associated with resources.
func buildSignalToNodeReclaimFuncs(imageGC ImageGC, containerGC ContainerGC, withImageFs bool, splitContainerImageFs bool) map[evictionapi.Signal]nodeReclaimFuncs {
	signalToReclaimFunc := map[evictionapi.Signal]nodeReclaimFuncs{}
	// usage of an imagefs is optional
	if withImageFs && !splitContainerImageFs {
		// with an imagefs, nodefs pressure should just delete logs
		signalToReclaimFunc[evictionapi.SignalNodeFsAvailable] = nodeReclaimFuncs{}
		signalToReclaimFunc[evictionapi.SignalNodeFsInodesFree] = nodeReclaimFuncs{}
		// with an imagefs, imagefs pressure should delete unused images
		signalToReclaimFunc[evictionapi.SignalImageFsAvailable] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalImageFsInodesFree] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		// ...
	} else {
		// without an imagefs, nodefs pressure should delete logs, and unused images
		signalToReclaimFunc[evictionapi.SignalNodeFsAvailable] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalNodeFsInodesFree] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalImageFsAvailable] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		signalToReclaimFunc[evictionapi.SignalImageFsInodesFree] = nodeReclaimFuncs{containerGC.DeleteAllUnusedContainers, imageGC.DeleteUnusedImages}
		// ...
	}
	return signalToReclaimFunc
}
```

**驱逐触发流程** (`pkg/kubelet/eviction/eviction_manager.go`):

```467:497:pkg/kubelet/eviction/eviction_manager.go
// reclaimNodeLevelResources attempts to reclaim node level resources.  returns true if thresholds were satisfied and no pod eviction is required.
func (m *managerImpl) reclaimNodeLevelResources(ctx context.Context, signalToReclaim evictionapi.Signal, resourceToReclaim v1.ResourceName) bool {
	nodeReclaimFuncs := m.signalToNodeReclaimFuncs[signalToReclaim]
	for _, nodeReclaimFunc := range nodeReclaimFuncs {
		// attempt to reclaim the pressured resource.
		if err := nodeReclaimFunc(ctx); err != nil {
			klog.InfoS("Eviction manager: unexpected error when attempting to reduce resource pressure", "resourceName", resourceToReclaim, "err", err)
		}

	}
	// ...
}
```

---

### Image GC 触发条件汇总表

| 触发类型 | 触发条件 | 配置参数 | 删除范围 |
|----------|----------|----------|----------|
| **周期性 GC** | 每 5 分钟检查一次，磁盘使用率 ≥ 85% | `ImageGCHighThresholdPercent`, `ImageGCLowThresholdPercent` | 按 LRU 删除直到使用率 < 80% |
| **MaxAge GC** | 镜像未使用时间 > MaxAge | `ImageMaximumGCAge` (需开启 Feature Gate) | 所有超龄未使用镜像 |
| **磁盘压力驱逐** | `imagefs.available` < 阈值 | `evictionHard.imagefs.available` | 所有未使用镜像 |
| **磁盘压力驱逐** | `imagefs.inodesFree` < 阈值 | `evictionHard.imagefs.inodesFree` | 所有未使用镜像 |
| **磁盘压力驱逐** | `nodefs.available` < 阈值（无独立 imagefs 时） | `evictionHard.nodefs.available` | 所有未使用镜像 |
| **磁盘压力驱逐** | `nodefs.inodesFree` < 阈值（无独立 imagefs 时） | `evictionHard.nodefs.inodesFree` | 所有未使用镜像 |

---

## 二、Image GC 如何导致 Content Blob 丢失

### 删除链路分析

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        Image GC 删除链路                                      │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  1. Kubelet ImageGCManager                                                   │
│     └── freeImage() 或 DeleteUnusedImages()                                  │
│         └── runtime.RemoveImage(imageID)                                     │
│                                                                              │
│  2. KubeGenericRuntimeManager (pkg/kubelet/kuberuntime/kuberuntime_image.go) │
│     └── RemoveImage()                                                        │
│         └── imageService.RemoveImage(ctx, &runtimeapi.ImageSpec{...})        │
│                                                                              │
│  3. CRI ImageService (通过 gRPC 调用 containerd)                              │
│     └── containerd 的 images.Delete() API                                    │
│                                                                              │
│  4. Containerd 内部处理                                                       │
│     └── 删除 k8s.io namespace 中的 image reference                           │
│     └── 触发 content store GC (content.GarbageCollection)                    │
│     └── 检查 content blob 引用计数                                            │
│     └── 如无其他引用 → 删除 content blob                                      │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

**关键代码** (`pkg/kubelet/kuberuntime/kuberuntime_image.go`):

```137:147:pkg/kubelet/kuberuntime/kuberuntime_image.go
// RemoveImage removes the specified image.
func (m *kubeGenericRuntimeManager) RemoveImage(ctx context.Context, image kubecontainer.ImageSpec) error {
	logger := klog.FromContext(ctx)
	err := m.imageService.RemoveImage(ctx, &runtimeapi.ImageSpec{Image: image.Image})
	if err != nil {
		logger.Error(err, "Failed to remove image", "image", image.Image)
		return err
	}

	return nil
}
```

### Content Blob 丢失原因

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                     Containerd Content Store 架构                            │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│   Content Store (共享存储)                                                   │
│   ┌────────────────────────────────────────────────────────────────────┐    │
│   │  sha256:abc123... (config blob)                                    │    │
│   │  sha256:def456... (layer blob)                                     │    │
│   │  sha256:d20f38e... (基础镜像的 config - commit 需要读取此 blob)     │    │
│   └────────────────────────────────────────────────────────────────────┘    │
│                           ▲                          ▲                       │
│                           │                          │                       │
│   ┌───────────────────────┴──────┐   ┌───────────────┴──────────────────┐   │
│   │     Namespace: k8s.io        │   │     Namespace: sealos.io         │   │
│   │  ┌────────────────────────┐  │   │  ┌────────────────────────────┐  │   │
│   │  │ Image: ghcr.io/xxx:tag │  │   │  │ Image: (无相同镜像)         │  │   │
│   │  │ (引用 sha256:d20f38e)  │  │   │  │                            │  │   │
│   │  └────────────────────────┘  │   │  └────────────────────────────┘  │   │
│   │          │                   │   │                                  │   │
│   │  ┌───────▼────────────────┐  │   │                                  │   │
│   │  │ Container (使用镜像)   │  │   │                                  │   │
│   │  └────────────────────────┘  │   │                                  │   │
│   └──────────────────────────────┘   └──────────────────────────────────┘   │
│                                                                              │
│   ⚠️ Kubelet 只管理 k8s.io namespace                                        │
│   ⚠️ 当 Kubelet 删除 k8s.io 中的镜像引用后，                                 │
│      如果 sealos.io 中没有相同引用，content blob 会被 GC 删除                │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Content Blob 丢失的详细过程

```
时间线：
┌────────────────────────────────────────────────────────────────────────────┐
│                                                                             │
│  T0: Devbox Pod 启动                                                        │
│      └── 基础镜像 ghcr.io/xxx:tag 存在于 k8s.io namespace                   │
│      └── 容器运行，使用 snapshot (不再直接引用镜像)                          │
│                                                                             │
│  T1: 容器内写入 10GB 数据                                                    │
│      └── /var/lib/containerd 磁盘使用率上升                                 │
│      └── 磁盘使用率达到 85%+ (或触发 eviction threshold)                    │
│                                                                             │
│  T2: Kubelet 触发 Image GC                                                  │
│      └── 扫描 k8s.io namespace 中的镜像                                     │
│      └── 检测镜像使用情况 (detectImages)                                    │
│      └── 基础镜像被判定为"未使用" ⚠️                                        │
│          (因为运行中的容器使用的是 snapshot，不直接引用镜像)                 │
│                                                                             │
│  T3: Kubelet 删除基础镜像                                                    │
│      └── 调用 imageService.RemoveImage()                                    │
│      └── containerd 删除 k8s.io namespace 中的 image reference              │
│      └── containerd 触发 content GC                                         │
│      └── sha256:d20f38e2b... 等 content blob 被删除                         │
│                                                                             │
│  T4: 用户执行 commit 操作                                                    │
│      └── 需要读取基础镜像 config 生成新 config                               │
│      └── 尝试读取 sha256:d20f38e2b...                                       │
│      └── ❌ content digest not found                                        │
│                                                                             │
└────────────────────────────────────────────────────────────────────────────┘
```

### 判断镜像"未使用"的逻辑

**代码位置**: `pkg/kubelet/images/image_gc_manager.go`

```243:273:pkg/kubelet/images/image_gc_manager.go
func (im *realImageGCManager) detectImages(ctx context.Context, detectTime time.Time) (sets.Set[string], error) {
	// ...
	imagesInUse := sets.New[string]()

	images, err := im.runtime.ListImages(ctx)
	pods, err := im.runtime.GetPods(ctx, true)

	// Make a set of images in use by containers.
	for _, pod := range pods {
		for _, container := range pod.Containers {
			// ...
			imagesInUse.Insert(container.ImageID)
		}
	}
	// ...
}
```

**关键问题**：
- Kubelet 判断镜像是否"在使用"是基于**运行中容器的 ImageID**
- 但容器运行后实际使用的是 **snapshot**，不再直接引用镜像的 content
- 因此，即使容器正在运行，其基础镜像仍可能被判定为"未使用"并被删除

### 为什么使用 `sealos.io` Namespace 不会出现此问题

| 特性 | k8s.io Namespace | sealos.io Namespace |
|------|------------------|---------------------|
| **管理者** | Kubelet | Devbox Controller |
| **GC 机制** | Kubelet Image GC + Eviction Manager | 无自动 GC |
| **删除时机** | 周期性 + 磁盘压力触发 | 仅手动或 Controller 控制 |
| **Content 引用** | 删除镜像后可能触发 content GC | 引用保持，content 不被 GC |

#### 关键机制：Content Store 全局共享 + 引用计数

**Content Store 架构**：
- Content blob（物理文件）是**全局共享**的，存储在 `/var/lib/containerd/io.containerd.content.v1.content/blobs/`
- Content 引用/metadata 是**按 namespace 隔离**的，存储在 BoltDB 中
- Content blob 只有在**所有 namespace 都不引用**时才会被 GC 删除

**GC 扫描逻辑**（`metadata/content.go` 第 803-888 行）：
```go
// GC 会遍历【所有 namespace】收集被引用的 content
v1c := v1bkt.Cursor()
for k, v := v1c.First(); k != nil; k, v = v1c.Next() {
    // 遍历每个 namespace
    cbkt := v1bkt.Bucket(k).Bucket(bucketKeyObjectContent)
    // 收集该 namespace 中被引用的 content digest
    contentSeen[string(ck)] = struct{}{}
}
// 只删除不在 contentSeen 中的 content
```

**这意味着**：
1. 如果在 `sealos.io` 中也拉取了基础镜像，该镜像的 content 有两个引用（k8s.io + sealos.io）
2. Kubelet GC 只删除 `k8s.io` 中的引用，不影响 `sealos.io` 中的引用
3. Content blob 仍有引用（sealos.io），不会被物理删除
4. 在 `sealos.io` namespace 执行 commit 时可以正常读取 content

详细文档参见：[Content Store 与 Namespace 共享机制详解](./content-store-namespace-sharing.md)

---

## 三、总结

### Image GC 触发条件完整列表

1. **周期性定时触发**（每 5 分钟）
   - 条件：磁盘使用率 ≥ `ImageGCHighThresholdPercent`（默认 85%）
   - 行为：按 LRU 删除镜像直到使用率 < `ImageGCLowThresholdPercent`（默认 80%）

2. **镜像最大年龄触发**
   - 条件：镜像未使用时间 > `ImageMaximumGCAge`
   - 行为：删除所有超龄未使用镜像
   - 注意：需开启 `ImageMaximumGCAge` Feature Gate

3. **驱逐管理器触发**
   - 条件：
     - `imagefs.available` < 硬/软驱逐阈值
     - `imagefs.inodesFree` < 硬/软驱逐阈值
     - `nodefs.available` < 硬/软驱逐阈值（无独立 imagefs 时）
     - `nodefs.inodesFree` < 硬/软驱逐阈值（无独立 imagefs 时）
   - 行为：删除**所有**未使用镜像（`DeleteUnusedImages`）

### Content Blob 丢失根因

1. **Kubelet 只管理 `k8s.io` namespace**
2. **运行中容器使用 snapshot，不直接引用镜像 content**
3. **镜像被删除后，如果没有其他 namespace 引用，content blob 被 containerd GC 删除**
4. **Commit 操作需要读取基础镜像 config，但 content 已丢失**

### 解决方案建议

1. **使用独立 namespace（如 `sealos.io`）** - 避免 Kubelet GC 影响
2. **在 commit 前检查并重新拉取镜像** - 确保 content 完整性
3. **调整 GC 阈值** - 提高触发阈值减少 GC 频率
4. **使用 pinned 镜像** - 标记为 pinned 的镜像不会被 GC