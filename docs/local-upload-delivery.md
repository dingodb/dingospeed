# 自研模型上传功能交付说明

| 项目 | 内容 |
|---|---|
| 日期 | 2026-08-12 |
| 覆盖范围 | `model-upload-requirements.md` §10 交付物与环 4 收尾；`batch-upload-requirements.md` 环 5~环 6 批量发布 |
| 下载通道 | HuggingFace 兼容通道 |
| 上传命名空间 | `dingo-local` |

## 1. 接口

上传接口独立监听 `upload.host:upload.port`，默认只绑定 `127.0.0.1:8091`。

### 1.1 完整上传

```bash
TOKEN='replace-with-upload-token'
FILE='config.json'
SIZE=$(wc -c < "$FILE")
SHA256=$(sha256sum "$FILE" | awk '{print $1}')

curl -sS -X POST \
  -H "X-Dingo-Upload-Token: ${TOKEN}" \
  --data-binary "@${FILE}" \
  "http://127.0.0.1:8091/api/local-upload/models/dingo-local/demo/main/config.json?size=${SIZE}&sha256=${SHA256}"
```

成功响应示例：

```json
{
  "repoType": "models",
  "repo": "dingo-local/demo",
  "revision": "main",
  "commit": "8c41f9...",
  "filePath": "config.json",
  "size": 1234,
  "sha256": "64位小写sha256",
  "status": "effective",
  "blobReused": false
}
```

覆盖已有路径的不同内容时，需要显式追加 `overwrite=true`。

### 1.2 续传

先查询当前可续传偏移：

```bash
TOKEN='replace-with-upload-token'
FILE='pytorch_model.bin'
SIZE=$(wc -c < "$FILE")
SHA256=$(sha256sum "$FILE" | awk '{print $1}')

curl -sS \
  -H "X-Dingo-Upload-Token: ${TOKEN}" \
  "http://127.0.0.1:8091/api/local-upload-progress/models/dingo-local/demo/main/pytorch_model.bin?sha256=${SHA256}"
```

再从服务端返回的 `resumeOffset` 继续传：

```bash
OFFSET=8388608
tail -c +$((OFFSET + 1)) "$FILE" | curl -sS -X POST \
  -H "X-Dingo-Upload-Token: ${TOKEN}" \
  --data-binary @- \
  "http://127.0.0.1:8091/api/local-upload/models/dingo-local/demo/main/pytorch_model.bin?size=${SIZE}&sha256=${SHA256}&start=${OFFSET}"
```

`start` 一旦声明就是续传语义，必须严格等于服务端返回的 `resumeOffset`；不声明 `start` 表示完整文件上传。

### 1.3 批量发布（一次让一整批文件同时生效）

发布一个完整模型时用这条路径。它和"逐个上传"的**最终结果完全一样**（快照标识逐字符相同），
区别只在于**什么时候生效**：整批一次生效，中途不会产生任何中间版本。

分两步：先逐个上传但声明 `defer=true`（只落内容、不生效），全部传完后调用一次发布。

```bash
TOKEN='replace-with-upload-token'
BASE='http://127.0.0.1:8091'
REPO='models/dingo-local/demo/main'
SRC_DIR='./my-model'

# 第一步：逐个上传，声明暂缓生效。此阶段下载方看不到任何变化。
ITEMS=''
cd "$SRC_DIR"
for FILE in $(find . -type f | sed 's|^\./||'); do
  SIZE=$(wc -c < "$FILE")
  SHA256=$(sha256sum "$FILE" | awk '{print $1}')
  curl -sS -X POST \
    -H "X-Dingo-Upload-Token: ${TOKEN}" \
    --data-binary "@${FILE}" \
    "${BASE}/api/local-upload/${REPO}/${FILE}?size=${SIZE}&sha256=${SHA256}&defer=true"
  ITEMS="${ITEMS}${ITEMS:+,}{\"path\":\"${FILE}\",\"sha256\":\"${SHA256}\",\"size\":${SIZE}}"
done

# 第二步：一条 curl 完成发布。
curl -sS -X POST \
  -H "X-Dingo-Upload-Token: ${TOKEN}" \
  -H 'Content-Type: application/json' \
  -d "{\"files\":[${ITEMS}]}" \
  "${BASE}/api/local-publish/${REPO}"
```

暂缓生效上传的响应中 `status` 为 `staged`、`commit` 为空串——内容已就绪，等待发布。

发布成功响应示例：

```json
{
  "repoType": "models",
  "repo": "dingo-local/demo",
  "revision": "main",
  "commit": "8c41f9...",
  "published": 12,
  "fileCount": 12,
  "added": 12,
  "replaced": 0,
  "unchanged": 0,
  "changed": true,
  "status": "published"
}
```

- `published` 是本批次声明的条目数，`fileCount` 是合并后该版本的文件总数。
- `changed` 为 `false`（`status` 为 `unchanged`）表示这批内容早就全部生效且没有变化，快照标识保持不变。
- **发布是合并语义**：清单里没提到的已生效文件原样保留。要覆盖已存在路径的不同内容，加 `?overwrite=true`，覆盖声明作用于整批。
- 发布清单**由调用方提供**，服务端不记忆"哪些上传属于哪一批"。路径、摘要、大小三样在上传时本来就是必填项，攒清单不增加额外负担。
- 发布是**全有或全无**：任何一条不满足（内容没传完、大小对不上、需要覆盖但没声明、清单里有重复路径），整次拒绝，不产生任何副作用，且错误信息会列出具体路径。

**中断了怎么办**：直接重跑整个脚本。已经传完的文件走幂等快路径（响应 `blobReused: true`）几乎不耗时，
没传完的按 §1.2 续传，最后再发布一次即可。结果与一次跑完完全一致。

### 1.4 什么时候用哪一种

| 场景 | 用法 |
|---|---|
| 发布一个完整模型 / 数据集（多个文件） | **批量发布**（§1.3）。用户不会看到只传了一半的仓库 |
| 单独补一个文件、改一个配置 | 单文件即时生效上传（§1.1）。一条 curl 搞定 |
| 超大权重文件，需要断点续传 | 两者都支持。批量场景下续传行为完全一致 |

> 逐个即时生效上传多个文件仍然可用，行为与以前完全一样，但会逐个产生中间快照标识
> （见 §5 已知限制）。多文件发布建议走批量路径。

## 2. 配置项

| 配置 | 默认值 | 说明 |
|---|---:|---|
| `upload.host` | `127.0.0.1` | 上传接口监听地址；非回环地址会被强制改回回环地址 |
| `upload.port` | `8091` | 上传接口端口 |
| `upload.token` | 空 | 上传凭证；为空时上传接口默认关闭 |
| `upload.namespace` | `dingo-local` | 允许上传的组织名 |
| `upload.concurrentLimit` | `4` | 上传并发上限 |
| `upload.stagingRetentionHours` | `168` | 未完成暂存文件保留时长 |
| `upload.stagingCleanupIntervalMinutes` | `60` | 暂存清理任务执行间隔 |
| `upload.publishMaxFiles` | `1000` | 单次批量发布的清单条目数上限，超出明确拒绝 |
| `upload.orphanRetentionHours` | `168` | **已完整落盘但还没被任何清单引用**的内容的保留时长，超期由同一个清理任务回收 |

> `orphanRetentionHours` 取值必须明显大于一次完整批量上传的正常耗时。一个大模型分批传完
> 再发布，中间可能跨小时甚至跨天（含中断续传），期限太短会把还等着发布的内容删掉。
> 保留期内的内容随时可以正常发布。

## 3. 错误码

| 错误码 | HTTP | 含义 |
|---|---:|---|
| `UPLOAD_DISABLED` | 403 | 服务端未配置上传凭证，上传默认关闭 |
| `UPLOAD_TOKEN_MISSING` | 401 | 请求未携带 `X-Dingo-Upload-Token` |
| `UPLOAD_TOKEN_INVALID` | 403 | 上传凭证错误 |
| `UPLOAD_INVALID_ARGUMENT` | 400 | URL 字段、`size`、`start` 或 `sha256` 参数非法 |
| `UPLOAD_INVALID_CONTENT` | 400 | 请求体长度或完整 SHA-256 校验不通过 |
| `UPLOAD_PATH_ESCAPE` | 400 | 目标路径逃逸缓存根目录 |
| `UPLOAD_PATH_SYMLINK` | 400 | 目标路径父级含符号链接 |
| `UPLOAD_OVERWRITE_REQUIRED` | 409 | 目标路径已有不同内容，需要 `overwrite=true` |
| `UPLOAD_FULL_OVERWRITE_REQUIRED` | 409 | 续传遇到已有不同内容，需改用完整上传并 `overwrite=true` |
| `UPLOAD_FILE_BUSY` | 409 | 同一文件已有上传进行中 |
| `UPLOAD_RESUME_OFFSET_MISMATCH` | 409 | 声明的 `start` 与服务端可续传偏移不一致 |
| `UPLOAD_RESUME_BINDING_MISMATCH` | 409 | 续传声明大小与暂存文件绑定大小不一致 |
| `UPLOAD_CONCURRENCY_LIMIT` | 429 | 超过上传并发上限 |
| `UPLOAD_INTERNAL_ERROR` | 500 | 上传服务内部错误 |

批量发布（§1.3）专有错误码：

| 错误码 | HTTP | 含义 |
|---|---:|---|
| `PUBLISH_INVALID_ARGUMENT` | 400 | 请求体不是合法 JSON、清单为空、条目超上限、路径/摘要/大小非法、清单内有重复路径 |
| `PUBLISH_BODY_TOO_LARGE` | 413 | 发布请求体超过 8MB |
| `PUBLISH_CONTENT_NOT_READY` | 409 | 清单中有文件的内容尚未完整落盘，错误信息列出具体路径 |
| `PUBLISH_CONTENT_MISMATCH` | 409 | 清单声明的大小与已落盘内容不符，错误信息列出具体路径 |
| `PUBLISH_OVERWRITE_REQUIRED` | 409 | 清单中有路径已存在且内容不同，需要 `overwrite=true`，错误信息列出冲突路径 |
| `PUBLISH_IN_PROGRESS` | 409 | 同一版本已有发布进行中 |
| `PUBLISH_INTERNAL_ERROR` | 500 | 发布服务内部错误 |

凭证相关错误码（`UPLOAD_DISABLED` / `UPLOAD_TOKEN_MISSING` / `UPLOAD_TOKEN_INVALID`）
在发布接口上含义一致——两个接口共用同一个凭证。

失败响应格式：

```json
{"code":"UPLOAD_INVALID_CONTENT","error":"sha256 mismatch: ..."}
```

## 4. 验证证据

| 验收项 | 证据 |
|---|---|
| §9.1、§9.2、§9.3、§9.4、§9.5、§9.6 部分、§9.14 公开模型下载回归 | `docs/e2e-verification-ring2.md` |
| §9.14 ModelScope 通道边界 | `TestUploadDatasetsAndRealisticFileNames` 验证上传不写入 `modelscope/` 缓存根；本功能未改动 ModelScope handler/service 路由 |
| §9.7 断点续传核心路径、异常路径、残留回收 | `internal/dao/upload_dao_test.go` |
| §9.8 覆盖与幂等 | `internal/dao/upload_dao_test.go` |
| §9.9 并发 | `internal/dao/upload_dao_test.go`、`internal/service/upload_service_test.go` |
| §9.10 安全与防护 | `internal/service/upload_service_test.go`、`docs/e2e-verification-ring2.md` |
| §9.11 防淘汰 | `internal/service/sys_service.go` 与对应测试 |
| §9.12 崩溃一致性 | `TestUploadCrashConsistencyKeepsOldRevisionUntilPublish` |
| §9.13 1000 文件规模 | `TestUploadMetadataFootprintStaysLinear`，本机实测 1000 文件上传约 10.13 秒 |

### 4.1 批量发布（`batch-upload-requirements.md` 环 5~环 6）

| 验收项 | 证据 |
|---|---|
| §9.1 等价性（快照标识逐字符相同） | `TestPublishIsEquivalentToSequentialUploads`；端到端脚本第 4 段 |
| §9.2 完整性（合并语义、追加不抹旧） | `TestPublishMergesInsteadOfReplacing`、`TestPublishMergesWithImmediateUploads` |
| §9.3 中间态不可见 / 只产生 1 个快照标识 | `TestStagedUploadsAreInvisibleUntilPublish`；端到端脚本第 2~3 段 |
| §9.4 发布前置校验 | `TestPublishRejectsContentThatIsNotReady`、`TestRejectedPublishLeavesEffectiveRevisionUntouched`、`TestValidatePublishParamListShape` |
| §9.5 覆盖与幂等 | `TestPublishOverwriteAndIdempotency` |
| §9.6 原子性与崩溃一致性 | `TestPublishCrashConsistencyKeepsOldRevisionUntilPublish`、`TestInterruptedBatchRerunMatchesUninterruptedRun` |
| §9.7 并发 | `TestConcurrentPublishOfSameRevisionIsRejected`、`TestConcurrentPublishAndImmediateUploadKeepBothFiles`、`TestPublishToDifferentRevisionsIsIndependent` |
| §9.8 续传在暂缓生效路径下不退化 | `TestDeferredUploadSupportsResume`、`TestDeferredResumeWithDifferentContentStillFailsHash` |
| §9.9 待发布内容回收与误删防护 | `internal/dao/upload_reclaim_test.go` |
| §9.10 安全（发布清单的路径逃逸矩阵、凭证） | `internal/service/upload_publish_test.go`；发布接口注册在既有回环监听上，未新增对外监听 |
| §9.11 规模与元数据写入量 | `TestPublishScaleComparedToSequentialUploads` |
| §9.12 回归 | 既有测试断言未修改且全部通过；`TestUploadWithoutDeferStillPublishesImmediately` |
| 端到端（真实进程 + curl，在线/离线各一遍） | [../test/publish-e2e/run-e2e.sh](../test/publish-e2e/run-e2e.sh)，各 40 项检查全部通过 |

### 4.2 元数据写入量实测

同一组文件，两条路径写入的**元数据**（清单 + 版本信息，不含内容文件）对比：

| 文件数 | 逐个即时生效上传 | 批量发布 | 差距 |
|---:|---|---|---|
| 200 | 602 个对象 / 5,357,290 字节 | 5 个对象 / 81,029 字节 | 120x 更少对象，66x 更少字节 |
| 1000 | 3002 个对象 / 129,187,582 字节 | 5 个对象 / 397,841 字节 | 600x 更少对象，325x 更少字节 |

逐个上传每生效一次就要重写一次全量清单，元数据字节数随文件数**平方增长**；批量发布
只写 1 份清单 + 2 份快照元数据 + 2 份版本标签元数据，**与文件数无关**。差距随规模继续拉大。

1000 文件那一行用 `DINGO_PUBLISH_SCALE_FILES=1000 go test ./internal/dao/ -run TestPublishScale` 复现；
标准测试跑 200 个文件以控制套件耗时。

> **关于耗时**：本机墙上时间波动极大（同一段上传循环在不同时刻相差一个数量级，
> 受后台扫描等外部因素影响），因此不作为交付指标记录，测试中也不对耗时做断言。
> 元数据写入量是结构性指标，与机器状态无关。

最终基线：

```text
go build ./...  PASS
go test ./...   PASS
```

## 5. 已知限制

| 限制 | 说明 |
|---|---|
| 仓库中间状态可见 | **仅在使用逐个即时生效上传时存在**：上传期间下载方可能看到只包含已生效文件的仓库。改用批量发布（§1.3）即可消除，整批一次生效 |
| 批量不减少传输次数 | N 个文件仍然是 N 次 HTTP 上传，批量优化的是发布不是传输 |
| 发布清单需调用方维护 | 服务端不记忆批次，发布时必须提供完整清单。脚本遍历目录算摘要时顺手攒出即可 |
| 不支持从版本中删除文件 | 发布是合并语义。需要一个"干净"的版本时，使用新的版本标签 |
| 发布失败需整批重试 | 不做部分发布。已落盘的文件重试时走幂等快路径，代价很小 |
| 待发布内容占用磁盘 | 传完未发布的内容会占盘直到发布或超期回收（`upload.orphanRetentionHours`，默认 168 小时） |
| 单次发布条目数上限 | `upload.publishMaxFiles`，默认 1000 |
| 仅支持单节点 | 集群部署时需要把自研仓库下载请求固定路由到上传节点 |
| 上传接口仅本机可访问 | 上传方需登录服务器本机，或自行建立端口转发 |
| 单文件顺序续传 | 不支持分块并行或乱序上传 |
| 续传最多重传一个数据块 | 中断点所在块可能需要重传 |
| 单仓库文件数上限 | 本期自动化验证上限为 1000 个文件 |
| 元数据形态与上游不完全相同 | 内容文件形态一致；本地仓库文件级元数据由清单派生，不落逐文件 `paths-info` 缓存 |
