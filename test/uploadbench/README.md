# uploadbench —— 本地单文件上传接口压测工具

针对 `POST /api/local-upload/:repoType/:org/:repo/:revision/*` 的压测客户端与实验脚本。
结论见 [`docs/upload-load-test-report.md`](../../docs/upload-load-test-report.md)。

## 构建

```bash
go build -o bin/dingospeed-test.exe ./cmd
go build -o test/uploadbench/uploadbench.exe ./test/uploadbench
```

`test/uploadbench` 是独立 module，不会被主 module 的 `go build ./...` 编译进去。

## 跑全套实验

```bash
pwsh -File test/uploadbench/run-all.ps1
```

实验服务跑在 `18090/18091/16060`，避开开发机上可能已在运行的正式实例；数据落在
`D:\dingospeed-bench\repos`，每个场景开跑前会清空。单个场景：

```bash
pwsh -File test/uploadbench/run-scenario.ps1 -Label demo -Limit 4 -BenchArgs "-scenario closed -c 8 -n 64 -size 8388608 -repo demo"
```

结果写在 `test/uploadbench/runs/<Label>/`：`result.json`（含服务端 CPU / 读写字节 / 峰值内存）、
`samples.jsonl`（逐请求样本）、`server.out|err`、`config.yaml`。
`pwsh -File test/uploadbench/summarize.ps1` 汇总成一张 CSV 表。

## 场景

| `-scenario` | 含义 |
|---|---|
| `closed` | 闭环压测，`-c` 个客户端各自循环上传互不相同的文件，共 `-n` 个；`-retry429` 打开后 429 会退避重试，模拟真实上传工具 |
| `dataset` | 把 `-n` 个文件写进同一个 repo/revision，观察第 k 个文件的耗时随 k 的变化；`-shardRepos N` 把文件散到 N 个仓库作为对照组 |
| `idem` | 反复上传同一个已存在的文件，测幂等快路径 |
| `slowloris` | `-holders` 条限速为 `-holdRate` 字节/秒的连接占住并发槽，同时用正常速度的探测请求看还能不能上传 |

请求体由种子确定性生成，不落客户端磁盘，`sha256` 预先算好后带在 query 里，
所以测到的读写放大只来自服务端。

## 采数说明

`serverWriteMB` / `serverReadMB` 取自 `Win32_Process` 的 `WriteTransferCount` /
`ReadTransferCount`，是**逻辑** IO 字节数，不等于落到物理盘的量（页缓存会吸收一部分）。
`peakWorkingSetMB` 是服务进程的峰值工作集。
