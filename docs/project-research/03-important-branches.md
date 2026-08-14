# 03. 重要支线

> 阅读路径：← [主线](02-mainline.md) · [首页](README.md) · [下一步](04-key-code.md)

## 1. 兼容性与失败隔离（最高优先级）

不要改变现有三个路由的默认语义。新增路由仅在 `upload.chunk.enabled=true` 时注册；配置缺失默认 false。旧 client、旧 curl、下载端、publish 均不感知 chunk。

feature flag 只能隔离**运行时行为**，不能隔离编译错误。因此施工必须同时使用：

1. 独立 Git 分支或 worktree；开发过程中不替换当前可运行目录/二进制。
2. 新 DTO、校验器、DAO 方法先做成无调用代码并单测。
3. 新 handler/router 最后接线；wire 生成文件也最后更新。
4. 旧测试 + 新测试全部通过才允许合并/替换二进制。
5. 部署时先发 `enabled=false`，验证旧接口；再灰度开启 8091 chunk 路由；异常时关开关并重启/回滚二进制。

## 2. 幂等与覆盖

当前 final blob 由整文件 SHA 寻址，完整则直接复用，见 [materializeBlob fast path](../../internal/dao/upload_dao.go#L468-L476)。chunk 也应保持：

| 状态 | 默认行为 | `force=true` |
|---|---|---|
| final blob 已完整 | 返回 `blob_reused`，不读写块 | 仍不允许覆盖 |
| staged 块不存在 | 校验 chunk hash 后写入 | 同左 |
| staged 块存在且 hash 相同 | `already_exists`，不写 | 可定义为仍不写 |
| staged 块存在但 hash 不同 | 409 `UPLOAD_CHUNK_CONFLICT` | 仅覆盖该 staged 块 |

如果只检查块位图并无条件跳过，调用方传错块、磁盘静默损坏或旧实现残留都会被伪装成成功，最终只能在 complete 的整文件 hash 阶段失败，定位困难。

## 3. 参数校验

后端至少校验：token；安全仓库/文件路径；整文件 size/SHA；`offset >= 0`；非末块 `offset % serverBlockSize == 0`；`chunkSize == min(blockSize, size-offset)`；`Content-Length == chunkSize`；请求体无多/少字节；chunk SHA；最大块数不超过位图能力。

不要让客户端决定物理块大小。首次创建 staged blob 时使用服务端配置；续传既有 staged blob 时以文件头的 blockSize 为准，并在响应中返回该值。否则服务端修改配置后，旧暂存任务会永久无法续传。

## 4. 并发与锁

- 同一 whole-file SHA 的 chunk 写入需共用 blob 锁；不同 SHA 可并行。
- 同一块同时提交时，一个写，另一个随后校验并返回 already_exists。
- complete 必须持有同一 blob 锁，避免边写边 hash/rename。
- 当前锁是进程内 keyed mutex；若未来同一仓库目录由多个 dingospeed 进程写入，需要升级为文件锁/分布式锁，不能宣称当前设计天然支持多 writer。

## 5. 性能与资源

现有 `WriteBlock` 每块写入后会回写完整块位图 header，见 [WriteBlock](../../internal/downloader/file.go#L300-L306)。分块 HTTP 不会消除该写放大；块越小，请求数、header 回写次数、鉴权与日志开销越高。默认沿用 8 MiB 是合理起点，但需要基准测试 8/16/32/64 MiB 后再定。

