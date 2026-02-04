# 容器停止流程详解

本文档详细解释 containerd 中容器停止的完整流程，涉及 `StopContainer`、`stopContainer` 和 `handleContainerExit` 三个核心函数的协作关系。

## 概述

容器停止涉及两个独立但相互配合的机制：

1. **主动停止**：由 kubelet 调用 CRI `StopContainer` 接口发起
2. **事件处理**：由 containerd 事件监控器监听 `TaskExit` 事件触发

## 函数调用关系图

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              kubelet                                         │
└─────────────────────────────────────┬───────────────────────────────────────┘
                                      │ gRPC 调用
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│  StopContainer (pkg/cri/server/container_stop.go)                           │
│  ├── 获取容器信息                                                            │
│  ├── 调用 stopContainer()  ──────────────────────────────────────────┐      │
│  ├── 调用 NRI StopContainer                                          │      │
│  └── (devbox) UpdateDevboxSnapshot 卸载 LVM                          │      │
└──────────────────────────────────────────────────────────────────────│──────┘
                                                                       │
                                                                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│  stopContainer (pkg/cri/server/container_stop.go)                           │
│  ├── 检查容器状态 (必须是 RUNNING 或 UNKNOWN)                                │
│  ├── 获取容器 task                                                           │
│  ├── 发送 SIGTERM 信号                                                       │
│  │      ↓                                                                    │
│  │   task.Kill(ctx, sig)                                                    │
│  │      ↓                                                                    │
│  ├── 等待 timeout 时间                                                       │
│  │      ↓                                                                    │
│  │   waitContainerStop(sigTermCtx, container)  ←─────────────────────┐      │
│  │      ↓                                                             │      │
│  │   [如果超时]                                                       │      │
│  │      ↓                                                             │      │
│  ├── 发送 SIGKILL 信号                                                │      │
│  │      ↓                                                             │      │
│  │   task.Kill(ctx, syscall.SIGKILL)                                 │      │
│  │      ↓                                                             │      │
│  └── 等待容器停止                                                     │      │
│         ↓                                                             │      │
│      waitContainerStop(ctx, container)  ←────────────────────────────│──┐   │
│         ↓                                                             │  │   │
│      select {                                                         │  │   │
│         case <-ctx.Done(): return error                              │  │   │
│         case <-container.Stopped(): return nil  ←────────────────────│──│───│
│      }                                                                │  │   │
└──────────────────────────────────────────────────────────────────────│──│───┘
                                                                       │  │
                       容器进程实际退出                                  │  │
                              │                                         │  │
                              ▼                                         │  │
┌─────────────────────────────────────────────────────────────────────────────┐
│  containerd 事件监控 (eventMonitor)                                         │
│  ├── 监听 /tasks/exit 事件                                                  │
│  └── 调用 handleContainerExit  ──────────────────────────────────────┐      │
└──────────────────────────────────────────────────────────────────────│──────┘
                                                                       │
                                                                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│  handleContainerExit (pkg/cri/server/events.go)                             │
│  ├── 附加容器 IO                                                             │
│  ├── 删除 task                                                               │
│  │      ↓                                                                    │
│  │   task.Delete(ctx, ...)                                                  │
│  │      ↓                                                                    │
│  ├── 更新容器状态                                                            │
│  │   ├── status.Pid = 0                                                     │
│  │   ├── status.FinishedAt = ...                                            │
│  │   └── status.ExitCode = ...                                              │
│  │      ↓                                                                    │
│  ├── (devbox) UpdateDevboxSnapshot 卸载 LVM                                 │
│  │      ↓                                                                    │
│  └── cntr.Stop()  ───────────────────────────────────────────────────┐      │
│         ↓                                                             │      │
│      关闭 container.Stopped() channel  ──────────────────────────────│──────│
└──────────────────────────────────────────────────────────────────────│──────┘
                                                                       │
                              waitContainerStop 收到信号，返回 nil ◄────┘
```

## 核心函数详解

### 1. StopContainer

**文件位置**: `pkg/cri/server/container_stop.go`

**调用者**: kubelet 通过 CRI gRPC 接口调用

**主要职责**:
- 作为 CRI 接口的入口点
- 协调停止流程
- 处理 NRI (Node Resource Interface) 回调
- 对于 devbox snapshotter，在停止后卸载 LVM

**关键代码**:
```go
func (c *criService) StopContainer(ctx context.Context, r *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
    // 1. 获取容器信息
    container, err := c.containerStore.Get(r.GetContainerId())
    
    // 2. 调用 stopContainer 执行实际停止逻辑
    if err := c.stopContainer(ctx, container, time.Duration(r.GetTimeout())*time.Second); err != nil {
        return nil, err
    }
    
    // 3. NRI 回调
    err = c.nri.StopContainer(ctx, &sandbox, &container)
    
    // 4. (devbox) 卸载 LVM
    if snapshotter == "devbox" {
        err = c.client.UpdateDevboxSnapshot(ctx, snapshotter, i.ID, unmountLvm, "true")
    }
    
    return &runtime.StopContainerResponse{}, nil
}
```

### 2. stopContainer

**文件位置**: `pkg/cri/server/container_stop.go`

**调用者**: `StopContainer`

**主要职责**:
- 发送停止信号 (SIGTERM/SIGKILL)
- 管理超时等待
- 与 `handleContainerExit` 通过 channel 协调

**信号发送流程**:

```
timeout > 0?
    ├── YES: 发送 SIGTERM
    │         ↓
    │       等待 timeout 秒
    │         ↓
    │       容器退出? ──YES──→ 返回成功
    │         ↓ NO
    │       发送 SIGKILL
    │         ↓
    │       等待容器退出
    │
    └── NO: 直接发送 SIGKILL
              ↓
            等待容器退出
```

**关键代码**:
```go
func (c *criService) stopContainer(ctx context.Context, container containerstore.Container, timeout time.Duration) error {
    // 发送 SIGTERM
    if timeout > 0 {
        task.Kill(ctx, sig)  // sig = SIGTERM
        
        // 等待 timeout 时间
        sigTermCtx, _ := context.WithTimeout(ctx, timeout)
        err = c.waitContainerStop(sigTermCtx, container)
        if err == nil {
            return nil  // 容器已退出
        }
    }
    
    // 发送 SIGKILL
    task.Kill(ctx, syscall.SIGKILL)
    
    // 等待容器退出
    err = c.waitContainerStop(ctx, container)
    return err
}
```

### 3. handleContainerExit

**文件位置**: `pkg/cri/server/events.go`

**调用者**: `eventMonitor` 在收到 `TaskExit` 事件时调用

**主要职责**:
- 清理容器 task
- 更新容器状态
- 通知等待者容器已退出
- 对于 devbox snapshotter，卸载 LVM

**关键代码**:
```go
func handleContainerExit(ctx context.Context, e *eventtypes.TaskExit, cntr containerstore.Container, ...) error {
    // 1. 获取并删除 task
    task, err := cntr.Container.Task(ctx, ...)
    task.Delete(ctx, ...)
    
    // 2. 更新容器状态
    cntr.Status.UpdateSync(func(status containerstore.Status) (containerstore.Status, error) {
        status.Pid = 0
        status.FinishedAt = protobuf.FromTimestamp(e.ExitedAt).UnixNano()
        status.ExitCode = int32(e.ExitStatus)
        
        // 3. (devbox) 卸载 LVM
        if container.Snapshotter == "devbox" {
            c.client.UpdateDevboxSnapshot(ctx, container.Snapshotter, container.ID, unmountLvm, "true")
        }
        return status, nil
    })
    
    // 4. 通知等待者
    cntr.Stop()  // 关闭 Stopped() channel
    
    return nil
}
```

### 4. waitContainerStop

**文件位置**: `pkg/cri/server/container_stop.go`

**作用**: 等待容器停止的同步点

**关键代码**:
```go
func (c *criService) waitContainerStop(ctx context.Context, container containerstore.Container) error {
    select {
    case <-ctx.Done():
        return fmt.Errorf("wait container %q: %w", container.ID, ctx.Err())
    case <-container.Stopped():
        return nil
    }
}
```

## 两个卸载 LVM 调用的原因

代码中有两处调用 `UpdateDevboxSnapshot` 卸载 LVM：

1. **`StopContainer`** (第 88-94 行)
2. **`handleContainerExit`** (第 451-456 行)

**原因**:
- `StopContainer` 中的调用：处理正常的 kubelet 调用停止容器的场景
- `handleContainerExit` 中的调用：处理容器异常退出或 OOM 等非正常退出场景

这种双重保险确保在任何情况下 LVM 都能被正确卸载。

## 超时时间说明

| 阶段 | 超时来源 | 典型值 |
|------|----------|--------|
| SIGTERM 等待 | `StopContainerRequest.Timeout` | 30-300 秒 (由 K8s `terminationGracePeriodSeconds` 决定) |
| SIGKILL 等待 | 原始 context (建议添加固定超时) | 依赖 gRPC context |

**注意**: 如果 `terminationGracePeriodSeconds` 设置过长（如 300 秒），容器可能需要等待 5 分钟才会被 SIGKILL。

## 常见问题

### Q: 容器停止时卡住很长时间？

可能原因：
1. `terminationGracePeriodSeconds` 设置过大
2. 容器进程在 D 状态（不可中断睡眠），无法响应信号
3. 磁盘/LV 空间满，导致进程无法完成退出前的写入操作

### Q: devbox LVM 没有被卸载？

检查：
1. 容器是否正常通过 StopContainer 退出
2. handleContainerExit 是否被正确调用
3. 查看 containerd 日志中是否有 `UpdateDevboxSnapshot` 调用记录

## 相关文件

- `pkg/cri/server/container_stop.go` - 停止容器主逻辑
- `pkg/cri/server/events.go` - 事件处理逻辑
- `pkg/cri/store/container/container.go` - 容器状态管理
- `snapshots/devbox/devbox.go` - devbox snapshotter 实现

