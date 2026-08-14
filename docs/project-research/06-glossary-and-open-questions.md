# 06. 术语与待确认项

> 阅读路径：← [架构分析](05-architecture-analysis.md) · [首页](README.md)

## 术语

| 术语 | 本文含义 |
|---|---|
| 裸模型 | api-only 本地文件系统中的普通模型目录/文件 |
| staged blob | `.uploading` 暂存 DingoCache；未通过完整校验，不可 publish |
| final blob | 通过完整 size/SHA 校验并原子改名后的内容寻址 blob |
| chunk/block | 建议第一版一一对应：一个 HTTP chunk 恰好写一个 DingoCache 物理 block |
| complete | 检查所有块、重算整文件 SHA、把 staged 原子提交为 final blob |
| publish | 把一组 final blob 合并进 manifest 并原子切换 revision；沿用现有接口 |
| force | 只允许重写冲突的 staged block，不允许改 final blob |

## 待确认问题

| 问题 | 影响 | 建议验证/决策 |
|---|---|---|
| 第一版是否要求乱序或并发块上传？ | 锁、重试、进度返回复杂度 | 建议否；先顺序逐块、允许任意块重试 |
| `force` 是块级还是整文件级？ | 数据安全与 API 含义 | 建议块级且仅 staged；final 永远 immutable |
| 是否需要服务端 capability API？ | api-only 与 blockSize 配置漂移 | 建议提供或让首次响应返回 authoritative blockSize/maxSize |
| chunk SHA 使用 SHA-256 还是更快摘要？ | CPU 与错误定位 | 首版建议 SHA-256，协议简单且已有依赖；压测后再评估 |
| complete 失败后保留多久？ | 可修复性与磁盘占用 | 沿用现有 staging retention，但监控需区分普通中断与 hash mismatch |
| 是否存在多个 dingospeed 进程共享同一 repos？ | 当前进程锁不足 | 部署核实；若是，实施前必须引入跨进程锁 |
| api-only 任务状态是否需要持久化？ | 进程重启后的自动恢复 | 与 chunk 协议可分期；首版可通过扫描进度/位图恢复，但当前 MemStore 任务会丢失 |

## 推荐下一步

先做一个不接路由的 DAO PoC，只验证 `PutChunk + CompleteBlob` 在临时目录中的正确性，并测 8/16/32 MiB 下的吞吐、RSS、请求数与 header 写入量。PoC 通过后再冻结正式 HTTP 契约；不要先改 api-only 循环。

