# portwake

按需拉起项目的流量入口：入口端口收到连接时自动启动项目，就绪后转发；空闲超时后自动停止。

支持三种项目类型（runner）：

| 类型 | 启动方式 | 停止方式 |
|---|---|---|
| `compose`（默认） | `docker compose up -d` | `docker compose stop` |
| `docker` | `docker run -d` / `docker start` | `docker stop` |
| `exec` | 直接启动前台二进制 | 向进程组发停止信号 |

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
| `--state-dir` | `<db>.state` | exec 项目的 pid 文件和日志目录 |
| `--tui` | false | 显示 bubbletea 仪表盘 |
| `--grace` | `10s` | 退出时等待现有连接的时长 |
| `--log-level` | `info` | 非 TUI 模式的日志级别；`debug` 会显示每个连接 |

`compose` / `docker` 类型需要 `docker` 在 PATH 中（compose 需要 v2）；只用 `exec` 时不需要 docker。

## 安装

不想自己编译时，从 [GitHub Releases](https://github.com/byebyebruce/portwake/releases) 下载对应平台的压缩包（Linux / macOS，amd64 / arm64）：

```sh
tar -xzf portwake_<version>_linux_amd64.tar.gz
./portwake --version
```

发布流程由 `.github/workflows/release.yml` 完成：推送 `v*` tag 时自动交叉编译并创建 Release，同时生成 `checksums.txt`。

```sh
git tag v0.1.0
git push origin v0.1.0
```

## Docker

仓库包含 `Dockerfile` 和 `docker-compose.yaml`，镜像内自带 `docker` / `docker compose` CLI：

```sh
docker compose up -d --build
```

管理页仍然是 http://127.0.0.1:9000。`docker-compose.yaml` 使用本地构建的 `portwake:latest` 镜像。

容器化运行的前提：

- **挂载 Docker socket**：`/var/run/docker.sock`（已配置）。`compose` 和 `docker` 类型都通过它操作宿主机 daemon。
- **项目目录同路径挂载（compose）**：`docker compose` 在容器内执行，配置里的 `dir`、`files` 和 compose 里的 bind mount 路径必须与宿主机一致，例如 `- /srv:/srv`。
- **入口端口要发布**：默认只发布 9000；配置里每个 `listen` 端口都要加到 `docker-compose.yaml` 的 `ports` 中。
- **upstream 必须从容器内可达**：bridge 模式下写 `host.docker.internal:18080`（已通过 `extra_hosts` 配置）；Linux 也可以改用 `network_mode: host`，此时可以直接写 `127.0.0.1`。
- **exec 类型不适合放在容器里**：它启动的进程会跑在 portwake 容器内，而不是宿主机上。pid 文件和日志位于 `--state-dir`（默认 `<db>.state`），随 `/data` 卷持久化。
- **文件模式**：挂载一个目录而不是单个文件（Web 写回使用原子 rename），例如 `- ./config:/config`，并加 `--config /config/portwake.yaml`。

> 挂载 docker socket 等价于把宿主机 root 权限交给容器；Web 页面没有鉴权，`docker-compose.yaml` 默认只把 9000 发布到 `127.0.0.1`。

## 配置

公共字段（三种类型通用）：

```yaml
projects:
  - name: blog                 # 项目名，匹配 [a-z0-9][a-z0-9_-]*
    type: compose              # compose（默认）/ docker / exec
    idle_timeout: 15m          # 默认 15m；0 = 永不自动停止
    start_timeout: 3m          # 默认 3m
    stop_timeout: 10s          # 默认 10s；超时后强制终止
    routes:
      - listen: 8080           # 入口端口，全局唯一
        bind: 0.0.0.0          # 默认 0.0.0.0
        mode: http             # tcp（默认）或 http
        upstream: 127.0.0.1:18080
        health_service: app    # 仅 compose：等该 service 的 healthcheck 变为 healthy
```

### compose（默认）

```yaml
  - name: blog
    # type: compose            # 可省略
    dir: /srv/blog             # compose 文件所在目录（必填）
    files: [compose.yaml]      # 可选；留空时按 compose 规则查找（compose.yaml 优先，也支持 docker-compose.yaml）
    env_file: .env             # 可选
    profiles: []               # 可选
```

**后端项目要自己把端口发布到宿主机**，并且不能和入口端口冲突，例如：

```yaml
services:
  app:
    ports: ["127.0.0.1:18080:80"]
```

### docker

```yaml
  - name: redis
    type: docker
    image: redis:7-alpine                  # 必填
    container_name: portwake-redis         # 可选，默认 portwake-<项目名>
    command: ["redis-server", "--appendonly", "yes"]  # 可选，覆盖镜像 CMD
    env:
      TZ: Asia/Shanghai
    env_file: .env                         # 可选，相对路径基于 portwake 的工作目录
    ports: ["127.0.0.1:16379:6379"]        # 可选，语法同 docker run -p
    volumes: ["/srv/redis:/data"]          # 可选
    network: host                          # 可选，如 host / bridge
    stop_signal: SIGTERM                   # 可选
    routes:
      - listen: 6380
        upstream: 127.0.0.1:16379
```

首次启动执行 `docker run -d`，之后 `docker start`；停止用 `docker stop`。容器名默认 `portwake-<项目名>`，并带 `portwake.project=<项目名>` 标签。删除项目只停止管理，不会删除容器。

容器已存在时 `Up` 只会 `docker start`，之后修改 `image`、`ports`、`env`、`volumes` 等字段不会应用到旧容器；需要先 `docker rm -f <容器名>`，下一次启动才会重新 `docker run`。

### exec

```yaml
  - name: app
    type: exec
    dir: /srv/app                          # 工作目录，可选
    command: ["./app", "--port", "18080"]  # 必填；argv，不经过 shell
    env:
      LOG_LEVEL: info
    env_file: .env                         # 可选，相对路径基于 dir
    stop_signal: SIGTERM                   # 可选，默认 SIGTERM
    routes:
      - listen: 8081
        upstream: 127.0.0.1:18080
```

- `command` 直接 exec，不做 shell 解析；`&&`、管道、重定向、通配符都不会生效。需要 bash 语法时显式写成 `["bash", "-c", "..."]`（镜像里没有 bash 时用 `sh`）；执行脚本文件则直接写 `["bash", "/srv/app/start.sh"]`。
- 进程必须保持前台：不要加 `&`，也不要自行 daemonize 或写 pid 文件。portwake 把它放进独立进程组，停止时向整个进程组发信号；`stop_timeout` 后仍未退出则 `SIGKILL`。
- stdout/stderr 写入 `<state-dir>/logs/<name>.log`（下一次全新启动时超过 4MiB 会轮转），Web 项目卡片的「日志」可查看。
- pid 等状态写在 `<state-dir>/<name>.json`。portwake 退出时进程保持运行；重启后如果进程还活着会被接管并继续空闲计时，不会重复启动。
- 如果二进制自己 daemonize 或改变进程组，portwake 将无法跟踪，请使用前台模式。

### 通用行为

配置里的项目全部由 portwake 接管：即使是手动启动的，空闲后也会被停止。不想被停的项目把 `idle_timeout` 设为 `0`。

### 日志

Web 项目卡片上的「日志」按钮：compose 通过 `docker compose logs`、docker 通过 `docker logs`、exec 读取本地日志文件。

### 配置来源：db 模式与文件模式

默认是 **db 模式**：项目配置保存在 `--db` 指定的 SQLite 中，Web 页面的增删改直接写数据库；`--import` 只在启动时用 YAML 替换一次已保存的配置。

加上 `--config <file>` 切换到 **文件模式**：

- 启动时从该 YAML 读取配置；文件不存在或校验失败则启动失败。
- Web 页面 / API 的增删改和 YAML 导入会原子写回该文件（文件会被重新序列化，手写注释不会保留）。
- 直接编辑文件后需要重启 portwake 才会生效。
- `--config` 与 `--import` 不能同时使用。

两种模式下，事件和统计都保存在 `--db` 指定的 SQLite 中。

### 就绪判断

- compose 项目配置了 `health_service`，并且该 service 有 healthcheck：等它变为 `healthy`。
- 否则：`http` 路由要求 upstream 返回任意 HTTP 响应；`tcp` 路由要求连接能保持打开（docker 的端口代理在容器内服务还没监听时，会接受连接然后立刻关闭）。
- exec 进程在就绪前退出时立即判定启动失败，错误里附带日志末尾，不用等满 `start_timeout`。

## 测试

```sh
go test ./...                              # 使用伪造的 runner 和测试辅助进程
go test -tags integration ./internal/app/  # 使用真实 docker 跑 nginx compose 项目
```
