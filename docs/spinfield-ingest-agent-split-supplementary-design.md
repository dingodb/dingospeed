# local-ingest agent 独立化——增补设计（迁移环 2）

| 项目 | 内容 |
|---|---|
| 文档版本 | v1.0 |
| 状态 | 待实施 |
| 日期 | 2026-08-13 |
| 需求基线 | `spinfield-api-only-dingospeed-host-migration-requirements.md` v1.0（环 1） |
| 继承设计 | `spinfield-api-only-dingospeed-host-migration-detailed-design.md` v1.0（环 1） |
| 涉及仓库 | `spinfield`（全部实现），`dingospeed`（零改动，仅本文档） |
| 设计性质 | 在环 1 已验证链路之上做角色与凭证收敛；不改上传/发布/下载协议 |

---

## 0. 为什么还要做这一环

环 1 已经把"扫描 + SHA + 流式上传"放到数据所在主机，这部分是标准的控制面/数据面分离，不需要推翻。留下的三个问题都不是拓扑问题，是**角色和凭证**问题：

| 环 1 现状 | 问题 |
|---|---|
| `spinfield.exe --api-only` 与控制面是同一交付物 | 运维会误认为装了第二个控制面；两台机器被迫同版本升级；controller/jenkins/mysql 代码被带到数据节点 |
| agent 与控制面共用 `SPINFIELD_ADMIN_TOKEN` | 数据节点持有控制面管理凭证，A 被攻破可直接操作 B 的部署/zone/模板接口 |
| 同源网关按 3 条精确 path/method 把 `/admin/v1` 拆到两个后端 | 同一 URL 前缀背后两个进程，排障不直观；每加一个上传相关端点都要改网关 |

对照 AWS DataSync Agent、GitLab Runner、Nomad Client、Kubernetes DaemonSet 这些先例：agent 与源数据同机是共同点，但**没有一个先例让 agent 持有控制面管理凭证，也没有一个让浏览器直接访问 agent**。本环只补齐这两点。

### 0.1 与环 1 的关系

本文只改"谁是 agent、谁信任谁、请求怎么进来"。以下环 0/环 1 结论**全部保留，不得借本环改动**：上传协议、`defer=true`、blob 内容寻址、一次性 publish、`overwrite=false`、幂等语义、双遍真实进度、MemStore、任务不绑请求 context、标准 `huggingface_hub` 下载。

### 0.2 冲突门禁

沿用环 1 需求 §0.3：本文与真实代码/权限/网络冲突时，先做最低成本只读验证；若结论不明确或替代方案会改变接口、信任边界或验收条件，停止并上报，不得自行决定。

---

## 1. 目标形态

```text
浏览器
→ 现有 HTTPS 同源网关（单 upstream，恢复环 0 之前的简单形态）
→ spinfield 控制面（唯一对浏览器暴露的后端）
→ 服务间调用：控制面 --Bearer SPINFIELD_INGEST_TOKEN--> ingest agent（机器 A 私网:8083）
→ agent 读取本机源目录、算 SHA、流式 POST 127.0.0.1:8091
→ dingospeed staging + publish
→ huggingface_hub 从 8090 下载
```

三条外部 API 路径、请求体、响应体、状态机**保持不变**，前端零改动。变化只有两处：这三条 API 的**外部落点回到控制面**，控制面把它们**转发**给 agent；agent 只认自己的凭证。

### 1.1 决策表

| 决策 | 结论 |
|---|---|
| agent 交付物 | 新增独立二进制 `spinfield-ingest`（`cmd/ingest-agent`），与 `spinfield` 分开构建、分开版本发布 |
| `--api-only` | 从 `spinfield` 主二进制**移除** |
| 代码复用 | `internal/modelupload` 保持共享包不动；HTTP 外壳新建 `internal/ingestserver`，不再复用 `adminapi.Server` |
| agent 凭证 | 新增 `SPINFIELD_INGEST_TOKEN`，与 `SPINFIELD_ADMIN_TOKEN` 无关，不得复用同值（启动时检查并拒绝相同值） |
| 浏览器可达性 | 浏览器**不可**直达 agent；agent 只接受来自控制面的服务间调用和运维健康检查 |
| 网关 | 恢复单 upstream，删除三条精确路由；`deploy/local-ingest-agent/gateway/` 模板作废 |
| 控制面角色 | 三个模型上传 API 的**纯 JSON 反向代理**，不读文件、不落盘、不缓冲文件字节（这三条 API 本来就没有文件字节） |
| dingospeed | 零改动 |
| 任务状态 | 仍在 agent 的 MemStore；控制面不持久化、不重试、不聚合 |
| 多节点 | 仍不支持；控制面只配置一个 agent endpoint |
| agent 连接方向 | 本环仍是控制面**入站**调用 agent。出站注册/领取任务（Runner/Nomad 模式）明确留到后续环 |

### 1.2 本环明确不做

- 不实现 agent 注册、节点列表、心跳、负载均衡、任务迁移；
- 不实现任务持久化、断点续传、重试、取消；
- 不改 dingospeed 任何 Go 代码或协议；
- 不引入 mTLS、短期票据、用户级 RBAC（列入后续环）；
- 不让控制面获得源目录访问能力，不做本地扫描回落；
- 不改前端页面结构和 API 契约。

---

## 2. 组件与信任边界

| 组件 | 监听 | 谁能访问 | 持有的凭证 |
|---|---|---|---|
| 同源网关 | 现有 HTTPS | 浏览器 | 无（透传 Authorization） |
| spinfield 控制面（机器 B） | 现有 `:8082` | 网关 | `SPINFIELD_ADMIN_TOKEN`（校验用户）、`SPINFIELD_INGEST_TOKEN`（出站调用 agent） |
| spinfield-ingest（机器 A） | `:8083`，绑私网地址 | 只有控制面 IP 和运维健康检查 | `SPINFIELD_INGEST_TOKEN`（校验入站）、`DINGOSPEED_UPLOAD_TOKEN`（出站调 8091） |
| dingospeed 上传 | `127.0.0.1:8091` | 只有同机 agent | 自身 upload token |
| dingospeed 下载 | `:8090` | 按现有 HF 下载策略 | 无 |

信任关系是单向链：浏览器 → 控制面 → agent → dingospeed。**任何一环都不持有上游的凭证。**机器 A 被攻破的最大影响退化为"能写这台 dingospeed 的 blob"，不再包含控制面管理权限。

监听地址要求：agent 默认 `SPINFIELD_INGEST_ADDR=:8083`；生产必须绑到私网网卡地址而不是 `0.0.0.0`，并由主机防火墙只放行控制面 IP。端口从 8082 改为 8083 是刻意的——避免与控制面同端口造成运维混淆。

---

## 3. 代码结构

### 3.1 新增

```text
cmd/ingest-agent/main.go          # agent 入口，构建出 spinfield-ingest(.exe)
cmd/ingest-agent/config.go        # agent 配置加载与校验（从 cmd/api_only_config.go 迁移改造）
cmd/ingest-agent/config_test.go
internal/ingestserver/server.go   # agent HTTP 外壳：/healthz + 3 条 API + ingest token 中间件
internal/ingestserver/routes.go   # 3 个 handler（从 internal/adminapi/model_uploads.go 迁移，逻辑不变）
internal/ingestserver/server_test.go
internal/ingestclient/client.go   # 控制面侧的 agent 客户端（JSON 转发）
internal/ingestclient/client_test.go
```

### 3.2 删除

```text
cmd/api_only_config.go / _test.go        # 迁移到 cmd/ingest-agent/
cmd/main.go 中的 --api-only 分支与 flag
internal/adminapi/server.go 中的 APIOnly 字段、NewAPIOnlyServer、APIOnly 提前 return 分支
internal/adminapi/model_uploads.go 中的本地实现（改为调用 ingestclient）
deploy/local-ingest-agent/gateway/                # 网关三路由模板整体删除
```

`internal/adminapi/server.go` 的 `installNotFound` 抽取保留（它本身是好的重构）。

### 3.3 保持不动

`internal/modelupload/**` 全部保持现状，包括路径白名单、逐级 reparse 校验、`openValidatedSourceFile` 的 Lstat→Open→Fstat+SameFile 三段校验、MemStore、双倍 work bytes 进度口径。本环不重写这些逻辑，只换调用它的外壳。

---

## 4. agent 设计（机器 A）

### 4.1 配置

| 环境变量 | 必填 | 默认 | 说明 |
|---|---:|---|---|
| `SPINFIELD_INGEST_TOKEN` | 是 | — | 入站服务间凭证；长度 ≥ 32；与 `SPINFIELD_ADMIN_TOKEN` 相同值时**启动失败** |
| `SPINFIELD_INGEST_ADDR` | 否 | `:8083` | 监听地址，校验规则沿用环 1 的 `validateAdminAddr` |
| `SPINFIELD_LOCAL_FS_ROOTS` | 是 | — | 允许根，`filepath.SplitList` 分隔，规则不变 |
| `SPINFIELD_LOCAL_FS_START_DIR` | 否 | 第一个根 | 必须在允许根内 |
| `DINGOSPEED_UPLOAD_BASE` | 是 | — | 必须是 loopback host，校验不变 |
| `DINGOSPEED_DOWNLOAD_BASE` | 是 | — | 不变 |
| `DINGOSPEED_UPLOAD_TOKEN` | 是 | — | 只存在于机器 A |
| `DINGOSPEED_PUBLISH_MAX_FILES` | 否 | 1000 | 与 dingospeed 一致 |

agent **不再读取** `SPINFIELD_ADMIN_TOKEN`；若检测到该变量被设置，启动时打印一条告警（提示这是控制面变量，agent 不需要），但不失败。

### 4.2 路由与鉴权

```text
GET  /healthz                              免鉴权
GET  /admin/v1/local-fs?path=              需要 Bearer SPINFIELD_INGEST_TOKEN
POST /admin/v1/model-uploads               需要 Bearer SPINFIELD_INGEST_TOKEN
GET  /admin/v1/model-uploads/{taskId}      需要 Bearer SPINFIELD_INGEST_TOKEN
其他任何路径                                JSON 404
```

保持 `/admin/v1/...` 路径不变，是为了让控制面转发时不必改写路径，也便于直接对 agent 做诊断请求。

- 使用 `crypto/subtle.ConstantTimeCompare` 比较 token；
- 空 token 不再等于"关闭鉴权"——配置校验阶段就要求非空，中间件里再兜底拒绝；
- agent **不提供** `/admin/v1/login`，不嵌入控制台 SPA（`internal/console` 不再被 agent 引用）。诊断改用 curl/PowerShell，见 §7.3。

### 4.3 启动与停止

```text
flag.Parse → logger → root context(SetupSignalHandler)
→ 配置校验（token、addr、roots、loopback upload base）
→ NewMemStore / NewHTTPDingoClient / modelupload.NewService
→ ingestserver.Start(rootCtx) 阻塞
```

不得 import `sigs.k8s.io/controller-runtime` 的 manager、client-go、MySQL driver 或 `internal/console`。**这一条要有强制测试**（见 §8.1 IMP-01）。

---

## 5. 控制面设计（机器 B）

### 5.1 配置

| 环境变量 | 必填 | 说明 |
|---|---:|---|
| `SPINFIELD_INGEST_ENDPOINT` | 否 | 例 `http://10.x.x.x:8083`。未设置 = 模型上传功能未启用，三条 API 不注册（返回既有 JSON 404） |
| `SPINFIELD_INGEST_TOKEN` | 与 endpoint 同时必填 | 出站 Bearer |

`DINGOSPEED_*` 在控制面仍然**不允许**配置。但把环 1 实现的"检测到就 `os.Exit(1)`"降级为：**打印明确的 ERROR 日志并继续启动**，不注册模型上传路由。理由：一个残留的环境变量不应该让整个 Kubernetes 控制面拒绝启动；可用性影响远大于误配风险，而误配的实际后果（多一个不生效的变量）是零。

### 5.2 转发行为

`internal/ingestclient` 对三条 API 各提供一个方法，行为要求：

- 逐字转发 query（`local-fs` 的 `path=`）和 path 参数（`taskId`）；
- 请求体只做大小限制（64 KiB）后**原样透传**，不做 JSON 改写；
- 出站头只设置 `Authorization: Bearer <SPINFIELD_INGEST_TOKEN>` 和 `Content-Type`；**必须丢弃**浏览器传来的 Authorization，不得转发用户 token 给 agent；
- 响应：status code、JSON body 原样回传；不重写 `error`/`message`/`code`；
- 超时：browse/get 10s，create 120s（同步扫描可能较慢），使用 `http.Client{Timeout}`，并把请求 context 传下去；
- 不做重试（保持环 0 "不自动重试"语义），不缓存，不落盘。

### 5.3 新增失败码

agent 不可达或超时时，控制面返回 **HTTP 502**：

```json
{ "error": "ingest_unavailable", "message": "local ingest agent is unreachable" }
```

未配置 endpoint 时三条路由不存在，走既有 `not_found`。**任何情况下都不得回落到控制面本地扫描**——控制面根本不应该有源目录访问能力。

### 5.4 鉴权链

浏览器侧完全不变：`requireAdminAuth` 用 `SPINFIELD_ADMIN_TOKEN` 校验用户 Bearer，401 行为、登录页跳转不变。校验通过后才发起出站调用。用户 token 与 ingest token 在这里完成隔离交换。

---

## 6. 前端

**零改动。** `services/modelUploads.ts` 继续用相对 `/admin/v1/...`，`apiFetch` 的 Bearer/401 不变。

唯一建议的文案调整（可选，不阻塞验收）：目录浏览器保留环 1 的提示"当前展示的是模型存储节点上的允许导入目录"。新增的 `ingest_unavailable` 错误按现有错误展示逻辑原样显示即可。

---

## 7. 部署物

### 7.1 目录改名

`deploy/local-ingest-agent/` → `deploy/spinfield-ingest/`，内容调整：

| 文件 | 变化 |
|---|---|
| `README.md` | 改为 agent 独立交付物说明；补控制面侧 `SPINFIELD_INGEST_ENDPOINT/TOKEN` 配置段；说明 A/B 可独立升级 |
| `agent.env.example` | 去掉 `SPINFIELD_ADMIN_TOKEN`，加 `SPINFIELD_INGEST_TOKEN`、`SPINFIELD_INGEST_ADDR=:8083` |
| `build-windows.ps1` | 改为 `go build -o bin/spinfield-ingest.exe ./cmd/ingest-agent`；**删除 CRA build 与 console 嵌入步骤**（agent 不再带前端） |
| `spinfield-local-ingest.xml.template` | 服务 ID 改 `spinfield-ingest`，可执行文件改名 |
| `run-agent.ps1` | `allowedKeys` 白名单同步更新；不再传 `--api-only` |
| `gateway/` | **整个删除**，并在 README 里说明网关恢复单 upstream |
| `verify-content.ps1` | 保留 |

### 7.2 控制面构建

`spinfield` 主二进制的构建链不变（Dockerfile 多阶段 + CRA 嵌入）。环 1 把 `cmd/main.go` 改成 `./cmd` 的修改保留（多文件 package 必需）。

### 7.3 诊断入口

agent 不再有内嵌页面。冒烟改用：

```powershell
Invoke-WebRequest -UseBasicParsing http://<agent-private-ip>:8083/healthz
Invoke-RestMethod -Uri 'http://<agent-private-ip>:8083/admin/v1/local-fs' -Headers @{ Authorization = "Bearer $env:SPINFIELD_INGEST_TOKEN" }
```

README 必须写明：这两条命令只能在管理网执行，token 不得出现在共享终端记录或截图里。

---

## 8. 测试设计

### 8.1 新增强制用例

| 编号 | 用例 | 断言 |
|---|---|---|
| IMP-01 | `go list -deps ./cmd/ingest-agent` | 不含 `controller-runtime/pkg/manager`、`k8s.io/client-go`、MySQL driver、`spinfield/internal/console` |
| AUTH-01 | agent 收到正确 ingest token | 200 |
| AUTH-02 | agent 收到控制面 admin token | 401 |
| AUTH-03 | agent 无 Authorization / 错误 token | 401 |
| AUTH-04 | `SPINFIELD_INGEST_TOKEN == SPINFIELD_ADMIN_TOKEN` | 启动失败，错误信息指出两者不得相同 |
| AUTH-05 | ingest token 为空或 < 32 字符 | 启动失败 |
| PROXY-01 | 控制面转发 browse/create/get | status 与 body 与 agent 原样一致 |
| PROXY-02 | 浏览器 Authorization 不被转发 | agent 收到的 Authorization 等于 ingest token，不含用户 token |
| PROXY-03 | agent 不可达 | 502 `ingest_unavailable`，不触碰本地文件系统 |
| PROXY-04 | 未配置 endpoint | 三条路由 JSON 404，其他 admin API 正常 |
| PROXY-05 | 创建请求体 > 64KiB | 413/400，不转发 |
| MGR-01 | 控制面设置了 `DINGOSPEED_*` | 打印 ERROR 日志但**正常启动**，模型上传路由按 endpoint 配置决定 |

### 8.2 保留的环 1 用例

`internal/modelupload` 全部路径/配置/扫描/服务测试原样保留并必须继续通过（CFG-M01..07、PATH-M01..09）。仅需把 `SPINFIELD_ADMIN_TOKEN` 相关的 agent 侧断言换成 ingest token。

### 8.3 顺带修复（本环一并做掉）

1. `internal/modelupload/scan.go` 中 `ResolveAllowedPath` 的错误码映射反了：`os.ErrNotExist` → 应为 `not_found`/`not_a_directory` 之外的语义混淆，且权限错误被误报成 `path_not_allowed`。改为：不存在 → `path_not_found`；权限/IO 错误 → `directory_unreadable`；确实是 link/reparse 或越界 → `path_not_allowed`。三者都保持 HTTP 422（`path_not_found` 用 422，避免与任务 404 混淆），并补测试。
2. Windows 大小写：`allowedRootForPath` 目前依赖 `filepath.Rel` 的分段比较，`D:\MODEL-SOURCE\x` 对根 `D:\model-source` 会被拒绝，与环 1 需求 §7.1「按平台语义比较大小写」不符。改为 Windows 下逐段 `strings.EqualFold` 比较（复用 `samePath` 的思路），并补 PATH-M04 的正向断言。**注意：这是放宽，必须同时保证同名前缀 `models2`、`..`、不同盘符仍然被拒。**
3. 文档口径：环 1 需求 §8.2 第 5 步写的 `hashing/uploading/publishing` 是文件级 status，任务级 `phase` 实际是 `created/transferring/publishing/succeeded/failed`。在本文档的验收脚本里按真实字段写，不要改代码去迁就旧文档措辞。

### 8.4 回归门禁（零失败）

```powershell
go build ./...
go test ./internal/modelupload
go test ./internal/ingestserver
go test ./internal/ingestclient
go test ./cmd/...
go test ./internal/adminapi -run 'TestModelUpload'
go test -race ./internal/modelupload
go test -race ./internal/ingestserver

Set-Location web/console
npm test -- --watchAll=false
npm run build
```

`go test ./internal/adminapi` 全量存在**既有失败**（zones/templates/releases/deployments 的 envtest fixture 冲突，与本环无关）。实施前先在干净工作区跑一次并记录数量作为基线，实施后必须与基线**完全一致**，不得增加，也不得通过修改这些测试来"消除"。

---

## 9. 验收

### 9.1 双主机 E2E（与环 1 相同的物理条件）

1. 机器 A：dingospeed 8090/8091 起，`spinfield-ingest` 起，源目录只在 A 上存在；
2. 机器 B：`spinfield` 控制面起，环境中**没有** `DINGOSPEED_*`，有 `SPINFIELD_INGEST_ENDPOINT/TOKEN`；
3. 网关只有**一个** upstream（控制面），无三路由扇出；
4. 浏览器登录控制台 → 浏览 A 的允许源根 → 创建任务 → 观察 phase 到 `succeeded` 与 commit；
5. `huggingface_hub.snapshot_download` 从 8090 下载；
6. 独立比对路径集合、size、SHA-256、逐字节内容。

### 9.2 出口清单

| # | 验收项 | 证据形式 |
|---:|---|---|
| 1 | 控制面二进制不含 `--api-only` | `spinfield --help` 输出 |
| 2 | agent 是独立二进制且无 K8s/MySQL/console 依赖 | IMP-01 测试输出 |
| 3 | agent 拒绝控制面 admin token | AUTH-02 实际请求的 401 响应 |
| 4 | 用户 token 未流向 agent | agent 侧记录到的 Authorization 值（脱敏后前缀比对） |
| 5 | 网关只有一个 upstream | 网关配置 diff |
| 6 | 浏览器无法直连 agent | 从浏览器所在网段访问 8083 超时/拒绝的记录 |
| 7 | 模型字节未经过控制面 | 控制面访问日志中三条 API 的最大请求体 < 64 KiB |
| 8 | agent→dingospeed 走 127.0.0.1:8091 | agent 日志/netstat |
| 9 | 全链路一致 | snapshot_download 后逐字节比对 MATCH |
| 10 | 越界路径、symlink/junction、错误 token 被拒 | 对应请求的 422/401 响应 |
| 11 | 回归门禁全绿、adminapi 失败数与基线一致 | 测试输出 |

### 9.3 单机验收替代

若暂时只有一台机器，可用同机不同端口 + 不同源根模拟，但必须显式记录"这是单机模拟"，且第 6、7 项验收改为在真实双机环境补做后才能关环。

---

## 10. 回滚

1. 网关本环不变（已是单 upstream），无需回滚流量；
2. 控制面清空 `SPINFIELD_INGEST_ENDPOINT` → 三条 API 立即消失，功能明确不可用；
3. 停止 `spinfield-ingest` 服务；
4. 不删除任何已 staging/已发布的 blob、commit 或源目录；数据清理另行审批。

回滚后不得声称"上传能力已恢复"——源文件只在机器 A，功能就是不可用状态，直到 agent 恢复。

---

## 11. 已知限制与后续

| 限制 | 本环处理 | 后续 |
|---|---|---|
| 控制面入站调用 agent（非出站注册） | 私网 + 防火墙 + 独立 token | Runner/Nomad 式注册与领取任务环 |
| 单 agent、单 endpoint | 控制面只配一个 endpoint | 节点注册与路由亲和环 |
| MemStore，重启丢任务 | 提示重新创建，复用已有 blob | 任务持久化环 |
| 静态长期 token，无 mTLS | 私网 + 长度要求 + 常量时间比较 | mTLS/短期票据环 |
| 双遍读盘 + loopback HTTP | 保持协议不变 | 若确证为瓶颈再评估 dingospeed 本地路径原语 |
| 控制面故障时无法创建新任务 | 已在运行的后台任务不受影响 | — |

---

## 12. 实施顺序（建议）

1. 建 `cmd/ingest-agent` + `internal/ingestserver`，把环 1 逻辑搬过去，先让 agent 独立跑通（不动控制面）；
2. 跑通 agent 侧单机 E2E（curl 三接口 + snapshot_download）；
3. 建 `internal/ingestclient`，改造 `internal/adminapi/model_uploads.go` 为转发；
4. 从 `cmd/main.go` 和 `adminapi.Server` 移除 `--api-only`/`APIOnly`；
5. 降级控制面 `DINGOSPEED_*` 的 fail-fast；
6. §8.3 三项顺带修复；
7. 部署物改名与网关模板删除；
8. 全量回归 + 单机 E2E + 证据归档。

每一步结束都应能独立构建和测试通过，不要攒到最后一次性验证。
