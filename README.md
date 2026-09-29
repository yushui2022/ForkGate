# ForkGate

ForkGate 是面向沙箱工作流的分支感知出网网关。多个推测分支可以通过网关读取外部服务，写请求先留在本地，避免未选中的分支产生外部副作用。当前可用功能是 **HTTP/HTTPS 代理、SecretGuard、分支身份和加密写暂存**；提交选中分支的写请求尚未实现。

当前可运行的 Phase 0 功能：

- 显式 HTTP 代理，以及 HTTPS CONNECT 后的 TLS MITM 转发。
- 首次启动生成本地 CA，按目标主机签发并缓存叶子证书。
- SQLite WAL 请求事件存储。
- 需要管理令牌的健康检查、CA 下载和事件计数接口。

Phase 1 的 SecretGuard 已接入代理转发链和 `/v1/secrets` API。只有设置 `FORKGATE_MASTER_KEY`（32 字节密钥的 hex 或 base64 表示）时才启用；未设置时保持 Phase 0 行为。登记值使用 AES-GCM 加密落盘，API 只返回名称和规则摘要。

启用分支模式后，创建树会得到 live 根分支；fork 会封存父分支并返回 speculative 子分支。子分支的 GET/HEAD 请求继续转发，其他方法先加密暂存并返回 `202 Accepted`。控制 API 可以查看暂存元数据、中止分支，或提交选定分支的暂存写。树创建和 fork 响应只返回一次明文分支 token，SQLite 只保存 SHA-256 摘要。

代理和控制 API 当前只支持 loopback 监听。分支模式需要同时设置 `FORKGATE_BRANCHES_ENABLED=1` 和 `FORKGATE_MASTER_KEY`，缺少密钥会拒绝启动。默认关闭分支模式，此时分支 API 返回 503，基础代理可直接使用；未配置 master key 时不检查密钥。

## 本地体验

```powershell
go run ./cmd/forkgate-demo
# Go 不在 PATH 时：
& 'G:\DevCache\go\go1.26.8\bin\go.exe' run ./cmd/forkgate-demo
```

命令会启动本地 HTTP 上游、ForkGate 代理和控制 API，运行一次三分支请求流程：读取到达上游，写入进入加密暂存，中止一个分支后暂存记录被丢弃，旧父 token 被拒绝。演示结束后服务退出，数据保留在输出所示的 G 盘临时目录。这能直接观察代理的行为；需要连续多次调用和 commit 的完整工作负载可运行下面的工具调用 agent。

仓库还提供了一个不依赖模型的本地验证 harness。它模拟“外部沙箱后端已经完成 fork，上层编排器把分支关系注册到 ForkGate”的流程，然后在随机本地上游执行多次读取和写入，abort 一个分支，提交另一个分支。这个 harness 不创建容器、快照或内存 fork：

```powershell
$env:FORKGATE_ADMIN_TOKEN = 'agent-admin'
& 'G:\DevCache\go\go1.26.8\bin\go.exe' run ./cmd/forkgate-agent
```

它输出 `child1_staged=2:discarded`、`commit=committed/2`、`child2_staged_after_commit=2:succeeded` 和 `upstream_writes=2` 时，说明分支登记后的多次出网调用已经按分支隔离，并且只有胜出分支的写请求真正发出。源码见 [cmd/forkgate-agent/main.go](cmd/forkgate-agent/main.go)。

## Windows 启动

使用 `go.mod` 指定的 Go 版本或更新版本，当前 CI 基线为 Go 1.26.8。未设置 `FORKGATE_ADMIN_TOKEN` 时程序应拒绝启动；不要把令牌写进仓库或命令历史。

```powershell
$env:FORKGATE_ADMIN_TOKEN = Read-Host '输入本次启动的随机管理令牌'
$env:FORKGATE_MASTER_KEY = Read-Host '输入 32 字节 SecretGuard 主密钥（hex/base64，可留空禁用）'
$env:FORKGATE_PROXY_ADDR = '127.0.0.1:3128'
$env:FORKGATE_CONTROL_ADDR = '127.0.0.1:7070'
$env:FORKGATE_BRANCHES_ENABLED = '0' # 先使用基础代理；分支使用流程见下文
$env:FORKGATE_DATA_DIR = 'G:\Projects\Flowsandbox\data\forkgate'
go run ./cmd/forkgate
```

如果 Go 不在 PATH，使用项目约定的 portable 工具链路径：

```powershell
& 'G:\DevCache\go\go1.26.8\bin\go.exe' run ./cmd/forkgate
```

默认代理地址是 `127.0.0.1:3128`，控制 API 地址是 `127.0.0.1:7070`。两个监听地址必须保持 loopback；程序会拒绝非 loopback 地址。数据目录保存事件数据库和 CA 私钥，只应给此开发实例使用。

在另一个 PowerShell 终端设置同一个管理令牌后检查控制面。包括 `/healthz` 在内的控制接口都需要 Bearer 鉴权：

```powershell
$env:FORKGATE_ADMIN_TOKEN = Read-Host '输入启动服务时使用的同一管理令牌'
$headers = @{ Authorization = "Bearer $env:FORKGATE_ADMIN_TOKEN" }
Invoke-RestMethod -Uri 'http://127.0.0.1:7070/healthz' -Headers $headers
Invoke-WebRequest -Uri 'http://127.0.0.1:7070/v1/ca.pem' -Headers $headers -OutFile '.\data\forkgate\client-ca.pem'
Invoke-WebRequest -Uri 'http://127.0.0.1:7070/metrics' -Headers $headers
```

设置 `FORKGATE_MASTER_KEY` 后，可以登记一个只允许发往 GitHub API Authorization header 的密钥。响应和后续事件只包含名称、规则及命中摘要：

```powershell
$secret = @{ name = 'GITHUB_TOKEN'; value = 'ghp_example_value_12345'; action = 'block'; allow = @(@{ host = 'api.github.com'; locations = @('header:Authorization') }) } | ConvertTo-Json -Depth 4
Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:7070/v1/secrets' -Headers $headers -ContentType 'application/json' -Body $secret
Invoke-RestMethod -Uri 'http://127.0.0.1:7070/v1/secrets' -Headers $headers
```

HTTPS 客户端只应为本次请求或测试进程指定该 CA，**不要把它安装到系统全局信任库**。支持本地 CA 文件的 curl 可以使用：

```powershell
curl.exe --http1.1 --proxy http://127.0.0.1:3128 --cacert .\data\forkgate\client-ca.pem https://example.com/
```

Windows Schannel curl 可能遇到本地 CA 的吊销状态不可查询问题；出现此问题时可先用 HTTP 服务接入，或使用支持 CA 文件的客户端。

## 使用分支暂存写入

停止服务，在启动终端中把 `FORKGATE_BRANCHES_ENABLED` 改为 `1`，设置固定的 `FORKGATE_MASTER_KEY` 后重新启动。重启需使用同一密钥以读取已保存的数据。所有代理请求现在都需要分支 token，管理 API 继续使用独立的管理令牌。

在客户端终端创建树并 fork 出两个子分支：

```powershell
$headers = @{ Authorization = "Bearer $env:FORKGATE_ADMIN_TOKEN" }
$tree = Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:7070/v1/trees' -Headers $headers
$rootID = $tree.root_branch.branch_id
$fork = Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:7070/v1/branches/$rootID/fork" -Headers $headers -ContentType 'application/json' -Body '{"count":2}'
$childID = $fork.children[0].branch_id
$childToken = $fork.children[0].token
```

用子分支 token 读取服务，GET 会正常转发。以下 HTTPS 请求使用前面下载的 CA：

```powershell
curl.exe --http1.1 --proxy http://127.0.0.1:3128 --proxy-header "Proxy-Authorization: Bearer $childToken" --cacert .\data\forkgate\client-ca.pem https://example.com/
```

把应用的一次 POST 改为经代理发送即可尝试暂存。下面用本地 `8080` 端口作为示例目标；这个 speculative POST 不会连接目标服务，而是返回 `202` 和暂存标识。`--noproxy ""` 用来确保本地地址也经过代理：

```powershell
curl.exe -i --noproxy "" --proxy http://127.0.0.1:3128 --proxy-header "Proxy-Authorization: Bearer $childToken" -H 'Content-Type: application/json' --data '{"title":"branch idea"}' http://127.0.0.1:8080/issues
Invoke-RestMethod -Uri "http://127.0.0.1:7070/v1/branches/$childID/staged" -Headers $headers
```

`staged` 查询只返回序号、方法、目标主机、状态和时间等元数据。请求 URL、headers 和 body 加密保存，不由查询接口返回。当前通用 `202` 响应只表示网关已暂存，并不是目标服务真正创建了资源；依赖上游返回对象 ID 的客户端需要等待后续 Profile 支持。

fork 之后继续用 `$tree.token` 请求会收到 `503 branch_sealed` 和 `Retry-After: 1`，应切换到选定子分支的 token。丢弃一个子分支后，它的暂存记录变为 `discarded`，该 token 随后返回 410：

```powershell
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:7070/v1/branches/$childID/abort" -Headers $headers
```

当前只支持单层 fork。live 根分支在 fork 之前的写请求会直接发出；speculative 子分支才会暂存写入。选定子分支的基础 commit 已实现：按序解密并直连发出暂存写，成功后将该分支标记为 committed 并自动丢弃兄弟分支；失败恢复、占位符替换和 exactly-once 仍未实现。

## 配置和接口

| 配置 | 默认值 / 要求 |
| --- | --- |
| `FORKGATE_ADMIN_TOKEN` / `-admin-token` | 必填，无默认值 |
| `FORKGATE_MASTER_KEY` | 32 字节 hex/base64；设置后启用 SecretGuard，分支模式必填，用于加密暂存请求 |
| `FORKGATE_BRANCHES_ENABLED` | 默认关闭；设为 `1` 后启用分支 API、要求代理 token 并暂存 speculative 写请求 |
| `FORKGATE_ALLOW_NON_LOOPBACK` | 默认关闭；仅在受控 Docker 私有网络演示中设为 `1`，允许容器访问网关监听端口 |
| `FORKGATE_PROXY_ADDR` / `-proxy-addr` | `127.0.0.1:3128`；默认必须 loopback，Docker 演示需显式开启 `FORKGATE_ALLOW_NON_LOOPBACK=1` |
| `FORKGATE_CONTROL_ADDR` / `-control-addr` | `127.0.0.1:7070`；默认必须 loopback，Docker 演示需显式开启 `FORKGATE_ALLOW_NON_LOOPBACK=1` |
| `FORKGATE_DATA_DIR` / `-data-dir` | `data/forkgate`，相对于启动工作目录 |

以下控制接口都需要 `Authorization: Bearer <FORKGATE_ADMIN_TOKEN>`：

| 接口 | 作用 |
| --- | --- |
| `GET /healthz` | 返回服务健康状态 |
| `GET /v1/ca.pem` | 下载此实例的 CA 公钥证书 |
| `GET /metrics` | Prometheus 格式的 `forkgate_events_total` |
| `GET /v1/events?since=&limit=` | 查询已脱敏的事件元数据 |
| `POST /v1/secrets` | 登记密钥；请求值不会在响应中返回 |
| `GET /v1/secrets` | 列出名称、allow 规则和动作 |
| `DELETE /v1/secrets/{name}` | 删除登记的密钥 |
| `POST /v1/trees` | 创建一棵树并返回 root branch token |
| `GET /v1/trees/{tree_id}` | 查看树和分支状态，不返回 token |
| `POST /v1/branches/{branch_id}/fork` | 封存父分支并创建 1–32 个 speculative 子分支 |
| `POST /v1/branches/{branch_id}/commit` | 按序提交 speculative 分支的暂存写，并中止同一 fork 下的兄弟分支 |
| `POST /v1/branches/{branch_id}/abort` | 中止 active 分支 |
| `GET /v1/branches/{branch_id}/staged` | 查看该分支的暂存记录元数据 |

完整事件同时保存在 `forkgate.db` 的 `events` 表中；查询接口不会返回请求 body、凭证或令牌。

## 开发

Makefile 将 `GOPATH`、`GOMODCACHE`、`GOCACHE`、`GOTMPDIR`、`TEMP` 和 `TMP` 默认放到 G 盘，临时目录位于 `G:\DevCache\Temp\ForkGate\go`。这些是当前构建进程的环境变量，不会修改全局 Windows 配置。PowerShell 下可直接调用 `go`，或将 `GO` 变量设为 portable `go.exe` 路径：

```powershell
$env:GO = 'G:\DevCache\go\go1.26.8\bin\go.exe'
make build
```

先构建并接入实际请求，根据使用中出现的问题迭代。需要排查回归时再运行相应包的测试；完整检查命令保留供 CI 和发布前使用：

```text
make check   # test + vet + lint
```

Lint 固定使用 [golangci-lint v2.14.0](https://github.com/golangci/golangci-lint/releases/tag/v2.14.0)。按[官方二进制安装说明](https://golangci-lint.run/docs/welcome/install/local/)下载对应版本后放到 PATH，或将 `GOLANGCI_LINT` 指向该二进制。CI 使用相同版本和 `.golangci.yml`。Go 版本固定为 `1.26.8`。

## 当前边界与后续

CONNECT 隧道内目前只接受 HTTP/1.1，并在处理一条请求后关闭连接。TLS 握手超时为 10 秒，内部请求头读取空闲超时为 60 秒，内部请求头上限为 64 KiB；响应按单请求生命周期返回。尚不支持隧道内 HTTP/2、多请求连接复用、WebSocket、gRPC 或通用 TCP 转发。MITM 需要客户端信任此实例的 CA，不兼容证书固定客户端。事件日志只保存已脱敏的请求元数据，不保存请求 body、凭证或令牌。

SecretGuard 当前支持原文、base64/base64url、hex、URL/JSON/换行、gzip/deflate/Brotli 视图和 host/location allow。加密、分片、逐字符发送和跨请求重组不在保证范围内。提交失败恢复、占位符替换、面向具体 API 的合成响应、透明模式及沙箱 adapter 尚未实现。

开发设计见 [docs/design.md](docs/design.md)，执行状态见 [docs/progress.md](docs/progress.md)，完整范围见 [ForkGate-规划执行蓝图.md](ForkGate-规划执行蓝图.md)。
第三方 agent 接入候选和当前运行门槛见 [docs/agent-validation.md](docs/agent-validation.md)。
