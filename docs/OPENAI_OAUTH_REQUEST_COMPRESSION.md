# Codex OAuth 上游请求体压缩

从 `v99.0.2.19-plus.4` 起，核心 HTTP 转发在发送到官方 Codex OAuth 上游前，
默认使用 zstd 默认级别（`SpeedDefault`，单编码 worker）压缩大 JSON。
这是 sub2api → ChatGPT 的上传链路，与 new-api → sub2api 的入站解压独立。

## 生效条件

- 账号严格为 OpenAI / OAuth；仅 HTTPS POST 到
  `https://chatgpt.com/backend-api/codex/responses`。
- 最终发送的 JSON 有 `stream: true`，大小不超过 **64 MiB**（无最小门槛），
  请求可重放且未带 `Content-Encoding`。
- 编码后必须比原文小；读取/压缩准备失败时保持原文发送。
- 插件已经接管的请求、API Key、自定义上游、compact、WebSocket 均不经过此压缩。
  条件针对最终上游请求；客户端非流式请求若在核心中转换为流式上游，也可满足条件。

只设置请求头 `Content-Encoding: zstd`，重建对应的长度和可重放请求体。
不改 `Accept-Encoding`、HTTP/2、代理选择、SSE 响应、事件 flush 或计费逻辑。
压缩需要先处理完整请求体，会增加编码 CPU/内存成本；网络节省取决于真实内容，
不能把单条样本压缩率直接当作整体代理流量节省率。

## 全局开关

后台「系统设置 → 网关服务 → Codex OAuth 请求体压缩」提供独立保存的全局开关，
默认开启。关闭后所有账号均不使用此压缩；开启后仍遵循下方账号单独关闭配置，
以及既有大小、路由和协议条件。此开关不改写任何账号配置。

管理接口为 `GET /api/v1/admin/settings/openai-request-compression` 与同路径 PUT。
请求/返回数据为 `{"enabled": true}` 或 `{"enabled": false}`，PUT 必须显式传布尔值。
设置保存在现有 settings 表的 `openai_request_compression_enabled`，不需要迁移。
当前实例保存成功后立即生效，其他实例最长 30 秒刷新。不影响已经发出的请求。
读取失败时保持原文发送，管理界面显示失败并允许重试；保存失败不更改当前缓存。

全局 UI 开关从 `v99.0.2.19-plus.5` 提供；已发布的 `plus.4` tag 和镜像保持不变。

## 按账号关闭 / 恢复

通过现有账号编辑 API、导入或维护工具，在账号 `extra` 中合并：

```json
{"openai_request_compression": false}
```

保留 `extra` 的其他字段。设为布尔值 `true` 或删除此字段恢复默认开启；
字段存在但类型错误时不启用。账号配置不能覆盖全局关闭。未新增依赖或数据库迁移。

## 编码不兼容时

仅在上游返回 HTTP 415，或 HTTP 400 的前 8 KiB 明确同时包含 `zstd`、
`Content-Encoding` 和 unsupported/decompress 错误时，以原文重试一次。
原文响应仍报错就交回现有处理链，不循环回退；普通 400、429、5xx、网络错误
不因本功能重发，避免对可能已被接受的生成重复提交。其他错误响应体完整保留。

回退不持久化修改账号；持续不兼容时按账号关闭即可。回滚本功能只需关闭对应账号
或撤销 helper 与调用入口，不涉及数据转换。

## 验证

`TestOpenAIOAuthRequestCompression*` 验证 zstd wire bytes 解码后与原文完全一致、
长度/GetBody、路由与大小条件、账号开关、错误回退次数和 SSE 响应不变。
发版前另跑完整单测、集成测试、lint、前端关键回归、fork hooks 与镜像校验。
发布镜像不代表线上已部署或线上压缩率已测量。

## 使用记录中的请求体统计

新记录可展示「上游请求体」：累计原始大小 → 累计编码大小及节省率。展开可查看尝试次数、zstd 次数、明文回退次数、最后已观测尝试的大小与原因。管理端和用户端共用展示。

仅覆盖官方 Codex OAuth HTTP 路径，插件、WebSocket、其他协议以及历史记录没有数据。多个账号/Agent Identity 重试与编码拒绝后的明文回退都会累计；对比基线也按每次尝试的原始 JSON 累计，节省率为 `1 - encoded_bytes / raw_bytes`，不是只比较最后一次成功尝试。

这些字节是交给 HTTP 传输层的请求体大小，不代表所有字节均确认送达；不含响应、HTTP/TLS、代理封装和传输层内部重试，不能替代 IDC 账单。没有生成使用记录的失败请求不会因此新增使用记录。数据不包含请求正文、密钥或响应正文。

迁移 `249_usage_log_upstream_request_body.sql` 只新增可空 JSONB 列，不回填或添加索引。旧记录省略 API 字段；回退旧镜像可保留该列。

同一请求若混用已覆盖与未覆盖的上游路径，统计只累计已覆盖尝试；“最后已观测尝试”不代表其他路径没有后续请求。
