# CubeSandbox 本机部署记录

2026-09-29：按用户要求停止本机部署，后续转到服务器。

## 已完成

- 官方源码目录：`G:\Projects\CubeSandbox`，安装版本 v0.7.2。
- 在 WSL 中用官方 dev-env 创建 OpenCloudOS 9.6 VM；CubeSandbox 控制面已安装，API `/health` 曾返回 `status=ok`。
- 普通 nested KVM 路径在创建模板时失败：CubeVMM 启动 vCPU 约 373ms 后报 `FailEntry(7, 2)`，Shim 随后报 10 秒等待超时。不能把这次失败解释为 ForkGate 功能失败。
- 在 G 盘创建独立 qcow2 overlay 进行 PVM 实验，没有覆盖原始 VM 镜像。
- overlay 启动官方 `6.6.69-opencloudos9.cubesandbox.pvm.host-g0de43d6b3bcd` 内核，卸载 guest 内 `kvm_intel` 后成功加载 `kvm_pvm`，将 CubeSandbox runtime 的 `vmlinux` 链接切换到 `vmlinux-pvm`。
- 官方 `sandbox-code:latest` 模板创建成功：`tpl-c1aee1703c944aaa8a9a41e9`；job `6c9093bd-c01d-4c54-9c1a-39457a58be07` 返回 READY、100%、ready_node_count=1。

## 未完成及停止状态

- 尚未从此模板完成独立用户 sandbox 的代码执行、snapshot/clone 和 ForkGate 联调。因此不能声称真实谱系沙箱验证通过。
- 用户要求转服务器后，已向 guest 发出关机指令，随后确认没有 QEMU 进程。保留镜像和部署记录，不再在本机继续试部署。
- WSL 更新失败（`0x80070070`）；当时 C 盘约 0.1GB 可用，更新后版本仍为 WSL 2.1.5.0 / kernel 5.15.146.1。
- 后续检查 C 盘约 3.7GB 可用；不能将此变化计作缓存删除成果。过期 pip 临时缓存删除命令被工具策略拒绝，没有执行删除。
- 原始镜像与 overlay 都在 `G:\Projects\CubeSandbox\dev-env\.workdir`；overlay 依赖原始镜像，不可单独移动或删除 backing image。
- PVM 内核下载包：`G:\DevCache\CubeSandbox\kernel-6.6.69-pvm-host.x86_64.rpm`。
- overlay 的 PVM 模块切换是本次启动的手动操作，尚未配置为可靠的自动启动流程。

## 服务器接续

官方功能体验配置为至少 4 核、8GB 内存；`/data/cubelet` 需要至少 50GB 可用空间以及 XFS reflink。建议本项目使用 4 核 16GB、约 100GB 磁盘留出父沙箱及多个 clone 的空间；这是项目建议，不是官方最低配置。

普通 x86_64 云 VM 可走 PVM，但需要 root、能够安装内核和重启。服务器信息到位后，先核对这些条件，再部署官方发行包；实际验证顺序为创建父沙箱、由 CubeSandbox snapshot/clone 出子沙箱、登记 ForkGate 分支、比较网关日志与真实上游写入记录。

参考：[官方 Quick Start](https://github.com/TencentCloud/CubeSandbox/blob/master/docs/guide/quickstart.md)、[PVM 部署](https://github.com/TencentCloud/CubeSandbox/blob/master/docs/guide/pvm-deploy.md)。
