# portwake

按需拉起 docker compose 项目的流量入口：入口端口收到连接时自动 `docker compose up -d`，就绪后转发；空闲超时后自动 `docker compose stop`。

## 运行

```sh
go build -o portwake ./cmd/portwake
./portwake                        # Web 管理页 http://127.0.0.1:9000，日志打印到 stderr
./portwake --tui                  # 终端仪表盘
./portwake --config example.yaml  # 文件模式：YAML 即配置源，Web 修改写回该文件
./portwake --import config.yaml   # db 模式：启动时用 YAML 替换已保存的配置
```

| 参数 | 默认 | 说明 |
|---|---|---|
| `--db` | `./portwake.db` | SQLite 数据库（db 模式保存项目配置；两种模式都保存事件和统计） |
| `--web` | `127.0.0.1:9000` | Web 管理页监听地址（无鉴权，建议只绑本机） |
| `--config` | | YAML 配置文件；设置后进入文件模式，Web 修改写回该文件 |
| `--import` | | db 模式下启动时导入的 YAML（不能与 `--config` 同用） |
| `--tui` | false | 显示 bubbletea 仪表盘 |
| `--grace` | `10s` | 退出时等待现有连接的时长 |
| `--log-level` | `info` | 非 TUI 模式的日志级别；`debug` 会显示每个连接 |

`docker` 需要在 PATH 中，并且支持 compose v2。

## Docker

仓库包含 `Dockerfile` 和 `docker-compose.yaml`，镜像内自带 `docker` / `docker compose` CLI：

```sh
docker compose up -d --build
```

管理页仍然是 http://127.0.0.1:9000。`docker-compose.yaml` 的镜像名是 `ghcr.io/byebyebruce/portwake:latest`，本地构建会直接打上这个 tag；也可以改用 GitHub Actions 构建并推送到 GHCR 的镜像（`docker pull ghcr.io/byebyebruce/portwake:latest`）。

容器化运行的前提：

- **挂载 Docker socket**：`/var/run/docker.sock`（已配置）。
- **项目目录同路径挂载**：`docker compose` 在容器内执行，配置里的 `dir`、`files` 和 compose 里的 bind mount 路径必须与宿主机一致，例如 `- /srv:/srv`。
- **入口端口要发布**：默认只发布 9000；配置里每个 `listen` 端口都要加到 `docker-compose.yaml` 的 `ports` 中。
- **upstream 必须从容器内可达**：bridge 模式下写 `host.docker.internal:18080`（已通过 `extra_hosts` 配置）；Linux 也可以改用 `network_mode: host`，此时可以直接写 `127.0.0.1`。
- **文件模式**：挂载一个目录而不是单个文件（Web 写回使用原子 rename），例如 `- ./config:/config`，并加 `--config /config/portwake.yaml`。

> 挂载 docker socket 等价于把宿主机 root 权限交给容器；Web 页面没有鉴权，`docker-compose.yaml` 默认只把 9000 发布到 `127.0.0.1`。

`.github/workflows/docker.yml` 会在 push 到 `main`、推送 `v*` tag、PR 或手动触发时构建 `linux/amd64` + `linux/arm64` 镜像；非 PR 推送到 `ghcr.io/byebyebruce/portwake`。使用 CI 镜像时，删掉 `docker-compose.yaml` 里的 `build:` 即可。

## 配置

```yaml
projects:
  - name: blog                 # compose 项目名（-p）
    dir: /srv/blog             # compose 文件所在目录
    files: [compose.yaml]      # 可选，默认按 compose 规则查找
    env_file: .env             # 可选
    profiles: []               # 可选
    idle_timeout: 15m          # 默认 15m；0 = 永不自动停止
    start_timeout: 3m          # 默认 3m
    routes:
      - listen: 8080           # 入口端口
        bind: 0.0.0.0          # 默认 0.0.0.0
        mode: http             # tcp（默认）或 http
        upstream: 127.0.0.1:18080
        health_service: app    # 可选：等该 service 的 healthcheck 变为 healthy
```

**后端项目要自己把端口发布到宿主机**，并且不能和入口端口冲突，例如：

```yaml
services:
  app:
    ports: ["127.0.0.1:18080:80"]
```

配置里的项目全部由 portwake 接管：即使是手动启动的，空闲后也会被停止。不想被停的项目把 `idle_timeout` 设为 `0`。

### 配置来源：db 模式与文件模式

默认是 **db 模式**：项目配置保存在 `--db` 指定的 SQLite 中，Web 页面的增删改直接写数据库；`--import` 只在启动时用 YAML 替换一次已保存的配置。

加上 `--config <file>` 切换到 **文件模式**：

- 启动时从该 YAML 读取配置；文件不存在或校验失败则启动失败。
- Web 页面 / API 的增删改和 YAML 导入会原子写回该文件（文件会被重新序列化，手写注释不会保留）。
- 直接编辑文件后需要重启 portwake 才会生效。
- `--config` 与 `--import` 不能同时使用。

两种模式下，事件和统计都保存在 `--db` 指定的 SQLite 中。

### 就绪判断

- 配置了 `health_service`，并且该 service 有 healthcheck：等它变为 `healthy`。
- 否则：`http` 路由要求 upstream 返回任意 HTTP 响应；`tcp` 路由要求连接能保持打开（docker 的端口代理在容器内服务还没监听时，会接受连接然后立刻关闭）。

## 测试

```sh
go test ./...                              # 使用伪造的 compose runner
go test -tags integration ./internal/app/  # 使用真实 docker 跑 nginx
```
