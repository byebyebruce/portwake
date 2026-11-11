# portwake 规格说明

## Problem Statement

我在单机上维护了很多 docker compose 项目，它们大部分时间都没人访问，却一直占着内存和 CPU。手动 `up` / `stop` 很麻烦：每次想用某个服务都得先记起它在哪个目录、敲命令、等它起来；用完又常常忘记停掉。我需要一个入口，让我像访问常驻服务一样访问这些项目，而它们只在真正有流量时才运行。

另外有些服务只是一个容器（Redis、Postgres 之类），或者只是一个编译好的二进制，用 compose 包一层并不划算。我希望同一个入口也能按需拉起它们，而不是维护多套工具。

## Solution

portwake 是一个用 Go 编写的单二进制流量入口，在宿主机上手动启动。它为每个被管理的项目监听一个或多个入口端口。项目有三种 runner：`compose`（docker compose 项目，默认）、`docker`（单个容器）、`exec`（前台二进制），由统一的 runner 管理器调度：

- 有连接进来时，如果项目没在运行，就通过对应的 runner 拉起它（`docker compose up -d` / `docker run -d` / 直接执行二进制），等就绪后再把挂起的连接转发过去；
- 项目连续一段时间（默认 15 分钟）没有活跃连接，就自动停止（`docker compose stop` / `docker stop` / 向进程组发停止信号）；
- 支持 TCP（任意协议）和 HTTP（带访问日志和请求统计）两种转发模式；
- 所有配置存在 SQLite 中，可以通过内嵌的 Web 页面编辑，也可以导入导出 YAML；
- Web 页面展示实时状态、事件日志和统计（拉起次数、启动耗时、运行时长、流量趋势），并可查看每个项目的日志（compose/docker 通过 docker CLI，exec 读取捕获的日志文件）；
- 加 `--tui` 参数时在终端显示 bubbletea 界面，否则用 charm log 打印日志。

## User Stories

### 转发与按需拉起

1. 作为用户，我想访问某个入口端口时，portwake 自动拉起对应的 compose 项目，这样就不用手动 `up`。
2. 作为用户，我想在项目启动期间进来的连接被挂起，就绪后再转发，这样第一次访问也能成功，不会立刻报错。
3. 作为用户，我想启动期间的多个并发连接只触发一次 `up`，这样不会重复执行 compose 命令。
4. 作为用户，我想项目已经在运行时，连接直接转发、不产生额外延迟，这样日常使用无感。
5. 作为用户，我想为每个入口端口选择 TCP 或 HTTP 模式，这样数据库、Redis 和 Web 服务都能接入。
6. 作为用户，我想 TCP 模式能透明转发任意协议的字节流，这样 MySQL、Redis、SSH 等服务也能按需拉起。
7. 作为用户，我想 HTTP 模式支持 WebSocket 升级，这样 Web 应用的实时功能正常工作。
8. 作为用户，我想一个项目可以挂多个入口端口（比如 Web 和 admin），共享同一个启停状态，这样访问其中任何一个都会拉起整个项目。
9. 作为用户，我想在配置里直接写上游地址（比如 `127.0.0.1:18080`），这样转发目标清楚明确。
10. 作为用户，我想为每个入口端口单独配置 bind 地址（默认 `0.0.0.0`），这样可以决定哪些端口对局域网开放。

### 就绪判断与启动失败

11. 作为用户，我想在指定了 `health_service` 且该 service 配置了 compose healthcheck 时，portwake 等它变成 `healthy` 才算就绪，这样不会把流量发给还没准备好的应用。
12. 作为用户，我想在没有 healthcheck 时，portwake 通过对上游地址做 TCP 探测判断就绪，这样无需额外配置也能工作。
13. 作为用户，我想为每个项目配置启动超时（默认 3 分钟），这样慢启动的项目也能正常拉起。
14. 作为用户，我想启动超时或 `up` 失败时，挂起的 TCP 连接被关闭、HTTP 请求得到 502/503，这样客户端不会无限等待。
15. 作为用户，我想启动失败时 compose 的错误输出被记录到事件里，这样我能在 Web、TUI 或日志中排查原因。
16. 作为用户，我想启动失败后，下一次有流量时再重新尝试拉起，这样临时故障可以自动恢复。

### 空闲停止

17. 作为用户，我想项目活跃连接数为 0 并持续 `idle_timeout`（默认 15 分钟）后被自动 `stop`，这样能释放资源。
18. 作为用户，我想有长连接挂着时项目不会被停掉，这样 WebSocket、数据库连接不会被误伤。
19. 作为用户，我想把 `idle_timeout` 设为 0 表示永不自动停止，这样常用项目可以一直保持运行。
20. 作为用户，我想正在 stop 时进来的连接等 stop 完成后立即重新拉起项目，这样不会因为时机不巧而访问失败。
21. 作为用户，我想停止使用 `stop` 而不是 `down`，这样下次拉起更快。
22. 作为用户，我想看到每个运行中项目的空闲倒计时，这样知道它什么时候会被停掉。

### 外部状态同步

23. 作为用户，我想配置里的项目全部由 portwake 接管，不管是谁启动的，这样行为统一。
24. 作为用户，我想 portwake 启动时把已在运行的项目识别为 running，并立刻开始空闲计时，这样重启 portwake 后状态是对的。
25. 作为用户，我想 portwake 定期（每 10 秒）核对项目的真实运行状态，这样我手动 `stop`/`up` 之后界面状态也是准确的。
26. 作为用户，我想手动在命令行启动的项目也会进入空闲计时并被自动停止，这样我不必记得收尾。

### 手动控制

27. 作为用户，我想在 Web 页面上手动启动某个项目，这样可以提前预热。
28. 作为用户，我想在 Web 页面上手动停止某个项目，这样可以立刻释放资源。
29. 作为用户，我想在 TUI 中用快捷键启动或停止选中的项目，这样不用切到浏览器。

### 配置管理

30. 作为用户，我想通过 Web 表单新增项目，填写名称、目录、compose 文件、env 文件、profiles、超时和路由，这样不用手写配置。
31. 作为用户，我想通过 Web 表单编辑和删除项目及路由，这样维护配置很方便。
32. 作为用户，我想修改配置后立刻生效、不用重启 portwake，这样调整成本低。
33. 作为用户，我想修改入口端口时已建立的连接不被断开，这样改配置不影响正在使用的会话。
34. 作为用户，我想修改上游地址只影响新连接，这样切换是平滑的。
35. 作为用户，我想删除项目时只停止它的监听、不停止项目本身，这样不会误杀正在用的服务。
36. 作为用户，我想新端口被占用或配置无效时保存失败并显示原因，配置回滚到修改前，这样数据库里的配置永远是可用的。
37. 作为用户，我想把全部配置导出为 YAML，这样可以备份或迁移。
38. 作为用户，我想从 YAML 导入配置（Web 页面或 `--import` 参数），这样首次使用或迁移时能快速初始化。
39. 作为用户，我想数据库默认放在当前目录（`./portwake.db`），并且能用 `--db` 指定其他路径，这样不同工作目录可以有各自的实例。
40. 作为用户，我想端口和项目名在保存时被校验唯一，这样不会产生冲突的配置。

### 状态与统计

41. 作为用户，我想在 Web 页面上实时看到每个项目的状态（stopped/starting/running/stopping）、活跃连接数和运行时长，这样一眼就能了解全局。
42. 作为用户，我想页面通过 SSE 实时更新、无需刷新，这样能看到拉起过程。
43. 作为用户，我想查看事件日志（连接、拉起、就绪、停止、错误），这样能了解发生了什么。
44. 作为用户，我想看到每次拉起的记录：时间、触发端口、启动耗时、结果，这样能评估冷启动体验。
45. 作为用户，我想看到每次停止的记录：时间、原因（空闲/手动/外部）、本次运行时长，这样能了解资源使用情况。
46. 作为用户，我想看到每个项目的拉起次数、平均启动耗时、累计运行时长，这样能决定哪些项目应该常驻。
47. 作为用户，我想看到每个项目或端口的连接数、上下行字节数、HTTP 请求数的趋势图，这样能了解使用模式。
48. 作为用户，我想在 HTTP 模式下看到请求级访问日志（方法、路径、状态码、耗时），这样能排查 Web 问题。
49. 作为用户，我想原始事件保留 30 天、按小时汇总的数据保留 1 年，这样数据库不会无限增长。

### 界面

50. 作为用户，我想 Web 页面是嵌入二进制的单个 HTML 文件，所有前端资源也内嵌、不依赖外部网络，这样离线也能用、部署只需一个文件。
51. 作为用户，我想 Web 页面美观、跟随系统深浅色，这样看着舒服。
52. 作为用户，我想 Web 服务默认只监听 `127.0.0.1:9000`，并且可以用参数修改，这样在没有鉴权的情况下也是安全的。
53. 作为用户，我想用 `--tui` 打开终端界面，看到项目表格（状态、端口、连接数、运行时长、空闲倒计时）和事件日志，这样不用打开浏览器也能监控。
54. 作为用户，我想不加 `--tui` 时用 charm log 打印结构化、带颜色的日志，这样终端输出好读。
55. 作为用户，我想无论是否开启 TUI，Web 服务都始终启动，这样随时能打开页面管理。

### 进程生命周期

56. 作为用户，我想按 Ctrl+C 或收到 SIGTERM 时，portwake 先关闭入口监听器，等现有连接最多 10 秒（`--grace` 可调）后退出，这样正在进行的请求有机会完成。
57. 作为用户，我想 portwake 退出时不停止任何项目，这样重启 portwake 不会中断服务。
58. 作为用户，我想 portwake 从 PATH 中找到 `docker` 命令，这样手动启动时不需要额外配置。

### 项目类型与日志

59. 作为用户，我想用 `type: compose` 管理 docker compose 项目（默认值，兼容旧配置），这样现有配置不用改。
60. 作为用户，我想用 `type: docker` 管理单个容器，由 portwake 负责 `docker run` / `start` / `stop`，这样常用中间件不用写 compose 文件。
61. 作为用户，我想用 `type: exec` 直接运行前台二进制，并用 argv 数组配置命令，这样编译产物不需要 shell 和 compose。
62. 作为用户，我想 exec 进程被放进独立进程组，停止时向整个进程组发信号、超时后 `SIGKILL`，这样父进程带起来的子进程也能被清理。
63. 作为用户，我想为 docker/exec 配置停止信号和停止超时，这样不同的程序可以优雅退出。
64. 作为用户，我想 portwake 退出时 exec 进程继续运行，重启后如果进程还活着就被接管并继续空闲计时，这样重启 portwake 不会中断服务。
65. 作为用户，我想 exec 的 stdout/stderr 被捕获到文件，并能在 Web 上查看最后若干行，这样排查启动失败很方便；日志过大时在下次全新启动时轮转。
66. 作为用户，我想 exec 在就绪前退出时立即报错并附上日志末尾，这样不用等满 `start_timeout`。
67. 作为用户，我想 Web 项目卡片上有「日志」按钮，三种类型用同一套接口查看日志。
68. 作为用户，我想配置按类型校验字段（compose 必须有 `dir`、docker 必须有 `image`、exec 必须有 `command`，且不接受不属于该类型的字段），这样错误配置在保存时就被拒绝。
69. 作为用户，我想只用 exec 时不需要安装 docker，这样这个工具在纯二进制场景也能用。

## Implementation Decisions

### 总体架构

- 单个 Go 二进制，Go 1.26+，module 名为 `portwake`，CLI 使用 cobra。
- 默认在宿主机上运行；也提供 Docker 镜像。容器内运行时必须挂载 docker socket，并以相同绝对路径挂载被管理的 compose 项目目录，上游地址必须从容器内可达（见 README）。
- 进程内的模块划分：
  - **runner**：定义统一的 `Runner` 接口（Up/Stop/Ps）、按项目类型分发的 `Manager`，以及可选的 `LogSource`（Logs）和 `Alive`（启动阶段存活探测）能力。
  - **compose**：封装 `docker compose` CLI 的 runner。
  - **docker**：通过 `docker` CLI 管理单个容器（run/start/stop/inspect/logs）的 runner。
  - **process**：exec runner，负责进程组、pid/日志文件、停止信号升级和重启接管。
  - **project**：每个项目一个状态机，负责启停、就绪等待、空闲计时、外部状态核对和连接计数。
  - **proxy**：入口监听器管理（按配置差异增删监听器），以及 TCP 和 HTTP 两种转发实现。
  - **store**：基于 SQLite 的配置、事件、统计存储，包含 YAML 导入导出和数据保留清理。
  - **events**：进程内事件总线，供 store、Web SSE、TUI、日志输出订阅。
  - **web**：JSON API、SSE 和内嵌的静态页面。
  - **tui**：bubbletea 界面，消费事件总线和状态快照。
  - **app**：组装以上模块，是测试和 CLI 的统一入口。

### Runner 抽象（唯一的测试接缝）

只把启动/停止/查询项目的部分抽象成 `runner.Runner` 接口，它是唯一可以被替换的依赖。`runner.Manager` 按 `project.RunnerType()` 把调用分发给 compose、docker、exec 三个 runner，对应 runner 为 nil 时返回 `ErrNoRunner`。操作语义如下：

- **Up(project)**：确保项目已启动，不等待应用就绪。
  - compose：`docker compose -p <name> -f <files...> [--env-file] [--profile...] up -d`，工作目录为项目的 `dir`。
  - docker：容器不存在时 `docker run -d`（带 `--name`、`--label portwake.project=<name>`、env/端口/卷/网络参数）；已存在但停止时 `docker start`；已在运行则什么也不做。
  - exec：状态文件里记录的进程还活着就接管；否则启动新进程（独立进程组，stdout/stderr 写入日志文件）。
- **Stop(project)**：compose 执行 `docker compose ... stop`；docker 执行 `docker stop --time <秒> [--signal SIG]`；exec 向进程组发停止信号（默认 `SIGTERM`），`stop_timeout` 后升级为 `SIGKILL`。
- **Ps(project)**：返回各 service/容器的运行状态和健康状态，用于启动时识别状态、定期核对和 healthcheck 就绪判断。exec 用 pid 文件加 `kill(pid, 0)` 判断。

可选接口：

- **LogSource.Logs(ctx, project, lines)**：返回最近若干行日志。compose/docker 调用 docker CLI，exec 读取 `<state-dir>/logs/<name>.log`。
- **Alive.Alive(project)**：启动等待期间快速判断进程是否还活着，exec 用它在进程提前退出时立即失败。

上游的 TCP 探测由 project 模块直接发起网络连接，不走这个接口。

### 项目状态机

```
stopped  --(连接到达 / 手动启动)-->  starting
starting --(就绪)-->                running
starting --(超时 / up 失败)-->       stopped   (挂起连接全部失败，记录错误)
running  --(空闲到期 / 手动停止)-->   stopping
running  --(核对发现已停)-->         stopped   (原因: external)
stopping --(stop 完成)-->           stopped   (若期间有连接到达，立即进入 starting)
stopped  --(核对发现在跑)-->         running   (立即开始空闲计时)
```

- 每个项目的状态转移在自己的 goroutine 或锁内串行执行。
- 挂起的连接不设数量上限，只受 `start_timeout` 约束。
- 空闲计时：活跃连接数降为 0 时开始计时，有新连接时取消；`idle_timeout = 0` 表示永不自动停止。
- 定期核对的间隔默认 10 秒，可配置（测试中用毫秒级）。

### 就绪判断

- 如果 route 指定了 `health_service`，并且该 service 有 compose healthcheck：轮询 `Ps`，直到它变成 `healthy`。
- 否则探测 upstream：`http` 路由要求返回任意 HTTP 响应；`tcp` 路由要求连接保持打开约 150ms（或收到数据）。原因是 Docker 的端口代理会在容器内服务还没监听时就接受连接，然后立刻关闭，单纯拨号成功不能说明服务已就绪。
- exec 项目在等待期间如果 `Alive` 返回 false（进程已退出），立即判定启动失败，并把日志末尾附在错误里，不用等满 `start_timeout`。
- 总时长受 `start_timeout` 约束（默认 3 分钟）。

### 配置模型

- **项目公共字段**：`name`（唯一）、`type`（`compose` 默认 / `docker` / `exec`）、`env_file`（可选；compose/exec 的相对路径基于 `dir`，docker 基于 portwake 的工作目录）、`env`（可选，键值对）、`idle_timeout`（默认 15 分钟）、`start_timeout`（默认 3 分钟）、`stop_timeout`（默认 10 秒）、`stop_signal`（默认 `SIGTERM`，仅 docker/exec）、`routes`。
- **compose 字段**：`dir`（必填）、`files`（可选；留空时使用 compose 的默认查找顺序：`compose.yaml`、`compose.yml`、`docker-compose.yaml`、`docker-compose.yml`，同时存在时靠前者优先）、`profiles`（可选）。项目名同时作为 compose 项目名。
- **docker 字段**：`image`（必填）、`container_name`（默认 `portwake-<name>`）、`command`（覆盖镜像 CMD）、`volumes`、`ports`、`network`。
- **exec 字段**：`command`（argv 数组，必填，不经过 shell）、`dir`（工作目录，可选）。
- **路由**：`listen`（端口，全局唯一）、`bind`（默认 `0.0.0.0`）、`mode`（`tcp` 或 `http`）、`upstream`（`host:port`）、`health_service`（可选，仅 compose）。
- 保存时按类型校验：缺少必填字段，或出现不属于该类型的字段（例如 compose 项目带 `image`、docker 项目带 `files`），都会拒绝。
- docker 容器带 `portwake.project=<name>` 标签；删除项目只停止管理，不删除容器和卷。
- exec 的状态文件是 `<state-dir>/<name>.json`（pid、启动时间、命令），日志是 `<state-dir>/logs/<name>.log`；`--state-dir` 默认是 `<db>.state`。
- 默认由 SQLite 保存项目配置（使用 `modernc.org/sqlite` 驱动，纯 Go，不需要 cgo）；指定 `--config <yaml>` 时，YAML 文件成为唯一配置源，Web 修改原子写回该文件。事件和统计始终保存在 SQLite 中。
- YAML 导入导出的结构和上面的模型一一对应。导入时整体校验，失败就不写入任何内容。

### 配置热更新

- 每次写入配置都在一个事务里完成：先校验，再按差异把变更应用到运行中的监听器和项目。任一监听失败就回滚数据库事务，并恢复原有的监听器状态，然后向调用方返回错误。
- 修改端口或 bind 地址：关闭旧监听器、启动新监听器，已建立的连接保持不断。
- 修改 upstream：只影响新连接。
- 删除项目：关闭它的监听器，不停止项目。
- 修改 `idle_timeout` 或 `start_timeout`：立即对后续的计时生效。

### 存储结构（逻辑层面）

- **projects、routes**：保存配置。
- **events**：保存原始事件。字段包括时间、项目、端口、类型（`conn_open`、`conn_close`、`start_begin`、`start_ok`、`start_fail`、`stop_begin`、`stop_done`、`http_request`、`config_change`、`error`），以及载荷（耗时、原因、错误输出、字节数、HTTP 方法、路径、状态码等）。保留 30 天。
- **stats_hourly**：按项目、端口、小时汇总的连接数、上下行字节数、请求数、拉起次数、启动总耗时、运行秒数。保留 1 年。
- 清理任务定期运行。

### Web API（JSON）

- 使用 gin 框架实现。

- 项目的增删改查，路由作为项目的子资源。写操作返回校验或监听错误。
- 对项目执行启动或停止操作。
- 查看项目日志：`GET /api/projects/:name/logs?lines=N` 返回最近 N 行纯文本（上限 2000）。
- 状态快照：每个项目的状态、活跃连接数、运行时长、空闲倒计时、最近一次错误。
- 事件查询：支持按项目、类型、时间范围筛选，并支持分页。
- 统计查询：汇总指标，以及按小时的时序数据。
- YAML 导出（下载）和导入（上传）。
- SSE 流：推送状态变化和新事件。
- 默认监听 `127.0.0.1:9000`，可用参数修改。不做鉴权。

### 前端

- 单个 HTML 页面，使用 Alpine.js 实现交互、uPlot 绘制图表，所有资源随二进制一起嵌入，不依赖 CDN。
- 手写 CSS 变量，深浅色跟随系统，风格简洁、适度美观。
- 页面区域：项目总览（实时状态）、项目编辑表单、事件日志、统计页、导入导出。

### CLI

- 根命令启动服务。参数：`--db`（默认 `./portwake.db`）、`--web`（默认 `127.0.0.1:9000`）、`--config <yaml>`（文件模式）、`--import <yaml>`（仅 db 模式）、`--state-dir`（exec 的 pid/日志目录，默认 `<db>.state`）、`--tui`、`--grace`（默认 10 秒）、`--version`。
- 启动时不强制要求 `docker` 存在：只有使用 compose/docker 类型时才需要，纯 exec 场景无需安装 docker。
- 未开启 TUI 时，日志用 charm log 输出到 stderr；开启 TUI 时，日志只进入 TUI 的事件面板。

### TUI

- 上方是项目表：状态、端口、活跃连接数、运行时长、空闲倒计时。
- 下方是事件日志滚动区。
- 快捷键：`s` 启动、`x` 停止、`tab` 切换焦点、`q` 退出。退出 TUI 就等同于退出 portwake，走优雅退出流程。

## Testing Decisions

- **好的测试只验证外部行为**：通过真实的入口端口连接、HTTP 请求和 Web API 观察结果，不断言内部字段、goroutine 或调用顺序等实现细节。
- **唯一的接缝是 `runner.Manager` / `Runner`**。测试里用伪造实现（`composetest.Fake` 同时充当 compose 和 docker runner）：执行 `Up` 时在本地随机端口上起真实的 TCP/HTTP echo 服务，代替“容器”；执行 `Stop` 时关闭这些服务；`Ps` 返回模拟的运行和健康状态。它可以设置启动延迟、启动失败、健康状态变化、日志内容，并能统计 Up/Stop 的调用次数。
- **exec runner 用真实的子进程测试**：重新执行测试二进制作为辅助进程，验证启动、转发、空闲停止、再次启动、进程组清理、升级到 `SIGKILL`、环境变量优先级，以及跨 runner 实例的进程接管。只有这些用例使用真实进程，其余测试都走伪造 runner。
- **从最外层驱动**：测试通过 app 模块构造整个应用（使用临时 SQLite 文件、模拟 runner、随机的 Web 端口和入口端口），然后只通过这两类外部入口操作：
  1. 向入口端口发起 TCP 连接或 HTTP 请求：验证按需拉起、并发连接只 Up 一次、转发正确、启动超时时断开连接或返回 502、WebSocket 透传。
  2. 调用 Web JSON API 和 SSE：验证配置的增删改、热更新、端口冲突时回滚、手动启停、状态快照、事件和统计、YAML 导入导出。
- **不抽象时钟**：`idle_timeout`、`start_timeout`、核对间隔、`grace` 都支持毫秒级配置，测试里直接用短时长，验证空闲停止、长连接不被停、stopping 期间来连接会重新拉起、外部状态核对等行为。
- **SQLite 用真实的**，不做 mock。
- **集成测试**（加 build tag，默认不跑）：用真实的 CLI runner 加一个 nginx compose 项目，走一遍“访问 → 拉起 → 转发 → 空闲 → stop”的完整流程。
- **不做自动化测试的部分**：TUI 渲染和 HTML 页面。它们只消费已经被覆盖的事件和状态 API。
- **已有的测试先例**：没有，这是新项目。

## Out of Scope

- 启动期间的 HTTP 等待页。
- 按 Host 或 Path 的七层路由，以及 TLS 终止。
- Web 页面的鉴权，以及多用户支持。
- 根据 `service` 和 `containerPort` 自动发现上游端口。
- 按 cron 计划停止项目，以及用 `down` 代替 `stop`。
- 基于“无字节流量”的空闲判断，以及强制断开长连接。
- 只按单个 service 启停。
- 在 TUI 里查看项目日志（Web 项目卡片支持）。
- exec 二进制自行 daemonize 或脱离进程组后的跟踪，以及 PTY/tty 交互。
- 常驻和服务化（launchd/systemd、`install` 命令），以及 `--docker` 路径参数。
- 退出时停止所有项目。
- 集群和多主机。

## Further Notes

- 后端必须自己监听/发布端口，而且不能和 portwake 的入口端口冲突：compose 服务要把端口发布到宿主机（建议绑定到 `127.0.0.1`），docker 类型用 `ports` 发布，exec 由二进制自己监听。README 里给出了示例。
- 由于配置里的项目全部被接管，所以手动启动的项目（包括手动跑起来的 exec 进程）也会在空闲后被停掉。不想被停的项目把 `idle_timeout` 设为 0。
- macOS 下用的是 Docker Desktop 或兼容的运行时，`docker compose` v2 必须可用；只用 exec 时不需要 docker。
- 仓库提供 `Dockerfile` 和 `docker-compose.yaml`；容器内运行 compose/docker 类型同样需要宿主机的 docker CLI 和 socket，而 exec 类型启动的进程会跑在 portwake 容器内，通常不如直接在宿主机上运行 portwake。
- 推送 `v*` tag 时，GitHub Actions 会交叉编译 Linux / macOS 的 amd64 / arm64 二进制并创建 Release，附带校验和文件。
- 后续可以考虑的功能：启动等待页、按 Host 路由、按 cron 停止、TUI 日志面板。
