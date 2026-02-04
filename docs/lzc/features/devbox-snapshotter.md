架构：

About: 
containerd就是叫做容器运行时的东西，负责容器的生命周期管理，是docker内部架构的一环，后来docker公司将containerd捐给CNCF，然后CNCF就将containerd独立出来作为一个项目进行开源；其实containerd跟docker其实是一个东西，但是没有docker那么全面，毕竟只是之前docker内部架构的一环；
那么K8s通过CRI-shim技术与容器运行时（docker，containerd）进行通信，也就是K8s专门负责容器的编排工作，容器的生命周期管理其实是更底层的容器运行时在管理； 
随着市面上越来越多的容器运行时工具出现，这些工具都想通过k8s作为容器编排工具，那么K8s就提出了标准的CRI接口，只要适配了这个接口的容器运行时都可以集成到K8s中去； 

CRI-shim 
CRI-shim是什么？
CRI-shim是containerd的一个重要组件，是将CRI请求转化为containerd的API请求之后进行调用的工具；
当kubelet发送一个请求（创建pod，更新pod等），那么kubelet会将这个请求转化为CRI请求发送给containerd（容器运行时），那么containerd的CRI-shim（有点像网关）就会将这个请求转化为containerd能够理解的API调用；

工作流程
1. kubelet 通过 CRI 协议向 containerd 发送请求（如创建 Pod）。
2. CRI-shim 接收这个 CRI 请求，解析并转换为 containerd 的内部 API 调用。
3. containerd 执行相应的操作（如创建容器、拉取镜像等）。
4. CRI-shim 将结果转换回 CRI 格式，返回给 kubelet。
镜像存储：
我们在使用一个镜像前，需要做：
● 通过Dockerfile将这个镜像打包然后推送至镜像仓库中
● 需要用到时就将其从镜像仓库中拉取
● 由本地容器运行时（containerd）负责镜像的存储
● containerd将镜像转化成容器运行所需的rootfs后挂载
，而containerd所做的主要是拉取镜像，将镜像解压，为容器准备rootfs这几步；
containerd中涉及镜像存储持久化的部分主要是metadata，content，snapshot；而metedata主要是用来存储元数据的，content则是保存镜像用的；
Content：
content中的数据主要是保存在var/lib/containerd/io.containerd.content.v1.content中，这底下有两个文件夹：

数据主要是存储在blobs/sha256：

为什么这些文件都是一大堆数字呢，这是因为这些文件名是根据文件内容然后进行sha256sum进行运算之后得到的哈希值；
那么为什么要使用哈希值作为文件名进行存储呢？自己猜测有几个原因：
● 去重，只要文件内容是一样的，那么文件名肯定是一样的，就可以避免同一文件存储多份，便于复用
● 便于内容寻址，当查找文件时不需要通过指定路径+文件名路径进行查找，而是通过指定路径+哈希值文件名进行查找，可能更加方便快捷；
当在拉取镜像时，会依次拉取镜像的index，config，manifest（也称作镜像清单文件），Layers文件，文件名都是sha256值：
index文件里配置了不同操作系统使用不同的manifest文件，也就是containerd通过index文件与自身操作系统去进行匹配，匹配到使用哪个manifest文件（sha256）去拉取镜像；
当通过index文件找到当前操作系统对应的manifest文件后，就查看对应的manifest文件：
manifest文件由config和layers组成，config是指镜像的配置文件信息，标志了该文件信息放在哪；layers表示当前镜像由哪些layer组成，越底下的layer越基础，最底下为父镜像layer，这里的layer为压缩后的哈希值，跟layer文件是一样的；
然后就是config文件，config文件是通过上述manifest文件可以找到的，config文件主要是存了镜像的构建历史信息，env，cmd等信息；config文件里的rootfs表示的是组成镜像rootfs的所有layer，这里的layer并不是manifest中对应的layer（tar+gzip格式），而是镜像layer文件解压后得到的sha256；
随后是layer文件，这里的layer文件都是未解压的tar+gzip格式文件，要查看文件里的内容还得对这个文件进行解压才能查看；
Snapshot:
为什么会有snapshot？
因为content中的镜像是tar+gzip格式的，没办法直接挂载到容器进行使用，所以containerd抽象出了snapshot这个概念；
主要的工作原理是：每一层镜像都会对应一层snapshot，这个snapshot是在当前镜像解压之后并且叠加父镜像之后进行sha256得到的；子snapshot继承父snapshot的文件系统；
snapshot的三种状态：
● Committed：当容器被提交之后，容器的可写层snapshot就会被标记为committed，并且该状态是不可修改的
● Actived：也即是容器的可写层，当启动容器时，会给容器加一个可写层的snapshot，这个snapshot的状态就是Actived
● View：view状态的snapshot是父snapshot的只读视图，挂载后不可修改；
snapshot 的生命周期如下图所示：

从上图可以看到：
1. 状态为 Committed 的 snapshot A0，经过 Prepare 调用后生成了 Active 状态的 snapshot a。
2. Acitve 状态的 snapshot a 是可读写的，可以挂载到指定目录进行操作，snapshot 中的文件系统经过修改后变为 a' (并没有生成新的 snapshot a'，只是相比于初始 snapshot a 发生了变化，暂且称为 a')。
3. a' 经过 Commit 操作后，生成 Committed 状态的 snapshot A1，以 a 为名的 snapshot 则会被删除 (Remove)。A0 是 A1 的父 snapshot。
4. Committed snapshot A0，还可以经过 View 调用后生成 view 状态的 snapshot b，snapshot b 是只读的，挂载后的文件系统不可被修改。
Snapshot的存储：
containerd中的snapshot存储是由snapshotter管理的，而containerd是支持多种snapshotter插件的，containerd默认的就是overlay插件：

如图可以看到blockfile插件，devbox插件等；
进入devbox中进行查看：

可以看到snapshot的文件名并不是使用sha256来进行命名的，而是从1开始的索引进行命名；
我们通过ctr snapshot ls查看到的是snapshot的key，通过这个key可以到bolt中找到该snapshot key对应的文件名：


注意，snapshot key 中 sha256 的值并不是镜像 layer content 解压之后的 sha256，而是每一层镜像 layer content 解压后再叠加 parent snapshot 中的内容，重新计算得到的 sha256 的值。如下图所示：

这里的layer都是镜像解压之后得到的sha256的值，而镜像本身是一个tar+gzip的压缩格式；

当启动容器时，就会出现一个新的snapshot，该snapshot的状态为Actived：
# 通过 ctr 启动 redis 容器
root@zjz:~# ctr run -d  docker.io/library/redis:5.0.9 redis-demo
c8d01e7d5537962fdc455a10723b7dcc9b7c9572539b799eb2604acdf3421b17
# 查看 snapshot
root@zjz:~# ctr snapshot ls
KEY                                                                     PARENT                                                                  KIND
redis-demo                                                              sha256:33bd296ab7f37bdacff0cb4a5eb671bcb3a141887553ec4157b1e64d6641c1cd Active
sha256:33bd296ab7f37bdacff0cb4a5eb671bcb3a141887553ec4157b1e64d6641c1cd sha256:bc8b010e53c5f20023bd549d082c74ef8bfc237dc9bbccea2e0552e52bc5fcb1 Committed
sha256:bc8b010e53c5f20023bd549d082c74ef8bfc237dc9bbccea2e0552e52bc5fcb1 sha256:aa4b58e6ece416031ce00869c5bf4b11da800a397e250de47ae398aea2782294 Committed
sha256:aa4b58e6ece416031ce00869c5bf4b11da800a397e250de47ae398aea2782294 sha256:a8f09c4919857128b1466cc26381de0f9d39a94171534f63859a662d50c396ca Committed
sha256:a8f09c4919857128b1466cc26381de0f9d39a94171534f63859a662d50c396ca sha256:2ae5fa95c0fce5ef33fbb87a7e2f49f2a56064566a37a83b97d3f668c10b43d6 Committed
sha256:2ae5fa95c0fce5ef33fbb87a7e2f49f2a56064566a37a83b97d3f668c10b43d6 sha256:d0fe97fa8b8cefdffcef1d62b65aba51a6c87b6679628a2b50fc6a7a579f764c Committed
sha256:d0fe97fa8b8cefdffcef1d62b65aba51a6c87b6679628a2b50fc6a7a579f764c 
镜像的每一层都会被创建成 committed 状态的 snapshot，committed 表示该镜像层不可变，在启动容器时，将为每个容器创建一个可读写的 active snapshot，这一层是可读写的。下图是镜像 layer 与 snapshot 的对应关系：

Overlayfs snapshotter：
about：
通过联合文件系统Union fs，将lowDir和upperDir的内容合并成MergeDir，用户看来容器只有MergeDir，实际上是通过底层的lowDir和upperDir结合COW技术呈现给用户看到的；

总结从拉取镜像到建立容器的过程：
● 首先从Registry中将镜像拉取下来，拉取的内容包括镜像的index，config，manifest等json文件；
● 从index文件中匹配当前操作系统的配置，找到对应的manifest文件；
● 从mainfest文件中找到layers，根据这个layers去containerd的content文件系统中进行查找对应的sha256值，找不到的就去拉取；
● 完了之后根据每一层layers，去建立对应的snapshot：
  ○ 首先先通过调用Prepare，Prepare通过lowdir去指定父snapshot的路径（lowdir=snapshot1/fs:snapshott2/fs），去创建一个基于父snapshot建立的snapshot，这个snapshot的状态是actived；第一层snapshot没有父snapshot；
  ○ Prepare调用之后返回的是该容器的挂载信息，然后将当前layer的镜像文件解压到这个新建立的snapshot中去；
  ○ 将该snapshot commit，该snapshot转变成committed状态，不可变，同时commit调用还会将刚刚的actived状态的snapshot删掉，这样子就又建立了一层snapshot；
  ○ 每一层snapshot都是基于父snapshot进行建立的，通过Prepare，Commit之后建立起来的；
● 在所有镜像都解压完成之后，再次通过Prepare调用，以前一个snapshot为父snapshot建立一个actived的snapshot，而Prepare调用返回的是该snapshot的挂载信息，容器根据这个挂载信息作为rootfs，当前rootfs就包含了该镜像的所有层信息，并且Prepare调用返回的是actived的snapshot，rootfs是可写的；
● 后续用户在容器里面查看到的目录为Merge Dir，Merge Dir会向下（也就是lowDir中一层一层）去查找指定目录，当需要做修改时通过写时复制将lowDir中的数据复制到upperDir中；
所以，每一层snapshot其实是由原镜像layer数据+当前修改的数据（如果该snapshot是可写层的话）组成的，对snapshot进行commit之后，数据就会保存到containerd的content文件系统中，所以也就是说snapshot之间的数据是有重复的，在mergeDir进行查找时通过lowDir从上往下进行查找，如果在上层找到了就不会再往下层去查找；

Overlay Snapshotter：
源码：
overlayfs snapshotter 是 containerd 的内建插件，也是默认的 snapshotter。其实现位于 containerd 项目的 snapshots/overlay 目录，仅有 3 个代码文件（不包含测试）。
● snapshots/overlay/plugin/plugin.go 插件注册：containerd 在启动时会调用 import 该包，执行里面的 init 函数，注册插件。

main.go里的导入：通过import里加一个_符号，就能够实现导入该包但不使用这个包，只执行该包的init函数；
main.go这里导入了builtins包，目的就是触发builtins包下的所有init函数：

而builtins包又导入了所有插件包，所以会导致触发所有插件包的init函数：

这里就包括devbox snapshotter，devbox snapshotter就会执行其init函数，也就是注册devbox snapshotter，最终让containerd识别到自己；

那么为什么不直接在main.go里面导入这些插件呢？
还是为了解耦合！在main.go里面直接导入会使得整个main.go文件狠臃肿，所以通过builtins.go来隔离开；

● snapshots/overlay/overlay.go 插件实现：实现了 snapshots/snapshotter.go@Snapshotter 接口。
● snapshots/overlay/overlayutils/check.go 插件实现依赖的工具函数。
Snapshot调用链路：
调用方：
● 可以是devbox controller使用containerd client进行gRPC调用
● 也可以是命令行的方式：ctr进行调用

命令行方式调用：

Proxy：
这个SnapshotService调用返回的其实是一个proxy：


这层代理是一个的gRPC客户端的，通过这个proxy向gRPC服务端发送请求：

gRPC服务端：
具体的文件在servics/snapshots/service.go：

Service/真正执行：
上文提到的Prepare函数，其初始化时是初始化的metadata.Snapshot：

具体来查看这个函数，可以看到这个snapshot是在service结构体里的ss字段得到的：


那么就得查看这个service结构体的初始化函数，可以看到这个ss是由plugins插件里拿到的：

并且拿的是service类型的插件：

而这个插件在containerd初始化时被载入，init函数如下，可以看到其初始化了metadata插件：

metadata层：
那么后面就知道了，实际上的snapshotter相关的函数实际调用时是在这个metadata层被调用：

在这一层，会先调用metadata层去更新元数据，然后再到snapshotter里的devbox snapshotter去执行底层的函数：

为什么set lv removable需要container？
调用处：

通过使用snapshotter的Update函数来更新LV，传入特定的label，在update就会走对应的逻辑；
执行处：
这个update会先走metadata/snapshotter.go的Update函数，这一层是在CRI和底层实现之间的封装层，用来记录元数据；
如果这个info.Name为空，那么直接就会被返回；
如果不为空，但是在bucket中查不到，也就是这个info.Name不是containerID，那么也会直接返回：

那么为什么需要查这个contaienrID对应的bucket呢？因为我们当前这个函数是Update！！！Update就是更新原数据库中的值；
那么在这里旧的值就是从这个bucket里面取出来的，也就是这个local，而新的值就是传进来的Info参数，用这个新的值去更新旧的值！
所以说调用这个Update函数时需要查找旧的snapshot bucket；
而传进来的这个fieldpaths作用如下：


handleContainerExit and stopContaienr:
devbox snapshotter的应用：
在这两个函数里都会执行devbox snapshotter的Update函数，为了将container和lv进行解挂载；
也就是当容器异常退出时，仅执行container Exit函数，会触发解挂载；
删除容器时，执行container stop和container exit函数，触发两次解挂载；
理论上container stop这里是可以不加解挂载的操作的，因为无论如何都会执行container exit函数；
执行时机对比
1. StopContainer (container_stop.go:39-94)
触发时机：主动停止请求
Kubernetes/CRI调用
      ↓
StopContainer API
      ↓
执行优雅停止
具体场景：
● ✓ kubectl delete pod
● ✓ Pod被驱逐（eviction）
● ✓ 用户通过CRI接口主动停止容器
● ✓ Kubernetes调度器删除Pod
● ✓ crictl stop 命令
执行流程：
1. CRI客户端发送 StopContainerRequest
2. 从containerStore获取容器信息
3. 调用 stopContainer() 执行优雅停止（带timeout）
4. NRI插件回调
5. 获取容器info和runtime信息
6. 【自定义代码】调用 UpdateDevboxSnapshot 卸载LVM
7. 返回 StopContainerResponse
特点：
● 同步调用 - 调用者等待完成
● 有超时控制 - 可以设置graceful timeout
● 主动操作 - 外部主动发起
● 返回结果 - 需要返回成功/失败

2. handleContainerExit (sbserver/events.go:378-475)
触发时机：被动事件响应
容器进程退出
      ↓
TaskExit事件
      ↓
handleContainerExit
      ↓
清理和状态更新
具体场景：
● ✓ 容器内主进程自然结束（exit 0）
● ✓ 容器内进程崩溃（segfault, panic）
● ✓ 容器被OOM killed
● ✓ 容器达到resource limit
● ✓ 应用程序主动退出
执行流程：
1. 容器task退出，产生 TaskExit 事件
2. containerd事件循环捕获事件
3. 调用 handleContainerExit 处理
4. 尝试加载task（用于IO清理）
5. 删除task（调用 task.Delete）
6. 清理task-service中的shim实例（防止泄漏）
7. 更新容器状态（FinishedAt, ExitCode）
8. 发送 CONTAINER_STOPPED_EVENT
特点：
● 异步处理 - 事件循环中处理
● 无超时控制 - 容器已经退出
● 被动响应 - 响应已发生的事件
● 容错处理 - 需要处理各种异常情况（NotFound, Unavailable）

关键区别
方面	StopContainer	handleContainerExit
调用方式	CRI API主动调用	事件驱动被动触发
执行时机	容器停止之前	容器退出之后
容器状态	正在运行 → 停止中	已退出 → 清理中
超时控制	有（graceful timeout）	无（已经退出）
IO处理	正常关闭	清理残留
返回值	需要返回Response	仅返回error
调用者	kubelet/crictl	containerd内部事件循环

实际执行序列
场景1：kubectl delete pod（正常停止）
1. kubectl delete pod
        ↓
2. kubelet调用 StopContainer()  ← 执行这个
        ↓
3. stopContainer() 发送SIGTERM
        ↓
4. 等待graceful timeout
        ↓
5. 如果超时，发送SIGKILL
        ↓
6. 容器进程退出
        ↓
7. TaskExit事件产生
        ↓
8. handleContainerExit()  ← 执行这个
        ↓
9. 清理task、更新状态、发送事件
两个函数都会执行！
场景2：容器内进程崩溃（异常退出）
1. 容器进程crash（如segfault）
        ↓
2. 进程直接退出
        ↓
3. TaskExit事件产生
        ↓
4. handleContainerExit()  ← 只执行这个
        ↓
5. 清理task、更新状态
        ↓
6. kubelet通过Watch发现容器退出
        ↓
7. kubelet可能调用StopContainer（幂等性，直接返回）
只执行 handleContainerExit！

您的自定义代码分析
StopContainer中的devbox代码（86-91行）
fmt.Println("Check snapshotter:", snapshotter)

err = c.client.UpdateDevboxSnapshot(ctx, snapshotter, i.ID, unmountLvm, "true")
if err != nil {
    fmt.Println("Failed to update devbox snapshot:", err)
}
执行时机： 主动停止时
作用： 在容器停止时卸载devbox LVM卷
handleContainerExit中的代码（sbserver/events.go）
我注意到在您提供的 events.go 中没有类似的UpdateDevboxSnapshot调用！
这意味着：
● ✓ 主动停止（kubectl delete）→ 会卸载LVM
● ✗ 容器崩溃/自然退出 → 不会卸载LVM ⚠️

建议
您可能需要在 handleContainerExit 中也添加类似的逻辑：
// 在 handleContainerExit 函数的状态更新之后添加
err = cntr.Status.UpdateSync(func(status containerstore.Status) (...) {
    // ... 现有代码 ...
})

// 添加devbox快照清理
container, err := c.client.ContainerService().Get(ctx, cntr.Container.ID())
if err == nil && container.Snapshotter == "devbox" {
    err = c.client.UpdateDevboxSnapshot(ctx, container.Snapshotter, container.ID, unmountLvm, "true")
    if err != nil {
        logrus.WithError(err).Errorf("Failed to update devbox snapshot for container %s", cntr.Container.ID())
    }
}
这样无论容器是主动停止还是异常退出，都能正确清理devbox资源。
RuntimeClass如何触发devbox snapshotter：
完整的触发流程：
1. Kubernetes层面：RuntimeClass → RuntimeHandler
RuntimeClass就是设计用来让不同的Pod使用不同的低级容器运行时（runc），我们在containerd的config.toml里就注册了一个叫devbox-runc的容器运行时，但是其runitme_type还是io.containerd.runc.v2
# RuntimeClass定义
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: devbox-runtime          # Pod引用的名字
handler: devbox-runc             # 传递给containerd的handler名称
# Pod定义
spec:
  runtimeClassName: devbox-runtime   # 告诉K8s使用这个RuntimeClass
  containers:
  - name: my-container
    image: nginx
作用：
● spec.runtimeClassName: devbox-runtime 告诉kubelet使用名为 devbox-runtime 的RuntimeClass
● Kubelet读取RuntimeClass，获取 handler: devbox-runc
● Kubelet通过CRI调用containerd时，传递 runtimeHandler="devbox-runc"
2. Containerd配置：RuntimeHandler → Snapshotter
在 /etc/containerd/config.toml 中：
[plugins."io.containerd.grpc.v1.cri".containerd]
  default_runtime_name = "runc"
  
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes]
    # 默认runtime
    [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
      runtime_type = "io.containerd.runc.v2"
      
    # devbox runtime - 关键配置！
    [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.devbox-runc]
      runtime_type = "io.containerd.runc.v2"
      snapshotter = "devbox"           # 指定使用devbox snapshotter！
      [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.devbox-runc.options]
        SystemdCgroup = true

# devbox snapshotter插件配置
[plugins."io.containerd.snapshotter.v1.devbox"]
  root_path = "/var/lib/containerd/io.containerd.snapshotter.v1.devbox"
  lvm_vg_name = "devbox-vg"              # LVM卷组名称
  thin_pool_name = "devbox-vg-thinpool"  # thin pool名称
3. 代码层面的调用链
从你看的代码文件，完整链路是：
a. 创建容器时（container_create.go:214）
// 第154行：从sandbox获取RuntimeHandler
ociRuntime, err := c.getSandboxRuntime(sandboxConfig, sandbox.Metadata.RuntimeHandler)

// 第214行：使用runtime指定的snapshotter
opts := []containerd.NewContainerOpts{
    containerd.WithSnapshotter(c.runtimeSnapshotter(ctx, ociRuntime)),
    customopts.WithNewSnapshot(id, containerdImage, sOpts...),
}
b. 获取Runtime配置（sandbox_run.go:552-580）
func (c *criService) getSandboxRuntime(config *runtime.PodSandboxConfig, runtimeHandler string) (criconfig.Runtime, error) {
    // runtimeHandler = "devbox-runc" (来自RuntimeClass)
    
    if runtimeHandler == "" {
        runtimeHandler = c.config.ContainerdConfig.DefaultRuntimeName
    }
    
    // 从配置中查找对应的runtime
    handler, ok := c.config.ContainerdConfig.Runtimes[runtimeHandler]
    //                                                 ^^^^^^^^^^^^^^^^
    //                                       查找 "devbox-runc" 的配置
    if !ok {
        return criconfig.Runtime{}, fmt.Errorf("no runtime for %q is configured", runtimeHandler)
    }
    return handler, nil  // 返回包含 Snapshotter="devbox" 的配置
}
c. 选择Snapshotter（container_create.go:415-421）
func (c *criService) runtimeSnapshotter(ctx context.Context, ociRuntime criconfig.Runtime) string {
    if ociRuntime.Snapshotter == "" {
        // 如果runtime没指定，使用默认snapshotter
        return c.config.ContainerdConfig.Snapshotter  // "overlayfs"
    }
    
    log.G(ctx).Debugf("Set snapshotter for runtime %s to %s", ociRuntime.Type, ociRuntime.Snapshotter)
    return ociRuntime.Snapshotter  // 返回 "devbox"
}
d. Devbox Snapshotter注册（plugin/plugin.go:42-85）
func init() {
    plugin.Register(&plugin.Registration{
        Type:   plugin.SnapshotPlugin,
        ID:     "devbox",  // 注册ID为"devbox"
        Config: &Config{},
        InitFn: func(ic *plugin.InitContext) (any, error) {
            config, ok := ic.Config.(*Config)
            
            // 从config.toml读取LVM配置
            oOpts = append(oOpts, devbox.WithLvmVgName(config.LvmVgName))
            oOpts = append(oOpts, devbox.WithThinPoolName(config.ThinPoolName))
            
            // 创建devbox snapshotter实例
            return devbox.NewSnapshotter(root, oOpts...)
        },
    })
}

完整流程图：
Pod YAML
  ↓
  spec.runtimeClassName: devbox-runtime
  ↓
RuntimeClass
  ↓
  handler: devbox-runc
  ↓
Kubelet (CRI调用)
  ↓
  runtimeHandler="devbox-runc"
  ↓
containerd CRI Server
  ↓
  getSandboxRuntime("devbox-runc")
  ↓
config.toml
  ↓
  runtimes.devbox-runc.snapshotter = "devbox"
  ↓
  runtimeSnapshotter() → 返回 "devbox"
  ↓
containerd.WithSnapshotter("devbox")
  ↓
devbox.NewSnapshotter()
  ↓
使用LVM创建容器存储层
oci Runtime家在我来详细追踪从 config.toml 到 ociRuntime 的完整流程。
[9 tools called]
现在我给你完整的流程图和代码解释：

从 config.toml 到 ociRuntime 的完整流程
第一步：启动时加载 config.toml
代码位置：cmd/containerd/command/main.go 第 109-128 行
app.Action = func(context *cli.Context) error {
    config := defaultConfig()
    
    configPath := context.GlobalString("config")  // 默认 /etc/containerd/config.toml
    _, err := os.Stat(configPath)
    if !os.IsNotExist(err) || context.GlobalIsSet("config") {
        if err := srvconfig.LoadConfig(configPath, config); err != nil {
            return err
        }
    }
    // ...
}
解释：containerd 启动时读取 /etc/containerd/config.toml，调用 LoadConfig 解析。

第二步：解析 TOML 文件
代码位置：services/server/config/config.go 第 246-260 行
func loadConfigFile(path string) (*Config, error) {
    config := &Config{}
    
    file, err := toml.LoadFile(path)  // 使用 toml 库加载文件
    if err != nil {
        return nil, fmt.Errorf("failed to load TOML: %s: %w", path, err)
    }
    
    if err := file.Unmarshal(config); err != nil {  // 反序列化到 Config 结构体
        return nil, fmt.Errorf("failed to unmarshal TOML: %w", err)
    }
    
    return config, nil
}
Config 结构体（第 34-74 行）：
type Config struct {
    Version int `toml:"version"`
    Root    string `toml:"root"`
    State   string `toml:"state"`
    // ...
    Plugins map[string]toml.Tree `toml:"plugins"`  // 关键：所有插件配置都在这里
    // ...
}
解释：你的 config.toml 中的 [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.devbox-runc] 会被解析到 Config.Plugins 这个 map 里，key 是 "io.containerd.grpc.v1.cri"，value 是整个 [plugins."io.containerd.grpc.v1.cri"] 段的 TOML 树。

第三步：初始化 CRI 插件时解码配置
代码位置：services/server/server.go 第 243-250 行
// load the plugin specific configuration if it is provided
if p.Config != nil {
    pc, err := config.Decode(p)  // 从 Config.Plugins 中提取该插件的配置
    if err != nil {
        return nil, err
    }
    initContext.Config = pc  // 将解码后的配置赋值给 InitContext.Config
}
result := p.Init(initContext)  // 调用插件的 InitFn
Decode 函数（services/server/config/config.go 第 181-194 行）：
func (c *Config) Decode(p *plugin.Registration) (interface{}, error) {
    id := p.URI()  // 对于 CRI 插件，URI 是 "io.containerd.grpc.v1.cri"
    if c.GetVersion() == 1 {
        id = p.ID
    }
    data, ok := c.Plugins[id]  // 从 Plugins map 中取出对应的 TOML 树
    if !ok {
        return p.Config, nil
    }
    if err := data.Unmarshal(p.Config); err != nil {  // 反序列化到插件的 Config 结构体
        return nil, err
    }
    return p.Config, nil
}
解释：
● CRI 插件注册时提供了默认配置 criconfig.DefaultConfig()（pkg/cri/cri.go 第 42-46 行）
● Decode 函数从 Config.Plugins["io.containerd.grpc.v1.cri"] 中取出你写的配置，反序列化到 criconfig.PluginConfig 结构体
● 这个结构体包含 ContainerdConfig，其中有 Runtimes map[string]Runtime

第四步：CRI 插件初始化，拿到完整配置
代码位置：pkg/cri/cri.go 第 58-84 行
func initCRIService(ic *plugin.InitContext) (interface{}, error) {
    // ...
    pluginConfig := ic.Config.(*criconfig.PluginConfig)  // 类型断言，拿到解码后的配置
    
    c := criconfig.Config{
        PluginConfig:       *pluginConfig,  // 包含了你的 runtimes 配置
        ContainerdRootDir:  filepath.Dir(ic.Root),
        ContainerdEndpoint: ic.Address,
        RootDir:            ic.Root,
        StateDir:           ic.State,
    }
    log.G(ctx).Infof("Start cri plugin with config %+v", c)
    
    // ...
    s, err = server.NewCRIService(c, client, getNRIAPI(ic), warn)
    // ...
}
PluginConfig 结构体（pkg/cri/config/config.go 第 255-279 行）：
type PluginConfig struct {
    // ...
    ContainerdConfig ContainerdConfig `toml:"containerd" json:"containerd"`
    // ...
}

type ContainerdConfig struct {
    Snapshotter        string             `toml:"snapshotter" json:"snapshotter"`
    DefaultRuntimeName string             `toml:"default_runtime_name" json:"defaultRuntimeName"`
    Runtimes           map[string]Runtime `toml:"runtimes" json:"runtimes"`  // 关键！
    // ...
}
解释：此时 pluginConfig.ContainerdConfig.Runtimes 已经是一个 map，包含：
● "runc" → Runtime{Type: "io.containerd.runc.v2", Options: {...}}
● "crun" → Runtime{Type: "io.containerd.runc.v2", Options: {BinaryName: "/usr/bin/crun", ...}}
● "devbox-runc" → Runtime{Type: "io.containerd.runc.v2", Snapshotter: "devbox", Options: {...}}

第五步：运行时查询 ociRuntime
代码位置：pkg/cri/server/sandbox_run.go 第 740-769 行
func (c *criService) getSandboxRuntime(config *runtime.PodSandboxConfig, runtimeHandler string) (criconfig.Runtime, error) {
    // ... 处理 untrusted workload 逻辑 ...
    
    if runtimeHandler == "" {
        runtimeHandler = c.config.ContainerdConfig.DefaultRuntimeName
    }
    
    handler, ok := c.config.ContainerdConfig.Runtimes[runtimeHandler]  // 从 map 中查询
    if !ok {
        return criconfig.Runtime{}, fmt.Errorf("no runtime for %q is configured", runtimeHandler)
    }
    return handler, nil  // 返回整个 Runtime 结构体
}
解释：
● 当 kubelet 传来 runtimeHandler = "devbox-runc"
● c.config.ContainerdConfig.Runtimes["devbox-runc"] 返回：
criconfig.Runtime{
    Type:        "io.containerd.runc.v2",
    Snapshotter: "devbox",
    Options:     map[string]interface{}{"SystemdCgroup": true},
}
● 这就是 ociRuntime

第六步：从 ociRuntime 提取 snapshotter
代码位置：pkg/cri/server/image_pull.go 第 789-796 行
ociRuntime, err := c.getSandboxRuntime(s, runtimeHandler)
if err != nil {
    return "", fmt.Errorf("experimental: failed to get sandbox runtime for %s, err: %+v", runtimeHandler, err)
}

snapshotter = c.runtimeSnapshotter(context.Background(), ociRuntime)
log.G(ctx).Infof("experimental: PullImage %q for runtime %s, using snapshotter %s", imageRef, runtimeHandler, snapshotter)
runtimeSnapshotter 函数（需要查找，但逻辑是）：
func (c *criService) runtimeSnapshotter(ctx context.Context, r criconfig.Runtime) string {
    if r.Snapshotter != "" {
        return r.Snapshotter  // 直接返回 "devbox"
    }
    return c.config.ContainerdConfig.Snapshotter  // 否则用默认的
}

总结：ociRuntime 到底是什么？
ociRuntime 是 criconfig.Runtime 类型的结构体，包含：
type Runtime struct {
    Type                string                 // "io.containerd.runc.v2"
    Path                string                 // 可选，shim 二进制路径
    Snapshotter         string                 // "devbox" ← 你配置的
    Options             map[string]interface{} // {"SystemdCgroup": true}
    PodAnnotations      []string
    ContainerAnnotations []string
    NetworkPluginConfDir string
    // ... 其他字段
}
对应你的配置：
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.devbox-runc]
  runtime_type = "io.containerd.runc.v2"          → ociRuntime.Type
  snapshotter = "devbox"                           → ociRuntime.Snapshotter
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.devbox-runc.options]
    SystemdCgroup = true                           → ociRuntime.Options["SystemdCgroup"]
流程图：
config.toml
    ↓ (LoadConfig + toml.Unmarshal)
Config.Plugins["io.containerd.grpc.v1.cri"] (toml.Tree)
    ↓ (Decode + Unmarshal)
criconfig.PluginConfig.ContainerdConfig.Runtimes["devbox-runc"] (criconfig.Runtime)
    ↓ (getSandboxRuntime)
ociRuntime (criconfig.Runtime 结构体实例)
    ↓ (runtimeSnapshotter)
snapshotter = "devbox"

关键配置文件示例：
你需要在 /etc/containerd/config.toml 中确保有这些配置[[memory:5125773]]：
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.devbox-runc]
  runtime_type = "io.containerd.runc.v2"
  snapshotter = "devbox"  # 这是关键！
  
[plugins."io.containerd.snapshotter.v1.devbox"]
  lvm_vg_name = "vg_data"  # 确保这个VG存在！
  thin_pool_name = "vg_data-thinpool"
这就是为什么你之前遇到LVM错误 - 配置中的 vg_data 卷组可能不存在！
Pull Image和CreateSnapshot拿snapshotter的方式：
两段代码虽然都要拿到 RuntimeHandler，但处境完全不同。
1. PullImage 阶段（pkg/cri/server/image_pull.go 784 行）
runtimeHandler, ok := s.Annotations[annotations.RuntimeHandler]
if !ok {
    return snapshotter, nil
}
● 数据来源：PodSandboxConfig 的 annotations。
● 时机：sandbox 还没创建——只是 kubelet 在发起 PullImage 请求时先打个招呼。
● 目的：用 RuntimeHandler 找到对应的 snapshotter/运行时配置，保证拉镜像时（尤其 unpack）和后续 pod 运行时保持一致。
● 特点：此时没有可依赖的 sandbox 元数据，只能依赖 kubelet 提前传来的 annotation（io.containerd.cri.runtime-handler）。如果 annotation 缺失，直接回退到默认 snapshotter。

2. ContainerCreate 阶段（pkg/cri/server/container_create.go 154 行）
ociRuntime, err := c.getSandboxRuntime(sandboxConfig, sandbox.Metadata.RuntimeHandler)
● 数据来源：已经创建好的 sandbox 对象的 Metadata.RuntimeHandler。
● 时机：sandbox 已经通过 RunPodSandbox 创建并登记到 CRI store。
● 目的：让新容器（业务容器）沿用同一个 sandbox runtime，保持 runtime/Snapshotter 与 sandbox 一致。
● 特点：这里不再读取 annotation；创建 sandbox 时已经把 handler 落到了内部元数据里，后续所有容器都以它为准。

总结
● PullImage 读的是 kubelet 传来的 Pod annotation，解决“尚未有 sandbox”时的 runtime 选择问题。
● ContainerCreate 读的是 sandbox 的持久 metadata，只要 sandbox 存在就能准确拿到 handler，避免依赖外部 annotation。
本质上是“预拉镜像阶段 vs sandbox 已落地后的容器创建阶段”的差异。

Devbox 删除：
Devbox：

devbox会做几件事：
● 首先是先将自己的子资源，最主要是Pod删除
● 然后删除Storage，这里也就是LV，这里面步骤比较长
  ○ 先创建一个临时的devbox容器，baseImage是alipine容器，才4MB左右
  ○ 然后通过传入的devbox标识，devbox snapshotter就可以识别这个容器
  ○ 最后调用SetLvRemovable函数来标记这个lv可以删除
  ○ 最后删除这个容器
● 然后移除自己的Finalizer标记，让kubenet知道当前这个devbox资源可以被删除
Containerd：
Devbox Pod删除：
容器Exit和Stop：
当devbox删除自己的pod时，containerd主要会触发两个函数：
● handleContainerExit
● handleContainerStop
其中Exit函数会先调用，然后再调用Stop函数：
这两个函数里面跟devbox相关的其实就一个，调用Update函数，让lv跟当前这个container解挂载：

snapshotter的Update函数，当检测到Label里带有这个"containerd.io/snapshot/devbox-unmount-lvm"key时，就会触发SetUnmountedWithKey和unmountLVM这两个函数：

其中，SetUnmoutedWithKey主要是将这个contentID对应的bucket中的path删掉：

然后调用unmountLvm函数进行解挂载：

删除LV：
删除lv的方式是通过controller那边重新建立一个container，并且将其挂载到原来的lv，最后调用snapshotter的Update函数，将这个contentID对应的bucekt标记为可删除：

containerd标记bucket为可删除的操作，还是通过Label的key来分辨：


等Controller的SetLvRemovable函数执行完成后，就会调用RemoveContainer函数，那么就会触发containerd删除容器的操作：


最后调用snapshotter的Remove函数，这里面去执行真正的解挂载操作：

首先先调用RemoveDevbox来将这个devbox对应的contentID的bucekt给删除，删除整个bucket是不需要比对key的，删除完成之后直接返回空：
而真正的解挂载和删除lv的操作是在defer函数里面做的，这个asyncRemove参数是控制异步还是同步删除，如果为false则是同步删除，那么就会获取对应的可以删除的目录（原snapshotter实现）和可以删除的lv目录（devbox snapshotter实现）：

获取可以删除的lv其实就是比对bolt里面存的lv（正在使用的）和整个vg的lv（包含正在使用的，不再使用的，非devbox的lv）做个比较，来获取devbox的并且不再使用的lv：（这里有bug，就是可能lv刚创建出来，还没写入bucket里，刚好触发containerd的gc，也就是Cleanup函数，导致这个lv直接被删除了）

然后将lv解挂载并且删除：

Kill容器内部进程：
问题背景：
通过kill掉容器进程模拟容器崩溃退出时发现的一个问题：

执行流程：
● Kill container1 
● 此时会执行container1 的handleContainerExit函数，也就是SetUnmountWithKey，将contentID对应的path删除
● 由于容器进程崩溃，pod也会被干掉，而devbox的机制会使得pod重建，所以容器也重新建立，这个时候会调用CreateSnapshot里的SetDevboxContent函数，来将这个contentID对应的path写入：

（因为lv已经存在，不需要重新建立lv，所以直接走这里的逻辑）
● 随后kubelet会发起RemoveContainer的cri请求，导致containerd里面会调用删除容器的函数，具体是删除snapshot的函数：

● 最终其实也就是会调用snapshotter的Remove函数：

● 然后remove又会调用RemoveDevbox函数，导致这个contentID对应的bucket里的path字段又被删除
总结：
也就是container2重建之后，并且contentID对应的bucket的path字段重建之后，kubelet会发起remove的cri请求将container1的path字段删除，而导致误删了container2的path字段，进而导致的后果就是当前这个bucket里对应的path为空，所以其他pod/容器可以挂载这个lv，也就是导致了一个lv可以被多个pod进行挂载！
解决方法：
在SetDevboxContent时，加一个key：containerID，也就是devbox这个bucekt多一个字段；
然后在RemoveDevbox调用内部，具体来说是删除path的逻辑里多一个校验步骤：
校验当前bucket里的containerID和执行删除path的containeriD是否为同一个，如果是才可以删除，如果不是则跳过删除：

这样一来，当第一个容器想要删除path时：
● 如果当前第二个容器还没创建，那么这个containerID key自然为第一个容器的containerID，允许删除这个contentID对应的path
● 如果第二个容器已经创建了，那么这个containerID就是第二个容器，那么此时就不允许第一个容器执行删除path的操作
也就是将误删的行为杜绝了。
节点重启导致旧Pod创建容器失败
复现
● 创建Running状态的devbox，等待pod启动就绪
● 将节点重启 sudo reboot
● 发现devbox pod处于CreateContainerError的状态

● 查看describe

containerd的行为分析
从下图中可以看到，在重启containerd后，会先清理一些container的遗留进程，随后开始调用criService的recover函数

而查看kubelet的日志，kubelet是后于containerd启动的，也就是说recover这个cri接口并不是kubelet调用的：

具体调用处是在插件初始化时启动的：



┌─────────────────────────────────────────────────────────┐
│                  Containerd 进程                          │
│                                                           │
│  ┌─────────────────────────────────────────────────────┐│
│  │          CRI 插件 (criService)                       ││
│  │                                                      ││
│  │  [内部方法]                    [RPC 方法]            ││
│  │  - recover()                   - CreateContainer()  ││
│  │  - loadSandbox()               - StopContainer()    ││
│  │  - loadContainer()             - RemoveContainer()  ││
│  │  ↑                             ↑                    ││
│  │  │                             │                    ││
│  │  │ 内部调用                     │ gRPC 调用          ││
│  │  │ (不需要 kubelet)            │ (需要 kubelet)     ││
│  └──┼─────────────────────────────┼───────────────────┘│
│     │                             │                     │
│     │                             │ gRPC 服务器         │
└─────┼─────────────────────────────┼─────────────────────┘
      │                             │
      │                             │ TCP 连接
      │                             │
      │                             ↓
      │                      ┌──────────────┐
      │                      │   Kubelet    │
      │                      │              │
      └──(不可见)──────────────│ (gRPC 客户端)│
                             └──────────────┘
分析流程
节点关机
流程：节点关机 -> 所有进程被kill -> 没有执行handleContainerExit函数 ，即没有调update函数：

导致path没有被删除！！
节点启动
流程：Containerd 调用cri recover函数 -> 将container标记为驱逐状态 -> Pod删除-> Controller使得Pod重建 -> Key对应不上 -> 导致CreateContainerError
分析：
当pod被删除时调用的RemoveContainer函数 -> WithSnapshotCleanup -> Remove -> RemoveDevbox ，所以只要在这里RemoveDevbox里重新加上移除Key的逻辑即可；
当pod被重建时就会发现这个Key被移除了，即可以挂载上LV；
Containerd lv误删
现象
某个devbox刚创建出来时，lv创建，但是lv在几秒后被清除：

导致devbox在commit时由于找不到原有的LV报错：

错误原因
猜测是由于devbox刚创建时，lv被创建，这时候刚好触发了containerd的GC机制，由于此时lv还没被写入bucket中，直接被Cleanup：

解决方案
内存白名单：为正在创建的lv维护一个sync.Map，Cleanup时跳过这个lv；
后续在snasphot误删过程中，将其改成事务互斥的解决方案，更加优雅和底层；
Containerd snasphot误删
问题
用户尝试Stopped的devbox一直卡着：

问题在于Commit过程失败导致的不断重试，commit失败的原因是：Containerd在创建容器时发现某个lowdir丢失了，导致挂载目录失败：


临时解决
删除sealos.io中的对应镜像，重新拉取解压到sealos.io下即可；
原因
devbox在Cleanup函数里的实现使用了读事务：

所以在创建容器的过程中涉及到的snapshot目录，以及lv，在刚好触发GC时不会在同一个bucket中被读取到，也就是会被GC时意外清理掉 ，所以就导致了之前的lv误删以及现在的lowdir不存在的问题；
解决方法
boltdb中只允许有一个写事务进行：

那么解决方法就是在Cleanup时使用写事务，也就是跟overlayfs snapshotter使用一样的方案：

这样一来，当在createSnapshot的时候，Cleanup函数会在“获取清理路径”时由于获取不到写锁而阻塞：

也就是说这时候不会触发gc操作，即不会在createSnapshot的时候将刚准备好的lowdir以及lv给误删了；
测试
准备
使用一个devbox不断commit接近400次，找到当前的base image，可以看到有460层:

hub.staging-usw-1.sealos.io/devbox-test/edge-toggle-devbox-0:sphrh-2025-11-14-073708
调整Containerd gc策略：
[plugins."io.containerd.gc.v1.scheduler"]
  pause_threshold = 0.5          # 提高到 50%，让 GC 更频繁
  deletion_threshold = 1         # 每删除 1 个资源就触发 GC
  mutation_threshold = 1         # 每 1 次变更就触发 GC
  schedule_delay = "0ms"         # 立即触发
  startup_delay = "10ms"         # 启动后 10ms 就开始
复现
将节点Containerd回退至和线上版本一样后，开始跑测试：
./dtest edge toggle --count 1 --concurrent 1 --cycles 3000 --image hub.staging-usw-1.sealos.io/devbox-test/edge-toggle-devbox-0:k9pcv-2025-11-14-060420
复现不了。。。。。 😭😭😭
执行
将其他节点的devbox.sealos.io/node=label去除，让devbox只调度到这个节点上，随后创建一个devbox并且写入数据，进行如下测试：
● 将Devbox 关机 commit
● 待commit完成之后删除当前的base Image （k8s.io）
● 等待6s后重新开机并且写入500M数据
● 循环以上步骤
让其Commit 3000次，在第177次时失败了，失败原因是状态转换太快导致的：

也就是并不是因为lowdir丢失而导致的失败 ，再将这个Devbox patch Running，该Devbox能够正常启动，查看当前使用的镜像snapshot层数，370层：

额外的修改
Remove
在Remove时将defer提到事务之外，也就是必须执行完事务，即元数据都被清理之后，才会去删除目标目录：

如果放在内部，当内部回调函数执行完成后返回nil，接着执行defer函数将目录清除，但是这时候事务才可能刚刚才去进行Commit操作 ，如果commit失败了，那么整体数据回滚，就会出现元数据还在但是目录已经不存在的风险；
修改之后，当元数据清理完成之后才会去执行清理目录操作，如果清理目录的操作失败了，那么这些目录就会交给containerd gc的时候去清理；
createSnapshot
将原先的MkdirAll操作改成Rename


Devbox 镜像占用两倍存储
原因



https://applink.feishu.cn/client/message/link/open?token=AmhmJrsuRYADaRLzEB9HABw%3D
解决方法
Devbox创建时解压
Containerd在拉取镜像时会根据Pod的Annotation：io.containerd.cri.runtime-handler来决定当前使用哪个Snapshotter：


所以在创建devbox pod时加入这个Annotation即可：

Commit时解压
拉取镜像时指定解压到devbox snapshotter下：

为什么选择这种方法
熟悉了代码之后使用的：
具体使用哪个snapshotter是在ociRuntime里拿的，但是在创建容器和拉取镜像时获取ociRuntime的方法不太一样：一个是从sandbox里拿，另一个是从pod的annotation里拿：

应该是拉取镜像时sandbox还没就绪，所以不能从sandbox里面拿，只能从k8s传过来的pod annotation里面拿；
主要还是 containerd 社区也依赖 runtime handler 来传入 snapshotter： CRI support specify snapshotter during pod creation instead of a global config · Issue #6657 · conta
似乎我们升级集群之后都不需要这样改了，这个功能已经实现了：
Support image pull per runtime class · Issue #4216 · kubernetes/enhancements
（Support image pull per runtime class · Issue #4216 · kubernetes/enhancements
Enhancement Description One-line enhancement description (can be used as a release note): Add support to containerd/kubelet/CRI to support image pull per runtime class Kubernetes Enhancement Propos...）
v1.29 alpha
Support image pull per runtime class by kiashok · Pull Request #11807 · containerd/containerd 升级集群后可以修复这个 pr ，目前可以先用 annotations 传入；
https://kubernetes.io/docs/concepts/containers/runtime-class/

测试
Devbox 创建时解压
改进后
使用devbox runtime镜像测试（使用其他镜像一会直接就退出了，然后又重建，不断循环），找到当前节点上不存在的镜像（k8s.io)，然后选用这个镜像来测试：
crpi-5yq5lj8mtkm1w8pm.cn-hangzhou.personal.cr.aliyuncs.com/linzichun-namespace/nginx-1.22.1:v1.1
节点上均不存在这个镜像：


创建一个base image为当前镜像的devbox观察节点containerd日志，Annotation被读取，使用devbox snapshotter进行解压：

查看两个snapshotter的snapshot目录：
ctr -n k8s.io images usage --snapshotter devbox crpi-5yq5lj8mtkm1w8pm.cn-hangzhou.personal.cr.aliyuncs.com/linzichun-namespace/nginx-1.22.1:v1.1
可以看到overlayfs snapshotter没有解压该镜像，而devbox snapshotter解压了该镜像：

改进前
清理镜像：


用相同的镜像去创建devbox，可以看到在overlayfs snapshotter和devbox snapshotter都解压了同一个镜像：

Commit时解压
改进后
使用alpine镜像来测试，当前环境下没有alpine镜像以及相关的snapshot：

随后使用上述新的接口来创建一个base Image为alpine:3.9的容器来查看对应snapshot是否存在于overlayfs和devbox snapshotter，创建的容器如下，以及可以看到当前镜像只解压到了devbox snapshotter：

改进前
清除alpine镜像：

使用旧的接口进行创建容器，可以看到镜像也是只解压到了devbox snapshotter里，改进前后无差别：

升级后续
在k8s集群升级后可以撤销这个pr：
https://github.com/lingdie/sealos/pull/73
Commit ns 切换
设计初衷
之前测试的时候发现直接在k8s.io里创建容器可能导致该容器被kubelet回收/干掉，所以在devbox进行commit时切换到sealos.io这个ns去执行具体的操作；
但是在上线之后，测试时发现在k8s.io里直接创建容器等不再会出现之前的情况，并且在sealos.io下执行有以下缺点：
● 在Commit时需要重新拉取当前devbox的base Image的一些配置文件到sealos.io下，需要花费额外的时间
● 管理复杂，容易混淆两个ns：
  ○ 当用户devbox Commit出错时需要将镜像重新拉取至sealos.io
  ○ 当用户devbox启动时出错时需要将镜像拉取至k8s.io
修改内容
1. 将devbox commit时所用的ns从sealos.io切换为k8s.io：

1. 需要将初始化的GC移除，否则初始化GC将移除k8s.io下的所有镜像和容器：

基础功能测试
在当前集群环境下跑生命周期测试：

devbox正常功能不受影响；
大镜像测试
创建两个Devbox，并且写入10G的数据，用来模拟大镜像Commit：

将这两个devbox写入10G数据之后进行commit，commit时报错：

而commit过程中nerdctl create容器的操作其实是成功的，但是nerdctl commit失败了，并且在报错之后，错误处理时没有删除这个新创建出来的容器，导致后续的commit一直失败；
推测Commit失败的原因是： 写入大数据之后，磁盘使用率上升，并且达到kubelet gc标准 -> 触发了kubelet的image gc机制，将一些不使用的基础镜像删除 -> 进而触发Containerd content store gc，删除镜像的content store -> 导致Commit失败；
而使用sealos.io进行Commit时，其content store不会被删除的原因是，containerd在回收content store时会遍历所有的ns，只有所有ns都没有这个content的引用时，才会将其删除：
Containerd Content Store与Namespace共享
TODO
● commit成功之后的移除镜像逻辑是否需要去掉？
Pin Devbox Image（解决ns切换问题）
需求场景
Devbox Commit ns
具体实现
在Containerd拉取镜像时，判断当前使用的snapshotter是否为devbox，如果是则给当前镜像加入pin label：

为什么选择使用snapshotter来判断？
因为devbox snapshotter只处理devbox相关的容器和镜像，并且当前devbox创建的容器所拉取的镜像其实就是我们要commit的容器的base镜像；通过保护这个镜像，就可以保护其在k8s.io进行commit时不被kubelet回收；
为什么不在Devbox Controller里实现？
最核心的点在于controller只是保证Pod创建成功，但是无法判断容器是否成功启动，也就是镜像是否拉取成功，那么我们就没办法给这个镜像pin住；
在Controller里实现可以通过轮询image service来判断镜像是否拉取成功，然后再去pin住镜像，但是有的镜像一拉就是十几甚至几十分钟，所以这个方案不管用；
测试结果
这个方式只能在重新拉取镜像时 才管用，也就是如果本地已经存在的镜像，或者已经正在运行的devbox，那么可能需要先执行个脚本，让他们的base image先加上pin label，然后再将此次改动上线；
Create Devbox
先提前清理对应的镜像：

然后创建一个对应镜像的devbox，可以看到其镜像已经被添加上pin label了：

Commit Devbox
将devbox写入数据进行Commit，然后开机，也能看到其Commit Image被pin住了：

是否驱逐
当前节点磁盘用量：

Kubelet gc 阈值配置：

先调整日志级别，使我们能够看到对应的pin日志：

修改前：
先将Containerd更换至和线上集群一样：
echo "替换containerd..."
systemctl stop containerd
mv /usr/bin/containerd /usr/bin/containerd.backup

wget -O /usr/bin/containerd 'http://sealos-io.oss-cn-hangzhou.aliyuncs.com/cloud%2Fdevbox%2Fv1alpha2%2F11-24%2Fcontainerd?Expires=2963978507&OSSAccessKeyId=LTAI5tQPN7zEdUaPXbsneT5g&Signature=lD87wyorpcZJMlE2QWPALlQY9FU%3D'
chmod +x /usr/bin/containerd

# start containerd
systemctl start containerd
然后提前创建好devbox，等待commit：

最后调整kubelet的gc阈值，调节至75%，重启kubelet：
systemctl restart kubelet
执行devbox commit，查看kubelet日志，可以看到当前镜像会被gc，但是commit过程成功了😢 ：

修改后：
更换containerd，创建devbox，可以看到其base image已经被pin住了：

往devbox里写入数据，然后commit，查看kubelet日志，kubelet会跳过当前被pin住的镜像：


上线测试
测试流程
该测试主要是将线上的正在Running的devbox ，将其base image pin住，分以下几种情况测试：
● 正在Running的Devbox -> 拿最新的ContentID
● 关机的Devbox -> 跳过没有Node的Devbox
● 非当前节点的Devbox -> 跳过非当前节点的Devbox
环境准备：

执行脚本，将当前节点的devbox base image pin住：

检查所有devbox的base image 均已经被pin住；
staging环境测试也ok👌；
Pin 脚本
#!/bin/bash

# Pin Devbox Base Images Script
# 用于批量 pin 当前节点上所有 devbox 的 base image，防止 Kubelet GC 删除镜像

set -euo pipefail

# 默认配置
NAMESPACE="${NAMESPACE:-}"
NODE_NAME="${NODE_NAME:-}"
DRY_RUN="${DRY_RUN:-false}"
VERBOSE="${VERBOSE:-false}"
CONTAINERD_ADDRESS="${CONTAINERD_ADDRESS:-unix:///var/run/containerd/containerd.sock}"
KUBECONFIG="${KUBECONFIG:-}"

# Pin 镜像的 label
PINNED_LABEL_KEY="io.cri-containerd.pinned"
PINNED_LABEL_VALUE="pinned"
CONTAINERD_NAMESPACE="k8s.io"

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 帮助信息
usage() {
    cat <<EOF
Usage: $0 [OPTIONS]

Pin base images for all devboxes on the current node.

OPTIONS:
    -n, --namespace NAMESPACE    Filter devboxes by namespace (default: all namespaces)
    --node-name NODE_NAME        Node name to filter devboxes (default: auto-detect)
    --dry-run                    Dry run mode: only print what would be done
    -v, --verbose                Verbose output
    --containerd-address ADDR    Containerd address (default: unix:///var/run/containerd/containerd.sock)
    --kubeconfig PATH           Path to kubeconfig file (default: use in-cluster config or ~/.kube/config)
    -h, --help                   Show this help message

ENVIRONMENT VARIABLES:
    NODE_NAME                    Current node name (auto-detected if not set)
    NAMESPACE                    Namespace to filter devboxes
    DRY_RUN                      Set to "true" for dry-run mode
    VERBOSE                      Set to "true" for verbose output
    KUBECONFIG                   Path to kubeconfig file

EXAMPLES:
    # Basic usage
    $0

    # Dry-run mode
    $0 --dry-run

    # Specify node name
    $0 --node-name=sealos-staging-devbox-worker001

    # Filter by namespace
    $0 --namespace=devbox-test

    # Verbose output
    $0 --verbose
EOF
}

# 解析命令行参数
parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            -n|--namespace)
                NAMESPACE="$2"
                shift 2
                ;;
            --node-name)
                NODE_NAME="$2"
                shift 2
                ;;
            --dry-run)
                DRY_RUN="true"
                shift
                ;;
            -v|--verbose)
                VERBOSE="true"
                shift
                ;;
            --containerd-address)
                CONTAINERD_ADDRESS="$2"
                shift 2
                ;;
            --kubeconfig)
                KUBECONFIG="$2"
                shift 2
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            *)
                echo "Unknown option: $1" >&2
                usage
                exit 1
                ;;
        esac
    done
}

# 日志函数
log_info() {
    echo -e "${GREEN}[INFO]${NC} $*"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $*"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*" >&2
}

log_verbose() {
    if [[ "$VERBOSE" == "true" ]]; then
        echo -e "${YELLOW}[VERBOSE]${NC} $*"
    fi
}

# 检查依赖
check_dependencies() {
    local missing_deps=()

    if ! command -v kubectl &> /dev/null; then
        missing_deps+=("kubectl")
    fi

    if ! command -v ctr &> /dev/null; then
        missing_deps+=("ctr")
    fi

    if ! command -v jq &> /dev/null; then
        missing_deps+=("jq")
    fi

    if [[ ${#missing_deps[@]} -gt 0 ]]; then
        log_error "Missing required dependencies: ${missing_deps[*]}"
        log_error "Please install: ${missing_deps[*]}"
        exit 1
    fi
}

# 获取当前节点名
get_current_node_name() {
    if [[ -n "$NODE_NAME" ]]; then
        echo "$NODE_NAME"
        return
    fi

    # 尝试从环境变量获取
    if [[ -n "${NODE_NAME:-}" ]]; then
        echo "$NODE_NAME"
        return
    fi

    # 尝试从 hostname 获取
    local hostname
    hostname=$(hostname 2>/dev/null || echo "")
    if [[ -n "$hostname" ]]; then
        echo "$hostname"
        return
    fi

    log_error "Failed to detect current node name. Please set NODE_NAME environment variable or use --node-name option"
    exit 1
}

# 配置 kubectl
setup_kubectl() {
    if [[ -n "$KUBECONFIG" ]]; then
        export KUBECONFIG="$KUBECONFIG"
    fi

    # 测试 kubectl 连接
    if ! kubectl cluster-info &> /dev/null; then
        log_warn "kubectl cluster-info failed, but continuing..."
    fi
}

# Pin 镜像
pin_image() {
    local image_name="$1"
    local devbox_name="$2"
    local devbox_namespace="$3"

    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY-RUN] Would pin image: $image_name (devbox: $devbox_namespace/$devbox_name)"
        return 0
    fi

    # 检查镜像是否已存在（使用 ctr image ls）
    # 直接在整个输出中匹配镜像名，使用 -F 进行精确字符串匹配
    # 这样可以避免正则表达式和输出格式的问题
    local image_exists
    # 使用 grep -F 精确匹配，匹配完整的镜像名（repo:tag 格式）
    # 过滤掉 DEPRECATION 警告，只检查镜像列表
    # 由于镜像名是唯一的，直接匹配即可
    image_exists=$(ctr -n "$CONTAINERD_NAMESPACE" image ls 2>/dev/null | grep -v "DEPRECATION" | grep -F "$image_name" | head -1 || echo "")
    
    if [[ -z "$image_exists" ]]; then
        log_error "Image $image_name not found in namespace $CONTAINERD_NAMESPACE"
        log_verbose "Hint: Make sure the image has been pulled by kubelet on this node"
        log_verbose "You can check with: ctr -n $CONTAINERD_NAMESPACE image ls | grep -F $(echo $image_name | cut -d: -f1)"
        return 1
    fi

    # 检查是否已经 pinned（使用 ctr image inspect 检查 label）
    local pinned_label
    pinned_label=$(ctr -n "$CONTAINERD_NAMESPACE" image inspect "$image_name" 2>/dev/null | jq -r ".labels.\"${PINNED_LABEL_KEY}\" // \"\"" || echo "")
    
    if [[ "$pinned_label" == "$PINNED_LABEL_VALUE" ]]; then
        log_verbose "Image $image_name is already pinned"
        return 0
    fi

    # Pin 镜像（使用单数 image，忽略 DEPRECATION 警告）
    local label_output
    label_output=$(ctr -n "$CONTAINERD_NAMESPACE" image label "$image_name" "${PINNED_LABEL_KEY}=${PINNED_LABEL_VALUE}" 2>&1)
    local label_exit_code=$?
    
    # 检查退出码，如果为 0 则成功
    if [[ $label_exit_code -eq 0 ]]; then
        # 验证 label 是否真的被设置了
        local pinned_label
        pinned_label=$(ctr -n "$CONTAINERD_NAMESPACE" image inspect "$image_name" 2>/dev/null | jq -r ".labels.\"${PINNED_LABEL_KEY}\" // \"\"" || echo "")
        
        if [[ "$pinned_label" == "$PINNED_LABEL_VALUE" ]]; then
            log_info "✓ Successfully pinned image: $image_name"
            return 0
        else
            log_warn "Pin command succeeded but label not found. Output: $label_output"
            # 即使 label 检查失败，如果命令成功，也认为操作成功（可能是时序问题）
            log_info "✓ Successfully pinned image: $image_name"
            return 0
        fi
    else
        # 命令失败，提取真正的错误信息（排除 DEPRECATION 警告和 label 信息）
        local error_output
        error_output=$(echo "$label_output" | grep -v "DEPRECATION" | grep -v "^$" | grep -iE "(error|failed|not found)" || echo "")
        
        log_error "Failed to pin image: $image_name"
        if [[ -n "$error_output" ]]; then
            log_error "Error details: $error_output"
        elif [[ -n "$label_output" ]]; then
            # 如果没有明确的错误信息，但命令失败，输出原始输出
            log_error "Command output: $label_output"
        fi
        return 1
    fi
}

# 处理单个 devbox
process_devbox() {
    local devbox_json="$1"
    local current_node="$2"

    local devbox_name
    devbox_name=$(echo "$devbox_json" | jq -r '.metadata.name // ""')
    local devbox_namespace
    devbox_namespace=$(echo "$devbox_json" | jq -r '.metadata.namespace // ""')
    local content_id
    content_id=$(echo "$devbox_json" | jq -r '.status.contentID // ""')
    local devbox_node
    devbox_node=$(echo "$devbox_json" | jq -r '.status.node // ""')

    # 检查 contentID
    if [[ -z "$content_id" ]]; then
        log_verbose "SKIP: Devbox $devbox_namespace/$devbox_name has no contentID"
        return 2  # skipped
    fi

    # 检查节点
    if [[ -z "$devbox_node" ]]; then
        log_verbose "SKIP: Devbox $devbox_namespace/$devbox_name has no node assigned"
        return 2  # skipped
    fi

    if [[ "$devbox_node" != "$current_node" ]]; then
        log_verbose "SKIP: Devbox $devbox_namespace/$devbox_name is on node $devbox_node, not current node $current_node"
        return 2  # skipped
    fi

    # 获取 commit record
    local commit_records
    commit_records=$(echo "$devbox_json" | jq -r '.status.commitRecords // {}')
    if [[ "$commit_records" == "{}" ]] || [[ "$commit_records" == "null" ]]; then
        log_verbose "SKIP: Devbox $devbox_namespace/$devbox_name has no commit records"
        return 2  # skipped
    fi

    local commit_record
    commit_record=$(echo "$commit_records" | jq -r ".[\"$content_id\"] // {}")
    if [[ "$commit_record" == "{}" ]] || [[ "$commit_record" == "null" ]]; then
        log_verbose "SKIP: Devbox $devbox_namespace/$devbox_name has no commit record for contentID $content_id"
        return 2  # skipped
    fi

    # 获取 base image
    local base_image
    base_image=$(echo "$commit_record" | jq -r '.baseImage // ""')
    if [[ -z "$base_image" ]]; then
        log_verbose "SKIP: Devbox $devbox_namespace/$devbox_name has no baseImage in commit record"
        return 2  # skipped
    fi

    # 处理 devbox
    log_info "Processing devbox $devbox_namespace/$devbox_name:"
    log_info "  ContentID: $content_id"
    log_info "  Node: $devbox_node"
    log_info "  BaseImage: $base_image"

    if pin_image "$base_image" "$devbox_name" "$devbox_namespace"; then
        return 0  # success
    else
        return 1  # error
    fi
}

# 主函数
main() {
    parse_args "$@"

    log_info "=========================================="
    log_info "Pin Devbox Base Images Script"
    log_info "=========================================="

    # 检查依赖
    check_dependencies

    # 获取当前节点名
    local current_node
    current_node=$(get_current_node_name)
    log_info "Current node name: $current_node"

    # 配置 kubectl
    setup_kubectl

    # 构建 kubectl 命令
    local kubectl_cmd="kubectl get devboxes"
    if [[ -n "$NAMESPACE" ]]; then
        kubectl_cmd="$kubectl_cmd -n $NAMESPACE"
    else
        kubectl_cmd="$kubectl_cmd --all-namespaces"
    fi
    kubectl_cmd="$kubectl_cmd -o json"

    # 获取所有 devbox
    log_info "Fetching devboxes..."
    local devboxes_json
    if ! devboxes_json=$($kubectl_cmd 2>/dev/null); then
        log_error "Failed to list devboxes. Check kubectl permissions and connectivity."
        exit 1
    fi

    local devbox_count
    devbox_count=$(echo "$devboxes_json" | jq -r '.items | length // 0')
    log_info "Found $devbox_count devbox(es)"

    if [[ "$devbox_count" -eq 0 ]]; then
        log_warn "No devboxes found"
        exit 0
    fi

    # 统计
    local success_count=0
    local skip_count=0
    local error_count=0

    # 处理每个 devbox
    local i=0
    while [[ $i -lt $devbox_count ]]; do
        local devbox_json
        devbox_json=$(echo "$devboxes_json" | jq -r ".items[$i]")

        local devbox_name
        devbox_name=$(echo "$devbox_json" | jq -r '.metadata.name // "unknown"')
        local devbox_namespace
        devbox_namespace=$(echo "$devbox_json" | jq -r '.metadata.namespace // "unknown"')

        log_verbose "Checking devbox $devbox_namespace/$devbox_name ($((i+1))/$devbox_count)..."

        if process_devbox "$devbox_json" "$current_node"; then
            success_count=$((success_count + 1))
        else
            exit_code=$?
            if [[ $exit_code -eq 2 ]]; then
                skip_count=$((skip_count + 1))
            else
                error_count=$((error_count + 1))
            fi
        fi

        i=$((i + 1))
    done

    # 输出统计
    log_info ""
    log_info "=========================================="
    log_info "Summary:"
    log_info "  Successfully processed: $success_count"
    log_info "  Skipped: $skip_count"
    log_info "  Errors: $error_count"
    log_info "=========================================="

    # 如果有错误，返回非零退出码
    if [[ $error_count -gt 0 ]]; then
        exit 1
    fi
}

# 运行主函数
main "$@"
执行命令：
# ./pin-devbox-image.sh --dry-run --verbose

./pin-devbox-image.sh --verbose
Containerd 更新
echo "替换containerd..."
systemctl stop containerd
mv /usr/bin/containerd /usr/bin/containerd.backup

wget -O /usr/bin/containerd 'https://containerd-test.oss-cn-hangzhou.aliyuncs.com/containerd?Expires=1766001869&OSSAccessKeyId=TMP.3Ko96J9mG3wMB2QHxpWQetfdG3dxaJTWkWwXVcniuw6VscjjFQkWW2bK4JD4qf9ce4dD6c9GNQMFFjC3uB1ojjf6aLCch1&Signature=1zFTfcfhoOyPd08htncXSGyjANc%3D'
chmod +x /usr/bin/containerd

# start containerd
systemctl start containerd
Controller 更新
ghcr.io/zllinc/devbox/devbox-control:v1.0.5
其他
可以使用：
ctr -n k8s.io image label ghcr.io/zllinc/devbox-base-expt/node.js-20:v0.0.1-alpha.1-en-us io.cri-containerd.pinned=pinned
来使得image被pin住：

同时可以通过：
ctr -n k8s.io image label ghcr.io/zllinc/devbox-base-expt/node.js-20:v0.0.1-alpha.1-en-us io.cri-containerd.pinned
来unpin一个镜像：

Content Store2
Content Store
存储结构

Content Store GC 逻辑
代码位置: metadata/content.go 第 803-888 行
// garbageCollect removes all contents that are no longer used.
func (cs *contentStore) garbageCollect(ctx context.Context) (d time.Duration, err error) {
    contentSeen := map[string]struct{}{}
    
    if err := cs.db.View(func(tx *bolt.Tx) error {
        v1bkt := tx.Bucket(bucketKeyVersion)
        if v1bkt == nil {
            return nil
        }
        
        // ⭐ 关键：遍历【所有 namespace】
        v1c := v1bkt.Cursor()
        for k, v := v1c.First(); k != nil; k, v = v1c.Next() {
            if v != nil {
                continue
            }
            
            // 收集该 namespace 中被引用的 content
            cbkt := v1bkt.Bucket(k).Bucket(bucketKeyObjectContent)
            if cbkt == nil {
                continue
            }
            bbkt := cbkt.Bucket(bucketKeyObjectBlob)
            if bbkt != nil {
                if err := bbkt.ForEach(func(ck, cv []byte) error {
                    if cv == nil {
                        // ⭐ 将被引用的 content 加入 contentSeen
                        contentSeen[string(ck)] = struct{}{}
                    }
                    return nil
                }); err != nil {
                    return err
                }
            }
        }
        return nil
    }); err != nil {
        return 0, err
    }
    
    // ⭐ 只删除不在 contentSeen 中的 content（即没有任何 namespace 引用的）
    err = cs.Store.Walk(ctx, func(info content.Info) error {
        if _, ok := contentSeen[info.Digest.String()]; !ok {
            if err := cs.Store.Delete(ctx, info.Digest); err != nil {
                return err
            }
            log.G(ctx).WithField("digest", info.Digest).Debug("removed content")
        }
        return nil
    })
    // ...
}
结论 ：Content blob 只有在所有 namespace 都不引用 时才会被删除。
触发链路
Kubelet
kubelet由于磁盘压力，触发gc，而这时候devbox原先的旧pod被删除，导致该pod的镜像被列入kubelet的回收镜像列表中，导致其被删除：

Containerd
containerd在收到删除镜像请求后，执行删除操作，将镜像的bucket删除，并且触发gc：
// RemoveImage removes the image.
// TODO(random-liu): Update CRI to pass image reference instead of ImageSpec. (See
// kubernetes/kubernetes#46255)
// TODO(random-liu): We should change CRI to distinguish image id and image spec.
// Remove the whole image no matter the it's image id or reference. This is the
// semantic defined in CRI now.
func (c *criService) RemoveImage(ctx context.Context, r *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
    span := tracing.SpanFromContext(ctx)
    image, err := c.localResolve(r.GetImage().GetImage())
    if err != nil {
        if errdefs.IsNotFound(err) {
            span.AddEvent(err.Error())
            // return empty without error when image not found.
            return &runtime.RemoveImageResponse{}, nil
        }
        return nil, fmt.Errorf("can not resolve %q locally: %w", r.GetImage().GetImage(), err)
    }
    span.SetAttributes(tracing.Attribute("image.id", image.ID))
    // Remove all image references.
    for i, ref := range image.References {
        var opts []images.DeleteOpt
        if i == len(image.References)-1 {
            // Delete the last image reference synchronously to trigger garbage collection.
            // This is best effort. It is possible that the image reference is deleted by
            // someone else before this point.
            opts = []images.DeleteOpt{images.SynchronousDelete()}
        }
        err = c.client.ImageService().Delete(ctx, ref, opts...)
        if err == nil || errdefs.IsNotFound(err) {
            // Update image store to reflect the newest state in containerd.
            if err := c.imageStore.Update(ctx, ref); err != nil {
                return nil, fmt.Errorf("failed to update image reference %q for %q: %w", ref, image.ID, err)
            }
            continue
        }
        return nil, fmt.Errorf("failed to delete image reference %q for %q: %w", ref, image.ID, err)
    }
    return &runtime.RemoveImageResponse{}, nil
}
func (s *imageStore) Delete(ctx context.Context, name string, opts ...images.DeleteOpt) error {
    namespace, err := namespaces.NamespaceRequired(ctx)
    if err != nil {
        return err
    }

    return update(ctx, s.db, func(tx *bolt.Tx) error {
        bkt := getImagesBucket(tx, namespace)
        if bkt == nil {
            return fmt.Errorf("image %q: %w", name, errdefs.ErrNotFound)
        }

        if err = bkt.DeleteBucket([]byte(name)); err != nil {
            if err == bolt.ErrBucketNotFound {
                err = fmt.Errorf("image %q: %w", name, errdefs.ErrNotFound)
            }
            return err
        }

        atomic.AddUint32(&s.db.dirty, 1)

        return nil
    })
}
后面在gc过程中判断该content是否被所有ns引用，如果没有就将其删除；
Content Store
ctr content ls:
root@sealos-staging-devbox-worker002:~# ctr -n k8s.io content ls | grep 61fe
DIGEST                                                                  SIZE    AGE     LABELS
sha256:61fec91190a0bab34406027bbec43d562218df6e80d22d4735029756f23c7007 317.6kB 2 hours containerd.io/distribution.source.sealos.hub=pause,containerd.io/uncompressed=sha256:e3e5579ddd43c08e4b5c74dc12941a4ef656fab070b1087a1fd5a8a836b71e7d
sha256:8d4106c88ec0bd28001e34c975d65175d994072d65341f62a8ab0754b0fafe10 526B    2 hours containerd.io/distribution.source.sealos.hub=pause,containerd.io/gc.ref.content.config=sha256:e6f1816883972d4be47bd48879a08919b96afcd344132622e4d444987919323c,containerd.io/gc.ref.content.l.0=sha256:61fec91190a0bab34406027bbec43d562218df6e80d22d4735029756f23c7007
root@sealos-staging-devbox-worker002:~# ctr -n k8s.io content ls | head -20

首先，content Info的结构体如下：
type Info struct {
    Digest    digest.Digest           // 61fec911... (文件名)
    Size      int64
    CreatedAt time.Time
    UpdatedAt time.Time
    Labels    map[string]string       // 关键！
}

// Labels 示例:
{
    "containerd.io/uncompressed": "sha256:e3e5579ddd43...",  // Diff ID
    "containerd.io/gc.ref.snapshot.overlayfs": "sha256:xxx"
}
会包含一个Digest和Labels，其中uncompressed标签存储的就是解压后的sha256的值；
现在可以看到第一条digest为61fe，并且其后面的label为e3e，所以这个e3e就是61fe的解压后的值；
而另一个标签：containerd.io/distribution.source.sealos.hub=pause - 表示其来源镜像；
现在来看第二条记录，可以看到其后面的label跟着的也是sealos.hub- pause这个镜像，并且还有config，content这两个label，证明其是一个manifest文件，再看其content刚好指的是第一条记录，证明其就是记录一的manifest！！
Error：
Pod在拉取镜像完镜像，启动容器时需要将镜像解压至对应的snapshotter中，这时候需要根据layer和content store的对应关系，去找到content store里对应layer的压缩，发现找不到所以报错；
Events:
  Type     Reason                  Age                     From     Message
  ----     ------                  ----                    ----     -------
  Warning  FailedCreatePodSandBox  7m43s (x3 over 8m13s)   kubelet  Failed to create pod sandbox: rpc error: code = NotFound desc = failed to create containerd container: error unpacking image: failed to extract layer sha256:e3e5579ddd43c08e4b5c74dc12941a4ef656fab070b1087a1fd5a8a836b71e7d: failed to get reader from content store: content digest sha256:61fec91190a0bab34406027bbec43d562218df6e80d22d4735029756f23c7007: not found
  kubelet  Failed to create pod sandbox: rpc error: code = NotFound desc = failed to create containerd container: error unpacking image: failed to extract layer sha256:e3e5579ddd43c08e4b5c74dc12941a4ef656fab070b1087a1fd5a8a836b71e7d: failed to get reader from content store: content digest sha256:61fec91190a0bab34406027bbec43d562218df6e80d22d4735029756f23c7007: not found
1. kubelet 请求创建 Pod Sandbox
   ↓
2. containerd 开始解包镜像
   ↓
3. 读取 manifest，发现需要提取层 e3e5579d...
   ↓
4. 查找这个层对应的压缩 blob: 61fec911...
   ↓
5. 尝试从 content store 打开文件读取器
   ↓
6. ❌ 文件不存在！
   ↓
7. 返回错误: "failed to get reader from content store: sha256:61fec911..."
   ↓
8. 向上传播: "failed to extract layer sha256:e3e5579d..."
   ↓
9. 继续向上: "error unpacking image"
   ↓
10. 最终: "Failed to create pod sandbox"
1. containerd 如何从 Layer 找到对应的 Blob？
先理解几个概念：
Manifest.Layers = diffID 	// 为解包后的sha256
Rootfs.DiffID = diffID 	// 为解包后的sha256
Content.Info	=	Content Store 元数据	// 即压缩后的sha256
关键在于 两处信息源 + Labels 映射：
信息来源
type Layer struct {
    Diff ocispec.Descriptor
    Blob ocispec.Descriptor
}

// ApplyLayers applies all the layers using the given snapshotter and applier.
// The returned result is a chain id digest representing all the applied layers.
// Layers are applied in order they are given, making the first layer the
// bottom-most layer in the layer chain.
func ApplyLayers(ctx context.Context, layers []Layer, sn snapshots.Snapshotter, a diff.Applier) (digest.Digest, error) {
构建映射过程
func (i *image) getLayers(ctx context.Context, platform platforms.MatchComparer, manifest ocispec.Manifest) ([]rootfs.Layer, error) {
    cs := i.ContentStore()
    diffIDs, err := i.i.RootFS(ctx, cs, platform)
    if err != nil {
        return nil, fmt.Errorf("failed to resolve rootfs: %w", err)
    }

    // parse out the image layers from oci artifact layers
    imageLayers := []ocispec.Descriptor{}
    for _, ociLayer := range manifest.Layers {
        if images.IsLayerType(ociLayer.MediaType) {
            imageLayers = append(imageLayers, ociLayer)
        }
    }
    if len(diffIDs) != len(imageLayers) {
        return nil, errors.New("mismatched image rootfs and manifest layers")
    }
    layers := make([]rootfs.Layer, len(diffIDs))
    for i := range diffIDs {
        layers[i].Diff = ocispec.Descriptor{
            // TODO: derive media type from compressed type
            MediaType: ocispec.MediaTypeImageLayer,
            Digest:    diffIDs[i],
        }
        layers[i].Blob = imageLayers[i]
    }
    return layers, nil
}
映射逻辑：
1. 从 Image Config (RootFS.DiffIDs) 读取解压后的 diff ID 数组
2. 从 Manifest (manifest.Layers) 读取压缩的 blob descriptor 数组
3. 按顺序配对：layers[i].Diff = diffIDs[i], layers[i].Blob = imageLayers[i]
4. 写入 Label：在 unpack 后，将映射关系存储到 content store 的 labels 中
        if unpacked {
            // Set the uncompressed label after the uncompressed
            // digest has been verified through apply.
            cinfo := content.Info{
                Digest: layer.Blob.Digest,
                Labels: map[string]string{
                    labels.LabelUncompressed: layer.Diff.Digest.String(),
                },
            }
            if _, err := cs.Update(ctx, cinfo, "labels."+labels.LabelUncompressed); err != nil {
                return err
            }
        }
反向查找（从 DiffID 找 Blob）
// GetDiffID gets the diff ID of the layer blob descriptor.
func GetDiffID(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (digest.Digest, error) {
    switch desc.MediaType {
    case
        // If the layer is already uncompressed, we can just return its digest
        MediaTypeDockerSchema2Layer,
        ocispec.MediaTypeImageLayer,
        MediaTypeDockerSchema2LayerForeign,
        ocispec.MediaTypeImageLayerNonDistributable: //nolint:staticcheck // deprecated
        return desc.Digest, nil
    }
    info, err := cs.Info(ctx, desc.Digest)
    if err != nil {
        return "", err
    }
    v, ok := info.Labels[labels.LabelUncompressed]
    if ok {
        // Fast path: if the image is already unpacked, we can use the label value
        return digest.Parse(v)
    }

2. 镜像拉取过程发生了什么？
根据代码，镜像拉取分为 Pull（下载） 和 Unpack（解包） 两个阶段：
阶段 1: Pull - 下载到 Content Store
Registry → containerd content store
下载的内容：
● Manifest: 描述镜像结构
● Config: 镜像配置（包含 RootFS.DiffIDs）
● Layers: 压缩的层文件（gzip/zstd）
存储方式：
/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/
├── 61fec911... (压缩的 layer blob)
├── e3e5579d... (另一个 blob，如果已存在的话)
├── 987b553c... (image config)
└── 9bb13890... (manifest)
阶段 2: Unpack - 解包到 Snapshotter
        containerd.WithPullSnapshotter(snapshotter),
        containerd.WithPullUnpack,
        containerd.WithPullLabels(labels),
        containerd.WithMaxConcurrentDownloads(c.config.MaxConcurrentDownloads),
        containerd.WithImageHandler(imageHandler),
        containerd.WithUnpackOpts([]containerd.UnpackOpt{
            containerd.WithUnpackDuplicationSuppressor(c.unpackDuplicationSuppressor),
            containerd.WithUnpackApplyOpts(diff.WithSyncFs(c.config.ImagePullWithSyncFs)),
        }),
    }
Unpack 步骤：
1. 读取 manifest 获取所有 layers
2. 读取 config 获取所有 diffIDs
3. 按顺序处理每个 layer：
  ○ 从 content store 读取压缩的 blob (61fec911...)
  ○ 解压并应用到 snapshotter
  ○ 计算解压后的 diff ID
  ○ 验证 diff ID 是否与 config 中的一致
  ○ 将映射关系写入 labels: blob.digest → diff.digest
完整流程图
┌─────────────┐
│   Registry  │
└──────┬──────┘
       │ 1. Pull
       ↓
┌─────────────────────────────────────────┐
│     Content Store                       │
│  blobs/sha256/61fec911... (compressed)  │ ← Blob Digest
│                                         │
│  metadata (labels):                     │
│    containerd.io/uncompressed=e3e5579d  │ ← 映射关系
└──────┬──────────────────────────────────┘
       │ 2. Unpack & Decompress
       ↓
┌─────────────────────────────────────────┐
│     Snapshotter                         │
│  overlayfs/snapshots/123/               │ ← Diff ID (e3e5579d)
│    (解压后的文件系统层)                  │
└─────────────────────────────────────────┘

3. 压缩和解包后的 SHA256 数据结构
OCI 规范数据结构
Manifest (描述镜像结构):
{
  "config": {
    "digest": "sha256:987b553c...",
    "size": 7648
  },
  "layers": [
    {
      "digest": "sha256:61fec911...",     ← Blob Digest (压缩)
      "size": 27092228,
      "mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip"
    }
  ]
}
Image Config (包含 DiffIDs):
// RootFS describes a layer content addresses
type RootFS struct {
    // Type is the type of the rootfs.
    Type string `json:"type"`

    // DiffIDs is an array of layer content hashes (DiffIDs), in order from bottom-most to top-most.
    DiffIDs []digest.Digest `json:"diff_ids"`
}
{
  "rootfs": {
    "type": "layers",
    "diff_ids": [
      "sha256:e3e5579ddd43...",   ← Diff ID (解压)
      "sha256:223b15010c47..."
    ]
  }
}
containerd 内部数据结构
rootfs.Layer:
type Layer struct {
    Diff ocispec.Descriptor  // 解压后的标识 (e3e5579d...)
    Blob ocispec.Descriptor  // 压缩的标识 (61fec911...)
}
content.Info (Content Store 元数据):
type Info struct {
    Digest    digest.Digest           // 61fec911... (文件名)
    Size      int64
    CreatedAt time.Time
    UpdatedAt time.Time
    Labels    map[string]string       // 关键！
}

// Labels 示例:
{
    "containerd.io/uncompressed": "sha256:e3e5579ddd43...",  // Diff ID
    "containerd.io/gc.ref.snapshot.overlayfs": "sha256:xxx"
}
物理存储示例
# Blob 文件（以压缩 digest 命名）
/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/61fec91190a0bab...

# 元数据（存储在 bolt DB 中）
metadata.db → content.Info {
    Digest: "sha256:61fec911...",
    Labels: {
        "containerd.io/uncompressed": "sha256:e3e5579ddd43..."
    }
}

# Snapshotter 快照（以 chainID 命名，基于 diff ID 计算）
/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/123/

总结
1. Layer → Blob 映射：通过 Image Config 的 DiffIDs 和 Manifest 的 Layers 按顺序配对，再存储到 content.Info.Labels 中
2. 拉取过程：先下载压缩 blob 到 content store，再解压到 snapshotter，解压时验证并建立映射
3. 数据结构：
  ○ Blob Digest (61fec911...): 压缩文件的哈希，用作 content store 的文件名
  ○ Diff ID (e3e5579d...): 解压内容的哈希，存储在 config 和 labels 中
  ○ 映射关系: 通过 containerd.io/uncompressed label 连接两者
在你的错误中，61fec911... 文件丢失了，导致无法读取和解压，从而无法验证 e3e5579d... 这个层！
Sandbox，Pause镜像，以及Pause镜像内置
Sandbox是什么？
sandbox是每个Pod第一个先起起来的容器，Pause 容器的作用是保持 Pod 的命名空间（网络、IPC、PID 等）存活；
每个Pod里面都有一个Sandbox容器，并且所有的业务容器都会带上这个Sandbox容器的ID；
Sandbox容器的ID其实就是Pod ID
和Pause镜像的关系
sandbox就是运行pause镜像的容器；
Pause镜像内置
当kubelet开始创建容器时，就需要一个Pause镜像，当我们需要创建cliume pod（负责网络的pod）时，我们需要一个pause镜像，并且本地没有，那么就需要去拉取这个镜像，而网络还没就绪就拉不了这个镜像，这样就出现了一个“鸡生蛋还是蛋生鸡”的问题；
Kubeadm reset -f
列出所有sandbox容器对应的pod：

然后执行RemoveContainers，将这些sandbox容器以及其他容器删除：

RemoveContainers里调用的主要是StopPodSandbox和RemovePodSandbox：

对应到containerd的cri接口也就是StopPodSandbox和RemovePodSandbox这俩；
Containerd 行为
StopPodSandbox：
会先将当前pod里的所有业务容器停止，这里面直接调用的containerd的内置函数，而不是走cri的Stop函数：
func (c *criService) stopPodSandbox(ctx context.Context, sandbox sandboxstore.Sandbox) error {
    // Use the full sandbox id.
    id := sandbox.ID

    // Stop all containers inside the sandbox. This terminates the container forcibly,
    // and container may still be created, so production should not rely on this behavior.
    // TODO(random-liu): Introduce a state in sandbox to avoid future container creation.
    stop := time.Now()
    containers := c.containerStore.List()
    for _, container := range containers {
        if container.SandboxID != id {
            continue
        }
        // Forcibly stop the container. Do not use `StopContainer`, because it introduces a race
        // if a container is removed after list.
        if err := c.stopContainer(ctx, container, 0); err != nil {
            return fmt.Errorf("failed to stop container %q: %w", container.ID, err)
        }
    }

    if err := c.cleanupSandboxFiles(id, sandbox.Config); err != nil {
        return fmt.Errorf("failed to cleanup sandbox files: %w", err)
    }

    // Only stop sandbox container when it's running or unknown.
    state := sandbox.Status.Get().State
    if state == sandboxstore.StateReady || state == sandboxstore.StateUnknown {
        if err := c.stopSandboxContainer(ctx, sandbox); err != nil {
            return fmt.Errorf("failed to stop sandbox container %q in %q state: %w", id, state, err)
        }
    }
    sandboxRuntimeStopTimer.WithValues(sandbox.RuntimeHandler).UpdateSince(stop)

    err := c.nri.StopPodSandbox(ctx, &sandbox)
    if err != nil {
        log.G(ctx).WithError(err).Errorf("NRI sandbox stop notification failed")
    }

    // Teardown network for sandbox.
    if sandbox.NetNS != nil {
        netStop := time.Now()
        // Use empty netns path if netns is not available. This is defined in:
        // https://github.com/containernetworking/cni/blob/v0.7.0-alpha1/SPEC.md
        if closed, err := sandbox.NetNS.Closed(); err != nil {
            return fmt.Errorf("failed to check network namespace closed: %w", err)
        } else if closed {
            sandbox.NetNSPath = ""
        }
        if sandbox.CNIResult != nil {
            if err := c.teardownPodNetwork(ctx, sandbox); err != nil {
                return fmt.Errorf("failed to destroy network for sandbox %q: %w", id, err)
            }
        }
        if err := sandbox.NetNS.Remove(); err != nil {
            return fmt.Errorf("failed to remove network namespace for sandbox %q: %w", id, err)
        }
        sandboxDeleteNetwork.UpdateSince(netStop)
    }

    log.G(ctx).Infof("TearDown network for sandbox %q successfully", id)

    return nil
}
RemovePodSandbox:
而执行解挂载操作其实是在外层的StopContainer，也就是发送停止容器的cri请求时才会单独调用：
// StopContainer stops a running container with a grace period (i.e., timeout).
func (c *criService) StopContainer(ctx context.Context, r *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
    start := time.Now()
    // Get container config from container store.
    container, err := c.containerStore.Get(r.GetContainerId())
    if err != nil {
        if !errdefs.IsNotFound(err) {
            return nil, fmt.Errorf("an error occurred when try to find container %q: %w", r.GetContainerId(), err)
        }

        // The StopContainer RPC is idempotent, and must not return an error if
        // the container has already been stopped. Ref:
        // https://github.com/kubernetes/cri-api/blob/c20fa40/pkg/apis/runtime/v1/api.proto#L67-L68
        return &runtime.StopContainerResponse{}, nil
    }

    defer c.nri.BlockPluginSync().Unblock()

    if err := c.stopContainer(ctx, container, time.Duration(r.GetTimeout())*time.Second); err != nil {
        return nil, err
    }

    sandbox, err := c.sandboxStore.Get(container.SandboxID)
    if err != nil {
        err = c.nri.StopContainer(ctx, nil, &container)
    } else {
        err = c.nri.StopContainer(ctx, &sandbox, &container)
    }
    if err != nil {
        log.G(ctx).WithError(err).Error("NRI failed to stop container")
    }

    i, err := container.Container.Info(ctx)
    if err != nil {
        return nil, fmt.Errorf("get container info: %w", err)
    }

    containerStopTimer.WithValues(i.Runtime.Name).UpdateSince(start)

    ociRuntime, err := c.getSandboxRuntime(&runtime.PodSandboxConfig{}, sandbox.Metadata.RuntimeHandler)

    if err != nil {
        return nil, fmt.Errorf("failed to get sandbox runtime: %w", err)
    }

    snapshotter := c.runtimeSnapshotter(ctx, ociRuntime)

    fmt.Println("Check snapshotter:", snapshotter)

    // Check if the snapshotter is devbox and update the devbox snapshot
    if snapshotter == "devbox" {
        err = c.client.UpdateDevboxSnapshot(ctx, snapshotter, i.ID, unmountLvm, "true")
        if err != nil {
            fmt.Println("Failed to update devbox snapshot:", err)
        }
    }

    return &runtime.StopContainerResponse{}, nil
}
Containerd不同ns隔离原理

Containerd拉取镜像
拉取镜像的整个流程可以大致看成是：
● 拉取镜像
● 解压镜像
拉取镜像
拉取镜像就是拉取镜像相关文件的过程，拉取后的数据是存放到Content Store里：
Registry → containerd content store
下载的内容：
● Manifest: 描述镜像结构
● Config: 镜像配置（包含 RootFS.DiffIDs）
● Layers: 压缩的层文件（gzip/zstd）
存储方式：
/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/
├── 61fec911... (压缩的 layer blob)
├── e3e5579d... (另一个 blob，如果已存在的话)
├── 987b553c... (image config)
└── 9bb13890... (manifest)
Unpack
Unpack - 解包到 Snapshotter
containerd.WithPullSnapshotter(snapshotter),
containerd.WithPullUnpack,
containerd.WithPullLabels(labels),
containerd.WithMaxConcurrentDownloads(c.config.MaxConcurrentDownloads),
containerd.WithImageHandler(imageHandler),
containerd.WithUnpackOpts([]containerd.UnpackOpt{
    containerd.WithUnpackDuplicationSuppressor(c.unpackDuplicationSuppressor),
    containerd.WithUnpackApplyOpts(diff.WithSyncFs(c.config.ImagePullWithSyncFs)),
}),
}
Unpack 步骤：
1. 读取 manifest 获取所有 layers （压缩）
2. 读取 config 获取所有 diffIDs （解压）
3. 按顺序处理每个 layer：
  ○ 从 content store 读取压缩的 blob (61fec911...)
  ○ 解压并应用到 snapshotter
  ○ 计算解压后的 diff ID 
  ○ 验证 diff ID 是否与 config 中的一致
  ○ 将映射关系写入 labels: blob.digest → diff.digest
完整流程图
┌─────────────┐
│   Registry  │
└──────┬──────┘
       │ 1. Pull
       ↓
┌─────────────────────────────────────────┐
│     Content Store                       │
│  blobs/sha256/61fec911... (compressed)  │ ← Blob Digest
│                                         │
│  metadata (labels):                     │
│    containerd.io/uncompressed=e3e5579d  │ ← 映射关系
└──────┬──────────────────────────────────┘
       │ 2. Unpack & Decompress
       ↓
┌─────────────────────────────────────────┐
│     Snapshotter                         │
│  overlayfs/snapshots/123/               │ ← Diff ID (e3e5579d)
│    (解压后的文件系统层)                  │
└─────────────────────────────────────────┘

不同ns之间镜像
场景：
Devbox刚启动时，会拉取base Image到k8s.io下，当devbox需要commit时，而Commit使用的ns在sealos.io下，那么镜像重新拉取时会做什么呢？
details：
这里需要说一下Containerd不同ns之间的隔离原理，可以大致理解为通过每个ns一个bucket来进行划分，也即是镜像元数据和内容元数据这些都是通过不同的ns进行划分的；
在Containerd看来，镜像由三部分组成：
1. Content Blobs（内容数据）：
● 存储在 /var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/
● 包括：manifest、config、layer tar.gz 文件
● 所有命名空间共享同一份物理文件（基于 content-addressable storage）
2. Image Metadata（镜像元数据）：
● 存储在 BoltDB 的 bucketKeyVersion -> [namespace] -> bucketKeyObjectImages
● 包括：镜像名称、Target Descriptor、Labels
● 每个命名空间独立存储
3. Content Info（内容元数据）：
● 存储在 BoltDB 的 bucketKeyVersion -> [namespace] -> bucketKeyObjectContent -> bucketKeyObjectBlob
● 包括：blob 的 digest、size、labels、创建时间
● 每个命名空间独立存储（即使 blob 物理文件共享）
● 用来映射Image Metadata和Content Blobs
那么当我们在k8s.io拉取了一份数据之后，其Content Blob以及Unpack后的Snapshot其实是已经存在于磁盘中，并且这些数据是共享的，也就是当我们需要再次拉取镜像到Sealos.io中时，就只需要通过网络到Registry中重新拉取一份元数据存到当前sealos.io下的bucket中即可；
总结
1. Layer → Blob 映射：通过 Image Config 的 DiffIDs 和 Manifest 的 Layers 按顺序配对，再存储到 content.Info.Labels 中
2. 拉取过程：先下载压缩 blob 到 content store，再解压到 snapshotter，解压时验证并建立映射
3. 数据结构：
  ○ Blob Digest (61fec911...): 压缩文件的哈希，用作 content store 的文件名
  ○ Diff ID (e3e5579d...): 解压内容的哈希，存储在 config 和 labels 中
  ○ 映射关系: 通过 containerd.io/uncompressed label 连接两者
OCI Image 规范
Config.DiffIDs和Manifest.Layers，这俩digest有何不同？
Manifest.layers是压缩后的tar+gzip文件后的sha256的值，而Config.DiffIDs里的sha256值就是解压后的文件的sha256；
这里的sha256只是一个hash的校验方法，跟我们平时用的md5sum是一个用法；