# OpenCode 请求停滞保护

使用环境变量 `OPENCODE_CONCURRENCY` 设置每个请求的竞速并发数，默认 `10`，必须为正整数。例如 Docker Compose：

```yaml
environment:
  OPENCODE_CONCURRENCY: "20"
```

也接受小写 `opencode_concurrency`；两者都有非空值时，大写优先。修改后重启程序或重新创建容器生效。每次兼容工具重试也使用同样的并发数；选出胜者后取消其他尝试。

程序为以下等待分别设置边界，正常持续输出的回答没有总时长限制：

| 阶段 | 默认值 | 配置 |
| --- | --- | --- |
| 读取 HTTP 请求头 | 10 秒 | 内建 |
| 读取 Chat / Responses JSON 请求体 | 30 秒 | 内建，不影响 Gemini 文件上传 |
| 每次上游尝试等待首个有效 token | 独立随机 10–30 秒 | 内建；到期只重播该席位 |
| 等待竞速选出可用上游 | 120 秒 | `OPENCODE_WAIT_TIMEOUT` |
| 上游响应体连续没有数据 | 60 秒 | `OPENCODE_STREAM_IDLE_TIMEOUT` |
| OpenCode 响应写入客户端阻塞 | 60 秒 | `OPENCODE_STREAM_IDLE_TIMEOUT` |

`OPENCODE_STREAM_IDLE_TIMEOUT` 必须是正时长，例如 `30s` 或 `2m`。它限制连续没有进展的等待，每次读取或写入都会重新获得时间预算。

每个并发席位独立获得 10–30 秒的首 token 宽限，从开始连接上游时计时，包含 DNS、连接、TLS 和等待响应头。HTTP 200、心跳、`role: assistant`、空内容、工具调用的 ID / index 等元数据都不算成功，也不会重置计时。只有内容、思考、工具名称 / 参数等有效输出才会赢得竞速。

某个席位在宽限内没有有效 token，就取消并关闭该尝试、分配新的来源 IPv6，立即重播相同请求并重新抽取宽限；其他席位继续等待各自的截止时间，不会整批重启。同时在途的尝试数不超过 `OPENCODE_CONCURRENCY`。明确的 HTTP / 协议错误仍按失败处理，避免对 401 / 403 / 429 等立即错误不停重放。整个选胜者过程受 `OPENCODE_WAIT_TIMEOUT` 约束，默认 120 秒后返回 504。

一旦任何席位返回有效首 token，就取消其余尝试并停止首 token 计时和竞速总计时；后续连续失活仍由 `OPENCODE_STREAM_IDLE_TIMEOUT` 保护。JSON Chat 客户端也先按首 token 选胜者，再只收集胜者的完整回答。Chat SSE 收到 `[DONE]` 后立即释放上游，不再等待对方关闭 TCP 连接。

响应流开始之前失败会返回 HTTP 错误。Responses 流开始后遇到读取错误会发送 `response.failed`；直接 Chat 流可能以缺少 `[DONE]` 的不完整响应结束。客户端取消请求时，上游也会被取消。上游 HTTP 429 继续映射为 503。

日志通过一个后台写入器和最多 256 条待写消息输出。stdout 或日志文件拥塞时，请求继续处理；队列满后会丢弃新日志，输出恢复时报告丢弃数量。关闭程序最多等待一秒排空日志，避免被日志设备拖住。

OpenCode 全部尝试失败或流处理出错会记录 ERROR 日志，包含实际 DNS、连接、解析或失活原因；DEBUG 日志额外记录每次尝试的源地址、HTTP 状态和具体错误。
