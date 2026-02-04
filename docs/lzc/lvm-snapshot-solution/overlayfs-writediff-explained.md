# OverlayFS writeDiff 函数详解

**创建时间**: 2026-01-23  
**目的**: 详细解释 `diff/overlayfs/differ.go` 中的 `writeDiff` 函数实现

---

## 一、函数签名

```go
func writeDiff(ctx context.Context, w io.Writer, lower []mount.Mount, upperRoot string, sourceDateEpoch *time.Time) error
```

**参数说明**：
- `ctx`: 上下文（用于取消、超时等）
- `w io.Writer`: 输出流（tar 数据写入这里）
- `lower []mount.Mount`: 父层的 mount 描述（需要挂载以获取 lowerRoot）
- `upperRoot string`: 当前层的 upperdir 路径（已经是真实目录）
- `sourceDateEpoch *time.Time`: 时间戳上界（用于可重复构建）

**返回值**：
- `error`: 错误信息

**功能**：将 upperRoot 相对于 lowerRoot 的差异打包成 tar 流，写入 w

---

## 二、函数完整源码（带注释）

```go
func writeDiff(ctx context.Context, w io.Writer, lower []mount.Mount, upperRoot string, sourceDateEpoch *time.Time) error {
    // ========== 步骤 1: 准备选项 ==========
    var opts []archive.ChangeWriterOpt
    if sourceDateEpoch != nil {
        // 设置时间戳上界，确保可重复构建
        opts = append(opts, archive.WithModTimeUpperBound(*sourceDateEpoch))
    }

    // ========== 步骤 2: 挂载 lower，在回调中执行 diff ==========
    return mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
        // lowerRoot: lower 挂载到的临时目录，例如 /tmp/containerd-mount-123456
        
        // ========== 步骤 3: 创建 ChangeWriter ==========
        cw := archive.NewChangeWriter(w, upperRoot, opts...)
        // cw: 负责将文件变化写入 tar 流
        // w: tar 数据的目标（可能是文件、网络连接、压缩流等）
        // upperRoot: 数据源目录
        
        // ========== 步骤 4: 遍历目录，计算 diff ==========
        if err := fs.DiffDirChanges(ctx, lowerRoot, upperRoot, fs.DiffSourceOverlayFS, cw.HandleChange); err != nil {
            return fmt.Errorf("failed to calculate diff changes: %w", err)
        }
        // DiffDirChanges 会：
        // 1. 遍历 upperRoot 的所有文件
        // 2. 对比 lowerRoot，判断每个文件是新增、修改还是删除
        // 3. 对每个变化调用 cw.HandleChange(kind, path, fileInfo, err)
        
        // ========== 步骤 5: 关闭 tar writer ==========
        return cw.Close()
        // 写入 tar 结束标记，刷新缓冲区
    })
    // WithTempMount 会自动 umount 并清理临时目录
}
```

---

## 三、逐层剖析

### 3.1 第一层：mount.WithTempMount

**作用**：将 lower mount 挂载到临时目录，执行回调后自动清理

```go
mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
    // 在这个回调函数内：
    // - lower 已经挂载到 lowerRoot（临时目录）
    // - 可以访问 lowerRoot 下的所有文件
    // - 回调返回后，自动 umount 和删除 lowerRoot
    
    // ...执行 diff 操作...
    
    return nil
})
```

**实际执行的操作**：

```bash
# 伪代码（WithTempMount 内部实现）

# 1. 创建临时目录
lowerRoot=/tmp/containerd-mount-123456
mkdir -p $lowerRoot

# 2. 挂载 lower mounts
# 假设 lower 是：
#   Type: "overlay"
#   Options: ["lowerdir=/snapshots/1/fs:/snapshots/2/fs", "upperdir=/snapshots/3/fs", ...]
mount -t overlay overlay \
  -o lowerdir=/snapshots/1/fs:/snapshots/2/fs,upperdir=/snapshots/3/fs,workdir=/snapshots/3/work \
  $lowerRoot

# 3. 执行回调函数
callback(lowerRoot)

# 4. 清理（无论成功还是失败）
umount $lowerRoot
rm -rf $lowerRoot
```

**为什么需要挂载 lower？**

```
upperRoot:
/var/lib/containerd/.../snapshots/4/fs/  (当前层的 upperdir)
├── etc/hosts       ← 修改的文件
└── tmp/test.txt    ← 新增的文件

lowerRoot (挂载后):
/tmp/containerd-mount-123456/  (父层的完整视图)
├── bin/bash
├── etc/
│   ├── hosts       ← 原始版本
│   └── passwd
└── usr/

目的：
- 对比 upperRoot/etc/hosts 和 lowerRoot/etc/hosts
- 判断 upperRoot/etc/hosts 是"修改"（lower 有）还是"新增"（lower 没有）
- 判断 lowerRoot/bin/bash 是否在 upperRoot 被删除
```

### 3.2 第二层：archive.NewChangeWriter

**作用**：创建一个 tar writer，负责将文件变化写入 tar 流

```go
cw := archive.NewChangeWriter(w, upperRoot, opts...)
```

**ChangeWriter 结构**：

```go
type ChangeWriter struct {
    tw                *tar.Writer         // 底层的 tar writer
    source            string              // 数据源目录（upperRoot）
    modTimeUpperBound *time.Time          // 时间戳上界
    whiteoutT         time.Time           // whiteout 文件的时间戳
    inodeSrc          map[uint64]string   // inode → 第一次出现的文件路径
    inodeRefs         map[uint64][]string // inode → 所有引用（硬链接）
    addedDirs         map[string]struct{} // 已添加的目录
}
```

**初始化过程**：

```go
func NewChangeWriter(w io.Writer, source string, opts ...ChangeWriterOpt) *ChangeWriter {
    cw := &ChangeWriter{
        tw:        tar.NewWriter(w),    // 包装成 tar writer
        source:    source,               // 保存 upperRoot 路径
        whiteoutT: time.Now(),          // whiteout 的默认时间戳
        inodeSrc:  map[uint64]string{}, // 硬链接跟踪
        inodeRefs: map[uint64][]string{},
        addedDirs: map[string]struct{}{},
    }
    // 应用选项（例如 WithModTimeUpperBound）
    for _, o := range opts {
        o(cw)
    }
    return cw
}
```

**ChangeWriter 的职责**：

1. **接收文件变化通知**（通过 `HandleChange` 方法）
2. **写入 tar 头**（文件元数据）
3. **写入文件内容**
4. **处理特殊情况**（硬链接、whiteout、父目录等）

### 3.3 第三层：fs.DiffDirChanges

**作用**：遍历两个目录，找出所有文件变化，对每个变化调用回调函数

```go
err := fs.DiffDirChanges(ctx, lowerRoot, upperRoot, fs.DiffSourceOverlayFS, cw.HandleChange)
```

**函数签名**：

```go
func DiffDirChanges(
    ctx context.Context,
    baseDir string,           // lowerRoot（父层）
    diffDir string,           // upperRoot（当前层）
    source DiffSource,        // DiffSourceOverlayFS（指示使用 OverlayFS 特性）
    changeFn ChangeFunc,      // cw.HandleChange（每个变化的回调）
) error
```

**核心逻辑**（简化版）：

```go
func DiffDirChanges(ctx, baseDir, diffDir string, source DiffSource, changeFn ChangeFunc) error {
    // ========== 步骤 1: 配置差异检测选项 ==========
    var o *diffDirOptions
    switch source {
    case DiffSourceOverlayFS:
        o = &diffDirOptions{
            deleteChange: overlayFSWhiteoutConvert,  // 识别 .wh.* 文件
        }
    }
    
    changedDirs := make(map[string]struct{})
    
    // ========== 步骤 2: 遍历 upperRoot 的所有文件 ==========
    return filepath.Walk(diffDir, func(path string, f os.FileInfo, err error) error {
        if err != nil {
            return err
        }
        
        // ========== 步骤 2.1: 规范化路径 ==========
        // path: /var/lib/.../snapshots/4/fs/etc/hosts
        // → relPath: /etc/hosts
        relPath, err := filepath.Rel(diffDir, path)
        relPath = filepath.Join("/", relPath)
        
        // 跳过根目录
        if relPath == "/" {
            return nil
        }
        
        // ========== 步骤 2.2: 检查是否是 whiteout 文件 ==========
        var kind ChangeKind
        deletedFile := false
        
        if o.deleteChange != nil {
            // 检查文件名是否以 .wh. 开头
            deletedFile, err = overlayFSWhiteoutConvert(diffDir, relPath, f, changeFn)
            // 如果是 .wh.foo，会调用 changeFn(ChangeKindDelete, "/foo", ...)
            // deletedFile = true
        }
        
        // ========== 步骤 2.3: 判断文件变化类型 ==========
        if deletedFile {
            kind = ChangeKindDelete
        } else {
            // 默认是新增
            kind = ChangeKindAdd
            
            // 检查文件在 baseDir 是否存在
            basePath := filepath.Join(baseDir, relPath)
            stat, err := os.Stat(basePath)
            
            if err == nil {
                // 文件在 base 存在 → 这是修改
                kind = ChangeKindModify
                
                // 特殊情况：目录可能只是因为子文件修改而被遍历到
                if stat.IsDir() && f.IsDir() {
                    // 检查目录属性是否真的变了
                    if f.Size() == stat.Size() && 
                       f.Mode() == stat.Mode() && 
                       sameFsTime(f.ModTime(), stat.ModTime()) {
                        // 目录本身没变，跳过
                        return nil
                    }
                }
            }
        }
        
        // ========== 步骤 2.4: 调用回调函数处理变化 ==========
        return changeFn(kind, relPath, f, nil)
        // 这里调用的是 cw.HandleChange(kind, relPath, f, nil)
    })
}
```

**DiffDirChanges 的工作流程图**：

```
遍历 upperRoot
    ↓
/etc/hosts
    ├─ 规范化路径 → /etc/hosts
    ├─ 检查 whiteout? → No
    ├─ 检查 lowerRoot/etc/hosts 是否存在?
    │  └─ Yes → kind = ChangeKindModify
    └─ 调用 changeFn(ChangeKindModify, "/etc/hosts", fileInfo, nil)
        └─ 实际调用：cw.HandleChange(ChangeKindModify, "/etc/hosts", fileInfo, nil)

/tmp/test.txt
    ├─ 规范化路径 → /tmp/test.txt
    ├─ 检查 whiteout? → No
    ├─ 检查 lowerRoot/tmp/test.txt 是否存在?
    │  └─ No → kind = ChangeKindAdd
    └─ 调用 changeFn(ChangeKindAdd, "/tmp/test.txt", fileInfo, nil)

/.wh.oldfile
    ├─ 规范化路径 → /.wh.oldfile
    ├─ 检查 whiteout? → Yes
    │  └─ overlayFSWhiteoutConvert 处理：
    │     - 识别出这是删除标记
    │     - 调用 changeFn(ChangeKindDelete, "/oldfile", ...)
    │     - 返回 deletedFile = true
    └─ 跳过（已在 overlayFSWhiteoutConvert 中处理）
```

**OverlayFS Whiteout 处理**：

```go
// overlayFSWhiteoutConvert 的简化逻辑
func overlayFSWhiteoutConvert(diffDir, path string, f os.FileInfo, changeFn ChangeFunc) (bool, error) {
    basename := filepath.Base(path)
    
    // 检查文件名是否以 .wh. 开头
    if strings.HasPrefix(basename, ".wh.") {
        // 提取原始文件名
        // .wh.oldfile → oldfile
        originalName := strings.TrimPrefix(basename, ".wh.")
        originalPath := filepath.Join(filepath.Dir(path), originalName)
        
        // 调用回调，通知这是一个删除操作
        if err := changeFn(ChangeKindDelete, originalPath, f, nil); err != nil {
            return false, err
        }
        
        return true, nil  // deletedFile = true
    }
    
    return false, nil  // 不是 whiteout
}
```

### 3.4 第四层：cw.HandleChange

**作用**：接收文件变化通知，将其写入 tar 流

```go
func (cw *ChangeWriter) HandleChange(k fs.ChangeKind, p string, f os.FileInfo, err error) error
```

**参数**：
- `k`: 变化类型（Add、Modify、Delete、Unmodified）
- `p`: 文件路径（相对于根目录）
- `f`: 文件信息（大小、权限、时间戳等）
- `err`: 错误（通常是 nil）

**核心逻辑**（简化版）：

```go
func (cw *ChangeWriter) HandleChange(k fs.ChangeKind, p string, f os.FileInfo, err error) error {
    if err != nil {
        return err
    }
    
    // ========== 情况 1: 删除文件 ==========
    if k == fs.ChangeKindDelete {
        // 创建 whiteout 文件
        // 例如：删除 /etc/hosts → 创建 /etc/.wh.hosts
        whiteOutDir := filepath.Dir(p)           // /etc
        whiteOutBase := filepath.Base(p)          // hosts
        whiteOut := filepath.Join(whiteOutDir, ".wh."+whiteOutBase)  // /etc/.wh.hosts
        
        // 创建 tar header
        hdr := &tar.Header{
            Typeflag:   tar.TypeReg,     // 普通文件
            Name:       whiteOut[1:],    // 去掉开头的 /
            Size:       0,                // 空文件
            ModTime:    cw.whiteoutT,    // 统一的时间戳
            AccessTime: cw.whiteoutT,
            ChangeTime: cw.whiteoutT,
        }
        
        // 确保父目录存在
        if err := cw.includeParents(hdr); err != nil {
            return err
        }
        
        // 写入 tar header（只有 header，没有内容）
        if err := cw.tw.WriteHeader(hdr); err != nil {
            return fmt.Errorf("failed to write whiteout header: %w", err)
        }
        
        return nil
    }
    
    // ========== 情况 2: 新增或修改文件 ==========
    
    // 构造源文件路径
    source := filepath.Join(cw.source, p)  // /var/lib/.../snapshots/4/fs + /etc/hosts
    
    // 跳过 socket 文件（不能打包到 tar）
    if f.Mode()&os.ModeSocket != 0 {
        return nil
    }
    
    // 处理符号链接
    var link string
    if f.Mode()&os.ModeSymlink != 0 {
        link, err = os.Readlink(source)
        if err != nil {
            return err
        }
    }
    
    // 创建 tar header（从 FileInfo）
    hdr, err := tarheader.FileInfoHeaderNoLookups(f, link)
    if err != nil {
        return err
    }
    
    // 调整权限、时间戳等
    hdr.Mode = int64(chmodTarEntry(os.FileMode(hdr.Mode)))
    hdr.Format = tar.FormatPAX
    
    // 应用时间戳上界
    if cw.modTimeUpperBound != nil && hdr.ModTime.After(*cw.modTimeUpperBound) {
        hdr.ModTime = *cw.modTimeUpperBound
    }
    hdr.ModTime = hdr.ModTime.Truncate(time.Second)
    
    // 规范化路径名
    name := strings.TrimPrefix(p, "/")
    if f.IsDir() && !strings.HasSuffix(name, "/") {
        name += "/"  // 目录必须以 / 结尾
    }
    hdr.Name = name
    
    // ========== 处理硬链接 ==========
    inode, isHardlink := fs.GetLinkInfo(f)
    if isHardlink {
        // 如果这个 inode 已经写入过
        if sourcePath, ok := cw.inodeSrc[inode]; ok {
            // 创建硬链接指向第一次出现的路径
            hdr.Typeflag = tar.TypeLink
            hdr.Linkname = sourcePath
            hdr.Size = 0
        } else {
            // 这是这个 inode 第一次出现
            cw.inodeSrc[inode] = name
        }
    } else if k == fs.ChangeKindUnmodified {
        // 未修改的文件不写入 diff
        return nil
    }
    
    // ========== 确保父目录存在 ==========
    if err := cw.includeParents(hdr); err != nil {
        return err
    }
    
    // ========== 写入 tar header ==========
    if err := cw.tw.WriteHeader(hdr); err != nil {
        return fmt.Errorf("failed to write file header: %w", err)
    }
    
    // ========== 写入文件内容 ==========
    if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
        // 打开源文件
        file, err := os.Open(source)
        if err != nil {
            return fmt.Errorf("failed to open file: %w", err)
        }
        defer file.Close()
        
        // 复制文件内容到 tar
        if _, err := io.Copy(cw.tw, file); err != nil {
            return fmt.Errorf("failed to copy file data: %w", err)
        }
    }
    
    return nil
}
```

**HandleChange 的三种情况**：

```
情况 1: ChangeKindDelete
输入：kind=Delete, path="/etc/oldfile"
操作：
  1. 构造 whiteout 路径：/etc/.wh.oldfile
  2. 创建 tar header（空文件）
  3. 写入 tar
输出到 tar：
  etc/.wh.oldfile (空文件)

情况 2: ChangeKindAdd（新增文件）
输入：kind=Add, path="/tmp/test.txt"
操作：
  1. 读取 upperRoot/tmp/test.txt
  2. 创建 tar header（包含元数据）
  3. 写入 tar header
  4. 写入文件内容
输出到 tar：
  tmp/test.txt (包含完整内容)

情况 3: ChangeKindModify（修改文件）
输入：kind=Modify, path="/etc/hosts"
操作：
  1. 读取 upperRoot/etc/hosts（新版本）
  2. 创建 tar header
  3. 写入 tar header
  4. 写入文件内容（完整内容，不是 diff）
输出到 tar：
  etc/hosts (包含完整内容，覆盖 lower 的版本)
```

### 3.5 第五层：cw.Close

**作用**：写入 tar 结束标记，刷新缓冲区

```go
func (cw *ChangeWriter) Close() error {
    // 写入 tar 结束标记（两个 512 字节的空块）
    if err := cw.tw.Close(); err != nil {
        return fmt.Errorf("failed to close tar writer: %w", err)
    }
    return nil
}
```

---

## 四、完整的数据流

### 4.1 调用链

```
writeDiff
  ├─ mount.WithTempMount(lower, ...)
  │   └─ 挂载 lower 到 /tmp/xxx
  │      └─ callback(lowerRoot)
  │         ├─ archive.NewChangeWriter(w, upperRoot)
  │         │   └─ 创建 ChangeWriter (cw)
  │         ├─ fs.DiffDirChanges(lowerRoot, upperRoot, cw.HandleChange)
  │         │   └─ filepath.Walk(upperRoot, ...)
  │         │      ├─ 对于 upperRoot 的每个文件
  │         │      ├─ 判断变化类型（Add/Modify/Delete）
  │         │      └─ 调用 cw.HandleChange(kind, path, fileInfo)
  │         │         ├─ 如果是 Delete：写入 .wh.* 到 tar
  │         │         ├─ 如果是 Add/Modify：写入文件内容到 tar
  │         │         └─ tar.WriteHeader() + tar.Write()
  │         └─ cw.Close()
  │            └─ tar.Close()（写入 tar 结束标记）
  └─ 自动 umount /tmp/xxx
```

### 4.2 数据流图

```
输入：
  lower: [{Type: "overlay", Options: [...]}]
  upperRoot: "/var/lib/containerd/.../snapshots/4/fs"
  w: io.Writer（输出流）

步骤 1: 挂载 lower
  mount.WithTempMount
    ↓
  lowerRoot = /tmp/containerd-mount-123456
  (包含父层的完整文件系统)

步骤 2: 创建 tar writer
  archive.NewChangeWriter(w, upperRoot)
    ↓
  cw.tw = tar.NewWriter(w)

步骤 3: 遍历 upperRoot，对比 lowerRoot
  fs.DiffDirChanges
    ↓
  filepath.Walk(upperRoot)
    ├─ /etc/hosts
    │  ├─ 检查 lowerRoot/etc/hosts → 存在
    │  ├─ kind = ChangeKindModify
    │  └─ 调用 cw.HandleChange(Modify, "/etc/hosts", fileInfo)
    │     ├─ 创建 tar header: {Name: "etc/hosts", Size: 100, ...}
    │     ├─ 写入 tar header 到 w
    │     ├─ 读取 upperRoot/etc/hosts 内容
    │     └─ 写入内容到 w
    │
    ├─ /tmp/test.txt
    │  ├─ 检查 lowerRoot/tmp/test.txt → 不存在
    │  ├─ kind = ChangeKindAdd
    │  └─ 调用 cw.HandleChange(Add, "/tmp/test.txt", fileInfo)
    │     ├─ 创建 tar header: {Name: "tmp/test.txt", Size: 50, ...}
    │     ├─ 写入 tar header 到 w
    │     ├─ 读取 upperRoot/tmp/test.txt 内容
    │     └─ 写入内容到 w
    │
    └─ /.wh.oldfile
       ├─ 检查 whiteout → 是
       ├─ kind = ChangeKindDelete
       └─ 调用 cw.HandleChange(Delete, "/oldfile", fileInfo)
          ├─ 创建 tar header: {Name: ".wh.oldfile", Size: 0, ...}
          └─ 写入 tar header 到 w

步骤 4: 关闭 tar writer
  cw.Close()
    ↓
  写入 tar 结束标记到 w

步骤 5: 清理
  umount lowerRoot
  rm -rf lowerRoot

输出：
  w 包含完整的 tar 流：
    - tar header: etc/hosts
    - file data: <100 bytes>
    - tar header: tmp/test.txt
    - file data: <50 bytes>
    - tar header: .wh.oldfile
    - (no data)
    - tar end marker
```

---

## 五、实例演示

### 5.1 输入数据

```
lowerRoot (父层，挂载后):
/tmp/containerd-mount-123456/
├── bin/bash
├── etc/
│   ├── hosts      (内容: "127.0.0.1 localhost")
│   └── passwd
└── usr/bin/

upperRoot (当前层 upperdir):
/var/lib/containerd/.../snapshots/4/fs/
├── etc/
│   └── hosts      (内容: "127.0.0.1 localhost\n10.0.0.1 myhost")
├── tmp/
│   └── test.txt   (内容: "Hello World")
└── .wh.passwd     (whiteout 文件)
```

### 5.2 执行过程

```
DiffDirChanges 遍历 upperRoot:

1. 遍历到：/etc/hosts
   - 相对路径：/etc/hosts
   - 检查 lowerRoot/etc/hosts → 存在
   - 对比内容：不同
   - kind = ChangeKindModify
   - 调用：HandleChange(Modify, "/etc/hosts", fileInfo)
   
   HandleChange 处理：
   - 创建 tar header:
     {
       Name: "etc/hosts",
       Size: 50,
       Mode: 0644,
       ModTime: <time>,
     }
   - 写入 tar header
   - 读取 upperRoot/etc/hosts（新内容）
   - 写入内容到 tar

2. 遍历到：/tmp/test.txt
   - 相对路径：/tmp/test.txt
   - 检查 lowerRoot/tmp/test.txt → 不存在
   - kind = ChangeKindAdd
   - 调用：HandleChange(Add, "/tmp/test.txt", fileInfo)
   
   HandleChange 处理：
   - 创建 tar header:
     {
       Name: "tmp/test.txt",
       Size: 11,
       Mode: 0644,
       ModTime: <time>,
     }
   - 写入 tar header
   - 读取 upperRoot/tmp/test.txt
   - 写入内容到 tar

3. 遍历到：/.wh.passwd
   - 相对路径：/.wh.passwd
   - 检查 whiteout：是（文件名以 .wh. 开头）
   - overlayFSWhiteoutConvert 处理：
     - 提取原始文件名：passwd
     - 调用：HandleChange(Delete, "/passwd", fileInfo)
   
   HandleChange 处理：
   - 创建 tar header:
     {
       Name: ".wh.passwd",
       Size: 0,
       Mode: 0644,
       ModTime: <time>,
     }
   - 写入 tar header
   - （不写入内容，whiteout 是空文件）

4. 关闭 tar writer
   - 写入 tar 结束标记
```

### 5.3 输出 tar 结构

```
tar 内容：

1. tar header (512 bytes)
   Name: etc/hosts
   Size: 50
   Mode: 0644
   ...

2. file data (50 bytes, 填充到 512 的倍数)
   127.0.0.1 localhost
   10.0.0.1 myhost

3. tar header (512 bytes)
   Name: tmp/test.txt
   Size: 11
   Mode: 0644
   ...

4. file data (11 bytes, 填充到 512 的倍数)
   Hello World

5. tar header (512 bytes)
   Name: .wh.passwd
   Size: 0
   Mode: 0644
   ...

6. (no data for whiteout)

7. tar end marker (1024 bytes, 两个空块)
```

### 5.4 解压后的效果

```
当这个 tar 层应用到父层时：

父层：
/bin/bash
/etc/hosts      (旧内容)
/etc/passwd

应用当前层 tar：
+ etc/hosts     (替换为新内容)
+ tmp/test.txt  (新增)
- .wh.passwd    (删除标记 → 删除 etc/passwd)

结果：
/bin/bash       (来自父层)
/etc/hosts      (来自当前层，已修改)
/tmp/test.txt   (来自当前层，新增)
(etc/passwd 被删除)
```

---

## 六、关键设计点

### 6.1 为什么需要 ChangeWriter？

```
直接使用 tar.Writer 的问题：
1. 需要手动处理 whiteout 文件名转换
2. 需要手动确保父目录存在
3. 需要手动处理硬链接
4. 需要手动规范化路径
5. 需要手动处理时间戳

ChangeWriter 的优势：
✅ 封装了所有 tar 相关的复杂逻辑
✅ 统一的变化处理接口（HandleChange）
✅ 自动处理边界情况
✅ 可重复构建（时间戳上界）
```

### 6.2 为什么需要 DiffDirChanges？

```
直接遍历 upperRoot 的问题：
1. 无法判断文件是新增还是修改（需要对比 lowerRoot）
2. 无法发现删除的文件（只在 lowerRoot 存在）
3. 需要手动识别 whiteout 文件
4. 需要手动处理目录变化

DiffDirChanges 的优势：
✅ 封装了目录对比逻辑
✅ 自动识别 OverlayFS whiteout
✅ 准确判断变化类型
✅ 统一的回调接口
```

### 6.3 为什么修改的文件要写入完整内容？

```
OCI 镜像层的语义：

每一层都是完整的"增量"，不是 patch：
- 新增文件：完整内容
- 修改文件：完整的新内容（不是 diff）
- 删除文件：whiteout 标记

应用层时：
1. 解压 tar 到目标目录
2. 新增/修改的文件直接覆盖
3. whiteout 文件触发删除操作

优势：
✅ 简单：不需要 patch 工具
✅ 通用：适用于任何文件系统
✅ 独立：每层可以单独验证
```

---

## 七、总结

### writeDiff 的核心流程

```
1. 挂载 lower（父层）到临时目录
   → 获得 lowerRoot（可访问的父层文件系统）

2. 创建 ChangeWriter
   → 封装 tar.Writer，提供 HandleChange 接口

3. 遍历 upperRoot，对比 lowerRoot
   → 找出所有文件变化（Add/Modify/Delete）

4. 对每个变化调用 HandleChange
   → 写入 tar header + 文件内容

5. 关闭 tar writer
   → 写入结束标记

6. 清理临时目录
   → 自动 umount 和删除
```

### 关键组件的职责

```
mount.WithTempMount:
  - 挂载 lower 到临时目录
  - 执行回调
  - 自动清理

archive.NewChangeWriter:
  - 封装 tar.Writer
  - 提供 HandleChange 接口
  - 处理 whiteout、硬链接、父目录等

fs.DiffDirChanges:
  - 遍历 upperRoot
  - 对比 lowerRoot
  - 判断变化类型
  - 调用回调函数

cw.HandleChange:
  - 接收变化通知
  - 写入 tar header
  - 写入文件内容
  - 处理特殊情况
```

### 对你的 LVM Diff 实现的启发

```
你可以参考这个结构：

1. 创建 LVM 快照（类似挂载 lower）
2. 使用 thin_send 获取块级差异
3. 解析块级差异，找出文件变化
4. 创建 ChangeWriter
5. 对每个变化调用 HandleChange
6. 生成 OCI 镜像层

关键：
- ChangeWriter 是通用的，可以复用
- 你只需要实现"找出文件变化"的部分
- 其他 tar 打包逻辑都可以复用
```

---

**文档版本**: 1.0.0  
**最后更新**: 2026-01-23  
**相关文档**: 
- [containerd Diff 机制详解](./containerd-diff-mechanism.md)
- [Thin Send/Recv 实现文档](./thin-send-testing.md)

