[English](README.md) | 简体中文

# volume-s3

![CI](https://img.shields.io/github/actions/workflow/status/swarmnative/swarm-s3-mounter/publish.yml?branch=main)
![Release](https://img.shields.io/github/v/release/swarmnative/swarm-s3-mounter)
![License](https://img.shields.io/github/license/swarmnative/swarm-s3-mounter)
![Docker Pulls](https://img.shields.io/docker/pulls/swarmnative/swarm-s3-mounter)
![Image Size](https://img.shields.io/docker/image-size/swarmnative/swarm-s3-mounter/latest)
![Go Version](https://img.shields.io/github/go-mod/go-version/swarmnative/swarm-s3-mounter)
![Last Commit](https://img.shields.io/github/last-commit/swarmnative/swarm-s3-mounter)
![Issues](https://img.shields.io/github/issues/swarmnative/swarm-s3-mounter)
![PRs](https://img.shields.io/github/issues-pr/swarmnative/swarm-s3-mounter)

为单机 Docker 与 Docker Swarm 提供 S3 兼容对象存储“卷”的轻量控制器：
- 宿主机级 rclone FUSE 挂载（业务容器通过 bind 使用）。
- 内置 HAProxy（可选）用于负载均衡与故障转移。
- 声明式“卷”（前缀）供给，接近 K8s 体验。

---

## 快速开始（最小 Stack）
前提：已初始化 Swarm；目标节点开启 FUSE；为使用挂载的节点打标签。
```bash
docker node update --label-add mount_s3=true <NODE>
```
创建凭据（Swarm secrets）：
```bash
docker secret create s3_access_key -
# 粘贴 AccessKey 回车，Ctrl-D 结束

docker secret create s3_secret_key -
# 粘贴 SecretKey 回车，Ctrl-D 结束
```
部署（单后端 Service 示例，mounter 通过内置 HAProxy 均衡到 tasks.minio:9000）：
```yaml
version: "3.8"

networks:
  s3_net:
    driver: overlay
    internal: true

secrets:
  s3_access_key:
    external: true
  s3_secret_key:
    external: true

services:
  minio:
    image: minio/minio:latest
    command: server --console-address :9001 /data
    environment:
      - MINIO_ROOT_USER_FILE=/run/secrets/s3_access_key
      - MINIO_ROOT_PASSWORD_FILE=/run/secrets/s3_secret_key
    secrets: [s3_access_key, s3_secret_key]
    volumes:
      - /srv/minio/data:/data
    networks: [s3_net]
    healthcheck:
      test: ["CMD-SHELL", "curl -fsS http://localhost:9000/minio/health/ready || exit 1"]
      interval: 10s
      timeout: 3s
      retries: 10
    deploy:
      placement:
        constraints:
          - node.labels.minio == true

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
      placement:
        constraints:
          - node.labels.mount_s3 == true
      restart_policy: { condition: any }
```
业务容器使用挂载：
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
      placement:
        constraints: [node.labels.mount_s3 == true]
```

---

## 配置（环境变量）

### 基本
| 变量 | 说明 | 默认 |
| --- | --- | --- |
| `VOLS3_ENDPOINT` | S3 端点（如 https://s3.local:9000） | 必填 |
| `VOLS3_PROVIDER` | 可选，通用 S3 留空；或 `Minio`/`AWS` | 空 |
| `VOLS3_RCLONE_REMOTE` | rclone 远端（如 `S3:bucket`） | `S3:bucket` |
| `VOLS3_MOUNTPOINT` | 宿主机挂载点 | `/mnt/s3` |
| `VOLS3_ACCESS_KEY_FILE` | AccessKey 的 secret 路径 | `/run/secrets/s3_access_key` |
| `VOLS3_SECRET_KEY_FILE` | SecretKey 的 secret 路径 | `/run/secrets/s3_secret_key` |
| `VOLS3_RCLONE_ARGS` | 追加 rclone 参数（唯一调优入口） | 空 |

### 负载均衡（HAProxy）
| 变量 | 说明 | 默认 |
| --- | --- | --- |
| `VOLS3_PROXY_ENABLE` | 是否启用内置反代 | `false` |
| `VOLS3_PROXY_ENGINE` | 反代引擎（仅 `haproxy`） | `haproxy` |
| `VOLS3_PROXY_LOCAL_SERVICES` | 后端 Service 名，支持逗号分隔 | `minio-local` |
| `VOLS3_PROXY_REMOTE_SERVICE` | 远端 Service 名（可留空） | `minio-remote` |
| `VOLS3_PROXY_BACKEND_PORT` | 后端端口 | `9000` |
| `VOLS3_PROXY_HEALTH_PATH` | 健康检查路径 | `/minio/health/ready` |

### 节点本地 LB（唯一别名）
| 变量 | 说明 | 默认 |
| --- | --- | --- |
| `VOLS3_PROXY_LOCAL_LB` | 启用“每节点本地 LB 别名”模式 | `false` |
| `VOLS3_PROXY_NETWORK` | HAProxy/mounter 所在 overlay 网络（需 attachable） | 空 |
| `VOLS3_PROXY_PORT` | HAProxy 监听端口 | `8081` |
- 别名规范：`volume-s3-lb-<hostname>`；启用后 rclone 端点将自动指向该别名。

### rclone 镜像/更新策略
| 变量 | 说明 | 默认 |
| --- | --- | --- |
| `VOLS3_DEFAULT_RCLONE_IMAGE` | 发布时内嵌的 rclone 镜像 | `rclone/rclone:latest` |
| `VOLS3_RCLONE_IMAGE` | 运行时覆盖 rclone 镜像 | 继承默认 |
| `VOLS3_RCLONE_UPDATE_MODE` | `never`/`periodic`/`on_change` | `never` |
| `VOLS3_RCLONE_PULL_INTERVAL` | `periodic` 模式拉取间隔 | `24h` |

### 清理与自动创建
| 变量 | 说明 | 默认 |
| --- | --- | --- |
| `VOLS3_UNMOUNT_ON_EXIT` | 退出时懒卸并移除本节点 mounter | `true` |
| `VOLS3_AUTOCREATE_BUCKET` | 自动创建桶（后端需支持） | `false` |
| `VOLS3_AUTOCREATE_PREFIX` | 自动创建前缀（目录） | `true` |

---

## 声明式“卷”（基于标签的前缀供给）
默认使用“无前缀”键；也可选用域名前缀（前缀优先，冲突告警）。

在服务的 `labels` 中声明（无前缀示例）：
- `s3.enabled=true`
- `s3.bucket=my-bucket`（可选）
- `s3.prefix=teams/appA/vol-data`
- 预留：`s3.class=throughput|low-latency|low-mem`、`s3.reclaim=Retain|Delete`、`s3.access=rw|ro`、`s3.args=--vfs-cache-max-size=5G`

若需启用统一域前缀（示例 `your-org.io`）：设置 `VOLS3_LABEL_PREFIX=your-org.io`，并改用：
- `your-org.io/s3.enabled=true`
- `your-org.io/s3.bucket=my-bucket`
- `your-org.io/s3.prefix=teams/appA/vol-data`

控制器会在本节点幂等创建 `/mnt/s3/<prefix>` 目录（若启用自动创建亦会尝试创建远端前缀/桶），应用 bind 到该路径即可使用。

---

## 最小 docker run 示例（默认无代理）
```bash
docker run -d --name vols3 \
  -e VOLS3_ENDPOINT=http://s3.local:9000 \
  -e VOLS3_RCLONE_REMOTE=S3:your-bucket \
  -e VOLS3_MOUNTPOINT=/mnt/s3 \
  -e VOLS3_ACCESS_KEY_FILE=/run/secrets/s3_access_key \
  -e VOLS3_SECRET_KEY_FILE=/run/secrets/s3_secret_key \
  -e VOLS3_ENABLE_METRICS=false \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /mnt/s3:/mnt/s3:rshared \
  --restart=always \
  ghcr.io/swarmnative/volume-s3:latest
```

## Prometheus 抓取示例
```yaml
scrape_configs:
  - job_name: 'volume-s3'
    scrape_interval: 30s
    static_configs:
      - targets: ['volume-s3:8080']
```

## 无代理直跑（去 supervisor）
- 当 `VOLS3_PROXY_ENABLE=false`（默认）时，容器入口将直接 `exec volume-ops`，不再启动 supervisor。

---

## FAQ
- MinIO 是否应先启动？
  - 建议先部署并通过健康检查；Swarm 无严格 `depends_on`，控制器会重试直至就绪。
- `tasks.<service>` 会不会连到其他节点的代理？
  - 它解析的是后端 Service 副本 IP，通常用于直连后端而非本项目的 HAProxy。若启用节点本地 LB，请使用 `volume-s3-lb-<hostname>` 端点。


