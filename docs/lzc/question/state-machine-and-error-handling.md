# 状态机制如何解决核心问题 & 错误处理策略

## 一、状态机制如何解决核心问题

### 1.1 当前架构的核心问题回顾

**问题**：慢速操作在事务内执行，导致数据库锁被长时间持有

```go
// ❌ 当前的问题代码
func (o *Snapshotter) Update(ctx context.Context, info snapshots.Info) error {
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        if value, ok := info.Labels[unmountLvm]; ok && value == "true" {
            mountPath, err := storage.SetUnmountedWithKey(ctx, info.Name)
            
            // ❌ 问题：unmount 在事务内，可能需要 5 分钟
            if err = o.unmountLvm(ctx, mountPath); err != nil {
                return err
            }
        }
        return nil
    })
    // ⚠️ 事务持有锁的时间 = 5 分钟
    // ⚠️ 其他操作被阻塞 5 分钟
}
```

**时间线**：
```
T1: 事务开始（锁定数据库）
T2: 更新元数据（快速，几毫秒）
T3: 执行 unmount（慢速，5 分钟）⚠️
T4: 事务提交（解锁数据库）

问题：T1-T4 期间（5 分钟），数据库被锁定
```

---

### 1.2 状态机制如何解决

**核心思想**：将长操作拆分成多个短事务，每个事务只更新状态

```go
// ✅ 状态机制的解决方案
func (o *Snapshotter) Update(ctx context.Context, info snapshots.Info) error {
    var needUnmount bool
    var lvName string
    var mountPath string
    
    // 事务 1：查询信息并标记"开始卸载"（快速，几毫秒）
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        if value, ok := info.Labels[unmountLvm]; ok && value == "true" {
            mountPath, err = storage.SetUnmountedWithKey(ctx, info.Name)
            if err != nil {
                return err
            }
            lvName = getLVName(...)
            
            // ✅ 只标记状态，不执行操作
            storage.SetLVState(ctx, lvName, "unmounting")
            needUnmount = true
        }
        return nil
    })
    // ✅ 事务 1 结束，数据库解锁
    
    if err != nil || !needUnmount {
        return err
    }
    
    // ✅ 慢速操作在事务外执行（不持有数据库锁）
    unmountErr := syscall.Unmount(mountPath, 0)  // 可能需要 5 分钟，但数据库已解锁
    
    // 事务 2：更新最终状态（快速，几毫秒）
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        if unmountErr == nil {
            storage.SetLVState(ctx, lvName, "unmounted")
        } else {
            storage.SetLVState(ctx, lvName, "faulty")
            storage.SaveError(ctx, lvName, unmountErr.Error())
        }
        return nil
    })
    // ✅ 事务 2 结束，数据库解锁
    
    return unmountErr
}
```

**时间线对比**：

```
旧架构：
T1: 事务开始（锁定数据库）━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓
T2: 更新元数据（几毫秒）                                            ┃
T3: unmount（5 分钟）⚠️ 数据库被锁定                                  ┃ 5 分钟
T4: 事务提交（解锁数据库）━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛

新架构：
T1: 事务 1 开始（锁定数据库）━━┓
T2: 标记状态 "unmounting"      ┃ 几毫秒
T3: 事务 1 提交（解锁数据库）━━┛
    ↓
T4: unmount（5 分钟）✅ 数据库未锁定
    ↓
T5: 事务 2 开始（锁定数据库）━━┓
T6: 更新状态 "unmounted"       ┃ 几毫秒
T7: 事务 2 提交（解锁数据库）━━┛

数据库锁定时间：T1-T3（几毫秒）+ T5-T7（几毫秒）= 总共几十毫秒
```

---

### 1.3 关键问题的解决

#### 问题 1：事务持有锁太久

**解决**：
- 事务 1 只标记状态（几毫秒）
- 慢速操作在事务外（数据库已解锁）
- 事务 2 只更新最终状态（几毫秒）

**效果**：
- 数据库锁定时间从 5 分钟降到几十毫秒
- 其他操作不会被长时间阻塞

#### 问题 2：其他操作被阻塞

**解决**：
- 由于数据库很快解锁，其他操作几乎不会被阻塞
- 如果其他操作看到 "unmounting" 状态，可以：
  - 等待（如果需要这个 LV）
  - 跳过（如果不需要）
  - 返回错误（如果不允许此时操作）

#### 问题 3：状态不一致

**解决**：
- 中间状态（"unmounting"）明确表示"操作正在进行"
- 启动时可以检测中间状态并恢复
- 有错误状态（"faulty"）记录失败的操作

---

## 二、错误处理策略

### 2.1 错误场景分析

#### 场景 1：第一步失败（事务 1 失败）

```go
func (o *Snapshotter) unmountLVM(ctx context.Context, lvName string) error {
    // 事务 1：标记"正在卸载"
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // ❌ 假设这里失败（例如数据库错误）
        return storage.SetLVState(ctx, lvName, "unmounting")
    })
    
    if err != nil {
        // ✅ 事务 1 失败，直接返回
        // ✅ 数据库状态未改变，保持原来的状态
        return fmt.Errorf("failed to mark LV as unmounting: %w", err)
    }
    
    // 这里不会执行
    // ...
}
```

**处理**：
- 事务 1 失败，事务回滚
- 数据库状态保持不变
- 返回错误给调用者
- **结果**：干净的失败，可以重试

---

#### 场景 2：中间步骤失败（慢速操作失败）

```go
func (o *Snapshotter) unmountLVM(ctx context.Context, lvName string) error {
    // 事务 1：标记"正在卸载"（成功）
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "unmounting")
    })
    // ✅ 事务 1 成功提交，状态 = "unmounting"
    
    // 慢速操作
    unmountErr := syscall.Unmount(mountPath, 0)
    if unmountErr != nil {
        // ❌ unmount 失败
        
        // 事务 2：标记为"错误"
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            storage.SetLVState(ctx, lvName, "faulty")
            storage.SaveError(ctx, lvName, unmountErr.Error())
            storage.SetErrorTime(ctx, lvName, time.Now())
            return nil
        })
        // ✅ 错误信息已记录
        
        return fmt.Errorf("failed to unmount LV %s: %w", lvName, unmountErr)
    }
    
    // 事务 2：标记"已卸载"（成功）
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "unmounted")
    })
    
    return nil
}
```

**处理流程**：

```
T1: 状态 = "mounted"
    ↓
T2: 事务 1 成功 → 状态 = "unmounting"
    ↓
T3: unmount 失败 ❌
    ↓
T4: 事务 2 标记错误 → 状态 = "faulty"
    ↓
T5: 返回错误
```

**恢复策略**：

1. **记录错误信息**：
   ```go
   type LVInfo struct {
       Name       string
       State      string
       Error      string      // 错误信息
       ErrorTime  time.Time   // 错误时间
       RetryCount int         // 重试次数
   }
   ```

2. **后续处理**：
   - Cleanup 时检测到 "faulty" 状态
   - 可以重试（如果是临时错误）
   - 或标记为需要人工处理（如果是持久错误）

---

#### 场景 3：最后步骤失败（事务 2 失败）

```go
func (o *Snapshotter) unmountLVM(ctx context.Context, lvName string) error {
    // 事务 1：标记"正在卸载"（成功）
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "unmounting")
    })
    // ✅ 状态 = "unmounting"
    
    // 慢速操作（成功）
    unmountErr := syscall.Unmount(mountPath, 0)
    // ✅ unmount 成功，设备已卸载
    
    // 事务 2：更新最终状态
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // ❌ 假设这里失败（例如数据库写入错误）
        return storage.SetLVState(ctx, lvName, "unmounted")
    })
    
    if err != nil {
        // ⚠️ 问题：unmount 成功了，但状态更新失败
        // ⚠️ 数据库状态 = "unmounting"，但实际已卸载
        log.Errorf("unmount succeeded but failed to update state: %v", err)
        return err
    }
    
    return nil
}
```

**问题**：
- 操作成功，但状态更新失败
- 数据库状态（"unmounting"）与实际状态（已卸载）不一致

**解决方案 1：重试机制**

```go
func (o *Snapshotter) unmountLVM(ctx context.Context, lvName string) error {
    // 事务 1：标记"正在卸载"
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "unmounting")
    })
    
    // 慢速操作
    unmountErr := syscall.Unmount(mountPath, 0)
    
    // 事务 2：更新最终状态（带重试）
    maxRetries := 3
    var err error
    for i := 0; i < maxRetries; i++ {
        err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            if unmountErr == nil {
                return storage.SetLVState(ctx, lvName, "unmounted")
            } else {
                storage.SetLVState(ctx, lvName, "faulty")
                storage.SaveError(ctx, lvName, unmountErr.Error())
                return nil
            }
        })
        
        if err == nil {
            break  // 成功
        }
        
        log.Warnf("failed to update LV state (attempt %d/%d): %v", i+1, maxRetries, err)
        time.Sleep(100 * time.Millisecond)
    }
    
    if err != nil {
        // ⚠️ 重试 3 次都失败
        // 记录到日志，启动时检查
        log.Errorf("unmount succeeded but failed to update state after %d retries: %v", maxRetries, err)
    }
    
    return unmountErr  // 返回操作的结果，而不是状态更新的结果
}
```

**解决方案 2：启动时检查**

```go
func (o *Snapshotter) ensureLVStates(ctx context.Context) error {
    var inconsistentLVs []*LVInfo
    
    // 遍历所有 LV
    o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
        return storage.WalkLVs(ctx, func(info *LVInfo) error {
            // 检查中间状态
            if info.State == "unmounting" {
                // ⚠️ LV 处于"正在卸载"状态
                // 检查实际状态
                if isMounted(info.Name) {
                    // 实际还在挂载，可能操作失败了
                    log.Warnf("LV %s marked as unmounting but still mounted, will retry", info.Name)
                    inconsistentLVs = append(inconsistentLVs, info)
                } else {
                    // 实际已卸载，更新状态
                    log.Infof("LV %s was unmounted but state not updated, fixing", info.Name)
                    storage.SetLVState(ctx, info.Name, "unmounted")
                }
            }
            return nil
        })
    })
    
    // 重试不一致的操作
    for _, lv := range inconsistentLVs {
        // 重新尝试 unmount
        // ...
    }
    
    return nil
}
```

---

#### 场景 4：进程崩溃

```go
// 操作执行到一半，进程崩溃
func (o *Snapshotter) unmountLVM(ctx context.Context, lvName string) error {
    // 事务 1：标记"正在卸载"（✅ 成功）
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "unmounting")
    })
    // 状态 = "unmounting"
    
    // 慢速操作开始
    unmountErr := syscall.Unmount(mountPath, 0)
    
    // 💥 进程崩溃！（可能在 unmount 成功后，状态更新前）
    
    // 这里不会执行
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "unmounted")
    })
}
```

**重启后的状态**：
- 数据库状态 = "unmounting"
- 实际状态 = 可能已卸载，也可能未卸载

**启动时检查并恢复**：

```go
func (o *Snapshotter) New(ctx context.Context, root string, config *Config) (*Snapshotter, error) {
    // ... 初始化 ...
    
    // ✅ 启动时检查并修复状态
    if err := s.ensureLVStates(ctx); err != nil {
        return nil, fmt.Errorf("failed to ensure LV states: %w", err)
    }
    
    return s, nil
}

func (o *Snapshotter) ensureLVStates(ctx context.Context) error {
    log.Info("Checking LV states on startup...")
    
    var toFix []*LVInfo
    
    // 检查所有中间状态
    o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
        return storage.WalkLVs(ctx, func(info *LVInfo) error {
            switch info.State {
            case "creating", "mounting", "unmounting", "removing":
                // ⚠️ 中间状态 = 操作被中断
                log.Warnf("LV %s in intermediate state '%s', will check actual state", 
                    info.Name, info.State)
                toFix = append(toFix, info)
            }
            return nil
        })
    })
    
    // 修复每个中间状态的 LV
    for _, lv := range toFix {
        if err := o.fixLVState(ctx, lv); err != nil {
            log.Errorf("Failed to fix LV %s: %v", lv.Name, err)
        }
    }
    
    return nil
}

func (o *Snapshotter) fixLVState(ctx context.Context, lv *LVInfo) error {
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lv.Name)
    
    // 检查实际状态
    deviceExists := fileExists(devicePath)
    isMounted := false
    if deviceExists {
        isMounted, _ = lvm.IsMountPoint(getMountPath(lv.Name))
    }
    
    // 根据中间状态和实际状态决定最终状态
    var finalState string
    switch lv.State {
    case "creating":
        if deviceExists {
            finalState = "created"
            log.Infof("LV %s creation completed, updating state", lv.Name)
        } else {
            finalState = "faulty"
            log.Warnf("LV %s marked as creating but device doesn't exist", lv.Name)
        }
        
    case "mounting":
        if isMounted {
            finalState = "mounted"
            log.Infof("LV %s mount completed, updating state", lv.Name)
        } else if deviceExists {
            finalState = "created"
            log.Warnf("LV %s marked as mounting but not mounted, reverting to created", lv.Name)
        } else {
            finalState = "faulty"
            log.Warnf("LV %s marked as mounting but device doesn't exist", lv.Name)
        }
        
    case "unmounting":
        if !isMounted {
            finalState = "unmounted"
            log.Infof("LV %s unmount completed, updating state", lv.Name)
        } else {
            finalState = "mounted"
            log.Warnf("LV %s marked as unmounting but still mounted, reverting", lv.Name)
        }
        
    case "removing":
        if !deviceExists {
            finalState = "removed"
            log.Infof("LV %s removal completed, updating state", lv.Name)
        } else {
            finalState = "faulty"
            log.Warnf("LV %s marked as removing but device still exists", lv.Name)
        }
    }
    
    // 更新到最终状态
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lv.Name, finalState)
    })
}
```

---

### 2.2 完整的错误处理流程

#### 流程图

```
开始操作
    ↓
┌─────────────────┐
│ 事务 1：        │
│ 标记中间状态     │
│ (creating/      │
│  mounting/etc)  │
└────────┬────────┘
         │ 成功
         ↓
┌─────────────────┐
│ 慢速操作        │ ← 可能失败
│ (在事务外)      │
└────────┬────────┘
         │
    ┌────┴────┐
    │         │
   成功      失败
    │         │
    ↓         ↓
┌───────┐ ┌───────┐
│事务 2 │ │事务 2 │
│标记   │ │标记   │
│成功   │ │faulty │
└───────┘ └───────┘
    │         │
    └────┬────┘
         ↓
      返回结果
```

#### 各阶段错误处理

| 阶段 | 失败场景 | 数据库状态 | 实际状态 | 处理方式 |
|------|---------|-----------|---------|---------|
| 事务 1 | 标记失败 | 原状态 | 原状态 | 返回错误，可重试 |
| 慢速操作 | 操作失败 | 中间状态 | 原状态 | 标记 faulty，记录错误 |
| 事务 2 | 更新失败 | 中间状态 | 新状态 | 重试；启动时检查 |
| 进程崩溃 | 中断 | 中间状态 | 未知 | 启动时检查并恢复 |

---

### 2.3 错误恢复策略

#### 策略 1：立即重试（适用于临时错误）

```go
func (o *Snapshotter) unmountLVMWithRetry(ctx context.Context, lvName string, maxRetries int) error {
    var lastErr error
    
    for i := 0; i < maxRetries; i++ {
        lastErr = o.unmountLVM(ctx, lvName)
        if lastErr == nil {
            return nil  // 成功
        }
        
        // 检查是否是可重试的错误
        if !isRetriableError(lastErr) {
            break  // 不可重试，直接返回
        }
        
        log.Warnf("unmount failed (attempt %d/%d): %v", i+1, maxRetries, lastErr)
        time.Sleep(time.Second * time.Duration(i+1))  // 指数退避
    }
    
    return lastErr
}

func isRetriableError(err error) bool {
    // 临时错误，可以重试
    return strings.Contains(err.Error(), "device busy") ||
           strings.Contains(err.Error(), "resource temporarily unavailable")
}
```

#### 策略 2：异步重试（Cleanup 时）

```go
func (o *Snapshotter) Cleanup(ctx context.Context) error {
    var faultyLVs []*LVInfo
    
    // 查找处于 faulty 状态的 LV
    o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
        return storage.WalkLVs(ctx, func(info *LVInfo) error {
            if info.State == "faulty" {
                // 检查错误时间，如果太久就放弃
                if time.Since(info.ErrorTime) < 24*time.Hour {
                    faultyLVs = append(faultyLVs, info)
                }
            }
            return nil
        })
    })
    
    // 重试失败的操作
    for _, lv := range faultyLVs {
        if lv.RetryCount >= 5 {
            log.Errorf("LV %s has failed %d times, giving up", lv.Name, lv.RetryCount)
            continue
        }
        
        log.Infof("Retrying failed operation for LV %s (attempt %d)", lv.Name, lv.RetryCount+1)
        
        // 根据之前的状态重试操作
        var err error
        switch lv.LastAttemptedState {
        case "unmounting":
            err = o.unmountLVM(ctx, lv.Name)
        case "removing":
            err = o.removeLV(ctx, lv.Name)
        }
        
        if err != nil {
            // 更新重试次数
            o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
                lv.RetryCount++
                lv.ErrorTime = time.Now()
                return storage.UpdateLV(ctx, lv)
            })
        }
    }
    
    return nil
}
```

#### 策略 3：人工介入标记

```go
// 对于无法自动恢复的错误，提供工具函数
func (o *Snapshotter) MarkLVForManualCleanup(ctx context.Context, lvName string, reason string) error {
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "manual_cleanup_required")
        storage.SaveNote(ctx, lvName, reason)
    })
}

// 管理员可以通过 CLI 工具查看需要人工处理的 LV
// $ devbox-admin list-faulty-lvs
// $ devbox-admin fix-lv <lv-name> --force
```

---

## 三、完整示例：创建 LV 的完整流程

### 3.1 正常流程

```go
func (o *Snapshotter) createAndMountLV(ctx context.Context, contentKey string, capacity string) (string, string, error) {
    lvName := "devbox-" + contentKey
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    mountPoint := "/path/to/mount"
    
    // ========== 步骤 1：标记"正在创建" ==========
    log.Infof("Step 1: Marking LV %s as creating", lvName)
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.AddLV(ctx, &LVInfo{
            Name:        lvName,
            State:       "creating",
            ContentKey:  contentKey,
            Capacity:    capacity,
            CreatedTime: time.Now(),
        })
    })
    if err != nil {
        return "", lvName, fmt.Errorf("failed to mark LV as creating: %w", err)
    }
    
    // ========== 步骤 2：创建 LV（慢速，事务外） ==========
    log.Infof("Step 2: Creating LV %s", lvName)
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{Capacity: capacity, VolGroup: o.lvmVgName},
    }
    
    if err := lvm.CreateVolume(ctx, vol); err != nil {
        // ❌ 创建失败
        log.Errorf("Failed to create LV %s: %v", lvName, err)
        
        // 标记为错误
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            storage.SetLVState(ctx, lvName, "faulty")
            storage.SaveError(ctx, lvName, err.Error())
            return nil
        })
        
        return "", lvName, fmt.Errorf("failed to create LV: %w", err)
    }
    
    // ========== 步骤 3：标记"已创建" ==========
    log.Infof("Step 3: Marking LV %s as created", lvName)
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "created")
    })
    
    // ========== 步骤 4：格式化文件系统（慢速，事务外） ==========
    log.Infof("Step 4: Formatting LV %s", lvName)
    if err := mkfs(devicePath); err != nil {
        // ❌ 格式化失败
        log.Errorf("Failed to format LV %s: %v", lvName, err)
        
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            storage.SetLVState(ctx, lvName, "faulty")
            storage.SaveError(ctx, lvName, err.Error())
            return nil
        })
        
        return "", lvName, fmt.Errorf("failed to format LV: %w", err)
    }
    
    // ========== 步骤 5：标记"正在挂载" ==========
    log.Infof("Step 5: Marking LV %s as mounting", lvName)
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "mounting")
    })
    
    // ========== 步骤 6：挂载（慢速，事务外） ==========
    log.Infof("Step 6: Mounting LV %s to %s", lvName, mountPoint)
    if err := syscall.Mount(devicePath, mountPoint, "ext4", 0, ""); err != nil {
        // ❌ 挂载失败
        log.Errorf("Failed to mount LV %s: %v", lvName, err)
        
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            storage.SetLVState(ctx, lvName, "faulty")
            storage.SaveError(ctx, lvName, err.Error())
            return nil
        })
        
        return "", lvName, fmt.Errorf("failed to mount LV: %w", err)
    }
    
    // ========== 步骤 7：标记"已挂载" ==========
    log.Infof("Step 7: Marking LV %s as mounted", lvName)
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        storage.SetLVState(ctx, lvName, "mounted")
        storage.SetMountPoint(ctx, lvName, mountPoint)
        return nil
    })
    
    log.Infof("Successfully created and mounted LV %s", lvName)
    return mountPoint, lvName, nil
}
```

### 3.2 各步骤失败的处理

| 步骤 | 失败时数据库状态 | 失败时实际状态 | 恢复方式 |
|------|----------------|---------------|---------|
| 1 | 无记录 | 无设备 | 直接返回错误，可重试整个流程 |
| 2 | "creating" | 可能有半成品 | 标记 faulty；启动时检查并清理 |
| 3 | "creating" | 已创建 | 启动时检测到设备存在，更新为 "created" |
| 4 | "created" | 已创建但未格式化 | 标记 faulty；可重试格式化 |
| 5 | "created" | 已格式化但未挂载 | 启动时检查，状态正确 |
| 6 | "mounting" | 已格式化但未挂载 | 标记 faulty；启动时重试挂载 |
| 7 | "mounting" | 已挂载 | 启动时检测到已挂载，更新为 "mounted" |

---

## 四、改进方案：主动回退机制（不依赖重启）

### 4.1 核心改进点

**❌ 旧方案的问题**：
- 失败后只记录错误，标记 "faulty"
- 依赖 Cleanup 或重启来处理
- 可能长时间处于不一致状态

**✅ 新方案的改进**：
- 失败后立即回退到上一个稳定状态
- 不依赖 Cleanup 或重启
- 保持系统一致性

---

### 4.2 完整的状态转换图

```
LV 生命周期状态转换：

初始化阶段：
  [不存在] → [creating] → [created] ← 回退点 1
                ↓ 失败
              [不存在]（已清理）

挂载阶段：
  [created] → [formatting] → [formatted] → [mounting] → [mounted] ← 回退点 2
      ↑          ↓ 失败           ↓ 失败        ↓ 失败        
      └──────────┴────────────────┴─────────────┘
                回退到 created 状态

使用阶段：
  [mounted] → [resizing] → [mounted]
                ↓ 失败
              [mounted]（保持原状）

卸载阶段：
  [mounted] → [unmounting] → [unmounted] ← 回退点 3
                ↓ 失败
              [mounted]（回退）

删除阶段：
  [unmounted] → [removing] → [removed]
                  ↓ 失败
                [unmounted]（回退）

错误状态（需要人工介入）：
  任何状态 → [faulty]（仅当无法自动回退时）
```

---

### 4.3 详细的状态定义

#### 稳定状态（Stable States）

| 状态 | 含义 | 数据库状态 | 实际状态 | 可执行操作 |
|------|------|-----------|---------|-----------|
| `none` | 不存在 | 无记录 | 无 LV | 创建 |
| `created` | 已创建 | 有记录 | LV 存在，未格式化 | 格式化、删除 |
| `formatted` | 已格式化 | 有记录 | LV 已格式化，未挂载 | 挂载、删除 |
| `mounted` | 已挂载 | 有记录 | LV 已挂载 | 使用、卸载、扩容 |
| `unmounted` | 已卸载 | 有记录 | LV 存在但未挂载 | 重新挂载、删除 |
| `removed` | 已删除 | 标记删除 | LV 不存在 | 清理元数据 |

#### 中间状态（Transient States）

| 状态 | 含义 | 前置状态 | 目标状态 | 失败回退到 |
|------|------|---------|---------|-----------|
| `creating` | 正在创建 | none | created | none（清理） |
| `formatting` | 正在格式化 | created | formatted | created |
| `mounting` | 正在挂载 | formatted | mounted | formatted |
| `unmounting` | 正在卸载 | mounted | unmounted | mounted |
| `resizing` | 正在扩容 | mounted | mounted | mounted |
| `removing` | 正在删除 | unmounted | removed | unmounted |

#### 错误状态（Error States）

| 状态 | 含义 | 何时使用 | 恢复方式 |
|------|------|---------|---------|
| `faulty` | 错误 | 无法自动回退时 | 人工介入或 Cleanup 重试 |
| `stuck` | 卡住 | 操作超时 | 强制回退或人工介入 |

---

### 4.4 带回退机制的完整实现

#### 示例 1：创建并挂载 LV（带回退）

```go
func (o *Snapshotter) createAndMountLV(ctx context.Context, contentKey string, capacity string) (string, string, error) {
    lvName := "devbox-" + contentKey
    devicePath := fmt.Sprintf("/dev/%s/%s", o.lvmVgName, lvName)
    mountPoint := "/path/to/mount"
    
    // ========== 步骤 1：标记"正在创建" ==========
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.AddLV(ctx, &LVInfo{
            Name:       lvName,
            State:      "creating",
            PrevState:  "none",  // 记录前一个状态，用于回退
        })
    })
    if err != nil {
        return "", lvName, fmt.Errorf("failed to mark as creating: %w", err)
    }
    
    // ========== 步骤 2：创建 LV ==========
    vol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{Name: lvName},
        Spec:       apis.VolumeInfo{Capacity: capacity, VolGroup: o.lvmVgName},
    }
    
    createErr := lvm.CreateVolume(ctx, vol)
    if createErr != nil {
        // ❌ 创建失败
        log.Errorf("Failed to create LV %s: %v", lvName, createErr)
        
        // ✅ 立即回退：删除元数据记录
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            return storage.RemoveLV(ctx, lvName)
        })
        
        // 尝试清理可能创建的残留
        _ = lvm.ForceDestroyVolume(ctx, vol)
        
        return "", lvName, fmt.Errorf("failed to create LV: %w", createErr)
    }
    
    // ========== 步骤 3：标记"已创建" ==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "created")
    })
    
    // ========== 步骤 4：标记"正在格式化" ==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "formatting")
    })
    
    // ========== 步骤 5：格式化 ==========
    formatErr := mkfs(devicePath)
    if formatErr != nil {
        // ❌ 格式化失败
        log.Errorf("Failed to format LV %s: %v", lvName, formatErr)
        
        // ✅ 立即回退：标记回 "created" 状态
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            storage.SetLVState(ctx, lvName, "created")
            storage.SaveError(ctx, lvName, formatErr.Error())
            return nil
        })
        
        return "", lvName, fmt.Errorf("failed to format LV: %w", formatErr)
    }
    
    // ========== 步骤 6：标记"已格式化" ==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "formatted")
    })
    
    // ========== 步骤 7：标记"正在挂载" ==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "mounting")
    })
    
    // ========== 步骤 8：挂载（带重试） ==========
    mountErr := o.mountWithRetry(ctx, devicePath, mountPoint, 3)
    if mountErr != nil {
        // ❌ 挂载失败（重试 3 次后仍失败）
        log.Errorf("Failed to mount LV %s after retries: %v", lvName, mountErr)
        
        // ✅ 立即回退：标记回 "formatted" 状态
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            storage.SetLVState(ctx, lvName, "formatted")
            storage.SaveError(ctx, lvName, mountErr.Error())
            return nil
        })
        
        return "", lvName, fmt.Errorf("failed to mount LV: %w", mountErr)
    }
    
    // ========== 步骤 9：标记"已挂载" ==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        storage.SetLVState(ctx, lvName, "mounted")
        storage.SetMountPoint(ctx, lvName, mountPoint)
        storage.ClearError(ctx, lvName)  // 清除之前的错误（如果有）
        return nil
    })
    
    return mountPoint, lvName, nil
}

// 带重试的挂载
func (o *Snapshotter) mountWithRetry(ctx context.Context, devicePath, mountPoint string, maxRetries int) error {
    var lastErr error
    for i := 0; i < maxRetries; i++ {
        lastErr = syscall.Mount(devicePath, mountPoint, "ext4", 0, "")
        if lastErr == nil {
            return nil
        }
        log.Warnf("Mount failed (attempt %d/%d): %v", i+1, maxRetries, lastErr)
        time.Sleep(time.Second * time.Duration(i+1))
    }
    return lastErr
}
```

#### 示例 2：卸载 LV（带回退）

```go
func (o *Snapshotter) unmountLV(ctx context.Context, lvName string) error {
    // ========== 步骤 1：标记"正在卸载" ==========
    err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        return storage.SetLVState(ctx, lvName, "unmounting")
    })
    if err != nil {
        return fmt.Errorf("failed to mark as unmounting: %w", err)
    }
    
    // ========== 步骤 2：获取挂载点 ==========
    var mountPoint string
    o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
        lv, err := storage.GetLV(ctx, lvName)
        if err == nil {
            mountPoint = lv.MountPoint
        }
        return nil
    })
    
    // ========== 步骤 3：卸载（带重试） ==========
    unmountErr := o.unmountWithRetry(ctx, mountPoint, 3)
    
    if unmountErr != nil {
        // ❌ 卸载失败（重试 3 次后仍失败）
        log.Errorf("Failed to unmount LV %s after retries: %v", lvName, unmountErr)
        
        // ✅ 立即回退：标记回 "mounted" 状态
        o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
            storage.SetLVState(ctx, lvName, "mounted")
            storage.SaveError(ctx, lvName, unmountErr.Error())
            storage.IncrementRetryCount(ctx, lvName)
            return nil
        })
        
        return fmt.Errorf("failed to unmount LV: %w", unmountErr)
    }
    
    // ========== 步骤 4：标记"已卸载" ==========
    o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        storage.SetLVState(ctx, lvName, "unmounted")
        storage.ClearMountPoint(ctx, lvName)
        storage.ClearError(ctx, lvName)
        return nil
    })
    
    return nil
}

// 带重试的卸载
func (o *Snapshotter) unmountWithRetry(ctx context.Context, mountPoint string, maxRetries int) error {
    var lastErr error
    for i := 0; i < maxRetries; i++ {
        lastErr = syscall.Unmount(mountPoint, 0)
        if lastErr == nil {
            return nil
        }
        log.Warnf("Unmount failed (attempt %d/%d): %v", i+1, maxRetries, lastErr)
        time.Sleep(time.Second * time.Duration(i+1))
    }
    return lastErr
}
```

---

### 4.5 各状态的错误处理策略

#### 创建阶段

| 状态转换 | 失败场景 | 回退策略 | Metadata 处理 |
|---------|---------|---------|--------------|
| none → creating | 标记失败 | 直接返回错误 | 不改变 |
| creating → created | LV 创建失败 | 删除 metadata 记录 | 删除 |
| creating → created | LV 创建成功但状态更新失败 | 重试更新；失败则标记 faulty | 保留 creating |

#### 格式化阶段

| 状态转换 | 失败场景 | 回退策略 | Metadata 处理 |
|---------|---------|---------|--------------|
| created → formatting | 标记失败 | 保持 created | 不改变 |
| formatting → formatted | 格式化失败 | 回退到 created | 设为 created + 记录错误 |
| formatting → formatted | 格式化成功但状态更新失败 | 重试更新；失败则保持 formatting | 保留 formatting（启动时检查） |

#### 挂载阶段

| 状态转换 | 失败场景 | 回退策略 | Metadata 处理 |
|---------|---------|---------|--------------|
| formatted → mounting | 标记失败 | 保持 formatted | 不改变 |
| mounting → mounted | 挂载失败（重试后） | 回退到 formatted | 设为 formatted + 记录错误 |
| mounting → mounted | 挂载成功但状态更新失败 | 重试更新；失败则保持 mounting | 保留 mounting（启动时检查） |

#### 卸载阶段

| 状态转换 | 失败场景 | 回退策略 | Metadata 处理 |
|---------|---------|---------|--------------|
| mounted → unmounting | 标记失败 | 保持 mounted | 不改变 |
| unmounting → unmounted | 卸载失败（重试后） | 回退到 mounted | 设为 mounted + 记录错误 |
| unmounting → unmounted | 卸载成功但状态更新失败 | 重试更新；失败则保持 unmounting | 保留 unmounting（启动时检查） |

#### 删除阶段

| 状态转换 | 失败场景 | 回退策略 | Metadata 处理 |
|---------|---------|---------|--------------|
| unmounted → removing | 标记失败 | 保持 unmounted | 不改变 |
| removing → removed | 删除失败 | 回退到 unmounted | 设为 unmounted + 记录错误 |
| removing → removed | 删除成功但状态更新失败 | 重试更新；成功则清理 metadata | 保留 removing（启动时清理） |

---

### 4.6 何时使用 faulty 状态

**faulty 状态的使用场景**（仅当无法自动回退时）：

1. **无法确定当前实际状态**
   ```go
   // 例如：操作超时，不确定是否完成
   if errors.Is(err, context.DeadlineExceeded) {
       // 不知道 unmount 是否成功，标记 faulty
       storage.SetLVState(ctx, lvName, "faulty")
   }
   ```

2. **回退操作本身失败**
   ```go
   // 例如：unmount 失败后，想回退到 mounted，但状态更新也失败
   if rollbackErr != nil {
       // 无法回退，标记 faulty
       storage.SetLVState(ctx, lvName, "faulty")
   }
   ```

3. **数据不一致且无法修复**
   ```go
   // 例如：LV 应该存在但实际不存在
   if !deviceExists && state == "mounted" {
       // 状态与实际严重不符，标记 faulty
       storage.SetLVState(ctx, lvName, "faulty")
   }
   ```

**faulty 状态的处理**：
- Cleanup 时尝试恢复（检查实际状态并修正）
- 重试失败则需要人工介入
- 提供 CLI 工具查看和修复

---

### 4.7 启动时检查的作用

**启动时检查不是主要恢复机制，而是兜底机制**：

1. **主要作用**：
   - 处理进程崩溃导致的中间状态
   - 修复状态更新失败的情况
   - 检测并标记 faulty 状态

2. **检查内容**：
   ```go
   func (o *Snapshotter) ensureLVStates(ctx context.Context) error {
       // 只处理中间状态（表示操作被中断）
       toFix := findIntermediateStates()  // creating, mounting, unmounting, etc.
       
       for _, lv := range toFix {
           actualState := checkActualState(lv)
           
           // 根据实际状态修正 metadata
           if actualState == targetState {
               // 操作实际完成了，只是状态没更新
               storage.SetLVState(ctx, lv.Name, targetState)
           } else {
               // 操作未完成，回退到前一状态
               storage.SetLVState(ctx, lv.Name, lv.PrevState)
           }
       }
   }
   ```

3. **不检查什么**：
   - 稳定状态（created, mounted, unmounted 等）不需要检查
   - faulty 状态交给 Cleanup 处理

---

## 五、完整的状态转换示例

### 5.1 成功流程

```
创建并挂载：
none → creating → created → formatting → formatted → mounting → mounted

卸载：
mounted → unmounting → unmounted

删除：
unmounted → removing → removed → (清理 metadata)
```

### 5.2 失败并回退流程

#### 场景 1：创建失败

```
none → creating → (创建失败) → none (metadata 已删除)
```

#### 场景 2：格式化失败

```
created → formatting → (格式化失败) → created (回退 + 记录错误)
```

#### 场景 3：挂载失败

```
formatted → mounting → (挂载失败，重试 3 次) → formatted (回退 + 记录错误)
```

#### 场景 4：卸载失败

```
mounted → unmounting → (卸载失败，重试 3 次) → mounted (回退 + 记录错误)
```

#### 场景 5：进程崩溃

```
创建时崩溃：
creating → (崩溃) → 启动时检查 →
  - 如果 LV 存在 → created
  - 如果 LV 不存在 → none (删除 metadata)

挂载时崩溃：
mounting → (崩溃) → 启动时检查 →
  - 如果已挂载 → mounted
  - 如果未挂载 → formatted (回退)

卸载时崩溃：
unmounting → (崩溃) → 启动时检查 →
  - 如果已卸载 → unmounted
  - 如果仍挂载 → mounted (回退)
```

---

## 六、总结

### 6.1 新方案的核心优势

1. **主动回退，不依赖重启**
   - ✅ 失败后立即回退到稳定状态
   - ✅ 不需要等待 Cleanup 或重启
   - ✅ 系统始终保持一致性

2. **状态机制解决核心问题**
   - ✅ 事务持有锁时间极短（几毫秒）
   - ✅ 慢速操作在事务外执行
   - ✅ 可以追踪操作进度

3. **完善的错误处理**
   - ✅ 立即重试（临时错误）
   - ✅ 立即回退（重试失败）
   - ✅ 启动时检查（兜底机制）
   - ✅ Cleanup 处理 faulty（持久错误）

### 6.2 方案对比

| 特性 | 旧方案（依赖重启） | 新方案（主动回退） | 优势 |
|------|------------------|------------------|------|
| 失败处理 | 标记 faulty | 立即回退 | 新方案 |
| 恢复时间 | 等待重启/Cleanup | 立即（秒级） | 新方案 |
| 系统一致性 | 可能长时间不一致 | 始终一致 | 新方案 |
| 依赖性 | 依赖重启 | 不依赖 | 新方案 |
| 实现复杂度 | 简单 | 中等 | 旧方案 |

### 6.3 实施建议

**Phase 1：核心功能（必须）**
1. 实现状态转换机制
2. 实现主动回退逻辑
3. 添加重试机制（3 次）

**Phase 2：完善功能（重要）**
4. 实现启动时检查（兜底）
5. 实现 Cleanup 处理 faulty
6. 添加错误记录和统计

**Phase 3：增强功能（可选）**
7. 实现人工介入工具
8. 添加监控告警
9. 优化重试策略

