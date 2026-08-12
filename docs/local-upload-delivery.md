# 自研模型上传功能交付说明

| 项目 | 内容 |
|---|---|
| 日期 | 2026-08-10 |
| 覆盖范围 | `model-upload-requirements.md` §10 交付物与环 4 收尾 |
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

最终基线：

```text
go build ./...  PASS
go test ./...   PASS
```

## 5. 已知限制

| 限制 | 说明 |
|---|---|
| 仓库中间状态可见 | 逐个文件上传期间，下载方可能看到只包含已生效文件的仓库 |
| 仅支持单节点 | 集群部署时需要把自研仓库下载请求固定路由到上传节点 |
| 上传接口仅本机可访问 | 上传方需登录服务器本机，或自行建立端口转发 |
| 单文件顺序续传 | 不支持分块并行或乱序上传 |
| 续传最多重传一个数据块 | 中断点所在块可能需要重传 |
| 单仓库文件数上限 | 本期自动化验证上限为 1000 个文件 |
| 元数据形态与上游不完全相同 | 内容文件形态一致；本地仓库文件级元数据由清单派生，不落逐文件 `paths-info` 缓存 |
