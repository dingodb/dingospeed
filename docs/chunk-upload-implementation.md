# 分块上传 —— 实现交接说明

> 日期：2026-08-16　代码基线：本仓库当前工作区
>
> **本文取代 `chunk-upload-change-spec.md` 与 `chunk-upload-detailed-design.md`。**
> 那两份文档引入了 stage controller、stageToken、reader/writer 租约、隔离态、
> 状态机等机制，全部超出需要。实现请只依据本文。
>
> 本文的每一条事实都来自读源码确认，引用处标了 `文件:行号`。

---

## 1. 核心思路

**分块上传对标下载侧的缓存写入，不引入任何下载侧没有的机制。**

下载侧的既有行为（已核实）：

| 事实 | 证据 |
|---|---|
| blob 路径从一开始就是最终路径 `files/<repoType>/<orgRepo>/blobs/<etag>` | `internal/dao/file_dao.go:208` |
| 空文件在请求刚进来、还没有任何内容时就已创建 | `internal/dao/file_dao.go:317`（`CreateFileIfNotExist`） |
| 内容按块增量写入该最终路径，**无暂存文件、无 rename** | `internal/downloader/remote_task.go` 全流程 |
| **从不做任何 hash 校验**，`etag` 是上游声称的值，从未被验证 | 全链路无 sha256/md5 比对 |
| **从不覆盖已置位的块** | `remote_task.go:126`、`:169`（`if !hasBlockBool { WriteBlock }`） |
| `WriteBlock` 失败只记日志，不向上返回；靠"位=0，下次重下"自愈 | `remote_task.go:129`、`:174` |
| 并发写者共享同一个 `*DingCache`，位图更新由 `fileLock` 串行 | `internal/downloader/file_manager.go:34` |

上传侧沿用同一套：**同一个最终路径、同一个位图、同一个"不覆盖已置位块"规则、同一个"失败就重传"模型。**
块内容用 chunk 级 sha 校验，**不做整文件 sha 校验**（blob 名与下载侧的 etag 一样，是外部声明，信任层级一致）。

---

## 2. 必须钉住的不变量

1. **位图的语义**：某块位 = 1，表示"写入这块时其内容通过了 chunk 级 sha 校验"。
   不承诺文件整体正确，不承诺事后没被外部改动。
2. **不覆盖 = 不覆盖已置位的块**，不是不覆盖已有字节。
   上次写一半崩掉留下的垃圾字节位是 0，重传必须能盖上去。
3. **写入顺序：先写 payload，后置位。** 现有 `WriteBlock` 已经是这个顺序
   （`internal/downloader/file.go:271`：先 `f.Write`，再 `fileLock` → `setHeaderBlock` → `flushHeader`），
   **不需要改 `DingCache`**。崩在中间只会导致位=0，方向安全。
4. **校验必须发生在置位之前** → chunk 内容要先进内存缓冲算 hash，通过后才调 `WriteBlock`。
5. **分块上传永远是 deferred，绝不自动生效。**
   这是替代 rename 闸门的关键：本地仓库的文件信息完全从 manifest 派生
   （`internal/dao/file_dao.go:359`，`GetPathsInfo` 对本地仓库只查清单，查不到直接 404），
   而 publish 时 `verifyPublishContent` → `inspectCompleteBlob` 会检查"位图无空洞 + size 匹配"
   （`internal/dao/upload_dao.go:355`、`:568`）。
   **清单是可见性闸门，不完整的 blob 对下载侧不可见。**
6. 所有锁都是进程内的。多个 dingospeed 副本共享同一个 `repos` 目录时本设计不成立。

---

## 3. 协议

```
PUT /api/local-upload-chunk/:repoType/:org/:repo/:revision/*
    ?sha256=<整文件 sha256，即 blob 文件名>
    &size=<整文件总字节数>
    &offset=<本 chunk 的起始字节偏移>
    &chunkSha256=<本 chunk 内容的 sha256>
Header: X-Dingo-Upload-Token
Body:   本 chunk 的原始字节
```

服务端处理顺序：

```
1. 校验 token（复用 validateUploadToken）
2. 校验 repo/file locator（复用 validateUploadLocator）
3. 对齐与范围校验，任一不过 → 400 UPLOAD_INVALID_ARGUMENT
     offset % blockSize == 0
     Content-Length % blockSize == 0    （末 chunk 除外）
     offset + Content-Length <= size
     offset >= 0
4. dingFile := downloader.GetInstance().GetDingFile(blobPath, size)
   defer downloader.GetInstance().ReleasedDingFile(blobPath)
5. 校验 header 已绑定的 FileSize 与本次声明的 size 一致
   不一致 → 409 UPLOAD_SIZE_BINDING_MISMATCH
6. 若本 chunk 覆盖的所有块都 HasBlock → 200 {"status":"already_present"}
   （幂等快路径，不读 body）
7. 读满 body 到内存；长度与 Content-Length 不符 → 400
8. sha256(buf) != chunkSha256 → 400 UPLOAD_CHUNK_SHA_MISMATCH（不写任何字节）
9. 逐块 WriteBlock，跳过已置位的块
10. 200 {"status":"written","blocks":n}
```

**没有 finalize 步骤、没有 rename、没有整文件 hash、没有状态机、没有 token/租约。**
"传完了"由 publish 时的位图检查判定。

### 进度查询

改造现有 `GET /api/local-upload-progress/...`（`internal/dao/upload_dao.go:711`）：

- 现有 `resumableOffset`（`upload_dao.go:645`）返回**第一个空洞**，是顺序续传的概念，
  乱序并发下无意义。改为返回**缺块区间列表**，agent 据此重传。
- **响应里带上服务端的 `blockSize`**。agent 必须先取它再切分，不允许自己配置 —— blockSize 是
  stage 文件 header 里的权威值，agent 侧配置错了就是静默数据损坏。这样不必新增 capabilities 接口。

---

## 4. 实现清单

### 4.1 新增

- `internal/router/upload_router.go`：注册 chunk 路由。
- `internal/handler/upload_handler.go`：新增 `UploadChunk`。
- `internal/service/upload_service.go`：新增参数解析与校验。
- `internal/dao/upload_dao.go`：新增 `UploadChunk`，实现 §3 的 10 步。

### 4.2 复用（不要重写）

- `downloader.GetInstance().GetDingFile()` —— 直接用下载侧的管理器。
  它顺带解决三件事：同一路径进程内唯一 handle（位图不丢更新、header 不撕裂）、
  创建与 `Resize` 在管理器的 `mu` 下串行（`file_manager.go:34`）、
  `Resize` 只在 `GetFileSize()==0 && fileSize>0` 时执行一次（`file_manager.go:53`）。
  stage 路径与下载缓存路径不重叠，共用一张 map 不冲突。
- `DingCache.HasBlock` / `WriteBlock` —— 一行不用改。
- `localBlobPath`（`upload_dao.go:585`）、`ensureLocalUploadPathSafe`、`validateUploadToken`、
  `validateUploadLocator`、`validateRepoLocator`、`validateFileLocator`。
- publish 全链路（`PublishFiles`、`verifyPublishContent`、`classifyPublishItems`）—— **一行不改**。

### 4.3 修改

| 位置 | 改动 |
|---|---|
| `upload_dao.go:135` `uploadBlobLocks` | `keyedMutex` → 读写版本。chunk 写取**读锁**（互不阻塞），legacy 的 rename 与 cleanup 的 `os.Remove` 取**写锁**。约 10 行。理由见 §5。 |
| `upload_dao.go:711` `QueryProgress` | 返回缺块列表 + `blockSize`，替代 `resumeOffset` 语义 |
| `upload_dao.go:940` 注释 | "这里只回收已经完整的那一份"与代码不符（代码 `Stat` 到就删，不检查完整性）。新用法下行为正确，改注释即可 |

### 4.4 不要做

- 不要在 chunk 路径上走 `tryEnterLocalUpload(fileKey)`（`upload_dao.go:194`）——
  它按整文件互斥，第二个并发块直接 409 `UPLOAD_FILE_BUSY`。
- 不要在 chunk 路径上走 `acquireUploadSlot` / `upload.concurrentLimit`（`upload_service.go`）——
  那是"整文件上传槽位"语义，默认 4，四个并发块就把其它文件全挡成 429。
- 不要给 chunk 路径加 `.uploading` 暂存与 rename。
- 不要加整文件 sha 校验、force 覆盖、状态机、隔离/quarantine。
- **不要动 `DingCache`**（`internal/downloader/file.go`）——它同时服务下载全链路。

### 4.5 老接口原样保留

`UploadWholeFile` / `PublishFiles` / `materializeBlob` / `writeStagedBlob` 及
`.uploading` 暂存 + 整文件校验 + rename 的整条路径**不动**。
它是**强制覆盖**的实现手段：内容寻址下唯一需要原子替换的场景就是覆盖，
`os.Rename` 是唯一能保证读者不看到半新半旧内容的方式。

---

## 5. 两条路径的唯一交叉点

同一个 sha 上，legacy 路径正在 rename、chunk 路径正在写同一个 blob 文件：

- Windows：`os.Rename` 因文件被占用而失败 —— 报错，可接受。
- Linux：inode 被替换，chunk writer 继续往孤儿 inode 上写，位图更新全部丢失且无人知晓。

修法就是 §4.3 的 `RWMutex`：chunk 写持读锁，rename / remove 持写锁。这是本设计里唯一新增的并发保护。

---

## 6. 实现时需确认的小事

1. **零字节文件**：块数为 0，位图天然"无空洞"，`inspectCompleteBlob` 应判定为完整。确认现有逻辑不会误判。
2. **末块**：允许 `Content-Length` 不是 `blockSize` 整数倍。`WriteBlock` 内部已按 `FileSize` 截断
   （`file.go:294` 附近），不需要额外处理；但传给 `WriteBlock` 的 buffer 必须补齐到整块长度
   （它会校验 `len(blockBytes) == blockSize`）。
3. **容量上限**：`DEFAULT_BLOCK_MASK_MAX = 1024*1024` 块，8MiB 块 → 单文件上限 8TiB。
   现有 `validateUploadParam` 已有这个检查，chunk 路径同样要做。
4. **内存**：校验缓冲 = 并发数 × chunk 大小。建议 chunk 取 1～8 个块（8～64MiB），不要更大。
5. **进程重启撞上 `flushHeader` 写到一半**：下次 `Open` 读 header 失败会写一份全新空 header
   （`file.go:74`），等于位图清零、整个文件重传。在"失败就重传"的模型下可接受，知道它存在即可。
6. **中途放弃的分块上传**：以不完整的 `blobs/<sha>` 形式存在，由 `CleanupUnreferencedBlobs`
   （`upload_dao.go:898`）按 modtime + 未被任何清单引用回收，默认保留 168h。
   续传间隔小于保留期就不会被误删。

---

## 7. 验收

- 同一文件多块**乱序并发**上传后，位图无空洞，publish 成功。
- 重复上传同一块 → `already_present`，不重复写。
- chunk sha 不匹配 → 400，且该块位仍为 0，磁盘无任何字节被改。
- 非对齐 offset / 非整数倍长度 → 400。
- 传一半中断 → 进度接口能列出缺块 → 只补缺块 → publish 成功。
- publish 时缺块 → `PUBLISH_CONTENT_NOT_READY`。
- 未 publish 的完整 blob，下载侧 404（清单闸门生效）。
- **老的单文件 / 多文件上传与 publish 行为零回归。**
- `go test -race`。
