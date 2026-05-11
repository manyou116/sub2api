# Sub2API 零停机升级指南（Docker Swarm）

> 用 Docker Swarm 在**单台服务器**上实现真正的零停机滚动升级，长流（SSE / 图片生成 / 长 chat completions）在升级期间不断流。
>
> 实测结果：100 秒内 781 个短请求 0 失败 + 3 个并发 60s 长流全部完整跑完，升级窗口期断服时间 = 0。

---

## 1. 为什么不用 `docker compose pull && up -d`

普通 `docker compose up -d` 升级镜像时，会：

1. **先停旧容器**（SIGKILL，可能强杀正在跑的请求）
2. 再启新容器
3. 中间端口被释放 → 客户端 502 / 连接拒绝

对短请求这窗口几秒，对长流（SSE / 图片生成可能几分钟）则**直接被强行掐断**。

## 2. 为什么不用 Nginx 蓝绿脚本

- 自己写脚本 = sed nginx + reload + sleep + docker stop + 失败回滚……每一步都可能出错
- 长流要"等几分钟才能放掉旧容器"，蓝绿脚本里的固定 `sleep 30s` 不够
- `proxy_next_upstream` 只对**新建连接**生效，已建立的 SSE 老连接被 docker stop 时还是会断

## 3. Swarm 怎么解决（核心机制）

```
            升级触发
              │
              ↓
   ┌─────────────────────┐
   │ start-first         │ ← 先起新 task（不接流量），等通过 healthcheck
   └────────┬────────────┘
            ↓
   ┌─────────────────────┐
   │ VIP 自动加入新 task   │ ← 新流量打到新 task
   │ VIP 立即剔除旧 task   │ ← 旧 task 不接新请求，但**继续处理在跑的连接**
   └────────┬────────────┘
            ↓
   ┌─────────────────────┐
   │ 旧 task 收 SIGTERM   │ ← 应用 graceful shutdown，等所有连接结束
   │ stop_grace_period    │ ← 给最长 N 秒（这里设 600s）
   └────────┬────────────┘
            ↓
   ┌─────────────────────┐
   │ 所有连接处理完 → exit │ ← 旧 task 干净销毁
   └─────────────────────┘
```

**前提：应用收到 SIGTERM 必须能 graceful shutdown，否则一切白搭**。
sub2api 默认的 `Server.Shutdown(ctx)` timeout 写死 5s（`backend/cmd/server/main.go:170`），
长流场景必须改成可配置的（参见下文）。

---

## 4. 三步迁移

### 4.1 应用层：Graceful Shutdown（已就绪）

`backend/cmd/server/main.go` 的退出逻辑已经是无超时等待：

```go
// SIGTERM 后，等所有 in-flight 请求自然结束（含长流 SSE / 图像生成）
if err := app.Server.Shutdown(context.Background()); err != nil { ... }
```

> **设计思路**：应用层不设硬超时——`Server.Shutdown` 立即停止监听新连接，
> 然后等所有正在处理的 handler 自然返回。如果某个请求卡死、客户端不断开，
> 由容器编排层 `stop_grace_period` 后的 SIGKILL 兜底，避免应用层暴力截断
> 正常的长流。

### 4.2 改 `deploy/docker-compose.yml`：加 `deploy:` 段 + `stop_grace_period`

```yaml
services:
  sub2api:
    image: weishaw/sub2api:latest
    # ...原有配置全部保留...
    
    # ★ 新增：graceful shutdown 上限（超过则 SIGKILL）。
    # 调大可让长流（图像生成、长上下文 SSE）有更多时间自然结束。
    stop_grace_period: 305s
    
    # ★ 新增：Swarm 滚动升级策略
    deploy:
      replicas: 2                      # 至少 2 个，才有滚动空间
      update_config:
        parallelism: 1                 # 一次只动 1 个
        delay: 10s                     # 两个之间隔 10s
        order: start-first             # ★ 关键：先起新的再停旧的
        failure_action: rollback       # 失败自动回滚
        monitor: 60s                   # 起来后观察 60s 是否稳定
        max_failure_ratio: 0.0
      rollback_config:
        parallelism: 1
        order: stop-first              # 回滚相反方向
      restart_policy:
        condition: on-failure
        max_attempts: 3
      resources:
        limits:
          memory: 2G                   # 防止内存爆炸
    
    healthcheck:                       # 已有，确认 interval/start_period 合理
      test: ["CMD","wget","-q","-T","5","-O","/dev/null","http://localhost:8080/health"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 30s
```

> **注意**：`postgres` / `redis` 也会被 stack 启动。如果你想保留它们用普通 docker compose 跑（避免影响）
> 可以拆成两份 compose：业务 stack 一份，基础设施一份。本指南示范全 stack。

### 4.3 一次性初始化 Swarm

```bash
# 单机模式：本机既是 Manager 也是 Worker
docker swarm init

# 部署 stack（替代 docker compose up -d）
cd /path/to/sub2api/deploy
docker stack deploy -c docker-compose.yml sub2api

# 查看状态
docker stack services sub2api
docker stack ps sub2api
```

---

## 5. 日常使用

### 5.1 升级镜像（替代 `docker compose pull && up -d`）

```bash
# 拉新镜像
docker pull weishaw/sub2api:latest

# 触发滚动升级
docker service update \
  --image weishaw/sub2api:latest \
  --force \
  sub2api_sub2api

# 实时观察滚动进度
watch -n 1 docker service ps sub2api_sub2api
```

升级完成后 `docker service ps` 会看到旧 v1 task 的 `DESIRED STATE = Shutdown`，
新 v2 task `Running`，整个过程**业务零感知**。

### 5.2 改环境变量（`.env` 改了之后）

Swarm 不会自动检测 `.env` 改动，需要手动重新 deploy：

```bash
docker stack deploy -c docker-compose.yml sub2api
```

deploy 命令是幂等的，只重启变化的 service。

### 5.3 改 compose 配置

```bash
docker stack deploy -c docker-compose.yml sub2api
# Swarm 自动检测到 deploy 段或环境变量变化，按 update_config 滚动升级
```

### 5.4 紧急回滚

```bash
docker service rollback sub2api_sub2api
```

会按 `rollback_config` 回到上一个版本。

### 5.5 查日志

```bash
# 整个 service 所有 task 的日志合并
docker service logs -f sub2api_sub2api

# 单个 task 的日志
docker service ps sub2api_sub2api  # 看 task ID
docker logs -f <task_id>
```

### 5.6 停掉整个 stack

```bash
docker stack rm sub2api
```

---

## 6. 与宝塔面板共存

**事实**：宝塔面板的「Docker 管理」基于 `docker run` / `docker compose` 视图，
**看不到 Swarm 的 service 名**，只能看到底层 task 容器（名字是 `sub2api_sub2api.1.xxxxxx`）。

| 宝塔操作 | Swarm 替代命令 |
|---|---|
| 容器列表 | `docker stack ps sub2api` |
| 重启容器 | `docker service update --force sub2api_sub2api` |
| 查看日志 | `docker service logs -f sub2api_sub2api` |
| 一键停止 | `docker stack rm sub2api` |
| 修改配置 | 改 docker-compose.yml + `docker stack deploy -c ...` |

**Nginx 反向代理保持不变**：宝塔已配的 `proxy_pass http://127.0.0.1:8080` 继续用，
Swarm 的 routing mesh 在 8080 端口监听并自动负载到所有 task。

---

## 7. 实测验证（本地复现）

可以用以下脚本在自己机器上验证零停机效果（不依赖 sub2api 业务）：

```bash
mkdir -p /tmp/swarm-test && cd /tmp/swarm-test

# 1. 写一个最小 Go server（含长流接口 + graceful shutdown）
# 2. build 两个版本镜像 swarm-demo:v1, swarm-demo:v2
# 3. docker swarm init
# 4. docker stack deploy -c stack.yml demo
# 5. 客户端：每 100ms 打 /quick + 3 个 /long 60s 长流
# 6. 升级中途触发：docker service update --image swarm-demo:v2 demo_app
```

实测结果（Docker 29.4.0 / 单机 Swarm）：

```
[long-1] lines=123 done_markers=1   ✅ 完整跑完 60s 长流
[long-2] lines=123 done_markers=1   ✅
[long-3] lines=123 done_markers=1   ✅
[short] total=781 ok=781 fail=0 success=100.00%
[short] ✅ ZERO downtime
```

---

## 8. 故障排查

| 现象 | 原因 | 解决 |
|---|---|---|
| `docker service ps` 显示 `Rejected` / `image not found` | 私有镜像未登录 | `docker login` 或 deploy 加 `--with-registry-auth` |
| 升级后旧 task 一直 `Shutdown: starting` | 旧 task 还在等长流结束 | 正常，等 `stop_grace_period` 到期 |
| 升级后两个 task 都是新版但短请求偶尔超时 | healthcheck `start_period` 太短 | 调大 `start_period`（如 60s） |
| 滚动升级触发自动回滚 | 新镜像 healthcheck 失败 | `docker service logs` 排查；调整 `monitor` 时长 |
| `docker stack deploy` 报 `network already exists` | 之前 docker compose 创建过同名网络 | `docker network rm <name>` 后重 deploy |
| 想看新旧 task 同时存在的瞬间 | 升级太快错过 | 调大 `delay`（如 30s）或 `monitor`（如 120s） |

---

## 9. 与现有 `docker compose` 命令对照表

| 旧 | 新 |
|---|---|
| `docker compose up -d` | `docker stack deploy -c docker-compose.yml sub2api` |
| `docker compose pull && docker compose up -d` | `docker pull <image> && docker service update --image <image> --force sub2api_sub2api` |
| `docker compose down` | `docker stack rm sub2api` |
| `docker compose logs -f sub2api` | `docker service logs -f sub2api_sub2api` |
| `docker compose ps` | `docker stack ps sub2api` |
| `docker compose restart sub2api` | `docker service update --force sub2api_sub2api` |

---

## 10. 关键参数速查

| 参数 | 作用 | 推荐值（长流场景） |
|---|---|---|
| `replicas` | 服务副本数 | 2（最少，单机够用）|
| `update_config.parallelism` | 一次替换几个 | 1 |
| `update_config.order` | 先起新还是先停旧 | **`start-first`**（必须）|
| `update_config.delay` | 两批之间间隔 | 10s |
| `update_config.monitor` | 新 task 起来后观察期 | 60s |
| `update_config.failure_action` | 失败动作 | `rollback` |
| `stop_grace_period` | 给旧 task 多久 graceful shutdown（超过则 SIGKILL） | **305s**（按你最长流时间）|
| `healthcheck.start_period` | 启动后多久才开始算健康检查 | 30s |

> **应用层不再使用** `SHUTDOWN_TIMEOUT_SECONDS`：`Server.Shutdown(context.Background())`
> 等所有 in-flight 自然结束，硬上限完全交给 `stop_grace_period` 管理。

---

## 附：完整 docker-compose.yml 改动 diff

```diff
 services:
   sub2api:
     image: weishaw/sub2api:latest
     container_name: sub2api
     restart: unless-stopped
+    stop_grace_period: 305s
     ulimits:
       nofile:
         soft: 100000
         hard: 100000
     environment:
       - AUTO_SETUP=true
       # ...其它环境变量
+    deploy:
+      replicas: 2
+      update_config:
+        parallelism: 1
+        delay: 10s
+        order: start-first
+        failure_action: rollback
+        monitor: 60s
+      rollback_config:
+        parallelism: 1
+        order: stop-first
+      restart_policy:
+        condition: on-failure
+        max_attempts: 3
+      resources:
+        limits:
+          memory: 2G
```
