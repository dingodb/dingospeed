# spinfield api-only 迁移到 dingospeed 主机——需求文档（迁移环 1）

| 项目 | 内容 |
|---|---|
| 文档版本 | v1.0 |
| 日期 | 2026-08-13 |
| 文档性质 | 环 0 之后的部署拓扑迁移需求 |
| 需求基线 | `spinfield-e2e-walkthrough-requirements.md` v2.2 |
| 涉及仓库 | `spinfield`（实现、构建、部署配置），`dingospeed`（运行配置与本文档） |
| 目标拓扑 | 源文件与 spinfield api-only 位于 dingospeed 主机；模型字节不经过 spinfield 控制面主机 |

---

## 0. 背景、优先级与冲突门禁

### 0.1 新确认的部署事实

环 0 假设源目录位于 spinfield 后端进程所在主机。实际长期环境中：

- 模型源文件不保证存在于 spinfield manager/控制面主机；
- 模型源文件保证存在于 dingospeed 主机；
- 两台主机之间不假设存在共享盘、DingoFS 挂载或相同路径映射；
- dingospeed 应保持单一职责，只提供文件分块、blob、staging、publish 和下载能力；
- 目录浏览、任务编排和页面进度仍由 spinfield 领域能力负责。

因此，环 0 代码链路本身继续有效，但 `spinfield --api-only` 的部署位置必须从“控制面主机”迁移到“dingospeed 主机”。本文只修正部署拓扑，不推翻环 0 已验证的上传协议和任务语义。

### 0.2 文档优先级

本轮实施时按以下优先级处理：

```text
本文需求 > 本轮详细设计 > 环 0 需求与详细设计 > 其他参考文档
```

本文未明确覆盖的扫描、哈希、进度、staging、publish、幂等和标准客户端下载规则，继续沿用环 0 基线。

### 0.3 最高优先级冲突门禁

若本文、详细设计与真实代码、目标主机权限、网络或运行结果冲突：

1. 立即停止冲突部分施工；
2. 先做成本最低的只读检查、最小复现或实际请求；
3. 能确认本文方案可行时继续并保存证据；
4. 若验证失败、结论不明确，或替代方案会改变接口、范围、信任边界或验收条件，不得自行决定；必须报告文档要求、真实事实、验证证据、影响与待决问题，并等待用户决定。

不得通过开放 CORS、暴露长期 token、共享整块主机文件系统或把模型字节转发回控制面来绕过冲突。

---

## 1. 迁移目标

目标链路调整为：

```text
spinfield 控制台
→ 同源网关把三个模型上传 API 路由到 dingospeed 主机
→ dingospeed 主机上的 spinfield --api-only 浏览/扫描/哈希本机源目录
→ api-only 通过 127.0.0.1 流式调用 dingospeed
→ dingospeed staging + publish 返回 commit
→ huggingface_hub 从 dingospeed 下载
→ 下载内容与 dingospeed 主机源目录逐文件、逐字节一致
```

迁移成功后必须满足：

1. spinfield manager/控制面主机不需要访问模型源目录；
2. 模型文件字节不经过 spinfield manager、控制面网关或浏览器；
3. SHA-256 的首次计算发生在 dingospeed 主机上的 api-only 进程；
4. api-only 到 dingospeed 上传端使用主机本地回环地址；
5. dingospeed 继续只接收文件流和 publish 清单，不承担目录 UI、用户任务或控制面路由；
6. 前端仍通过相对同源 URL 使用现有 Bearer/401 行为，不引入浏览器 CORS 和 dingospeed token。

---

## 2. 目标组件与职责

### 2.1 术语

为避免把两个 spinfield 运行形态混淆，本文使用：

| 名称 | 含义 |
|---|---|
| spinfield 控制面 | 正常 manager 模式，负责现有 Kubernetes/部署管理能力 |
| local-ingest agent | 部署在 dingospeed 主机上的同一 spinfield 二进制，以 `--api-only` 运行 |
| dingospeed 执行器 | 现有 dingospeed 进程，负责上传校验、分块、blob、staging、publish 和 HF 下载 |

`local-ingest agent` 是部署角色，不要求本轮拆出新仓库或新二进制。

### 2.2 职责边界

| 组件 | 负责 | 不负责 |
|---|---|---|
| 控制台 | 选择 agent 可见目录、创建任务、轮询、显示结果 | 读取模型字节、计算 SHA、持有 dingospeed token |
| 同源 L7 网关 | 将模型上传 API 路由到唯一 agent，转发 Authorization | 读取/缓存/改写模型字节、维护任务状态 |
| local-ingest agent | 浏览受控源目录、扫描、SHA、任务状态、流式调用本机 dingospeed | Kubernetes、MySQL、跨节点调度、文件分块格式 |
| dingospeed | 校验上传流、分块、blob、staging、publish、下载 | 目录浏览、spinfield 用户任务、控制台鉴权 |
| spinfield 控制面 | 继续提供其他 admin API | 模型文件 I/O、模型上传 worker、代理模型字节 |

---

## 3. 本轮范围

### 3.1 必须完成

- 将 `spinfield --api-only` 作为受监督进程部署到唯一 dingospeed 主机；
- api-only 启动仍早于任何 kubeconfig、manager、Kubernetes client 或 MySQL 初始化；
- `DINGOSPEED_UPLOAD_BASE` 固定指向该主机的回环上传地址；
- 模型浏览、创建、查询三个接口由同源网关定向到该 agent；
- spinfield 控制面不配置 dingospeed 变量，不注册模型上传路由；
- agent 与控制面使用一致的 `SPINFIELD_ADMIN_TOKEN`，网关原样转发 Authorization；
- 新增并强制执行一个或多个本地源根白名单；
- 浏览和扫描均不得越出源根，必须拒绝符号链接、junction/reparse point 等逃逸路径；
- 产出 Windows 原生进程部署说明、配置模板、启动/停止/健康检查和回滚步骤；
- 保持环 0 的三个 REST API 请求/响应兼容；
- 完成真实双主机语义验收：源目录只在 dingospeed 主机存在，控制面不读取源文件。

### 3.2 本轮明确不做

- 不修改 dingospeed Go 代码、上传协议、分块格式、publish 或下载协议；
- 不新增 dingospeed “传本地绝对路径”接口；
- 不让浏览器直连 8091，不向浏览器下发 `DINGOSPEED_UPLOAD_TOKEN`；
- 不让网关或 spinfield 控制面转发模型文件 body；
- 不实现多 dingospeed 节点选择、节点注册、负载均衡或任务迁移；
- 不实现 CORS 方案；
- 不实现任务持久化、重启恢复、暂停、取消、重试或断点续传；
- 不改仓库覆盖、删除、`baseCommit`、`overwrite=false` 和幂等语义；
- 不将 api-only 重新依赖 Kubernetes、MySQL 或 controller-runtime manager；
- 不拆分独立 agent 仓库或独立协议。

### 3.3 单节点约束

本轮一个环境只能配置一个 local-ingest agent。任务 ID 和 MemStore 都属于该进程，禁止把模型上传 API 轮询请求负载均衡到其他实例。

若未来需要多 dingospeed 节点，必须另行设计节点 ID、目录归属、路由亲和、健康状态和任务持久化；不得在本轮用随机负载均衡提前模拟。

---

## 4. 网络与同源要求

### 4.1 端口与可达性

| 服务 | 监听建议 | 可达范围 |
|---|---|---|
| dingospeed 下载 | `:8090` | 按现有 HF 下载网络策略 |
| dingospeed 上传 | `127.0.0.1:8091` | 只允许同主机 agent |
| local-ingest agent | `:8082` 或指定私网地址 | 只允许同源网关和运维健康检查 |
| 控制台入口 | 现有 HTTPS 域名 | 用户浏览器 |

生产入口必须由现有网关终止 TLS。不得把 8082 作为匿名公网接口。

### 4.2 网关路由

只有以下模型上传路由定向到 agent：

```text
GET  /admin/v1/local-fs
POST /admin/v1/model-uploads
GET  /admin/v1/model-uploads/{taskId}
```

`/admin/v1/login` 和其他 admin API 继续由 spinfield 控制面提供。网关不得改写请求/响应 JSON，不得移除 Authorization。

开发和单机验收可以直接访问 agent 内嵌控制台；这只是回退/诊断入口，不改变生产同源路由结论。

---

## 5. 配置要求

agent 至少配置：

```text
SPINFIELD_ADMIN_TOKEN=<非空，和控制面一致>
SPINFIELD_ADMIN_ADDR=:8082
SPINFIELD_LOCAL_FS_ROOTS=D:\model-source
DINGOSPEED_UPLOAD_BASE=http://127.0.0.1:8091
DINGOSPEED_DOWNLOAD_BASE=http://127.0.0.1:8090
DINGOSPEED_UPLOAD_TOKEN=<非空，仅保存在 dingospeed 主机>
DINGOSPEED_PUBLISH_MAX_FILES=1000
```

要求：

- `SPINFIELD_LOCAL_FS_ROOTS` 在迁移模式下必填，支持多个明确根目录；
- 根目录必须启动时规范化并验证存在、为目录且可读取；
- `SPINFIELD_LOCAL_FS_START_DIR` 如保留，只能位于允许根内；
- token 不得出现在日志、进程命令行、前端构建变量或网关配置回显中；
- 控制面主机不得配置 `DINGOSPEED_*`，防止错误注册本地文件路由；
- 部署配置必须明确主机名、服务账户、工作目录和日志目录。

---

## 6. 接口与行为兼容

以下 API 路径与环 0 保持不变：

```text
GET  /admin/v1/local-fs?path=
POST /admin/v1/model-uploads
GET  /admin/v1/model-uploads/{taskId}
```

创建请求仍只允许：

```json
{
  "name": "demo-model",
  "revision": "v1",
  "sourceDir": "D:\\model-source\\demo-model"
}
```

不得新增由前端提交的 `files[]`、size、SHA-256、token、agent 地址或 dingospeed 地址。

环 0 的以下语义全部保留：

- 后端同步扫描和预检；
- Windows 路径转仓库 `/` 路径；
- 后端逐文件流式 SHA-256；
- 哈希后重新打开文件；
- 每文件 `defer=true`；
- `models/dingo-local`、`overwrite=false`；
- 全文件成功后一次 publish；
- `changed=false` 仍成功；
- code/error 原样保留；
- 双遍真实进度和 blobReused 校准；
- 后台任务不绑定创建请求 context；
- root context 取消时停止；
- 不自动重试。

新增路径边界错误必须使用现有 JSON 错误外壳，HTTP 422，并明确指出“路径不在允许源根内”或“路径类型不允许”，但不得泄露根目录以外内容。

---

## 7. 运行、安全和失败语义

### 7.1 本地文件安全最低线

- 浏览空路径时从允许根或配置起点开始；
- 请求路径必须在规范化后属于某个允许根；
- Windows 路径比较必须使用符合平台语义的大小写规则；
- 不能只做字符串前缀判断，例如 `D:\models2` 不属于 `D:\models`；
- 浏览和递归扫描都拒绝 symlink、junction、mount point、reparse point；
- agent 使用只拥有模型源目录读取权限的低权限服务账户；
- 不在 API 快照中返回文件绝对路径。

### 7.2 进程与任务

- agent 必须由 Windows Service 或等价 supervisor 管理；
- 健康检查使用 `/healthz`；
- 优雅停止时取消 root context，停止后台 worker；
- 本轮继续使用 MemStore：agent 重启后旧 task ID 返回 404，用户重新创建任务；
- dingospeed 的内容寻址与幂等能力可复用已完成 blob，但不能把它描述为任务恢复。

### 7.3 故障边界

| 故障 | 期望行为 |
|---|---|
| agent 无法访问源目录 | 创建任务同步 422，不调用 dingospeed |
| dingospeed 8091 不可达 | 当前任务 failed，保留连接错误，不 publish |
| token 不一致 | 任务 failed，保留 dingospeed code/error |
| agent 重启 | 运行任务中止，旧任务 404，人工重新创建 |
| 网关路由错误 | 模型上传 API 不可用，但不得回落到控制面本地扫描 |
| 控制面 Kubernetes 故障 | 已创建的 agent 后台任务继续，不依赖 manager |

---

## 8. 测试与验收

### 8.1 必测范围

- 配置：源根必填、多个根、非法根、start dir 越界、非回环上传地址警告/拒绝策略；
- 路径：根本身、合法子目录、同名前缀逃逸、`..`、不同盘符、大小写、symlink/junction/reparse point；
- API：三个接口兼容、Bearer、422 边界错误、API-only 其他接口 JSON 404；
- 启动：无 kubeconfig、无 MySQL、无 Kubernetes，服务仍健康；
- 路由：三个模型 API 命中 agent，其他 API 命中控制面；
- 网络：8091 只接受本机连接，浏览器拿不到上传 token；
- 回归：环 0 modelupload、adminapi 定向测试、race、前端测试和 build 全绿。

### 8.2 真实迁移验收

测试必须构造“控制面主机无法读取、dingospeed 主机可以读取”的源目录，完成：

1. dingospeed 主机启动 8090/8091；
2. 同一主机启动 `spinfield --api-only`，无有效 kubeconfig；
3. 控制面不设置任何 `DINGOSPEED_*`；
4. 从现有 spinfield 控制台经同源网关浏览 dingospeed 主机目录；
5. 创建任务并观察 hashing/uploading/publishing/succeeded/commit；
6. 证明 agent 到上传端的目标为 `127.0.0.1:8091`；
7. 验证源 SHA 对应 blob 存在；
8. 使用标准 `huggingface_hub.snapshot_download` 下载；
9. 在独立校验端重新比较路径集合、size、SHA-256 和逐字节内容；
10. 检查网关/控制面请求记录，确认没有模型文件 body 穿过控制面。

### 8.3 验收出口

以下全部通过才结束迁移环 1：

| # | 验收项 |
|---:|---|
| 1 | api-only 确实运行在 dingospeed 主机，且无 Kubernetes 启动 |
| 2 | 页面经同源路由浏览的是 dingospeed 主机允许源根 |
| 3 | 控制面主机没有源目录访问能力，任务仍可成功 |
| 4 | SHA、进度、publish 和 commit 正确，agent→dingospeed 使用回环地址 |
| 5 | 模型字节未经过浏览器、网关和 spinfield 控制面 |
| 6 | blob 验证与标准 HF 下载成功 |
| 7 | 下载目录与源目录逐文件、逐字节一致 |
| 8 | 越界路径、symlink/junction 和无效 token 被明确拒绝 |

---

## 9. 回滚

回滚只改变流量和进程，不删除 dingospeed 数据：

1. 停止网关到 agent 的三个路由；
2. 停止 dingospeed 主机上的 api-only 服务；
3. 恢复迁移前的模型上传入口状态；
4. 保留已发布仓库、commit 和 blob；任何数据删除必须另行审批。

若旧控制面主机不存在源文件，不得把路由回切后宣称上传能力恢复；应将功能明确置为不可用，直到 agent 恢复。

