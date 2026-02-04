# Thin Snapshot 数据保留机制详解

**创建时间**: 2026-01-22  
**目的**: 解释删除原始 LV 后，thin snapshot 的数据保留机制

---

## 问题场景

```
1. lvA (thin LV) 包含数据
2. 创建 lvA-snapshot (thin snapshot)
3. lvA 继续修改（新数据写入）
4. 删除 lvA
5. lvA-snapshot 的数据会怎样？
6. Thin pool 的数据会怎样？
```

---

## 一、快速答案

### lvA-snapshot 的数据

✅ **完全保留**，不会有任何变化！

### Thin pool 的数据

🔄 **部分释放**：
- lvA 独占的块（快照后新写入的）→ 被释放
- 快照共享的块（快照时的数据）→ 保留

---

## 二、详细解释

### 2.1 Thin Snapshot 的本质

Thin snapshot **不是数据拷贝，而是块映射的引用计数**：

```
Thin Pool 的块引用计数机制：

每个数据块都有一个引用计数器：
- refcount = 0: 块空闲，可以被分配
- refcount = 1: 只有一个 LV 使用
- refcount = 2+: 多个 LV 共享
```

### 2.2 创建快照时发生了什么

```
T0: lvA 初始状态
┌─────────────────────────────────────┐
│ Thin Pool (100GB)                   │
│ ┌─────────────────────────────────┐ │
│ │ 块 A: "data1"  refcount=1       │ │  ← lvA 使用
│ │ 块 B: "data2"  refcount=1       │ │  ← lvA 使用
│ │ 块 C: "data3"  refcount=1       │ │  ← lvA 使用
│ └─────────────────────────────────┘ │
└─────────────────────────────────────┘
        ↑
        └─ lvA 的映射表
           Block 0 → 块 A
           Block 1 → 块 B
           Block 2 → 块 C

T1: 创建快照 (lvcreate -s -n lvA-snapshot lvA)
┌─────────────────────────────────────┐
│ Thin Pool (100GB)                   │
│ ┌─────────────────────────────────┐ │
│ │ 块 A: "data1"  refcount=2 ← 共享│ │
│ │ 块 B: "data2"  refcount=2 ← 共享│ │
│ │ 块 C: "data3"  refcount=2 ← 共享│ │
│ └─────────────────────────────────┘ │
└─────────────────────────────────────┘
    ↑                   ↑
    │                   │
lvA 的映射表      lvA-snapshot 的映射表
Block 0 → 块 A    Block 0 → 块 A
Block 1 → 块 B    Block 1 → 块 B
Block 2 → 块 C    Block 2 → 块 C

关键：
1. 没有复制数据！只是增加了引用计数
2. lvA 和 lvA-snapshot 指向相同的块
3. 快照创建几乎是瞬间完成（只操作元数据）
```

### 2.3 修改 lvA 后发生了什么

```
T2: lvA 修改数据（快照后）
写入 lvA 的 Block 1:

┌─────────────────────────────────────┐
│ Thin Pool (100GB)                   │
│ ┌─────────────────────────────────┐ │
│ │ 块 A: "data1"     refcount=2    │ │  ← 未修改，仍共享
│ │ 块 B: "data2"     refcount=1    │ │  ← lvA 不再使用，只有快照用
│ │ 块 C: "data3"     refcount=2    │ │  ← 未修改，仍共享
│ │ 块 D: "new-data"  refcount=1    │ │  ← lvA 新写入的块
│ └─────────────────────────────────┘ │
└─────────────────────────────────────┘
    ↑                   ↑
    │                   │
lvA 的映射表      lvA-snapshot 的映射表
Block 0 → 块 A    Block 0 → 块 A
Block 1 → 块 D    Block 1 → 块 B  ← 快照保留旧块
Block 2 → 块 C    Block 2 → 块 C

写入过程（COW）：
1. lvA 要修改 Block 1
2. Thin pool 检测到 Block 1 的块 B refcount=2（共享）
3. 分配新块 D，写入新数据 "new-data"
4. lvA 的 Block 1 指向块 D
5. 块 B 的 refcount 减 1 → refcount=1
6. lvA-snapshot 仍然指向块 B（保留旧数据）
```

### 2.4 删除 lvA 后发生了什么

```
T3: 删除 lvA (lvremove lvA)

┌─────────────────────────────────────┐
│ Thin Pool (100GB)                   │
│ ┌─────────────────────────────────┐ │
│ │ 块 A: "data1"     refcount=1    │ │  ← 只有快照使用
│ │ 块 B: "data2"     refcount=1    │ │  ← 只有快照使用
│ │ 块 C: "data3"     refcount=1    │ │  ← 只有快照使用
│ │ 块 D: "new-data"  refcount=0    │ │  ← 被释放（无人使用）
│ └─────────────────────────────────┘ │
└─────────────────────────────────────┘
                        ↑
                        │
                  lvA-snapshot 的映射表
                  Block 0 → 块 A
                  Block 1 → 块 B
                  Block 2 → 块 C

删除过程：
1. 遍历 lvA 的映射表
2. 对每个块减少引用计数：
   - 块 A: refcount 2→1 (仍被快照使用，保留)
   - 块 D: refcount 1→0 (无人使用，释放)
   - 块 C: refcount 2→1 (仍被快照使用，保留)
3. 删除 lvA 的映射表
4. lvA-snapshot 继续正常使用，数据完整

结果：
✅ lvA-snapshot 数据完全保留（块 A, B, C）
✅ Thin pool 释放 lvA 独占的块（块 D）
✅ Thin pool 保留共享的块（块 A, B, C）
```

---

## 三、实际验证

### 3.1 测试步骤

```bash
# 1. 创建 thin pool 和 thin LV
lvcreate -L 10G -T vg/thin-pool
lvcreate -V 1G -T vg/thin-pool -n lvA
mkfs.ext4 /dev/vg/lvA
mkdir /mnt/lvA
mount /dev/vg/lvA /mnt/lvA

# 2. 写入初始数据
echo "original data in lvA" > /mnt/lvA/file1.txt
echo "more data" > /mnt/lvA/file2.txt
umount /mnt/lvA

# 3. 创建快照
lvcreate -s -n lvA-snapshot /dev/vg/lvA

# 4. 查看当前使用情况
lvs -o lv_name,lv_size,data_percent vg
#   LV           LSize  Data%
#   lvA          1.00g  2.00
#   lvA-snapshot 1.00g  0.00   ← 共享块，不占用额外空间
#   thin-pool   10.00g  0.20   ← 实际只用了 20MB

# 5. 修改 lvA（快照后）
mount /dev/vg/lvA /mnt/lvA
echo "new data after snapshot" > /mnt/lvA/file3.txt
dd if=/dev/zero of=/mnt/lvA/bigfile bs=1M count=100
umount /mnt/lvA

# 6. 再次查看使用情况
lvs -o lv_name,lv_size,data_percent vg
#   LV           LSize  Data%
#   lvA          1.00g  12.00  ← lvA 占用增加
#   lvA-snapshot 1.00g   2.00  ← 快照仍然只占用初始数据
#   thin-pool   10.00g   1.40  ← thin pool 总使用增加

# 7. 验证快照数据完整性
mount /dev/vg/lvA-snapshot /mnt/snap -o ro
ls /mnt/snap
# file1.txt  file2.txt  lost+found
# ← 只有快照时的文件，没有 file3.txt 和 bigfile

cat /mnt/snap/file1.txt
# original data in lvA  ← 数据完整
umount /mnt/snap

# 8. 删除 lvA
lvremove -y /dev/vg/lvA

# 9. 检查快照是否仍然可用
mount /dev/vg/lvA-snapshot /mnt/snap -o ro
cat /mnt/snap/file1.txt
# original data in lvA  ← 数据仍然完整！

ls /mnt/snap
# file1.txt  file2.txt  lost+found  ← 数据完全保留

umount /mnt/snap

# 10. 检查 thin pool 空间释放
lvs -o lv_name,lv_size,data_percent vg
#   LV           LSize  Data%
#   lvA-snapshot 1.00g   2.00  ← 快照数据完整
#   thin-pool   10.00g   0.20  ← 释放了 lvA 独占的块（bigfile）
```

### 3.2 预期结果

```
删除前：
- thin-pool 使用: 1.40% (约 140MB)
  └─ lvA 原始数据: 20MB (块 A, B, C)
  └─ lvA 快照后新数据: 100MB (块 D - bigfile)
  └─ 快照数据: 0MB (共享块 A, B, C)

删除后：
- thin-pool 使用: 0.20% (约 20MB)
  └─ 释放: 100MB (块 D，lvA 独占的)
  └─ 保留: 20MB (块 A, B, C，快照仍在使用)
  
快照数据：
✅ 完全保留，可以正常挂载和读取
```

---

## 四、块引用计数详解

### 4.1 引用计数的工作原理

```
Thin Pool 的元数据结构：

┌─────────────────────────────────────────────────────────┐
│ Thin Pool Metadata                                      │
│                                                          │
│ LV Mapping Tables:                                      │
│ ┌─────────────────────────────────────────────────────┐ │
│ │ lvA:                                                │ │
│ │   Block 0 → Physical Block 1000                     │ │
│ │   Block 1 → Physical Block 1005                     │ │
│ │   Block 2 → Physical Block 1002                     │ │
│ └─────────────────────────────────────────────────────┘ │
│                                                          │
│ ┌─────────────────────────────────────────────────────┐ │
│ │ lvA-snapshot:                                       │ │
│ │   Block 0 → Physical Block 1000                     │ │
│ │   Block 1 → Physical Block 1001                     │ │
│ │   Block 2 → Physical Block 1002                     │ │
│ └─────────────────────────────────────────────────────┘ │
│                                                          │
│ Block Reference Counts:                                 │
│ ┌─────────────────────────────────────────────────────┐ │
│ │ Physical Block 1000: refcount=2 (lvA + snapshot)    │ │
│ │ Physical Block 1001: refcount=1 (snapshot only)     │ │
│ │ Physical Block 1002: refcount=2 (lvA + snapshot)    │ │
│ │ Physical Block 1005: refcount=1 (lvA only)          │ │
│ └─────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────┘
```

### 4.2 删除 lvA 时的元数据操作

```
步骤 1: 遍历 lvA 的映射表
for each block in lvA.mapping_table:
    physical_block = block.physical_address
    refcount[physical_block]--
    
    if refcount[physical_block] == 0:
        mark_block_as_free(physical_block)

步骤 2: 删除 lvA 的映射表
delete lvA.mapping_table

步骤 3: 更新元数据
commit_metadata_transaction()

结果：
- Physical Block 1000: refcount 2→1 (保留)
- Physical Block 1001: refcount 1→1 (保留，快照使用)
- Physical Block 1002: refcount 2→1 (保留)
- Physical Block 1005: refcount 1→0 (释放)
```

---

## 五、关键特性总结

### 5.1 Thin Snapshot 的独立性

```
Thin snapshot 创建后是完全独立的 LV：

1. 有自己的映射表
2. 引用的块被保护（refcount 机制）
3. 删除原始 LV 不影响快照
4. 删除快照不影响原始 LV
5. 可以将快照提升为普通 LV
```

### 5.2 数据保留的保证

```
只要 refcount > 0，块就不会被释放：

场景 A: 原始 LV 和快照都存在
- 共享块: refcount=2

场景 B: 删除原始 LV
- 共享块: refcount=1 (快照保留)

场景 C: 再创建一个快照的快照
- 共享块: refcount=3

场景 D: 删除所有快照和原始 LV
- 共享块: refcount=0 (释放)
```

### 5.3 空间释放机制

```
删除 LV 时，只释放它独占的块：

lvA 删除后：
✅ 释放: lvA 快照后新写入的块（refcount 1→0）
✅ 保留: 快照时共享的块（refcount 2→1）
✅ 保留: 快照后快照修改的块（refcount 1→1）

结果：
- 快照数据完整
- Thin pool 空间部分回收
- 无数据丢失风险
```

---

## 六、与 Thick Snapshot 的对比

### 6.1 Thick Snapshot 行为

```
Thick snapshot 的数据是独立存储的：

创建快照时：
- 预留独立的空间（例如 2GB）
- 写入时才复制数据到快照空间

删除原始 LV 后：
- 快照变为 Invalid（失效）
- 数据丢失！

结论：Thick snapshot 依赖原始 LV
```

### 6.2 Thin Snapshot 优势

```
Thin snapshot 的数据是引用计数的：

创建快照时：
- 不预留空间
- 只增加块引用计数

删除原始 LV 后：
- 快照仍然有效
- 数据完全保留

结论：Thin snapshot 完全独立
```

---

## 七、实际应用场景

### 7.1 容器 Commit 场景

```
你的 LVM snapshot commit 方案：

Step 1: 创建可写层快照
writable-lv → writable-lv-snapshot

Step 2: Commit 过程中，用户可能继续使用容器
writable-lv 继续被修改

Step 3: Diff 操作
thin_send base-lv writable-lv-snapshot
↑ 快照数据稳定，不受 writable-lv 修改影响

Step 4: Commit 完成后，可以删除快照
lvremove writable-lv-snapshot
↑ thin pool 回收快照独占的块
```

### 7.2 多版本管理

```
场景：保留多个版本的快照

v1: lvA
    ├─ lvA-snapshot-v1 (commit 1)
    └─ 继续修改
v2: lvA
    ├─ lvA-snapshot-v2 (commit 2)
    └─ 继续修改
v3: lvA
    └─ lvA-snapshot-v3 (commit 3)

删除 lvA 后：
- 所有快照仍然可用
- 每个快照保留对应版本的完整数据
- Thin pool 只释放 lvA 独占的块
```

---

## 八、常见问题

### Q1: 删除原始 LV 后，快照能否继续修改？

**A**: 可以！快照是完全独立的 thin LV

```bash
# 挂载快照（读写）
mount /dev/vg/lvA-snapshot /mnt/snap

# 修改数据
echo "modify snapshot" > /mnt/snap/new.txt

# 快照会分配新块，完全正常
```

### Q2: 如果创建了多个快照，删除顺序重要吗？

**A**: 不重要！每个快照都是独立的

```bash
lvcreate -s -n snap1 /dev/vg/lvA
lvcreate -s -n snap2 /dev/vg/lvA
lvcreate -s -n snap3 /dev/vg/lvA

# 任意顺序删除都没问题
lvremove /dev/vg/lvA      # 先删原始 LV
lvremove /dev/vg/snap2    # 再删中间的快照
lvremove /dev/vg/snap1    # 然后删第一个
lvremove /dev/vg/snap3    # 最后删最后一个

# 每次删除只减少引用计数，不影响其他 LV
```

### Q3: 如何查看块引用情况？

**A**: 使用 `dmsetup` 工具

```bash
# 查看 thin pool 状态
dmsetup status vg-thin--pool-tpool

# 查看详细的块映射
thin_dump /dev/vg/thin-pool_tmeta

# 输出示例（简化）：
# <device dev_id="1">
#   <range_mapping origin_begin="0" data_begin="1000" length="8"/>
# </device>
# <device dev_id="2">
#   <range_mapping origin_begin="0" data_begin="1000" length="8"/>
# </device>
# ↑ 两个设备共享 data_begin="1000" 的块
```

### Q4: Thin pool 空间满了会怎样？

**A**: 所有 thin LV 都无法写入

```bash
# 监控 thin pool
lvs -o lv_name,data_percent vg/thin-pool
#   LV        Data%
#   thin-pool 95.00  # 接近满

# 扩展 thin pool
lvextend -L +10G /dev/vg/thin-pool

# 或者删除不需要的快照释放空间
lvremove /dev/vg/old-snapshot
```

---

## 九、总结

### 核心机制

```
Thin snapshot 的数据保留依赖：

1. 块引用计数机制
   - 每个块维护 refcount
   - refcount > 0 的块不会被释放

2. 独立的映射表
   - 每个 thin LV 有自己的映射表
   - 删除 LV 只删除其映射表

3. COW 机制
   - 修改共享块时分配新块
   - 旧块引用计数减 1
   - 快照继续使用旧块
```

### 你的问题答案

```
lvA → lvA-snapshot
  ↓ lvA 修改
  ↓ 删除 lvA
  
lvA-snapshot 的数据：
✅ 完全保留，不受任何影响

Thin pool 的数据：
✅ 快照时的共享块：保留（refcount 2→1）
✅ lvA 快照后新写入的块：释放（refcount 1→0）
✅ 空间部分回收，快照数据完整
```

### 对你项目的意义

```
在 commit 过程中：

1. 创建快照后，可以安全删除原始 writable-lv
2. 快照数据稳定，不受后续修改影响
3. Diff 操作可以放心基于快照进行
4. Commit 完成后删除快照，自动回收空间
5. 无需担心数据丢失或损坏
```

---

**文档版本**: 1.0.0  
**最后更新**: 2026-01-22  
**相关文档**: [Thin vs Thick Snapshot](./thin-vs-thick-snapshot.md)

