# Codex OAuth 上游请求体压缩

从 `v99.0.2.19-plus.4` 起，核心 HTTP 转发在发送到官方 Codex OAuth 上游前，
默认使用 zstd 默认级别（`SpeedDefault`，单编码 worker）压缩大 JSON。
这是 sub2api → ChatGPT 的上传链路，与 new-api → sub2api 的入站解压独立。

## 生效条件

- 账号严格为 OpenAI / OAuth；仅 HTTPS POST 到
  `https://chatgpt.com/backend-api/codex/responses`。
- 最终发送的 JSON 有 `stream: true`，大小为 **64 KiB–64 MiB**（包含边界），
  请求可重放且未带 `Content-Encoding`。
- 编码后必须比原文小；读取/压缩准备失败时保持原文发送。
- 插件已经接管的请求、API Key、自定义上游、compact、WebSocket 均不经过此压缩。
  条件针对最终上游请求；客户端非流式请求若在核心中转换为流式上游，也可满足条件。

只设置请求头 `Content-Encoding: zstd`，重建对应的长度和可重放请求体。
不改 `Accept-Encoding`、HTTP/2、代理选择、SSE 响应、事件 flush 或计费逻辑。
压缩需要先处理完整请求体，会增加编码 CPU/内存成本；网络节省取决于真实内容，
不能把单条样本压缩率直接当作整体代理流量节省率。

## 按账号关闭 / 恢复

通过现有账号编辑 API、导入或维护工具，在账号 `extra` 中合并：

```json
{"openai_request_compression": false}
```

保留 `extra` 的其他字段。设为布尔值 `true` 或删除此字段恢复默认开启；
字段存在但类型错误时不启用。此次未新增 UI、依赖或数据库迁移。

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
