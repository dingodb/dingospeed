# 统一模型目录：使用与用户验收

本文说明统一模型目录的挂载方式及可重复验证步骤。验收报告由使用者在自己的测试环境中生成。

## 约定和实现

```
source_file_path = /mnt/dingo-models/{namespace}/{完整仓库名}/{revision}
```

读取 `source_file_path/config.json`、权重分片或子目录文件，得到原始正文。namespace、完整仓库名、revision 保留原名；内部 SHA 和 blob 路径不出现在用户目录中。上传个人/公司 namespace 沿用 repository.json 的身份，旧上传兼容 dingo-local。HF 为 huggingface。

支持两种模式：`-tree-root` 自动发现一个本地缓存根的 HF/上传仓库版本；`-tree-config` 明确选择要提供的版本，支持多个缓存根。配置是选择仓库版本，不是手工填写每个文件的映射。

本次只提供 models，不包含 ModelScope、远程节点取回或同步。目录反映**挂载时固定的本地仓库清单**，不是不断跟随更新的 main。仓库页面应选择同一节点、同一版本，并在验收期间保持不变。使用结束后卸载、重新启动可取得新版本。已有授权规则不由本地 FUSE 替代：运行工具的管理员决定暴露范围，默认仅挂载用户访问，不启用 allow_other。

如果某个已发现版本缺文件、缺块或清单不可用：版本目录仍在，进入/读取返回 EIO；日志和映射报告写明原因。绝不显示成一个看似正常的部分目录。身份格式错误、版本路径有歧义或无法安全发现仓库则整个启动失败。合法空版本保留空目录。

```mermaid
flowchart LR
  A[HF缓存元数据] --> C[版本解析与文件清单]
  B[上传仓库原始清单] --> C
  C --> D[固定原始路径与内容映射]
  D --> E[/mnt/dingo-models/namespace/repo/revision]
  U[用户控件选择文件夹] --> E
  E --> F[只读 FUSE：正文偏移映射]
  F --> G[已有 DingCache 正文]
```

上传读取与仓库文件树相同的 `dingo-local-manifest.json`，HF 复用现有 hfprojection 完整清单解析。挂载不写回仓库元数据、不迁移格式、不复制权重。保留源文件描述符到卸载，可保护 Linux unlink/rename 后的读取；**仍要求内容不被外部原地修改或 truncate，尚未接入全局清理租约**。

统一树模式也允许 HF 历史缓存中的普通文件，但要求清单大小已知且相符。具有 OLAH 魔数但头部损坏的文件不会退化成普通文件暴露。上传文件继续要求合法完整 OLAH。

## 先用看得懂的小样本验收

在有 FUSE 的 Linux/WSL 中，进入 dingospeed 源码目录。使用一个新的验收目录；报告也不覆盖旧文件。

```sh
CGO_ENABLED=0 go build -o ./cachemount ./cmd/cachemount
python3 test/cachemount/accept_tree.py demo --work "$HOME/dingo-tree-demo"
```

样本含两个完整模型目录和一个故意缺块的版本；有嵌套文件路径，并另外保存一份原始文件用于独立对照。权重是小型合成字节，不是可训练模型。

首次使用时，请管理员创建空目录 `/mnt/dingo-models` 并授权当前挂载用户使用。目录若非空，程序拒绝挂载。然后在终端 A 运行：

```sh
./cachemount -tree-config "$HOME/dingo-tree-demo/selection.json" \
  -report "$HOME/dingo-tree-demo/mapping.json" /mnt/dingo-models
```

保持终端 A 运行。在终端 B 亲自看和读：

```sh
find /mnt/dingo-models/huggingface/demo/HFModel/main -type f
find /mnt/dingo-models/datacanvas/team/UploadModel/v1 -type f
cat /mnt/dingo-models/datacanvas/team/UploadModel/v1/config.json
cmp "$HOME/dingo-tree-demo/originals/datacanvas/team/UploadModel/v1/model.safetensors" \
    /mnt/dingo-models/datacanvas/team/UploadModel/v1/model.safetensors
ls /mnt/dingo-models/datacanvas/team/Incomplete/v1
```

你应看到：两个正常目录的三份文件均保留原名和子目录；cat 输出可读 JSON；cmp 无输出且退出码为 0；最后一个故意缺块的目录明确报 I/O 错误，不能正常进入空目录。

生成浏览器可打开的现场对照报告：

```sh
python3 test/cachemount/accept_tree.py check \
  --report "$HOME/dingo-tree-demo/mapping.json" \
  --originals "$HOME/dingo-tree-demo/originals" \
  --output "$HOME/dingo-tree-demo/acceptance.html" --full
```

打开 acceptance.html。每个版本左右并排显示“仓库预期清单”和“实际目录”，逐文件列出大小和读取结果；配置文件展示内部 OLAH 头的十六进制，以及用户实际读到的 JSON。原始样本对照使用独立文件，不是把挂载结果自己和自己比较。

**认可条件：路径和文件数一致、缺失/额外均为空、原始样本内容一致、配置是正常 JSON、缺块目录被明确拒绝。任何一项不满足都不验收。** 这份报告只验证文件系统映射，不声称真实模型训练通过。

## 再用你自己的仓库验收

先停止读者，卸载上述样本：

```sh
fusermount3 -u /mnt/dingo-models
```

自动发现真实仓库（将 `/absolute/repos` 替换为实际缓存根目录，并使用新报告文件）：

```sh
./cachemount -tree-root /absolute/repos \
  -report "$HOME/dingo-real-mapping.json" /mnt/dingo-models
```

终端日志会打印每个实际 `source_file_path` 及 ready/unavailable。也可用 `-tree-config` 只选择你在仓库页面看到的版本：

```json
[
  {"cache_root":"/absolute/repos","namespace":"huggingface","repo":"Qwen/demo","revision":"main"},
  {"cache_root":"/absolute/repos","namespace":"datacanvas","repo":"team/model","revision":"v1"}
]
```

在仓库页面选择相同节点及版本，逐项对照文件夹的原名、层级和大小，再在对方真实控件里选择打印的目录，使用其现有模型库加载。

```sh
python3 test/cachemount/accept_tree.py check --report "$HOME/dingo-real-mapping.json" \
  --output "$HOME/dingo-real-acceptance.html"
```

默认小文件全量比较，大于 16 MiB 的文件只比较头/中/尾，并明确标注“抽样，未全量”。要对大权重完整验收，增加 `--full`，将实际读取所有内容；有 64 位内容摘要时同时比对 SHA256，不要把默认抽样当成全量正确性证明。真实验收不要传 `--originals`，该参数只用于小型独立样本。

报告的预期树来自挂载时固定清单，不能代替与实际仓库页面的人工对照。最后一项验收必须是对方原有控件与模型加载库成功使用该目录。若加载进程在容器内，应先将此挂载目录暴露到容器中；服务器存在路径不等于其他机器也有这个路径。
