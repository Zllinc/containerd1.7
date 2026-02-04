# Devbox Snapshotter 设计总结

## 一、背景与目的

### 1.1 技术背景

Containerd 作为容器运行时，通过 Snapshotter 插件机制管理容器的文件系统层。默认的 Overlayfs Snapshotter 基于联合文件系统（Union FS）实现，适用于大多数容器场景。

但对于 Devbox 这种需要持久化开发环境、支持快照和动态扩容的场景，Overlayfs 存在以下局限：
- 无法方便地实现存储快照和回滚
- 难以实现动态容量扩展
- 存储管理相对简单，不支持精细化的存储控制

### 1.2 设计目的

Devbox Snapshotter 基于 LVM thin provisioning 技术，旨在：
1. **提供持久化存储**：为开发环境提供独立的 LVM 逻辑卷
2. **支持快照功能**：通过 LVM snapshot 实现环境状态保存（commit）
3. **动态扩容**：支持运行时扩展存储容量
4. **与 Kubernetes 集成**：通过 RuntimeClass 机制无缝集成到 K8s 集群

## 二、核心架构

### 2.1 Containerd 架构关键组件

```
Kubelet (CRI Client)
    ↓
CRI-shim (协议转换)
    ↓
Containerd Core
    ├── Content Store (镜像内容存储)
    ├── Metadata Store (元数据管理)
    └── Snapshotter Plugin (文件系统层管理)
        ├── Overlayfs (默认)
        └── Devbox (自定义)
```

### 2.2 Devbox Snapshotter 工作原理

#### Snapshot 三种状态
1. **Committed**：已提交的只读快照（镜像层）
2. **Active**：可读写的活跃快照（容器可写层）
3. **View**：只读视图快照

#### Snapshot 生命周期
```
Committed Snapshot A0
    ↓ Prepare
Active Snapshot a (可读写)
    ↓ 修改文件系统
Active Snapshot a' (修改后)
    ↓ Commit
Committed Snapshot A1
```

### 2.3 LVM Thin Provisioning 架构

```
Volume Group (VG)
    └── Thin Pool
        ├── LV1 (Devbox 1 存储)
        ├── LV2 (Devbox 2 存储)
        └── LV3 (Devbox 3 存储)
```

**优势**：
- 按需分配存储空间
- 支持 COW (Copy-on-Write) 快照
- 可动态扩容

## 三、关键功能实现

### 3.1 RuntimeClass 触发机制

通过 Kubernetes RuntimeClass 指定使用 Devbox Snapshotter：

```yaml
# RuntimeClass 定义
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: devbox-runtime
handler: devbox-runc

# Pod 定义
spec:
  runtimeClassName: devbox-runtime
  containers:
  - name: my-container
    image: nginx
```

**流程**：
```
Pod YAML (runtimeClassName: devbox-runtime)
    ↓
Kubelet 读取 RuntimeClass (handler: devbox-runc)
    ↓
Containerd config.toml (runtimes.devbox-runc.snapshotter = "devbox")
    ↓
使用 Devbox Snapshotter 创建容器存储
```

### 3.2 镜像拉取与解压

#### 拉取阶段（Pull）
从 Registry 下载到 Content Store：
- Manifest（镜像结构描述）
- Config（镜像配置，包含 RootFS.DiffIDs）
- Layers（压缩的层文件，tar.gz）

存储位置：`/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/`

#### 解压阶段（Unpack）
从 Content Store 解压到 Snapshotter：
1. 读取 manifest 获取所有压缩层（Blob Digest）
2. 读取 config 获取所有解压层（Diff ID）
3. 按顺序处理每个 layer：
   - 从 content store 读取压缩 blob
   - 解压并应用到 snapshotter（创建 LVM LV）
   - 验证 diff ID 一致性
   - 建立映射关系（blob digest → diff digest）

**关键**：通过 `containerd.io/uncompressed` label 建立压缩和解压数据的映射关系。

### 3.3 容器创建流程

```
1. Kubelet 发起 CreateContainer 请求
    ↓
2. 确定使用 devbox snapshotter
    ↓
3. 调用 Prepare() 创建 Active Snapshot
    ↓
4. 创建 LVM LV（如果不存在）
    ↓
5. 格式化文件系统（ext4）
    ↓
6. 挂载 LV 到指定目录
    ↓
7. 返回挂载信息给容器运行时
    ↓
8. 容器启动，使用该 LV 作为 rootfs
```

### 3.4 容器停止与清理

#### StopContainer
- 主动停止请求（kubectl delete pod）
- 发送 SIGTERM，等待 graceful timeout
- 调用 `UpdateDevboxSnapshot` 解挂载 LVM
- 超时则发送 SIGKILL

#### handleContainerExit
- 被动事件响应（容器崩溃、自然退出）
- 清理 task、更新状态
- 发送 CONTAINER_STOPPED_EVENT

**关键**：两个函数都需要调用解挂载逻辑，确保容器异常退出时也能清理资源。

### 3.5 Devbox Commit 机制

Commit 流程：
1. Controller 创建临时容器，挂载目标 LV
2. 调用 `SetLvRemovable` 标记 LV 可删除
3. 使用 `nerdctl commit` 将容器 rootfs 打包成镜像
4. 推送镜像到 Registry
5. 删除临时容器
6. 更新 Devbox CR 的 commitRecords

## 四、解决的关键问题

### 4.1 镜像占用双倍存储

**问题**：
- Overlayfs 和 Devbox 两个 Snapshotter 都会解压同一镜像
- 造成存储空间浪费

**解决方案**：
通过 Pod Annotation 指定 runtime handler，使镜像只解压到 Devbox Snapshotter：
```yaml
annotations:
  io.containerd.cri.runtime-handler: "devbox-runc"
```

**原理**：
- PullImage 阶段读取 annotation，确定使用 devbox snapshotter
- Unpack 阶段只解压到 devbox snapshotter

### 4.2 LVM 误删问题

**问题**：
刚创建的 LV 还未写入 metadata bucket，就被 Containerd GC 误删。

**根本原因**：
Cleanup 函数使用读事务，无法与 createSnapshot 互斥。

**解决方案**：
将 Cleanup 改为使用写事务，与 createSnapshot 互斥：
```go
// 使用写事务，阻塞 GC
return o.ms.db.Update(func(tx *bolt.Tx) error {
    // 获取需要清理的目录和 LV
    ...
})
```

**效果**：
- createSnapshot 获取写锁时，Cleanup 无法同时执行
- 避免 GC 误删正在创建的资源

### 4.3 Snapshot 误删问题

**问题**：
容器创建过程中，lowdir（父 snapshot）被 GC 误删，导致挂载失败。

**原因**：
同 LVM 误删问题，Cleanup 使用读事务。

**解决方案**：
统一使用写事务，确保事务互斥。

### 4.4 容器快速重建导致的挂载冲突

**问题场景**：
1. 容器 1 crash，触发 handleContainerExit，删除 path
2. Pod 重建，容器 2 启动，写入新 path
3. Kubelet 发起 RemoveContainer(容器 1)，误删容器 2 的 path
4. 导致一个 LV 被多个 Pod 挂载

**解决方案**：
在 bucket 中增加 `containerID` 字段：
```go
// SetDevboxContent 时记录 containerID
bucket.Put([]byte("containerID"), []byte(containerID))

// RemoveDevbox 时校验
storedContainerID := bucket.Get([]byte("containerID"))
if string(storedContainerID) != containerID {
    // 不是同一个容器，跳过删除
    return nil
}
```

### 4.5 节点重启导致旧 Pod 创建失败

**问题**：
1. 节点关机，容器进程被 kill，未执行 handleContainerExit
2. Path 未被删除
3. 节点重启，Containerd recover 标记容器驱逐
4. Pod 重建，发现 path 已存在（key 不匹配），创建失败

**解决方案**：
在 RemoveDevbox 函数中重新加上移除 path 的逻辑，确保容器删除时清理完整。

### 4.6 Content Store 被误删导致镜像损坏

**问题**：
Kubelet 触发 image GC，删除 devbox base image，导致 content blob 被删除，commit 失败。

**根本原因**：
- Devbox commit 在 k8s.io namespace 执行
- 镜像在 k8s.io 中可能被 Kubelet GC
- Content Store GC 需要所有 namespace 都不引用才删除
- 但 k8s.io 中的引用被删除后，sealos.io 中的引用无法阻止 blob 删除

**解决方案 1（初始）**：
Commit 在 sealos.io namespace 执行，避开 Kubelet GC。

**解决方案 2（最终）**：
在镜像拉取时自动 Pin 住 devbox 镜像：
```go
// PullImage 时判断 snapshotter
if snapshotter == "devbox" {
    // 添加 pin label
    imageLabels[crilabels.PinnedImageLabelKey] = crilabels.PinnedImageLabelValue
}
```

**Pin 机制**：
- Kubelet GC 会跳过带有 `io.cri-containerd.pinned=pinned` label 的镜像
- 保护 devbox base image 不被回收

### 4.7 LVM 命令超时与进程清理

**问题**：
LVM 命令可能卡住，需要超时机制和进程组清理。

**解决方案**：
1. 使用 `context.WithTimeout` 设置统一 2 分钟超时
2. 使用 `cmd.Cancel` 在超时时发送 SIGTERM
3. 使用 `cmd.WaitDelay` 自动在 2 秒后发送 SIGKILL
4. 使用 `Setpgid: true` 创建进程组，确保子进程也被清理

```go
ctx, cancel := context.WithTimeout(context.Background(), CommandTimeout)
defer cancel()

cmd := exec.CommandContext(ctx, command, args...)
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
cmd.Cancel = func() error {
    return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}
cmd.WaitDelay = CommandGraceTimeout

err := cmd.Run()
```

## 五、Content Store 与 Namespace 共享

### 5.1 存储结构

Content Store 由三部分组成：

1. **Content Blobs**（物理文件）
   - 路径：`/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/`
   - 所有 namespace 共享
   - Content-addressable storage

2. **Image Metadata**（镜像元数据）
   - 存储：BoltDB `bucketKeyVersion -> [namespace] -> bucketKeyObjectImages`
   - 每个 namespace 独立
   - 包含：镜像名称、Target Descriptor、Labels

3. **Content Info**（内容元数据）
   - 存储：BoltDB `bucketKeyVersion -> [namespace] -> bucketKeyObjectContent`
   - 每个 namespace 独立
   - 作用：映射 Image Metadata 和 Content Blobs

### 5.2 GC 逻辑

Content Store GC 会遍历所有 namespace，收集被引用的 content：
```go
contentSeen := map[string]struct{}{}
// 遍历所有 namespace
for namespace in all_namespaces {
    // 收集该 namespace 中被引用的 content
    contentSeen[digest] = struct{}{}
}
// 只删除不在 contentSeen 中的 content
```

**结论**：Content blob 只有在所有 namespace 都不引用时才会被删除。

## 六、技术要点

### 6.1 Blob Digest vs Diff ID

- **Blob Digest**（61fec911...）：压缩文件（tar.gz）的 SHA256
  - 作为 content store 的文件名
  - 存储在 Manifest.Layers

- **Diff ID**（e3e5579d...）：解压文件的 SHA256
  - 存储在 Config.RootFS.DiffIDs
  - 通过 `containerd.io/uncompressed` label 映射到 Blob Digest

### 6.2 Snapshot Key vs Snapshot ID

- **Snapshot Key**：用户可见的标识（如 `sha256:xxx` 或容器 ID）
- **Snapshot ID**：存储层的内部 ID（从 1 开始的数字）
- 映射关系存储在 BoltDB metadata 中

### 6.3 ChainID 计算

Snapshot 的 ChainID 基于层叠的 Diff ID 计算：
```
ChainID(layer[0]) = DiffID(layer[0])
ChainID(layer[n]) = SHA256(ChainID(layer[n-1]) + " " + DiffID(layer[n]))
```

确保相同的层堆栈产生相同的 ChainID。

## 七、总结

Devbox Snapshotter 通过 LVM thin provisioning 技术，为 Devbox 提供了：
1. **持久化存储**：独立的 LVM 逻辑卷
2. **快照能力**：基于 LVM snapshot 的 commit 功能
3. **动态扩容**：运行时调整存储容量
4. **资源管理**：完善的 GC 和清理机制
5. **Kubernetes 集成**：通过 RuntimeClass 无缝集成

在实现过程中，解决了多个关键技术问题：
- 镜像双倍存储
- 资源误删（LVM、Snapshot）
- 挂载冲突
- 节点重启恢复
- 镜像 GC 保护
- LVM 命令超时

这些优化使 Devbox Snapshotter 成为一个稳定可靠的生产级 Containerd 插件。

