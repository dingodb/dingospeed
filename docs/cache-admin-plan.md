# 缓存管理（页面 + 服务）实施计划

面向 dingospeed 的缓存文件查看 / 清理能力：远端拉取的缓存与本地上传的内容统一在一个页面里查看、
批量移入回收站、以及从回收站彻底删除。

---

## 一、现状事实（先对齐，后面所有设计都依赖这些）

### 1.1 磁盘布局

```
repos/
  files/<repoType>/<org>/<repo>/blobs/<sha|etag>            # 内容寻址的唯一实体
  files/<repoType>/<org>/<repo>/resolve/<commit>/<path>     # 指向 blob 的软链（可能降级为硬链/拷贝）
  api/<repoType>/<org>/<repo>/paths-info/<commit>/<path>/paths-info_post.json   # 远端：路径→oid/size
  api/<repoType>/<org>/<repo>/revision/<commit>/dingo-local-manifest.json       # 本地：快照清单（权威）
  api/<repoType>/<org>/<repo>/revision/<revision|commit>/meta_get.json|meta_head.json
```

- blob 是 `DingCache` 格式：头部（版本/块大小/文件大小）+ 块位图 + 数据。
  **磁盘上的文件大小 ≠ 内容大小**（稀疏 + 头部），内容大小要读头部（`DingCache.GetFileSize()`），
  已缓存字节数要数位图（`HasBlock`）。列表页的"大小"和"完整度"都得走这条路。
- blob 路径含 `org/repo`，所以**同一个 sha 在不同 repo 下是两个独立文件**，不存在跨 repo 共享引用。
  但同一个 repo 内多个 path 去重指向同一个 sha 是常态。

### 1.2 "谁引用了 blob"，远端和本地不是一回事

| | 远端缓存 | 本地上传（`dingo-local` 命名空间） |
|---|---|---|
| 权威引用 | `paths-info` + `resolve` 软链 | `revision/<commit>/dingo-local-manifest.json` |
| resolve 何时建立 | 下载链路首次请求时建立（`FileDao.ConstructBlobsAndFileFile`，`file_dao.go:211`） | **懒建立**，发布时不建（`upload_dao.go:1256` 的注释写明了原因：O(N²) 文件对象） |
| 删掉 resolve 的后果 | blob 失去引用 | **无效**：清单仍引用，下次下载会把 resolve 重建出来 |

### 1.3 已有的自动回收

- `UploadDao.CleanupUnreferencedBlobs`（`upload_dao.go:1016`）：扫 `files/**/blobs/*`，
  **只收 `IsLocalOrgRepo` 为真的**（`collectLocalBlobs` 里的过滤，注释明确说漏掉这个判断会误删公开缓存），
  判据是"不被该 repo 下**任何** revision 的清单引用" + "blob mtime 超过 `orphanRetentionHours`（默认 168h）"。
- `CleanupExpiredStagedUploads`：只管 `.uploading` 暂存文件。
- 两者由 `RunStagedUploadCleanup` 按 `stagingCleanupIntervalMinutes`（默认 60 分钟）轮询。
- `SysService.checkDiskUsage`（`sys_service.go:81`）：容量超限时按 LRU/FIFO/LARGE_FIRST 直接
  `os.Remove` **`repos/files` 下的任意文件**，仅豁免本地命名空间（`isProtectedLocalUploadCacheFile`）。
  它会删 blob，也会删 resolve 软链。**本次不改它。**
  （附带结论：`repos/api` 不在它的扫描范围内，所以清单和我们新加的墓碑文件不会被它误删。）

---

## 二、与需求的冲突点

### C1 一级删除对"本地上传的文件"光删 resolve 不成立 —— 必须动清单

需求描述的是"删除同 repo 下指向 blob 的 resolve"。这对远端缓存完全成立，对本地上传的内容不成立：
清单才是权威，删了 resolve 既不会让 blob 变成未引用（`referencedShas` 只看清单），
下次有人下载该文件时 `ConstructBlobsAndFileFile` 还会把软链重建出来。

三种可选语义：

- **方案 A**：原地重写该 commit 的清单，摘掉这条。
  破坏"commit id 是清单内容的确定性摘要"这个不变式（commit 内容变了，id 没变），
  会让已缓存该 commit 的客户端读到与 id 不符的内容。**不采用。**
- **方案 B**：生成一个新 commit（旧清单减去该条目），把版本标签指向新 commit，保留旧 commit 清单。
  语义最干净，但旧快照仍引用 blob，`referencedShas` 扫全部快照 → blob 永远不会进回收站。
  除非同时删旧清单，那就退化成方案 C。**不采用。**
- **方案 C（采用）**：一级删除 = 从**该 repo 下所有 revision 快照清单**中摘除该条目
  （命中方式见下），同步重写受影响 revision 的 `meta_get/meta_head`，并删掉已存在的 resolve 软链。
  blob 立刻变成"未被任何清单引用"，与 `CleanupUnreferencedBlobs` 的判据天然对齐。

  > **实现时的修正（重要）**：方案 C 起初写成"原地重写该 commit 的清单，commit id 不变"，
  > 落地测试直接把它打回来了——它其实就是方案 A，而方案 A 的危害比语义不洁严重得多：
  > 发布路径靠"算出来的清单摘要 == 版本标签当前指向的 commit"判断这批发布无变化并**直接跳过**
  > （`upload_dao.go` 的 `commit == currentCommit` 分支）。标识一旦与内容对不上，
  > 删除后重新上传同一个文件会被误判成"unchanged"，元数据不重写，**文件再也回不来**。
  >
  > 最终实现是"旧快照整体作废"：新清单落到它自己的摘要标识下，指向旧快照的版本标签改指新标识，
  > 旧快照目录（清单 + 元数据 + 该 commit 下的 resolve 链接）整体删除；
  > 摘空且无标签指向的历史快照直接丢弃，不留空清单。
  > 快照标识的计算抽成 `manifestCommit`，与发布路径共用同一份实现。
  >
  > resolve 链接必须随旧快照一起删：`CreateLinkOrCopyIfNotExists` 是 软链→硬链→拷贝 三级降级，
  > 降级成硬链时链接还在的话，二级删除 `os.Remove(blob)` **不会释放磁盘**。

  方案 C 满足需求里那句"后续上传了相同的文件，这个 resolve 仍然会重新建立"：
  重新上传 + 发布 → 清单重新引用同一个 sha；只要 blob 还没被二级删除，
  上传走的还是 `materializeBlob` 的幂等快路径（秒传），resolve 由下载链路重建。

  选中粒度：页面上一行 = (repoType, orgRepo, path, sha)。摘除时按 **path + sha 同时匹配**，
  避免把"同路径但别的快照里是另一份内容"的条目一起摘掉。

### C2 自动回收目前**够不着**远端缓存，但不能无差别放开

需求要"复用 168h 那套"。事实是：本地上传的内容**已经**在这套里；缺的是远端。
但直接把远端 blob 纳入"无引用即回收"是危险的，两条独立理由：

1. `collectLocalBlobs` 的注释已经点名了这个风险——公开缓存与自研内容共用同一棵目录树，
   放开过滤等于改动磁盘清理对公开模型的既有行为。
2. diskClean 的 LRU 会直接 `os.Remove` resolve 软链。一个 blob 完全可能"resolve 被 LRU 删了、
   blob 还在"，此时它在"扫 resolve 判引用"的口径下就是未引用的，无差别 GC 会把它删掉——
   而这跟用户的任何删除动作都无关。

**采用：墓碑驱动。** 只有经过一级删除、留下了回收站墓碑的远端 blob 才进入自动回收。
没有墓碑的远端 blob 生命周期一个字不改。

### C3 保留期的计时起点：blob mtime 对回收站语义是错的

现有实现用 blob 的 `ModTime` 判 168h。对"上传完等发布"的场景这没问题。
但对回收站不成立：一级删除一个三个月前上传的文件，它的 mtime 早就超过 168h，
下一个清理 tick（最多 1 小时后）就会被彻底删掉——**回收站形同虚设，且页面上的"剩余保留时间"是假的**。

**采用：有墓碑时以墓碑里的 `unlinkedAt` 为准，无墓碑回落 blob mtime。**
这对"暂缓生效的上传残留"这条老路径完全没影响（它没有墓碑）。

### C4 resolve → blob 的映射不能只靠 `os.Readlink`

`CreateLinkOrCopyIfNotExists`（`repo_util.go:227`）是 symlink → hardlink → 拷贝三级降级。
Windows 无开发者模式时拿不到软链，`Readlink` 会失败。
所以列表和删除都应以 **paths-info（远端）/ 清单（本地）** 为映射来源，
resolve 目录只用于"这个路径当前有没有落地链接"的展示与清理，读不到链接目标也不影响正确性。

### C5 并发与在途句柄

- 加锁顺序既有约定是 repo → revision → blob，删除路径必须沿用，反向获取会死锁。
- 二级删除 `os.Remove(blob)` 必须持 blob **写锁**（`uploadBlobLocks.Lock`），
  与分块上传的读锁、老接口的 rename 互斥——这正是 `cleanupRepoBlobs` 已经在做的，直接复用。
- 另需挡住"正在被下载/上传的文件被彻底删除"：删除前查 `localUploadInFlight`，
  以及 `DingCacheManager` 里是否还有该路径的活跃句柄（`dingCacheRef`）；有则跳过并在结果里说明原因。
  （`file_manager.go` 目前没有对外暴露"是否在用"的查询，需要加一个只读方法。）

### C6 上传/管理服务只监听 loopback

`NewUploadServer` 强制把非 loopback 的 host 改成 `127.0.0.1`。这是既有的安全姿态，
本次沿用，不放开。

**这条直接决定了页面不能放在 dingospeed 里**：挂在这个服务上的页面只能从容器/主机本机
访问（或端口转发），而运维的入口是 spinfield 控制台。所以接口留在这个回环端口后面，
页面进控制台，中间由与 dingospeed 同机的 ingest agent 转发——和模型上传是同一条链路
（见第四节）。

### C7 wire 是生成代码

新增 dao/service/handler 需同时改 `cmd/wire.go` 与 `cmd/wire_gen.go`。
打包机（内网构建机）没有 Go，跑不了 `wire` 生成，**手改 `wire_gen.go` 即可**（它就是普通 Go 代码）。

### C8 离线部署不能引外部资源

`templates/repos.html` 从 cdnjs 拉 semantic-ui，离线部署下这页面是裸的。
最终方案天然规避了这点：页面进了 spinfield 控制台，走它已有的构建产物
（`internal/console/dist`，`embed` 进 manager 二进制），不额外引任何外部资源。

---

## 三、后端设计

### 3.1 回收站墓碑

路径：`repos/api/<repoType>/<org>/<repo>/recycle/<sha>.json`（原子写，复用 `util.WriteDataToFileAtomic`）

```json
{
  "repoType": "models",
  "orgRepo": "Qwen/Qwen2.5-0.5B",
  "sha": "…",
  "size": 1234567,
  "source": "remote",
  "paths": ["config.json"],
  "revisions": ["main", "<commit>"],
  "unlinkedAt": 1755400000
}
```

- 放在 `api/` 下而不是 `files/` 下：避开 diskClean 的扫描范围（它只扫 `files`）。
- `referencedShas` 只遍历 `revision/*`，新增 `recycle/` 兄弟目录不干扰它。
- 一个 sha 一个文件：并发删除不需要合并写，天然原子。

### 3.2 判定"blob 已无任何引用"

统一成一个函数 `blobReferences(repoType, orgRepo, sha) []reference`：

- 本地命名空间：复用 `UploadDao.referencedShas` 的思路，扫全部 `revision/<commit>/dingo-local-manifest.json`。
- 远端：扫 `api/<…>/paths-info/<commit>/**/paths-info_post.json`，取 `lfs.oid`（空则 `oid`）。
  外加扫 `files/<…>/resolve/**` 作为补充（覆盖 paths-info 被 LRU 清掉但 resolve 还在的情况）。

二级列表 = 墓碑集合中，blob 仍存在且 `blobReferences` 仍为空的那些
（墓碑存在但引用又回来了 → 说明重新上传/下载过，墓碑已作废，列表里不显示）
∪ 本地命名空间下"无墓碑但清单未引用"的 blob（保持现有回收对象可见，`unlinkedAt` 回落 mtime 并标 `inferred: true`）。

作废墓碑的**删除**只在持有仓库锁的两趟回收任务里做，列表路径只是不显示它。
列表是不加锁的，在那里顺手删会与并发的一级删除撞车：一次列表请求可能把刚写下的、
完全有效的墓碑抹掉，那条内容的保留期起点就永久丢了。

### 3.3 接口（挂在 upload echo，`X-Dingo-Upload-Token` 鉴权，复用 `validateUploadToken`）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/cache/summary` | 总占用、文件数、回收站条数与大小、`orphanRetentionHours`、清理间隔 |
| GET | `/api/cache/repos` | repo 列表：repoType、orgRepo、source(remote/local)、文件数、合计大小 |
| GET | `/api/cache/files` | 一级列表，参数 `repoType/orgRepo/revision/source/keyword/sort/page/pageSize` |
| POST | `/api/cache/files/delete` | 一级删除，`{items:[{repoType,orgRepo,path,sha,revision?}]}`，逐条返回 ok/skipped/reason |
| GET | `/api/cache/orphans` | 二级列表（回收站） |
| POST | `/api/cache/orphans/delete` | 二级删除（彻底删 blob + 墓碑） |

一级列表行字段：`repoType, orgRepo, revision, commit, path, sha, size, cachedBytes, complete, source, blobMTime, hasResolve`。

二级列表行字段：`repoType, orgRepo, sha, size, diskSize, paths[], revisions[], unlinkedAt, expiresAt, source, inferred`。

分页、排序、关键字过滤都在服务端做。

**扫描结果没有做缓存**：每次请求都重扫目录树并重读 blob 头部。原本计划用 `data.BaseData.Cache`
加一层短 TTL 缓存，实现时放弃了——这个页面的每次读都紧跟着一次删除决策，
缓存带来的陈旧窗口会让用户对着已经不存在的条目点删除，而省下的只是一个内网管理页的响应时间。
代价是仓库很大时列表会变慢（每个 blob 要开一次文件读 128KB 的头部）；
真的成为问题时，正确的做法是按 (repoType, orgRepo) 缓存并在删除路径上主动失效，而不是全局 TTL。

### 3.4 一级删除流程（`CacheAdminDao.SoftDelete`）

```
按 (repoType, orgRepo) 分组，逐组：
  uploadRepoLocks.Lock(repo)
  0. 先记下命中快照当前挂着的版本标签（下一步会把旧快照连同标签映射一起抹掉）
  1. 本地仓库：遍历该 repo 全部 revision 清单，摘除 (path, sha) 命中的条目；
     清单有变化的 commit → replaceSnapshot：
        新清单写到 manifestCommit(新清单) 下 → 新标识与指向旧快照的标签都重写 meta_get/meta_head
        → 旧快照目录与 resolve/<旧commit>/ 整体删除 → 失效 FileDao 的清单缓存
     远端仓库：删除对应的 paths-info_post.json
  2. 删除 files/<…>/resolve/<commit>/<path>（存在才删；软链/硬链/拷贝三种形态都直接 os.Remove）
  3. 重新计算该 sha 的引用；仍为空 → 写墓碑（unlinkedAt=now）
     不为空（同 repo 内还有别的 path 指向它）→ 不写墓碑，结果标记 "still-referenced"
  uploadRepoLocks.Unlock(repo)
```

**不删 blob。** 这是需求里"一级删除"的全部含义。

### 3.5 二级删除（`CacheAdminDao.PurgeOrphans`）

```
逐条：
  repo 锁 → 校验 blobReferences 仍为空（防竞态：期间重新发布过就跳过）
  → 检查在途（localUploadInFlight / DingCacheManager 活跃句柄）→ 有则跳过
  → uploadBlobLocks.Lock(blob) → os.Remove(blob) → Unlock
  → 删墓碑
  → 若配了 scheduler client，复用 SysService.deleteRecordByFilePath 的逻辑同步删调度侧记录
```

### 3.6 自动回收改造（复用，不另起一套）

在 `upload_dao.go` 里：

1. 把 `cleanupRepoBlobs` 里"检查 → 加锁 → Remove"那段抽成 `reclaimBlob(repoType, orgRepo, sha, cutoff, unlinkedAt)`。
2. 现有本地扫描保持原逻辑，只有两处变化：
   - 保留期基准改为"有墓碑取 `unlinkedAt`，无墓碑取 blob mtime"（对应 C3）。
   - 回收成功后顺带删墓碑。
3. `RunStagedUploadCleanup` 里新增第三趟：**墓碑扫描**。遍历所有 `api/**/recycle/*.json`：
   - blob 已不存在 → 删墓碑。
   - 引用又回来了 → 删墓碑。
   - `now - unlinkedAt > orphanRetention` → `reclaimBlob` + 删墓碑。
   本地命名空间的墓碑在第 2 趟已经处理，这一趟跳过它们，避免重复扫描。

净效果：上传的内容与远端缓存都受同一个 `orphanRetentionHours`（168h）管辖，配置项不新增。

### 3.7 不做

- 不动 diskClean 的本地命名空间豁免。
- 不改无墓碑的远端 blob 的既有生命周期。
- 不引入新的保留期配置项。

---

## 四、前端设计

页面在 **spinfield 控制台**里，不在 dingospeed 里。

最初落地时它是 `internal/server/templates/cache.html`（`embed` 进二进制，挂 `GET /cache`
于上传服务）。那是个走错了位置的实现：上传服务只监听回环地址（C6），页面只能从
dingospeed 所在主机访问；而运维实际使用的入口是 spinfield 控制台，缓存管理必须和
「模型上传」在同一个界面里。该模板与 `/cache` 路由已删除，dingospeed 这侧只保留
`/api/cache/*` 这 6 个 JSON 接口。

链路与模型上传完全一致（复用同一套已有设施）：

```
浏览器（spinfield 控制台，session token）
  → 控制面 adminapi  /admin/v1/cache/*      internal/adminapi/cache_admin.go
  → ingest agent     /admin/v1/cache/*      internal/ingestserver/cache_admin.go
  → dingospeed       /api/cache/*           X-Dingo-Upload-Token 由 agent 附加
```

关键点：**浏览器不再需要输入 token**。上传 token 由与 dingospeed 同机的 agent 从自己的
配置（`DINGOSPEED_UPLOAD_TOKEN`）里附加，不下发到前端。两跳都只做透传，不解析报文体，
列表字段增删不需要改中间两层；可达的路径在两跳各有一张固定的 op 表，避免把上传写接口
一并暴露到控制面后面。

spinfield 侧文件：`web/console/src/pages/CacheAdmin.tsx`、`services/cacheAdmin.ts`、
`types/cacheAdmin.ts`，路由 `/cache-admin`，侧边栏「缓存管理」（与「模型上传」同组）。

**布局**

```
┌ 顶栏：缓存管理                                          [刷新]         ┐
├ 提示条（醒目、常驻）：                                                  │
│   删除后的文件进入回收站，不会立即释放磁盘；                            │
│   回收站中的文件在最后一次引用被删除 168 小时后由系统自动彻底删除。      │
├ 概览卡：缓存总占用 / 文件数 / 回收站条数 / 回收站可释放空间             │
├ Tab【缓存文件】 Tab【回收站】                                          │
└─────────────────────────────────────────────────────────────────────┘
```

**Tab 1 缓存文件**

- 左：仓库树，一级 `模型 / 数据集`，二级 `远端缓存 / 本地上传`，三级 repo（带文件数与合计大小）。
- 右：表格 —— 复选框 | 文件路径 | 大小 | 缓存完整度 | 来源 | 修订 | 内容摘要(短) | 更新时间。
- 搜索框按路径/摘要模糊过滤；表头点击按大小/时间排序；服务端分页。
- 表头复选框全选当前页；行支持 shift 连选；已选条数与合计大小常驻在工具栏。
- 工具栏右侧垃圾桶按钮，选中数为 0 时禁用；点击弹确认框，
  列出选中项数量与合计大小，文案："这些文件将从仓库中移除并进入回收站，磁盘空间在保留期结束后释放。"

> 实现差异：行不支持 shift 连选（控制台其他列表也没有这个交互，为它单独引入一套
> 选区逻辑不划算）；全选复选框只作用于当前页，跨页选择靠已选计数保持。
> 关键字过滤按「查询」才生效，不随输入即时触发——每次列表请求都会在 agent 主机上
> 重扫目录树并读每个 blob 的头部（见 3.3），逐键触发会把管理页变成压测。

**Tab 2 回收站**

- 表格 —— 复选框 | 内容摘要 | 大小 | 所属仓库 | 原文件路径（多条折叠展示）| 进入回收站时间 | 剩余保留时间 | 来源。
- 剩余保留时间前端按 `expiresAt` 倒计时渲染，`< 24h` 标红。
- 垃圾桶图标 → 二次确认，红色按钮，文案："彻底删除后无法恢复，磁盘空间立即释放。"

**措辞**：页面上只出现"删除 / 移入回收站 / 彻底删除"，不出现"一级/二级删除"。

---

## 五、落地清单（已完成）

| 文件 | 内容 |
|---|---|
| `internal/dao/cache_admin_dao.go` | 新增。扫描、引用判定、墓碑、一级/二级删除 |
| `internal/dao/upload_dao.go` | 抽出 `manifestCommit`；回收改为墓碑优先的保留期判定；新增 `CleanupRecycledBlobs` 并挂进 `RunStagedUploadCleanup` |
| `internal/dao/file_dao.go` | 新增 `InvalidateLocalManifest`（快照被取代时清缓存） |
| `internal/downloader/file_manager.go` | 新增 `IsInUse`（挡住删除正在传输的文件） |
| `internal/service/cache_admin_service.go` | 新增。鉴权、筛选、排序、分页 |
| `internal/handler/cache_admin_handler.go` | 新增。6 个 JSON 接口（无页面入口） |
| `internal/router/upload_router.go` | 挂载 `/api/cache/*` |
| `dao.go` / `service.go` / `handler.go` / `cmd/wire_gen.go` | 注册新组件（`wire_gen.go` 手改，打包机没有 Go） |

spinfield 侧（页面与两跳转发）：

| 文件 | 内容 |
|---|---|
| `web/console/src/pages/CacheAdmin.tsx` | 新增。管理页面，复用控制台既有组件 |
| `web/console/src/services/cacheAdmin.ts` / `types/cacheAdmin.ts` | 新增。接口封装与报文类型 |
| `web/console/src/App.tsx` / `components/Layout/Sidebar.tsx` / `locales/*.json` | 路由 `/cache-admin` 与侧边栏入口 |
| `internal/adminapi/cache_admin.go` / `server.go` | 控制面转发到 agent（仅在配了 agent 时注册） |
| `internal/ingestserver/cache_admin.go` / `server.go` | agent 转发到 dingospeed，附加上传 token |
| `internal/ingestclient/client.go` / `internal/modelupload/cacheadmin.go` | 两跳各自的固定 op 表与超时 |

## 六、测试

### 6.1 自动化测试

`internal/dao/cache_admin_dao_test.go`（按风险点分组，A–F）：

- **A 扫描与列表**：上传内容可见且大小取自 blob 头部而非磁盘文件；去重内容一路径一行；
  远端缓存可见且 `hasResolve` 正确；部分缓存必须显示成部分；`ListRepos` 双来源汇总。
  外加一条**只读性**断言：列表跑一遍之后 blob 的 mtime 与大小不变——
  `readBlobStat` 若误用 `NewDingCache` 就会以 `O_RDWR` 打开并可能重写头部，页面刷一下就把缓存改了。
- **B 一级删除·本地**：跨全部历史快照摘除、旧标识作废、标签改指、其余文件不受影响；
  元数据（`meta_get`/`meta_head` 的 siblings）同步重写、`FileDao` 清单缓存失效；
  去重内容要等最后一个路径消失才进回收站；重复删除幂等（skipped，不升级成删 blob）；
  删除后重新上传走秒传（`blobReused=true`）且引用恢复、墓碑作废。
- **C 一级删除·远端**：resolve 与 paths-info 都删掉、blob 保留、墓碑写下；
  无墓碑的远端孤儿 blob（模拟 LRU 删掉链接）**不得**出现在回收站里。
- **D 二级删除**：真删 blob 与墓碑；引用回归时跳过；文件正被传输时失败而不是删掉。
- **E 自动回收**：**回归**——刚删进回收站的老文件必须活过保留期（不能按 blob mtime 判）；
  墓碑超期后回收且墓碑一并清除；无墓碑的历史残留仍按 mtime 回收（老行为不变）；
  远端只回收有墓碑且超期的，被引用的与无墓碑的都不动；墓碑扫描跳过本地命名空间避免重复回收。
- **F 输入校验**：`orgRepo` 的 `..`、绝对路径、`a/../../b` 一律拒绝，并断言仓库根之外的文件没被动过；
  非法 `repoType` 拒绝；回收站条目带得出 `expiresAt` / `paths` / `revisions`。

`internal/service/cache_admin_service_test.go`：六个入口逐个验证 token 缺失/错误/正确；
空批次拒绝；排序四个键与升降序；分页越界回落最后一页、空集不产生第 0 页、`pageSize` 上限 500；
关键字大小写不敏感且跨字段匹配。

```bash
go test ./internal/dao/ ./internal/service/
```

全量回归（既有的上传/发布/回收测试必须一起绿）：

```bash
go test ./...
```

### 6.2 手工端到端（已跑通）

用一份改了端口与 `repos` 的配置副本起服务（`upload.port` 与下载端口都要改，`pprof` 关掉），然后：

1. `GET /api/cache/summary` 无 token 返回 401、带 token 返回汇总。
2. 上传两个文件并发布 → `/api/cache/files` 两行，大小/来源/版本正确。
3. 通过下载端口取一次文件（这会建立 resolve 链接）。
4. 删除其中一个 → 该文件从列表消失、出现在 `/api/cache/orphans`、blob 仍在盘上、
   下载返回 404，另一个文件照常下载。
5. 重新上传同一个文件 → `blobReused=true`（秒传）→ 发布 → 下载恢复、回收站清空。
6. 再删一次并彻底删除 → blob 目录清空、墓碑目录清空、重复彻底删除返回 skipped。
7. 越权与非法输入：`orgRepo=../../..` 返回 failed，空 items 返回 400。

页面侧当时是在 dingospeed 自带的 `/cache` 上用浏览器验证的：概览与 168 小时提示、
仓库树、表格渲染、表头全选、选中计数与合计、删除确认弹窗文案（两个 Tab 文案不同）、
删除后自动刷新、回收站的剩余保留时间（"6 天 23 小时"）、彻底删除后的 toast。控制台无报错。

页面迁进 spinfield 控制台后，这些行为由 `web/console/src/__tests__/cache-admin-page.test.tsx`
与 `cache-admin-service.test.ts` 覆盖（提示条取自服务端的 retention/interval、仓库树筛选、
关键字按「查询」生效、一级删除按 path+sha、二级删除按 sha、切 Tab 清空选中、
逐条结果里的 skipped/failed 必须出现在提示里）。转发两跳各有 Go 测试：
`internal/adminapi/cache_admin_test.go`、`internal/ingestserver/server_test.go`。
**尚未在真机上端到端点过这条新链路**（控制台 → agent → dingospeed），
部署到 245.51 后需要手工过一遍第 2–7 步。

### 6.3 没有覆盖到的

- **新链路的真机验证**：上面那条，控制台 → agent → dingospeed 只有单测，没跑过真机。

- **并发**：删除与上传/发布/下载同时进行的竞态只靠既有锁保证，没写并发压测；
  `upload_publish_scale_test.go` 那类规模测试没有扩展到删除路径。
- **diskClean 交互**：LRU 删掉 resolve 或 blob 之后再走管理页的行为只在单测里模拟，
  没有起真实的磁盘超限场景。
- **spaces 类型**：代码按三种 repoType 处理，测试只覆盖了 models。

## 七、落地顺序（实际执行）

1. `internal/dao/cache_admin_dao.go`：目录扫描、blob 头解析、引用计算、列表（单测：远端/本地/去重/空仓）
2. 墓碑读写 + 一级删除（单测：本地清单摘除、同 sha 多路径、远端 paths-info 删除、幂等重复删）
3. `upload_dao.go` GC 改造（单测：墓碑保留期、引用回归作废墓碑、无墓碑回落 mtime 的老行为不变）
4. 二级删除（单测：竞态跳过、在途跳过）
5. `internal/service/cache_admin_service.go` + `internal/handler/cache_admin_handler.go`
   + `upload_router.go` 路由 + `wire.go` / `wire_gen.go`
6. `templates/cache.html` + 上传服务的 Renderer（`NewUploadEngine` 目前没设 Renderer，需要补）
7. 端到端：上传 → 列表可见 → 删除 → 回收站可见 → 彻底删除 → 磁盘释放；
   以及 remote 拉取 → 删除 → 重新拉取重建

---

## 八、已确认的两点

1. **一级删除对本地上传内容采用方案 C**（从该 repo 全部快照清单中摘除该条目）。
   代价：历史 commit 的清单会变，记录过旧 commit id 的客户端将读不到这个文件。
   这是"让 blob 真正变成未引用"的必要条件，也是需求想要的效果。
2. **保留期计时起点改为墓碑 `unlinkedAt`**（无墓碑仍回落 blob mtime，老行为不变）。
   不改的话回收站对"老文件"完全不生效。
