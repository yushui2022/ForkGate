# Agent 验证候选

ForkGate 的本地链路已经由 `cmd/forkgate-agent` 验证。它是一个固定计划的 harness，不依赖模型：模拟外部沙箱已经完成 fork 后的分支登记，执行多次 GET 和 POST，查询暂存，abort 一个分支，验证兄弟分支继续调用，再 commit 胜出分支并检查真实上游写入次数。它不创建或隔离沙箱。

为了继续验证真实工具运行时，候选项目按接入难度排序如下：

| 项目 | 适合验证 | 当前状态 |
| --- | --- | --- |
| [Open Interpreter](https://github.com/openinterpreter/open-interpreter) | 终端、shell、文件和网络请求 | 已浅克隆到 `G:\Projects\AgentCandidates\open-interpreter`；当前版本以 Codex CLI/Rust 工具链为主，尚未安装运行包 |
| [Browser Use](https://github.com/browser-use/browser-use) | 浏览器导航、表单提交、网页 prompt injection | 已浅克隆到 `G:\Projects\AgentCandidates\browser-use`；源码支持 Playwright `ProxySettings`，尚未安装浏览器和模型依赖 |
| [SWE-agent](https://github.com/SWE-agent/SWE-agent) | Docker 中的 shell、文件编辑、测试和 git 操作 | 已浅克隆到 `G:\Projects\AgentCandidates\SWE-agent`；需要 Docker runtime 和模型 provider |
| [OpenHands](https://github.com/OpenHands/OpenHands) | 完整 coding agent、Docker runtime、命令和浏览器 | 已浅克隆到 `G:\Projects\AgentCandidates\OpenHands`；需要 Docker Desktop 和模型 provider |

当前机器有 Docker CLI，但 Docker Desktop daemon 没有运行；也没有为这些第三方 agent 配置模型 provider。因此暂不为了安装大型依赖启动完整第三方栈。优先级是：

1. 用 Open Interpreter 验证 shell/HTTP 代理环境；
2. 用 Browser Use 验证浏览器显式代理和网页写操作；
3. 启动 Docker Desktop 后，再用 SWE-agent 或 OpenHands 验证容器内 agent 的直接出网绕过。

每个 agent 都应同时验证两条路径：正常请求必须经过 ForkGate；关闭或绕过代理的直连必须在容器网络层被禁止。仅设置 `HTTP_PROXY` 不能证明后者。

官方运行参考：

- OpenHands 的 Docker runtime 和网络隔离：[OpenHands Docker runtime](https://github.com/OpenHands/docs/blob/main/openhands/usage/v0/runtimes/V0_docker.mdx)
- SWE-agent 的 CLI 和 Docker 环境：[SWE-agent CLI tutorial](https://github.com/SWE-agent/SWE-agent/blob/main/docs/usage/cl_tutorial.md)
- Browser Use 的本地 Python agent 和浏览器配置：[Browser Use quickstart](https://github.com/browser-use/browser-use#quickstart)
- Open Interpreter 的安装和沙箱配置：[Open Interpreter quickstart](https://github.com/openinterpreter/openinterpreter/blob/main/docs/quickstart.md)、[sandbox](https://github.com/openinterpreter/openinterpreter/blob/main/docs/sandbox.md)
