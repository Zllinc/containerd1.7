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

// Package overlay 提供 mysimple snapshotter 的插件注册
// 注意：包名必须是 overlay，这是 containerd 插件系统的要求
package overlay

import (
	"errors"

	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/snapshots/mysimple"
	"github.com/containerd/platforms"
)

// Config 定义插件的配置结构
// 这些配置项会从 containerd 的 config.toml 中读取
type Config struct {
	// RootPath 是 snapshotter 的存储根目录
	// 对应配置: [plugins."io.containerd.snapshotter.v1.mysimple"]
	//           root_path = "/custom/path"
	RootPath string `toml:"root_path"`

	// UpperdirLabel 是否添加 upperdir label
	// 对应配置: upperdir_label = true
	UpperdirLabel bool `toml:"upperdir_label"`

	// SyncRemove 是否同步删除（false = 异步删除）
	// 对应配置: sync_remove = false
	SyncRemove bool `toml:"sync_remove"`

	// MountOptions 额外的挂载选项
	// 对应配置: mount_options = ["nodev", "nosuid"]
	MountOptions []string `toml:"mount_options"`
}

// init 函数在包被导入时自动执行
// 这是插件注册的关键入口点
func init() {
	// 📝 教学重点：这是插件注册的核心！
	plugin.Register(&plugin.Registration{
		// Type 必须是 plugin.SnapshotPlugin
		Type: plugin.SnapshotPlugin,

		// ID 是插件的唯一标识符
		// - 配置文件中使用: [plugins."io.containerd.snapshotter.v1.mysimple"]
		// - 命令行使用: --snapshotter mysimple
		ID: "mysimple",

		// Config 指定配置结构体
		// containerd 会自动从 config.toml 解析配置到这个结构体
		Config: &Config{},

		// InitFn 是插件的初始化函数
		// containerd 启动时会调用这个函数来创建 snapshotter 实例
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			// 1. 设置支持的平台（默认是当前平台）
			ic.Meta.Platforms = append(ic.Meta.Platforms, platforms.DefaultSpec())

			// 2. 解析配置
			config, ok := ic.Config.(*Config)
			if !ok {
				return nil, errors.New("invalid mysimple snapshotter configuration")
			}

			// 3. 确定存储根目录
			// 优先使用配置文件中的 root_path，否则使用默认路径
			root := ic.Root // 默认: /var/lib/containerd/io.containerd.snapshotter.v1.mysimple
			if config.RootPath != "" {
				root = config.RootPath
			}

			// 4. 构建 snapshotter 选项
			var opts []mysimple.Opt

			// 根据配置添加选项
			if config.UpperdirLabel {
				opts = append(opts, mysimple.WithUpperdirLabel)
			}

			if !config.SyncRemove {
				// 如果 sync_remove=false，则启用异步删除（性能更好）
				opts = append(opts, mysimple.AsynchronousRemove)
			}

			// TODO: 如果需要支持 mount_options，可以添加：
			// if len(config.MountOptions) > 0 {
			//     opts = append(opts, mysimple.WithMountOptions(config.MountOptions))
			// }

			// 5. 导出元数据（可选，用于调试和监控）
			// 其他插件可以通过 ic.Meta.Exports 获取这个信息
			ic.Meta.Exports[plugin.SnapshotterRootDir] = root

			// 6. 创建并返回 snapshotter 实例
			// ⚠️ 重要：必须返回 snapshots.Snapshotter 接口的实现
			return mysimple.NewSnapshotter(root, opts...)
		},
	})
}

// ============================================================================
// 📝 教学说明
// ============================================================================
//
// 1. 插件注册流程：
//    a. containerd 启动
//    b. 导入所有内置插件包（通过 builtins）
//    c. 执行所有 init() 函数
//    d. plugin.Register() 注册插件到全局注册表
//    e. containerd 根据配置文件初始化需要的插件
//    f. 调用 InitFn 创建实例
//
// 2. 配置文件示例 (/etc/containerd/config.toml)：
//    [plugins."io.containerd.snapshotter.v1.mysimple"]
//      root_path = "/var/lib/mysimple"
//      upperdir_label = true
//      sync_remove = false
//
// 3. 如何让 containerd 使用这个 snapshotter：
//    a. 作为默认 snapshotter：
//       [plugins."io.containerd.grpc.v1.cri".containerd]
//         snapshotter = "mysimple"
//
//    b. 作为特定 runtime 的 snapshotter：
//       [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.myruntime]
//         snapshotter = "mysimple"
//
//    c. 通过命令行指定：
//       ctr run --snapshotter mysimple ...
//
// 4. 关键概念：
//    - plugin.InitContext: 提供插件初始化时的上下文信息
//      - ic.Root: 默认存储路径
//      - ic.Config: 解析后的配置对象
//      - ic.Meta: 插件元数据
//
//    - plugin.Registration: 插件注册信息
//      - Type: 插件类型（Snapshotter/Content/Runtime等）
//      - ID: 唯一标识符
//      - InitFn: 工厂函数
//
// 5. 与 devbox 的对比：
//    - mysimple: 简单包装，配置项少
//    - devbox: 复杂配置（LVM VG名称、thin pool等）
//
//    两者的注册流程是完全一样的！
