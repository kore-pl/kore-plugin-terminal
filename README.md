# kore-plugin-terminal

kore 的 Web 终端插件（生产级）：每个会话是一个**真实 PTY + 登录 shell**，不是命令
执行包装器。写入即键盘输入，读取即终端输出（含转义序列），前端直接用 xterm.js 之类的
终端模拟器对接即可。

## PTY 实现（不引第三方库）

`internal/terminal/pty_linux.go` 只用标准库，按 Linux 固定流程打开伪终端：

1. `os.OpenFile("/dev/ptmx", os.O_RDWR|O_NOCTTY|O_CLOEXEC, 0)` 打开主端；
2. `ioctl(fd, TIOCSPTLCK, &0)` 解锁从端；
3. `ioctl(fd, TIOCGPTN, &n)` 取从端编号，打开 `/dev/pts/<n>`；
4. `exec.Command(shell, "-l")` 把 stdin/stdout/stderr 全指向从端；
   `SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}` 让 shell 成为会话首进程，
   从而 **进程组号等于 pid**（close 才能 `kill(-pgid)` 清掉整棵子进程树）；
5. `TIOCSWINSZ` 调整窗口大小；master fd 设为非阻塞，读用 `select` 零超时探活、
   写用 `select` 等待可写，避免一次粘贴把整个会话循环卡死。

**平台范围**：PTY 实现在 `linux/amd64`、`linux/arm64`、`linux/riscv64`、`linux/loong64`
编译；其它平台按显式降级返回 `E_UNSUPPORTED`（不静默降级、不伪造成功）。
`internal/core` 的 G-16 禁止 `syscall`，本插件仓不受该条约束——这是插件能干活的前提。

## 协议方法（kore.plugin.v1）

| 方法 | 入参 | 结果 | 说明 |
| --- | --- | --- | --- |
| `kore.ping` | `{}` | `{status,uptime_s,goroutines,rss_kb}` | 存活与运行期指标 |
| `kore.shutdown` | `{}` | `{ok:true}` | 关闭全部会话后优雅退出 |
| `kore.reload` | `{}` | `{ok:true,config_digest:""}` | 会话不中断，空操作 |
| `terminal.create` | `{cols?,rows?,shell?}` | `{session_id,shell,pid,cols,rows}` | 缺省 80×24、`/bin/bash` |
| `terminal.write` | `{session_id,data}` | `{written}` | `data` 为 **base64** |
| `terminal.resize` | `{session_id,cols,rows}` | `{cols,rows}` | `TIOCSWINSZ` 生效后回显 |
| `terminal.read` | `{session_id,max_bytes?}` | `{data,bytes,eof}` | `data` 为 **base64**，增量读 |
| `terminal.close` | `{session_id}` | `{closed:true}` | `kill(-pgid)` + 关 fd + 移出 map |
| `terminal.list` | `{}` | `{sessions,count,max_sessions}` | 当前会话快照 |

### 数据编码：base64（唯一口径，README 即契约）

`terminal.write.data` 与 `terminal.read.data` 统一用**标准 base64（带 `=` 填充）**：

- 终端输出可能含任意二进制与**被增量读截断的多字节 UTF-8 序列**；直接放进 JSON 字符串
  会被 `json.Marshal` 静默替换成 U+FFFD，造成数据损坏。
- 前端 `xterm.js` 本来就需要字节流（`term.write(Uint8Array)`），base64 解码一次即可。
- 非法 base64 回 `E_INVALID`；`bytes` 是解码后的字节数，可用来自检。

## event 还是轮询？

**结论：轮询 `terminal.read`（增量读），不用 event 推送。** 依据（不是猜的）：

- `pkg/pluginapi/envelope.go` 确实有 `TypeEvent` 帧，但 `docs/PLUGIN-PROTOCOL.md` §8
  规定事件名必须已登记（`plugin.log` / `plugin.health` / `plugin.state` / `system.event`），
  自定义 `terminal.output` 属于未登记事件，协议侧不收。
- 内核实现侧也未接入：`internal/core/bus/call.go` 对插件事件的处置是
  `"插件事件暂未接入 SSE，丢弃"`，HTTP 面 `GET /api/v1/events` 目前是 501
  `E_UNSUPPORTED`（`internal/core/httpapi/handlers.go`）。

因此当前唯一可用的输出通道就是 `terminal.read`：调用即返回**自上次读取以来新增的输出**，
没有数据返回空串（不是错误、不阻塞），`eof=true` 表示 shell 已退出且输出已读完。
前端按 30–80ms 轮询或输入后立即读即可获得交互级体验。等协议开放自定义事件并接通 SSE 后，
可以在不改方法契约的前提下追加 `event:terminal.output`（事件只增不改）。

## 安全与资源

- **会话上限**：默认 4（`KORE_TERMINAL_MAX_SESSIONS`，1–16），超出回 `E_RATELIMIT`。
- **空闲回收**：默认 1800s（`KORE_TERMINAL_IDLE_SEC`，1–86400）。**没有 ticker**
  （G-19）——`terminal.create` / `terminal.write` / `terminal.read` 每次顺带清理超时会话。
- **进程树清理**：`terminal.close` 先 `kill(-pgid)`（Setsid 后 pgid = pid），3s 未退再补
  一刀，最后关 master fd；插件收到 `kore.shutdown` / 连接断开时同样关闭全部会话。
- **不记录输入**：任何日志都不包含 `data`（可能含密码），只记会话 id 与字节数。
- **token 隔离**：子 shell 的环境变量里剔除 `KORE_PLUGIN_TOKEN`，避免协议凭据泄漏进终端。
- **容量边界**：单次读 1 MiB（`max_bytes` 缺省 64 KiB）、单次写 1 MiB、窗口 1..1000、
  每个请求 5s 预算；超限回 `E_INVALID`。
- **shell 校验**：`shell` 必须是存在的可执行文件绝对路径；`bash/sh/dash/zsh/ksh/mksh/fish`
  以 `-l` 登录 shell 启动，其它二进制不加参数。
- **能力声明**：`system:admin` + `proc:write`（真实起进程、写 PTY）。

## 生命周期

`lifecycle=resident`：终端必须常驻，不能像 lazy 插件那样在会话空闲时被回收——否则
用户正开着的 shell 会被宿主杀掉。`resources` 按 12 MiB / 25% CPU / 64 pids 预算。

## 测试（全真跑）

```sh
gofmt -l .
go vet ./...
go test -timeout 200s ./...
make build
make dist
```

- `internal/terminal/*_test.go`：真实 PTY——create 后 write `echo hello` 读到 `hello`、
  `printf` 标记证明真的执行、`stty size` 证明 resize 生效、close 后 pid 消失、
  会话上限、空闲回收、EOF、未知会话、非法参数。
- `tests/fault/`：四类故障注入（kill / timeout / disk / cgroup），验证方法级错误返回
  （`ErrClosed` / `ErrTimeout` / `ErrIO` / `ErrResource`）与"清理路径永不被阻断"。
- `tests/e2e/`：真实插件二进制 + 假内核（UDS 上回 `kore.hello`），跑
  create → write → 增量 read（`kore-terminal-ok`）→ resize（`stty size` = `40 120`）→
  `exec -a` 标记进程 → `pgrep -f` 证明存在 → close → `pgrep` 清空 + pid 消失 →
  会话上限 → shutdown。日志里输出 `E2E_*` 证据行。

## 构建与打包

`Makefile` 的 `dist` 目标显式列出 `info.json`、`bin/terminal`、`SHA256SUMS`，用
`--sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner` + `gzip -9n` 保证确定性；
`info.json.size_bytes` 按真实 `.tar.gz` 字节数迭代回填。构建基线：
`CGO_ENABLED=0 GOAMD64=v1 -trimpath -buildvcs=false -ldflags="-s -w"`。

MIT License — pengtang1024
