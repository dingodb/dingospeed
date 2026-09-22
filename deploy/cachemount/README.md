# 消费者启动适配

`consumer_supervisor.py` 与 `healthcheck.py` 用于同容器启动 cachemount 和消费者。默认命令 `/modelfleet`，也可以传入其他消费者命令。

镜像需包含 Linux cachemount 二进制 `/usr/local/bin/cachemount`、Python 3、fuse3，以及在 passwd/group 中登记的消费者 UID/GID。挂载目录必须为空且归该用户所有；容器需访问 `/dev/fuse` 并允许 FUSE 挂载。容器运行时需按其安全策略配置 FUSE 所需权限。

环境变量：`DINGO_CACHE_ROOT` 默认 `/repos`，`DINGO_MODEL_MOUNT` 默认 `/mnt/dingo-models`，`DINGO_MOUNT_STATE` 默认 `/run/cachemount`。当前 Spinfield 健康检查按默认状态目录和 localhost:8082 配置；更改状态目录时应同时适配健康检查。

```text
python3 /opt/cachemount/consumer_supervisor.py /modelfleet
```

只读挂入原始 repos。挂载和应用必须使用同一个 UID、同一挂载命名空间。应用就绪应同时满足挂载和自身 HTTP 健康；helper 失败会停止应用，以便容器编排系统重新启动整个容器。

模型目录格式为 `/mnt/dingo-models/{namespace}/{完整仓库名}/{revision}`，可作为模型加载库的目录参数。完整仓库名可以包含 `/`，不能只保留最后一级名称。

如果实际模型加载进程运行在另一个容器、Pod 或节点，需在该消费者中启动 helper，或将节点上已挂载的正文目录挂入消费者；仅传递路径字符串不能让其他容器访问目录。挂载固定启动时的清单，更新版本需停止读者并重新挂载，期间源文件不能被原地修改。

构建及可重复验证步骤见 [只读挂载说明](../../docs/cachemount.md) 和 [统一模型目录](../../docs/model-tree-acceptance.md)。
