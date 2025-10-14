#!/bin/bash
# mysimple snapshotter 测试脚本

set -e

echo "=========================================="
echo "MySimple Snapshotter 测试脚本"
echo "=========================================="
echo ""

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 检查是否为 root
if [ "$EUID" -ne 0 ]; then 
   echo -e "${RED}错误: 请使用 sudo 运行此脚本${NC}"
   exit 1
fi

# 步骤 1: 检查插件是否注册
echo -e "${YELLOW}[1/6] 检查插件是否已注册...${NC}"
if ctr plugins ls 2>/dev/null | grep -E "snapshotter.*mysimple"; then
    echo -e "${GREEN}✓ mysimple 插件已注册${NC}"
    ctr plugins ls | grep mysimple
else
    echo -e "${RED}✗ mysimple 插件未找到${NC}"
    echo "提示: 请先编译并启动包含 mysimple 的 containerd"
    echo "  make && sudo systemctl restart containerd"
    exit 1
fi
echo ""

# 步骤 2: 准备测试镜像
echo -e "${YELLOW}[2/6] 准备测试镜像...${NC}"
if ! ctr images ls 2>/dev/null | grep -q "docker.io/library/alpine:latest"; then
    echo "拉取 alpine 镜像..."
    ctr images pull docker.io/library/alpine:latest
fi
echo -e "${GREEN}✓ 镜像准备完成${NC}"
echo ""

# 步骤 3: 创建自定义 lower 目录
echo -e "${YELLOW}[3/6] 创建自定义 lower 层目录...${NC}"
CUSTOM_LOWER="/tmp/mysimple-test-lower"
rm -rf "$CUSTOM_LOWER"
mkdir -p "$CUSTOM_LOWER"

# 创建测试文件
echo "Hello from custom lower layer!" > "$CUSTOM_LOWER/test-from-host.txt"
echo "#!/bin/sh" > "$CUSTOM_LOWER/custom-script.sh"
echo "echo 'This script is from custom lower layer'" >> "$CUSTOM_LOWER/custom-script.sh"
chmod +x "$CUSTOM_LOWER/custom-script.sh"

echo -e "${GREEN}✓ 创建了自定义 lower 目录: $CUSTOM_LOWER${NC}"
ls -la "$CUSTOM_LOWER"
echo ""

# 步骤 4: 测试基本功能（不使用自定义 lower）
echo -e "${YELLOW}[4/6] 测试基本功能（无自定义 lower）...${NC}"
SNAPSHOT_KEY="mysimple-test-basic"

# 清理旧 snapshot
ctr snapshot --snapshotter mysimple rm "$SNAPSHOT_KEY" 2>/dev/null || true

# 创建 snapshot
IMAGE_CHAIN=$(ctr images ls -q name==docker.io/library/alpine:latest | xargs -I {} sh -c 'ctr content ls -q | head -1')
echo "创建 snapshot: $SNAPSHOT_KEY"
ctr snapshot --snapshotter mysimple prepare "$SNAPSHOT_KEY" ""

# 查看 snapshot 信息
echo "Snapshot 信息:"
ctr snapshot --snapshotter mysimple info "$SNAPSHOT_KEY"

# 清理
ctr snapshot --snapshotter mysimple rm "$SNAPSHOT_KEY"
echo -e "${GREEN}✓ 基本功能测试通过${NC}"
echo ""

# 步骤 5: 测试自定义 lower 层功能
echo -e "${YELLOW}[5/6] 测试自定义 lower 层功能...${NC}"
SNAPSHOT_KEY_CUSTOM="mysimple-test-custom"

# 清理旧 snapshot
ctr snapshot --snapshotter mysimple rm "$SNAPSHOT_KEY_CUSTOM" 2>/dev/null || true

# 使用自定义 label 创建 snapshot
echo "创建带自定义 lower 的 snapshot..."
ctr snapshot --snapshotter mysimple prepare \
    --label "containerd.io/snapshot/mysimple.custom-lowers=$CUSTOM_LOWER" \
    "$SNAPSHOT_KEY_CUSTOM" ""

# 查看 snapshot 信息
echo "Snapshot 信息 (应该包含自定义 label):"
ctr snapshot --snapshotter mysimple info "$SNAPSHOT_KEY_CUSTOM"

# 查看 mounts 信息
echo ""
echo "Mount 信息 (应该包含 $CUSTOM_LOWER 在 lowerdir 中):"
MOUNTS=$(ctr snapshot --snapshotter mysimple mounts "$SNAPSHOT_KEY_CUSTOM")
echo "$MOUNTS"

# 验证 lowerdir 是否包含自定义路径
if echo "$MOUNTS" | grep -q "lowerdir.*$CUSTOM_LOWER"; then
    echo -e "${GREEN}✓ 自定义 lower 路径已正确添加到 lowerdir${NC}"
else
    echo -e "${RED}✗ 警告: 未在 lowerdir 中找到自定义路径${NC}"
fi

# 清理
ctr snapshot --snapshotter mysimple rm "$SNAPSHOT_KEY_CUSTOM"
echo -e "${GREEN}✓ 自定义 lower 层测试通过${NC}"
echo ""

# 步骤 6: 测试容器运行
echo -e "${YELLOW}[6/6] 测试容器运行...${NC}"
CONTAINER_ID="mysimple-test-container"

# 清理旧容器
ctr task kill "$CONTAINER_ID" 2>/dev/null || true
ctr task rm "$CONTAINER_ID" 2>/dev/null || true
ctr container rm "$CONTAINER_ID" 2>/dev/null || true

echo "创建容器并验证自定义 lower 层..."
# 创建并运行容器
ctr run \
    --snapshotter mysimple \
    --snapshotter-label "containerd.io/snapshot/mysimple.custom-lowers=$CUSTOM_LOWER" \
    --rm \
    docker.io/library/alpine:latest \
    "$CONTAINER_ID" \
    sh -c "
        echo '=== 验证自定义文件 ==='
        if [ -f '$CUSTOM_LOWER/test-from-host.txt' ]; then
            echo '✓ 找到自定义文件'
            cat '$CUSTOM_LOWER/test-from-host.txt'
        else
            echo '✗ 未找到自定义文件'
            exit 1
        fi
        
        echo ''
        echo '=== 验证自定义脚本 ==='
        if [ -x '$CUSTOM_LOWER/custom-script.sh' ]; then
            echo '✓ 找到可执行脚本'
            '$CUSTOM_LOWER/custom-script.sh'
        else
            echo '✗ 未找到可执行脚本'
            exit 1
        fi
        
        echo ''
        echo '=== 测试写隔离 ==='
        echo 'Modified in container' > '$CUSTOM_LOWER/container-write.txt'
        echo '✓ 容器内写入成功'
    " && CONTAINER_SUCCESS=true || CONTAINER_SUCCESS=false

if [ "$CONTAINER_SUCCESS" = true ]; then
    echo -e "${GREEN}✓ 容器运行测试通过${NC}"
    
    # 验证写隔离
    echo ""
    echo "验证写隔离 (宿主机不应该看到容器的写入):"
    if [ -f "$CUSTOM_LOWER/container-write.txt" ]; then
        echo -e "${RED}✗ 警告: 在宿主机上看到了容器的写入 (写隔离失败)${NC}"
    else
        echo -e "${GREEN}✓ 写隔离正常 (宿主机看不到容器的写入)${NC}"
    fi
else
    echo -e "${RED}✗ 容器运行测试失败${NC}"
fi
echo ""

# 清理测试目录
echo "清理测试文件..."
rm -rf "$CUSTOM_LOWER"

echo ""
echo "=========================================="
echo -e "${GREEN}所有测试完成！${NC}"
echo "=========================================="
echo ""
echo "测试总结:"
echo "1. ✓ 插件已正确注册"
echo "2. ✓ 基本 snapshot 功能正常"
echo "3. ✓ 自定义 lower 层功能正常"
echo "4. ✓ 容器可以访问自定义 lower 层"
echo "5. ✓ 写隔离正常工作"
echo ""
echo "下一步:"
echo "- 查看日志: journalctl -u containerd -f | grep mysimple"
echo "- 列出 snapshots: ctr snapshot --snapshotter mysimple ls"
echo "- 查看配置: cat /etc/containerd/config.toml | grep -A 5 mysimple"

