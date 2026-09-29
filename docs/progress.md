# ForkGate 执行进度

更新日期：2026-09-29。

当前可以运行 HTTP/HTTPS 代理、登记密钥防止外发、创建分支，把 speculative 分支的写请求加密暂存，并将选定分支的暂存写按序提交到上游。项目尚未发布。

## 现在能做什么

- 显式 HTTP 代理和 HTTPS CONNECT 后的 TLS MITM；自动生成本地 CA。
- SQLite WAL 保存脱敏事件，管理 API 提供健康检查、CA 下载、事件查询和计数。
- SecretGuard 在转发前扫描登记的密钥，支持原文、base64/base64url、hex、URL/JSON/换行和 gzip/deflate/Brotli 视图，以及 host/location allow 规则。
- 登记密钥使用 AES-GCM 保存，可在重启后加载；API 不返回密钥值。
- 创建 live 根分支，单层 fork 为 speculative 子分支；父分支随即封存，旧 token 返回 `503 branch_sealed`。
- 分支 token 只在创建响应中返回一次，数据库保存 SHA-256 摘要，响应使用 `Cache-Control: no-store`。
- speculative 分支的读取转发给上游，其他请求加密暂存并返回通用 `202` 响应；管理 API 可查看暂存元数据。
- active 分支可 abort；对应暂存记录标记为 `discarded`，token 随后返回 410。
- 选定的 speculative 分支可通过 `POST /v1/branches/{branch_id}/commit` 按序重放暂存写；全部成功后标记为 `committed`，同一 fork 下的其他 active 分支自动 abort。

分支模式默认关闭。设置 `FORKGATE_BRANCHES_ENABLED=1` 后启用分支 API 和代理鉴权，且必须配置 `FORKGATE_MASTER_KEY`。不启用分支模式时，可直接使用基础代理；没有 master key 时 SecretGuard 关闭。监听地址目前限于 loopback。

## 使用入口

`go run ./cmd/forkgate-demo` 会启动本地上游、代理和控制面，走一次三分支请求流程并输出观察结果。HTTP/HTTPS 读取会真的到达本地服务，写请求应只留在暂存库。演示结束后进程退出，输出会显示保留的数据目录。

长期运行和手动接入流程见 [README.md](../README.md)：外部编排器先调用沙箱后端完成 fork，再调用 ForkGate 的注册接口同步分支关系；之后把子分支 token 加入对应沙箱客户端的 `Proxy-Authorization`。仓库中的 `cmd/forkgate-agent` 是本地 harness，不是沙箱运行时；它覆盖了分支登记后的多次读写、abort、兄弟分支继续调用和胜出分支 commit。下一步要把这个 harness 换成真实 Docker/netns 或托管沙箱适配器，再依据实际 agent 行为迭代。

## 当前边界

- 暂存返回通用 `202`，还不能模拟具体 API 的资源 ID 和响应结构；依赖这些返回值的客户端暂时无法完整继续执行。
- commit 目前是基础成功路径：支持按序直连重放、成功状态和兄弟分支丢弃；部分提交后的恢复、unknown/resolve、占位符替换和崩溃恢复协议尚未实现，不提供 exactly-once 保证。
- 只支持单层 fork；live 根分支的请求直接转发，speculative 子分支才暂存写入。
- CONNECT 只支持 HTTP/1.1 单请求生命周期；尚不支持 HTTP/2、WebSocket、gRPC 和通用 TCP。
- SecretGuard 不覆盖加密、逐字符发送、分片和跨请求重组等绕过方式。
- 已在本机 Docker Desktop Linux engine 跑通 `examples/docker-sandbox` 的真实容器后端兼容 smoke：Docker 用 `docker commit` 复制父容器文件系统，再启动 3 个子容器并向 ForkGate 登记分支；3 个子容器共享同一快照镜像且写层独立，6 个 speculative 写请求先全部暂存，提交 child-2 后上游只收到 child-2 的 2 个按序写入，child-1/3 的写入被丢弃。报告见 `output/docker-sandbox/20260929-172548-b52548ba/result.json`。这证明的是 ForkGate 的分支与外部副作用协议，不等于企业级 Agent 沙箱：当前没有运行中进程/内存 fork，也没有共享内核之外的硬件隔离。当前仍是显式 HTTP 代理；未代理的 direct probe 能绕过，透明代理、netns/veth/nftables 和真实沙箱绕过拦截仍未完成。

## 下一步

用户已决定停止本机沙箱部署、转到服务器。本机 CubeSandbox v0.7.2 控制面和 PVM 模板已成功创建，但尚未完成真实 sandbox snapshot/clone 与 ForkGate 联调；本机实验 VM 已停止。环境和恢复信息见 [CubeSandbox 本机部署记录](cubesandbox-local-status.md)。下一步在资源足够的服务器部署 CubeSandbox，先完成父沙箱到多个 clone 的实际请求验证，再依据真实应用行为补 Profile、占位符替换和必要的错误恢复。

已有自动化测试和 CI 配置保留用于定位回归。此前基础代理、SecretGuard 的测试与构建已通过；这些记录不代表当前已经能完成真实 agent 的整套工作流。
