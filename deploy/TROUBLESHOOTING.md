# OpenCode 请求重试与停滞保护

使用环境变量 `OPENCODE_CONCURRENCY` 设置每个请求的竞速并发数，默认 `10`，必须为正整数。例如 Docker Compose：

```yaml
environment:
  OPENCODE_CONCURRENCY: "20"
```

也接受小写 `opencode_concurrency`；两者都有非空值时，大写优先。修改后重启程序或重新创建容器生效。每次首 token 重播和错误重试都保持相同的在途并发上限；选出胜者后取消其他尝试。

大请求默认降低并发，避免把 600KB～750KB 的长上下文同时上传十份。请求体达到 `OPENCODE_LARGE_CONTEXT_BYTES`（默认 `262144` 字节）时，竞速席位数会限制为 `OPENCODE_LARGE_CONTEXT_CONCURRENCY`（默认 `2`）。把阈值设为 `0` 可关闭该限制。

来源池还会按可用 `/64` 数量封顶竞速席位：只有单个 `/64` 时只发一路请求，因为 OpenCode 免费层按 `/64` 计限流，同一块内的多个 `/128` 不构成独立重试。使用代理池时按代理数量封顶；没有 IPv6 也没有代理时只使用一个默认出口，不会重复十路相同的请求。

程序为以下等待分别设置边界，正常持续输出的回答没有总时长限制：

| 阶段 | 默认值 | 配置 |
| --- | --- | --- |
| 读取 HTTP 请求头 | 10 秒 | 内建 |
| 读取 Chat / Responses JSON 请求体 | 30 秒 | 内建，不影响 Gemini 文件上传 |
| 每次上游尝试等待首个有效 token | 60 秒 | `OPENCODE_FIRST_TOKEN_TIMEOUT` |
| 等待竞速选出可用上游 | 180 秒 | `OPENCODE_WAIT_TIMEOUT` |
| 上游响应体连续没有数据 | 60 秒 | `OPENCODE_STREAM_IDLE_TIMEOUT` |
| OpenCode 响应写入客户端阻塞 | 60 秒 | `OPENCODE_STREAM_IDLE_TIMEOUT` |

`OPENCODE_FIRST_TOKEN_TIMEOUT`、`OPENCODE_WAIT_TIMEOUT` 和 `OPENCODE_STREAM_IDLE_TIMEOUT` 必须是正时长，例如 `60s` 或 `2m`。流空闲超时限制连续没有进展的等待，每次读取或写入都会重新获得时间预算。

每个并发席位独立获得首 token 宽限。计时从完整请求体写入传输层之后开始，因此 DNS、连接、TLS 和上传慢请求由 `OPENCODE_WAIT_TIMEOUT` 约束，不会误杀大 Prompt 的上传阶段。HTTP 200、心跳、`role: assistant`、空内容、工具调用的 ID / index 等元数据都不算成功，也不会重置计时。只有内容、思考、工具名称 / 参数等有效输出才会赢得竞速。

某个席位在宽限内没有有效 token，就取消并关闭该尝试、分配新的来源 IPv6 或代理，立即重播相同请求并重新开始计时；其他席位继续等待各自的截止时间，不会整批重启。首 token 到期重播不消耗 `OPENCODE_RETRIES`，只受 `OPENCODE_WAIT_TIMEOUT` 约束。整个选胜者过程默认 180 秒后返回 504。

`OPENCODE_RETRIES` 设置错误重试预算，默认 `10`，设为 `0` 可关闭。HTTP 403、408、425、429、5xx，以及 HTTP 2xx 但响应内容无效时，会消耗一次预算并使用新的来源重试；401 和其他 4xx 直接失败。403 和 429 会把对应的来源 IPv6 `/64` 冷却 `OPENCODE_COOLDOWN`（默认 `10m`，设为 `0s` 关闭）；若上游提供 `Retry-After`，冷却时间取两者较长值。同一 `/64` 不会被立即重播。没有其他 `/64` 或代理出口时，403/429 会直接结束该席位，而不是重复发送同一个被封请求。全部尝试仍失败时，429 映射为 503，其他上游状态保留。

## 代理与无 IPv6 启动

`OPENCODE_PROXY_URLS` 接受以逗号或空白分隔的代理池，支持 `http://`、`https://`、`socks5://` 和 `socks5h://`；单数 `OPENCODE_PROXY_URL` 作为别名。SOCKS 代理省略端口时使用 `1080`。每次上游尝试按顺序轮换一个代理，代理池存在时不再绑定来源 IPv6。

```yaml
environment:
  OPENCODE_PROXY_URLS: "socks5h://user:pass@proxy-a:1080,socks5h://proxy-b:1080"
```

程序不会使用进程环境中的 `HTTP_PROXY` / `HTTPS_PROXY`，避免它们绕过显式来源绑定。若启动时没有检测到可用 IPv6 网段，只记录 WARN 并继续运行；OpenCode 可直接使用主机默认出口或代理池，Gemini 请求仍需要主机具备可用的 IPv6 出口。可通过 `IPV6_CIDR` / `-cidr` 显式提供来源范围。

未设置 `OPENCODE_MODELS` 时，除了内置免费模型列表，还接受任意以 `-free` 结尾的模型和 `big-pickle`。显式设置后只允许列表中的模型，避免把未知模型转发到付费上游。

匿名请求按抓取到的 OpenCode `1.19.4` CLI 契约构造：`opencode/1.19.4` User-Agent、每请求 `ses_` / `msg_` 身份、`x-opencode-project: global`、单一 system message（官方 default prompt + 环境 + 调用方指令 + skills）、11 个官方工具定义以及 `max_tokens: 32000`（调用方指定时沿用其值）。这套契约与 2.x 的 header、工具名和 system prompt 不同，不能混用。

一旦任何席位返回有效首 token，就取消其余尝试并停止首 token 计时和竞速总计时；后续连续失活仍由 `OPENCODE_STREAM_IDLE_TIMEOUT` 保护。JSON Chat 客户端也先按首 token 选胜者，再只收集胜者的完整回答。Chat SSE 收到 `[DONE]` 后立即释放上游，不再等待对方关闭 TCP 连接。

响应流开始之前失败会返回 HTTP 错误。Responses 流开始后遇到读取错误会发送 `response.failed`；直接 Chat 流可能以缺少 `[DONE]` 的不完整响应结束。客户端取消请求时，上游也会被取消。上游 HTTP 429 继续映射为 503。

日志通过一个后台写入器和最多 256 条待写消息输出。stdout 或日志文件拥塞时，请求继续处理；队列满后会丢弃新日志，输出恢复时报告丢弃数量。关闭程序最多等待一秒排空日志，避免被日志设备拖住。

OpenCode 全部尝试失败或流处理出错会记录 ERROR 日志，包含实际 DNS、连接、解析或失活原因；DEBUG 日志额外记录每次尝试的源地址、HTTP 状态和具体错误。
