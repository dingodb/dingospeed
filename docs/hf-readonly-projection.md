# HF 缓存只读投射

模型仓库展示使用以下独立 GET 接口，保留原有下载接口、HF owner/repo 数据库身份和缓存布局。

- `/api/repositories/{repoType}/huggingface/local-catalog`：发现本地 HF 仓库及已知 revision，不要求 `repository.json`。
- `/api/repositories/{repoType}/huggingface/local-manifest?repo=Qwen/demo&revision=main`：返回已缓存的上游清单，不在列清单时读取文件正文或检查块完整性。
- `/api/repositories/{repoType}/huggingface/local-file?repo=Qwen/demo&revision=main&path=README.md`：只读取已完整缓存的内容，支持 HTTP Range；缺失返回 404，未完整/未知返回 409。

三者只使用只读文件操作，无数据库、网络、注册、缓存创建或缓存修复行为。独立展示索引由 Spinfield 维护，不写回 HF 原目录。不要用原 `metadata/tree/file` 的回源读取代替这些展示接口。

仓库身份为 `namespace=huggingface`，repo 保留上游 owner/name。扫描排除 `dingo-local` 和 `modelscope` 物理根，不改变公司与个人上传归属。发现基于已有 `api/<type>/<owner>/<repo>` 和 `files/<type>/<owner>/<repo>` 下的缓存结构，未下载且无任何本地缓存事实的上游仓库不在该目录中。

`access` 为 public/restricted/unknown：只有元数据明确 `private=false` 且 `gated=false` 才确认为 public；任一已知版本受限即 restricted，属性缺失即 unknown。控制面将后两者限制为管理员可见，不能因为节点曾下载成功就推断可公开。这里只反映本地最后已知元数据，不声称实时查询了上游权限。分支指针已指向的 commit 不再重复列为 revision；没有分支指针时保留 commit 作为恢复浏览入口。

这些端点与现有 Speed 仓库接口共用内部服务信任边界；面向用户的鉴权由 Spinfield 执行。Speed 本身不是面向不可信用户的权限网关。

`metadataAvailable` 表示可读本地上游元数据；`manifestComplete` 表示其中存在 siblings 清单，不表示实时上游状态或本地文件完整。元数据缺失时仍展示 paths-info/resolve 中已知文件，清单完整性为 false。损坏的元数据明确报错，目录以单仓库 error 表达，不冒充空库。

清单中的 `cacheStatus` 固定为 unknown，`cachedBytes` 不表示已经核验的缓存量；`size=-1` 表示文件大小未知，`oid` 可为空。列出文件名不代表该文件可读。实际读取时才校验文件路径、链接目标、OLAH v8 头、块位图和物理长度；指向仓库外或其他仓库的链接被拒绝。普通文件要求已知大小与实际大小一致。HF 摘要不被强制视为 SHA256，读取检查也不重新计算内容摘要。

数据目录可能在下载过程中变化；读取错误须保留并显示异常，不可将错误当作删除或完整空清单。接口输出是一次读取观察，不提供跨文件事务快照。当前 cached metadata wrapper 上限 32 MiB，超限明确报错。

仅剩孤立 blobs、没有 revision/paths-info/resolve 证据的目录不会被猜测成可展示仓库。实际读取时，以 OLAH 魔数开头但头部无效的文件会被拒绝（包括无法与损坏容器区分的少见普通历史文件），不根据物理长度相等就当作完整内容返回。

验证：`go test ./pkg/hfprojection ./pkg/repository ./internal/router ./internal/handler ./internal/dao`。隔离浏览器验收物料位于父项目 `artifacts/hf-projection-20260917`，测试缓存以只读 mount 提供。
