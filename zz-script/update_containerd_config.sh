#!/bin/bash

# containerd配置更新脚本
# 用于添加devbox相关配置到containerd配置文件

set -e

# update containerd config
CONFIG_FILE="/etc/containerd/config.toml"
BACKUP_FILE="/etc/containerd/config.toml.backup.$(date +%Y%m%d_%H%M%S)"

echo "=== containerd配置更新脚本 ==="

# check if running with root permission
if [[ $EUID -ne 0 ]]; then
   echo "错误: 此脚本需要root权限运行"
   exit 1
fi

# backup original config file
if [[ -f "$CONFIG_FILE" ]]; then
    echo "备份原配置文件到: $BACKUP_FILE"
    cp "$CONFIG_FILE" "$BACKUP_FILE"
else
    echo "警告: 配置文件 $CONFIG_FILE 不存在，请创建新文件"
fi

# 创建新的配置文件
echo "更新containerd配置文件..."

cat > "$CONFIG_FILE" << 'EOF'
version = 2
root = "/var/lib/containerd"
state = "/run/containerd"
oom_score = 0

[grpc]
  address = "/run/containerd/containerd.sock"
  uid = 0
  gid = 0
  max_recv_message_size = 16777216
  max_send_message_size = 16777216

[debug]
  address = "/run/containerd/containerd-debug.sock"
  uid = 0
  gid = 0
  level = "warn"

[timeouts]
  "io.containerd.timeout.shim.cleanup" = "5s"
  "io.containerd.timeout.shim.load" = "5s"
  "io.containerd.timeout.shim.shutdown" = "3s"
  "io.containerd.timeout.task.state" = "2s"

[plugins]
  # 增加 containerd diff配置
  [plugins."io.containerd.service.v1.diff-service"]
    default = ["overlayfs-diff"]
  # 增加结束
  
  [plugins."io.containerd.grpc.v1.cri"]
    sandbox_image = "sealos.hub:5000/pause:3.9"
    max_container_log_line_size = 16384
    max_concurrent_downloads = 20
    disable_apparmor = false
    
    [plugins."io.containerd.grpc.v1.cri".containerd]
      snapshotter = "overlayfs"
      default_runtime_name = "runc"
      
      [plugins."io.containerd.grpc.v1.cri".containerd.runtimes]
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
          runtime_type = "io.containerd.runc.v2"
          [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
            SystemdCgroup = true
            
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.crun]
          runtime_type = "io.containerd.runc.v2"
          [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.crun.options]
            BinaryName = "/usr/bin/crun"
            SystemdCgroup = true
            
        # 增加runtime配置
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.devbox-runc]
          runtime_type = "io.containerd.runc.v2"
          snapshotter = "devbox"
          [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.devbox-runc.options]
            SystemdCgroup = true
        # 增加结束
        
    [plugins."io.containerd.grpc.v1.cri".registry]
      config_path = "/etc/containerd/certs.d"
      
      [plugins."io.containerd.grpc.v1.cri".registry.configs]
        [plugins."io.containerd.grpc.v1.cri".registry.configs."sealos.hub:5000".auth]
          username = "admin"
          password = "passw0rd"
          
  # 增加devbox启动配置
  [plugins."io.containerd.snapshotter.v1.devbox"]
    root_path = "/var/lib/containerd/io.containerd.snapshotter.v1.devbox"
    upperdir_label = true
    sync_remove = true
    lvm_vg_name = "devbox-vg"
    thin_pool_name = "devbox-vg-thinpool"
  # 增加结束
EOF

echo "配置文件更新完成!"

# 验证配置文件语法
echo "验证配置文件语法..."
if command -v containerd >/dev/null 2>&1; then
    if containerd config dump >/dev/null 2>&1; then
        echo "✓ 配置文件语法验证通过"
    else
        echo "✗ 配置文件语法验证失败"
        echo "恢复备份文件..."
        if [[ -f "$BACKUP_FILE" ]]; then
            cp "$BACKUP_FILE" "$CONFIG_FILE"
            echo "已恢复原配置文件"
        fi
        exit 1
    fi
else
    echo "警告: 未找到containerd命令，跳过语法验证"
fi

# replace containerd
echo "替换containerd..."
systemctl stop containerd
mv /usr/bin/containerd /usr/bin/containerd.old

wget -O /usr/bin/containerd 'http://sealos-io.oss-cn-hangzhou.aliyuncs.com/cloud%2Fdevbox%2Fv1alpha2%2Fcontainerd?Expires=2957993625&OSSAccessKeyId=LTAI5tQPN7zEdUaPXbsneT5g&Signature=wj9%2FDTuZRaGaERKngwHPfSWKAaA%3D'
chmod +x /usr/bin/containerd

# start containerd
systemctl start containerd
