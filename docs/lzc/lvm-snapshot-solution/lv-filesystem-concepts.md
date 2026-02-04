# LV、文件系统、挂载的关系详解

**创建时间**: 2026-01-22  
**目的**: 澄清 LV 卸载后数据是否还存在的疑问

---

## 一、核心概念

### 1.1 LV（逻辑卷）是块设备

**LV 本质**：
- LV 是一个**块设备**（Block Device），类似硬盘分区
- 数据**永久存储**在 LV 上（除非主动删除 LV）
- 卸载（umount）**不会删除数据**

**类比**：
```
LV = 硬盘分区
文件系统 = 格式化后的分区（ext4、xfs 等）
挂载 = 让操作系统能通过路径访问分区
```

### 1.2 挂载（mount）的作用

**挂载是什么**：
- 将块设备（LV）的文件系统**关联到目录树**上
- 让用户可以通过路径（如 `/mnt/base-lv`）访问数据

**挂载 ≠ 数据传输**：
- 挂载**不是复制数据**到挂载点
- 只是建立一个**访问入口**

**类比**：
```
LV 上的数据 = 书架上的书
挂载点 = 书架的标签
挂载 = 给书架贴上标签，让你能找到它
卸载 = 撕掉标签，但书还在书架上
```

### 1.3 卸载（umount）的作用

**卸载是什么**：
- 断开文件系统和挂载点的**关联**
- 释放挂载点目录
- **数据仍然在 LV 上**

**卸载后**：
- LV 还在（`/dev/test-vg/base-lv` 仍然存在）
- 数据还在（ext4 文件系统和所有文件都在）
- 只是**不能通过 `/mnt/base-lv` 访问**了

---

## 二、图解说明

### 2.1 挂载前

```
┌─────────────────────────────────────┐
│  块设备：/dev/test-vg/base-lv       │
│  ┌───────────────────────────────┐  │
│  │  ext4 文件系统                │  │
│  │  - file1.txt                  │  │
│  │  - file2.txt                  │  │
│  │  - dir1/file3.txt             │  │
│  └───────────────────────────────┘  │
│  数据在这里！                        │
└─────────────────────────────────────┘
         ↑
         │ 数据永久存储在 LV 上
         │
┌─────────────────────────────────────┐
│  挂载点：/mnt/base-lv （空目录）     │
│  - 无法访问 LV 上的数据              │
└─────────────────────────────────────┘
```

### 2.2 挂载后

```
┌─────────────────────────────────────┐
│  块设备：/dev/test-vg/base-lv       │
│  ┌───────────────────────────────┐  │
│  │  ext4 文件系统                │  │
│  │  - file1.txt                  │  │
│  │  - file2.txt                  │  │
│  │  - dir1/file3.txt             │  │
│  └───────────────────────────────┘  │
│  数据在这里！                        │
└─────────────────────────────────────┘
         ↑
         │ mount 建立关联
         │
┌─────────────────────────────────────┐
│  挂载点：/mnt/base-lv                │
│  - file1.txt ← 访问的是 LV 上的数据  │
│  - file2.txt ← 访问的是 LV 上的数据  │
│  - dir1/file3.txt                   │
└─────────────────────────────────────┘
```

### 2.3 卸载后

```
┌─────────────────────────────────────┐
│  块设备：/dev/test-vg/base-lv       │
│  ┌───────────────────────────────┐  │
│  │  ext4 文件系统                │  │
│  │  - file1.txt ✅                │  │
│  │  - file2.txt ✅                │  │
│  │  - dir1/file3.txt ✅           │  │
│  └───────────────────────────────┘  │
│  数据依然在这里！                    │
└─────────────────────────────────────┘
         ↑
         │ umount 断开关联
         │ 但数据还在 LV 上
         │
┌─────────────────────────────────────┐
│  挂载点：/mnt/base-lv （空目录）     │
│  - 无法访问，但 LV 上的数据没丢      │
└─────────────────────────────────────┘
```

---

## 三、验证数据仍然存在

### 3.1 方法 1：重新挂载后查看

```bash
# 卸载
sudo umount /mnt/base-lv

# 查看挂载点（应该是空的）
ls /mnt/base-lv
# 输出：（空，没有任何文件）

# 重新挂载
sudo mount /dev/test-vg/base-lv /mnt/base-lv

# 查看数据（数据还在！）
ls /mnt/base-lv
# 输出：file1.txt  file2.txt  dir1  lost+found

cat /mnt/base-lv/file1.txt
# 输出：Base file 1
```

### 3.2 方法 2：使用 file 命令检查 LV

```bash
# 卸载后，直接检查 LV（块设备）
sudo file -s /dev/test-vg/base-lv

# 输出示例：
# /dev/test-vg/base-lv: Linux rev 1.0 ext4 filesystem data, 
# UUID=xxx, volume name "base-lv" (extents) (64bit) (large files) (huge files)
```

**说明**：
- `file -s` 显示块设备的文件系统类型
- 证明 ext4 文件系统仍然存在于 LV 上

### 3.3 方法 3：使用 debugfs 查看文件（不挂载）

```bash
# 使用 debugfs 工具直接读取 LV 上的文件系统
sudo debugfs -R 'ls -l' /dev/test-vg/base-lv

# 输出示例：
#  2   40755 (2)      0      0    4096 22-Jan-2026 10:30 .
#  2   40755 (2)      0      0    4096 22-Jan-2026 10:30 ..
# 12  100644 (1)      0      0      12 22-Jan-2026 10:30 file1.txt
# 13  100644 (1)      0      0      12 22-Jan-2026 10:30 file2.txt
# ...
```

**说明**：
- `debugfs` 可以**不挂载**直接读取 ext4 文件系统
- 证明文件系统和文件都在 LV 上

### 3.4 方法 4：直接读取块设备内容

```bash
# 使用 strings 查看 LV 中的可见字符
sudo strings /dev/test-vg/base-lv | grep "Base file"

# 输出：
# Base file 1
# Base file 2
# Base file in dir
```

**说明**：
- 直接读取块设备的原始数据
- 找到了我们写入的字符串
- 证明数据确实在 LV 上

---

## 四、为什么 thin_send 可以在卸载后工作？

### 4.1 thin_send 工作在块级别

**关键点**：
- thin_send **不需要挂载**文件系统
- 它直接读取 LV 的**块设备**
- 它读取的是 **thin pool 的 COW 表**，而不是文件系统

### 4.2 工作流程

```bash
# 1. 卸载 LV（确保没有写入）
sudo umount /mnt/base-lv
sudo umount /mnt/writable-lv

# 2. 创建快照（块级别操作）
sudo lvcreate -s test-vg/writable-lv -n writable-lv-snapshot

# 3. 使用 thin_send（直接读取块设备）
sudo thin_send /dev/test-vg/base-lv /dev/test-vg/writable-lv-snapshot > /tmp/diff.stream
```

**为什么可以工作**：
```
thin_send 不关心文件系统
           ↓
直接读取 LV 的块数据
           ↓
读取 thin pool 的 COW 表
           ↓
找出变化的块
           ↓
输出块级差异
```

---

## 五、实际场景示例

### 5.1 测试场景演示

```bash
# 步骤 1：创建并写入数据
sudo lvcreate -V 1G -T test-vg/test-thinpool -n demo-lv
sudo mkfs.ext4 /dev/test-vg/demo-lv
sudo mkdir -p /mnt/demo
sudo mount /dev/test-vg/demo-lv /mnt/demo
echo "Hello World" | sudo tee /mnt/demo/test.txt

# 步骤 2：卸载
sudo umount /mnt/demo

# 步骤 3：验证挂载点是空的
ls /mnt/demo
# 输出：（空）

# 步骤 4：验证 LV 上有数据
sudo strings /dev/test-vg/demo-lv | grep "Hello"
# 输出：Hello World

# 步骤 5：重新挂载，数据还在
sudo mount /dev/test-vg/demo-lv /mnt/demo
cat /mnt/demo/test.txt
# 输出：Hello World
```

---

## 六、常见误区

### 误区 1：卸载 = 删除数据

**错误理解**：
```
umount /mnt/base-lv  →  数据被删除 ❌
```

**正确理解**：
```
umount /mnt/base-lv  →  断开访问路径，数据仍在 LV 上 ✅
```

### 误区 2：挂载点有数据，数据在挂载点目录

**错误理解**：
```
数据存储在 /mnt/base-lv 目录 ❌
```

**正确理解**：
```
数据存储在 /dev/test-vg/base-lv（LV）
/mnt/base-lv 只是访问入口 ✅
```

### 误区 3：删除挂载点 = 删除数据

**错误理解**：
```
rm -rf /mnt/base-lv  →  数据被删除 ❌
```

**正确理解**：
```
rm -rf /mnt/base-lv  →  只删除挂载点目录，数据仍在 LV 上 ✅
（前提是已经卸载）
```

---

## 七、如何真正删除数据？

### 7.1 删除 LV 上的数据（文件系统级别）

```bash
# 挂载后删除文件
sudo mount /dev/test-vg/base-lv /mnt/base-lv
sudo rm -rf /mnt/base-lv/*  # 删除文件系统中的所有文件
sudo umount /mnt/base-lv
# 数据被删除，但 LV 和文件系统还在
```

### 7.2 删除整个 LV（块设备级别）

```bash
# 删除 LV 本身
sudo lvremove -f test-vg/base-lv
# LV、文件系统、所有数据都被删除
```

### 7.3 格式化 LV（重建文件系统）

```bash
# 重新格式化
sudo mkfs.ext4 /dev/test-vg/base-lv
# 旧的文件系统被覆盖，数据无法访问（但可能可以恢复）
```

---

## 八、总结

### 关键要点

1. **LV 是块设备**：数据永久存储在 LV 上
2. **挂载只是访问入口**：不是数据传输或复制
3. **卸载不会删除数据**：只是断开访问路径
4. **thin_send 工作在块级别**：不需要挂载，直接读取 LV

### 测试流程中的逻辑

```bash
# 1. 创建 base-lv 并写入数据
sudo mount /dev/test-vg/base-lv /mnt/base-lv
echo "data" > /mnt/base-lv/file.txt
sudo umount /mnt/base-lv
# ✅ 数据在 LV 上

# 2. 创建 writable-lv 并修改数据
sudo mount /dev/test-vg/writable-lv /mnt/writable-lv
# ... 修改数据 ...
sudo umount /mnt/writable-lv
# ✅ 修改后的数据在 LV 上

# 3. 创建快照
sudo lvcreate -s test-vg/writable-lv -n writable-lv-snapshot
# ✅ 快照保存了 writable-lv 的当前状态

# 4. 使用 thin_send 做 diff
sudo thin_send /dev/test-vg/base-lv /dev/test-vg/writable-lv-snapshot
# ✅ 对比两个 LV 的块级差异
# base-lv 有数据，writable-lv-snapshot 也有数据
# diff 结果 = 用户写入的变化
```

---

**文档版本**: 1.0.0  
**最后更新**: 2026-01-22

