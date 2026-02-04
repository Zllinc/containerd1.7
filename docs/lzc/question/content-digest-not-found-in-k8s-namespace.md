# Commit 时 Content Digest Not Found 问题分析

## 问题描述

在将 containerd namespace 从 `sealos.io` 切换到 `k8s.io` 后，执行写入大量数据（如 10GB）的容器 commit 操作时，出现以下错误：

```
failed to generate commit image config: content digest sha256:d20f38e2b951197c7d0b5fff56720f46113f91035ea0f9d7482e6b8fb41ec0f1: not found
```

## 根本原因

**Kubelet Image GC（垃圾回收）机制**

当使用 `k8s.io` namespace 时，该 namespace 由 Kubelet 管理，而 Kubelet 有自己的镜像垃圾回收机制。当写入大量数据导致磁盘使用率上升时，触发了 Kubelet 的 Image GC，导致基础镜像的 content 被删除。

## 详细分析

### 1. Containerd Namespace 隔离机制

```
┌─────────────────────────────────────────────────────────────────┐
│                     Containerd                                   │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │                   Content Store                          │   │
│  │     (共享存储，跨 namespace 共享 blob 数据)               │   │
│  │                                                          │   │
│  │   sha256:abc123... ◄─────┬─────────► sha256:def456...   │   │
│  │                          │                               │   │
│  └──────────────────────────┼───────────────────────────────┘   │
│                             │                                    │
│  ┌──────────────────────────┼───────────────────────────────┐   │
│  │         Namespace: k8s.io │        Namespace: sealos.io  │   │
│  │  ┌─────────────────┐     │    ┌─────────────────┐        │   │
│  │  │ Image Reference │     │    │ Image Reference │        │   │
│  │  │  (指向 content) │     │    │  (指向 content) │        │   │
│  │  └────────┬────────┘     │    └────────┬────────┘        │   │
│  │           │              │             │                  │   │
│  │  ┌────────▼────────┐     │    ┌────────▼────────┐        │   │
│  │  │   Containers    │     │    │   Containers    │        │   │
│  │  └─────────────────┘     │    └─────────────────┘        │   │
│  └──────────────────────────┴───────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

**关键点**：
- **Content Store 是共享的**：所有 namespace 共享同一个 content store
- **Image/Container 引用是隔离的**：每个 namespace 有独立的 image 和 container 记录
- **引用计数机制**：content 被删除需要所有 namespace 的引用都被移除

### 2. k8s.io vs sealos.io Namespace 对比

| 特性 | k8s.io | sealos.io |
|------|--------|-----------|
| 管理者 | Kubelet | Devbox Controller |
| GC 策略 | Kubelet Image GC | 无自动 GC |
| GC 触发条件 | 磁盘使用率阈值 | 手动或 Controller 控制 |
| 影响范围 | 所有 K8s Pod 镜像 | 仅 Devbox 镜像 |

### 3. Kubelet Image GC 机制

Kubelet 的 Image GC 由以下参数控制：

```yaml
# kubelet 配置
imageGCHighThresholdPercent: 85  # 磁盘使用率超过 85% 触发 GC
imageGCLowThresholdPercent: 80   # GC 后目标降到 80%
imageMinimumGCAge: 2m            # 镜像最少存活 2 分钟
```

**GC 流程**：

```
┌─────────────────────────────────────────────────────────────┐
│                    Kubelet Image GC 流程                     │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│   1. 监控磁盘使用率                                           │
│      └── 周期性检查 /var/lib/containerd 使用率               │
│                                                              │
│   2. 判断是否触发 GC                                          │
│      └── 使用率 > imageGCHighThresholdPercent (85%)          │
│                                                              │
│   3. 收集可删除镜像                                           │
│      └── 未被任何 Running/Pending Pod 使用的镜像              │
│      └── 按 LRU (最近最少使用) 排序                           │
│                                                              │
│   4. 删除镜像直到使用率 < imageGCLowThresholdPercent (80%)   │
│      └── 调用 containerd DeleteImage API                     │
│      └── 触发 containerd content GC                          │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

### 4. 问题发生链路

```
┌─────────────────────────────────────────────────────────────┐
│                      问题发生链路                             │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│   1. Devbox Pod 运行中                                       │
│      └── 使用基础镜像 ghcr.io/xxx:tag                        │
│      └── 镜像存储在 k8s.io namespace                         │
│                                                              │
│   2. 容器内写入 10GB 数据                                     │
│      └── /var/lib/containerd 磁盘使用率上升                  │
│      └── 超过 imageGCHighThresholdPercent (85%)             │
│                                                              │
│   3. Kubelet 触发 Image GC                                   │
│      └── 扫描 k8s.io namespace 中未使用的镜像                │
│      └── 基础镜像可能被判定为"未使用"                         │
│         (因为 Pod 使用的是 snapshot，不是直接引用镜像)        │
│                                                              │
│   4. Kubelet 删除基础镜像                                     │
│      └── 删除 k8s.io namespace 中的 image reference          │
│      └── 触发 containerd content GC                          │
│      └── 删除 sha256:d20f38e2b... 等 content blob           │
│                                                              │
│   5. Commit 操作失败                                          │
│      └── 需要读取基础镜像 config 生成新镜像 config            │
│      └── 找不到 content digest                               │
│      └── 报错: content digest sha256:xxx not found          │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

### 5. 为什么 sealos.io Namespace 不会出现此问题？

1. **不受 Kubelet 管理**：`sealos.io` namespace 不在 Kubelet 的管理范围内
2. **无自动 GC**：除非 Devbox Controller 主动调用删除，否则镜像不会被清理
3. **独立的引用计数**：即使 `k8s.io` 中的镜像被删除，`sealos.io` 中的引用仍然存在，content 不会被 GC

### 6. Content Store GC 机制

Containerd 的 Content GC 规则：

```go
// 伪代码表示 content GC 逻辑
func shouldDeleteContent(digest string) bool {
    for namespace := range allNamespaces {
        images := getImages(namespace)
        for _, image := range images {
            if image.referencesContent(digest) {
                return false  // 只要有一个引用，就不删除
            }
        }
    }
    return true  // 没有任何引用时删除
}
```

**关键**：当 `k8s.io` namespace 中的镜像被删除，且 `sealos.io` namespace 中没有相同镜像时，content 会被 GC。

## 解决方案

### 方案 1：继续使用 sealos.io Namespace（推荐）

保持原有设计，使用独立的 `sealos.io` namespace：

```go
// const.go
const DefaultNamespace = "sealos.io"
```

**优点**：
- 完全独立于 Kubelet GC
- 资源生命周期由 Devbox Controller 完全控制

### 方案 2：在 k8s.io 中保持镜像引用

如果必须使用 `k8s.io` namespace，需要确保基础镜像始终有 Pod 引用：

```yaml
# 创建一个 DaemonSet 引用基础镜像
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: devbox-image-holder
spec:
  template:
    spec:
      containers:
      - name: holder
        image: ghcr.io/labring-actions/devbox/go-1.23.0:latest
        command: ["sleep", "infinity"]
```

**缺点**：
- 需要额外资源
- 每个基础镜像都需要维护

### 方案 3：调整 Kubelet GC 阈值

修改 Kubelet 配置，提高 GC 阈值：

```yaml
# /var/lib/kubelet/config.yaml
imageGCHighThresholdPercent: 95
imageGCLowThresholdPercent: 90
```

**缺点**：
- 可能导致节点磁盘空间不足
- 影响整个集群的 GC 策略

### 方案 4：Commit 前检查并重新拉取镜像

在 commit 操作前，检查基础镜像是否完整，如不完整则重新拉取：

```go
func (c *CommitterImpl) ensureBaseImage(ctx context.Context, baseImage string) error {
    // 检查镜像是否存在且完整
    img, err := c.containerdClient.GetImage(ctx, baseImage)
    if err != nil {
        // 镜像不存在，需要拉取
        return c.pullImage(ctx, baseImage)
    }
    
    // 检查 content 是否完整
    config, err := img.Config(ctx)
    if err != nil {
        // content 不完整，需要重新拉取
        c.RemoveImages(ctx, []string{baseImage}, true, false)
        return c.pullImage(ctx, baseImage)
    }
    
    return nil
}
```

## 建议

1. **保持使用 sealos.io namespace**：这是最稳定的方案，避免与 Kubelet GC 冲突

2. **如需使用 k8s.io**：
   - 在 commit 前检查镜像完整性
   - 实现镜像预检和自动重拉取机制
   - 考虑增加磁盘监控告警

3. **监控指标**：
   - 监控 `/var/lib/containerd` 磁盘使用率
   - 监控 Kubelet Image GC 事件
   - 监控 commit 失败率

## 相关代码

| 文件 | 说明 |
|------|------|
| `internal/commit/const.go` | Namespace 定义 (`sealos.io`) |
| `internal/commit/commit.go` | Commit 实现逻辑 |

## 参考资料

- [Kubernetes Garbage Collection](https://kubernetes.io/docs/concepts/architecture/garbage-collection/)
- [Containerd Content Store](https://github.com/containerd/containerd/blob/main/docs/content-flow.md)
- [Kubelet Image GC](https://kubernetes.io/docs/concepts/cluster-administration/kubelet-garbage-collection/)

## 更新日志

| 日期 | 版本 | 更新内容 |
|------|------|----------|
| 2025-12-11 | v1.0 | 初始版本 |

