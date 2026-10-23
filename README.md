# Agent Status Runtime

轻量本机实时监测与广播服务，版本 **0.3.0 / schema 3**。核心在本次运行期间观察默认四类 Agent 的文件变化，或接收第三方插件的标准状态报告，通过 HTTP/SSE 广播。

核心不判断最终显示状态，不保存活动数据库，不恢复旧任务。正式设备上位机尚未开发；测试窗口用于分析来源和验证客户端规则。

## Windows 安装

双击 `bin/agent-status-setup.exe`，无需 Go、.NET 或管理员权限。安装到当前用户的 `%LOCALAPPDATA%\Programs\AgentStatusRuntime`；运行目录为 `%APPDATA%\AgentStatusRuntime`。

安装器注册当前用户计划任务：登录启动、每分钟补启动、忽略重复实例、允许电池运行、没有执行时限。首次安装立即启动，后台不弹控制台。策略禁止任务注册时显示具体错误，不自动提权。

维护命令（安装后的核心程序）：

```powershell
& "$env:LOCALAPPDATA/Programs/AgentStatusRuntime/agent-statusd.exe" stop
& "$env:LOCALAPPDATA/Programs/AgentStatusRuntime/agent-statusd.exe" start
& "$env:LOCALAPPDATA/Programs/AgentStatusRuntime/agent-statusd.exe" restart
& "$env:LOCALAPPDATA/Programs/AgentStatusRuntime/agent-statusd.exe" uninstall
```

停止会先禁用补启动；启动重新启用。重复运行安装器原位更新。卸载只移除自身程序和计划任务，保留诊断数据，不修改 Agent 目录。

## 开发运行与测试窗口

```powershell
./scripts/build.ps1 -Test
./bin/agent-statusd.exe serve --data-dir ./.runtime/live
./bin/agent-statusd.exe doctor --data-dir ./.runtime/live
./bin/agent-statusd.exe watch --data-dir ./.runtime/live
./bin/agent-statusd.exe status --data-dir ./.runtime/live
./scripts/build-test-window.ps1 -Test
./bin/agent-status-test.exe
```

`status` 输出近期诊断记录；`watch` 订阅建立后的新增记录。窗口只读服务，不启动、停止或配置核心。默认优先使用用户安装服务的运行目录；没有用户连接文件时，才发现带有新版连接文件的项目 `.runtime/live`。也可显式指定或选择目录；旧连接文件不作兼容读取。

默认监听 `127.0.0.1:9120`，占用时自动选择空闲端口；显式 `--listen` 不自动换端口。Linux/macOS 使用对应产物手动运行，不提供本轮自动安装。

## 内置来源

| Agent | 默认发现 | 观察内容 |
|---|---|---|
| Codex | CODEX_HOME 或 ~/.codex 下 sessions | 输入、消息、工具及原生回合边界 |
| Claude Code | CLAUDE_CONFIG_DIR 或 ~/.claude 下 projects | 输入、消息停止原因和工具 |
| DeepSeek DSH | DSH_HOME 或 ~/.dsh 下 sessions | v3/v4 JSONL/Zstd、工具、审批及回合边界 |
| Pi | PI_CODING_AGENT_SESSION_DIR 或 ~/.pi/agent/sessions | v3 JSONL、消息停止原因、工具及持久化分支变化 |

目录不存在时正常待机，出现后自动接入。自定义目录可使用重复 `--root agent=path`（替代自动根目录）。不修改 Agent 配置，不加载插件；远程/WSL 文件和无持久化会话不自动覆盖。桌面版本兼容性仍需独立验证。

启动已有文件只建立基线，不广播旧活动。半行/半帧不提前推进；截断、替换、继承记录不伪造新完成。原生时间不重排实际接收顺序。

## 第三方接入

第三方只报告标准状态，不发送原始消息或工具事件。例如：

```json
{"schema":3,"producer_id":"pi-status","event_id":"event-1","agent":"pi","session_id":"session-1","operation_id":"run-1","state":"working","reported_basis":"native_event"}
```

状态包括 working、tool_activity、waiting_confirmation、idle、completed、interrupted、failed、unknown。活动和结束状态需要稳定 operation_id；idle 不等于成功。reported_basis 区分 native_event、inferred、self_report，属于来源声明。

生成工具专用接入提示词：

```powershell
./bin/agent-statusd.exe integration-prompt --data-dir ./.runtime/live
```

插件根据所用版本的原生 Hook 转换状态；不依赖模型记得发送命令，不用定时工作状态冒充实际进展。Pi 的 agent_end 后可能自动继续，应核对 agent_settled 等真实稳定边界。本仓库本轮不提供具体 Pi 插件。

完整字段、HTTP/SSE 行为及限制见 [接入协议](doc/v0.3接入协议.md)。

## 内存与显示规则

近期队列最多 4096 条 / 8 MiB / 10 分钟；上报去重最多 10000 条 / 10 分钟；流序号最多 4096 个 / 闲置 1 小时；文件上下文最多 4096 个，失效 1 小时淘汰。超限明确降级或报告缺口。服务退出即丢弃活动数据，无离线补报队列。

只落盘连接发现文件、操作系统锁和最多两份各 1 MiB 的诊断日志。连接文件只允许当前用户和 SYSTEM 访问；读写令牌分离，每次启动轮换。API 仅 loopback，不支持浏览器跨域。正文、工具参数和输出不进入广播或日志。

测试窗口所有来源严格最新优先，只显示一个轮次，依据强弱不锁定来源。活动/工具/审批切换不重置计时；普通 5 分钟、审批 30 分钟无有效活动时保底结束，心跳和未知不续期。文件正常消息结束无工具、审批或已知缺口时保留 3 秒推断。结束通知 10 秒后清除，后续活动另起一轮。ready、gap、重连和 recent 不恢复顶部活动。

## 验证与交付

`scripts/test-install.ps1` 在仓库专属隔离目录和独立任务下验证安装、重装、启停、强杀恢复、卸载，不覆盖已有安装。

`scripts/soak.ps1` 使用独立 Runtime 和构造记录进行固定负载运行，默认 72 小时，输出资源及健康 CSV，不操作真实 Agent 记录。验证进度以实际采样为准；详见 [v0.3 验证记录](doc/v0.3验证记录.md)。

构建产物统一在 bin，每个程序和目标平台只保留最新版。构建核验后关闭占用输出的本项目进程，原位构建并恢复实例；临时发布和内嵌载荷构建后清理。
