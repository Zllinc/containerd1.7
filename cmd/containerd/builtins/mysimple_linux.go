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

package builtins

// 📝 教学说明：
//
// 这个文件的作用是将 mysimple snapshotter 的插件注册代码导入到 containerd
//
// 工作原理：
// 1. containerd 的 main 函数会导入 builtins 包
// 2. Go 在导入包时会执行所有 import 的包的 init() 函数
// 3. mysimple/plugin 包的 init() 函数会调用 plugin.Register()
// 4. 从而将 mysimple snapshotter 注册到 containerd

import (
	// 导入插件包，触发 init() 函数执行
	// 注意：必须使用下划线 _ 导入，因为我们不直接使用这个包的任何符号
	// 我们只是需要它的副作用（side effect）：执行 init() 函数
	_ "github.com/containerd/containerd/snapshots/mysimple/plugin"
)

// ============================================================================
// 📝 深入理解
// ============================================================================
//
// 1. 为什么需要这个文件？
//    - containerd 使用插件系统，所有插件必须在编译时链接进去
//    - 如果不导入插件包，插件的 init() 就不会执行
//    - init() 不执行 = plugin.Register() 不会被调用 = 插件不存在
//
// 2. 文件命名规则：
//    - _linux.go: 只在 Linux 平台编译
//    - _windows.go: 只在 Windows 平台编译
//    - 因为 mysimple 只支持 Linux，所以用 _linux.go
//
// 3. 查看 containerd 包含了哪些插件：
//    $ ls cmd/containerd/builtins/
//      builtins.go              # 导入所有通用插件
//      builtins_linux.go        # 导入 Linux 特定插件
//      overlay_linux.go         # overlay snapshotter
//      devbox_linux.go          # devbox snapshotter
//      mysimple_linux.go        # 我们的新插件！
//
// 4. 执行流程：
//    cmd/containerd/main.go
//      ↓ import "...builtins"
//    cmd/containerd/builtins/builtins_linux.go
//      ↓ import _ "...mysimple/plugin"
//    snapshots/mysimple/plugin/plugin.go
//      ↓ init() { plugin.Register(...) }
//    containerd 启动完成，mysimple 可用
//
// 5. 验证插件是否注册：
//    $ containerd --version
//    $ ctr plugins ls | grep mysimple
//      应该能看到: io.containerd.snapshotter.v1.mysimple
