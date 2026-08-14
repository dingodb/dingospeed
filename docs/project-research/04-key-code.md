# 04. 关键代码

> 阅读路径：← [重要支线](03-important-branches.md) · [首页](README.md) · [下一步](05-architecture-analysis.md)

| 符号 | 业务职责 | 对分块方案的含义 |
|---|---|---|
| `HTTPDingoClient.StageFile` | api-only 发完整文件 | 新增 `PutChunk/CompleteFile`，旧方法不改 |
| `Service.runTask` | 逐文件 hash、上传、发布 | 仅在显式配置启用时替换单文件传输策略 |
| `UploadService.UploadWholeFile` | 鉴权、校验、限流 | 提取可复用校验器，但保持方法签名/错误码 |
| `UploadDao.materializeBlob` | 暂存到完整 blob | 新 chunk DAO 与其共享路径、锁、final fast path |
| `writeStagedBlob` | 从 start 写到 EOF 并整文件验 hash | 不修改为双重语义；新增 chunk 写方法 |
| `DingCache.WriteBlock` | 随机块写与位图置位 | 可复用，但上层必须先做存在/摘要/force 判定 |
| `PublishFiles` | blob 一次性绑定到 revision | 原样复用，是可见性边界 |

## 关键解读

### `HTTPDingoClient.StageFile`

**[已验证/工作区]** 它设置 `Content-Length=整文件大小`，只接受 HTTP 201 和 `status=staged`。[源码](../../../../spinfield/internal/modelupload/client.go#L113-L145)，`spinfield/internal/modelupload/client.go:113-145`。直接改变它会同时改变现有多文件上传；应增加新方法并由策略选择。

### `Service.runTask`

**[已验证/工作区]** 当前每个文件先完整读取一遍 hash，再重新打开完整读取一遍上传，进度总量因此是源大小的两倍；所有文件 stage 后才 publish。[源码](../../../../spinfield/internal/modelupload/service.go#L204-L290)，`spinfield/internal/modelupload/service.go:204-290`。分块不会自动减少第一次 hash，但可让第二遍传输按块失败重试。

### `UploadDao.materializeBlob` / `writeStagedBlob`

**[已验证]** final blob 完整时直接复用；否则写 `.uploading` 暂存文件，完整 hash 匹配后原子 rename。[源码](../../internal/dao/upload_dao.go#L459-L558)，`internal/dao/upload_dao.go:459-558`。这是 chunk 方案必须保持的提交边界。

### `DingCache.WriteBlock`

**[已验证]** 支持按块索引 Seek 写入，最后块截短落盘，随后设置位图并 flush header。[源码](../../internal/downloader/file.go#L271-L310)，`internal/downloader/file.go:271-310`。物理能力已经存在，新工作主要是 HTTP 协议、校验、锁和完成态编排。

### `PublishFiles`

**[已验证]** 同 revision 并发 publish 明确拒绝；持仓库锁与 revision 锁，验证 blob 后合并 manifest；清单不变则不写元数据。[源码](../../internal/dao/upload_dao.go#L279-L350)，`internal/dao/upload_dao.go:279-350`。chunk 方案不应复制发布逻辑。

## 测试证据

- whole-file 的覆盖、幂等、断点、多次中断、同文件并发与崩溃一致性已在 `internal/dao/upload_dao_test.go` 覆盖。
- batch publish 的不可见中间态、覆盖、并发、续传发布已在 `internal/dao/upload_publish_test.go` 覆盖。
- 2026-08-13 目标包测试全部通过；尚无 chunk 测试或 PoC。

