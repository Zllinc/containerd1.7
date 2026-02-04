# 基于 LVM 快照的 Commit 实现方案详解

## 一、架构理解

### 当前架构

```
┌─────────────────────────────────────────────────────────┐
│                    Devbox 容器                          │
│  ┌───────────────────────────────────────────────────┐  │
│  │  可写层：LV (devbox-xxx)                           │  │
│  │  - 包含：只读层最上层拷贝 + 用户写入的数据          │  │
│  │  - 容器实际运行在 LV 上                             │  │
│  └───────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────┘
                        ↑
                        │ Prepare 时拷贝
                        │
┌─────────────────────────────────────────────────────────┐
│         只读层（OverlayFS 分层结构）                     │
│  ┌───────────────────────────────────────────────────┐  │
│  │  Layer 1 (base)                                    │  │
│  │  Layer 2 (runtime)                                 │  │
│  │  Layer 3 (dependencies)                            │  │
│  │  ...                                               │  │
│  │  Layer N (top layer)                               │  │
│  └───────────────────────────────────────────────────┘  │
│         ↓ OverlayFS 合并 ↓                              │
│     只读层最上层（完整文件系统视图）                      │
└─────────────────────────────────────────────────────────┘
```

### Commit 流程

```
T0: Prepare 阶段
    - 将只读层最上层拷贝到可写层 LV
    - LV 包含：只读层最上层数据

T1: 用户使用阶段
    - 用户在 LV 上写入数据
    - LV 现在包含：只读层最上层 + 用户写入的数据

T2: Commit 触发
    - 对可写层 LV 创建快照
    - 快照保存：只读层最上层 + 用户写入的数据（T2时刻）

T3: Diff 计算
    - 需要找出：用户写入的数据（差异部分）
    - 对比：快照（只读层最上层 + 用户数据） vs 只读层最上层
    - 结果：只包含用户写入的差异

T4: 打包推送
    - 将差异打包为容器镜像层
    - 推送到 Registry
```

## 二、两种实现方案

### 方案 1：快照 vs 只读层文件系统 Diff

**核心思路**：挂载快照 LV 和只读层最上层，使用文件系统工具进行文件级别的 diff

#### 1.1 架构设计

```
Commit 时：
┌─────────────────────────────────────────┐
│  快照 LV (已挂载)                        │
│  /mnt/snapshot                           │
│  - 包含：只读层最上层 + 用户写入的数据    │
└─────────────────────────────────────────┘
              │
              │ 文件系统 diff
              ↓
┌─────────────────────────────────────────┐
│  只读层最上层 (已挂载)                    │
│  /mnt/readonly-top                       │
│  - 包含：只读层最上层（原始状态）          │
└─────────────────────────────────────────┘
              │
              ↓
        差异文件列表
              │
              ↓
        打包为镜像层
```

#### 1.2 详细实现步骤

**步骤 1：获取只读层最上层路径**

```go
// 从 snapshot metadata 获取 parent IDs
func getReadOnlyTopLayer(ctx context.Context, snapshotKey string) (string, error) {
    id, info, _, err := storage.GetInfo(ctx, snapshotKey)
    if err != nil {
        return "", err
    }
    
    // 获取 parent IDs（只读层）
    if len(info.ParentIDs) == 0 {
        return "", fmt.Errorf("no parent layers found")
    }
    
    // 只读层最上层是第一个 parent
    topParentID := info.ParentIDs[0]
    topLayerPath := filepath.Join(o.root, "snapshots", topParentID, "fs")
    
    return topLayerPath, nil
}
```

**步骤 2：创建快照并挂载**

```go
func commitWithSnapshotDiff(ctx context.Context, vgName, lvName, snapshotKey string) error {
    // 1. 创建快照
    snapshotPath, err := lvm.CreateDevboxLVSnapshot(ctx, vgName, lvName)
    if err != nil {
        return fmt.Errorf("failed to create snapshot: %w", err)
    }
    defer lvm.DestroyDevboxLVSnapshot(ctx, vgName, lvName)
    
    // 2. 获取只读层最上层路径
    readonlyTopPath, err := getReadOnlyTopLayer(ctx, snapshotKey)
    if err != nil {
        return err
    }
    
    // 3. 创建临时挂载点
    snapshotMount := "/tmp/commit-snapshot"
    readonlyMount := "/tmp/commit-readonly"
    os.MkdirAll(snapshotMount, 0755)
    os.MkdirAll(readonlyMount, 0755)
    defer os.RemoveAll(snapshotMount)
    defer os.RemoveAll(readonlyMount)
    
    // 4. 挂载快照 LV（只读）
    snapshotLV := fmt.Sprintf("/dev/%s", snapshotPath)
    cmd := exec.Command("mount", "-o", "ro", snapshotLV, snapshotMount)
    if err := cmd.Run(); err != nil {
        return fmt.Errorf("failed to mount snapshot: %w", err)
    }
    defer exec.Command("umount", snapshotMount).Run()
    
    // 5. 挂载只读层最上层（只读）
    cmd = exec.Command("mount", "-o", "ro", "--bind", readonlyTopPath, readonlyMount)
    if err := cmd.Run(); err != nil {
        return fmt.Errorf("failed to mount readonly layer: %w", err)
    }
    defer exec.Command("umount", readonlyMount).Run()
    
    // 6. 计算文件系统差异
    return calculateFileSystemDiff(snapshotMount, readonlyMount)
}
```

**步骤 3：使用 rsync 计算差异**

```go
func calculateFileSystemDiff(snapshotPath, readonlyPath string) error {
    // 使用 rsync 的 dry-run 模式计算差异
    // --itemize-changes: 详细输出每个变化
    // --delete: 检测删除的文件
    // -a: archive mode（保留权限、时间戳等）
    cmd := exec.Command("rsync",
        "-n",                    // dry run，不实际复制
        "-a",                    // archive mode
        "-v",                    // verbose
        "--itemize-changes",      // 详细输出变化
        "--delete",               // 检测删除的文件
        snapshotPath + "/",
        readonlyPath + "/",
    )
    
    output, err := cmd.Output()
    if err != nil {
        return fmt.Errorf("rsync diff failed: %w", err)
    }
    
    // 7. 解析 rsync 输出
    changes := parseRsyncOutput(output)
    
    // 8. 打包变化的文件
    return createTarFromChanges(changes, snapshotPath)
}
```

**步骤 4：解析 rsync 输出**

```go
type FileChange struct {
    Path    string
    Type    ChangeType // Add, Modify, Delete
    Size    int64
    ModTime time.Time
}

type ChangeType int
const (
    ChangeAdd ChangeType = iota
    ChangeModify
    ChangeDelete
)

func parseRsyncOutput(output []byte) ([]FileChange, error) {
    var changes []FileChange
    lines := strings.Split(string(output), "\n")
    
    for _, line := range lines {
        if len(line) < 12 {
            continue
        }
        
        // rsync 输出格式：
        // >f.st.... file.txt    # 文件被修改
        // >f+++++++ newfile.txt # 新文件
        // *deleting  oldfile.txt # 删除的文件
        
        indicator := line[0:11]
        filePath := strings.TrimSpace(line[11:])
        
        if strings.HasPrefix(indicator, "*deleting") {
            changes = append(changes, FileChange{
                Path: filePath,
                Type: ChangeDelete,
            })
        } else if strings.Contains(indicator, "+++++++") {
            // 新文件
            changes = append(changes, FileChange{
                Path: filePath,
                Type: ChangeAdd,
            })
        } else if strings.Contains(indicator, "st") || strings.Contains(indicator, "c") {
            // 修改的文件（size/time 变化或内容变化）
            changes = append(changes, FileChange{
                Path: filePath,
                Type: ChangeModify,
            })
        }
    }
    
    return changes, nil
}
```

**步骤 5：打包变化的文件**

```go
func createTarFromChanges(changes []FileChange, basePath string) error {
    // 创建 tar 文件
    tarFile, err := os.Create("layer.tar")
    if err != nil {
        return err
    }
    defer tarFile.Close()
    
    tw := tar.NewWriter(tarFile)
    defer tw.Close()
    
    for _, change := range changes {
        if change.Type == ChangeDelete {
            // 对于删除的文件，在 tar 中记录 whiteout 文件
            // Docker/OCI 镜像使用 .wh.<filename> 表示删除
            whiteoutPath := ".wh." + strings.TrimPrefix(change.Path, "/")
            header := &tar.Header{
                Name: whiteoutPath,
                Size: 0,
                Typeflag: tar.TypeReg,
            }
            tw.WriteHeader(header)
            continue
        }
        
        // 添加或修改的文件
        fullPath := filepath.Join(basePath, change.Path)
        file, err := os.Open(fullPath)
        if err != nil {
            continue // 跳过无法打开的文件
        }
        defer file.Close()
        
        info, err := file.Stat()
        if err != nil {
            continue
        }
        
        header, err := tar.FileInfoHeader(info, "")
        if err != nil {
            continue
        }
        header.Name = change.Path
        
        tw.WriteHeader(header)
        io.Copy(tw, file)
    }
    
    return nil
}
```

#### 1.3 方案 1 的优缺点

**优点**：
- ✅ 实现相对简单，使用成熟的工具（rsync）
- ✅ 不需要修改 Prepare 流程
- ✅ 可以精确识别文件级别的变化
- ✅ 支持权限、时间戳等元数据

**缺点**：
- ⚠️ 需要挂载两个文件系统（快照 LV 和只读层）
- ⚠️ 需要遍历整个文件系统（大文件系统可能较慢）
- ⚠️ 依赖文件系统工具（rsync）

---

### 方案 2：LV 级别的块级 Diff

**核心思路**：在 Prepare 时将只读层最上层拷贝到 LV，Commit 时对 LV 创建快照，然后对比两个 LV（块设备）的差异

#### 2.1 架构设计

```
Prepare 时：
┌─────────────────────────────────────────┐
│  只读层最上层                            │
│  /snapshots/parent-id/fs                 │
└─────────────────────────────────────────┘
              │
              │ 拷贝到 LV
              ↓
┌─────────────────────────────────────────┐
│  可写层 LV (devbox-xxx)                  │
│  - 包含：只读层最上层数据（完整拷贝）      │
└─────────────────────────────────────────┘

Commit 时：
┌─────────────────────────────────────────┐
│  可写层 LV (devbox-xxx)                  │
│  - 包含：只读层最上层 + 用户写入的数据    │
└─────────────────────────────────────────┘
              │
              │ 创建快照
              ↓
┌─────────────────────────────────────────┐
│  快照 LV (devbox-xxx-snapshot)          │
│  - 保存：只读层最上层 + 用户写入的数据    │
└─────────────────────────────────────────┘
              │
              │ 块级 diff
              ↓
┌─────────────────────────────────────────┐
│  原始 LV (devbox-xxx)                    │
│  - 当前：只读层最上层 + 用户写入的数据    │
│  - 但我们需要对比的是：                   │
│    快照 vs 只读层最上层（需要另一个 LV）  │
└─────────────────────────────────────────┘
```

**关键问题**：我们需要一个包含"只读层最上层"的 LV 来对比。

**解决方案**：在 Prepare 时，除了将只读层拷贝到可写层 LV，还可以创建一个"基准 LV"保存只读层最上层的状态。

#### 2.2 改进的架构设计

```
Prepare 时：
┌─────────────────────────────────────────┐
│  只读层最上层                            │
└─────────────────────────────────────────┘
              │
              ├─→ 拷贝到可写层 LV (devbox-xxx)
              │   - 用于容器运行
              │
              └─→ 拷贝到基准 LV (devbox-xxx-base)
                  - 用于 commit 时对比
                  - 只读，不挂载

Commit 时：
┌─────────────────────────────────────────┐
│  可写层 LV (devbox-xxx)                  │
│  - 包含：只读层最上层 + 用户写入的数据    │
└─────────────────────────────────────────┘
              │
              │ 创建快照
              ↓
┌─────────────────────────────────────────┐
│  快照 LV (devbox-xxx-snapshot)          │
│  - 保存：只读层最上层 + 用户写入的数据    │
└─────────────────────────────────────────┘
              │
              │ 块级 diff
              ↓
┌─────────────────────────────────────────┐
│  基准 LV (devbox-xxx-base)              │
│  - 包含：只读层最上层（原始状态）          │
└─────────────────────────────────────────┘
```

#### 2.3 详细实现步骤

**步骤 1：Prepare 时创建基准 LV**

```go
func (o *Snapshotter) prepareLvmDirectory(ctx context.Context, snapshotDir string, contentKey string, useLimit string) (string, string, error) {
    lvName := "devbox-" + contentKey
    baseLVName := lvName + "-base"  // 基准 LV 名称
    
    // ... 创建可写层 LV 的代码 ...
    
    // 创建基准 LV（保存只读层最上层）
    baseVol := &apis.LVMVolume{
        ObjectMeta: metav1.ObjectMeta{
            Name: baseLVName,
        },
        Spec: apis.VolumeInfo{
            Capacity:      capacity,  // 与可写层相同大小
            VolGroup:      o.lvmVgName,
            ThinProvision: o.ThinPoolName,
        },
    }
    
    err = lvm.CreateVolume(ctx, baseVol)
    if err != nil {
        return td, lvName, fmt.Errorf("failed to create base LV: %w", err)
    }
    
    // 格式化基准 LV
    if err = o.mkfs(baseLVName); err != nil {
        return td, lvName, fmt.Errorf("failed to format base LV: %w", err)
    }
    
    // 挂载基准 LV
    baseMount := "/tmp/base-mount"
    os.MkdirAll(baseMount, 0755)
    if err = o.mountLvm(ctx, baseLVName, baseMount); err != nil {
        return td, lvName, err
    }
    
    // 获取只读层最上层路径
    readonlyTopPath := o.getReadOnlyTopLayerPath(ctx, contentKey)
    
    // 拷贝只读层最上层到基准 LV
    if err = o.copyToLV(readonlyTopPath, baseMount); err != nil {
        return td, lvName, fmt.Errorf("failed to copy readonly layer to base LV: %w", err)
    }
    
    // 卸载基准 LV（保持只读状态）
    o.unmountLvm(ctx, baseMount)
    
    // 保存基准 LV 名称到 metadata
    storage.SetBaseLVName(ctx, contentKey, baseLVName)
    
    // ... 继续创建可写层 LV 的代码 ...
}
```

**步骤 2：Commit 时进行块级 Diff**

```go
func commitWithBlockLevelDiff(ctx context.Context, vgName, lvName, contentKey string) error {
    // 1. 创建快照
    snapshotPath, err := lvm.CreateDevboxLVSnapshot(ctx, vgName, lvName)
    if err != nil {
        return fmt.Errorf("failed to create snapshot: %w", err)
    }
    defer lvm.DestroyDevboxLVSnapshot(ctx, vgName, lvName)
    
    // 2. 获取基准 LV 名称
    baseLVName, err := storage.GetBaseLVName(ctx, contentKey)
    if err != nil {
        return err
    }
    
    snapshotLV := fmt.Sprintf("/dev/%s", snapshotPath)
    baseLV := fmt.Sprintf("/dev/%s/%s", vgName, baseLVName)
    
    // 3. 使用块级工具进行 diff
    return calculateBlockLevelDiff(snapshotLV, baseLV)
}
```

**步骤 3：块级 Diff 实现**

有几种方式可以实现块级 diff：

##### 方式 A：使用 `dd` + `cmp` 逐块对比

```go
func calculateBlockLevelDiff(snapshotLV, baseLV string) error {
    // 1. 获取 LV 大小
    snapshotSize := getLVSize(snapshotLV)
    baseSize := getLVSize(baseLV)
    
    if snapshotSize != baseSize {
        return fmt.Errorf("LV sizes don't match")
    }
    
    // 2. 逐块读取并对比
    blockSize := 4096  // 4KB blocks
    numBlocks := snapshotSize / blockSize
    
    var changedBlocks []uint64
    
    snapshotFile, _ := os.Open(snapshotLV)
    baseFile, _ := os.Open(baseLV)
    defer snapshotFile.Close()
    defer baseFile.Close()
    
    snapshotBuf := make([]byte, blockSize)
    baseBuf := make([]byte, blockSize)
    
    for i := uint64(0); i < numBlocks; i++ {
        snapshotFile.ReadAt(snapshotBuf, int64(i*blockSize))
        baseFile.ReadAt(baseBuf, int64(i*blockSize))
        
        if !bytes.Equal(snapshotBuf, baseBuf) {
            changedBlocks = append(changedBlocks, i)
        }
    }
    
    // 3. 将变化的块映射到文件
    return mapBlocksToFiles(changedBlocks, snapshotLV)
}
```

##### 方式 B：使用 Device Mapper 的 COW 表

```go
func calculateBlockLevelDiffFromCOW(snapshotLV, baseLV string) error {
    // 1. 获取快照的 device mapper 名称
    dmName := getDeviceMapperName(snapshotLV)
    
    // 2. 读取 COW 表（通过 /sys/fs/devicemapper）
    cowTablePath := fmt.Sprintf("/sys/fs/devicemapper/%s/snapshot", dmName)
    cowData, err := os.ReadFile(cowTablePath)
    if err != nil {
        return fmt.Errorf("failed to read COW table: %w", err)
    }
    
    // 3. 解析 COW 表，获取变化的块
    changedBlocks := parseCOWTable(cowData)
    
    // 4. 将变化的块映射到文件
    return mapBlocksToFiles(changedBlocks, snapshotLV)
}
```

**步骤 4：将变化的块映射到文件**

```go
func mapBlocksToFiles(changedBlocks []uint64, lvPath string) error {
    // 1. 挂载 LV
    mountPoint := "/tmp/diff-mount"
    os.MkdirAll(mountPoint, 0755)
    exec.Command("mount", lvPath, mountPoint).Run()
    defer exec.Command("umount", mountPoint).Run()
    
    // 2. 使用 debugfs 或类似工具获取块的 inode 映射
    // 对于 ext4: debugfs -R "icheck <block>" /dev/lv
    // 对于 xfs: xfs_db -c "blockget -b <block>" /dev/lv
    
    var changedFiles []string
    fileSet := make(map[string]bool)
    
    for _, block := range changedBlocks {
        // 获取块对应的 inode
        inode := getInodeForBlock(lvPath, block)
        if inode == 0 {
            continue
        }
        
        // 获取 inode 对应的文件路径
        filePath := getFilePathForInode(mountPoint, inode)
        if filePath != "" && !fileSet[filePath] {
            changedFiles = append(changedFiles, filePath)
            fileSet[filePath] = true
        }
    }
    
    // 3. 打包变化的文件
    return createTarFromFileList(changedFiles, mountPoint)
}
```

#### 2.4 方案 2 的优缺点

**优点**：
- ✅ 性能最优，只处理变化的块
- ✅ 不需要遍历整个文件系统
- ✅ 可以在块级别精确识别变化
- ✅ 不需要挂载只读层

**缺点**：
- ⚠️ 需要额外的基准 LV（占用存储空间）
- ⚠️ 实现复杂度高（块到文件的映射）
- ⚠️ 需要理解文件系统内部结构（inode、块映射）
- ⚠️ 需要修改 Prepare 流程

---

## 三、方案对比

| 特性 | 方案 1：文件系统 Diff | 方案 2：块级 Diff |
|------|---------------------|------------------|
| **实现难度** | ⭐⭐ | ⭐⭐⭐⭐⭐ |
| **性能** | ⭐⭐⭐ | ⭐⭐⭐⭐⭐ |
| **存储开销** | 无额外开销 | 需要基准 LV |
| **精确度** | 文件级别 | 块级别 |
| **依赖** | rsync/tar | 文件系统工具（debugfs等） |
| **适用场景** | 中小型文件系统 | 大型文件系统，性能敏感 |

## 四、推荐实施路径

### 阶段 1：实现方案 1（文件系统 Diff）

**理由**：
- 实现简单，可以快速验证功能
- 不需要修改 Prepare 流程
- 对于大多数场景性能足够

**实施步骤**：
1. 实现快照创建和挂载
2. 实现只读层最上层路径获取
3. 实现 rsync diff 和结果解析
4. 实现 tar 打包

### 阶段 2：优化方案 1

**优化方向**：
- 并行处理多个目录
- 增量检测（只检查变化的目录）
- 缓存文件列表

### 阶段 3：探索方案 2（可选）

**前提条件**：
- 方案 1 性能不满足需求
- 有足够的存储空间用于基准 LV
- 需要块级别的精确控制

## 五、关键注意事项

1. **快照空间管理**：确保快照有足够空间，监控使用率
2. **挂载点管理**：避免挂载点冲突，确保正确清理
3. **错误处理**：快照空间不足、挂载失败等边界情况
4. **性能考虑**：大文件系统的 diff 可能需要较长时间
5. **一致性保证**：确保 diff 期间数据一致性
6. **基准 LV 生命周期**：方案 2 需要管理基准 LV 的创建和删除
