# 已发布二开功能保留清单（升级与发版必读）

## 规则

本文件是独立于当前 main 历史的验收清单。每次升级/整理/发版必须逐行确认，
不能用“旧 main 全量回放了”或 tag 版本号递增代替。保留本文件、AGENTS.md 入口、
项目 release skill、hook 脚本及行为测试；CI 必须运行 hook 检查。

1. 拉取远端 main 和全部 fork tags，盘点已发布但不属于目标 main 祖先链的版本。
2. 对每条已发布功能记录 retained / superseded / explicitly reverted，附源码和行为证据。
   历史压缩会改变祖先链，因此使用功能所有权与实际源码验收，不要求旧 SHA 必须为祖先。
3. 核对每一条保留项的入口、调用链、UI/API、迁移和直接测试。测试文件也可能一同遗漏，
   “现有测试全绿”不能代替清单核查。不得将旧 Web 控制面当成生图影子功能。
4. 发布前跑 hooks、API contracts、相关前后端行为与真实 PostgreSQL 迁移回归。
   对照所有已发布迁移的完整文件名和原始内容；不因前缀撞号而改名/改写已发布 SQL。
5. 主线 CI/安全检查通过后，在同一提交打未使用的新 tag，再确认 Release 和镜像 revision。
   非主线发版不得同步主线 VERSION；共享 latest 会切换功能集合，不能作为功能包含证据。

可用的发现命令（结果是核查入口，不是自动删除/自动合并依据）：

```bash
git fetch origin main --tags
git for-each-ref --format='%(refname:short)' 'refs/tags/v99.*'
git merge-base --is-ancestor SOURCE_TAG TARGET_REF
git diff OLD_REF TARGET_REF -- backend/migrations
./deploy/upstream-sync/check-fork-hooks.sh
```

## 功能所有权与验收

| 已发布功能线 / 来源 | 本次状态 | 必须保留的行为 / 所有权 | 直接验收 |
|---|---|---|---|
| Web/WM 生图重建，v99.0.2.16-plus.13 / dd9c30869 | retained：恢复到 v0.2.19 主线 | `WEB_IMAGE_SHADOWS.md` 全契约：影子创建/批量管理/编辑/备份转换；固定模式和别名；Web 与母账号 Codex 资源隔离；普通单槽位 | WebImage/WorkImage/ImageShadow 单测；TestWebImageShadowDatabase；TestConfiguredImageShadowModes；shadow UI critical tests |
| 生图协议可靠性，6d99e7cb9、7b5918b97（同属 plus.13） | retained | accepted/uncertain 后禁止重新生成；同任务安全读恢复；Retry-After；SSE 取消；下载大小/格式/域名认证约束；日志脱敏；父额度探测 fencing | webdriver recovery/execution/download/poll tests；TestWebImageAcceptedFailureNeverRequestsFailover；TestWebImageErrorHealthScopes |
| 独立分支发布 VERSION 保护，244d63570（同属 plus.13） | retained | release 源提交必须属于默认分支才同步 VERSION，fetch-depth=0、RELEASE_SHA、merge-base 检查缺一不可 | release workflow ancestor guard；release helper tests；fork hooks |
| 旧父账号 Web enabled/inflight 模式，plus.1–12/14–17 | superseded by shadows | 只保留已发布 SQL 和显式转换 API；不重建旧开关、专属 inflight 或提交后 transport 重生成 | legacy helper 缺失检查；TestWebImageAdminUsesExactlyOneSlotAndReleases；conversion tests |
| Kiro，e7577f1af / b88777c72 | retained | 平台注册、导入、token、Chat/Responses、模型发现、配额允许列表、缓存身份 | Kiro 单测；TestPlatformMigrationsPreserveExistingKiroQuota；platform/UI tests |
| 大池 bounded scheduler / 快照读取，5c529262c / 48b0f97f7 | retained with upstream snapshot memo | 有界候选、优先级证明、动态 last-used、上游快照缓存失效 | TestSchedulerCacheSnapshot / Window / LastUsed / Memo；scheduler tests |
| 代理 HTTP/WebSocket 预算，5c529262c | retained | 可选预算、流量记账、续约、force-close 和一次结算 | TestBudgeting；proxybudget/httpclient/ws tests（外部 Python ledger 单列跳过状态） |
| OpenAI/Grok 兼容与管理测试提示词，bec60204d / 2d8d3bddd | retained | 自定义 Responses prompt、Agent Identity 重试、model plan failover、已维护网关适配 | TestAccountHandlerTestOpenAIResponsesPrompt；AgentIdentityRetryPreservesPrompt；compat/failover tests |
| 独立周期 reset-credit 与额度对账，plus.7/8、cc090213f | retained | 5h/7d 阈值、批量编辑、opt-in 后台对账；不复活已撤销的 OAuth delivery | AutoResetCredit / OpenAIQuotaAutoReset / OpenAIReconcile tests；BulkEdit tests |
| overload / 慢首字冷却，plus.9–11/14、f6ac85bea | retained | 共享 Redis generation/探测租约、文本暂停、影子母账号认证/传输保护优先、慢首字事件/恢复 | TestCapacityCooldown；TestSlowFirstOutput；settings tests |
| 账号重置与隐藏状态恢复，plus.17 / 404cd8b90 | retained | 清 DB/Redis 冷却、对延迟结果 fencing、重置后刷新 | TestAdminService_ClearAccountError；TestOpenAIResetQuota；server account-reset integration |
| 分组额度/消耗监控，plus.15/16、f2ddd6e7c | retained | 按需查询、额度样本/资源去重、大数据查询优化 | TestCapacityMonitor；GroupCapacityMonitor repository integration；feature UI tests |
| ops 长窗口预聚合，v99.0.2.19-plus.2 / 85fc470b3 | retained | 无过滤长窗口预聚合、raw 边界及过滤查询、singleflight | ops_repo_trends_preagg_test.go；ops snapshot tests |
| Codex OAuth 上游请求压缩，v99.0.2.19-plus.4；全局 UI 从 plus.5 提供 | retained | `OPENAI_OAUTH_REQUEST_COMPRESSION.md`：官方 HTTP 流式 JSON ≥64 KiB 默认 zstd；全局总开关优先，账号 extra 可关闭；明确编码拒绝才原文重试一次；插件/SSE 保持原有路径 | TestOpenAIRequestCompression* / TestOpenAIOAuthRequestCompression*；管理 UI 与完整网关回归 |
| API Key 生图统计、优雅退出、部署/安全工具链 | retained | 生图观测计数、graceful drain、Swarm 配置、Go/前端安全版本和 release skill | APIContracts；concurrency/cleanup tests；shell/release checks；security CI |
| versioned OAuth credential delivery，d1c834f9d | explicitly reverted，f6658be72 | 明确撤销，禁止仅因历史存在而恢复 | 保留 revert 决策 |
| 代理调度摘要展示，d4721673d | explicitly reverted，5845edeea | 明确撤销，禁止自动回放 | 保留 revert 决策 |

## 2026-10 遗漏原因及恢复边界

plus.13 从 `codex/webimg-shadow` 独立发布，但未合回 main。plus.14–17 从 plus.12
主线继续；10 月 28 日升级只盘点 plus.17，旧 main 与整理后 tree 一致也无法发现
未合入的已发布分支。旧 hooks 只检查旧 Web 路径，影子源码、测试、文档一起漏掉，
因此测试与 hooks 仍通过。此次恢复以 85fc470b3（plus.2）为底，移入 plus.13 的
生图行为和发版保护，保留后续二开与 v0.2.19 上游变化。

## 迁移与上线

`246_web_image_shadows.sql`、`247_work_image_shadows.sql` 必须保留 plus.13
原字节/文件名。上游的 `246_enable_gemini_image_generation_groups.sql` 与
`247_scoped_admin_api_keys.sql` 是不同文件；runner 以完整 filename 为标识。
新安装和已装过 plus.13 的数据库都要验证。发布构建不等于部署/迁移完成。
旧 enabled 父账号需要按 WEB_IMAGE_SHADOWS.md 显式预览/转换，不能自动创建/改写
生产账号；已有影子不需要重复创建。上线前排空旧 Web 请求，备份数据库，再确认
影子数量、配置、母账号状态及服务版本。回退到不支持影子的版本必须先评估存量
账号与迁移，不能只凭镜像可拉取就判定回退安全。
