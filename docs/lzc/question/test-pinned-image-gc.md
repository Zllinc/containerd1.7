# 测试 Pinned 镜像是否会被 Kubelet GC 删除

## 一、调整 Kubelet Image GC 阈值

### 1.1 配置方式

Kubelet 的 Image GC 配置可以通过以下方式设置：

#### 方式 1：Kubelet 配置文件（推荐）

编辑 `/var/lib/kubelet/config.yaml`（或 kubelet 的配置文件路径）：

```yaml
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
imageGCHighThresholdPercent: 70  # 默认 85，降低以便更容易触发 GC
imageGCLowThresholdPercent: 60    # 默认 80，降低以便更容易触发 GC
imageMinimumGCAge: 2m             # 默认 2m，镜像最小存活时间
imageGCPeriod: 5m                 # 默认 5m，GC 检查周期
```

**关键参数说明**：
- `imageGCHighThresholdPercent`：磁盘使用率超过此值时触发清理（默认 85%）
- `imageGCLowThresholdPercent`：清理目标，将使用率降到此值以下（默认 80%）
- `imageMinimumGCAge`：镜像存活最小时间，新拉取的镜像不会被立即删除（默认 2 分钟）
- `imageGCPeriod`：GC 执行周期（默认 5 分钟）

#### 方式 2：Kubelet 启动参数

如果使用 systemd 管理 kubelet，编辑 `/etc/systemd/system/kubelet.service.d/10-kubeadm.conf` 或相应配置文件：

```ini
[Service]
ExecStart=/usr/bin/kubelet \
  --image-gc-high-threshold=70 \
  --image-gc-low-threshold=60 \
  --minimum-image-ttl-duration=2m \
  ...
```

#### 方式 3：Kubeadm 集群（通过 kubelet-config ConfigMap）

```bash
# 1. 获取当前配置
kubectl get configmap kubelet-config -n kube-system -o yaml > kubelet-config.yaml

# 2. 编辑配置
# 在 data.kubelet 中添加或修改：
# imageGCHighThresholdPercent: 70
# imageGCLowThresholdPercent: 60

# 3. 应用配置
kubectl apply -f kubelet-config.yaml

# 4. 重启 kubelet（每个节点）
systemctl restart kubelet
```

### 1.2 验证配置是否生效

```bash
# 查看 kubelet 当前配置
ps aux | grep kubelet | grep -E "image-gc|imageGC"

# 或者查看 kubelet 日志
journalctl -u kubelet -f | grep -i "image.*gc"
```

---

## 二、测试方案

### 2.1 测试环境准备

#### 前置条件

1. **准备一个 devbox 镜像**（确保会被 pin）
   ```bash
   # 镜像示例：ghcr.io/labring-actions/devbox/go-1.23.0:13aacd8
   ```

2. **确认镜像已被 pin**
   ```bash
   # 方法 1：查看 label
   ctr -n k8s.io images ls | grep devbox
   # 应该看到 LABELS 列包含 io.cri-containerd.pinned
   
   # 方法 2：通过 CRI API 查看
   crictl --runtime-endpoint unix:///run/containerd/containerd.sock inspecti <镜像引用> | jq '.image.pinned'
   # 应该输出 true
   ```

3. **准备一个非 pinned 镜像作为对照组**
   ```bash
   # 拉取一个普通镜像（不使用 devbox snapshotter）
   kubectl run test-pod --image=nginx:latest --rm -it --restart=Never
   ```

### 2.2 测试步骤

#### 步骤 1：创建使用 devbox 镜像的 Pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: devbox-test-pod
spec:
  runtimeClassName: devbox  # 使用 devbox runtime class
  containers:
  - name: devbox-container
    image: ghcr.io/labring-actions/devbox/go-1.23.0:13aacd8
    command: ["sleep", "3600"]
```

```bash
kubectl apply -f devbox-test-pod.yaml
```

#### 步骤 2：记录镜像信息（GC 前）

```bash
# 记录镜像 ID 和 pinned 状态
IMAGE_REF="ghcr.io/labring-actions/devbox/go-1.23.0:13aacd8"
IMAGE_ID=$(crictl --runtime-endpoint unix:///run/containerd/containerd.sock images | grep "$IMAGE_REF" | awk '{print $3}')

echo "镜像引用: $IMAGE_REF"
echo "镜像 ID: $IMAGE_ID"
echo "Pinned 状态: $(crictl --runtime-endpoint unix:///run/containerd/containerd.sock inspecti $IMAGE_REF | jq -r '.image.pinned')"

# 记录镜像是否存在
crictl --runtime-endpoint unix:///run/containerd/containerd.sock images | grep "$IMAGE_REF" > /tmp/image-before-gc.txt
```

#### 步骤 3：触发磁盘压力（模拟 GC 条件）

**方法 A：降低 GC 阈值（推荐，更可控）**

```bash
# 编辑 kubelet 配置，将阈值降低到当前磁盘使用率以下
# 例如：当前使用率 50%，设置 HighThreshold=45, LowThreshold=40
# 这样会立即触发 GC
```

**方法 B：填充磁盘空间**

```bash
# 在节点上创建大文件，提高磁盘使用率
# 注意：需要确保不会影响系统运行
dd if=/dev/zero of=/tmp/fill-disk bs=1G count=50  # 根据实际情况调整

# 监控磁盘使用率
df -h /var/lib/containerd
watch -n 1 'df -h /var/lib/containerd | tail -1'
```

**方法 C：使用 Eviction Manager 触发**

```bash
# 设置 eviction 阈值（在 kubelet 配置中）
# evictionHard:
#   imagefs.available: "10%"  # 当 imagefs 可用空间 < 10% 时触发
```

#### 步骤 4：等待 GC 执行

```bash
# 监控 kubelet 日志，观察 GC 执行
journalctl -u kubelet -f | grep -i "image.*gc\|garbage.*collect"

# 或者等待一个 GC 周期（默认 5 分钟）
# 如果降低了阈值，GC 应该很快触发
```

#### 步骤 5：验证镜像是否仍然存在（GC 后）

```bash
# 检查镜像是否仍然存在
crictl --runtime-endpoint unix:///run/containerd/containerd.sock images | grep "$IMAGE_REF" > /tmp/image-after-gc.txt

# 对比前后
diff /tmp/image-before-gc.txt /tmp/image-after-gc.txt

# 如果镜像仍然存在，说明 pinned 保护生效 ✅
# 如果镜像被删除，说明 pinned 保护失效 ❌
```

#### 步骤 6：执行 Commit 操作（验证 content blob 是否完整）

```bash
# 在 devbox 容器内执行 commit
# 如果基础镜像的 content blob 被删除，commit 会失败并报错：
# "content digest not found" 或类似错误

# 如果 commit 成功，说明 content blob 完整 ✅
```

---

## 三、完整测试脚本

```bash
#!/bin/bash
set -e

IMAGE_REF="ghcr.io/labring-actions/devbox/go-1.23.0:13aacd8"
RUNTIME_ENDPOINT="unix:///run/containerd/containerd.sock"
LOG_FILE="/tmp/pinned-image-gc-test.log"

log() {
    echo "[$(date +'%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG_FILE"
}

log "=== 开始测试 Pinned 镜像 GC ==="

# 1. 检查镜像是否存在
log "步骤 1: 检查镜像是否存在"
if ! crictl --runtime-endpoint "$RUNTIME_ENDPOINT" images | grep -q "$IMAGE_REF"; then
    log "错误: 镜像不存在，请先拉取镜像"
    exit 1
fi

# 2. 检查 pinned 状态
log "步骤 2: 检查镜像 pinned 状态"
PINNED=$(crictl --runtime-endpoint "$RUNTIME_ENDPOINT" inspecti "$IMAGE_REF" | jq -r '.image.pinned // false')
if [ "$PINNED" != "true" ]; then
    log "警告: 镜像未被 pin，测试可能不准确"
fi
log "镜像 pinned 状态: $PINNED"

# 3. 记录镜像信息
log "步骤 3: 记录 GC 前的镜像信息"
IMAGE_ID=$(crictl --runtime-endpoint "$RUNTIME_ENDPOINT" images | grep "$IMAGE_REF" | awk '{print $3}')
log "镜像 ID: $IMAGE_ID"
crictl --runtime-endpoint "$RUNTIME_ENDPOINT" images | grep "$IMAGE_REF" > /tmp/image-before-gc.txt

# 4. 检查磁盘使用率
log "步骤 4: 检查磁盘使用率"
DISK_USAGE=$(df -h /var/lib/containerd | tail -1 | awk '{print $5}' | sed 's/%//')
log "当前磁盘使用率: ${DISK_USAGE}%"

# 5. 触发 GC（通过降低阈值或填充磁盘）
log "步骤 5: 触发 GC"
log "提示: 请手动触发 GC（降低 kubelet GC 阈值或填充磁盘）"
log "等待 GC 执行..."
sleep 60  # 等待 GC 执行

# 6. 检查镜像是否仍然存在
log "步骤 6: 检查 GC 后的镜像状态"
if crictl --runtime-endpoint "$RUNTIME_ENDPOINT" images | grep -q "$IMAGE_REF"; then
    log "✅ 测试通过: 镜像仍然存在，pinned 保护生效"
    crictl --runtime-endpoint "$RUNTIME_ENDPOINT" images | grep "$IMAGE_REF" > /tmp/image-after-gc.txt
else
    log "❌ 测试失败: 镜像被删除，pinned 保护失效"
    exit 1
fi

# 7. 对比前后镜像信息
log "步骤 7: 对比镜像信息"
if diff -q /tmp/image-before-gc.txt /tmp/image-after-gc.txt > /dev/null; then
    log "✅ 镜像信息未变化"
else
    log "⚠️  镜像信息有变化（可能是标签更新）"
    diff /tmp/image-before-gc.txt /tmp/image-after-gc.txt || true
fi

log "=== 测试完成 ==="
log "日志文件: $LOG_FILE"
```

---

## 四、验证要点

### 4.1 成功标准

1. **镜像仍然存在**：GC 后，pinned 镜像仍然在镜像列表中
2. **Content blob 完整**：可以成功执行 commit 操作
3. **非 pinned 镜像被删除**：对照组镜像被 GC 删除（证明 GC 确实执行了）

### 4.2 失败情况

如果测试失败，可能的原因：

1. **镜像未被正确 pin**
   - 检查 `io.cri-containerd.pinned` label 是否存在
   - 检查 CRI API 返回的 `pinned` 字段是否为 `true`

2. **GC 未执行**
   - 检查磁盘使用率是否达到阈值
   - 检查 kubelet 日志是否有 GC 相关错误

3. **Kubelet 版本问题**
   - 确认 kubelet 版本支持 pinned 镜像功能
   - 检查 `imagesInEvictionOrder` 是否正确跳过 pinned 镜像

---

## 五、快速测试命令

```bash
# 一键测试脚本
IMAGE_REF="ghcr.io/labring-actions/devbox/go-1.23.0:13aacd8"

# 1. 检查 pinned 状态
echo "=== 检查 Pinned 状态 ==="
crictl --runtime-endpoint unix:///run/containerd/containerd.sock inspecti "$IMAGE_REF" | jq '.image.pinned'

# 2. 记录镜像
echo "=== GC 前镜像列表 ==="
crictl --runtime-endpoint unix:///run/containerd/containerd.sock images | grep "$IMAGE_REF"

# 3. 触发 GC（手动降低阈值或填充磁盘）

# 4. 等待 5 分钟（或 GC 周期）

# 5. 再次检查
echo "=== GC 后镜像列表 ==="
crictl --runtime-endpoint unix:///run/containerd/containerd.sock images | grep "$IMAGE_REF"

# 6. 对比结果
# 如果镜像仍然存在 → pinned 保护生效 ✅
# 如果镜像被删除 → pinned 保护失效 ❌
```

---

## 六、监控和调试

### 6.1 监控 Kubelet GC 日志

```bash
# 实时监控 GC 日志
journalctl -u kubelet -f | grep -E "image.*gc|garbage.*collect|Image.*GC"

# 查看历史 GC 记录
journalctl -u kubelet --since "1 hour ago" | grep -i "image.*gc"
```

### 6.2 检查 GC 执行情况

```bash
# 查看 kubelet 指标（如果启用了 metrics）
curl http://localhost:10255/metrics | grep -i "image.*gc"

# 或通过 Prometheus 查询
# image_gc_duration_seconds
# image_gc_total
```

### 6.3 调试技巧

1. **降低 GC 周期**：将 `imageGCPeriod` 设置为 `1m`，加快测试速度
2. **降低阈值**：将 `imageGCHighThresholdPercent` 设置为当前使用率以下，立即触发
3. **查看详细日志**：设置 kubelet 日志级别为 `--v=4` 或更高

---

## 七、注意事项

1. **测试环境**：建议在测试环境进行，避免影响生产环境
2. **磁盘空间**：填充磁盘时注意不要填满，避免系统崩溃
3. **GC 周期**：默认 5 分钟，测试时可能需要等待
4. **容器状态**：确保测试 Pod 正在运行，否则镜像可能被判定为"未使用"
5. **多节点**：如果集群有多个节点，需要在每个节点上测试

---

## 八、预期结果

### 成功场景

```
GC 前：
- Pinned 镜像存在 ✅
- 非 pinned 镜像存在 ✅

GC 执行：
- Kubelet 日志显示 GC 执行
- 非 pinned 镜像被删除 ✅
- Pinned 镜像仍然存在 ✅

GC 后：
- Pinned 镜像仍然存在 ✅
- Commit 操作成功 ✅
- Content blob 完整 ✅
```

### 失败场景

```
GC 前：
- Pinned 镜像存在 ✅

GC 执行：
- Kubelet 日志显示 GC 执行
- Pinned 镜像被删除 ❌

GC 后：
- Pinned 镜像不存在 ❌
- Commit 操作失败（content digest not found）❌
```

---

## 九、相关文档

- [Kubelet Pin 镜像机制详解](./kubelet-pin-image.md)
- [Containerd Pin 镜像机制详解](./containerd-pin-image.md)
- [Image GC 触发机制与 Content Blob 丢失分析](../features/image-gc.md)

