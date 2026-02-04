# 中间状态策略分析

## 一、核心问题

**当操作失败时，我们应该：**
1. **回退状态 + 回退操作**：状态从 `mounting` 回退到 `created`
2. **只回退操作，保持中间状态**：状态保持 `mounting`，但操作已回退

这两种策略各有优劣，需要根据实际需求选择。

---

## 二、方案对比

### 2.1 方案 A：回退状态和操作

**实现**：
```go
func (o *Snapshotter) mountLV(ctx, key, lvName, snapID) {
    // 标记为 mounting
    o.setLVState(ctx, lvName, LVStateMounting)
    
    // 尝试挂载（带重试）
    mountErr := o.mountWithRetry(ctx, devicePath, mountPoint, 3)
    
    if mountErr != nil {
        // ❌ 挂载失败（重试 3 次后）
        
        // ✅ 立即回退状态到 created
        o.setLVState(ctx, lvName, LVStateCreated)
        o.saveLVError(ctx, lvName, mountErr)
        return err
    }
    
    // ✅ 成功，标记为 mounted
    o.setLVState(ctx, lvName, LVStateMounted)
}
```

**时间线**：
```
T1: created → mounting (标记开始)
T2: 执行挂载（失败）
T3: mounting → created (回退状态)
    ↓
下次调用 Prepare：
T4: 检测到 created 状态 → 直接挂载
```

**优点**：
- ✅ 系统始终处于稳定状态（created, mounted 等）
- ✅ 容易理解和维护
- ✅ 重试逻辑简单（每次都从稳定状态开始）
- ✅ 不会有"卡住"的中间状态

**缺点**：
- ❌ 丢失了"曾经尝试过"的信息
- ❌ 无法区分"从未挂载"和"挂载失败后回退"
- ❌ 每次重试都需要重新检查状态

---

### 2.2 方案 B：只回退操作，保持中间状态

**实现**：
```go
func (o *Snapshotter) mountLV(ctx, key, lvName, snapID) {
    // 标记为 mounting
    o.setLVState(ctx, lvName, LVStateMounting)
    
    // 尝试挂载（带重试）
    mountErr := o.mountWithRetry(ctx, devicePath, mountPoint, 3)
    
    if mountErr != nil {
        // ❌ 挂载失败（重试 3 次后）
        
        // ✅ 保持 mounting 状态，但记录错误和重试次数
        o.saveLVError(ctx, lvName, mountErr)
        o.incrementRetryCount(ctx, lvName)
        return err
    }
    
    // ✅ 成功，标记为 mounted
    o.setLVState(ctx, lvName, LVStateMounted)
    o.clearRetryCount(ctx, lvName)
}
```

**时间线**：
```
T1: created → mounting (标记开始)
T2: 执行挂载（失败）
T3: 状态保持 mounting，记录错误
    ↓
下次调用 Prepare：
T4: 检测到 mounting 状态 + 有错误记录 → 重试挂载
```

**优点**：
- ✅ 保留了操作进度信息
- ✅ 可以看到"正在尝试挂载"
- ✅ 便于监控和调试（知道哪个环节出错）
- ✅ 可以记录重试次数，避免无限重试

**缺点**：
- ❌ 系统可能长期处于中间状态
- ❌ 需要额外的字段记录错误和重试次数
- ❌ 重试逻辑更复杂（需要判断是否有错误）
- ❌ 可能出现"卡住"的中间状态

---

## 三、推荐方案：混合策略

### 3.1 核心思想

**结合两种方案的优点**：
1. **短期内保持中间状态**（允许立即重试）
2. **长期回退到稳定状态**（避免系统卡住）

### 3.2 具体规则

```
规则 1：操作正在进行时，允许中间状态
规则 2：操作失败后立即重试（3次），保持中间状态
规则 3：重试全部失败后，回退到稳定状态
规则 4：下次外部调用时，从稳定状态重新开始
```

### 3.3 实现方案

#### 数据结构

```go
type LVInfo struct {
    Name         string
    State        string    // 当前状态
    PrevState    string    // 前一个稳定状态
    MountPoint   string
    
    // 错误和重试相关
    LastError    string    // 最后的错误信息
    ErrorTime    time.Time // 错误时间
    RetryCount   int       // 当前操作的重试次数
    
    // 用于判断是否需要回退
    OperationStartTime time.Time // 操作开始时间
}
```

#### 操作流程

```go
func (o *Snapshotter) mountLV(ctx, key, lvName, snapID) error {
    // ========== 第 1 步：标记开始挂载 ==========
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.PrevState = lv.State  // 记录前一个状态（created）
            lv.State = LVStateMounting
            lv.OperationStartTime = time.Now()
            lv.RetryCount = 0
        })
    })
    
    // ========== 第 2 步：挂载（带立即重试）==========
    var mountErr error
    for attempt := 1; attempt <= 3; attempt++ {
        log.G(ctx).Infof("Mounting LV %s (attempt %d/3)", lvName, attempt)
        
        mountErr = syscall.Mount(devicePath, mountPoint, "ext4", 0, "")
        
        if mountErr == nil {
            // ✅ 成功
            break
        }
        
        // ❌ 失败，更新重试次数（保持 mounting 状态）
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.RetryCount = attempt
                lv.LastError = mountErr.Error()
                lv.ErrorTime = time.Now()
            })
        })
        
        if attempt < 3 {
            time.Sleep(time.Second * time.Duration(attempt))
        }
    }
    
    // ========== 第 3 步：根据结果更新状态 ==========
    if mountErr != nil {
        // ❌ 重试 3 次全部失败
        log.G(ctx).WithError(mountErr).Error("Failed to mount LV after 3 retries")
        
        // ✅ 回退到稳定状态（created）
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
                lv.State = lv.PrevState  // 回退到 created
                lv.LastError = fmt.Sprintf("mount failed after %d retries: %v", 3, mountErr)
                lv.ErrorTime = time.Now()
            })
        })
        
        return fmt.Errorf("failed to mount LV: %w", mountErr)
    }
    
    // ✅ 成功，标记为 mounted
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.UpdateLV(ctx, lvName, func(lv *LVInfo) {
            lv.State = LVStateMounted
            lv.MountPoint = mountPoint
            lv.LastError = ""      // 清除错误
            lv.RetryCount = 0      // 清除重试次数
        })
    })
    
    return nil
}
```

#### 下次调用时的处理

```go
func (o *Snapshotter) Prepare(ctx, key, parent, opts) {
    currentState, lvInfo, _ := o.getLVState(ctx, lvName)
    
    switch currentState {
    case LVStateMounting:
        // 检测到中间状态
        
        // 判断：是否是进程崩溃导致的遗留状态？
        if time.Since(lvInfo.OperationStartTime) > 10*time.Minute {
            // 超过 10 分钟，认为是遗留状态（进程崩溃）
            log.G(ctx).Warn("Detected stale mounting state, will check actual state")
            
            // 检查实际状态
            isMounted, _ := lvm.IsMountPoint(mountPoint)
            if isMounted {
                // 实际已挂载，更新状态
                o.setLVState(ctx, lvName, LVStateMounted)
                return mountPoints
            } else {
                // 实际未挂载，回退到 created
                o.setLVState(ctx, lvName, LVStateCreated)
                // 继续挂载
                return o.mountLV(ctx, key, lvName, snapID)
            }
        } else {
            // 操作刚失败不久，检查是否有错误记录
            if lvInfo.LastError != "" {
                // 有错误记录，说明之前失败了
                log.G(ctx).Warnf("Previous mount attempt failed: %s, will retry", lvInfo.LastError)
                
                // 回退到 created，重新开始
                o.setLVState(ctx, lvName, LVStateCreated)
                return o.mountLV(ctx, key, lvName, snapID)
            } else {
                // 没有错误记录，可能正在挂载中（并发调用？）
                return nil, fmt.Errorf("LV is being mounted by another operation")
            }
        }
        
    case LVStateCreated:
        // 稳定状态，直接挂载
        return o.mountLV(ctx, key, lvName, snapID)
    }
}
```

---

## 四、详细对比：三种策略

### 4.1 策略对比表

| 维度 | 方案A：立即回退状态 | 方案B：保持中间状态 | 推荐方案：混合策略 |
|------|------------------|------------------|------------------|
| **中间状态存在时间** | 极短（几秒） | 可能很长 | 短期（几秒到几分钟） |
| **系统稳定性** | ✅ 高 | ❌ 低 | ✅ 高 |
| **操作进度可见** | ❌ 低 | ✅ 高 | ✅ 高 |
| **重试逻辑复杂度** | ✅ 简单 | ❌ 复杂 | 🔶 中等 |
| **监控和调试** | ❌ 难 | ✅ 易 | ✅ 易 |
| **并发安全** | ✅ 好 | ❌ 需要额外处理 | ✅ 好 |
| **实现复杂度** | ✅ 简单 | 🔶 中等 | ❌ 复杂 |

### 4.2 各策略的状态时间线

#### 方案 A：立即回退状态

```
T1  (0s):   created
T2  (0.1s): mounting (标记)
T3  (0.2s): mounting (挂载失败)
T4  (1.2s): mounting (重试1失败)
T5  (3.2s): mounting (重试2失败)
T6  (6.2s): mounting (重试3失败)
T7  (6.3s): created (立即回退) ✅

下次 Prepare 调用：
T8 (10s):   created → 直接挂载
```

**状态存在时间**：
- `mounting`: 6.3 秒
- `created`: 始终保持

---

#### 方案 B：保持中间状态

```
T1  (0s):   created
T2  (0.1s): mounting (标记)
T3  (0.2s): mounting (挂载失败，记录错误)
T4  (1.2s): mounting (重试1失败，记录错误)
T5  (3.2s): mounting (重试2失败，记录错误)
T6  (6.2s): mounting (重试3失败，记录错误)
T7  (6.3s): mounting (保持状态) ⚠️

下次 Prepare 调用（假设 1 小时后）：
T8 (3600s): mounting (仍然是 mounting!) ❌
            → 需要额外逻辑判断是否重试
```

**状态存在时间**：
- `mounting`: 可能长达数小时甚至更久 ⚠️
- 系统长期处于不稳定状态

---

#### 推荐方案：混合策略

```
T1  (0s):   created
T2  (0.1s): mounting (标记，记录 PrevState=created)
T3  (0.2s): mounting (挂载失败，RetryCount=1)
T4  (1.2s): mounting (重试1失败，RetryCount=2)
T5  (3.2s): mounting (重试2失败，RetryCount=3)
T6  (6.2s): mounting (重试3失败)
T7  (6.3s): created (回退到稳定状态) ✅

下次 Prepare 调用：
T8 (10s):   created → 直接挂载 ✅

进程崩溃后重启：
T1 (启动):  mounting (检测到遗留状态)
            → 检查 OperationStartTime (可能是昨天的)
            → 检查实际状态
            → 回退或前进到稳定状态
```

**状态存在时间**：
- `mounting`: 最长几秒到几分钟（操作进行中 + 立即重试）
- 操作完成（成功或失败）后，立即回到稳定状态

---

## 五、具体场景分析

### 5.1 场景 1：正常挂载

```
方案 A & 推荐方案（相同）：
created → mounting (0.1s) → mounted ✅

方案 B（相同）：
created → mounting (0.1s) → mounted ✅
```

**结论**：三种方案相同，都能正常工作。

---

### 5.2 场景 2：挂载失败，10秒后重试

```
方案 A：
T1:  created → mounting → (失败3次) → created
T10: created → mounting → mounted ✅

方案 B：
T1:  created → mounting → (失败3次，记录错误) → mounting
T10: mounting (需要检查 LastError) → 清除状态 → mounting → mounted ✅

推荐方案：
T1:  created → mounting → (失败3次) → created
T10: created → mounting → mounted ✅
```

**结论**：
- 方案 A 和推荐方案简单明了
- 方案 B 需要额外逻辑判断

---

### 5.3 场景 3：挂载时进程崩溃

```
方案 A：
T1: created → mounting → (进程崩溃)
重启后: mounting (遗留状态)
       → 需要检查实际状态 → 回退或前进

方案 B：
T1: created → mounting → (进程崩溃)
重启后: mounting (遗留状态)
       → 需要检查实际状态 → 回退或前进

推荐方案：
T1: created → mounting (OperationStartTime=T1) → (进程崩溃)
重启后: mounting + OperationStartTime 很久之前
       → 检测到遗留状态
       → 检查实际状态 → 回退或前进 ✅
```

**结论**：
- 推荐方案可以通过 `OperationStartTime` 自动检测遗留状态
- 方案 A 和 B 都需要启动时检查

---

### 5.4 场景 4：Kubelet 频繁重试（5秒间隔）

```
方案 A & 推荐方案：
T0:  Prepare → created → mounting → 失败 → created
T5:  Prepare → created → mounting → 失败 → created
T10: Prepare → created → mounting → 失败 → created
T15: Prepare → created → mounting → 成功 → mounted ✅

每次都从 created 开始，逻辑清晰

方案 B：
T0:  Prepare → created → mounting → 失败 → mounting (错误)
T5:  Prepare → mounting (有错误) → 需要判断 → 清除 → mounting → 失败 → mounting
T10: Prepare → mounting (有错误) → 需要判断 → 清除 → mounting → 失败 → mounting
T15: Prepare → mounting (有错误) → 需要判断 → 清除 → mounting → 成功 → mounted

需要额外逻辑判断 LastError，清除状态
```

**结论**：
- 方案 A 和推荐方案更适合频繁重试的场景
- 方案 B 需要复杂的状态清除逻辑

---

## 六、最终推荐

### 6.1 推荐：混合策略

**核心原则**：
1. **操作进行中**：允许中间状态（mounting, unmounting 等）
2. **立即重试期间**：保持中间状态，记录重试次数
3. **重试全部失败后**：立即回退到稳定状态
4. **下次外部调用**：从稳定状态开始

**关键字段**：
```go
type LVInfo struct {
    State              string    // 当前状态
    PrevState          string    // 前一个稳定状态（用于回退）
    OperationStartTime time.Time // 操作开始时间（用于检测遗留状态）
    LastError          string    // 最后的错误
    RetryCount         int       // 重试次数
}
```

### 6.2 为什么不用方案 A（立即回退）？

**看起来更简单，但有隐藏问题**：
1. 丢失操作进度信息（不知道在哪个环节失败）
2. 无法记录重试次数（可能无限重试）
3. 监控困难（看不到"正在挂载"的状态）

### 6.3 为什么不用方案 B（保持中间状态）？

**看起来信息更丰富，但有严重问题**：
1. 系统可能长期处于中间状态（不稳定）
2. 重试逻辑复杂（需要判断 LastError、RetryCount 等）
3. 容易出现"卡住"的状态

### 6.4 混合策略的优势

**结合两者优点**：
- ✅ 操作进行中可以看到进度（mounting）
- ✅ 失败后立即回到稳定状态（created）
- ✅ 记录了重试次数和错误信息
- ✅ 可以检测遗留状态（OperationStartTime）
- ✅ 重试逻辑清晰（每次从稳定状态开始）

---

## 七、总结

### 7.1 答案

**是否允许中间状态存在？**

✅ **允许，但有条件**：
1. 操作进行中：允许（几秒到几分钟）
2. 操作失败后：不允许，立即回退到稳定状态
3. 进程崩溃后：启动时检测并清理

### 7.2 应该如何处理失败？

**推荐：回退状态 + 回退操作**

```go
// 操作失败后
if err != nil {
    // 1. 回退操作（已经做了，因为操作失败）
    
    // 2. 回退状态（关键）
    o.setLVState(ctx, lvName, prevStableState)
    
    // 3. 记录错误信息（用于调试）
    o.saveLVError(ctx, lvName, err.Error())
}
```

### 7.3 实施建议

**Phase 1**：实现基本的回退机制
- 操作失败后立即回退状态
- 记录错误信息

**Phase 2**：增强监控和调试
- 记录 `OperationStartTime`
- 记录 `RetryCount`
- 检测遗留状态

**Phase 3**：优化重试策略
- 指数退避
- 可配置的重试次数
- 智能重试（根据错误类型）

