//go:build linux

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

// Package mysimple 提供一个简单的 snapshotter 实现
// 这是一个教学示例，展示如何从零构建一个完整的 snapshotter
package mysimple

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/overlay"
	"github.com/containerd/log"
)

// 自定义 label key - 用于添加额外的 lower 层
const (
	labelCustomLowerPaths = "containerd.io/snapshot/mysimple.custom-lowers"
)

// Opt 定义 snapshotter 的配置选项
type Opt func(*config) error

// config 内部配置结构
type config struct {
	// 传递给底层 overlay snapshotter 的选项
	overlayOpts []overlay.Opt
}

// WithUpperdirLabel 添加 upperdir label 支持
func WithUpperdirLabel(c *config) error {
	c.overlayOpts = append(c.overlayOpts, overlay.WithUpperdirLabel)
	return nil
}

// AsynchronousRemove 启用异步删除
func AsynchronousRemove(c *config) error {
	c.overlayOpts = append(c.overlayOpts, overlay.AsynchronousRemove)
	return nil
}

// snapshotter 是我们的自定义实现
// 它包装了 overlay snapshotter 并添加自定义功能
type snapshotter struct {
	snapshots.Snapshotter // 嵌入底层的 overlay snapshotter
	root                  string
}

// NewSnapshotter 创建一个新的 mysimple snapshotter
// 这是插件系统会调用的工厂函数
func NewSnapshotter(root string, opts ...Opt) (snapshots.Snapshotter, error) {
	// 1. 处理配置选项
	var config config
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return nil, err
		}
	}

	// 2. 创建底层的 overlay snapshotter
	baseSnapshotter, err := overlay.NewSnapshotter(root, config.overlayOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create overlay snapshotter: %w", err)
	}

	// 3. 创建并返回我们的包装器
	return &snapshotter{
		Snapshotter: baseSnapshotter,
		root:        root,
	}, nil
}

// ============================================================================
// 实现 snapshots.Snapshotter 接口
// 大部分方法直接透传给底层的 overlay snapshotter
// 只重写需要自定义的方法
// ============================================================================

// Prepare 创建一个可写的活动快照
// 这是容器启动时调用的主要方法
func (s *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).WithField("key", key).WithField("parent", parent).Debug("mysimple: Prepare called")

	// 1. 调用底层实现
	mounts, err := s.Snapshotter.Prepare(ctx, key, parent, opts...)
	if err != nil {
		return nil, err
	}

	// 2. 应用我们的自定义逻辑
	return s.customizeMounts(ctx, key, mounts)
}

// View 创建一个只读的快照视图
func (s *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	log.G(ctx).WithField("key", key).WithField("parent", parent).Debug("mysimple: View called")

	mounts, err := s.Snapshotter.View(ctx, key, parent, opts...)
	if err != nil {
		return nil, err
	}

	return s.customizeMounts(ctx, key, mounts)
}

// Mounts 返回已存在快照的挂载信息
func (s *snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	log.G(ctx).WithField("key", key).Debug("mysimple: Mounts called")

	mounts, err := s.Snapshotter.Mounts(ctx, key)
	if err != nil {
		return nil, err
	}

	return s.customizeMounts(ctx, key, mounts)
}

// ============================================================================
// 自定义逻辑实现
// ============================================================================

// customizeMounts 是核心的自定义逻辑
// 根据 label 添加额外的 lower 层到 overlayfs
func (s *snapshotter) customizeMounts(ctx context.Context, key string, mounts []mount.Mount) ([]mount.Mount, error) {
	// 1. 只处理 overlay 类型的挂载
	if len(mounts) != 1 || mounts[0].Type != "overlay" {
		log.G(ctx).WithField("key", key).Debug("mysimple: not overlay mount, skip customization")
		return mounts, nil
	}

	// 2. 获取快照信息（包含 labels）
	info, err := s.Snapshotter.Stat(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to stat snapshot %s: %w", key, err)
	}

	// 3. 检查是否配置了自定义 lower 路径
	customLowers, ok := info.Labels[labelCustomLowerPaths]
	if !ok || customLowers == "" {
		log.G(ctx).WithField("key", key).Debug("mysimple: no custom lowers configured")
		return mounts, nil
	}

	// 4. 解析并验证路径
	lowerPaths := strings.Split(customLowers, ":")
	validPaths := []string{}
	for _, p := range lowerPaths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		// 确保目录存在
		if err := os.MkdirAll(p, 0755); err != nil {
			log.G(ctx).WithError(err).Warnf("mysimple: failed to create lower path %s", p)
			continue
		}

		validPaths = append(validPaths, p)
		log.G(ctx).WithField("path", p).Info("mysimple: added custom lower path")
	}

	if len(validPaths) == 0 {
		return mounts, nil
	}

	// 5. 修改 overlayfs 的 lowerdir 选项
	for i, option := range mounts[0].Options {
		if strings.HasPrefix(option, "lowerdir=") {
			// 提取原始的 lowerdir
			originalLower := strings.TrimPrefix(option, "lowerdir=")

			// 将自定义路径添加到前面（优先级最高）
			newLower := strings.Join(validPaths, ":") + ":" + originalLower

			// 替换选项
			mounts[0].Options[i] = "lowerdir=" + newLower

			log.G(ctx).WithField("key", key).
				WithField("original", originalLower).
				WithField("new", newLower).
				Info("mysimple: customized lowerdir")

			break
		}
	}

	return mounts, nil
}

// ============================================================================
// 其他接口方法说明
// ============================================================================

// 以下方法都直接继承自嵌入的 overlay snapshotter：
// - Stat(ctx, key) - 获取快照信息
// - Update(ctx, info, fieldpaths) - 更新快照元数据
// - Usage(ctx, key) - 获取快照使用的磁盘空间
// - Commit(ctx, name, key, opts) - 提交活动快照为只读快照
// - Remove(ctx, key) - 删除快照
// - Walk(ctx, fn, filters) - 遍历所有快照
// - Close() - 关闭 snapshotter

// 📝 教学说明：
//
// 1. 这个实现采用了"包装器"模式（类似装饰器）
//    - 优点：代码简单，复用 overlay 的成熟实现
//    - 缺点：无法深度定制存储后端（如 LVM）
//
// 2. 如果需要完全控制（像 devbox），需要：
//    - 自己管理元数据存储（BoltDB）
//    - 实现所有接口方法
//    - 管理 snapshot 目录结构
//    - 处理父子关系
//
// 3. 关键点：
//    - Prepare/View/Mounts 必须返回 []mount.Mount
//    - mount.Mount 包含 overlayfs 的挂载参数
//    - Labels 必须以 "containerd.io/snapshot/" 开头才会被继承
//
// 4. 调用流程：
//    containerd CRI → Prepare(key, parent, opts) → 底层 overlay → customizeMounts → 返回 mounts
