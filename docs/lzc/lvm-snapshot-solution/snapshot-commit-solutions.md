# 基于 LVM 快照的 Commit 方案调研

## 一、thin-send-recv 项目介绍

### 1.1 项目概述

**LINBIT/thin-send-recv** 是一个模仿 ZFS 的 `zfs send/zfs recv` 功能的工具，用于 LVM thin provisioning 世界。

- **开发者**: Philipp Reisner (LINBIT，DRBD 的开发商)
- **许可证**: GPLv3
- **状态**: Active
- **GitHub**: https://github.com/LINBIT/thin-send-recv

### 1.2 核心功能

发送 LVM thin snapshot 之间的增量数据，支持流式传输和压缩。

**使用示例**:

```bash
# 直接通过 SSH 发送到远程机器
thin_send ssd_vg/CentOS7.6 ssd_vg/li0 | \
    ssh root@target-machine "thin_recv kubuntu-vg/li0"

# 使用 socat + zstd 进行流式传输和压缩
# 目标机器
target-machine$ socat TCP-LISTEN:4321 STDOUT | zstd -d | thin_recv kubuntu-vg/li0

# 源机器
source-machine$ thin_send ssd_vg/CentOS7.6 ssd_vg/li0 | zstd | \
    socat STDIN TCP:10.43.8.39:4321
```

### 1.3 技术原理

利用 LVM thin provisioning 的元数据：
- 找出两个 thin snapshot 之间差异的块
- 发送增量数据而不是完整数据
- 接收端重建 thin LV

### 1.4 与 Devbox 场景的关系

| 特性 | 适用性 | 说明 |
|------|-------|------|
| 增量数据传输 | ✅ | 可以发送快照之间的增量 |
| 块级精度 | ✅ | 精确到块级别 |
| 输出格式 | ⚠️ | 输出是块级数据流，不是文件系统格式 |
| 接收端要求 | ⚠️ | 接收端必须是 thin LV，不适合直接打包镜像 |

**结论**: thin-send-recv 可以获取块级差异，但不适合直接用于镜像打包场景。

---

## 二、基于快照的 Commit 方案详解

### 方案 1: 文件系统 diff（推荐用于第一阶段）

#### 1.1 原理

```
原始 LV (devbox001)          快照 LV (devbox001snapshot)
        ↓                              ↓
    挂载到 /mnt/original          挂载到 /mnt/snapshot
        ↓                              ↓
    对比两个目录的文件差异
        ↓
    找出新增/修改/删除的文件
        ↓
    打包变化的文件 → 镜像
```

#### 1.2 实现方式

##### 1.2.1 使用 rsync 算法

```bash
# 1. 创建快照
lvcreate -s -n devbox001snapshot lvm-vg/devbox001

# 2. 挂载原始 LV 和快照
mkdir -p /mnt/original /mnt/snapshot
mount /dev/lvm-vg/devbox001 /mnt/original
mount /dev/lvm-vg/devbox001snapshot /mnt/snapshot

# 3. 使用 rsync --only-writebatch 找出差异
rsync -a --delete \
      --only-writebatch=diff.batch \
      --partial-dir=.rsync-partial \
      /mnt/snapshot/ /mnt/original/

# 4. 打包变化的文件
tar -czf changes.tar.gz -T diff.batch

# 5. 基于变化的文件创建镜像（使用 nerdctl/ctr commit）
```

**优点**:
- ✅ 成熟稳定，rsync 算法经过广泛验证
- ✅ 只打包变化的文件
- ✅ 支持增量检测
- ✅ 可以跨文件系统使用

**缺点**:
- ⚠️ 需要挂载两个 LV
- ⚠️ 需要遍历文件系统（大量小文件时慢）
- ⚠️ 仍然是文件级，不是块级

##### 1.2.2 使用 find + tar

```bash
# 1. 找出 mtime/ctime 变化的文件
find /mnt/original -newer /mnt/snapshot -type f > changed_files.txt

# 2. 找出新文件
comm -13 <(find /mnt/snapshot -type f | sort) \
         <(find /mnt/original -type f | sort) >> changed_files.txt

# 3. 找出删除的文件（记录在元数据中）
comm -23 <(find /mnt/snapshot -type f | sort) \
         <(find /mnt/original -type f | sort) > deleted_files.txt

# 4. 打包变化的文件
tar -czf changes.tar.gz -T changed_files.txt
```

##### 1.2.3 使用 Go 实现文件级 diff

```go
package lvm

import (
    "os"
    "path/filepath"
)

// FileChangeType 文件变化类型
type FileChangeType int

const (
    Added FileChangeType = iota
    Modified
    Deleted
)

// FileChange 文件变化记录
type FileChange struct {
    Type FileChangeType
    Path string
    Size int64
}

// ComputeFilesystemDiff 计算文件系统差异
func ComputeFilesystemDiff(originalDir, snapshotDir string) ([]FileChange, error) {
    changes := []FileChange{}

    // 遍历原始 LV
    filepath.Walk(originalDir, func(path string, info os.FileInfo, err error) error {
        if err != nil {
            return nil // 跳过错误
        }

        if info.IsDir() {
            return nil
        }

        relPath, _ := filepath.Rel(originalDir, path)
        snapshotPath := filepath.Join(snapshotDir, relPath)

        snapshotInfo, err := os.Stat(snapshotPath)
        if err != nil {
            if os.IsNotExist(err) {
                // 文件在快照中不存在 → 新文件
                changes = append(changes, FileChange{
                    Type: Added,
                    Path: relPath,
                    Size: info.Size(),
                })
            }
            return nil
        }

        // 比较修改时间和大小
        if info.ModTime().After(snapshotInfo.ModTime()) ||
           info.Size() != snapshotInfo.Size() {
            // 文件被修改
            changes = append(changes, FileChange{
                Type: Modified,
                Path: relPath,
                Size: info.Size(),
            })
        }

        return nil
    })

    // 找出删除的文件
    filepath.Walk(snapshotDir, func(path string, info os.FileInfo, err error) error {
        if err != nil || info.IsDir() {
            return nil
        }

        relPath, _ := filepath.Rel(snapshotDir, path)
        originalPath := filepath.Join(originalDir, relPath)

        if _, err := os.Stat(originalPath); os.IsNotExist(err) {
            changes = append(changes, FileChange{
                Type: Deleted,
                Path: relPath,
                Size: 0,
            })
        }
        return nil
    })

    return changes, nil
}
```

**使用示例**:

```go
// 1. 创建快照
snapshotPath, err := CreateDevboxLVSnapshot(ctx, vgName, lvName)

// 2. 挂载快照和原始 LV
originalMount := "/mnt/devbox-original"
snapshotMount := "/mnt/devbox-snapshot"

mountDevice(ctx, fmt.Sprintf("/dev/%s/%s", vgName, lvName), originalMount)
mountDevice(ctx, fmt.Sprintf("/dev/%s/%s", vgName, snapshotPath), snapshotMount)

// 3. 计算差异
changes, err := ComputeFilesystemDiff(originalMount, snapshotMount)

// 4. 打包变化的文件
for _, change := range changes {
    srcPath := filepath.Join(originalMount, change.Path)
    // 添加到归档
}
```

---

### 方案 2: 块级 diff（thin-send-recv）

#### 2.1 原理

```
原始 LV (devbox001)          快照 LV (devbox001snapshot)
        ↓                              ↓
  LVM Thin 元数据记录
  哪些块被修改了
        ↓
thin_send 工具读取元数据
        ↓
发送变化的块
        ↓
接收端重建 LV
```

#### 2.2 直接使用 thin-send-recv

```bash
# 1. 创建快照
lvcreate -s -n devbox001snapshot lvm-vg/devbox001

# 2. 发送增量数据到管道
thin_send lvm-vg/devbox001snapshot lvm-vg/devbox001 | \
    # 3. 处理数据流（转换/打包）
    process_thin_output | \
    # 4. 创建镜像
```

**⚠️ 关键问题**:
- thin-send-recv 的输出是**块级数据流**
- 接收端必须是 thin LV（不适合直接打包镜像）
- 需要额外的转换层来提取文件

#### 2.3 读取 LVM 元数据

```go
// 伪代码：读取 LVM thin snapshot 的元数据
func GetChangedBlocks(vgName, lvName, snapName string) ([]uint64, error) {
    // 1. 使用 lvs 命令获取设备信息
    cmd := exec.Command("lvs", "--noheadings", "-o",
                        "lv_kernel_major,lv_kernel_minor",
                        fmt.Sprintf("%s/%s", vgName, snapName))
    output, err := cmd.Output()

    // 2. 解析 major/minor 号
    major, minor := parseMajorMinor(string(output))

    // 3. 读取 device-mapper 的 status
    statusFile := fmt.Sprintf("/sys/block/dm-%d/dm/status", minor)
    data, err := os.ReadFile(statusFile)

    // 4. 解析 thin snapshot 的元数据
    // 找出哪些块被修改了
    changedBlocks := parseThinSnapshotMetadata(data)

    return changedBlocks, nil
}

// 提取变化的块数据
func ExtractChangedBlocks(vgName, lvName, snapName string, changedBlocks []uint64) ([]byte, error) {
    devicePath := fmt.Sprintf("/dev/%s/%s", vgName, snapName)
    f, err := os.Open(devicePath)
    if err != nil {
        return nil, err
    }
    defer f.Close()

    blockSize := int64(4096) // 默认块大小
    buf := make([]byte, blockSize)

    var result []byte
    for _, blockNum := range changedBlocks {
        f.Seek(blockNum*blockSize, 0)
        n, _ := f.Read(buf)
        result = append(result, buf[:n]...)
    }

    return result, nil
}
```

**优点**:
- ✅ 真正的块级 diff
- ✅ 精确，不依赖文件系统
- ✅ 不需要挂载

**缺点**:
- ❌ LVM 没有直接暴露"变化块列表"的 API
- ❌ 需要解析 device-mapper 的内部数据结构
- ❌ 输出是块数据，不是文件，难以直接打包成镜像

---

### 方案 3: 混合方案（推荐）

#### 3.1 思路

结合文件级 diff 和块级优化的优势：
- 文件级 diff 提供基础变化检测
- 块级 diff 优化大文件传输

#### 3.2 实现步骤

```
Step 1: 创建快照
lvcreate -s -n devbox001snapshot lvm-vg/devbox001

Step 2: 获取文件级 diff（方案 1）
→ 得到变化的文件列表

Step 3: 块级优化
→ 对于大文件（> 10MB），使用块级 diff
→ 找出文件内哪些块变化了
→ 只发送变化的块

Step 4: 打包
→ 小文件：完整打包
→ 大文件变化块：增量打包
→ 创建镜像层
```

#### 3.3 大文件的块级 diff

```go
// ComputeBlockLevelDiff 计算大文件的块级差异
func ComputeBlockLevelDiff(file1, file2 string, blockSize int64) ([]BlockRange, error) {
    f1, err := os.Open(file1)
    if err != nil {
        return nil, err
    }
    defer f1.Close()

    f2, err := os.Open(file2)
    if err != nil {
        return nil, err
    }
    defer f2.Close()

    var ranges []BlockRange
    buf1 := make([]byte, blockSize)
    buf2 := make([]byte, blockSize)

    blockIndex := int64(0)
    var inChangedRange bool
    var currentRange *BlockRange

    for {
        n1, err1 := f1.Read(buf1)
        n2, err2 := f2.Read(buf2)

        if err1 == io.EOF && err2 == io.EOF {
            break
        }

        // 比较
        changed := !bytes.Equal(buf1[:n1], buf2[:n2])

        if changed && !inChangedRange {
            // 开始新的变化范围
            currentRange = &BlockRange{
                Start: blockIndex,
                End:   blockIndex,
            }
            inChangedRange = true
        } else if changed && inChangedRange {
            // 继续当前范围
            currentRange.End = blockIndex
        } else if !changed && inChangedRange {
            // 结束当前范围
            ranges = append(ranges, *currentRange)
            inChangedRange = false
            currentRange = nil
        }

        blockIndex++
    }

    if inChangedRange && currentRange != nil {
        ranges = append(ranges, *currentRange)
    }

    return ranges, nil
}

// BlockRange 块范围
type BlockRange struct {
    Start int64
    End   int64
}

// ExtractChangedBlocksFromRange 提取变化范围的块数据
func ExtractChangedBlocksFromRange(filePath string, ranges []BlockRange, blockSize int64) (map[int64][]byte, error) {
    f, err := os.Open(filePath)
    if err != nil {
        return nil, err
    }
    defer f.Close()

    result := make(map[int64][]byte)
    buf := make([]byte, blockSize)

    for _, r := range ranges {
        for i := r.Start; i <= r.End; i++ {
            f.Seek(i*blockSize, 0)
            n, err := f.Read(buf)
            if err != nil {
                continue
            }
            result[i] = make([]byte, n)
            copy(result[i], buf[:n])
        }
    }

    return result, nil
}
```

#### 3.4 完整流程

```go
// CommitWithHybridDiff 混合 diff 的 commit
func CommitWithHybridDiff(ctx context.Context, vgName, lvName string) error {
    const largeFileThreshold = 10 * 1024 * 1024 // 10MB
    const blockSize = 64 * 1024                  // 64KB

    // 1. 创建快照
    snapPath, err := CreateDevboxLVSnapshot(ctx, vgName, lvName)
    if err != nil {
        return err
    }
    defer DestroyDevboxLVSnapshot(ctx, vgName, lvName)

    // 2. 挂载
    originalMount, snapshotMount, err := mountBoth(ctx, vgName, lvName, snapPath)
    if err != nil {
        return err
    }
    defer unmountBoth(originalMount, snapshotMount)

    // 3. 文件级 diff
    fileChanges, err := ComputeFilesystemDiff(originalMount, snapshotMount)
    if err != nil {
        return err
    }

    // 4. 处理每个变化文件
    for _, change := range fileChanges {
        srcPath := filepath.Join(originalMount, change.Path)
        snapPath := filepath.Join(snapshotMount, change.Path)

        if change.Size < largeFileThreshold {
            // 小文件：完整处理
            if err := addFileToArchive(srcPath, change.Path); err != nil {
                return err
            }
        } else {
            // 大文件：块级 diff
            ranges, err := ComputeBlockLevelDiff(snapPath, srcPath, blockSize)
            if err != nil {
                return err
            }

            blocks, err := ExtractChangedBlocksFromRange(srcPath, ranges, blockSize)
            if err != nil {
                return err
            }

            if err := addBlocksToArchive(blocks, change.Path); err != nil {
                return err
            }
        }
    }

    // 5. 创建镜像
    return createImageFromArchive()
}
```

**优点**:
- ✅ 平衡精度和性能
- ✅ 小文件快速处理
- ✅ 大文件优化传输
- ✅ 灵活可扩展

**缺点**:
- ⚠️ 实现复杂度较高
- ⚠️ 需要挂载文件系统

---

### 方案 4: 基于文件系统特性

#### 4.1 使用 btrfs/zfs 子卷

**前提**: 使用 btrfs 或 zfs 作为 LV 的文件系统

```bash
# btrfs
btrfs subvolume snapshot /mnt/original /mnt/snapshot
btrfs send /mnt/snapshot | btrfs receive /mnt/backup

# zfs
zfs snapshot pool/ds@snap1
zfs send pool/ds@snap1 | zfs receive pool/backup
```

**优点**:
- ✅ 原生支持增量发送
- ✅ 性能极佳
- ✅ 实现简单

**缺点**:
- ❌ Devbox 当前使用 ext4
- ❌ 需要更换文件系统（成本高）

#### 4.2 利用 ext4 的 file system features

```bash
# 使用 filefrag 查看文件的块映射
filefrag -v /mnt/original/large_file.dat

# 使用 debugfs 读取文件系统元数据
debugfs -R "stat /path/to/file" /dev/lvm-vg/devbox001

# 使用 fiemap 系统调用
ioctl(fd, FIEMAP, &fiemap)
```

**Go 实现示例**:

```go
import (
    "syscall"
    "unsafe"
)

// Fiemap 文件扩展映射
type Fiemap struct {
    Start          uint64
    Length         uint64
    Flags          uint32
    MappedExtents  uint32
    ExtentCount    uint32
    Reserved       uint32
    Extents        [1]FiemapExtent
}

// FiemapExtent 文件扩展
type FiemapExtent struct {
    Logical    uint64
    Physical   uint64
    Length     uint64
    Flags      uint32
    Reserved   [3]uint32
}

// GetFileExtents 获取文件的物理块映射
func GetFileExtents(filePath string) ([]FiemapExtent, error) {
    fd, err := syscall.Open(filePath, syscall.O_RDONLY, 0)
    if err != nil {
        return nil, err
    }
    defer syscall.Close(fd)

    const FIEMAP = 0xC020660B
    var fiemap Fiemap
    fiemap.ExtentCount = 256

    _, _, errno := syscall.Syscall(
        syscall.SYS_IOCTL,
        uintptr(fd),
        uintptr(FIEMAP),
        uintptr(unsafe.Pointer(&fiemap)),
    )

    if errno != 0 {
        return nil, errno
    }

    extents := make([]FiemapExtent, fiemap.MappedExtents)
    for i := uint32(0); i < fiemap.MappedExtents; i++ {
        extents[i] = fiemap.Extents[i]
    }

    return extents, nil
}
```

---

### 方案 5: Btrfs-style COW with ext4

**思路**: 在 ext4 上实现类似 btrfs 的 COW 机制

```
用户修改文件时：
1. 检测到快照存在
2. 将旧数据复制到 COW 区域
3. 快照指向旧数据
4. 原文件指向新数据
```

**实现方式**:
- 使用 FUSE 拦截文件系统操作
- 或修改内核 VFS 层

**复杂度**: 极高，需要内核级开发

---

## 三、方案对比

| 方案 | 复杂度 | 精度 | 性能 | 可行性 | 推荐度 |
|------|-------|------|------|-------|-------|
| 文件级 diff (rsync) | 低 | 中 | 中 | ✅ 高 | ⭐⭐⭐⭐ |
| 文件级 diff (Go) | 中 | 中 | 高 | ✅ 高 | ⭐⭐⭐⭐⭐ |
| thin-send-recv | 高 | 高 | 高 | ⚠️ 中 | ⭐⭐⭐ |
| 块级 diff (LVM API) | 极高 | 高 | 高 | ❌ 低 | ⭐⭐ |
| 混合方案 | 高 | 高 | 高 | ✅ 高 | ⭐⭐⭐⭐ |
| Btrfs/ZFS | 低 | 高 | 极高 | ❌ 低（需换 FS） | ⭐ |
| ext4 fiemap | 中 | 高 | 高 | ✅ 中 | ⭐⭐⭐ |

**详细说明**:

1. **文件级 diff (Go)** - ⭐⭐⭐⭐⭐
   - 最适合 Devbox 场景
   - 实现难度适中
   - 性能可接受
   - 易于维护和扩展

2. **混合方案** - ⭐⭐⭐⭐
   - 适合有大量大文件的场景
   - 实现复杂度较高
   - 可作为后续优化方向

3. **thin-send-recv** - ⭐⭐⭐
   - 技术上可行但不太适合
   - 输出格式不匹配镜像打包需求
   - 可以借鉴其思路

4. **块级 diff (LVM API)** - ⭐⭐
   - LVM 没有直接 API
   - 需要解析内部数据结构
   - 维护成本高

---

## 四、推荐实现路径

### 阶段 1: 文件级 diff（MVP）

**目标**: 快速实现可用的 commit

**实现步骤**:

```go
// 在 lvm.go 中添加

// CommitSnapshot 基于快照的 commit
func CommitSnapshot(ctx context.Context, vgName, lvName string) error {
    // 1. 创建快照
    snapPath, err := CreateDevboxLVSnapshot(ctx, vgName, lvName)
    if err != nil {
        return errors.Wrap(err, "failed to create snapshot")
    }
    defer DestroyDevboxLVSnapshot(ctx, vgName, lvName)

    // 2. 挂载
    originalMount, snapshotMount, err := mountForDiff(ctx, vgName, lvName, snapPath)
    if err != nil {
        return errors.Wrap(err, "failed to mount")
    }
    defer unmountForDiff(originalMount, snapshotMount)

    // 3. 扫描文件差异
    changes, err := ComputeFilesystemDiff(originalMount, snapshotMount)
    if err != nil {
        return errors.Wrap(err, "failed to compute diff")
    }

    log.Infof("Found %d changed files", len(changes))

    // 4. 返回变化的文件列表（供调用者打包镜像）
    return nil
}

// ComputeFilesystemDiff 计算文件系统差异
func ComputeFilesystemDiff(originalDir, snapshotDir string) ([]FileChange, error) {
    // 见上文实现
}
```

**测试验证**:

```go
func TestCommitSnapshot(t *testing.T) {
    ctx := context.Background()

    // 创建测试 LV
    createTestLV(ctx, t)

    // 修改一些文件
    modifyTestFiles(ctx, t)

    // 执行 commit
    err := CommitSnapshot(ctx, testVGName, testLVName)
    if err != nil {
        t.Fatalf("CommitSnapshot failed: %v", err)
    }

    // 验证结果
}
```

### 阶段 2: 优化性能

**优化方向**:

1. **并行扫描**
```go
func ComputeFilesystemDiffParallel(originalDir, snapshotDir string, workers int) ([]FileChange, error) {
    // 使用 worker pool 并行处理
}
```

2. **缓存文件元数据**
```go
type FileMetadataCache struct {
    cache map[string]os.FileInfo
    mutex sync.RWMutex
}
```

3. **增量打包优化**
```go
func StreamChangesToTar(changes []FileChange, baseDir string, w io.Writer) error {
    // 流式打包，避免内存占用过高
}
```

### 阶段 3: 块级优化（可选）

**实现场景**: 当有大量大文件（> 10MB）时

```go
// EnhancedFileChange 增强的文件变化记录
type EnhancedFileChange struct {
    Type      FileChangeType
    Path      string
    Size      int64
    BlockRanges []BlockRange // 块级变化范围（仅大文件）
}

func ComputeEnhancedDiff(originalDir, snapshotDir string) ([]EnhancedFileChange, error) {
    // 文件级 + 块级混合
}
```

---

## 五、其他开源参考

### 5.1 restic

- **项目**: https://github.com/restic/restic
- **特点**: 去重备份工具
- **算法**: Chunker 算法（CDC - Content Defined Chunking）
- **参考价值**: 优秀的去重和分块算法

### 5.2 casync

- **项目**: https://github.com/systemd/casync
- **特点**: 内容寻址存储，支持块级 diff
- **算法**: 固定大小分块 + 去重
- **参考价值**: 块级存储和索引设计

### 5.3 borg

- **项目**: https://github.com/borgbackup/borg
- **特点**: 去重备份
- **算法**: 固定大小 chunking
- **参考价值**: 高效的压缩和加密

### 5.4 rdiff-backup

- **项目**: http://rdiff-backup.net/
- **特点**: 基于 librsync 的增量备份
- **算法**: rsync rolling checksum
- **参考价值**: 增量检测算法

---

## 六、总结

### 6.1 关键发现

1. **thin-send-recv** 适合块级传输，但不适合镜像打包场景
2. **文件级 diff** 是最可行的方案，平衡了复杂度和性能
3. **混合方案** 可以作为后续优化方向

### 6.2 推荐方案

**第一阶段（MVP）**:
- 使用 Go 实现文件级 diff
- 挂载快照和原始 LV
- 遍历对比文件变化
- 返回变化文件列表供打包

**后续优化**:
- 并行处理提升性能
- 大文件使用块级 diff
- 集成到 Devbox Controller 的 commit 流程

### 6.3 技术风险

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| 挂载冲突 | 中 | 使用唯一挂载点 |
| 性能问题 | 中 | 并行处理 + 缓存 |
| 大文件处理 | 低 | 分块处理 |
| 快照空间耗尽 | 中 | 监控 data_percent |

---

**文档版本**: v1.0
**创建时间**: 2025-01-22
**作者**: lzc
**最后更新**: 2025-01-22
