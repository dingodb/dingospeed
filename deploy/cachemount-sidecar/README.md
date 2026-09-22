# cachemount Sidecar 启动包

本服务只做一件事：把原始缓存仓库展示成正常模型文件目录。它不启动 Spinfield，也不选择或启动推理服务。

## 交付内容

- `Dockerfile`：构建 cachemount 镜像。
- `config.env`：只配置原始缓存根目录和模型正文根目录。
- `README.md`：本文。
- `source/`：构建必需的程序源码和许可证，无需修改。

## 配置与构建

修改 `config.env` 中两个容器内路径即可，目录位置完全由接收方决定：

```env
DINGO_CACHE_ROOT=/cache-input
DINGO_MODEL_MOUNT=/model-view/models
```

输入根目录必须包含完整仓库元数据与文件（例如 api/、files/），不是只有 blob 的目录。输入与输出不能重叠。正文挂载在专用共享卷的空子目录，不能覆盖已有数据。

在本目录构建镜像：

```sh
docker build -t 你们的镜像地址:版本 .
```

镜像地址写在接收方自己的 Pod 配置中。把 config.env 的两项作为 sidecar 环境变量注入即可（例如用 ConfigMap 的 envFrom；Docker 可用 --env-file config.env）。文件不会自动被 Kubernetes 加载。

## 消费程序如何读取

启动后，服务自动发现原始仓库中的 HF 和上传仓库版本，并建立目录树，不需要配置 namespace、repo 或 revision。

```text
正文根目录/
  namespace/
    仓库名/
      revision/
        config.json
        模型权重文件
        其他仓库文件
```

接收方自己选择仓库与版本，将以下路径传给推理程序：

```text
source_file_path = 正文根目录/namespace/仓库名/revision
```

例如输出根目录选为 `/dingofs/data2/userdata/dingo-models`，就读取：

```text
/dingofs/data2/userdata/dingo-models/datacanvas/hf-tiny-gpt2/main
```

仓库名可以包含多级路径（如 sshleifer/tiny-gpt2），应完整保留。文件名与版本清单一致，读取返回原始正文，不包含 dingocache 头。目录只读；模型本身的格式和依赖仍需被推理框架支持。

## 部署方需做的挂载接线

由接收方把本镜像加入每个读权重 Pod 的 sidecar，无需提供推理镜像、启动命令或模型选择给本服务。

1. 把原始仓库卷只读挂到 DINGO_CACHE_ROOT；确保 Pod 调度到任何目标节点都能读取这份数据。
2. 准备 Pod 专用共享卷（推荐内存型 emptyDir），例如在 sidecar 挂到 `/model-view`，cachemount 在它的子目录 `/model-view/models` 建立 FUSE 挂载。这个卷不存储权重副本。
3. 同一卷挂给推理容器。sidecar 设置 mountPropagation: Bidirectional，推理容器设置 HostToContainer。仅仅共享卷或同 Pod 不会自动共享 FUSE 子挂载。
4. sidecar 需要 /dev/fuse 和 privileged；两个容器使用相同 UID（镜像默认 65532），共享卷父目录需允许该 UID 创建目录。当前不启用 allow_other；变更 UID 须同步镜像用户记录与权限。
5. 先等挂载就绪，再加载模型。sidecar 就绪检查命令为 `python3 /opt/cachemount/sidecar.py check`；推理容器也要确认其模型路径确实可读。

两边共享卷可以挂在不同路径。消费程序使用的是推理容器内对应的正文路径，不一定与 sidecar 内的 DINGO_MODEL_MOUNT 字符串相同。挂载父目录应专用，不要覆盖其他正在使用的目录。

集群需允许上述 FUSE 权限和挂载传播。可用原生 sidecar 加启动探针安排启动顺序；具体 Pod/Deployment/worker 模板由接收方维护。

## 使用边界与验收

- 目录清单在启动时确定。新增仓库或更新版本后重建 Pod；当前不持续监听目录变化。
- 缺块或损坏版本不可读，日志会报告原因；就绪不代表每个版本都完整。没有可用版本时服务启动失败。
- cachemount 必须持续运行。发生故障时重建整个 Pod 并重新加载模型；sidecar 重启不保证推理进程已有的文件描述符或 mmap 恢复。
- 挂载期间原始缓存不能被原地修改或截断；当前没有与 Speed 写入/清理租约联动。该目录会暴露输入仓库中的可用版本，部署方应做好访问授权与卷隔离。
- 正式验收必须从推理容器中列目录、读取配置并加载模型。不能仅用 sidecar 自己能读作为跨容器通过的依据。

本包仍为未压缩目录。此前已验证独立容器的 FUSE 正文读取；Kubernetes 挂载传播、DingoFS 和实际推理部署需要接收方在目标环境验收。
