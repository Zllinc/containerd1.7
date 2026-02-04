# Golangci-lint 错误说明和修复方法

## 一、错误说明

### 错误 1: `var contentId should be contentID`

**错误位置**: `snapshots/devbox/devbox.go:704`

**错误原因**:
- Go 命名规范要求：**缩写词应该全大写**
- `Id` 是 "Identifier" 的缩写，应该写成 `ID`，而不是 `Id`
- 类似的还有：`URL`, `HTTP`, `API`, `JSON` 等

**错误代码**:
```go
contentId, idOk := base.Labels[devboxContentIDKey]  // ❌ 错误
```

**正确代码**:
```go
contentID, idOk := base.Labels[devboxContentIDKey]  // ✅ 正确
```

### 错误 2: `don't use underscores in Go names; var parent_upperdir should be parentUpperdir`

**错误位置**: `snapshots/devbox/devbox.go:789`

**错误原因**:
- Go 命名规范要求：**变量名使用驼峰命名（camelCase），不使用下划线**
- `parent_upperdir` 应该改为 `parentUpperdir`

**错误代码**:
```go
parent_upperdir := o.upperPath(parentID)  // ❌ 错误
```

**正确代码**:
```go
parentUpperdir := o.upperPath(parentID)  // ✅ 正确
```

---

## 二、如何修复

### 方法 1: 使用 golangci-lint 自动修复（部分）

```bash
# 运行 golangci-lint，尝试自动修复
golangci-lint run --fix snapshots/devbox/

# 注意：命名规范错误通常无法自动修复，需要手动修改
```

### 方法 2: 手动修复（推荐）

需要手动修改变量名，因为命名规范错误无法自动修复。

---

## 三、golangci-lint 使用指南

### 基本命令

```bash
# 1. 检查指定目录的 lint 错误
golangci-lint run snapshots/devbox/

# 2. 尝试自动修复（如果可以）
golangci-lint run --fix snapshots/devbox/

# 3. 只检查特定文件
golangci-lint run snapshots/devbox/devbox.go

# 4. 显示详细信息
golangci-lint run -v snapshots/devbox/

# 5. 只显示错误，不显示警告
golangci-lint run --issues-exit-code=1 snapshots/devbox/
```

### 常用选项

| 选项 | 说明 |
|------|------|
| `--fix` | 尝试自动修复可以修复的问题 |
| `-v` | 显示详细信息 |
| `--timeout` | 设置超时时间（默认 1 分钟） |
| `--out-format` | 输出格式（如 `colored-line-number`, `json` 等） |
| `--issues-exit-code` | 有错误时的退出码（0=总是成功，1=有错误时失败） |

### 配置文件

golangci-lint 会查找以下配置文件（按优先级）：
1. `.golangci.yml` 或 `.golangci.yaml`（项目根目录）
2. `.golangci.toml`（项目根目录）
3. `.golangci.json`（项目根目录）

### 禁用特定规则

如果某个 lint 规则不适用于你的场景，可以在代码中禁用：

```go
//nolint:revive // 解释为什么禁用
var contentId string  // 禁用 revive 规则
```

或者：

```go
//nolint:var-naming // 禁用变量命名检查
var contentId string
```

---

## 四、修复步骤

### 步骤 1: 查看所有错误

```bash
golangci-lint run snapshots/devbox/
```

### 步骤 2: 尝试自动修复

```bash
golangci-lint run --fix snapshots/devbox/
```

### 步骤 3: 手动修复无法自动修复的错误

对于命名规范错误，需要手动修改代码。

### 步骤 4: 再次检查

```bash
golangci-lint run snapshots/devbox/
```

---

## 五、常见 Go 命名规范

### 变量命名

- ✅ 使用驼峰命名：`userName`, `maxSize`, `parentUpperdir`
- ❌ 不使用下划线：`user_name`, `max_size`, `parent_upperdir`
- ✅ 缩写全大写：`userID`, `httpURL`, `apiKey`
- ❌ 缩写不大写：`userId`, `httpUrl`, `apiKey`

### 常量命名

- ✅ 全大写+下划线：`MAX_SIZE`, `DEFAULT_TIMEOUT`
- ✅ 或驼峰命名（如果是导出常量）：`MaxSize`, `DefaultTimeout`

### 函数命名

- ✅ 导出函数：`GetUserName()`（首字母大写）
- ✅ 私有函数：`getUserName()`（首字母小写）
- ✅ 使用驼峰命名

---

## 六、修复后的验证

修复后，运行以下命令验证：

```bash
# 1. 再次运行 lint 检查
golangci-lint run snapshots/devbox/

# 2. 如果还有错误，查看详细信息
golangci-lint run -v snapshots/devbox/

# 3. 确保代码可以编译
go build ./snapshots/devbox/...
```

