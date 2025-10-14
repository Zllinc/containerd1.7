#!/bin/bash

# 部署devbox RuntimeClass脚本
# 用于在Kubernetes中配置devbox-runc运行时

set -e

echo "=== 部署devbox RuntimeClass ==="

# 检查是否以root权限运行
if [[ $EUID -ne 0 ]]; then
   echo "警告: 建议使用root权限运行此脚本"
fi

# 检查kubectl是否安装
if ! command -v kubectl &> /dev/null; then
    echo "错误: kubectl未安装，请先安装kubectl"
    echo "安装方法: https://kubernetes.io/docs/tasks/tools/"
    exit 1
fi

# 检查kubectl是否能连接到集群
if ! kubectl cluster-info &> /dev/null; then
    echo "错误: 无法连接到Kubernetes集群"
    echo "请检查kubeconfig配置: kubectl config view"
    exit 1
fi

echo "✓ kubectl已安装且可连接到集群"

# 创建RuntimeClass YAML文件
RUNTIMECLASS_FILE="/tmp/devbox-runtimeclass.yaml"

cat > "$RUNTIMECLASS_FILE" << 'EOF'
# RuntimeClass 定义于 node.k8s.io API 组
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  # 用来引用 RuntimeClass 的名字
  # RuntimeClass 是一个集群层面的资源
  name: devbox-runtime
# 对应的 CRI 配置的名称
handler: devbox-runc
EOF

echo "✓ 已创建RuntimeClass配置文件: $RUNTIMECLASS_FILE"
echo ""
echo "配置内容："
cat "$RUNTIMECLASS_FILE"
echo ""

# 检查RuntimeClass是否已存在
if kubectl get runtimeclass devbox-runtime &> /dev/null; then
    echo "警告: RuntimeClass 'devbox-runtime' 已存在"
    read -p "是否要删除并重新创建？(y/n): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        echo "删除现有RuntimeClass..."
        kubectl delete runtimeclass devbox-runtime
        echo "✓ 已删除"
    else
        echo "跳过创建，使用现有RuntimeClass"
        exit 0
    fi
fi

# 应用RuntimeClass配置
echo "应用RuntimeClass配置..."
kubectl apply -f "$RUNTIMECLASS_FILE"

# 验证创建结果
echo ""
echo "验证RuntimeClass创建结果..."
if kubectl get runtimeclass devbox-runtime &> /dev/null; then
    echo "✓ RuntimeClass 'devbox-runtime' 创建成功！"
    echo ""
    echo "详细信息："
    kubectl get runtimeclass devbox-runtime -o yaml
else
    echo "✗ RuntimeClass创建失败"
    exit 1
fi

echo ""
echo "=== 部署完成 ==="
