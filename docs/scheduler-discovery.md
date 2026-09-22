# Scheduler 注册端点与无令牌配置

2026-09-22：为先调通控制面链路，`/api/scheduler-registration` 的 GET/PUT
不再检查 DINGO_NODE_MANAGEMENT_TOKEN。调用方无须 Authorization；保留字段校验、
已绑定身份不可变及活动缓存任务保护。传输设置原本就不要求该令牌。

注册请求新增 `managementUrl` / `downloadUrl`，同时保存到 `.scheduler-registration.json`。
ModelFleet 下发时传自己的可访问地址；独立部署可配置 `upload.advertiseUrl`。
未显式指定时，管理 URL 由注册 host 与 upload.port 生成；下载 URL 由注册 host/port 生成。
需要 HTTPS、容器端口映射或路径前缀时填写完整 URL，不依靠默认推导。

Register gRPC 新增字段 5/6 上报上述端点，已有字段编号和业务协议保持不变。
Scheduler `/api/v1/nodes/health?endpoints=true` 用于 ModelFleet 自动发现。
本地 Standalone 和库存独立性保持不变；Scheduler 不可达不影响本地文件操作。
