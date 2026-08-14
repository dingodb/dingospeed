# spinfield × dingospeed 链路打通 — 需求文档（环 0：先亮灯）

| 项目 | 内容 |
|---|---|
| 文档版本 | v2.2 |
| 日期 | 2026-08-12 |
| 文档性质 | 增量需求定义（**以两个项目的真实链路是否跑通为唯一验收**） |
| 涉及仓库 | `D:\project\workplace\spinfield`（主要改动）、`D:\project\workplace\ModelManager\dingospeed`（本期零代码改动） |
| 读者 | 两侧实现者 / 编码 Agent |
| v2.0 变更 | 摘要改由后端计算；字节来源改为后端按路径读取本地文件 |
| v2.1 变更 | 补充 spinfield 侧已有可运行交互原型，明确前端可复用原型中的目录浏览和进度交互 |
| v2.2 变更 | 按“安全后置、避免过度设计、先跑通两个项目”收缩环 0；删除仓库列表/容量/完整元信息/mock-first 等非闭环工作；修正 `/repos`、本地代理、Windows 路径、启动与冒烟命令；新增“文档与代码冲突处理门禁” |

---

## 0. 最高原则

### 0.1 唯一优先级

> **先让 spinfield 从本地目录读取模型，通过 dingospeed 上传并发布，再用标准 `huggingface_hub` 客户端完整下载回来。**

本期只证明两个项目的真实链路能通。安全加固、通用化、完整产品界面、长期部署形态和未来扩展，全部给这个目标让路。

本环成功标准是一次真实、可重复、可独立验证的演示，不是一份面向生产的完整设计。

### 0.2 与其他文档的关系

| 文档 | 关系 |
|---|---|
| [spinfield-integration-requirements.md](./spinfield-integration-requirements.md) | 目标态。安全边界、部署形态、回调协议、乐观锁、残留回收仍有价值，但不属于环 0；与本文冲突时以本文为准 |
| [model-upload-requirements.md](./model-upload-requirements.md)、[batch-upload-requirements.md](./batch-upload-requirements.md) | dingospeed 已交付能力的定义；本期不修改、不放宽其约束 |
| spinfield `docs/prototype/model-upload-fields.md` | 字段与交互参考，不是环 0 必须完整实现的范围 |
| spinfield `docs/prototype/model-upload.html` | 可运行交互原型；环 0 只复用目录浏览、启动、进度和完成态需要的部分，不要求完整移植 |

### 0.3 判断是否跑偏

如果某项工作不能直接帮助完成下面这条链路，它就不应阻塞环 0：

```text
选择本地目录 → spinfield 后端扫描/算摘要/上传 → dingospeed 发布
→ 返回 commit → huggingface_hub 下载 → 与源目录逐字节一致
```

回调签名、源根白名单、RBAC、乐观锁、断点续传、暂停、完整仓库管理页、五 tab 详情、全量模型元信息、MySQL、Kubernetes、跨主机部署，都不属于环 0。

### 0.4 【最高优先级门禁】文档与真实代码冲突时怎么处理

需求分析不可能提前覆盖已有代码的全部细节。本文中的接口判断、代码结构建议、启动方式和实现推断，都可能在真实阅读或实施时遇到新的代码事实。

任何阅读者或实现 Agent 一旦发现“真实代码/运行结果”与本文存在冲突、本文前提无法确认，或按本文方案继续会明显改变范围，必须遵守以下门禁：

1. **先停止继续实现冲突部分**，不得用猜测、静默改需求或额外抽象绕开矛盾。
2. 用成本最低的只读检查、现有测试、最小复现或实际请求，验证本文方案的**正确性与可行性**。
3. 若验证通过，可以继续，并记录验证依据。
4. 若验证不通过、结论仍不明确，或解决办法会改变本环接口/范围/验收，必须立即停止当前实施会话，明确报告：
   - 文档怎么写；
   - 真实代码或运行结果是什么；
   - 已做过哪些验证；
   - 冲突会影响什么；
   - 需要用户决定什么。
5. **报告后不得自行继续，不得自行选择替代方案，也不得把失败项降级。必须等待需求提出者（本文中的“用户”）发送一条新消息来解决这个矛盾；收到该消息后才能继续。**

这条门禁优先于“尽快完成”“保持会话连续”“按经验做合理假设”等一般执行倾向。它不要求为未来做过度设计，只要求在已发现的事实冲突上不盲目推进。

---

## 1. 已核实的现状

### 1.1 dingospeed：上传和发布能力已经存在

| 能力 | 现状 | 接口 |
|---|---|---|
| 单文件暂存上传 | 已实现 | `POST /api/local-upload/:repoType/:org/:repo/:revision/*?size=&sha256=&defer=true` |
| 批量原子发布 | 已实现 | `POST /api/local-publish/:repoType/:org/:repo/:revision` |
| 上传进度 / 续传偏移 | 已实现，但环 0 不使用 | `GET /api/local-upload-progress/...?sha256=` |
| 内容按 SHA-256 寻址与去重 | 已实现 | 上传响应 `blobReused` |
| HF 兼容仓库元数据 | 已实现 | `GET /api/:repoType/:org/:repo/revision/:revision` |
| HF 兼容仓库文件树 | 已实现 | `GET /api/:repoType/:org/:repo/tree/:revision` |
| 凭证 | 已实现 | 请求头 `X-Dingo-Upload-Token`；`upload.token` 为空时上传关闭 |

#### 已确认的例外：`GET /repos` 不是 JSON API

当前 `GET /repos` 渲染并返回 `repos.html`，不能直接作为 spinfield 前端仓库列表的数据源。因此环 0：

- 不代理 `GET /repos`；
- 不实现通用仓库列表；
- 不实现仓库容量页和五 tab 详情；
- 完成态只展示本次任务的 `repo / revision / commit`，并允许按这三个已知值读取元数据和文件树。

dingospeed 元数据已经包含 `usedStorage`。未来若恢复容量展示，直接使用该字段，不需要再次汇总文件树。

**结论：dingospeed 本期只改本机配置，不改代码。**

### 1.2 dingospeed 没有“一个请求传多个文件”的接口

批量的是发布，不是字节传输：

```text
for 每个文件:
    POST /api/local-upload/...?...&defer=true   # 一次只传一个文件

全部暂存完成后:
    POST /api/local-publish/...                # 只传清单，不传文件字节
```

环 0 不新增 multipart、tar、zip 或自定义归档协议。

### 1.3 spinfield：有控制台和 adminapi 地基，但没有模型上传链路

| 能力 | 现状 |
|---|---|
| admin REST API | chi 路由，`/admin/v1/*`，已有 Bearer token 机制 |
| 控制台 | React 18 + CRA/craco + Tailwind + react-router |
| 流式推送先例 | 有 SSE，但环 0 使用 1 秒轮询 |
| 模型上传 API / 状态机 | 不存在 |
| 模型上传 React 页面 | 不存在 |
| 交互原型 | 存在：`docs/prototype/model-upload.html` |
| 无 Kubernetes 的启动方式 | 不存在；现有 main 在创建 adminapi 前会初始化 controller-runtime manager |

#### 已确认的例外：CRA 当前没有 3000 → 8082 代理

真实 adapter 在 `REACT_APP_USE_MOCK=false` 时默认使用同源空地址，但当前 `package.json` / `craco.config.js` 没有开发代理。环 0 必须选择一个真实可运行方式：

1. **本地开发推荐**：给 CRA 增加 `http://127.0.0.1:8082` 代理，前端继续使用同源空地址；
2. 或先构建前端，再由 adminapi 在 8082 同源提供。

环 0 默认采用方案 1。不要仅设置跨域 base URL 后再额外设计 CORS。

### 1.4 本机约束

| 项 | 状态 |
|---|---|
| 操作系统 | Windows |
| Go / Node / npm | 已安装 |
| Kubernetes | 不可用 |
| MySQL | 不需要 |
| dingofs / 共享挂载 | 不需要 |

---

## 2. 环 0 唯一目标与出口

在本机完成一次演示：

1. 启动 dingospeed、spinfield api-only 后端和 spinfield 控制台；
2. 在最小模型上传页面填写仓库名和 revision，浏览并选择一个本机模型目录；
3. 启动任务，看到逐文件 hashing / uploading 状态和总进度；
4. 完成后看到 `repo / revision / commit`；
5. 在 dingospeed blob 目录看到按 SHA-256 落盘的内容；
6. 用标准 `huggingface_hub` 客户端从 dingospeed 下载整个仓库，与源目录逐文件、逐字节一致。

**六步全绿，环 0 立即结束。**

仓库管理页、完整元信息、任务历史、去重展示优化等即使尚未完成，也不得阻止环 0 结束。

---

## 3. 范围与硬约束

### 3.1 dingospeed 零代码改动

本期仅配置：

```yaml
upload:
    host: 127.0.0.1
    port: 8091
    token: "dev-token-change-me"
    namespace: dingo-local
    concurrentLimit: 4
    publishMaxFiles: 1000
```

### 3.2 行为约束：spinfield 上传链路必须能脱离 Kubernetes 运行

要求的是运行结果，不预先锁死代码组织：

- 模型扫描、哈希、上传、发布不得依赖 Kubernetes 类型或客户端；
- 新增模型上传路由不得访问 `s.Client` / `s.Clientset`；
- 必须有不创建 controller-runtime manager、不读取 kubeconfig 即可启动模型上传 API 的入口；
- 如果采用 `main --api-only`，分支必须发生在 `ctrl.GetConfigOrDie()` / `ctrl.NewManager()` 之前。

可以用独立包、main 早分支或其他更小的实现。不要为了“一定只有一个二进制”或“一定使用某种包结构”扩大改动。

### 3.3 本期明确不做

| 不做 | 说明 |
|---|---|
| 浏览器上传文件字节 | 本期只允许后端读取本地目录 |
| 前端计算 SHA-256 | 本期文件内容不经过浏览器 |
| 源路径白名单、路径逃逸、TOCTOU 加固 | 安全后置；只保留跑通所需的基本路径正确性 |
| 断点续传、暂停、取消、重试、任务恢复 | 失败后重新创建任务 |
| SSE | 1 秒轮询足够 |
| MySQL、资产表、完整元信息落库 | 使用内存任务对象 |
| MockAdapter 的模型上传实现 | 直接接真实后端；mock 不是前置门槛 |
| 任务列表 | 只查询当前任务 |
| 仓库列表、容量页、五 tab 详情 | `/repos` 不是 JSON，且这些不影响链路 |
| 四步向导完整移植 | 只复用最小必要交互 |
| README 表单生成文件 | 目录中真实存在 `README.md` 时按普通文件上传 |
| Kubernetes、跨主机、dingofs、回调协议 | 后续再做 |
| 任何生产安全承诺 | 环 0 仅限本机演示 |

### 3.4 环 0 的仓库写入语义

环 0 只保证：

- 首次上传一个新的 `repo + revision`；
- 或对相同 `repo + revision` 重复提交完全相同的目录，得到幂等结果。

不同内容覆盖、删除旧文件、冲突策略和 baseCommit 不属于环 0。前端不提供 `conflictPolicy`，spinfield 对 dingospeed 固定使用 `overwrite=false`。

---

## 4. 最小数据流

```text
浏览器                    spinfield 后端                      dingospeed
  |                            |                                  |
  |-- 浏览目录 ---------------->|                                  |
  |<-- 当前目录条目 ------------|                                  |
  |                            |                                  |
  |-- 创建任务 ---------------->|                                  |
  |   {name, revision, sourceDir}                                  |
  |                            |-- 同步递归扫描 + stat              |
  |<-- 202 {taskId,...} -------|   空目录/非法项/超上限则不建任务    |
  |                            |                                  |
  |                            |   对每个文件串行：                  |
  |                            |   1. 读取并计算 SHA-256             |
  |                            |   2. 再次打开并流式上传 ---------->|
  |                            |      defer=true                   |
  |                            |<-- staged / blobReused -----------|
  |                            |                                  |
  |-- 每秒轮询任务 ------------>|                                  |
  |<-- 状态/进度/文件状态 -------|                                  |
  |                            |                                  |
  |                            |-- local-publish 清单 ------------>|
  |                            |<-- commit ------------------------|
  |<-- succeeded + commit -----|                                  |
```

逐文件执行“算摘要 → 立即上传”，本期串行，不做并发优化。

---

## 5. spinfield 最小接口

### 5.1 `GET /admin/v1/local-fs?path=`

返回指定目录的直接子项：

```json
{
  "path": "D:\\models",
  "entries": [
    {"name": "demo", "directory": true, "size": 0},
    {"name": "README.md", "directory": false, "size": 1234}
  ]
}
```

要求：

- 区分目录与普通文件；
- 只负责浏览，不在前端递归整个目录；
- 本期不做源根白名单；
- 复用现有 `/admin/v1/*` Bearer token 机制，不新建鉴权体系。

### 5.2 `POST /admin/v1/model-uploads`

请求只包含：

```json
{
  "name": "demo-model",
  "revision": "v1.0.0",
  "sourceDir": "D:\\models\\demo-model"
}
```

**不得由前端提交 `files[]`、size 或 sha256。** 后端同步递归扫描 `sourceDir`，生成唯一文件清单并完成基本预检，然后再创建任务。

同步拒绝：

- 目录不存在或不是目录；
- 目录为空；
- 条目无法读取或不是普通文件；
- 文件数超过 dingospeed `publishMaxFiles`；
- 仓库名或 revision 不符合 dingospeed 现有规则。

成功返回：

```json
{
  "taskId": "...",
  "phase": "created",
  "totalFiles": 8,
  "sourceBytes": 123456789,
  "totalWorkBytes": 246913578,
  "processedWorkBytes": 0
}
```

### 5.3 `GET /admin/v1/model-uploads/{taskId}`

返回当前任务快照：

```json
{
  "taskId": "...",
  "name": "demo-model",
  "revision": "v1.0.0",
  "phase": "transferring",
  "totalFiles": 8,
  "doneFiles": 3,
  "sourceBytes": 123456789,
  "totalWorkBytes": 246913578,
  "processedWorkBytes": 80000000,
  "files": [
    {
      "path": "subdir/config.json",
      "size": 1234,
      "status": "staged",
      "processedWorkBytes": 2468
    }
  ],
  "commit": "",
  "changed": false,
  "error": null
}
```

任务状态：

```text
created → transferring → publishing → succeeded | failed
```

文件状态：

```text
pending → hashing → uploading → staged | reused | failed
```

### 5.4 进度口径

每个文件会读取两遍：哈希一次、上传一次。因此：

- `sourceBytes = 所有源文件大小之和`；
- `totalWorkBytes = sourceBytes × 2`；
- `processedWorkBytes = 已哈希字节 + 已上传字节`；
- 单文件 `processedWorkBytes` 范围是 `0..2×size`；
- UI 总进度为 `processedWorkBytes / totalWorkBytes`。

若 dingospeed 直接走 blob 复用快路径而没有完整读取 request body，spinfield 在收到成功且 `blobReused=true` 的响应后，必须把该文件的上传部分进度直接记为 `size`，保证成功任务最终达到 100%。

禁止继续使用含义不清的单个 `doneBytes`，避免上传阶段总进度超过 100%。

发布阶段不虚构字节进度，只展示阶段状态。

### 5.5 MemStore 最低正确性

任务在后台 goroutine 更新，前端同时轮询。内存 Store 必须：

- 用 mutex 或等价方式保证并发安全；
- 查询时返回快照副本，不能把正在修改的 map/slice 直接暴露给 handler；
- 进程重启丢任务可接受。

这是运行正确性要求，不是持久化或架构设计。

---

## 6. 文件扫描、路径和上传规则

### 6.1 后端扫描目录

后端从 `sourceDir` 递归生成文件清单。前端只选择目录，不递归发请求，也不提交相对路径数组。

扫描完成并预检通过后才返回 202；随后异步执行哈希、上传和发布。

### 6.2 Windows 路径必须转换成仓库路径

本机是 Windows，而 dingospeed 拒绝带反斜杠的仓库内路径。相对路径必须这样生成：

```go
rel, err := filepath.Rel(sourceDir, absoluteFile)
if err != nil {
    // reject task
}
repoPath := filepath.ToSlash(rel)
```

要求：

- 本地读取使用操作系统路径；
- 发给 dingospeed 的 `*` 和 publish 清单 `path` 使用 `/`；
- 保留子目录结构；
- 构造 HTTP URL 时逐段进行 URL 编码，不把本地绝对路径放进 URL；
- 目录里真实存在的 `README.md` 与其他文件完全同等处理。

### 6.3 后端计算 SHA-256

```go
h := sha256.New()
f, err := os.Open(absPath)
if err != nil {
    // fail task
}
reader := newProgressReader(f, updateHashProgress) // 包装 Read 并累计实际读取字节
_, err = io.Copy(h, reader)
sum := hex.EncodeToString(h.Sum(nil))
```

具体计数包装可以不同，但必须满足：

- 内存占用与文件大小无关；
- 不使用 `io.ReadAll`；
- 不引入临时文件；
- 不引入额外哈希库；
- 哈希后重新打开源文件，以流式 request body 上传。

### 6.4 spinfield → dingospeed 映射

| dingospeed 参数 | 来源 / 取值 |
|---|---|
| `repoType` | 固定 `models` |
| `org` | 固定 `dingo-local`，必须与 `upload.namespace` 一致 |
| `repo` | 请求 `name` |
| `revision` | 请求 `revision` |
| 仓库内路径 | `filepath.ToSlash(relativePath)` |
| `size` | 后端 stat |
| `sha256` | 后端计算出的 64 位小写 hex |
| `defer` | 固定 `true` |
| `overwrite` | 固定 `false` |
| 上传 token | spinfield 后端配置，不下发浏览器 |

仓库名和 revision 前后端都按 dingospeed 已有规则即时校验：

```text
^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$
```

不做静默替换或“智能规范化”；不合法就明确报错。

### 6.5 自动发布

所有文件 staged/reused 后，spinfield 自动调用一次：

```text
POST /api/local-publish/models/dingo-local/{repo}/{revision}
```

请求体为：

```json
{
  "files": [
    {"path": "config.json", "sha256": "...", "size": 1234}
  ]
}
```

发布响应中的 `commit` 是任务核心产出。`changed=false` 表示重复提交没有产生新快照，任务仍然是 succeeded。

### 6.6 最低失败处理

| 情况 | 行为 |
|---|---|
| 创建任务时扫描失败 | 同步拒绝，不创建任务，返回具体路径和原因 |
| 哈希或读取失败 | 任务 failed，记录失败文件和错误 |
| 单文件上传失败 | 任务 failed，保留 dingospeed 返回的 `code` 和 `error` |
| 发布失败 | 任务 failed，保留 dingospeed 返回的 `code` 和 `error` |
| 任务失败后 | 用户重新创建任务；本期不自动重试 |

spinfield 自身错误不需要建立完整错误码体系，返回可定位问题的阶段、文件路径和原始错误即可。

---

## 7. 最小前端

### 7.1 页面只需要这些元素

- 仓库名输入框；
- revision 输入框；
- 目录浏览器；
- “开始上传”按钮；
- 当前阶段；
- 总进度；
- 逐文件路径与 hashing/uploading/staged/reused/failed 状态；
- 完成态 `repo / revision / commit`；
- 失败态原始错误。

可以从 `docs/prototype/model-upload.html` 移植这些交互和样式，但不得把完整四步向导、全量字段、任务列表或仓库五 tab 作为前置工作。

### 7.2 直接接真实后端

模型上传功能不要求先实现 MockAdapter。环 0 应尽早设置：

```powershell
$env:REACT_APP_USE_MOCK='false'
```

并让 CRA 把 `/admin/*` 代理到 `http://127.0.0.1:8082`。`REACT_APP_ADMIN_API_BASE` 保持为空，走同源代理。

若真实后端尚未实现，可以先做静态布局，但 mock 不得成为真实链路之前的硬性里程碑。

---

## 8. 本机启动与冒烟

### 8.1 端口

| 服务 | 地址 |
|---|---|
| dingospeed 下载 | `http://127.0.0.1:8090` |
| dingospeed 上传 | `http://127.0.0.1:8091` |
| spinfield api-only | `http://127.0.0.1:8082` |
| spinfield CRA | `http://127.0.0.1:3000` |

### 8.2 构建并启动 dingospeed

在 `D:\project\workplace\ModelManager\dingospeed`：

```powershell
go build -o .\bin\dingospeed.exe .\cmd
.\bin\dingospeed.exe -config .\config\config.yaml
```

启动前确认实际使用的配置文件里 `upload.token` 非空，且 namespace 是 `dingo-local`。

### 8.3 动代码前先冒烟 dingospeed

以下示例动态计算真实大小和摘要，不允许写死 `size=1234`：

```powershell
$smokeFile = (Resolve-Path .\config.json).Path
$smokeSize = (Get-Item -LiteralPath $smokeFile).Length
$smokeSha = (Get-FileHash -Algorithm SHA256 -LiteralPath $smokeFile).Hash.ToLowerInvariant()
$uploadURL = "http://127.0.0.1:8091/api/local-upload/models/dingo-local/smoke/v1/config.json?size=$smokeSize&sha256=$smokeSha&defer=true"

curl.exe -sS -X POST `
  -H "X-Dingo-Upload-Token: dev-token-change-me" `
  --data-binary "@$smokeFile" `
  $uploadURL

$publishBody = @{
  files = @(@{path = "config.json"; sha256 = $smokeSha; size = $smokeSize})
} | ConvertTo-Json -Depth 4 -Compress

Invoke-RestMethod -Method Post `
  -Uri "http://127.0.0.1:8091/api/local-publish/models/dingo-local/smoke/v1" `
  -Headers @{"X-Dingo-Upload-Token" = "dev-token-change-me"} `
  -ContentType "application/json" `
  -Body $publishBody
```

curl 上传和 publish 都成功后，再开始写 spinfield。若失败，先按 §0.4 验证配置和接口，不得继续堆 spinfield 代码。

### 8.4 启动 spinfield api-only

具体命令以最终实现为准，但必须满足：

```powershell
# 示例
go build -o .\bin\spinfield.exe .\cmd
$env:SPINFIELD_ADMIN_TOKEN='dev-admin-token'
$env:DINGOSPEED_UPLOAD_BASE='http://127.0.0.1:8091'
$env:DINGOSPEED_DOWNLOAD_BASE='http://127.0.0.1:8090'
$env:DINGOSPEED_UPLOAD_TOKEN='dev-token-change-me'
.\bin\spinfield.exe --api-only
```

若实际 flag 或环境变量命名与此不同，应在实现时同步更新本文。若现有代码事实导致该入口不能按行为约束实现，执行 §0.4 门禁。

### 8.5 启动控制台

在 `D:\project\workplace\spinfield\web\console`：

```powershell
$env:REACT_APP_USE_MOCK='false'
Remove-Item Env:REACT_APP_ADMIN_API_BASE -ErrorAction SilentlyContinue
npm start
```

前提是 CRA 代理已经指向 `http://127.0.0.1:8082`。

---

## 9. 验收

### 9.1 核心验收：全部是阻塞项

| # | 验收项 | 证据 |
|---|---|---|
| 1 | spinfield api-only 在无 Kubernetes 环境启动，控制台能访问真实接口 | 启动日志 + health 请求 |
| 2 | 页面能浏览目录、填写 name/revision 并创建任务 | 实际操作 |
| 3 | hashing/uploading 进度真实推进，最终显示 commit | 页面或接口快照 |
| 4 | dingospeed blob 目录存在对应 SHA-256 文件，大小正确 | PowerShell 文件与哈希输出 |
| 5 | `huggingface_hub` 从 `http://127.0.0.1:8090` 下载成功 | 标准客户端输出 |
| 6 | 下载目录与源目录文件集合、相对路径、大小和 SHA-256 全部一致 | 独立校验脚本输出 |

只看前端 succeeded 不算验收通过。

标准客户端必须显式指向本机 endpoint，例如：

```python
from huggingface_hub import snapshot_download

path = snapshot_download(
    repo_id="dingo-local/demo-model",
    revision="v1.0.0",
    endpoint="http://127.0.0.1:8090",
)
print(path)
```

### 9.2 非阻塞补充验证

核心六项通过后，如成本很低，可以再验证：

- 同一目录含子目录时结构保持；
- 两个内容相同的文件出现 `blobReused=true`；
- 相同 `repo + revision + 目录` 重复执行时 `changed=false` 且 commit 不变。

这些验证失败需要记录，但不得在核心链路尚未打通前抢占实现时间。若失败暴露了文档与真实行为冲突，仍执行 §0.4。

---

## 10. 环 0 明确欠下的债

| 债 | 后果 | 后续 |
|---|---|---|
| 目录浏览无源根白名单 | 能浏览后端进程可读路径 | 安全环 |
| 上传 token 明文放在本机后端配置/环境变量 | 不适合长期环境 | 安全环 |
| 共享 admin token，无真实用户 | 无法审计操作者 | 未排期 |
| 任务状态仅在内存 | 重启丢任务 | 持久化环 |
| 无断点续传、取消、暂停、重试 | 大文件失败要重来 | 大文件环 |
| 无 baseCommit / 覆盖 / 删除语义 | 只适合新 revision 或相同内容重跑 | 并发与版本环 |
| 无资产表和完整元信息 | 不能作为模型管理产品使用 | 资产环 |
| 无通用仓库列表与容量页 | 只能查看本次任务结果 | 资产环；需先解决 JSON 列表来源 |
| 未发布内容会暂时占盘 | 失败可能留下暂存内容 | 依赖 dingospeed 现有回收 |
| 仅单机、仅 models、仅本地路径 | 来源和部署受限 | 后续扩展 |

> 环 0 产物仅用于本机验证，不进入长期运行环境。

---

## 11. 后续分环（不约束环 0 实现）

| 环 | 内容 | 出口 |
|---|---|---|
| 环 0 | 本文：最小页面 + 本地目录 + 后端哈希/上传/发布 + HF 下载校验 | §9.1 六项全绿 |
| 大文件环 | 断点续传、取消/暂停/重试、任务恢复、SSE | 真实 30GB 模型可控上传 |
| 资产环 | MySQL、模型资产表、完整元信息、任务历史、仓库列表/详情/容量 | 可作为模型仓库管理功能使用 |
| 安全环 | 源根白名单、路径边界、密钥管理、用户/RBAC、审计 | 可进入受控长期环境 |
| 部署扩展环 | 跨主机、dingofs、其他来源、回调协议、baseCommit | 按目标态文档验收 |

后续环的顺序不是环 0 的前置条件。安全后置意味着先证明回路存在，不意味着环 0 已具备生产安全性。
