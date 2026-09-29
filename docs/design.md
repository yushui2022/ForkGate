# ForkGate Phase 0–2 设计

Phase 0 建立 HTTP/HTTPS 数据面、管理面与事件存储。Phase 1 在设置 `FORKGATE_MASTER_KEY` 后启用 SecretGuard；未设置时保持基础转发行为。Phase 2 已加入单层树/分支状态机、token 鉴权和加密写暂存。

## 模块与请求流程

| 模块 | 当前职责 |
| --- | --- |
| `cmd/forkgate` | 配置、loopback 地址校验、服务启动与关闭 |
| `internal/proxy` | 显式 HTTP 代理、CONNECT TLS MITM、上游转发和请求事件 |
| `internal/mitm` | 本地 CA 的生成/加载、按主机签发叶子证书与缓存 |
| `internal/store` | SQLite WAL、events 表和事件计数 |
| `internal/api` | 管理令牌鉴权、健康状态、CA 下载、指标和 SecretGuard 登记 |
| `internal/secretguard` | 编码模式、规范化视图、allow/block/flag 和大小限制 |
| `internal/secrets` | AES-GCM 密钥登记持久化与重启加载 |

普通 HTTP 请求以绝对 URL 进入代理。代理建立独立的上游请求，再把响应返回客户端。HTTPS 请求先经 CONNECT 建立客户端连接，由本地 CA 签发的叶子证书终止 TLS；网关读取内部 HTTP 请求后，建立独立的上游 TLS 连接。生产转发保留上游证书校验，本地测试只为假上游显式添加测试证书信任。

当前 CONNECT 生命周期是“一条 TLS 连接、一条 HTTP/1.1 请求”，响应后主动关闭。TLS 握手超时为 10 秒，内部请求头读取空闲超时为 60 秒，内部请求头上限为 64 KiB，响应也按单请求生命周期返回；这为下一阶段检查 HTTPS 请求体提供边界，但不支持 HTTP/2 多路复用、长连接复用和其他隧道协议。

## 身份和监听边界

代理与管理面均强制只监听 loopback 地址。`FORKGATE_ADMIN_TOKEN` 必须显式提供，空值或缺失时启动失败；`/healthz`、`/v1/ca.pem`、`/metrics` 均需 `Authorization: Bearer` 鉴权。Phase 0 的数据面没有分支 token，因此不能暴露给公网、局域网其他设备或不受信任用户；管理令牌不等于代理客户端身份。

分支模式关闭时，代理会转发普通 HTTP 方法，包括 POST。分支模式开启后，live 根分支仍直接转发；speculative 分支的 GET/HEAD/OPTIONS 继续转发，其他方法在 SecretGuard 检查通过后加密写入 `staged_writes`，返回通用 `202`，不会访问上游。分支 token 通过 `Proxy-Authorization: Bearer` 提供，旧或终止 token 在数据面被拒绝。面向具体 API 的合成响应和提交语义仍未实现。

## 日志与存储边界

事件库使用 SQLite WAL 与单连接写入。事件只包含已脱敏的结构化元数据，例如方法、目标主机、路径、状态和错误类别；不保存请求 body、凭证或令牌。SecretGuard 事件只写密钥名、编码、位置、偏移和目标 host。事件数量通过 `/metrics` 暴露，`/v1/events` 可分页查询这些元数据。SecretGuard 仍不是完整的秘密识别保障。

CA 保存到配置的数据目录；CA 私钥属于实例敏感数据。只将 CA 公钥通过 `/v1/ca.pem` 交给指定客户端，不添加系统全局信任。测试生成的 CA 与数据库使用临时目录；Makefile 将 Go 缓存和临时文件定位到 G 盘。

## 实施顺序与验证

Phase 0 先验证 HTTP 转发、HTTPS CONNECT/MITM、CA 重载、SQLite 事件以及控制面鉴权。测试上游使用 `httptest`，不以第三方网站的可达性作为单元或集成测试前提。构建、vet、格式检查与 lint 分别检查编译、静态问题与仓库约定。

Phase 1 已加入 SecretGuard 的扫描大小限制、编码匹配、规范化、允许流向规则和密钥加密。Phase 2 目前完成树/分支、token、加密写暂存与 abort；后续加入 Profile、提交恢复和 adapter。实现优先通过本地 demo 和真实客户端接入迭代，不把尚未存在的能力写成接口承诺。

主要待验证风险是客户端 TLS 兼容性、事件内容与敏感数据边界、后续 fork 时的连接失效语义。当前 Windows Schannel curl 的吊销查询问题单独保留；本地 Go 客户端的 HTTPS 集成测试已通过，不能据此宣称所有系统客户端兼容。
