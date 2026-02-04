# Devbox Snapshotter 参与 GC 的问题分析

## 一、问题描述

**现象**：containerd 重启时执行 GC，但只看到 `overlayfs` snapshotter 参与 GC，没有看到 `devbox` snapshotter。

**日志示例**：
```
Resolved snapshotter name: overlayfs
Snapshotter name: overlayfs
Using cached snapshotter: overlayfs
```

## 二、GC 机制分析

### 2.1 GC 如何选择 snapshotter？

**代码位置**：`metadata/db.go:376-378`

```go
if n.Type == ResourceSnapshot {
    if idx := strings.IndexRune(n.Key, '/'); idx > 0 {
        m.dirtySS[n.Key[:idx]] = struct{}{}  // 提取 snapshotter 名称
    }
}
```

**机制**：
1. GC 扫描所有被删除的 snapshot
2. 从 snapshot key 中提取 snapshotter 名称（key 格式：`snapshotter/snapshot-name`）
3. 将 snapshotter 名称添加到 `m.dirtySS` map
4. 对 `m.dirtySS` 中的每个 snapshotter 调用 `cleanupSnapshotter`

**关键点**：
- **只有被删除的 snapshot 才会触发对应 snapshotter 的 GC**
- 如果某个 snapshotter 的 snapshot 没有被删除，就不会被加入 `m.dirtySS`
- 因此不会调用该 snapshotter 的 `Cleanup` 方法

### 2.2 Snapshotter 注册流程

**代码位置**：`metadata/plugin/plugin.go:103-119`

```go
snapshottersRaw, err := ic.GetByType(plugin.SnapshotPlugin)
// ...
snapshotters := make(map[string]snapshots.Snapshotter)
for name, sn := range snapshottersRaw {
    sn, err := sn.Instance()
    // ...
    snapshotters[name] = sn.(snapshots.Snapshotter)
}
// ...
mdb := metadata.NewDB(db, cs.(content.Store), snapshotters, dbopts...)
```

**流程**：
1. Metadata 插件初始化时，通过 `ic.GetByType(plugin.SnapshotPlugin)` 获取所有 snapshotter 插件
2. 将它们转换为 `map[string]snapshots.Snapshotter`
3. 传递给 `metadata.NewDB`，注册到 `m.ss` map 中

**devbox 注册**：`snapshots/devbox/plugin/plugin.go:42-84`
- 已正确注册为 `plugin.SnapshotPlugin`，ID 为 `"devbox"`
- 应该会被 metadata 插件自动发现并注册

### 2.3 为什么只看到 overlayfs？

**可能原因**：

#### 原因 1：只有 overlayfs 的 snapshot 被删除了 ⭐ 最可能

**解释**：
- GC 只对**被删除的 snapshot** 进行处理
- 如果当前只有 overlayfs 的 snapshot 被删除，就只有 overlayfs 被加入 `m.dirtySS`
- devbox 的 snapshot 如果没有被删除，就不会触发 devbox 的 GC

**验证方法**：
```bash
# 查看 containerd 的 metadata 数据库
# 检查是否有 devbox 的 snapshot 被标记为删除
```

#### 原因 2：Devbox snapshotter 没有被正确注册

**检查方法**：
1. 查看 containerd 启动日志，确认 devbox snapshotter 是否被加载
2. 检查 `m.ss` map 中是否包含 `"devbox"`

**验证代码**：
```go
// 在 metadata/db.go 的 cleanupSnapshotter 中添加日志
func (m *DB) cleanupSnapshotter(ctx context.Context, name string) (time.Duration, error) {
    log.G(ctx).WithField("snapshotter", name).Info("cleanupSnapshotter called")
    sn, ok := m.ss[name]
    if !ok {
        log.G(ctx).WithField("snapshotter", name).Warn("snapshotter not found in m.ss")
        return 0, nil
    }
    // ...
}
```

#### 原因 3：Devbox 的 snapshot key 格式不对

**检查**：
- Snapshot key 格式应该是：`devbox/snapshot-name`
- 如果格式不对，GC 无法正确提取 snapshotter 名称

---

## 三、如何让 Devbox 参与 GC？

### 方案 1：确保 Devbox 的 Snapshot 被删除 ⭐ 推荐

**原理**：GC 只处理被删除的 snapshot，所以需要确保 devbox 的 snapshot 被正确删除。

**方法**：
1. **手动触发删除**：删除使用 devbox snapshotter 的容器
2. **等待自动 GC**：当容器被删除时，snapshot 会被标记为删除，触发 GC

**验证**：
```bash
# 查看是否有 devbox 的容器
crictl ps -a

# 删除一个使用 devbox 的容器
crictl rm <container-id>

# 观察日志，应该能看到 devbox 的 GC
```

### 方案 2：强制所有 Snapshotter 参与 GC ⭐⭐ 可选

**原理**：修改 GC 逻辑，对所有已注册的 snapshotter 都执行 Cleanup，而不仅仅是被删除的。

**修改位置**：`metadata/db.go:420-438`

**当前代码**：
```go
if len(m.dirtySS) > 0 {
    // 只处理 m.dirtySS 中的 snapshotter
    for snapshotterName := range m.dirtySS {
        go func(snapshotterName string) {
            m.cleanupSnapshotter(ctx, snapshotterName)
            // ...
        }(snapshotterName)
    }
}
```

**修改方案**：
```go
// 方案 A：处理所有已注册的 snapshotter
for snapshotterName := range m.ss {
    go func(snapshotterName string) {
        m.cleanupSnapshotter(ctx, snapshotterName)
        // ...
    }(snapshotterName)
}

// 方案 B：处理 dirtySS + 所有已注册的 snapshotter（去重）
allSnapshotters := make(map[string]struct{})
for name := range m.dirtySS {
    allSnapshotters[name] = struct{}{}
}
for name := range m.ss {
    allSnapshotters[name] = struct{}{}
}
for snapshotterName := range allSnapshotters {
    go func(snapshotterName string) {
        m.cleanupSnapshotter(ctx, snapshotterName)
        // ...
    }(snapshotterName)
}
```

**优势**：
- ✅ 确保所有 snapshotter 都参与 GC
- ✅ 可以清理 orphaned 资源

**劣势**：
- ⚠️ 可能执行不必要的清理操作
- ⚠️ 性能开销略大

### 方案 3：定期强制 GC（不推荐）

**原理**：定期对所有 snapshotter 执行 Cleanup。

**实现**：
```go
// 在 containerd 启动时，启动一个 goroutine
go func() {
    ticker := time.NewTicker(1 * time.Hour)
    defer ticker.Stop()
    for range ticker.C {
        for name := range m.ss {
            m.cleanupSnapshotter(context.Background(), name)
        }
    }
}()
```

**劣势**：
- ⚠️ 增加系统负载
- ⚠️ 不是标准做法

---

## 四、验证 Devbox 是否参与 GC

### 4.1 添加日志

**修改**：`metadata/db.go:420-438`

```go
if len(m.dirtySS) > 0 {
    var sl sync.Mutex
    stats.SnapshotD = map[string]time.Duration{}
    wg.Add(len(m.dirtySS))
    for snapshotterName := range m.dirtySS {
        log.G(ctx).WithField("snapshotter", snapshotterName).Info("GC: scheduling snapshotter cleanup")  // 添加日志
        go func(snapshotterName string) {
            st1 := time.Now()
            m.cleanupSnapshotter(ctx, snapshotterName)
            // ...
        }(snapshotterName)
    }
    m.dirtySS = map[string]struct{}{}
} else {
    log.G(ctx).Debug("GC: no dirty snapshotters to clean up")  // 添加日志
}
```

### 4.2 检查 Snapshotter 注册

**添加日志**：`metadata/db.go:134-136`

```go
for name, sn := range ss {
    log.G(ctx).WithField("snapshotter", name).Info("Registering snapshotter")  // 添加日志
    m.ss[name] = newSnapshotter(m, name, sn)
}
```

### 4.3 检查 Snapshot Key 格式

**添加日志**：`metadata/db.go:376-378`

```go
if n.Type == ResourceSnapshot {
    if idx := strings.IndexRune(n.Key, '/'); idx > 0 {
        snapshotterName := n.Key[:idx]
        log.G(ctx).WithField("snapshotter", snapshotterName).WithField("key", n.Key).Debug("GC: found snapshot to clean")  // 添加日志
        m.dirtySS[snapshotterName] = struct{}{}
    }
}
```

---

## 五、推荐解决方案

### 5.1 短期方案（立即执行）

1. **添加日志**：在 GC 相关代码中添加日志，确认：
   - devbox snapshotter 是否被注册
   - devbox 的 snapshot 是否被删除
   - devbox 是否被加入 `m.dirtySS`

2. **手动触发**：删除一个使用 devbox 的容器，观察是否触发 devbox 的 GC

### 5.2 长期方案（可选）

**如果确认 devbox 应该参与 GC，但当前没有触发**：

**方案 A**：修改 GC 逻辑，对所有已注册的 snapshotter 都执行 Cleanup
- 优点：确保所有 snapshotter 都参与 GC
- 缺点：可能执行不必要的清理

**方案 B**：保持当前逻辑，但确保 devbox 的 snapshot 被正确删除
- 优点：符合 containerd 的设计
- 缺点：需要确保删除逻辑正确

---

## 六、总结

### 6.1 问题本质

**GC 机制是事件驱动的**：
- 只有当 snapshot 被删除时，才会触发对应 snapshotter 的 GC
- 如果 devbox 的 snapshot 没有被删除，就不会触发 devbox 的 GC

### 6.2 为什么只看到 overlayfs？

**最可能的原因**：
- 当前只有 overlayfs 的 snapshot 被删除
- devbox 的 snapshot 没有被删除，所以不会触发 devbox 的 GC

### 6.3 如何让 devbox 参与 GC？

1. **确保 devbox 的 snapshot 被删除**（推荐）
2. **修改 GC 逻辑，强制所有 snapshotter 参与**（可选）

### 6.4 验证步骤

1. 添加日志，确认 devbox 是否被注册
2. 删除使用 devbox 的容器，观察是否触发 GC
3. 检查 snapshot key 格式是否正确

---

## 附录：相关代码位置

- `metadata/db.go:376-378` - GC 提取 snapshotter 名称
- `metadata/db.go:420-438` - GC 调度 snapshotter cleanup
- `metadata/db.go:511-526` - `cleanupSnapshotter` 实现
- `metadata/plugin/plugin.go:103-119` - Snapshotter 注册
- `snapshots/devbox/plugin/plugin.go:42-84` - Devbox 插件注册

