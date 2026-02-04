# Containerd Content Store 与 Namespace 共享机制详解

本文档详细解释 containerd 中 Content Store 的跨 namespace 共享机制，以及为什么使用 `sealos.io` namespace 可以避免 Kubelet GC 导致的 content 丢失问题。

## 一、核心架构：Content Store 是全局共享的

### 1.1 存储结构

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        Containerd 存储架构                                    │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│   Content Store (全局共享存储)                                               │
│   路径: /var/lib/containerd/io.containerd.content.v1.content/blobs/         │
│   ┌────────────────────────────────────────────────────────────────────┐    │
│   │  sha256/                                                            │    │
│   │  ├── abc123...  (镜像 manifest)                                    │    │
│   │  ├── def456...  (镜像 config)                                      │    │
│   │  ├── 789xyz...  (镜像 layer)                                       │    │
│   │  └── ...                                                            │    │
│   └────────────────────────────────────────────────────────────────────┘    │
│                           ▲                                                  │
│                           │ 被多个 namespace 引用                            │
│   ┌───────────────────────┴──────────────────────────────────────────────┐  │
│   │                                                                       │  │
│   │  ┌─────────────────────┐        ┌─────────────────────┐              │  │
│   │  │   Namespace: k8s.io │        │ Namespace: sealos.io│              │  │
│   │  │                     │        │                     │              │  │
│   │  │  Images:            │        │  Images:            │              │  │
│   │  │  ├── redis:5.0      │        │  ├── redis:5.0      │              │  │
│   │  │  └── nginx:latest   │        │  └── my-image:v1    │              │  │
│   │  │                     │        │                     │              │  │
│   │  │  Snapshots:         │        │  Snapshots:         │              │  │
│   │  │  └── ...            │        │  └── ...            │              │  │
│   │  └─────────────────────┘        └─────────────────────┘              │  │
│   │                                                                       │  │
│   └───────────────────────────────────────────────────────────────────────┘  │
│                                                                              │
│   ⭐ 关键点：Content blobs 是全局唯一的，通过 digest 标识                     │
│   ⭐ 不同 namespace 可以引用相同的 content blob                               │
│   ⭐ Content blob 只有在【所有 namespace 都不引用】时才会被 GC 删除           │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 1.2 核心代码：Content Store GC 逻辑

**代码位置**: `metadata/content.go` 第 803-888 行

```go
// garbageCollect removes all contents that are no longer used.
func (cs *contentStore) garbageCollect(ctx context.Context) (d time.Duration, err error) {
    contentSeen := map[string]struct{}{}
    
    if err := cs.db.View(func(tx *bolt.Tx) error {
        v1bkt := tx.Bucket(bucketKeyVersion)
        if v1bkt == nil {
            return nil
        }
        
        // ⭐ 关键：遍历【所有 namespace】
        v1c := v1bkt.Cursor()
        for k, v := v1c.First(); k != nil; k, v = v1c.Next() {
            if v != nil {
                continue
            }
            
            // 收集该 namespace 中被引用的 content
            cbkt := v1bkt.Bucket(k).Bucket(bucketKeyObjectContent)
            if cbkt == nil {
                continue
            }
            bbkt := cbkt.Bucket(bucketKeyObjectBlob)
            if bbkt != nil {
                if err := bbkt.ForEach(func(ck, cv []byte) error {
                    if cv == nil {
                        // ⭐ 将被引用的 content 加入 contentSeen
                        contentSeen[string(ck)] = struct{}{}
                    }
                    return nil
                }); err != nil {
                    return err
                }
            }
        }
        return nil
    }); err != nil {
        return 0, err
    }
    
    // ⭐ 只删除不在 contentSeen 中的 content（即没有任何 namespace 引用的）
    err = cs.Store.Walk(ctx, func(info content.Info) error {
        if _, ok := contentSeen[info.Digest.String()]; !ok {
            if err := cs.Store.Delete(ctx, info.Digest); err != nil {
                return err
            }
            log.G(ctx).WithField("digest", info.Digest).Debug("removed content")
        }
        return nil
    })
    // ...
}
```

**结论**：Content blob 只有在**所有 namespace 都不引用**时才会被删除。

---

## 二、问题一：跨 Namespace 拉取已存在镜像的过程

### 2.1 场景描述

假设 `redis:5.0` 镜像已经在 `k8s.io` namespace 中存在，现在要在 `sealos.io` namespace 中拉取同一个镜像。

### 2.2 完整流程

```
┌─────────────────────────────────────────────────────────────────────────────┐
│   在 sealos.io namespace 拉取 k8s.io 中已存在的镜像                          │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│   步骤 1: 解析镜像引用                                                        │
│   ┌────────────────────────────────────────────────────────────────────┐    │
│   │  client.Pull(ctx, "redis:5.0")                                     │    │
│   │  → 解析得到 manifest digest: sha256:abc123...                       │    │
│   └────────────────────────────────────────────────────────────────────┘    │
│                                    │                                         │
│                                    ▼                                         │
│   步骤 2: 检查 Content Store 是否已有该内容                                   │
│   ┌────────────────────────────────────────────────────────────────────┐    │
│   │  cs.Info(ctx, digest) → 检查 /blobs/sha256/abc123... 是否存在       │    │
│   │                                                                     │    │
│   │  if 存在:                                                           │    │
│   │      ✅ 不需要从 registry 下载                                       │    │
│   │      → 只需要在 sealos.io namespace 创建 metadata                   │    │
│   │                                                                     │    │
│   │  if 不存在:                                                          │    │
│   │      → 从 registry 下载并存储到 Content Store                        │    │
│   └────────────────────────────────────────────────────────────────────┘    │
│                                    │                                         │
│                                    ▼                                         │
│   步骤 3: 在 sealos.io namespace 创建 Image 引用                             │
│   ┌────────────────────────────────────────────────────────────────────┐    │
│   │  BoltDB 中的存储结构:                                                │    │
│   │                                                                     │    │
│   │  v1/                                                                │    │
│   │  ├── k8s.io/                                                        │    │
│   │  │   ├── content/                                                   │    │
│   │  │   │   └── blob/                                                  │    │
│   │  │   │       └── sha256:abc123... (引用)                            │    │
│   │  │   └── images/                                                    │    │
│   │  │       └── redis:5.0 → sha256:abc123...                           │    │
│   │  │                                                                  │    │
│   │  └── sealos.io/                 ← 新增                              │    │
│   │      ├── content/                                                   │    │
│   │      │   └── blob/                                                  │    │
│   │      │       └── sha256:abc123... (引用) ← 新增                     │    │
│   │      └── images/                                                    │    │
│   │          └── redis:5.0 → sha256:abc123... ← 新增                    │    │
│   └────────────────────────────────────────────────────────────────────┘    │
│                                                                              │
│   ⭐ 结果：                                                                   │
│   - Content blobs（实际文件）：不会重复下载，共享使用                          │
│   - Image metadata：在每个 namespace 独立存储                                 │
│   - Content 引用：每个 namespace 各自维护引用关系                              │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 2.3 相关代码

**代码位置**: `metadata/content.go` 第 46-60 行

```go
// newContentStore returns a namespaced content store using an existing
// content store interface.
// policy defines the sharing behavior for content between namespaces. Both
// modes will result in shared storage in the backend for committed. Choose
// "shared" to prevent separate namespaces from having to pull the same content
// twice.  Choose "isolated" if the content must not be shared between
// namespaces.
//
// If the policy is "shared", writes will try to resolve the "expected" digest
// against the backend, allowing imports of content from other namespaces. In
// "isolated" mode, the client must prove they have the content by providing
// the entire blob before the content can be added to another namespace.
```

### 2.4 总结

| 资源类型 | 是否共享 | 存储位置 |
|----------|----------|----------|
| Content blobs (实际文件) | ✅ 全局共享 | `/var/lib/containerd/io.containerd.content.v1.content/blobs/` |
| Content metadata | ❌ 按 namespace 隔离 | BoltDB: `v1/<namespace>/content/blob/<digest>` |
| Image references | ❌ 按 namespace 隔离 | BoltDB: `v1/<namespace>/images/<name>` |
| Snapshots | ❌ 按 namespace 隔离 | BoltDB: `v1/<namespace>/snapshots/<snapshotter>/<key>` |

---

## 三、问题二：为什么 sealos.io 下 commit 没问题？

### 3.1 场景回顾

用户报告的问题：
1. 在 `k8s.io` namespace 创建容器，使用基础镜像 `ghcr.io/xxx:tag`
2. 容器写入 10GB 数据，触发 Kubelet Image GC
3. Kubelet 删除了 `k8s.io` 中的基础镜像
4. 在 `k8s.io` 下执行 commit 失败：`content digest sha256:d20f38e2b...: not found`
5. 但在 `sealos.io` 下执行 commit 成功

### 3.2 关键问题解答

**用户疑问**：
> "按理来说就算在 sealos.io 下拉取了镜像，其 content 也会丢失吧？"

**答案**：**不会！**

因为 Kubelet Image GC 的删除过程如下：

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    Kubelet Image GC 删除链路                                  │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│   1. Kubelet 调用 CRI RemoveImage                                            │
│      ↓                                                                       │
│   2. CRI 服务处理（使用 k8s.io namespace）                                    │
│      ↓                                                                       │
│   3. 删除 k8s.io namespace 中的 Image 引用                                    │
│      ↓                                                                       │
│   4. 触发 containerd GC                                                      │
│      ↓                                                                       │
│   5. GC 扫描【所有 namespace】的 content 引用                                 │
│      ↓                                                                       │
│   6. 检查 sha256:d20f38e2b... 是否还有引用：                                  │
│                                                                              │
│      ┌─────────────────────────────────────────────────────────────────┐    │
│      │  k8s.io namespace:                                               │    │
│      │  └── content/blob/sha256:d20f38e2b... → ❌ 已删除               │    │
│      │                                                                  │    │
│      │  sealos.io namespace:                                            │    │
│      │  └── content/blob/sha256:d20f38e2b... → ✅ 仍然存在             │    │
│      └─────────────────────────────────────────────────────────────────┘    │
│                                                                              │
│   7. 结论：sealos.io 仍有引用 → Content blob 不会被删除                       │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 3.3 Content 引用计数机制

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    Content Blob 引用计数示例                                  │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│   Content Blob: sha256:d20f38e2b... (基础镜像 config)                        │
│                                                                              │
│   初始状态（两个 namespace 都有镜像）：                                        │
│   ┌────────────────────────────────────────────────────────────────────┐    │
│   │  引用来源:                                                          │    │
│   │  1. k8s.io/images/ghcr.io/xxx:tag     → sha256:d20f38e2b...   ✅   │    │
│   │  2. sealos.io/images/ghcr.io/xxx:tag  → sha256:d20f38e2b...   ✅   │    │
│   │                                                                     │    │
│   │  引用计数: 2                                                         │    │
│   │  GC 结果: 不删除                                                     │    │
│   └────────────────────────────────────────────────────────────────────┘    │
│                                                                              │
│   Kubelet GC 后（只删除 k8s.io 中的镜像）：                                    │
│   ┌────────────────────────────────────────────────────────────────────┐    │
│   │  引用来源:                                                          │    │
│   │  1. k8s.io/images/ghcr.io/xxx:tag     → (已删除)              ❌   │    │
│   │  2. sealos.io/images/ghcr.io/xxx:tag  → sha256:d20f38e2b...   ✅   │    │
│   │                                                                     │    │
│   │  引用计数: 1                                                         │    │
│   │  GC 结果: 仍然不删除！                                               │    │
│   └────────────────────────────────────────────────────────────────────┘    │
│                                                                              │
│   只有当 sealos.io 中的镜像也被删除时：                                        │
│   ┌────────────────────────────────────────────────────────────────────┐    │
│   │  引用来源:                                                          │    │
│   │  1. k8s.io/images/ghcr.io/xxx:tag     → (已删除)              ❌   │    │
│   │  2. sealos.io/images/ghcr.io/xxx:tag  → (已删除)              ❌   │    │
│   │                                                                     │    │
│   │  引用计数: 0                                                         │    │
│   │  GC 结果: 删除 content blob                                          │    │
│   └────────────────────────────────────────────────────────────────────┘    │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 3.4 为什么 k8s.io 下 commit 失败？

问题的关键在于：**commit 时 nerdctl 从哪个 namespace 查找 content？**

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    Commit 操作的 Content 查找                                 │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│   在 k8s.io namespace 执行 commit：                                           │
│   ┌────────────────────────────────────────────────────────────────────┐    │
│   │  1. 需要读取基础镜像 config 生成新 config                            │    │
│   │  2. 查找 sha256:d20f38e2b... (使用 k8s.io namespace)                │    │
│   │  3. 检查 k8s.io/content/blob/sha256:d20f38e2b...                    │    │
│   │  4. ❌ 未找到（因为 k8s.io 中的镜像引用已被 GC 删除）                 │    │
│   │  5. 错误: content digest sha256:d20f38e2b...: not found             │    │
│   └────────────────────────────────────────────────────────────────────┘    │
│                                                                              │
│   在 sealos.io namespace 执行 commit：                                        │
│   ┌────────────────────────────────────────────────────────────────────┐    │
│   │  1. 需要读取基础镜像 config 生成新 config                            │    │
│   │  2. 查找 sha256:d20f38e2b... (使用 sealos.io namespace)             │    │
│   │  3. 检查 sealos.io/content/blob/sha256:d20f38e2b...                 │    │
│   │  4. ✅ 找到（sealos.io 中的引用未被删除）                            │    │
│   │  5. 读取实际文件: /blobs/sha256/d20f38e2b... (全局共享存储)          │    │
│   │  6. 成功生成新 config                                                │    │
│   └────────────────────────────────────────────────────────────────────┘    │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

**关键理解**：

1. **Content 物理文件**：全局共享，存储在 `/blobs/sha256/`
2. **Content 引用/metadata**：按 namespace 隔离，存储在 BoltDB 中
3. **查找 Content 时**：先检查当前 namespace 的引用是否存在，再读取物理文件
4. **Kubelet GC**：只删除 `k8s.io` namespace 中的引用，不影响其他 namespace

---

## 四、完整时间线对比

### 4.1 只使用 k8s.io namespace（会出问题）

```
T0: 拉取基础镜像到 k8s.io
    └── k8s.io 中有 image 引用和 content 引用

T1: 创建容器，容器运行，写入 10GB 数据
    └── 容器使用 snapshot，不直接引用镜像

T2: 磁盘压力，Kubelet 触发 Image GC
    └── 删除 k8s.io 中的镜像引用
    └── Content 引用计数: 1 → 0
    └── ⚠️ Content blob 被 GC 删除

T3: 执行 commit
    └── 尝试读取基础镜像 config
    └── ❌ Content 已丢失
    └── 错误: content digest not found
```

### 4.2 同时使用 sealos.io namespace（可解决问题）

```
T0: 拉取基础镜像到 k8s.io
    └── k8s.io 中有 image 引用和 content 引用

T0': 同时在 sealos.io 拉取相同镜像
    └── Content blob 不会重复下载（共享）
    └── sealos.io 中新增 image 引用和 content 引用
    └── Content 引用计数: 2

T1: 创建容器，容器运行，写入 10GB 数据

T2: 磁盘压力，Kubelet 触发 Image GC
    └── 删除 k8s.io 中的镜像引用
    └── Content 引用计数: 2 → 1
    └── ✅ sealos.io 仍有引用，Content blob 不被删除

T3: 在 sealos.io 执行 commit
    └── 读取 sealos.io 中的 content 引用
    └── ✅ Content 仍存在
    └── 成功生成新镜像
```

---

## 五、总结

### 5.1 关键结论

| 问题 | 答案 |
|------|------|
| Content Store 是否所有 namespace 共用？ | **是**，Content blob（物理文件）全局共享 |
| 跨 namespace 拉取已存在镜像会重新下载吗？ | **不会**，只创建新的 namespace-specific metadata |
| Kubelet GC 删除镜像时会删除 content blob 吗？ | **不一定**，只有当所有 namespace 都不引用时才删除 |
| 为什么 sealos.io 下 commit 没问题？ | 因为 sealos.io 中的镜像引用未被 Kubelet 删除，content 引用仍存在 |

### 5.2 解决方案

1. **在创建容器前**，确保在 `sealos.io` namespace 中也拉取基础镜像
2. 这样即使 Kubelet GC 删除了 `k8s.io` 中的镜像，content blob 仍然存在
3. 在 `sealos.io` namespace 执行 commit 即可成功

### 5.3 相关代码文件

- `metadata/content.go` - Content Store 实现和 GC 逻辑
- `metadata/gc.go` - 垃圾回收核心逻辑
- `docs/garbage-collection.md` - 官方 GC 文档
- `docs/content-flow.md` - 官方 Content 流程文档

