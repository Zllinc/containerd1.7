# 镜像垃圾回收函数详细解析

## 一、`imagesInEvictionOrder` 函数详细解析

**函数签名**：
```go
func (im *realImageGCManager) imagesInEvictionOrder(ctx context.Context, freeTime time.Time) ([]evictionInfo, error)
```

**函数位置**：`pkg/kubelet/images/image_gc_manager.go:548-592`

### 逐行代码解析

```548:592:pkg/kubelet/images/image_gc_manager.go
// Queries all of the image records and arranges them in a slice of evictionInfo, sorted based on last time used, ignoring images pinned by the runtime.
func (im *realImageGCManager) imagesInEvictionOrder(ctx context.Context, freeTime time.Time) ([]evictionInfo, error) {
```

**第548行**：函数注释说明
- 查询所有镜像记录
- 将它们排列成 `evictionInfo` 切片
- 按最后使用时间排序
- 忽略被运行时 pin 的镜像

**第549行**：检查特性开关
```go
isRuntimeClassInImageCriAPIEnabled := utilfeature.DefaultFeatureGate.Enabled(features.RuntimeClassInImageCriAPI)
```
- 检查 `RuntimeClassInImageCriAPI` 特性是否启用
- 如果启用，镜像记录使用 `(imageID, runtimeHandler)` 元组作为键
- 如果未启用，仅使用 `imageID` 作为键

**第550行**：检测当前使用的镜像
```go
imagesInUse, err := im.detectImages(ctx, freeTime)
```
- 调用 `detectImages` 函数，检测哪些镜像正在被容器使用
- `freeTime` 作为检测时间点传入
- 返回 `imagesInUse` 集合（包含所有正在使用的镜像ID）
- 如果出错，返回错误

**第551-553行**：错误处理
```go
if err != nil {
    return nil, err
}
```
- 如果 `detectImages` 失败，直接返回错误
- 返回 `nil` 切片和错误信息

**第555-556行**：加锁保护
```go
im.imageRecordsLock.Lock()
defer im.imageRecordsLock.Unlock()
```
- 获取 `imageRecords` 的互斥锁
- 使用 `defer` 确保函数返回时释放锁
- 防止并发访问导致的数据竞争

**第557行**：获取日志记录器
```go
logger := klog.FromContext(ctx)
```
- 从上下文获取结构化日志记录器
- 用于后续的日志输出

**第560行**：初始化结果切片
```go
images := make([]evictionInfo, 0, len(im.imageRecords))
```
- 创建 `evictionInfo` 类型的切片
- 初始长度为 0，容量为 `imageRecords` 的长度
- 预分配容量可以避免多次内存重新分配，提高性能

**第561行**：遍历所有镜像记录
```go
for image, record := range im.imageRecords {
```
- `image`：镜像的键（可能是 `imageID` 或 `(imageID, runtimeHandler)` 元组）
- `record`：镜像记录指针，包含镜像的元数据（大小、使用时间、pin 状态等）

**第562-565行**：跳过正在使用的镜像
```go
if isImageUsed(image, imagesInUse) {
    logger.V(5).Info("Image ID is being used", "imageID", image)
    continue
}
```
- `isImageUsed` 检查镜像是否在 `imagesInUse` 集合中
- 如果镜像正在被容器使用，记录调试日志（V(5) 级别）
- `continue` 跳过当前镜像，不加入可删除列表
- **原因**：正在使用的镜像不能被删除，否则会导致容器运行失败

**第566-570行**：跳过被 pin 的镜像
```go
// Check if image is pinned, prevent garbage collection
if record.pinned {
    logger.V(5).Info("Image is pinned, skipping garbage collection", "imageID", image)
    continue
}
```
- 检查镜像的 `pinned` 字段
- 如果镜像被 pin，记录调试日志
- `continue` 跳过当前镜像
- **原因**：被 pin 的镜像通常是系统关键镜像（如 pause 镜像），需要永久保留

**第572-576行**：处理未启用 RuntimeClassInImageCriAPI 的情况
```go
if !isRuntimeClassInImageCriAPIEnabled {
    images = append(images, evictionInfo{
        id:          image,
        imageRecord: *record,
    })
```
- 如果特性未启用，直接使用 `image` 作为镜像ID
- 创建 `evictionInfo` 结构体：
  - `id`：镜像ID（直接使用 `image` 键）
  - `imageRecord`：镜像记录的副本（解引用指针）
- 添加到 `images` 切片

**第577-588行**：处理启用 RuntimeClassInImageCriAPI 的情况
```go
} else {
    imageID := getImageIDFromTuple(image)
    // Ensure imageID is valid or else continue
    if imageID == "" {
        im.recorder.Eventf(im.nodeRef, v1.EventTypeWarning, "ImageID is not valid, skipping, ImageID: %v", imageID)
        continue
    }
    images = append(images, evictionInfo{
        id:          imageID,
        imageRecord: *record,
    })
}
```
- **第578行**：从元组中提取镜像ID
  - `image` 可能是 `"imageID,runtimeHandler"` 格式
  - `getImageIDFromTuple` 提取出 `imageID` 部分
- **第580-583行**：验证镜像ID有效性
  - 如果提取的 `imageID` 为空，记录警告事件
  - `continue` 跳过无效镜像
  - **原因**：无效的镜像ID无法用于删除操作
- **第584-587行**：创建 `evictionInfo`
  - `id` 使用提取的 `imageID`（不包含 runtimeHandler）
  - `imageRecord` 包含完整的镜像记录

**第590行**：排序
```go
sort.Sort(byLastUsedAndDetected(images))
```
- 使用 `byLastUsedAndDetected` 排序器对镜像进行排序
- 排序规则（见 `byLastUsedAndDetected.Less` 方法）：
  1. **主要排序**：按 `lastUsed` 时间升序（最久未使用的在前）
  2. **次要排序**：如果 `lastUsed` 相同，按 `firstDetected` 时间升序
- **目的**：实现 LRU（Least Recently Used）策略，优先删除最久未使用的镜像

**第591行**：返回结果
```go
return images, nil
```
- 返回排序后的可删除镜像列表
- 返回 `nil` 表示没有错误

---

## 二、`freeSpace` 函数详细解析

**函数签名**：
```go
func (im *realImageGCManager) freeSpace(ctx context.Context, bytesToFree int64, freeTime time.Time, images []evictionInfo) ([]string, int64, error)
```

**函数位置**：`pkg/kubelet/images/image_gc_manager.go:482-521`

### 函数注释说明

```476:481:pkg/kubelet/images/image_gc_manager.go
// Tries to free bytesToFree worth of images on the disk.
//
// Returns the images that are still available after the cleanup, the number of bytes freed
// and an error if any occurred. The number of bytes freed is always returned.
// Note that error may be nil and the number of bytes free may be less
// than bytesToFree.
```

- **目标**：尝试释放 `bytesToFree` 字节的磁盘空间
- **返回值**：
  1. `[]string`：清理后剩余的镜像ID列表
  2. `int64`：实际释放的字节数（即使出错也会返回）
  3. `error`：错误信息（可能为 `nil`）
- **注意**：即使没有错误，释放的空间也可能少于 `bytesToFree`

### 逐行代码解析

**第483行**：函数注释
```go
// Delete unused images until we've freed up enough space.
```
- 说明函数目的：删除未使用的镜像直到释放足够的空间

**第484行**：初始化错误收集器
```go
var deletionErrors []error
```
- 创建错误切片，用于收集删除镜像时遇到的所有错误
- **原因**：即使某些镜像删除失败，也要继续尝试删除其他镜像

**第485行**：获取日志记录器
```go
logger := klog.FromContext(ctx)
```
- 从上下文获取结构化日志记录器

**第486行**：初始化释放空间计数器
```go
spaceFreed := int64(0)
```
- 初始化为 0，用于累计实际释放的磁盘空间

**第487行**：初始化剩余镜像列表
```go
var imagesLeft []string
```
- 创建字符串切片，用于存储未被删除的镜像ID
- 包括：不符合删除条件的镜像、删除失败的镜像

**第488行**：遍历已排序的镜像列表
```go
for _, image := range images {
```
- `images` 是 `imagesInEvictionOrder` 返回的已排序列表
- 按 LRU 顺序遍历（最久未使用的在前）

**第489行**：记录评估日志
```go
logger.V(5).Info("Evaluating image ID for possible garbage collection based on disk usage", "imageID", image.id, "runtimeHandler", image.runtimeHandlerUsedToPullImage)
```
- 记录调试级别日志（V(5)）
- 输出镜像ID和运行时处理器信息
- 用于追踪垃圾回收决策过程

**第491-495行**：检查镜像最近使用时间
```go
// Images that are currently in used were given a newer lastUsed.
if image.lastUsed.Equal(freeTime) || image.lastUsed.After(freeTime) {
    imagesLeft = append(imagesLeft, image.id)
    logger.V(5).Info("Image ID was used too recently, not eligible for garbage collection", "imageID", image.id, "lastUsed", image.lastUsed, "freeTime", freeTime)
    continue
}
```
- **第491行注释**：说明正在使用的镜像会被赋予更新的 `lastUsed` 时间
- **第492行**：检查条件
  - `image.lastUsed.Equal(freeTime)`：最后使用时间等于当前时间
  - `image.lastUsed.After(freeTime)`：最后使用时间晚于当前时间
  - **两种情况都表示镜像最近被使用过**
- **第493行**：将镜像ID加入剩余列表
- **第494行**：记录跳过原因
- **第495行**：`continue` 跳过当前镜像
- **原因**：防止删除刚被标记为"使用中"的镜像，避免竞态条件

**第497-503行**：检查镜像最小年龄
```go
// Avoid garbage collect the image if the image is not old enough.
// In such a case, the image may have just been pulled down, and will be used by a container right away.
if freeTime.Sub(image.firstDetected) < im.policy.MinAge {
    imagesLeft = append(imagesLeft, image.id)
    logger.V(5).Info("Image ID's age is less than the policy's minAge, not eligible for garbage collection", "imageID", image.id, "age", freeTime.Sub(image.firstDetected), "minAge", im.policy.MinAge)
    continue
}
```
- **第497-499行注释**：说明目的
  - 避免删除太新的镜像
  - 新拉取的镜像可能很快被容器使用
- **第499行**：计算镜像年龄
  - `freeTime.Sub(image.firstDetected)`：当前时间减去首次检测时间
  - 如果年龄小于 `MinAge`（默认 2 分钟），则跳过
- **第500行**：加入剩余列表
- **第501行**：记录详细信息（镜像ID、实际年龄、最小年龄要求）
- **第502行**：`continue` 跳过
- **原因**：防止删除刚拉取的镜像，给容器启动留出时间

**第505-509行**：尝试删除镜像
```go
if err := im.freeImage(ctx, image, ImageGarbageCollectedTotalReasonSpace); err != nil {
    deletionErrors = append(deletionErrors, err)
    imagesLeft = append(imagesLeft, image.id)
    continue
}
```
- **第505行**：调用 `freeImage` 删除镜像
  - `ImageGarbageCollectedTotalReasonSpace`：删除原因（因为空间不足）
  - 返回错误如果删除失败
- **第506行**：如果出错，将错误加入错误列表
- **第507行**：将镜像ID加入剩余列表（删除失败，镜像仍在）
- **第508行**：`continue` 继续处理下一个镜像
- **原因**：即使某个镜像删除失败，也要继续尝试删除其他镜像

**第510行**：累计释放的空间
```go
spaceFreed += image.size
```
- 删除成功后，将镜像大小累加到 `spaceFreed`
- `image.size` 是镜像占用的字节数

**第512-514行**：检查是否达到目标
```go
if spaceFreed >= bytesToFree {
    break
}
```
- 如果已释放的空间达到或超过目标值，跳出循环
- **优化**：不需要删除所有可删除的镜像，达到目标即可停止

**第517-519行**：处理删除错误
```go
if len(deletionErrors) > 0 {
    return nil, spaceFreed, fmt.Errorf("wanted to free %d bytes, but freed %d bytes space with errors in image deletion: %w", bytesToFree, spaceFreed, errors.NewAggregate(deletionErrors))
}
```
- **第517行**：检查是否有删除错误
- **第518行**：如果有错误，返回：
  - `nil`：剩余镜像列表（因为出错，不返回部分结果）
  - `spaceFreed`：实际释放的字节数
  - 错误信息：包含目标字节数、实际释放字节数、所有删除错误的聚合

**第520行**：正常返回
```go
return imagesLeft, spaceFreed, nil
```
- 如果没有错误，返回：
  - `imagesLeft`：未被删除的镜像ID列表
  - `spaceFreed`：实际释放的字节数
  - `nil`：无错误

---

## 三、两个函数的区别总结

| 维度 | `imagesInEvictionOrder` | `freeSpace` |
|------|------------------------|-------------|
| **阶段** | 准备/筛选阶段 | 执行/删除阶段 |
| **主要操作** | 只读操作（筛选、排序） | 写入操作（删除镜像） |
| **输入参数** | `ctx`, `freeTime` | `ctx`, `bytesToFree`, `freeTime`, `images` |
| **筛选条件** | 1. 未使用<br>2. 未 pin | 1. 最近未使用<br>2. 满足最小年龄 |
| **排序** | ✅ 按 LRU 排序 | ❌ 使用已排序的列表 |
| **删除操作** | ❌ 不删除 | ✅ 调用 `freeImage` 删除 |
| **空间计算** | ❌ 不计算 | ✅ 累计释放的空间 |
| **停止条件** | ❌ 处理所有镜像 | ✅ 达到 `bytesToFree` 即停止 |
| **返回值** | `[]evictionInfo`, `error` | `[]string`, `int64`, `error` |
| **调用关系** | 被 `freeSpace` 的调用者使用 | 使用 `imagesInEvictionOrder` 的结果 |

---

## 四、Pin 镜像详解

### 4.1 Pin 镜像的作用

**Pin（固定）镜像**是容器运行时标记为**不应被垃圾回收**的镜像。这些镜像通常是：

1. **系统关键镜像**：如 Kubernetes 的 `pause` 镜像（所有 Pod 的基础镜像）
2. **运行时依赖镜像**：容器运行时自身需要的镜像
3. **用户显式标记的镜像**：通过运行时 API 标记的镜像

### 4.2 Pin 状态的数据流

```
容器运行时 (containerd/CRI-O)
    ↓
CRI API: ListImages() 响应
    ↓ Image.Pinned = true/false
Kubelet: kuberuntime.ListImages()
    ↓ container.Image.Pinned
Kubelet: detectImages()
    ↓ imageRecord.pinned = image.Pinned
Kubelet: imagesInEvictionOrder()
    ↓ if record.pinned { continue }
垃圾回收跳过该镜像 ✅
```

### 4.3 Pin 状态的来源

**Pin 状态由容器运行时决定**，通过 CRI (Container Runtime Interface) API 返回：

```1593:1596:staging/src/k8s.io/cri-api/pkg/apis/runtime/v1/api.proto
    // Recommendation on whether this image should be exempt from garbage collection.
    // It must only be treated as a recommendation -- the client can still request that the image be deleted,
    // and the runtime must oblige.
    bool pinned = 8;
```

**关键点**：
- `pinned` 字段是**运行时给 Kubelet 的建议**
- Kubelet **必须尊重**这个建议（代码中会跳过 pinned 镜像）
- 但理论上，Kubelet 仍可以强制删除（虽然代码中没有这样做）

### 4.4 如何让镜像被 Pin？

**方法取决于容器运行时**：

#### 方法 1：容器运行时自动 Pin（最常见）

大多数容器运行时会自动 pin 系统关键镜像：

- **containerd**：可能会 pin `pause` 镜像
- **CRI-O**：可能会 pin 运行时依赖的镜像

#### 方法 2：通过容器运行时 API（如果支持）

某些容器运行时可能提供 API 来标记镜像为 pinned，但这不是 Kubernetes 标准功能。

#### 方法 3：修改容器运行时代码

如果使用自定义容器运行时，可以在 `ListImages` 实现中设置 `pinned = true`。

### 4.5 代码中的 Pin 处理

**在 `detectImages` 中读取 Pin 状态**：

```309:310:pkg/kubelet/images/image_gc_manager.go
		logger.V(5).Info("Image ID is pinned", "imageID", imageKey, "pinned", image.Pinned)
		im.imageRecords[imageKey].pinned = image.Pinned
```

- 从容器运行时返回的 `Image.Pinned` 字段读取
- 存储到 `imageRecord.pinned`

**在 `imagesInEvictionOrder` 中跳过 Pin 镜像**：

```566:570:pkg/kubelet/images/image_gc_manager.go
		// Check if image is pinned, prevent garbage collection
		if record.pinned {
			logger.V(5).Info("Image is pinned, skipping garbage collection", "imageID", image)
			continue

		}
```

- 如果 `record.pinned == true`，直接跳过
- 不会加入可删除列表

### 4.6 Pin 镜像的保护机制

**Pin 镜像永远不会被 Kubelet 垃圾回收**，因为：

1. ✅ 在 `imagesInEvictionOrder` 中被过滤（第567行）
2. ✅ 不会出现在 `freeSpace` 的输入列表中
3. ✅ 即使磁盘空间不足，也不会被删除

**注意**：Pin 是**运行时级别的保护**，不是 Kubernetes 级别的。如果直接通过容器运行时 API 删除，仍然可以删除 pinned 镜像。

---

## 五、完整调用流程示例

```
GarbageCollect(ctx, beganGC)
    │
    ├─→ freeTime = time.Now()
    │
    ├─→ imagesInEvictionOrder(ctx, freeTime)
    │   │
    │   ├─→ detectImages(ctx, freeTime)
    │   │   ├─→ runtime.ListImages() → 获取所有镜像（包含 Pinned 状态）
    │   │   ├─→ runtime.GetPods() → 获取所有容器
    │   │   └─→ 更新 imageRecords（包括 pinned 字段）
    │   │
    │   ├─→ 遍历 imageRecords
    │   │   ├─→ 跳过：正在使用的镜像
    │   │   ├─→ 跳过：pinned 镜像 ✅
    │   │   └─→ 加入：未使用且未 pinned 的镜像
    │   │
    │   └─→ 按 LRU 排序
    │
    ├─→ freeOldImages(ctx, images, freeTime, beganGC)  // 可选：基于 MaxAge 删除
    │
    └─→ freeSpace(ctx, amountToFree, freeTime, images)
        │
        ├─→ 遍历已排序的 images
        │   ├─→ 检查：lastUsed < freeTime
        │   ├─→ 检查：age >= MinAge
        │   └─→ 删除：freeImage()
        │
        └─→ 返回：剩余镜像、释放空间、错误
```

---

## 六、关键设计要点

### 6.1 为什么需要两个阶段？

1. **职责分离**：
   - `imagesInEvictionOrder`：负责筛选和排序（只读）
   - `freeSpace`：负责实际删除（写入）

2. **可复用性**：
   - `imagesInEvictionOrder` 的结果可以被多个函数使用
   - 例如：`GarbageCollect` 和 `DeleteUnusedImages` 都使用它

3. **测试友好**：
   - 可以单独测试筛选逻辑和删除逻辑

### 6.2 为什么需要最小年龄检查？

- **防止删除刚拉取的镜像**：镜像刚拉取时，容器可能还在启动中
- **给容器启动留出时间**：避免镜像被拉取后立即被删除
- **默认 2 分钟**：通常足够容器完成启动

### 6.3 为什么需要最近使用时间检查？

- **防止竞态条件**：在 `detectImages` 和 `freeSpace` 之间，镜像可能被标记为使用中
- **双重保护**：即使镜像通过了 `imagesInEvictionOrder` 的筛选，也要再次检查

### 6.4 Pin 镜像的设计考虑

- **运行时决定**：容器运行时最了解哪些镜像不应该被删除
- **建议性质**：虽然名为"建议"，但 Kubelet 完全尊重这个建议
- **系统保护**：确保关键镜像（如 pause）不会被误删

