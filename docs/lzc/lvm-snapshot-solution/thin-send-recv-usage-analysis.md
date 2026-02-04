# Thin-send-recv 使用场景分析与架构优化

## 一、讨论记录（持续更新）

**当前结论（待最终确认）**：
1. ✅ 使用基准 LV，但**当前阶段不将只读层最上层拷贝到可写层 LV**。
2. ✅ 后续计费场景需要时，再补齐"只读层最上层 → 可写层 LV"的拷贝流程。
3. ✅ 先实现"两个 LV（基准 LV vs 可写层快照）"的 diff 与镜像层打包。
4. ✅ 暂不修改 Snapshotter 代码，先在 commit 流程外部实现验证。
5. ✅ 使用 thin-send-recv 工具进行块级 diff（已实现 ThinSend 模块）。
6. ✅ 建立模块文档制度：每个模块单独文档 + 持续维护。

## 二、实现进度跟踪

### 已完成模块

#### 1. ThinSend 模块（块级 diff）

**文件**: `snapshots/devbox/lvm/thin_send_recv.go`  
**功能**: 
- `ThinSend(ctx, baseLV, targetLV, out)`: 流式输出块级差异
- `ThinSendToFile(ctx, baseLV, targetLV, path)`: 输出到文件

**相关文档**:
- [ThinSend 实现文档](./thin-send-implementation.md)
- [ThinSend 测试指南](./thin-send-testing.md)
- [LV、文件系统、挂载的关系](./lv-filesystem-concepts.md)
- [Thin_send 块级 Diff 原理](./thin-send-block-level-diff.md)
- [Devbox 架构详解](./devbox-architecture-explained.md)
- [基准 LV 创建方案](./base-lv-creation-plan.md)
- [Thin Snapshot vs Thick Snapshot](./thin-vs-thick-snapshot.md)

**CLI 工具**: `cmd/devbox-thin-send-recv`

### 进行中模块

- [ ] **thin_send 输出解析**（下一步）
  - 解析二进制流
  - 提取变化的块地址
  
- [ ] **块到文件映射**
  - 使用 debugfs/xfs_db 映射块到文件
  - 生成文件变更列表
  
- [ ] **镜像层打包**
  - 生成 layer.tar
  - 处理 whiteout 文件
  - 计算 digest/diffID

### 待开始模块

- [ ] 集成到 commit 流程
- [ ] 性能优化与监控
- [ ] 生产环境测试
5. ✅ 已实现 thin_send 的 diff 流式导出工具与 API（不改 Snapshotter）。

**实现进度（当前）**：
- `snapshots/devbox/lvm/thin_send_recv.go`：提供 `ThinSend` / `ThinSendToFile` API
- `cmd/devbox-thin-send-recv`：提供 CLI 生成 thin_send 差异流文件

---

## 二、问题 2：是否需要拷贝只读层到可写层？

### 当前阶段的架构（根据最新决策）

```
Prepare 时：
只读层最上层 → 拷贝到 → 基准 LV (devbox-xxx-base) [只读，不挂载]
可写层 LV (devbox-xxx) 空 LV，仅用于用户写入
                ↓
           用户写入数据
                ↓
Commit 时：
可写层 LV → 创建快照 → 快照 LV
thin_send 基准 LV 快照 LV → 获取块级差异
```

### 如果使用 thin-send-recv，是否需要拷贝？

**答案：取决于 diff 方案**

#### 方案 A：仍然需要拷贝（推荐）

**理由**：
1. **thin-send-recv 需要两个 thin volume 进行对比**
   - thin-send-recv 的语法：`thin_send <base_volume> <target_volume>`
   - 需要两个都是 thin volume 或 thin snapshot
   - 只读层最上层是文件系统目录，不是 thin volume

2. **基准 LV 的作用**：
   - 基准 LV = 只读层最上层的 thin volume 副本
   - 用于与快照 LV 进行 thin-send-recv diff
   - 提供稳定的对比基准

**优化后的架构**：
```
Prepare 时：
只读层最上层 → 拷贝到 → 可写层 LV (devbox-xxx)
只读层最上层 → 拷贝到 → 基准 LV (devbox-xxx-base) [只读，不挂载]

Commit 时：
可写层 LV → 创建快照 → 快照 LV
thin_send 基准 LV 快照 LV → 获取块级差异
```

#### 方案 B：不需要拷贝（如果只读层也是 thin volume）

**前提条件**：
- 只读层最上层本身就是一个 thin volume
- 可以直接用 thin-send-recv 对比

**架构**：
```
Commit 时：
可写层 LV → 创建快照 → 快照 LV
thin_send 只读层 thin volume 快照 LV → 获取块级差异
```

**但你的项目情况**：
- 只读层是 OverlayFS 分层结构，不是 thin volume
- 所以**仍然需要拷贝到基准 LV**

### 结论（当前阶段）

**不需要拷贝到可写层 LV**，但需要：
- 基准 LV：用于 commit diff（只读，不挂载，可复用）
- 可写层 LV：仅承载用户写入数据

**后续阶段（计费需求）**：
- 再加入“只读层最上层 → 可写层 LV”的拷贝流程

---

## 二、问题 3：thin-send-recv 对快照类型的要求

### thin-send-recv 的工作原理

**thin-send-recv 支持**：
- ✅ **thin volume** vs **thin snapshot**
- ✅ **thin snapshot** vs **thin snapshot**
- ✅ **thin volume** vs **thin volume**

**关键点**：
- thin-send-recv 工作在**块设备级别**
- 不关心源是 volume 还是 snapshot
- 只要求两个都是 thin volume/snapshot

### 快照在 diff 过程中的稳定性

#### LVM 快照的数据稳定性（为什么不会变化）

**结论**：快照一旦创建，就固定在创建时刻的数据视图。后续对原始 LV 的写入不会改变快照看到的数据。

**原因分两种实现机制（取决于 thin / regular）：**

1. **thin snapshot（thin pool）机制**  
   - 快照和原始 LV 共享同一 thin pool 的数据块。  
   - **快照记录的是“块映射表”**（mapping）：  
     - 创建快照时，把当时的映射固定下来。  
     - 原始 LV 写入新数据时，会**分配新的块并更新原始 LV 的映射**。  
     - **快照的映射不变**，因此快照读到的是旧数据。  
   - **关键点**：在 thin snapshot 中，通常是“映射变化”，而不是“拷贝旧块”。

2. **regular snapshot（非 thin）机制**  
   - 通过传统 COW：  
     - 原始 LV 写入前，会把旧块拷贝到快照区域。  
     - 原始 LV 再写入新数据。  
   - **快照保存旧块**，因此快照看到的内容不会变化。

**示意（以 thin snapshot 为主）**：
```
创建快照时：
原始 LV 映射 -> [A, B, C, D, E]
快照  LV 映射 -> [A, B, C, D, E]  (固定)

原始 LV 写入：
原始 LV 映射 -> [A, B', C, D', E]  (映射更新到新块)
快照  LV 映射 -> [A, B,  C, D,  E]  (映射不变)
```

**关键保证**：
1. **快照数据不会变化**：映射被冻结或旧块被保存。
2. **写入隔离**：原始 LV 的写入只影响原始 LV 的映射或数据。
3. **只读保护**：快照本身为只读（符合一致性语义）。

### 如果可写层 LV 本身是快照？

**场景分析**：
```
情况 A：可写层是原始 thin volume
可写层 LV (devbox-xxx) → 创建快照 → 快照 LV
用户继续修改可写层 LV → 不影响快照 LV ✅

情况 B：可写层本身是快照（不太可能，但理论上）
可写层快照 LV → 创建快照 → 快照的快照 LV
用户修改原始 LV → 不影响可写层快照 → 不影响快照的快照 ✅
```

**结论**：
- ✅ **快照在 diff 过程中是稳定的**
- ✅ **用户修改原始 LV 不会影响快照数据**
- ✅ **可以使用 thin-send-recv 进行 diff**

### thin-send-recv 的使用示例

```bash
# 场景 1：thin volume vs thin snapshot
thin_send /dev/vg/base-lv /dev/vg/snapshot-lv

# 场景 2：thin snapshot vs thin snapshot
thin_send /dev/vg/base-snapshot /dev/vg/target-snapshot

# 场景 3：thin volume vs thin volume
thin_send /dev/vg/base-lv /dev/vg/target-lv
```

**你的项目场景**：
```bash
# 基准 LV (包含只读层最上层)
/dev/vg/devbox-xxx-base

# 快照 LV (包含只读层最上层 + 用户写入)
/dev/vg/devbox-xxx-snapshot

# 使用 thin-send-recv
thin_send /dev/vg/devbox-xxx-base /dev/vg/devbox-xxx-snapshot
```

---

## 三、优化后的架构设计

### 方案：使用基准 LV + thin-send-recv

```
Prepare 时：
┌─────────────────────────────────────────┐
│  只读层最上层 (OverlayFS)                │
└─────────────────────────────────────────┘
           │
           ├─→ 拷贝到可写层 LV (devbox-xxx)
           │   - 用于容器运行
           │   - 可读写，挂载
           │
           └─→ 拷贝到基准 LV (devbox-xxx-base)
               - 用于 commit diff
               - 只读，不挂载
               - 可被多个 commit 复用

Commit 时：
┌─────────────────────────────────────────┐
│  可写层 LV (devbox-xxx)                  │
│  - 包含：只读层最上层 + 用户写入          │
└─────────────────────────────────────────┘
           │
           │ 创建快照（thin snapshot）
           ↓
┌─────────────────────────────────────────┐
│  快照 LV (devbox-xxx-snapshot)          │
│  - 保存：只读层最上层 + 用户写入（T2时刻）│
│  - 用户继续修改可写层 LV 不影响快照      │
└─────────────────────────────────────────┘
           │
           │ thin_send
           ↓
┌─────────────────────────────────────────┐
│  基准 LV (devbox-xxx-base)              │
│  - 包含：只读层最上层（原始状态）          │
└─────────────────────────────────────────┘
           │
           ↓
      块级差异数据
           │
           ↓
      映射到文件
           │
           ↓
      打包为镜像层
```

### 实现代码示例

```go
func commitWithThinSendRecv(ctx context.Context, vgName, lvName, contentKey string) error {
    // 1. 获取或创建基准 LV
    baseLVName, err := getOrCreateBaseLV(ctx, vgName, contentKey)
    if err != nil {
        return err
    }
    
    // 2. 创建快照（thin snapshot，不指定 size）
    snapshotPath, err := lvm.CreateDevboxLVSnapshot(ctx, vgName, lvName)
    if err != nil {
        return fmt.Errorf("failed to create snapshot: %w", err)
    }
    defer lvm.DestroyDevboxLVSnapshot(ctx, vgName, lvName)
    
    baseLV := fmt.Sprintf("/dev/%s/%s", vgName, baseLVName)
    snapshotLV := fmt.Sprintf("/dev/%s", snapshotPath)
    
    // 3. 使用 thin_send 获取块级差异
    cmd := exec.Command("thin_send", baseLV, snapshotLV)
    output, err := cmd.Output()
    if err != nil {
        return fmt.Errorf("thin_send failed: %w", err)
    }
    
    // 4. 解析 thin_send 输出，获取变化的块
    changedBlocks := parseThinSendOutput(output)
    
    // 5. 将变化的块映射到文件
    return mapBlocksToFiles(changedBlocks, snapshotLV)
}
```

---

## 四、关键结论

### 问题 2 的答案

**是否需要拷贝只读层到可写层？**

**答案：需要，但可以优化**

- ✅ **可写层 LV**：需要拷贝，用于容器运行
- ✅ **基准 LV**：需要拷贝，用于 commit diff（可复用）
- ✅ **如果使用 thin-send-recv**：基准 LV 是必需的，因为只读层不是 thin volume

### 问题 3 的答案

**thin-send-recv 对快照类型的要求？**

**答案：没有特殊要求**

- ✅ thin-send-recv 支持 thin volume 和 thin snapshot
- ✅ 快照在 diff 过程中是**稳定的**（COW 机制保证）
- ✅ 用户修改原始 LV **不会影响**快照数据
- ✅ 可以安全使用 thin-send-recv 进行 diff

### 推荐方案

1. **修改快照创建**：不指定 size，创建真正的 thin snapshot
2. **创建基准 LV**：在 Prepare 时创建，保存只读层最上层
3. **使用 thin-send-recv**：对比基准 LV 和快照 LV
4. **块到文件映射**：将变化的块映射到文件，打包为镜像层

