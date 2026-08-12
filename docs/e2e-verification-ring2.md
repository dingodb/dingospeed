# 端到端验证记录 —— 环 2 收尾

| 项目 | 内容 |
|---|---|
| 日期 | 2026-08-10 |
| 对应需求 | `model-upload-requirements.md` §9.1、§9.2、§9.3、§9.4、§9.5、§9.6、§9.14 |
| 被测产物 | `go build ./cmd` 产出的 `dingospeed` 二进制 |
| 客户端 | `huggingface_hub` 1.27.0（标准公开客户端，未做任何改造） |

---

## 1. 验证环境

两个独立实例，除 `server.online` 与端口、`repos` 目录外配置完全相同：

| 实例 | `server.online` | 下载端口 | 上传端口 |
|---|---|---|---|
| offline | `false` | 18091 | 18092 |
| online | `true` | 18093 | 18094 |

`upload.token` 配置为固定值，`upload.namespace` 为默认 `dingo-local`，`diskClean.enabled` 关闭。
在线实例的上游为 `huggingface.co`，验证期间外网可达（`/api/models/...` 返回 200）。

**测试仓库内容**（同一份 fixture 同时传入两个实例）：

- 模型 `dingo-local/e2e-model`：6 个文件，含点文件 `.gitattributes`、两级子目录 `subdir/deep/nested.txt`、20 MiB 二进制 `pytorch_model.bin`（跨 3 个缓存块）。
- 数据集 `dingo-local/e2e-dataset`：3 个文件，含子目录 `data/train-00000-of-00001.parquet`（3 MiB）。

上传方式为每个文件一条 `curl`，符合 §4.2 对接口复杂度的检验标准。

---

## 2. §9.1 核心验收：端到端

同一套用例在 offline 与 online 两种配置下分别执行，**结果完全一致**：

| 检查项 | model / offline | dataset / offline | model / online | dataset / online |
|---|---|---|---|---|
| 版本标签解析出快照标识 | PASS | PASS | PASS | PASS |
| 整仓拉取文件集合一致 | PASS (6) | PASS (3) | PASS (6) | PASS (3) |
| 全部文件逐字节一致 | PASS | PASS | PASS | PASS |
| 多级目录结构保持 | PASS | PASS | PASS | PASS |
| 单文件拉取逐字节一致 | PASS | PASS | PASS | PASS |
| 重复拉取不重新下载（§9.6） | PASS | PASS | PASS | PASS |

**针对坑 1 的结论**:在线模式下自研仓库的元数据请求没有走向上游求证，行为与离线模式无差别。两种模式解析出的快照标识**逐字符相同**：

- `dingo-local/e2e-model` → `19b457edd11e7ead2dd18e893c0f50d10fbe24e3d5d92d701d989a05427113cf`
- `dingo-local/e2e-dataset` → `10cd64f92cba01b09a1625de6c1c98a38a34412eb7154bc02152a4367a7d5197`

两个实例互相独立、各自上传，快照标识仍然相同，这同时验证了 FR-5 要求的"由清单确定性计算"。

---

## 3. §9.2 清单完整性（针对坑 3）

仓库 `dingo-local/e2e-incr`，逐个上传 5 个文件（含两级子目录），**每传完一个就做一次整仓拉取**：

| 已上传 | 整仓拉取得到 | 结果 |
|---|---|---|
| 1 | `a.txt` | PASS |
| 2 | `a.txt`, `sub/b.txt` | PASS |
| 3 | + `sub/deep/c.txt` | PASS |
| 4 | + `d.txt` | PASS |
| 5 | + `sub/e.txt` | PASS |

每一次内容变化都产生了不同的快照标识（5 次上传得到 5 个互不相同的 commit）。

---

## 4. §9.3 内容更新可见性（针对坑 12）

| 用例 | 期望 | 结果 |
|---|---|---|
| 覆盖上传后用同样命令拉取 | 拿到新内容，不是缓存里的旧内容 | PASS |
| 覆盖后快照标识变化 | `a1098c0bdf86` → `ac0ef9f476fa` | PASS |
| 覆盖后不混入旧文件 | 文件集合不变 | PASS |
| 追加新文件后整仓拉取 | 新旧文件全部拿到 | PASS |
| 追加后快照标识再次变化 | PASS |
| 内容未变时重复上传 | 走幂等快路径（`status=already_exists`、`blobReused=true`），快照标识保持不变 | PASS |

---

## 5. §9.4 落盘形态一致性（针对坑 2）

在同一个 online 实例上，对比"上传进来的文件"与"从上游下载缓存下来的文件"（`hf-internal-testing/tiny-random-gpt2`）的磁盘产物：

| 字段 | 上传产物 | 上游下载产物 | |
|---|---|---|---|
| magic | `OLAH` | `OLAH` | 相同 |
| version | 8 | 8 | 相同 |
| blockSize | 8388608 | 8388608 | 相同 |
| blockMaskSize | 1048576 | 1048576 | 相同 |
| headerSize | 131108 | 131108 | 相同 |
| 块位图 | 全部就绪 | 全部就绪 | 相同 |

上传产物的有效负载 SHA-256 与其 blob 文件名一致。
`resolve/<commit>/<path>` 条目在两者上都是指向 blob 的硬链接（链接数 2、大小相同），形态无差别。

**已知的一处差异（有意接受，见需求文档 §11.1）**:元数据层面两者可区分——本地仓库有 `dingo-local-manifest.json` 且不落逐文件的 `paths-info` 缓存。内容文件形态无差别。

---

## 6. §9.5 分段下载

对上传好的 20 MiB 文件用 `curl` 发起按字节区间请求：

| 区间 | 位置 | 状态码 | `Content-Length` | `Content-Range` | 内容比对 |
|---|---|---|---|---|---|
| `0-1023` | 开头 | 206 | 1024 | `bytes 0-1023/20971520` | 一致 |
| `10485760-10486783` | 中间（跨块边界） | 206 | 1024 | `bytes 10485760-10486783/20971520` | 一致 |
| `20970497-20971519` | 末尾 | 206 | 1023 | `bytes 20970497-20971519/20971520` | 一致 |

---

## 7. §9.14 回归：公开模型不受影响

在 online 实例上用标准客户端经代理拉取公开模型 `hf-internal-testing/tiny-random-gpt2`：

- 元数据解析正常，拿到上游真实快照标识 `71034c5d8bde858ff824298bdedc65515b97d2b9`；
- 10 个文件全部拉取成功；
- 缓存产物形态见第 5 节。

---

## 8. §4.1 / §9.10 网络暴露边界

`netstat` 实测监听地址：

```
TCP    0.0.0.0:18091      LISTENING     <- 下载服务（offline 实例）
TCP    127.0.0.1:18092    LISTENING     <- 上传接口（offline 实例）
TCP    0.0.0.0:18093      LISTENING     <- 下载服务（online 实例）
TCP    127.0.0.1:18094    LISTENING     <- 上传接口（online 实例）
```

下载服务绑定全部网络接口，上传接口**只绑定回环地址**，符合 §4.1 的硬性约束。
配置中把 `upload.host` 写成非回环地址时，服务会告警并强制改回 `127.0.0.1`。

### 8.1 从另一台机器实际发起连接（§9.10 首行 / 环 1 验收第 8 条）

**已完成。** 用一部与本机连在同一 WiFi 下的手机作为第二台机器。

为了排除防火墙成为混淆变量，先对 `dingospeed.exe` 按**程序**而非按端口添加一条临时入站放行规则（`dingospeed-e2e-temp`）——这样下载端口与上传端口在防火墙眼中待遇完全相同，两者表现的差异只可能来自监听地址。

| 手机浏览器访问 | 期望 | 实际 |
|---|---|---|
| `http://<本机LAN地址>:18093/info`（下载端口，对照组） | 能访问 | **返回服务信息 JSON** |
| `http://<本机LAN地址>:18094/info`（上传端口） | 连接不上 | **打不开** |

对照组通过说明网络路径与防火墙都不构成阻碍，因此上传端口的不可达**只能**由绑定地址解释。结论：上传接口的安全边界确实由监听地址保证，不依赖任何约定或防火墙配置。

> 验证结束后需删除临时防火墙规则：`Remove-NetFirewallRule -DisplayName "dingospeed-e2e-temp"`。

---

## 9. 尚未取得的证据

| 项目 | 原因 | 归属 |
|---|---|---|
| §9.6 客户端完整性校验失败场景 | 只验证了"重复拉取不重新下载"这一半，未单独构造校验失败的反例 | 环 4 |
| §9.14 中"ModelScope 通道行为完全未被改动" | 环 4 已补边界验证：上传不写入 `modelscope/` 缓存根；证据见 `docs/local-upload-delivery.md` | 环 4 已完成 |
| §9.7 大文件与断点续传全部用例 | 功能本身归属环 3 | 环 3 |
| §9.12 崩溃一致性、§9.13 规模 | 归属环 4 | 环 4 |

---

## 10. 复现方式

验证脚本与配置保存在会话工作目录 `e2e/` 下：

| 文件 | 用途 |
|---|---|
| `config/config-offline.yaml`、`config/config-online.yaml` | 两个实例的配置 |
| `upload.sh` | 逐文件调用上传接口（每文件一条 curl） |
| `verify.py` | §9.1 / §9.6 整仓与单文件拉取、逐字节比对 |
| `verify_incremental.py` | §9.2 清单完整性、§9.3 更新可见性 |

复现步骤：起两个实例 → `upload.sh` 传 fixture → 对两个 endpoint 各跑一遍 `verify.py`（models 与 datasets）→ 跑 `verify_incremental.py`。
