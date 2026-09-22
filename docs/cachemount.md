# DingCache 训练目录只读挂载

统一目录树模式及可重复验证步骤见 [统一模型目录：使用与用户验收](model-tree-acceptance.md)。下文的单目录入口继续保留。

消费者容器的启动、健康检查及模型目录接入见 [消费者启动适配](../deploy/cachemount/README.md)。

独立 Linux FUSE 工具，将现有 OLAH v8 缓存文件的正文展示成普通模型文件。不修改缓存格式，不启动 Speed/Scheduler，不回源下载，不生成权重副本。训练进程使用挂载目录作为本地模型路径。

## 构建及使用

需要 Linux、可访问的 `/dev/fuse` 和 `fusermount3`。在 dingospeed 仓库运行：

```sh
CGO_ENABLED=0 go build -o cachemount ./cmd/cachemount
mkdir -p /path/to/empty-mount
./cachemount -cache-root /absolute/existing/cache -repo owner/model -revision main /path/to/empty-mount
```

HF 模式使用已有缓存元数据，固定启动时的清单和 commit。要求元数据包含完整 siblings 清单，并且所有文件已完整缓存为 OLAH v8。缺失、缺块、损坏、清单大小不符或目录冲突均拒绝整个挂载，不隐藏失败文件。尚不支持历史普通文件与 OLAH 混合目录。

其他来源可提供管理员准备的 JSON 清单；不要求调整原目录结构：

```json
{
  "config.json": "/absolute/cache/config-blob",
  "model-00001-of-00002.safetensors": "/absolute/cache/weight-blob-1",
  "model-00002-of-00002.safetensors": "/absolute/cache/weight-blob-2"
}
```

```sh
./cachemount -manifest /absolute/files.json /path/to/empty-mount
```

清单键是训练侧相对路径，值是已有缓存文件绝对路径。清单由可信本地管理员提供；它不是面向任意用户的鉴权接口。工具默认只允许挂载用户访问，不启用 `allow_other`。跨用户、容器及分布式训练的挂载传播需要单独配置和验证。

训练完成并释放所有 mmap/文件引用后，另一个终端执行：

```sh
fusermount3 -u /path/to/empty-mount
```

也可以向挂载进程发送 SIGINT/SIGTERM；忙碌时保留服务并报告错误，释放读者后可再次发送信号。checkpoint 和训练输出必须写到另一个可写目录。

## 数据及生命周期

- 挂载前只读取文件头和分块位图，检查完整性和物理长度；不全量读取权重，也不在启动时重算 SHA256。
- 随机读映射为 `缓存头长度 + 逻辑偏移`。支持正常文件读取和只读 mmap，文件大小对外排除缓存头。
- 所有源文件描述符保留到卸载。Linux 下源文件被 unlink、rename 或同路径替换后，挂载仍读取原 inode；底层空间在描述符关闭后才能释放。
- **源文件必须在挂载期间保持内容不变。** 保留描述符不防止其他进程原地写入、truncate 或打洞。本工具没有接入 Speed 的写入/清理租约；不能宣称对任意原地修改提供快照隔离。读取遇到截断返回 EIO，但已缓存页可能仍可读取。
- 已完成权重应按内容身份管理。挂载不会自动跟随分支更新；更新模型需要结束读者并重新挂载。
- 无持久化权重副本；内核可能同时缓存底层和 FUSE 页，仍会占用内存。每个清单文件占用一个源描述符，大量分片需检查 `ulimit -n`。
- 首版采用用户态读取转发，尚未调优大模型吞吐。FUSE 服务异常退出会导致未完成的读取失败，不能当作高可用存储层。

## 可重复验证

```sh
DINGO_TEST_FUSE=1 CGO_ENABLED=0 go test -v ./pkg/cachemount
CGO_ENABLED=0 go test ./pkg/hfprojection -run TestMountSources
CGO_ENABLED=0 go vet ./pkg/cachemount ./cmd/cachemount
python3 test/cachemount/validate.py --binary /absolute/cachemount --work /absolute/new-test-dir
python3 test/cachemount/validate.py --hf --binary /absolute/cachemount --work /absolute/another-new-test-dir
```

Python 测试只在新目录生成样本，验证摘要、mmap、跨进程读取、只读限制、缓存原内容不变，并记录挂载准备、普通文件与挂载文件的热缓存读取、导出落盘耗时。安装 PyTorch 后会实际执行 mmap 加载、5 步 CPU 训练、checkpoint 保存和恢复；未安装时报告 unavailable，不能算训练通过。

另安装 transformers、safetensors、numpy 后，测试会本地生成随机 tiny GPT2，保存为 safetensors，经挂载目录离线 `from_pretrained` 加载，比较原始目录与挂载目录输出，再训练 3 步、保存恢复并继续训练。无需下载上游模型。

Go 测试包含真实内核挂载、并发读取、缺块和损坏拒绝、目录冲突、截断错误、删除后读取，以及 512 GiB **逻辑大小的稀疏文件**头部准备测试。稀疏文件仅证明启动不扫描正文，不代表读取了 512 GiB 真实权重。

测试报告写入上述命令指定的新测试目录。小型模型和合成样本的结果不能代表实际大模型、GPU、多节点或长期运行情况；热缓存测试不代表磁盘性能，不能按样本线性外推大模型耗时。

### 已有大文件只读验证

`test/cachemount/validate_existing.py` 对现有完整 OLAH 文件执行两遍全量 SHA256：直接读取正文、经 FUSE 读取，均与仓库清单摘要比较；随后验证随机读取、跨块/末尾读取、mmap 及四线程读取。源文件只读打开，不复制正文、不清空系统页缓存，所有输出写入新的测试目录。

```sh
python3 -u test/cachemount/validate_existing.py \
  --binary /absolute/cachemount \
  --source /absolute/repos/files/models/owner/repo/blobs/HASH \
  --expected-sha256 HASH \
  --work /absolute/new-validation-dir
```

报告分别记录总读校验时间与 read 调用累计时间。后者排除了用户态摘要计算时间，但仍受缓存、调度及存储路径影响；顺序为先直读后挂载，不是受控冷缓存对照。Windows 盘经 WSL DrvFS/9p 访问的结果不能当作原生 Linux NVMe 吞吐。
