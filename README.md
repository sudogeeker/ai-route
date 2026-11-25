# ai-route

Gemini、OpenAI 與選用的 OpenCode API 轉發閘道。

## 設定與啟動

```sh
cp ai-route.example.yaml ai-route.yaml
./ai-route -config ai-route.yaml
```

從原始碼也可以執行 `go run ./cmd/ai-route -config ai-route.yaml`。

不指定 `-config` 時，程式會讀取目前目錄的 `ai-route.yaml`；檔案不存在時預設只啟用 Gemini 並監聽 `:8080`。明確指定的檔案必須存在。修改設定後需重啟程式。含有金鑰的設定檔請限制檔案權限，不要提交到 Git。

Docker Compose 用同一份 YAML，不需要設定一組環境變數：

```yaml
services:
  ai-route:
    image: ghcr.io/sudogeeker/ai-route:latest
    ports: ["127.0.0.1:8080:8080"]
    volumes:
      - ./ai-route.yaml:/etc/ai-route/config.yaml:ro
    command: ["-config", "/etc/ai-route/config.yaml"]
```

映像需使用含有新版 YAML 載入器的版本；若尚未發布，可用 `docker build -f Dockerfile.source -t ai-route:local .` 建置，並將 `image` 改為 `ai-route:local`。

## API 路由

| 服務 | 閘道位址 |
| --- | --- |
| Gemini 原生 API | `https://gemini.example.com/v1beta`（保留原生版本、Live WebSocket 與檔案上傳路徑） |
| Gemini 的 OpenAI 相容 API | `https://gemini.example.com/v1beta/openai`，使用 Gemini API Key |
| OpenAI 官方 API | `https://openai.example.com/v1`，使用 OpenAI API Key |
| OpenCode | `/opencode/v1/models`、`/opencode/v1/responses`；舊的模型分流只在未被 OpenAI 接管的 `/v1` 路由上執行 |

### 只更換 OpenAI 網域

```yaml
openai:
  enabled: true
  inbound_host: openai.example.com
```

把客戶端的 `https://api.openai.com/v1` 換成 `https://openai.example.com/v1`，不用加 `/openai`。若客戶端自行加上 `/v1`，則只設定 `https://openai.example.com`。Realtime WebSocket 同樣只換網域，例如 `wss://openai.example.com/v1/realtime?model=...`。請在反向代理終結 HTTPS，保留原始 `Host`，並允許 WebSocket 升級，見 [Caddy 範例](deploy/gapi.caddy)。

OpenAI 的 `/v1/*` 路徑、查詢參數、授權標頭與請求內容會直接轉到上游，不檢查模型名稱或將請求翻譯成其他 API。這涵蓋模型列表、Chat Completions、Responses、語音合成、音訊轉錄與翻譯、Realtime WebSocket 和 Realtime 的 WebRTC HTTP 信令。WebRTC 媒體本身不經過此 HTTP 閘道。模型可用性及權限仍由 OpenAI 帳號與專案決定。

### Gemini 與 OpenAI 共用一個網域

同時啟用 `gemini.enabled` 和 `openai.enabled`，**不要設定 `openai.inbound_host`**。若使用 Cloudflare Tunnel，只需一條公開主機名規則指向閘道的 HTTP 監聽埠。

| 用途 | 同網域路徑 |
| --- | --- |
| OpenAI（含音訊、Responses 與 Realtime） | `/v1`，照常使用 OpenAI 憑證 |
| Gemini 原生、OpenAI 相容介面 | `/v1beta`、`/v1beta/openai` |
| Gemini 穩定版 | `/v1` 搭配 `x-goog-api-key` 或 `?key=`；OAuth Bearer 使用 `/gemini/v1` |

在共用的 `/v1` 路徑，只有明確的 Google API key 才會分流至 Gemini，不檢查模型名稱或請求內容。Google API key 和 `Authorization` 同時存在時回傳 `400`，不會把兩份憑證送到任一上游。`/gemini/v1` 和 `/openai/v1` 可明確指定上游；前綴會在轉發時移除。Gemini 原有的上傳與 Live WebSocket 路徑不變。

若使用不同網域，仍可設定 `openai.inbound_host` 專供 OpenAI，以主機名隔離 `/v1`。只使用 OpenAI 時可設定 `gemini.enabled: false`。

省略 `opencode` 或保留 `enabled: false` 時，程式不初始化 OpenCode 或 WARP，專用路徑回傳 `404`。對外開放 OpenCode 前請設定 `opencode.auth_key`。

## 多 IP 轉發

Gemini 與 OpenAI 各有獨立的 DNS 快取及連線池。`ip_family` 可為 `ipv4`、`ipv6` 或 `dual`；Gemini 預設 `ipv6`，OpenAI 預設 `dual`。新連線會輪流選擇 DNS 回傳的目的 IP；也可用 `destination_ips` 明確指定一組目的 IP、跳過 DNS。無論選到哪個 IP，HTTP Host 和 TLS 驗證仍使用 `target_host`。已建立的連線會重用，串流不會中途換 IP；TCP 連線失敗時會嘗試其他地址，不會因輪換 IP 主動重送已送出的請求。

設定 `ipv6.source_cidrs` 後，IPv6 連線會從這些來源網段選址。省略時會嘗試自動偵測；沒有可用來源網段時使用系統預設來源位址。主機必須已經能綁定並路由這些 IPv6 位址，程式不會修改主機網路設定。IPv4 連線使用系統預設出口。

### 內建 DoH

Gemini 和 OpenAI 可各自指定 DoH 解析器，無需本機 DNS 代理或 sidecar。例如：

```yaml
gemini:
  doh:
    url: https://dns.google/dns-query
openai:
  enabled: true
  doh:
    url: https://cloudflare-dns.com/dns-query
```

只填 `doh.url` 即可。若 URL 是網域，程式會使用系統解析器取得 **DoH 伺服器本身**的位址；Gemini 或 OpenAI 上游網域仍由 DoH 以 HTTPS POST 查詢，按 `ip_family` 選擇 A、AAAA 或兩者。若不希望依賴系統 DNS，可選填 `bootstrap_ips`：程式只嘗試清單中的位址，仍以 URL 的網域驗證 TLS。也可直接使用有相符 TLS 憑證的 IP 型 URL，此時不設 `bootstrap_ips`。

DoH 失敗時不會改用系統 DNS 查詢上游，已有的上游 IP 快取會保留。省略 `doh.url` 才會用系統 DNS 查詢上游。`destination_ips` 或 IP 型的 `target_host` 會跳過 DNS。OpenCode 的解析設定不變。

## 從舊版遷移

原本的程式環境變數與 `-listen`、`-cidr`、`-log-level`、`-log-file` 不再讀取。將 `LISTEN_ADDR` 改為 `listen`、`IPV6_CIDR` 改為 `ipv6.source_cidrs` 清單、`TARGET_HOST` 改為 `gemini.target_host`、`LOG_LEVEL` 和 `LOG_FILE` 改為 `log.level` 和 `log.file`。原有的 `OPENCODE_*` 設定移至 `opencode` 區段，並設 `enabled: true`；`proxy_urls` 和 `models` 改用 YAML 清單。Gemini 和 OpenAI 會直連所選 IP，不使用 `HTTP_PROXY` 或 `HTTPS_PROXY`。完整欄位請參考 [範例設定](ai-route.example.yaml) 與 [OpenCode 疑難排解](deploy/TROUBLESHOOTING.md)。
