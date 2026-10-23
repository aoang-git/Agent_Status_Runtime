为当前 Agent 编写一个主动状态上报插件。使用当前安装版本的官方原生扩展/Hook，不依赖模型记得执行命令。
Runtime: {{EXE}}
数据目录: {{DATA_DIR}}
只接收标准状态，不接收原始消息/工具事件。插件负责将原生回调转换为 working、tool_activity、waiting_confirmation、idle、completed、interrupted、failed、unknown。
先核对工具及版本、原生回调含义，列出映射。Pi 应采用真正停止自动继续处理的 agent_settled；agent_end、消息结束、工具失败均不自动表示任务成功。无法判断成功时报告 unknown，不能伪造完成。
与文件来源连续计时需要准确相同的 session_namespace、真实 session_id 和 operation_id；不能按目录、名称相近猜测关联，无法确证就保留独立身份。每轮保持稳定 operation_id；没有原生ID就每轮生成一次。只有工作开始、工具/审批切换、结束或真实活动进展时发送。不要用定时存活上报工作状态来刷新超时。
reported_basis: native_event 表示确定性原生回调；inferred 表示插件推断；self_report 表示其它自报。这些是上报依据声明，核心不独立验证。
发送时重新读取 private/ingest.json 的 url/token，以独立写令牌 POST /v3/ingest。仅允许本机 HTTP loopback IP，关闭代理和重定向；总超时1秒，最多重试一次且保持同一event_id；401重读发现文件。也可将同一JSON写入 "{{EXE}}" report --data-dir "{{DATA_DIR}}" --best-effort 的 stdin。
201=本次运行内存队列已接收，200=短期重复，409=同ID不同内容；不承诺落盘或客户端必达。失败不得影响 Agent 工作。不要离线补发历史状态。
不得写入或展示令牌，不发送正文、提示词、工具参数和结果。核心不安装、加载或执行插件，不修改 Agent 配置。
JSON Schema:
{{INPUT_SCHEMA}}
