# OpenCode 请求停滞保护

程序为以下等待分别设置边界，正常持续输出的回答没有总时长限制：

| 阶段 | 默认值 | 配置 |
| --- | --- | --- |
| 读取 HTTP 请求头 | 10 秒 | 内建 |
| 读取 Chat / Responses JSON 请求体 | 30 秒 | 内建，不影响 Gemini 文件上传 |
| 等待竞速选出可用上游 | 120 秒 | `OPENCODE_WAIT_TIMEOUT` |
| 上游响应体连续没有数据 | 60 秒 | `OPENCODE_STREAM_IDLE_TIMEOUT` |
| OpenCode 响应写入客户端阻塞 | 60 秒 | `OPENCODE_STREAM_IDLE_TIMEOUT` |

`OPENCODE_STREAM_IDLE_TIMEOUT` 必须是正时长，例如 `30s` 或 `2m`。它限制连续没有进展的等待，每次读取或写入都会重新获得时间预算。

仅包含 `role: assistant` 的 SSE 开场事件不会赢得竞速。上游必须开始返回内容、思考或工具调用等输出，才会取消其余请求。Chat SSE 收到 `[DONE]` 后立即释放上游，不再等待对方关闭 TCP 连接。

响应流开始之前失败会返回 HTTP 错误。Responses 流开始后遇到读取错误会发送 `response.failed`；直接 Chat 流可能以缺少 `[DONE]` 的不完整响应结束。客户端取消请求时，上游也会被取消。上游 HTTP 429 继续映射为 503。

日志通过一个后台写入器和最多 256 条待写消息输出。stdout 或日志文件拥塞时，请求继续处理；队列满后会丢弃新日志，输出恢复时报告丢弃数量。关闭程序最多等待一秒排空日志，避免被日志设备拖住。

OpenCode 全部尝试失败或流处理出错会记录 ERROR 日志，包含实际 DNS、连接、解析或失活原因；DEBUG 日志额外记录每次尝试的源地址、HTTP 状态和具体错误。
