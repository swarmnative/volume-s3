English | [简体中文](README.zh.md)

# volume-s3

![CI](https://img.shields.io/github/actions/workflow/status/swarmnative/volume-s3/publish.yml?branch=main)
![Release](https://img.shields.io/github/v/release/swarmnative/volume-s3)
![License](https://img.shields.io/github/license/swarmnative/volume-s3)
![Docker Pulls](https://img.shields.io/docker/pulls/swarmnative/volume-s3)
![Image Size](https://img.shields.io/docker/image-size/swarmnative/volume-s3/latest)
![Go Version](https://img.shields.io/github/go-mod/go-version/swarmnative/volume-s3)
![Last Commit](https://img.shields.io/github/last-commit/swarmnative/volume-s3)
![Issues](https://img.shields.io/github/issues/swarmnative/volume-s3)
![PRs](https://img.shields.io/github/issues-pr/swarmnative/volume-s3)

Cluster S3 volume controller for single Docker and Docker Swarm.

---

## Features
- Host-level rclone FUSE mount (apps bind-mount from host path)
- Optional in-cluster HAProxy for load-balancing & failover
- Declarative "volumes" via labels (S3 bucket/prefix), K8s-like experience
- Self-healing: lazy-unmount & recreate if mount becomes unhealthy
- Optional node-local LB alias: `volume-s3-lb-<hostname>` for nearest access

---

## Quick Start (minimal stack)
Prereqs: Swarm initialized; FUSE enabled; label nodes that should mount.
```bash
docker node update --label-add mount_s3=true <NODE>
```
Create credentials (Swarm secrets):
```bash
docker secret create s3_access_key -
# paste AccessKey then Ctrl-D

docker secret create s3_secret_key -
# paste SecretKey then Ctrl-D
```
Deploy (single backend service, mounter reaches `tasks.minio:9000` via built-in HAProxy):
```yaml
version: "3.8"

networks: { s3_net: { driver: overlay, internal: true } }

secrets:
  s3_access_key: { external: true }
  s3_secret_key: { external: true }

services:
  minio:
    image: minio/minio:latest
    command: server --console-address :9001 /data
    environment:
      - MINIO_ROOT_USER_FILE=/run/secrets/s3_access_key
      - MINIO_ROOT_PASSWORD_FILE=/run/secrets/s3_secret_key
    secrets: [s3_access_key, s3_secret_key]
    volumes: ["/srv/minio/data:/data"]
    networks: [s3_net]
    healthcheck:
      test: ["CMD-SHELL", "curl -fsS http://localhost:9000/minio/health/ready || exit 1"]
      interval: 10s
      timeout: 3s
      retries: 10
    deploy:
      placement: { constraints: [node.labels.minio == true] }

  volume-s3:
    image: ghcr.io/swarmnative/volume-s3:latest
    networks: [s3_net]
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - type: bind
        source: /mnt/s3
        target: /mnt/s3
        bind: { propagation: rshared }
    secrets: [s3_access_key, s3_secret_key]
    environment:
      - VOLS3_PROXY_ENABLE=true
      - VOLS3_PROXY_ENGINE=haproxy
      - VOLS3_PROXY_LOCAL_SERVICES=minio
      - VOLS3_PROXY_BACKEND_PORT=9000
      - VOLS3_PROXY_HEALTH_PATH=/minio/health/ready
      - VOLS3_PROVIDER=Minio
      - VOLS3_RCLONE_REMOTE=S3:mybucket
      - VOLS3_MOUNTPOINT=/mnt/s3
      - VOLS3_ACCESS_KEY_FILE=/run/secrets/s3_access_key
      - VOLS3_SECRET_KEY_FILE=/run/secrets/s3_secret_key
      - VOLS3_RCLONE_ARGS=--vfs-cache-mode=writes --dir-cache-time=12h
      - VOLS3_UNMOUNT_ON_EXIT=true
      - VOLS3_AUTOCREATE_BUCKET=false
      - VOLS3_AUTOCREATE_PREFIX=true
    deploy:
      mode: global
      placement: { constraints: [node.labels.mount_s3 == true] }
      restart_policy: { condition: any }
```
Use the mount from application containers:
```yaml
services:
  app:
    image: your/app:latest
    volumes:
      - type: bind
        source: /mnt/s3
        target: /data
        bind: { propagation: rshared }
    deploy:
      placement: { constraints: [node.labels.mount_s3 == true] }
```

---

## Configuration (env vars)

### Basic
| Variable | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `VOLS3_ENDPOINT` | url | yes | - | S3 endpoint (e.g. http://s3.local:9000) |
| `VOLS3_PROVIDER` | string | no | empty | S3 provider hint: `Minio`/`AWS` |
| `VOLS3_RCLONE_REMOTE` | string | yes | `S3:bucket` | rclone remote (e.g. `S3:bucket`) |
| `VOLS3_MOUNTPOINT` | path | yes | `/mnt/s3` | Host mountpoint |
| `VOLS3_ACCESS_KEY_FILE` | path | yes | `/run/secrets/s3_access_key` | AccessKey secret file |
| `VOLS3_SECRET_KEY_FILE` | path | yes | `/run/secrets/s3_secret_key` | SecretKey secret file |
| `VOLS3_RCLONE_ARGS` | string | no | empty | Extra rclone args |

### HAProxy / Node-local LB
| Variable | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `VOLS3_PROXY_ENABLE` | bool | no | `false` | Enable built-in reverse proxy |
| `VOLS3_PROXY_ENGINE` | string | no | `haproxy` | Proxy engine |
| `VOLS3_PROXY_LOCAL_SERVICES` | csv | when enabled | `minio-local` | Local backend service names (comma-separated) |
| `VOLS3_PROXY_REMOTE_SERVICE` | string | no | `minio-remote` | Optional remote service name |
| `VOLS3_PROXY_BACKEND_PORT` | int | when enabled | `9000` | Backend port |
| `VOLS3_PROXY_HEALTH_PATH` | string | no | `/minio/health/ready` | Health check path |
| `VOLS3_PROXY_LOCAL_LB` | bool | no | `false` | Per-node alias mode |
| `VOLS3_PROXY_NETWORK` | string | when local LB | empty | Overlay network (attachable) |
| `VOLS3_PROXY_PORT` | int | when enabled | `8081` | HAProxy listen port |

### rclone image/update
| Variable | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `VOLS3_DEFAULT_RCLONE_IMAGE` | string | no | `rclone/rclone:latest` | Default rclone image baked in |
| `VOLS3_RCLONE_IMAGE` | string | no | inherits default | Override rclone image at runtime |
| `VOLS3_RCLONE_UPDATE_MODE` | enum | no | `never` | `never`/`periodic`/`on_change` |
| `VOLS3_RCLONE_PULL_INTERVAL` | duration | no | `24h` | Pull interval for `periodic` |

### Cleanup & autocreation
| Variable | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `VOLS3_UNMOUNT_ON_EXIT` | bool | no | `true` | Lazy unmount & remove mounter on exit |
| `VOLS3_AUTOCREATE_BUCKET` | bool | no | `false` | Autocreate bucket (if backend supports) |
| `VOLS3_AUTOCREATE_PREFIX` | bool | no | `true` | Autocreate prefix (directory) |
| `VOLS3_READ_ONLY` | bool | no | `false` | Enforce read-only (skips remote mkdir) |

---

## Deployment Modes
- Single backend: set `VOLS3_PROXY_LOCAL_SERVICES=minio`, mounter reaches `tasks.minio:9000`
- Multiple services (per node): comma-separated `VOLS3_PROXY_LOCAL_SERVICES=minio1,minio2,...`
- Node-local LB: enable `VOLS3_PROXY_LOCAL_LB=true` and set `VOLS3_PROXY_NETWORK`; rclone uses `volume-s3-lb-<hostname>`

---

## Operations
- HTTP:
  - `/ready` readiness (write-probe or RO-aware when `VOLS3_READ_ONLY=true`)
  - `/healthz` liveness
  - `/status` JSON snapshot
  - `/validate` config validation (JSON)
  - `/metrics` Prometheus (enable `VOLS3_ENABLE_METRICS=true`)
- Logs: JSON `slog`; configurable `VOLS3_LOG_LEVEL=debug|info|warn|error`

---

## Security & Best Practices
- Least-privileged S3 credentials; rotate periodically
- Container: non-root, read-only rootfs, `no-new-privileges:true`, drop `NET_RAW`
- Docker API: consider docker-socket-proxy with minimal endpoints

---

## FAQ
- Should MinIO start first?
  - Recommended yes. Controller retries until backend is available.
- Will `tasks.<service>` connect to other nodes’ proxies?
  - It resolves backend service replicas. For node-local LB, use `volume-s3-lb-<hostname>`.

---

## License
MIT (see `LICENSE`).

## Contributing
PRs/Issues welcome (see `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`).

## Example: S3-compatible cluster with node-local HAProxy (nearest + LB + failover)
Node-local LB with HAProxy is recommended for Swarm: each node’s rclone connects to its own local proxy alias for lowest latency, while HAProxy performs health checks and failover. Strategy: `leastconn`.

Assumptions:
- You already have an S3-compatible cluster reachable via multiple Swarm services (e.g., `s3node1,s3node2,...`) on port 9000
- Each service exposes an HTTP readiness endpoint (e.g., `/health`)

```yaml
version: "3.8"

networks:
  s3_net:
    driver: overlay
    attachable: true

secrets:
  s3_access_key: { external: true }
  s3_secret_key: { external: true }

services:
  volume-s3:
    image: ghcr.io/swarmnative/volume-s3:latest
    networks: [s3_net]
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - type: bind
        source: /mnt/s3
        target: /mnt/s3
        bind: { propagation: rshared }
    secrets: [s3_access_key, s3_secret_key]
    environment:
      # Proxy: multi-service backends, node-local alias (nearest + failover)
      - VOLS3_PROXY_ENABLE=true
      - VOLS3_PROXY_ENGINE=haproxy
      - VOLS3_PROXY_LOCAL_SERVICES=s3node1,s3node2,s3node3,s3node4
      - VOLS3_PROXY_BACKEND_PORT=9000
      - VOLS3_PROXY_HEALTH_PATH=/health
      - VOLS3_PROXY_LOCAL_LB=true
      - VOLS3_PROXY_NETWORK=s3_net
      - VOLS3_PROXY_PORT=8081
      # S3 and rclone
      - VOLS3_RCLONE_REMOTE=S3:team-bucket
      - VOLS3_MOUNTPOINT=/mnt/s3
      - VOLS3_ACCESS_KEY_FILE=/run/secrets/s3_access_key
      - VOLS3_SECRET_KEY_FILE=/run/secrets/s3_secret_key
      # Tuning: many small files & deep dirs (balanced cache/mem)
      - VOLS3_RCLONE_ARGS=--vfs-cache-mode=full --vfs-cache-max-size=4G --vfs-cache-max-age=48h --dir-cache-time=24h --attr-timeout=2s --buffer-size=8M --s3-chunk-size=8M --s3-upload-concurrency=4 --s3-max-upload-parts=10000
      - VOLS3_UNMOUNT_ON_EXIT=true
      - VOLS3_AUTOCREATE_PREFIX=true
    deploy:
      mode: global
      placement: { constraints: [node.labels.mount_s3 == true] }
      restart_policy: { condition: any }
```

Notes:
- `--vfs-cache-mode=full` improves read patterns for many small files; adjust cache size/age based on disk space.
- For S3-compatible endpoints requiring path-style addressing, add `--s3-force-path-style=true`. For AWS, also add `--s3-region=<region>`.
