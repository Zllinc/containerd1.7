# Containerd Pin 镜像详解

## 一、Containerd 如何 Pin 镜像

### 1.1 Pin 机制的核心：Label 标签

Containerd 通过**镜像标签（Label）**来标记镜像是否被 pin。具体来说：

- **标签键（Key）**：`io.cri-containerd.pinned`
- **标签值（Value）**：`pinned`

定义位置：

```go 26:29:pkg/cri/labels/labels.go
	// PinnedImageLabelKey is the label value indicating the image is pinned.
	PinnedImageLabelKey = criContainerdPrefix + ".pinned"
	// PinnedImageLabelValue is the label value indicating the image is pinned.
	PinnedImageLabelValue = "pinned"
```

### 1.2 Pin 状态的读取

当 containerd CRI 插件需要获取镜像的 pinned 状态时，会从 containerd 镜像的 labels 中读取：

```go 152:152:pkg/cri/store/image/image.go
	pinned := i.Labels()[labels.PinnedImageLabelKey] == labels.PinnedImageLabelValue
```

**关键逻辑**：
- 从 containerd 镜像对象中获取 labels
- 检查 `io.cri-containerd.pinned` 标签是否存在且值为 `"pinned"`
- 如果匹配，则 `pinned = true`，否则 `pinned = false`

---

## 二、如何 Pin 镜像

### 方法 1：自动 Pin（Sandbox 镜像）

**Containerd 会自动 pin sandbox 镜像**（通常是 pause 镜像）。这是最常见的 pin 方式。

实现位置：

```go 311:327:pkg/cri/server/image_pull.go
// getLabels get image labels to be added on CRI image
func (c *criService) getLabels(ctx context.Context, name string) map[string]string {
	labels := map[string]string{crilabels.ImageLabelKey: crilabels.ImageLabelValue}
	configSandboxImage := c.config.SandboxImage
	// parse sandbox image
	sandboxNamedRef, err := distribution.ParseDockerRef(configSandboxImage)
	if err != nil {
		log.G(ctx).Errorf("failed to parse sandbox image from config %s", sandboxNamedRef)
		return nil
	}
	sandboxRef := sandboxNamedRef.String()
	// Adding pinned image label to sandbox image
	if sandboxRef == name {
		labels[crilabels.PinnedImageLabelKey] = crilabels.PinnedImageLabelValue
	}
	return labels
}
```

**工作原理**：
1. 在拉取镜像时，`getLabels` 函数会被调用
2. 函数检查镜像名称是否与配置的 `SandboxImage` 匹配
3. 如果匹配，则在 labels 中添加 `io.cri-containerd.pinned = "pinned"`
4. 这个标签会被保存到 containerd 的镜像元数据中

### 方法 2：手动 Pin（通过 containerd API）

如果你想手动 pin 其他镜像，可以通过 **containerd 的 ImageService API** 直接给镜像添加 label。

#### 使用 ctr 命令行工具

```bash
# 1. 查看镜像的当前 labels
ctr images inspect <镜像引用>

# 2. 给镜像添加 pinned 标签
ctr images label <镜像引用> io.cri-containerd.pinned=pinned

# 示例：pin nginx 镜像
ctr images label docker.io/library/nginx:latest io.cri-containerd.pinned=pinned
```

#### 使用 containerd Go Client API

```go
import (
    "context"
    "github.com/containerd/containerd"
    "github.com/containerd/containerd/pkg/cri/labels"
)

func pinImage(ctx context.Context, client *containerd.Client, imageRef string) error {
    // 获取镜像
    img, err := client.ImageService().Get(ctx, imageRef)
    if err != nil {
        return err
    }
    
    // 添加 pinned 标签
    if img.Labels == nil {
        img.Labels = make(map[string]string)
    }
    img.Labels[labels.PinnedImageLabelKey] = labels.PinnedImageLabelValue
    
    // 更新镜像
    _, err = client.ImageService().Update(ctx, img, "labels."+labels.PinnedImageLabelKey)
    return err
}
```

### 方法 3：修改 containerd 配置

虽然不能直接通过配置 pin 镜像，但可以修改代码逻辑，在特定条件下自动 pin 镜像。

例如，可以在 `getLabels` 函数中添加自定义逻辑：

```go
func (c *criService) getLabels(ctx context.Context, name string) map[string]string {
    labels := map[string]string{crilabels.ImageLabelKey: crilabels.ImageLabelValue}
    
    // 自动 pin sandbox 镜像
    configSandboxImage := c.config.SandboxImage
    sandboxNamedRef, err := distribution.ParseDockerRef(configSandboxImage)
    if err == nil {
        sandboxRef := sandboxNamedRef.String()
        if sandboxRef == name {
            labels[crilabels.PinnedImageLabelKey] = crilabels.PinnedImageLabelValue
        }
    }
    
    // 自定义：pin 特定镜像
    if strings.HasPrefix(name, "my-registry.com/critical/") {
        labels[crilabels.PinnedImageLabelKey] = crilabels.PinnedImageLabelValue
    }
    
    return labels
}
```

---

## 三、Pin 镜像的完整流程

### 3.1 流程图

```
┌─────────────────────────────────────────────────────────────────┐
│ 1. 镜像拉取阶段 (PullImage)                                      │
└─────────────────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ getLabels() 检查镜像名称                                         │
│ - 如果是 sandbox 镜像 → 添加 pinned 标签                        │
│ - 或者手动添加 pinned 标签                                      │
└─────────────────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ createImageReference() 创建/更新镜像引用                         │
│ - 将 labels（包含 pinned）保存到 containerd ImageService        │
└─────────────────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ 2. 镜像存储阶段 (ImageStore.Update)                              │
└─────────────────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ getImage() 从 containerd 读取镜像信息                            │
│ - 读取镜像 labels                                                │
│ - 检查 io.cri-containerd.pinned == "pinned"                    │
│ - 设置 Image.Pinned = true/false                                │
└─────────────────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ imageStore.update() 更新内部缓存                                 │
│ - 将 Image 对象添加到 store.images                              │
│ - 如果 Pinned=true，记录到 store.pinnedRefs                      │
└─────────────────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ 3. CRI API 查询阶段 (ListImages/ImageStatus)                     │
└─────────────────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ imageStore.List() 获取所有镜像                                   │
│ - 返回包含 Pinned 字段的 Image 列表                             │
└─────────────────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ toCRIImage() 转换为 CRI API 格式                                 │
│ - 将内部 Image.Pinned 复制到 runtime.Image.Pinned               │
└─────────────────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ 4. Kubelet 接收阶段                                              │
└─────────────────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ Kubelet 通过 CRI API 获取镜像列表                                │
│ - 每个 Image 对象包含 Pinned 字段                                │
└─────────────────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ detectImages() 更新 imageRecords                                 │
│ - imageRecord.pinned = image.Pinned                             │
└─────────────────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ imagesInEvictionOrder() 筛选可删除镜像                           │
│ - 跳过 record.pinned == true 的镜像                              │
└─────────────────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────────────────┐
│ ✅ Pinned 镜像永远不会被垃圾回收                                 │
└─────────────────────────────────────────────────────────────────┘
```

### 3.2 详细代码流程

#### 阶段 1：镜像拉取时设置 Pin 标签

**位置**：`pkg/cri/server/image_pull.go`

```312:327:pkg/cri/server/image_pull.go
func (c *criService) getLabels(ctx context.Context, name string) map[string]string {
	labels := map[string]string{crilabels.ImageLabelKey: crilabels.ImageLabelValue}
	configSandboxImage := c.config.SandboxImage
	// parse sandbox image
	sandboxNamedRef, err := distribution.ParseDockerRef(configSandboxImage)
	if err != nil {
		log.G(ctx).Errorf("failed to parse sandbox image from config %s", sandboxNamedRef)
		return nil
	}
	sandboxRef := sandboxNamedRef.String()
	// Adding pinned image label to sandbox image
	if sandboxRef == name {
		labels[crilabels.PinnedImageLabelKey] = crilabels.PinnedImageLabelValue
	}
	return labels
}
```

**关键点**：
- 在拉取镜像时，`getLabels` 会被调用
- 如果镜像名称匹配 `SandboxImage` 配置，自动添加 pinned 标签
- 标签会被传递给 `createImageReference`，保存到 containerd

#### 阶段 2：从 containerd 读取 Pin 状态

**位置**：`pkg/cri/store/image/image.go`

```127:163:pkg/cri/store/image/image.go
// getImage gets image information from containerd.
func getImage(ctx context.Context, i containerd.Image) (*Image, error) {
	// Get image information.
	diffIDs, err := i.RootFS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get image diffIDs: %w", err)
	}
	chainID := imageidentity.ChainID(diffIDs)

	size, err := i.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("get image compressed resource size: %w", err)
	}

	desc, err := i.Config(ctx)
	if err != nil {
		return nil, fmt.Errorf("get image config descriptor: %w", err)
	}
	id := desc.Digest.String()

	spec, err := i.Spec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get OCI image spec: %w", err)
	}

	pinned := i.Labels()[labels.PinnedImageLabelKey] == labels.PinnedImageLabelValue

	return &Image{
		ID:         id,
		References: []string{i.Name()},
		ChainID:    chainID.String(),
		Size:       size,
		ImageSpec:  spec,
		Pinned:     pinned,
	}, nil

}
```

**关键点**：
- 第 152 行：从 containerd 镜像的 labels 中读取 pinned 状态
- 检查 `io.cri-containerd.pinned == "pinned"`
- 将结果存储到内部 `Image` 结构的 `Pinned` 字段

#### 阶段 3：内部存储管理

**位置**：`pkg/cri/store/image/image.go`

```205:236:pkg/cri/store/image/image.go
func (s *store) add(img Image) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if _, err := s.digestSet.Lookup(img.ID); err != nil {
		if err != digestset.ErrDigestNotFound {
			return err
		}
		if err := s.digestSet.Add(imagedigest.Digest(img.ID)); err != nil {
			return err
		}
	}

	if img.Pinned {
		if refs := s.pinnedRefs[img.ID]; refs == nil {
			s.pinnedRefs[img.ID] = sets.New(img.References...)
		} else {
			refs.Insert(img.References...)
		}
	}

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
}
```

**关键点**：
- 第 217-223 行：如果镜像被 pin，记录到 `pinnedRefs` 映射中
- 第 233 行：合并多个引用时，使用 `||` 逻辑（只要有一个引用被 pin，整个镜像就被 pin）
- `pinnedRefs` 用于跟踪哪些镜像引用（reference）被 pin

#### 阶段 4：转换为 CRI API 格式

**位置**：`pkg/cri/server/image_list.go` 和 `pkg/cri/server/image_status.go`

```28:39:pkg/cri/server/image_list.go
func (c *criService) ListImages(ctx context.Context, r *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	imagesInStore := c.imageStore.List()

	var images []*runtime.Image
	for _, image := range imagesInStore {
		// TODO(random-liu): [P0] Make sure corresponding snapshot exists. What if snapshot
		// doesn't exist?
		images = append(images, toCRIImage(image))
	}

	return &runtime.ListImagesResponse{Images: images}, nil
}
```

```64:82:pkg/cri/server/image_status.go
// toCRIImage converts internal image object to CRI runtime.Image.
func toCRIImage(image imagestore.Image) *runtime.Image {
	repoTags, repoDigests := parseImageReferences(image.References)

	runtimeImage := &runtime.Image{
		Id:          image.ID,
		RepoTags:    repoTags,
		RepoDigests: repoDigests,
		Size_:       uint64(image.Size),
		Pinned:      image.Pinned,
	}

	uid, username := getUserFromImage(image.ImageSpec.Config.User)
	if uid != nil {
		runtimeImage.Uid = &runtime.Int64Value{Value: *uid}
	}
	runtimeImage.Username = username

	return runtimeImage
}
```

**关键点**：
- `ListImages` 从 `imageStore` 获取所有镜像
- `toCRIImage` 将内部 `Image.Pinned` 字段复制到 CRI API 的 `runtime.Image.Pinned` 字段（第 72 行）
- Kubelet 通过 CRI API 调用 `ListImages` 时，会收到包含 `Pinned` 字段的镜像列表

#### 阶段 5：Kubelet 处理 Pin 镜像

根据你提供的文档，Kubelet 的处理流程：

1. **detectImages()**：从 CRI API 获取镜像列表，读取 `Image.Pinned` 字段，更新到 `imageRecord.pinned`
2. **imagesInEvictionOrder()**：筛选可删除镜像时，跳过 `record.pinned == true` 的镜像
3. **结果**：Pinned 镜像永远不会出现在可删除列表中，因此不会被垃圾回收

---

## 四、Pin 镜像的引用级别管理

Containerd 的 pin 机制支持**引用级别（reference-level）**的 pin，这意味着：

- 同一个镜像可能有多个引用（如 `nginx:latest` 和 `nginx:1.21`）
- 可以只 pin 其中一个引用，而不影响其他引用
- 只有当**所有引用都被 unpin** 时，镜像才会被 unpin

### 4.1 Pin 引用管理

```238:247:pkg/cri/store/image/image.go
func (s *store) isPinned(id, ref string) bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	digest, err := s.digestSet.Lookup(id)
	if err != nil {
		return false
	}
	refs := s.pinnedRefs[digest.String()]
	return refs != nil && refs.Has(ref)
}
```

**关键点**：
- `isPinned(id, ref)` 检查特定镜像的特定引用是否被 pin
- `pinnedRefs` 是一个映射：`map[imageID]Set[reference]`
- 可以精确跟踪哪些引用被 pin

### 4.2 Pin/Unpin 操作

```go 249:272:pkg/cri/store/image/image.go
func (s *store) pin(id, ref string) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	digest, err := s.digestSet.Lookup(id)
	if err != nil {
		if err == digestset.ErrDigestNotFound {
			err = errdefs.ErrNotFound
		}
		return err
	}
	i, ok := s.images[digest.String()]
	if !ok {
		return errdefs.ErrNotFound
	}

	if refs := s.pinnedRefs[digest.String()]; refs == nil {
		s.pinnedRefs[digest.String()] = sets.New(ref)
	} else {
		refs.Insert(ref)
	}
	i.Pinned = true
	s.images[digest.String()] = i
	return nil
}
```

```274:303:pkg/cri/store/image/image.go
func (s *store) unpin(id, ref string) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	digest, err := s.digestSet.Lookup(id)
	if err != nil {
		if err == digestset.ErrDigestNotFound {
			err = errdefs.ErrNotFound
		}
		return err
	}
	i, ok := s.images[digest.String()]
	if !ok {
		return errdefs.ErrNotFound
	}

	refs := s.pinnedRefs[digest.String()]
	if refs == nil {
		return nil
	}
	if refs.Delete(ref); len(refs) > 0 {
		return nil
	}

	// delete unpinned image, we only need to keep the pinned
	// entries in the map
	delete(s.pinnedRefs, digest.String())
	i.Pinned = false
	s.images[digest.String()] = i
	return nil
}
```

**关键逻辑**：
- **pin**：将引用添加到 `pinnedRefs`，设置 `Image.Pinned = true`
- **unpin**：从 `pinnedRefs` 中删除引用，只有当所有引用都被删除时，才设置 `Image.Pinned = false`

---

## 五、实际使用示例

### 5.1 查看镜像的 Pin 状态

```bash
# 使用 ctr 查看镜像信息
ctr images inspect docker.io/library/nginx:latest

# 输出中会显示 labels：
# "io.cri-containerd.pinned": "pinned"
```

### 5.2 手动 Pin 镜像

```bash
# 方法 1：使用 ctr 命令
ctr images label docker.io/library/nginx:latest io.cri-containerd.pinned=pinned

# 方法 2：使用 containerd API（需要重启 CRI 插件才能生效）
# 或者等待下一次镜像更新时自动同步
```

### 5.3 验证 Pin 是否生效

```bash
# 1. 通过 CRI API 查询（需要 CRI 客户端工具）
# 2. 通过 Kubelet 日志查看垃圾回收过程
# 3. 观察镜像是否在垃圾回收时被跳过
```

---

## 六、Pin 机制的作用范围和限制

### 6.1 重要理解：Pin 机制是 Kubelet 层面的保护

**关键点**：`io.cri-containerd.pinned` 标签**主要是为 Kubelet 提供保护**，**不直接保护镜像不被 containerd 的 GC 删除**。

### 6.2 Containerd 核心 GC 机制

Containerd 核心有自己的垃圾回收机制，但它**不检查 `io.cri-containerd.pinned` 标签**。Containerd 的 GC 基于：

1. **Lease（租约）机制**：
   - 资源必须被 lease 持有，否则会被 GC
   - 镜像如果被容器使用，会通过容器→快照→镜像的引用链保护

2. **资源引用关系**：
   - 通过 labels 如 `containerd.io/gc.ref.*` 定义引用关系
   - 如果镜像被容器、快照或其他资源引用，不会被 GC

3. **支持的 GC 标签**（来自 `docs/garbage-collection.md`）：
   - `containerd.io/gc.root`：标记根对象，保护其引用的所有资源
   - `containerd.io/gc.ref.content`：标记内容引用关系
   - `containerd.io/gc.ref.snapshot.<snapshotter>`：标记快照引用关系
   - **注意**：`io.cri-containerd.pinned` **不在** containerd 核心 GC 支持的标签列表中

### 6.3 Pin 机制的实际保护方式

虽然 containerd 核心不检查 pinned 标签，但 pinned 镜像通常仍然受到保护，原因如下：

#### 方式 1：通过 Kubelet 保护（主要方式）

```
Kubelet 垃圾回收
    │
    ├─→ 调用 CRI API: ListImages()
    │
    ├─→ Containerd CRI 插件返回: Image.Pinned = true
    │
    ├─→ Kubelet: imagesInEvictionOrder() 跳过 pinned 镜像
    │
    └─→ Kubelet 不会调用 RemoveImage() ✅
            │
            └─→ Containerd 不会收到删除请求
                    │
                    └─→ 镜像不会被删除 ✅
```

**关键**：Kubelet 是镜像删除的主要入口。如果 Kubelet 不调用 `RemoveImage`，镜像就不会被删除。

#### 方式 2：通过容器引用保护（间接保护）

如果 pinned 镜像正在被容器使用：

```
容器 → 快照 → 镜像
  │
  └─→ Containerd GC 检测到引用关系
      │
      └─→ 不会删除被引用的镜像 ✅
```

**注意**：这种保护是**间接的**，依赖于镜像被容器使用。如果镜像没有被任何容器使用，且没有 lease 保护，理论上 containerd 的 GC 可能会删除它。

### 6.4 潜在的风险场景

**场景 1：直接通过 containerd API 删除**

```bash
# 如果直接通过 containerd API 删除镜像，pinned 标签不会阻止删除
ctr images rm <镜像引用>

# 或者通过 ImageService.Delete()
client.ImageService().Delete(ctx, imageRef)
```

**场景 2：Containerd GC 清理未使用的镜像**

如果镜像：
- 没有被任何容器使用
- 没有被 lease 持有
- 没有其他资源引用

那么即使有 `io.cri-containerd.pinned` 标签，containerd 的 GC **理论上**可能会删除它（虽然实际中很少发生，因为 CRI 插件管理的镜像通常会被正确引用）。

### 6.5 如何确保镜像真正被保护？

1. **依赖 Kubelet 保护**（推荐）：
   - Pin 标签确保 Kubelet 不会删除镜像
   - 这是最常见和有效的保护方式

2. **确保镜像被使用**：
   - 如果镜像被容器使用，containerd GC 会通过引用关系保护它
   - 这是最可靠的保护方式

3. **使用 Lease**（高级用法）：
   ```go
   // 创建 lease 并持有镜像
   ctx, done, err := client.WithLease(ctx)
   defer done(ctx)
   // 拉取镜像，镜像会被 lease 保护
   ```

### 6.6 总结对比

| 保护机制 | 作用层面 | 检查 pinned 标签 | 保护方式 |
|---------|---------|-----------------|---------|
| **Kubelet GC** | Kubelet | ✅ 是 | 跳过 pinned 镜像，不调用 RemoveImage |
| **Containerd GC** | Containerd 核心 | ❌ 否 | 基于 lease 和引用关系 |
| **容器引用** | Containerd 核心 | ❌ 否 | 通过资源引用链保护 |

**结论**：
- **Pin 机制主要是 Kubelet 层面的保护**
- Containerd 核心 GC **不检查** pinned 标签
- 但在实际使用中，pinned 镜像通常仍然安全，因为：
  1. Kubelet 是主要的删除入口
  2. 被使用的镜像会被引用关系保护
  3. CRI 插件管理的镜像通常有正确的引用

---

## 七、总结

### 7.1 Pin 机制的关键点

1. **存储方式**：使用 containerd 镜像的 labels（`io.cri-containerd.pinned = "pinned"`）
2. **自动 Pin**：Sandbox 镜像（pause 镜像）会自动被 pin
3. **手动 Pin**：可以通过 containerd API 或 ctr 命令手动添加标签
4. **引用级别**：支持按引用（reference）级别 pin，而不是整个镜像
5. **CRI 传递**：Pin 状态通过 CRI API 的 `Image.Pinned` 字段传递给 Kubelet
6. **保护机制**：Kubelet 会跳过所有 pinned 镜像的垃圾回收
7. **作用范围**：**主要是 Kubelet 层面的保护**，containerd 核心 GC 不检查此标签

### 7.2 与 Kubelet 的协作

```
Containerd (CRI Plugin)
    │
    ├─→ 镜像 labels: io.cri-containerd.pinned = "pinned"
    │
    ├─→ ImageStore: Image.Pinned = true
    │
    ├─→ CRI API: runtime.Image.Pinned = true
    │
    └─→ Kubelet: imageRecord.pinned = true
            │
            └─→ imagesInEvictionOrder(): 跳过 pinned 镜像 ✅
```

### 7.3 最佳实践

1. **系统关键镜像**：确保 pause 镜像等关键镜像被 pin（默认已自动 pin）
2. **自定义镜像**：对于重要的业务镜像，可以手动 pin
3. **监控**：定期检查哪些镜像被 pin，避免过度 pin 导致磁盘空间不足
4. **清理**：不再需要 pin 的镜像，及时 unpin

---

## 八、相关代码文件

- **标签定义**：`pkg/cri/labels/labels.go`
- **Pin 状态读取**：`pkg/cri/store/image/image.go` (getImage 函数)
- **自动 Pin 逻辑**：`pkg/cri/server/image_pull.go` (getLabels 函数)
- **CRI API 转换**：`pkg/cri/server/image_list.go`, `pkg/cri/server/image_status.go`
- **内部存储管理**：`pkg/cri/store/image/image.go` (store 结构和方法)

