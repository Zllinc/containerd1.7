# 磁盘 I/O 控制完整指南：从原理到实践

## 目录

1. [磁盘 I/O 基础知识](#一磁盘-io基础)
2. [为什么需要限制 Devbox 的 I/O？](#二为什么需要限制)
3. [Cgroup v1: BFQ 调度器与 IO Weight](#三cgroup-v1-bfq)
4. [Cgroup v2: IO 控制器](#四cgroup-v2)
5. [解决方案：Containerd 集成](#五解决方案)
6. [实际效果验证](#六实际效果)
7. [常见问题解答](#七常见问题)

---

## 一、磁盘 I/O 基础知识

### 1.1 什么是磁盘 I/O？

**磁盘 I/O（Input/Output）**：
- 从磁盘读取数据到内存（Input）
- 从内存写入数据到磁盘（Output）

**示例**：
```bash
# 读取文件到内存
cat /path/to/file

# 写入内存数据到磁盘
echo "data" > /path/to/file
```

---

### 1.2 为什么需要限制 I/O？

#### 问题：磁盘性能瓶颈

**磁盘性能的限制**：
- **机械硬盘（HDD）**：寻道时间约 5-10ms
- **固态硬盘（SSD）**：无寻道，但是带宽有限（通常 1-5GB/s）
- **NVMe SSD**：更高带宽（5-30GB/s），但仍然有限制

**影响**：
- I/O 操作会占用磁盘带宽
- 高 I/O 可能导致其他进程无法访问磁盘
- 可能导致 Pleget 卡顿（容器无法启动）

---

### 1.3 I/O 性能指标

#### 关键指标

| 指标 | 说明 | 示例值 |
|------|------|--------|
| **IOPS** | 每秒能执行的 I/O 操作次数 | 10k - 100k |
| **Bandwidth** | 数据传输速率（MB/s） | 100 - 500 |
| **Latency** | 完成 I/O 操作所需时间（延迟） | 0.1 - 10ms |
| **Queue Depth** | 队列深度，表示有多少 I/O 请求在等待 | 1 - 32 |

#### 指标高低参考值

**IOPS（每秒 I/O 操作数）**：
- **机械硬盘（HDD）**：
  - 低：50-100 IOPS（随机读写）
  - 中：100-200 IOPS
  - 高：200+ IOPS
- **SATA SSD**：
  - 低：5k-10k IOPS
  - 中：10k-50k IOPS
  - 高：50k-100k IOPS
- **NVMe SSD**：
  - 低：50k-100k IOPS
  - 中：100k-500k IOPS
  - 高：500k-1M+ IOPS

**Bandwidth（带宽，MB/s）**：
- **机械硬盘（HDD）**：
  - 低：50-100 MB/s
  - 中：100-150 MB/s
  - 高：150-200 MB/s
- **SATA SSD**：
  - 低：200-400 MB/s
  - 中：400-550 MB/s
  - 高：550-600 MB/s
- **NVMe SSD**：
  - 低：1-3 GB/s
  - 中：3-5 GB/s
  - 高：5-7 GB/s（消费级）或 10-30 GB/s（企业级）

**Latency（延迟，毫秒）**：
- **优秀**：< 1ms（NVMe SSD 典型值）
- **良好**：1-5ms（SATA SSD 典型值）
- **一般**：5-10ms（HDD 典型值）
- **较差**：> 10ms（可能存在问题或高负载）

**Queue Depth（队列深度）**：
- **低负载**：1-4（I/O 请求少，系统空闲）
- **正常负载**：4-16（正常业务场景）
- **高负载**：16-32（I/O 密集型任务）
- **过载**：> 32（可能成为瓶颈，需要优化）

**计算带宽**：
```
实际带宽 = (IOPS × 每次操作的数据量)
         = 5000 IOPS × 4KB = 20MB/s
```

**实际场景参考**：
- **开发环境编译**：通常需要 1k-5k IOPS，带宽 50-200 MB/s
- **数据库查询**：需要低延迟（< 5ms），IOPS 取决于查询复杂度
- **文件传输**：主要看带宽，通常需要 100-500 MB/s
- **容器启动**：需要低延迟（< 10ms），否则会感觉卡顿

---

## 二、为什么需要限制 Devbox 的 I/O？

### 2.1 问题：Devbox 是开发环境，但共享资源

**Devbox 的特性**：
- 用于开发环境的容器
- 运行编译、构建等 I/O 密集型任务
- 与其他业务容器共享磁盘资源
- 可能产生大量 I/O，导致硬盘打满

**问题场景**：
```
容器 A (业务容器)
  ├─ 正常启动
  └─ 磁盘 I/O 被占用
      ↓
      容器 B (Devbox 容器)
        ├─ 运行 cargo build
        ├─ 产生大量 I/O
        ├─ 占用大量磁盘带宽
        └─ 导致容器 A 无法启动（Pleget 卡顿）
```

---

### 2.2 解决方案：通过 Cgroup 限制 I/O 资源

**原理**：
- 将容器分到不同的 cgroup
- 为每个 cgroup 配置 IO 权重
- 权重越高，分配到 I/O 资源越多

**效果**：
- 限制 Devbox 的 I/O 优先级
- 保证关键业务容器能够正常启动
- 避免硬盘被占满

---

## 三、Cgroup v1: BFQ 调度器与 IO Weight

### 3.1 什么是 Cgroup？

**Cgroup (Control Group)** 是 Linux 内核提供的一种机制，用于：
- **限制**（Limiting）：限制进程组可以使用的资源上限
- **优先级**（Prioritization）：为不同进程组分配不同的资源优先级
- **记录**（Accounting）：记录进程组的资源使用情况
- **隔离**（Isolation）：将进程组彼此隔离

**支持控制的资源类型**：
- CPU（cpu, cpuacct）
- 内存（memory）
- 磁盘 I/O（blkio）
- 网络带宽（net_cls, net_prio）

**Cgroup v1 架构**：
```
/proc/cgroups
  ├─ cpu          (CPU 资源控制)
  ├─ cpuacct      (CPU 资源统计)
  ├─ memory       (内存资源控制)
  ├─ blkio        (块设备 I/O 控制)  ← 磁盘 I/O 使用这个
  ├─ net_cls      (网络分类)
  └─ net_prio     (网络优先级)
```

每个控制器都有自己独立的层级树，这是 v1 的特点。

---

### 3.2 什么是 BFQ？

**BFQ (Budget Fair Queueing Scheduler)**：
- 一种 **IO 调度算法**，位于块设备层
- 工作在内核的 I/O 路径上，介于文件系统和磁盘驱动之间
- 为每个 cgroup 分配"预算"（budget，即 I/O 时间片）
- 预算耗尽后，该 cgroup 进入等待状态，让其他 cgroup 使用磁盘

**IO 调度器的位置**：
```
应用层 (Application)
  ↓
文件系统 (ext4, xfs)
  ↓
IO 调度器 (BFQ/CFQ/Deadline/NOOP)  ← BFQ 在这里
  ↓
设备驱动 (Device Driver)
  ↓
物理磁盘 (HDD/SSD)
```

**BFQ vs 其他调度器**：

| 调度器 | 特点 | 适用场景 | 支持 IO Weight |
|--------|------|----------|----------------|
| **BFQ** | 按预算公平分配，支持权重 | 桌面、虚拟化、多租户 | ✅ 是 |
| **CFQ** | 按时间片轮询，部分支持权重 | 通用场景 | ⚠️ 有限 |
| **Deadline** | 读写请求有截止时间 | 数据库、低延迟要求 | ❌ 否 |
| **NOOP** | 简单 FIFO 队列 | SSD、闪存 | ❌ 否 |

**BFQ 的关键特性**：
- **支持权重（Weight）**：权重越高，分配到的 I/O 时间片（预算）越大
- **公平性保证**：即使某个 cgroup 不产生 I/O，其他 cgroup 也不会饿死
- **低延迟优化**：对交互式应用（如数据库）更友好
- **按设备独立配置**：不同的磁盘设备可以配置不同的 BFQ 参数

---

### 3.3 IO Weight 的含义与计算

#### 权重分配原理

**公式**：
```
某个 cgroup 的 I/O 比例 = 该 cgroup 的权重 / 所有权重之和
```

**示例 1：两个容器**
```
容器 A：Weight = 1000
容器 B：Weight = 100

总权重 = 1000 + 100 = 1100

容器 A 获得：1000 / 1100 ≈ 90.9% 的 I/O 资源
容器 B 获得：100 / 1100 ≈ 9.1% 的 I/O 资源
```

**示例 2：三个容器**
```
容器 A (业务)：Weight = 1000
容器 B (Devbox)：Weight = 100
容器 C (后台)：Weight = 10

总权重 = 1000 + 100 + 10 = 1110

容器 A 获得：1000 / 1110 ≈ 90.1%
容器 B 获得：100 / 1110 ≈ 9.0%
容器 C 获得：10 / 1110 ≈ 0.9%
```

#### 权重范围与建议值

**权重范围**：1-1000（BFQ 的标准范围）

**建议配置**：

| 容器类型 | 权重值 | 说明 |
|----------|--------|------|
| 关键业务容器 | 800-1000 | 高优先级，需要快速响应 |
| 普通业务容器 | 100-500 | 正常优先级 |
| Devbox 开发容器 | 10-100 | 低优先级，避免影响其他容器 |
| 后台批处理 | 1-10 | 最低优先级 |

**注意事项**：
- ⚠️ 权重是相对值，不是绝对值
- ⚠️ 权重越低，I/O 优先级越低（但不是完全不分配）
- ⚠️ 当某个 cgroup 没有I/O 请求时，其他 cgroup 可以使用全部带宽

---

### 3.4 Cgroup v1 的 blkio 控制器

#### blkio 控制器的功能

**blkio** 是 Cgroup v1 中用于控制块设备 I/O 的控制器，提供多种限制方式：

| 参数 | 含义 | 单位 |
|------|------|------|
| `blkio.weight` | 设备无关的权重 | 1-1000 |
| `blkio.weight_device` | 按设备设置权重 | 1-1000 |
| `blkio.throttle.read_bps_device` | 按设备限制读带宽 | bytes/s |
| `blkio.throttle.write_bps_device` | 按设备限制写带宽 | bytes/s |
| `blkio.throttle.read_iops_device` | 按设备限制读 IOPS | operations/s |
| `blkio.throttle.write_iops_device` | 按设备限制写 IOPS | operations/s |

#### 两种控制方式

**方式 1：权重控制（需要 BFQ）**
```bash
# 为所有设备设置权重
echo 100 > /sys/fs/cgroup/blkio/kubepods/pod123/blkio.weight

# 为特定设备设置权重 (格式: major:minor weight)
echo "8:0 100" > /sys/fs/cgroup/blkio/kubepods/pod123/blkio.weight_device
# 8:0 是 /dev/sda 的设备号
```

**方式 2：绝对限制（Throttle，不需要 BFQ）**
```bash
# 限制 /dev/sda (8:0) 的读带宽为 50MB/s
echo "8:0 52428800" > /sys/fs/cgroup/blkio/kubepods/pod123/blkio.throttle.read_bps_device

# 限制 /dev/sda (8:0) 的写 IOPS 为 1000
echo "8:0 1000" > /sys/fs/cgroup/blkio/kubepods/pod123/blkio.throttle.write_iops_device
```

⚠️ **重要区别**：
- **权重控制**：比例分配，所有 cgroup 按权重共享磁盘
- **绝对限制**：硬上限，达到限制后 I/O 被阻塞

#### 查看 Cgroup 配置

```bash
# 查看设备的 major:minor 号
ls -l /dev/sda
# 输出: brw-rw---- 1 root disk 8, 0 Jan 1 00:00 /dev/sda
#                     主设备号=8, 次设备号=0

# 查看某个 cgroup 的 blkio 配置
cat /sys/fs/cgroup/blkio/kubepods/pod123/blkio.weight
cat /sys/fs/cgroup/blkio/kubepods/pod123/blkio.weight_device

# 查看统计信息
cat /sys/fs/cgroup/blkio/kubepods/pod123/blkio.io_serviced        # I/O 次数
cat /sys/fs/cgroup/blkio/kubepods/pod123/blkio.io_service_bytes    # I/O 字节数
```

---

### 3.5 如何启用 BFQ？

#### 步骤 1：检查是否支持 BFQ

```bash
# 检查当前调度器
cat /sys/block/sda/queue/scheduler
# 如果输出包含 "bfq"，说明已启用
```

#### 步骤 2：启用 BFQ 调度器

```bash
# 方法 1：临时启用
modprobe bfq

# 方法 2：永久启用
echo "bfq" >> /etc/modules-load.d/bfq.conf

# 检查
lsmod | grep bfq
```

---

### 3.4 配置设备参数

```bash
# 查看设备参数
cat /sys/block/sda/queue/iosched/max_sectors_kb
cat /sys/block/sda/queue/iosched/read_ahead_kb

# 修改设备参数
echo "2048" > /sys/block/sda/queue/iosched/max_sectors_kb
echo "256" > /sys/block/sda/queue/iosched/read_ahead_kb
```

---

## 四、Cgroup v2: IO 控制器

### 4.1 Cgroup v2 架构

**Cgroup v2 的改进**：
- **统一层级**：所有控制器共享同一个层级树
- **子树控制**：更灵活的控制器启用/禁用机制
- **更清晰的接口**：简化了配置文件结构
- **更好的性能**：减少了内核开销

**Cgroup v1 vs v2 对比**：

| 特性 | Cgroup v1 | Cgroup v2 |
|------|-----------|-----------|
| 层级结构 | 每个控制器独立树 | 统一的单层级树 |
| IO 控制器名称 | `blkio` | `io` |
| 权重控制 | 需要 BFQ 调度器 | 内置支持，不依赖调度器 |
| 绝对限制 | throttle.* 文件 | max.* 文件 |
| 设备配置 | `blkio.throttle.*_device` | `io.max` |

**Cgroup v2 目录结构**：
```
/sys/fs/cgroup/
  ├─ cgroup.controllers        # 可用的控制器列表
  ├─ cgroup.subtree_control    # 启用的控制器
  ├─ io.max                    # IO 限制配置
  ├─ io.weight                 # IO 权重配置
  ├─ io.stat                   # IO 统计信息
  └─ kubepods/
      └─ besteffort/
          └─ pod123/
              └─ container1/
                  ├─ io.max    # 容器的 IO 限制
                  └─ io.stat   # 容器的 IO 统计
```

---

### 4.2 IO 控制器的两种模式

Cgroup v2 的 IO 控制器支持两种模式：

#### 模式 1：权重模式 (io.weight)

**适用场景**：按比例分配 I/O 资源，类似 BFQ

```bash
# 设置权重（1-10000，v2 范围比 v1 更大）
echo "100" > /sys/fs/cgroup/kubepods/pod123/io.weight
```

**特点**：
- ✅ 不需要特定的 IO 调度器
- ✅ 所有调度器都支持
- ⚠️ 只能按比例分配，不能硬限制

#### 模式 2：限制模式 (io.max)

**适用场景**：硬限制带宽或 IOPS

```bash
# 格式: "major:minor rbps=X wbps=Y risops=Z wiops=W"
echo "8:0 rbps=52428800 wbps=20971520" > /sys/fs/cgroup/kubepods/pod123/io.max
# 8:0 = /dev/sda
# rbps=52428800 = 读限制 50MB/s
# wbps=20971520 = 写限制 20MB/s
```

**参数说明**：

| 参数 | 含义 | 单位 | 示例 |
|------|------|------|------|
| `rbps` | 读带宽限制 | bytes/s | `52428800` = 50MB/s |
| `wbps` | 写带宽限制 | bytes/s | `20971520` = 20MB/s |
| `riops` | 读 IOPS 限制 | operations/s | `1000` |
| `wiops` | 写 IOPS 限制 | operations/s | `500` |

**组合配置**：
```bash
# 同时限制带宽和 IOPS（取更严格的限制）
echo "8:0 rbps=52428800 wbps=20971520 riops=1000 wiops=500" > io.max
```

---

### 4.3 io.cost 模型（E2E 带宽控制）

**io.cost** 是 Cgroup v2 提供的高级功能，可以实现端到端的带宽控制。

#### 启用 io.cost

```bash
# 步骤 1：启用 io 控制器
echo "+io" > /sys/fs/cgroup/cgroup.subtree_control

# 步骤 2：为设备启用 io.cost 模型
echo "8:0 enable=1" > /sys/fs/cgroup/io.cost.qos

# 步骤 3：设置 QoS 参数
echo "8:0 enable=1 ctrl=auto rpct=95.00 rlat=6600 wpct=95.00 wlat=13500" > \
  /sys/fs/cgroup/io.cost.qos
```

#### io.cost 参数详解

**参数含义**：

| 参数 | 含义 | 示例值 | 说明 |
|------|------|--------|------|
| `enable` | 是否启用 | `1` | 1=启用，0=禁用 |
| `ctrl` | 控制模式 | `auto` | auto=自动，user=手动 |
| `rpct` | 读性能目标 | `95.00` | 95% 的请求要在延迟内完成 |
| `rlat` | 读延迟目标 | `6600` | 6600 微秒（6.6ms） |
| `wpct` | 写性能目标 | `95.00` | 95% 的请求要在延迟内完成 |
| `wlat` | 写延迟目标 | `13500` | 13500 微秒（13.5ms） |

**工作原理**：
```
当 I/O 请求到达时：
  ├─ 请求延迟 < rlat → 正常处理
  ├─ 请求延迟 > rlat → 开始节流
  └─ 达到 rpct 目标 → 动态调整带宽分配
```

#### 配置权重

```bash
# 为 cgroup 设置权重（影响 io.cost 分配）
echo "8:0 weight=100" > /sys/fs/cgroup/kubepods/pod123/io.cost.weight
```

---

### 4.4 实际配置示例

#### 场景 1：Devbox 容器限制（使用 io.max）

```bash
# 1. 找到容器的 cgroup 路径
crictl inspect <container-id> | grep cgroup
# 输出: "cgroups": { "path": "kubepods/besteffort/pod123/container1" }

# 2. 设置 I/O 限制
echo "8:0 rbps=52428800 wbps=20971520 riops=1000 wiops=500" > \
  /sys/fs/cgroup/kubepods/besteffort/pod123/container1/io.max
```

#### 场景 2：多容器按比例分配（使用 io.weight）

```bash
# 业务容器（高优先级）
echo "8:0 weight=1000" > /sys/fs/cgroup/kubepods/podA/io.weight

# Devbox 容器（低优先级）
echo "8:0 weight=100" > /sys/fs/cgroup/kubepods/podB/io.weight

# 结果：业务容器获得约 91% 的 I/O，Devbox 获得约 9%
```

#### 场景 3：验证配置是否生效

```bash
# 1. 查看 io.max 配置
cat /sys/fs/cgroup/kubepods/pod123/container1/io.max
# 输出: 8:0 rbps=52428800 wbps=20971520 riops=1000 wiops=500

# 2. 查看 io.stat 统计（实时监控）
cat /sys/fs/cgroup/kubepods/pod123/container1/io.stat
# 输出示例:
# 8:0 rbytes=123456789 wbytes=987654321 rios=12345 wios=54321
#      读字节数       写字节数        读次数    写次数

# 3. 使用 iotop 监控
iotop -p $(pidof containerd-shim)
```

---

### 4.5 Cgroup v2 与 udev 规则集成

**通过 udev 自动配置 IO 限制**：

```bash
# /etc/udev/rules.d/99-io-cost.rules
# 当检测到特定设备时，自动启用 io.cost

# 为 /dev/vdc 启用 io.cost
ACTION=="add|change", KERNEL=="vdc", \
  RUN+="/bin/sh -c 'echo \"253:0 enable=1 ctrl=auto\" > /sys/fs/cgroup/io.cost.qos'"

# 为 /dev/sda 设置性能目标
ACTION=="add|change", KERNEL=="sda", \
  RUN+="/bin/sh -c 'echo \"8:0 enable=1 ctrl=auto rpct=95.00 rlat=6600 wpct=95.00 wlat=13500\" > /sys/fs/cgroup/io.cost.qos'"
```

**重新加载 udev 规则**：
```bash
udevadm control --reload-rules
udevadm trigger --type=devices --action=change
```

---

### 4.6 常见问题

#### Q1: 如何判断系统使用的是 Cgroup v1 还是 v2？

```bash
# 检查挂载类型
mount | grep cgroup

# Cgroup v1 输出:
# tmpfs on /sys/fs/cgroup type tmpfs (...)
# cgroup on /sys/fs/cgroup/blkio type cgroup (...)
# cgroup on /sys/fs/cgroup/memory type cgroup (...)

# Cgroup v2 输出:
# cgroup2 on /sys/fs/cgroup type cgroup2 (...)

# 或者检查文件
ls -l /sys/fs/cgroup/
# 如果看到 cgroup.controllers 文件，说明是 v2
```

#### Q2: Cgroup v2 不支持 BFQ 吗？

**答**：Cgroup v2 的 `io` 控制器**不依赖 BFQ**，可以直接使用 `io.weight` 和 `io.max`。
- v2 的权重功能内置在 io 控制器中
- 可以与任何 IO 调度器配合使用（BFQ、mq-deadline、none）

#### Q3: io.max 和 io.weight 可以同时使用吗？

**答**：**可以**，但它们是独立的：
- `io.max`：硬限制，达到上限后 I/O 被阻塞
- `io.weight`：软限制，按比例分配剩余带宽

**推荐做法**：
- 使用 `io.max` 设置绝对上限（如 50MB/s）
- 使用 `io.weight` 设置相对优先级（如 100）

---

## 五、解决方案：Containerd 集成

### 5.1 配置 BlockIO Class

#### 步骤 1：创建 BlockIO 配置文件

**文件位置**：`/etc/containerd/blockio.yaml`

**配置内容**：
```yaml
Classes:
  # 定义一个名为 "Devbox" 的 BlockIO class
  Devbox:
    # Weight 定义默认权重
    - Weight: 10
      # 限制带宽
      ThrottleReadBps: 50M      # 最多 50MB/s 读取
      ThrottleWriteBps: 20M     # 最多 20MB/s 写入
      # 限制 IOPS
      ThrottleReadIOPS: 1000    # 最多 1000 次/s 读取
      ThrottleWriteIOPS: 500    # 最多 500 次/s 写入
```

**参数说明**：

这些限制参数是**同时生效的独立限制**，系统会取**更严格的限制**作为实际限制：

| 参数 | 含义 | 示例值说明 |
|------|------|-----------|
| `ThrottleReadBps` | 读取带宽上限 | 50MB/s = 每秒最多读取 50MB 数据 |
| `ThrottleWriteBps` | 写入带宽上限 | 20MB/s = 每秒最多写入 20MB 数据 |
| `ThrottleReadIOPS` | 读取 IOPS 上限 | 1000 = 每秒最多执行 1000 次读取操作 |
| `ThrottleWriteIOPS` | 写入 IOPS 上限 | 500 = 每秒最多执行 500 次写入操作 |

**工作原理**：

带宽和 IOPS 限制**同时生效**，取更严格的限制：

```
实际限制 = min(带宽限制, IOPS限制 × 每次操作的数据量)
```

**示例计算**：

假设读取操作：
- 带宽限制：50MB/s
- IOPS 限制：1000 次/s

**场景 1：小文件读取（每次 4KB）**
```
IOPS 限制的带宽 = 1000 × 4KB = 4MB/s
实际限制 = min(50MB/s, 4MB/s) = 4MB/s  ← IOPS 限制更严格
```

**场景 2：大文件读取（每次 100KB）**
```
IOPS 限制的带宽 = 1000 × 100KB = 100MB/s
实际限制 = min(50MB/s, 100MB/s) = 50MB/s  ← 带宽限制更严格
```

**场景 3：中等文件读取（每次 50KB）**
```
IOPS 限制的带宽 = 1000 × 50KB = 50MB/s
实际限制 = min(50MB/s, 50MB/s) = 50MB/s  ← 两者相等
```

**总结**：
- ✅ **不是相乘关系**：不是 50M × 1000 = 50GB/s
- ✅ **是独立限制**：带宽和 IOPS 各自有上限
- ✅ **取更严格限制**：哪个限制更严格，就用哪个
- ✅ **读写独立**：读取和写入的限制是分开计算的

#### 步骤 2：配置 Containerd

**文件位置**：`/etc/containerd/config.toml`

**配置内容**：
```toml
[plugins."io.containerd.service.v1.tasks-service"]
  # 指定 blockio 配置文件
  blockio_config_file = "/etc/containerd/blockio.yaml"
```

---

### 5.2 Pod 配置

#### YAML 配置

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: devbox-low-priority-pod
  annotations:
    # 指定使用 Devbox class
    blockio.resources.beta.kubernetes.io/pod: "Devbox"
spec:
  containers:
  - name: my-container
    image: ubuntu
    command: ["stress-ng", "--cpu", "--runtime", "--stress-cpu", "60", "--cpu-load", "99"]
```

---

## 六、实际效果验证

### 6.1 测试结果对比

#### 普通 Pod（无限制）

```
IOPS: 18.5k
Bandwidth: 72.4MB/s
Latency (usec): min=2, max=7149, avg=9.39
```

#### 低优先级 Pod（限制后）

```
IOPS: 19.1k
Bandwidth: 74.8MB/s
Latency (usec): min=3, max=8698, avg=170.69
```

**分析**：
- 性能略低于普通 Pod
- 但不明显，因为当前没有其他容器竞争 I/O
- 需要真实的多容器压力测试

---

### 6.2 验证配置是否生效

#### 检查 BFQ 是否启用

```bash
lsmod | grep bfq
```

#### 检查 IO Weight

```bash
# 查看容器 cgroup
crictl inspect <pod-id> | grep cgroup

# 检查 IO Weight
cat /sys/fs/cgroup/.../io.bfq.weight
```

#### 检查 Pod 配置

```bash
crictl inspect devbox-pod | grep -i annotation
# 应该看到：blockio.resources.beta.kubernetes.io/pod: "Devbox"
```

---

## 七、常见问题解答

### 7.1 为什么需要启用 BFQ？

**原因**：
- 默认的 CFQ 调度器不支持 IO Weight
- BFQ 才支持按比例分配 IO 资源

**验证 BFQ 是否启用**：
```bash
cat /sys/block/sda/queue/scheduler
```

---

### 7.2 权重越低越好吗？

**不是！** 权重越低，I/O 优先级越低：
- Weight=10：低优先级，只占用 1% 的 I/O 资源
- Weight=100：正常优先级，占用 50% 的 I/O 资源
- Weight=1000：高优先级，占用 83% 的 I/O 资源

**建议**：
- Devbox 容器使用 Weight=10
- 普通容器使用 Weight=100
- 关键业务容器使用 Weight=1000

---

### 7.3 如何验证 LV 是否被删除？

```bash
# 检查 LV 是否还存在
lvs | grep devbox

# 检查挂载点
mount | grep devbox
```

---

## 八、关键要点总结

### 1. 核心原理

**Cgroup IO 限制链路**：
```
Pod Annotation
  ↓
Containerd CRI 插件
  ↓
Containerd Tasks Service
  ↓
Runc
  ↓
Cgroup
  ↓
BFQ 调度器
  ↓
磁盘驱动
```

### 2. 配置流程

```
1. 编写 blockio.yaml 配置
2. 配置 containerd.config.toml
3. 重启 containerd
4. 编写 Pod YAML（添加 annotation）
5. 创建 Pod
6. 验证效果
```

### 3. 监控方法

```bash
# 检查容器状态
crictl inspect <pod-id>

# 检查 cgroup
cat /sys/fs/cgroup/.../io.bfq.weight

# 检查 LV 是否残留
lvs | grep devbox
```

---

## 九、扩展阅读

- Linux Cgroup 文档：`man 7 cgroups` / `man 7 cgroup-v1` / `man 7 cgroup-v2`
- Kernel 文档：`man 7 cgroups` /`man blockio` /`man io.cost.qos`
- FIO 工具文档：`man fio`
- Containerd 文档：`man containerd-config`

---

这个文档是否详细讲解了磁盘 I/O 控制？需要我补充具体的代码示例吗？
