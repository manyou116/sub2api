# Sub2API zero-downtime rolling update (Docker Swarm)

This fork supports long-running API, SSE, and image requests during rolling
updates by combining application-level graceful shutdown with a Swarm
start-first deployment.

## How it works

1. `sub2api` receives `SIGTERM`.
2. The process calls `routes.MarkShuttingDown()`, so `/health` returns `503`.
3. It optionally waits `SERVER_SHUTDOWN_DRAIN_DELAY` seconds for load balancers
   to stop routing new requests.
4. `http.Server.Shutdown` waits up to `SERVER_SHUTDOWN_TIMEOUT` seconds for
   in-flight requests.
5. Swarm `stop_grace_period` is kept slightly higher than the app timeout as
   the final SIGKILL guard.

The Swarm compose files put Caddy in front of the app. Caddy sends traffic to
`tasks.sub2api:8080`, while `sub2api` uses `endpoint_mode: dnsrr`; existing
upstream TCP connections stay pinned to the old task until the request ends.

## Production stack

```bash
cd deploy
docker swarm init

export POSTGRES_PASSWORD='change-me'
export JWT_SECRET="$(openssl rand -hex 32)"
export TOTP_ENCRYPTION_KEY="$(openssl rand -hex 32)"
export SUB2API_IMAGE='ghcr.io/manyou116/sub2api:v99.0.1.161-plus.2'

docker stack deploy -c docker-compose.swarm.yml sub2api
```

Upgrade:

```bash
docker pull ghcr.io/manyou116/sub2api:<new-tag>
docker service update \
  --image ghcr.io/manyou116/sub2api:<new-tag> \
  --force \
  sub2api_sub2api
```

Rollback:

```bash
docker service rollback sub2api_sub2api
```

Watch:

```bash
docker stack services sub2api
docker service ps sub2api_sub2api
docker service logs -f sub2api_sub2api
```

## Local verification stack

The local file reuses host PostgreSQL / Redis and a locally built image.

```bash
bash deploy/build_image.sh

export JWT_SECRET="$(openssl rand -hex 32)"
export TOTP_ENCRYPTION_KEY="$(openssl rand -hex 32)"

docker swarm init
cd deploy
docker stack deploy -c docker-compose.swarm-local.yml sub2api
```

Trigger a rolling update after rebuilding:

```bash
bash deploy/build_image.sh
docker service update --force --image sub2api:latest sub2api_sub2api
```

## Key settings

| Setting | Purpose | Default |
|---------|---------|---------|
| `SERVER_SHUTDOWN_TIMEOUT` | App waits for in-flight requests | `300` |
| `SERVER_SHUTDOWN_DRAIN_DELAY` | `/health=503` delay before listener closes | `5` in Swarm, `0` elsewhere |
| `stop_grace_period` | Swarm hard kill guard | `320s` |
| `deploy.update_config.order` | Start replacement before stopping old task | `start-first` |
| `deploy.endpoint_mode` | Resolve task IPs directly for Caddy | `dnsrr` |

For image generation or very long SSE requests, raise both
`SERVER_SHUTDOWN_TIMEOUT` and `stop_grace_period` together.

## Zero-downtime smoke test

1. Start a long streaming request through Caddy (`http://127.0.0.1:8080`).
2. Run `docker service update --force --image <new-image> sub2api_sub2api`.
3. Confirm the client receives the terminal event and the old task exits after
   the stream completes.

If the client drops during update, check:

- `docker service ps sub2api_sub2api`
- `docker service logs -f sub2api_sub2api`
- `/health` returns `503 {"status":"draining"}` after SIGTERM
- Caddy resolves `tasks.sub2api` inside the overlay network
