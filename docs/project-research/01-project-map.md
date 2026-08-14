# 01. 项目地图

> 阅读路径：← [首页](README.md) · [下一步](02-mainline.md)

## 与本方案相关的职责

| 边界 | 当前职责 | 数据所有权 | 证据 |
|---|---|---|---|
| spinfield api-only | 扫描本地目录、逐文件算 SHA-256、流式 stage、汇总 manifest 后 publish | 内存任务状态、源文件只读句柄 | [Service.runTask](../../../../spinfield/internal/modelupload/service.go#L204-L290)，`spinfield/internal/modelupload/service.go:204-290` |
| spinfield Dingo client | 组装 `size/sha256/defer=true` 的完整文件请求；发布 JSON 清单 | 不持久化上传状态 | [HTTPDingoClient.StageFile](../../../../spinfield/internal/modelupload/client.go#L113-L145)，`spinfield/internal/modelupload/client.go:113-145` |
| dingospeed upload handler/service | token 优先、参数解析、路径/大小校验、并发限流、错误码映射 | 请求级语义 | [UploadWholeFile](../../internal/service/upload_service.go#L30-L72)，`internal/service/upload_service.go:30-72` |
| dingospeed UploadDao | blob 互斥、暂存、整文件哈希、原子改名、manifest/commit 发布 | staged blob、final blob、manifest、revision | [materializeBlob](../../internal/dao/upload_dao.go#L459-L558)，`internal/dao/upload_dao.go:459-558` |
| DingCache | 固定块大小、块位图、随机块写入 | 单个物理缓存文件 | [WriteBlock](../../internal/downloader/file.go#L271-L310)，`internal/downloader/file.go:271-310` |

## 当前入口与隔离边界

- **[已验证]** 上传服务使用独立 Echo 与默认 8091 端口，不与下载 HTTP 入口共用 engine，见 [UploadServer](../../internal/server/upload.go#L18-L70)，`internal/server/upload.go:18-70`。
- **[已验证]** 当前只注册 progress、whole-file upload、publish 三个路由，见 [UploadRouter.initRouter](../../internal/router/upload_router.go#L24-L28)，`internal/router/upload_router.go:24-28`。
- **[已验证]** `blockSize` 是下载/缓存格式配置，默认 8 MiB；上传另有请求并发上限和发布文件数上限，见 [Config](../../pkg/config/config.go#L76-L80)，[Upload config](../../pkg/config/config.go#L168-L196)。

## 数据所有权

blob 路径由 `repoType + orgRepo + whole-file sha256` 决定，与仓库内路径和 revision 无关。chunk 协议必须保持这一事实，否则 publish 的现有内容校验与去重语义会被破坏。

## 暂不展开

下载回源、scheduler、多节点缓存同步、磁盘清理策略的普通路径不属于本次主线；只在它们可能读取或删除 staged/final blob 时作为兼容性测试覆盖。
