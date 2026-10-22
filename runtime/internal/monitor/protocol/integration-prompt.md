# 为当前 Agent 实现状态接入

请为我实际使用的 Agent 工具编写一个最小、可卸载的状态转发接入。Runtime 不会替你安装或加载插件。

Runtime 可执行文件：{{EXE}}
数据目录：{{DATA_DIR}}
协议：Agent Status Runtime v2。写入发现文件为上述目录的 private/ingest.json，只在发送时读取；不要把令牌写入代码、日志、提示词、URL 或展示给用户。

先检查当前工具及版本，查阅该版本官方扩展/Hook/生命周期文档。使用确定性的原生回调；不要仅要求模型记得在回答后执行命令。先列出回调到下方标准事件的映射和边界含义。找不到可靠原生边界时，使用 state.reported，reported_basis=inferred 或 self_report，不得伪造原生完成。声明 native_event 仍只是外部声明，不代表 Runtime 已独立验证。

接入方式任选：
1. 将一条符合下方规范的 UTF-8 JSON 写到 stdin，执行上述程序的 report --data-dir <上述目录> --best-effort。完成记录必须有稳定 operation_id。每条事件产生唯一 event_id，重试保持同一 ID 和内容。
2. 读取 private/ingest.json 的 url/token，以 Authorization: Bearer <token> 向 url + /v2/ingest POST application/json。只允许 HTTP loopback IP；关闭代理和重定向，总发送超时不超过一秒，最多一次重试，401 时重读连接文件。只读 connection.json 的令牌不能写入。

Runtime 离线时允许丢失，不启动服务、不阻塞工具、不要偷偷增加后台服务或磁盘队列。上报失败仅输出脱敏错误到 stderr。Hook 的 stdout 必须符合该工具原生契约，不输出 Runtime ACK，不返回审批决定、阻断指令或上下文注入。

producer_id 标识你的接入实例，不是认证身份。agent 使用工具名称。session_id 使用真实会话 ID，operation_id 使用本轮原生执行 ID；若原生无 ID，接入侧为每轮分配并保存稳定 ID，不能每条回调重新生成。session_namespace 默认 producer_id；只有能用 GET /v2/health 中的命名空间及真实会话 ID 确证关联时，才与文件来源共用命名空间。不得按工作目录相近、工具名称或时间相近猜测关联。source_sequence/stream_id 只有在确有连续来源序号时一起填写；没有序号就省略。

只发送白名单元信息；禁止提示词、正文、工具参数、工具输出、整个 Hook JSON 或任意嵌套原始对象。工具请求不等于执行开始，审批请求不等于需要人工操作，消息结束不等于整轮成功。生命周期 scope 必须明确为 message/agent_turn/agent_run/session，结束结果单独表达；正常停止与成功完成不能混为一谈。

结束名称不能直接决定范围或成功结果。例如 Pi 的 agent_end 后可能还有自动重试、恢复或后续工作，agent_settled 表示没有剩余自动工作，但还需区分中断和成功。Claude Stop 等 Hook 可能因其他 Hook 再次继续；须按实际版本的契约确认边界。参考 https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/json.md 与 https://code.claude.com/docs/en/hooks 。本次仅生成接入实现及配置差异，不实际安装、启用或修改 Agent 配置。

输出：接入代码、原生事件映射及限制、需要的配置差异、验证命令、仅撤销自身改动的卸载步骤。修改配置前遵守当前用户的授权范围，保留其他设置和已有 Hook。不要修改原工具实现或日志。先用 report --validate-only 校验，再向专用测试 Runtime 发送构造事件；协议通过只能证明链路，不代表目标工具全生命周期已经兼容。

每次 POST 返回 201 表示已持久化；200 表示重复并返回原 id；409 表示同 ID 不同内容。其余失败不应影响原工具。事件类型以规范封闭集合为准，未知字段会被拒绝。GET /v2/schema 返回完整输出/快照/错误规范；读取接口使用 private/connection.json 的独立只读令牌。

完整输入 JSON Schema：
{{INPUT_SCHEMA}}
