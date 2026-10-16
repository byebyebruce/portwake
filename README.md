# dcpentry

按需拉起 docker compose 项目的流量入口：入口端口收到连接时自动 `docker compose up -d`，就绪后转发；空闲超时后自动 `docker compose stop`。

## 运行

```sh
go build -o dcpentry ./cmd/dcpentry
./dcpentry                       # Web 管理页 http://127.0.0.1:9000，日志打印到 stderr
./dcpentry --tui                 # 终端仪表盘
./dcpentry --import config.yaml  # 用 YAML 替换已保存的配置
```

| 参数 | 默认 | 说明 |
|---|---|---|
| `--db` | `./dcpentry.db` | SQLite 数据库（配置、事件、统计） |
| `--web` | `127.0.0.1:9000` | Web 管理页监听地址（无鉴权，建议只绑本机） |
| `--import` | | 启动时导入的 YAML |
| `--tui` | false | 显示 bubbletea 仪表盘 |
| `--grace` | `10s` | 退出时等待现有连接的时长 |
| `--log-level` | `info` | 非 TUI 模式的日志级别；`debug` 会显示每个连接 |

`docker` 需要在 PATH 中，并且支持 compose v2。

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

配置里的项目全部由 dcpentry 接管：即使是手动启动的，空闲后也会被停止。不想被停的项目把 `idle_timeout` 设为 `0`。

### 就绪判断

- 配置了 `health_service`，并且该 service 有 healthcheck：等它变为 `healthy`。
- 否则：`http` 路由要求 upstream 返回任意 HTTP 响应；`tcp` 路由要求连接能保持打开（docker 的端口代理在容器内服务还没监听时，会接受连接然后立刻关闭）。

## 测试

```sh
go test ./...                              # 使用伪造的 compose runner
go test -tags integration ./internal/app/  # 使用真实 docker 跑 nginx
```
