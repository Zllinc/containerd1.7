# 镜像 Tag 和 Digest 引用关系详解

## 一、如何知道 Digest 对应哪个带 Tag 的镜像？

### 1.1 通过 CRI API 查询

```bash
# 方法 1: 使用 crictl 查看镜像详细信息
crictl --runtime-endpoint unix:///run/containerd/containerd.sock inspecti <digest引用>

# 输出会包含 RepoTags 和 RepoDigests
# 例如：
# {
#   "image": {
#     "id": "sha256:65a7ca5a90f0c154d8a55c43936ce8a39a4cc2cab764e38e87bd868208d64ce6",
#     "repoTags": [
#       "sealos.hub:5000/devbox-test/concurrent-test-devbox-0:latest"
#     ],
#     "repoDigests": [
#       "sealos.hub:5000/devbox-test/concurrent-test-devbox-0@sha256:65a7ca5a90f0c154d8a55c43936ce8a39a4cc2cab764e38e87bd868208d64ce6"
#     ],
#     "pinned": true
#   }
# }
```

### 1.2 通过 containerd 查询

```bash
# 查看镜像的所有引用
ctr -n k8s.io images ls | grep <digest的前几位>

# 或者查看镜像详细信息（如果支持）
ctr -n k8s.io images info <digest引用>
```

### 1.3 代码实现

在 containerd CRI 插件中，tag 和 digest 的关系是通过 `image.References` 数组维护的：

```211:225:pkg/cri/server/image_pull.go
	repoDigest, repoTag := getRepoDigestAndTag(namedRef, image.Target().Digest, isSchema1)
	for _, r := range []string{imageID, repoTag, repoDigest} {
		if r == "" {
			continue
		}
		if err := c.createImageReference(ctx, r, image.Target(), labels); err != nil {
			return nil, fmt.Errorf("failed to create image reference %q: %w", r, err)
		}
		// Update image store to reflect the newest state in containerd.
		// No need to use `updateImage`, because the image reference must
		// have been managed by the cri plugin.
		if err := c.imageStore.Update(ctx, r); err != nil {
			return nil, fmt.Errorf("failed to update image store %q: %w", r, err)
		}
	}
```

**关键点**：
- 拉取镜像时，会创建**三个独立的引用**：
  1. `imageID`：镜像 config 的 digest（如 `sha256:65a7ca5a...`）
  2. `repoTag`：带 tag 的引用（如 `sealos.hub:5000/devbox-test/concurrent-test-devbox-0:latest`）
  3. `repoDigest`：带 digest 的引用（如 `sealos.hub:5000/devbox-test/concurrent-test-devbox-0@sha256:65a7ca5a...`）

- 这三个引用在 containerd 中是**独立的 Image 对象**，但它们：
  - 指向**同一个镜像内容**（相同的 `Target`）
  - 在 image store 中被**合并**到一个 `Image` 对象中（通过 `imageID` 作为键）

### 1.4 查看引用关系

```bash
# 通过 Go 代码查看
# image.References 数组包含所有引用：
# [
#   "sha256:65a7ca5a...",  // imageID
#   "sealos.hub:5000/devbox-test/concurrent-test-devbox-0:latest",  // repoTag
#   "sealos.hub:5000/devbox-test/concurrent-test-devbox-0@sha256:65a7ca5a..."  // repoDigest
# ]
```

---

## 二、Digest 引用也被 Pin 住是正常的吗？

**答案：是的，这是正常的！**

### 2.1 Pin 机制的工作原理

当你通过 devbox snapshotter 拉取镜像时：

```163:169:pkg/cri/server/image_pull.go
	// if snapshotter is devbox, add the pinned image label
	if snapshotter == "devbox" {
		if labels == nil {
			labels = map[string]string{}
		}
		labels[crilabels.PinnedImageLabelKey] = crilabels.PinnedImageLabelValue
	}
```

这个 `labels` 会被应用到**所有三个引用**上：

```216:216:pkg/cri/server/image_pull.go
		if err := c.createImageReference(ctx, r, image.Target(), labels); err != nil {
```

**关键代码**：

```211:225:pkg/cri/server/image_pull.go
	repoDigest, repoTag := getRepoDigestAndTag(namedRef, image.Target().Digest, isSchema1)
	for _, r := range []string{imageID, repoTag, repoDigest} {
		if r == "" {
			continue
		}
		if err := c.createImageReference(ctx, r, image.Target(), labels); err != nil {
			return nil, fmt.Errorf("failed to create image reference %q: %w", r, err)
		}
		// Update image store to reflect the newest state in containerd.
		// No need to use `updateImage`, because the image reference must
		// have been managed by the cri plugin.
		if err := c.imageStore.Update(ctx, r); err != nil {
			return nil, fmt.Errorf("failed to update image store %q: %w", r, err)
		}
	}
```

### 2.2 为什么 Digest 也被 Pin？

1. **所有引用共享相同的 labels**：
   - `imageID`、`repoTag`、`repoDigest` 三个引用都使用**相同的 `labels`**
   - 如果 `labels` 包含 `io.cri-containerd.pinned=pinned`，**所有三个引用都会被 pin**

2. **Image Store 的合并逻辑**：

```225:235:pkg/cri/store/image/image.go
	i, ok := s.images[img.ID]
	if !ok {
		// If the image doesn't exist, add it.
		s.images[img.ID] = img
		return nil
	}
	// Or else, merge and sort the references.
	i.References = docker.Sort(util.MergeStringSlices(i.References, img.References))
	i.Pinned = i.Pinned || img.Pinned
	s.images[img.ID] = i
	return nil
```

- 第 233 行：`i.Pinned = i.Pinned || img.Pinned`
- 这意味着：**只要有一个引用被 pin，整个镜像就被 pin**

3. **引用级别的 Pin 管理**：

```217:223:pkg/cri/store/image/image.go
	if img.Pinned {
		if refs := s.pinnedRefs[img.ID]; refs == nil {
			s.pinnedRefs[img.ID] = sets.New(img.References...)
		} else {
			refs.Insert(img.References...)
		}
	}
```

- 所有引用（包括 digest）都会被记录到 `pinnedRefs` 中

### 2.3 结论

**这是正常且正确的行为**：
- ✅ Tag 引用被 pin
- ✅ Digest 引用也被 pin
- ✅ ImageID 引用也被 pin

**原因**：
- 它们指向**同一个镜像内容**
- 应该**统一管理**，要么都 pin，要么都不 pin
- 这样可以确保无论通过哪种引用访问，镜像都不会被 GC

---

## 三、为什么删除 Tag 时，Digest 引用没有被自动删除？

### 3.1 删除逻辑分析

查看 `RemoveImage` 的实现：

```go 36:68:pkg/cri/server/image_remove.go
func (c *criService) RemoveImage(ctx context.Context, r *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	span := tracing.SpanFromContext(ctx)
	image, err := c.localResolve(r.GetImage().GetImage())
	if err != nil {
		if errdefs.IsNotFound(err) {
			span.AddEvent(err.Error())
			// return empty without error when image not found.
			return &runtime.RemoveImageResponse{}, nil
		}
		return nil, fmt.Errorf("can not resolve %q locally: %w", r.GetImage().GetImage(), err)
	}
	span.SetAttributes(tracing.Attribute("image.id", image.ID))
	// Remove all image references.
	for i, ref := range image.References {
		var opts []images.DeleteOpt
		if i == len(image.References)-1 {
			// Delete the last image reference synchronously to trigger garbage collection.
			// This is best effort. It is possible that the image reference is deleted by
			// someone else before this point.
			opts = []images.DeleteOpt{images.SynchronousDelete()}
		}
		err = c.client.ImageService().Delete(ctx, ref, opts...)
		if err == nil || errdefs.IsNotFound(err) {
			// Update image store to reflect the newest state in containerd.
			if err := c.imageStore.Update(ctx, ref); err != nil {
				return nil, fmt.Errorf("failed to update image reference %q for %q: %w", ref, image.ID, err)
			}
			continue
		}
		return nil, fmt.Errorf("failed to delete image reference %q for %q: %w", ref, image.ID, err)
	}
	return &runtime.RemoveImageResponse{}, nil
}
```

### 3.2 问题分析

**理论上应该删除所有引用**：

- 第 49 行：`for i, ref := range image.References` - 遍历所有引用
- 第 57 行：`c.client.ImageService().Delete(ctx, ref, opts...)` - 删除每个引用
- 第 60 行：`c.imageStore.Update(ctx, ref)` - 更新 image store

**但为什么 Digest 没有被删除？**

可能的原因：

#### 原因 1: Pinned 镜像的删除被阻止

如果镜像被 pin，Kubelet 的 GC 机制会**跳过删除操作**，因此 `RemoveImage` 根本不会被调用。

**验证方法**：

```bash
# 检查镜像是否真的被 pin
crictl --runtime-endpoint unix:///run/containerd/containerd.sock inspecti <tag引用> | jq '.image.pinned'

# 如果返回 true，说明镜像被 pin，Kubelet 不会调用 RemoveImage
```

#### 原因 2: 手动删除 Tag 时，Digest 引用独立存在

如果你**手动删除 tag 引用**（例如通过 `ctr images rm <tag>`），digest 引用是**独立的 containerd Image 对象**，不会被自动删除。

**原因**：
- Tag 和 Digest 在 containerd 中是**两个独立的 Image 对象**
- 删除一个不会影响另一个
- 只有通过 CRI `RemoveImage` API 删除时，才会遍历所有引用

#### 原因 3: Image Store 的引用管理

查看 `imageStore.Update` 的逻辑：

```96:125:pkg/cri/store/image/image.go
// update updates the internal cache. img == nil means that
// the image does not exist in containerd.
func (s *Store) update(ref string, img *Image) error {
	oldID, oldExist := s.refCache[ref]
	if img == nil {
		// The image reference doesn't exist in containerd.
		if oldExist {
			// Remove the reference from the store.
			s.store.delete(oldID, ref)
			delete(s.refCache, ref)
		}
		return nil
	}
	if oldExist {
		if oldID == img.ID {
			if s.store.isPinned(img.ID, ref) == img.Pinned {
				return nil
			}
			if img.Pinned {
				return s.store.pin(img.ID, ref)
			}
			return s.store.unpin(img.ID, ref)
		}
		// Updated. Remove tag from old image.
		s.store.delete(oldID, ref)
	}
	// New image. Add new image.
	s.refCache[ref] = img.ID
	return s.store.add(*img)
}
```

**关键点**：
- `s.store.delete(oldID, ref)` 只是从 `image.References` 数组中**移除引用**
- 但**不会删除 containerd 中的 Image 对象**

### 3.3 实际删除流程

```
用户删除 Tag 引用
    │
    ├─→ 通过 CRI RemoveImage API
    │   └─→ RemoveImage() 遍历 image.References
    │       ├─→ 删除 tag 引用 ✅
    │       ├─→ 删除 digest 引用 ✅
    │       └─→ 删除 imageID 引用 ✅
    │
    └─→ 直接通过 containerd API (ctr images rm)
        └─→ 只删除指定的引用 ❌
            └─→ 其他引用（digest）仍然存在
```

### 3.4 为什么会出现这种情况？

**场景 1: 通过 Kubelet GC 删除**

如果镜像被 pin，Kubelet 的 `imagesInEvictionOrder` 会**跳过这个镜像**，`RemoveImage` 根本不会被调用，所以所有引用都还在。

**场景 2: 手动删除 Tag**

```bash
# 手动删除 tag
ctr -n k8s.io images rm sealos.hub:5000/devbox-test/concurrent-test-devbox-0:latest

# Digest 引用仍然存在，因为它是独立的 Image 对象
ctr -n k8s.io images ls | grep @sha256:65a7ca5a
```

**场景 3: 部分引用被删除**

如果 `RemoveImage` 执行过程中，某些引用删除失败（例如权限问题、网络问题），可能导致部分引用被删除，部分引用还在。

### 3.5 如何验证和解决？

#### 验证方法

```bash
# 1. 查看镜像的所有引用
crictl --runtime-endpoint unix:///run/containerd/containerd.sock inspecti <tag引用> | jq '.image.repoTags, .image.repoDigests'

# 2. 检查 containerd 中的引用
ctr -n k8s.io images ls | grep -E "<tag>|<digest>"

# 3. 检查 image store 中的引用
# 需要通过代码或日志查看
```

#### 解决方案

**方案 1: 通过 CRI API 删除（推荐）**

```bash
# 使用 crictl 删除（会删除所有引用）
crictl --runtime-endpoint unix:///run/containerd/containerd.sock rmi <tag或digest>

# 这会调用 RemoveImage，删除所有引用
```

**方案 2: 手动删除所有引用**

```bash
# 先找到所有引用
IMAGE_ID=$(crictl --runtime-endpoint unix:///run/containerd/containerd.sock inspecti <tag> | jq -r '.image.id')

# 删除所有引用
ctr -n k8s.io images rm <tag引用>
ctr -n k8s.io images rm <digest引用>
ctr -n k8s.io images rm $IMAGE_ID
```

**方案 3: 修改代码确保完整删除**

如果需要确保删除 tag 时也删除 digest，可以在 `RemoveImage` 中添加额外的检查逻辑。

---

## 四、总结

### 4.1 问题 1: 如何知道 Digest 对应哪个 Tag？

**方法**：
1. 通过 CRI API：`crictl inspecti <digest>` 查看 `repoTags`
2. 通过代码：查看 `image.References` 数组
3. 通过 containerd：`ctr images ls` 查看所有引用

### 4.2 问题 2: Digest 也被 Pin 是正常的吗？

**答案：是的，正常！**

- 所有引用（tag、digest、imageID）共享相同的 labels
- 如果镜像被 pin，所有引用都会被 pin
- 这是正确的行为，确保镜像不会被 GC

### 4.3 问题 3: 为什么删除 Tag 时 Digest 没有被删除？

**可能原因**：
1. **镜像被 pin**：Kubelet GC 跳过了删除，`RemoveImage` 没有被调用
2. **手动删除**：直接通过 `ctr images rm` 删除 tag，digest 是独立对象，不会被删除
3. **部分删除失败**：`RemoveImage` 执行过程中某些引用删除失败

**解决方案**：
- 使用 CRI API 删除（`crictl rmi`），会删除所有引用
- 或者手动删除所有引用（tag、digest、imageID）

---

## 五、最佳实践

1. **统一使用 CRI API 删除镜像**：确保所有引用都被删除
2. **检查 Pinned 状态**：删除前确认镜像是否被 pin
3. **清理时检查所有引用**：确保 tag、digest、imageID 都被清理
4. **监控镜像引用**：定期检查是否有孤立的 digest 引用

