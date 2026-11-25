# kore-plugin-terminal — Web 终端插件

## 它做什么

这是 kore 的 Web 终端插件（id `terminal`）。每个会话是一个**真实 PTY + 登录 shell**，
不是命令执行包装器：写入即键盘输入，读取即终端输出（含转义序列），前端用 xterm.js 之类的
终端模拟器对接即可。0.2.0 起补齐三件事：**方法级 capability**、**按会话持久化的命令历史**、
**断线重连**（读游标可恢复）。字号、全屏、主题、分屏等纯前端呈现项不在本仓范围。

- 协议：`kore.plugin.v1`，`lifecycle=resident`（终端必须常驻），深度等级 `production`。
- 能力：`system:read` + `system:admin` + `proc:write`，并声明方法级 `method_capabilities`。
- 资源预算：12 MiB 内存 / 25% CPU / 64 pids。

## PTY 实现（不引第三方库）

`internal/terminal/pty_linux.go` 只用标准库，按 Linux 固定流程打开伪终端：打开 `/dev/ptmx`、
解锁从端（TIOCSPTLCK）、取从端编号（TIOCGPTN）并打开 `/dev/pts/<n>`、
用 `Setsid` + `Setctty` 拉起登录 shell（进程组号等于 pid，close 才能 `kill(-pgid)` 清整棵树）、
`TIOCSWINSZ` 调整窗口大小。master fd 设非阻塞，读写用 `select` 避免一次粘贴卡死循环。
平台范围：`linux/amd64`、`linux/arm64`、`linux/riscv64`、`linux/loong64` 编译，其它平台显式返回 `E_UNSUPPORTED`。

## 协议方法（kore.plugin.v1）

| 方法 | 入参 | 说明 |
| --- | --- | --- |
| `kore.ping` | `{}` | 存活与运行期指标 |
| `kore.shutdown` | `{}` | 落盘历史、关闭全部会话后优雅退出 |
| `terminal.create` | `{client_id?,cols?,rows?,shell?}` | 缺省 80×24、`/bin/bash`、`default` |
| `terminal.write` | `{session_id,client_id?,data}` | `data` 为 base64；输入记入历史 |
| `terminal.resize` | `{session_id,client_id?,cols,rows}` | `TIOCSWINSZ` 生效后回显 |
| `terminal.read` | `{session_id,client_id?,cursor?,max_bytes?}` | 增量读；带 `cursor` 从游标续读 |
| `terminal.attach` | `{session_id,client_id?,cursor?}` | 重连存活会话，校验游标可恢复 |
| `terminal.close` | `{session_id,client_id?}` | `kill(-pgid)` + 关 fd + 移出 map |
| `terminal.list` | `{client_id?}` | 不带为管理视图（全部），带则按归属过滤 |
| `terminal.history` | `{session_id?,client_id?,limit?}` | 命令历史查询（最新在前） |
| `terminal.history.clear` | `{session_id?,client_id?}` | 清理该调用方历史 |

**数据编码**：`terminal.write.data`、`terminal.read.data`、`terminal.history.entries[].data`
统一用标准 base64（带 `=` 填充）——终端输出可能含任意二进制与跨帧截断的多字节 UTF-8，
直接放进 JSON 会被静默替换成 U+FFFD；前端解码一次即可交给 `term.write(Uint8Array)`。
非法 base64 回 `E_INVALID`。

## 会话归属与越权拒绝

`terminal.create` 可带 `client_id`（字母/数字/`-_.:@`，≤64 字节，缺省 `default`），
会话创建后归属不可变。`write/read/resize/close/attach/history` 必须携带同一 `client_id`，
否则回 `E_UNAUTHORIZED`，且不触碰归属方会话。**信任模型**：`client_id` 是插件内逻辑归属标签、
不是认证凭据；真正的认证边界在内核的 `method_capabilities` 授权。

## 断线重连与读游标

每个会话维护单调递增的绝对字节游标 `offset`（已交付字节数），只由 `read` 推进。
`cursor == offset` 为增量读；`cursor < offset` 且落在补读窗口内则回放 `[cursor, offset)` 的字节
（`replay=true`）；早于窗口下界回 `E_CONFLICT`，超前回 `E_INVALID`。补读窗口
`KORE_TERMINAL_REPLAY_BYTES` 默认 64 KiB、上限 4 MiB，只保留最近交付的字节；同一游标重复读
幂等。会话存活至显式关闭 / shell 退出 / 空闲回收（默认 1800s，惰性调用时驱动、无 ticker），
回收后不可恢复，`attach` 回 `E_NOTFOUND`。

## 命令历史

只记录 `terminal.write` 的输入，按会话分组，存到 `<KORE_STATE_DIR>/history/session-<id>.json`
（目录 0700、文件 0600、原子写）。每会话条数（默认 200）、单条字节（默认 4096）、保留会话数
（默认 50）均可配，超出淘汰最旧、单条超出截断并标记 `truncated`。落盘是 best-effort：
失败不拒绝终端输入，错误经 `terminal.history.storage_error` 如实返回。`KORE_TERMINAL_HISTORY=off`
或 `KORE_STATE_DIR` 缺失时停用并 WARN。**隐私**：历史可能含用户输入的口令，插件从不把输入写日志，
但仍建议不要在终端里直接粘贴明文口令。

## 安全与资源

会话上限默认 4（`KORE_TERMINAL_MAX_SESSIONS`，1–16，超出 `E_RATELIMIT`）；单次读/写 1 MiB，
每个请求 5s 预算；`shell` 必须是存在的可执行文件绝对路径；子 shell 环境剔除 `KORE_PLUGIN_TOKEN`，
避免协议凭据泄漏进终端。

环境变量：`KORE_TERMINAL_MAX_SESSIONS`(4)、`KORE_TERMINAL_IDLE_SEC`(1800)、
`KORE_TERMINAL_SHELL`(`/bin/bash`)、`KORE_TERMINAL_REPLAY_BYTES`(65536)、
`KORE_TERMINAL_HISTORY`(on)、`KORE_TERMINAL_HISTORY_MAX`(200)、
`KORE_TERMINAL_HISTORY_ENTRY_BYTES`(4096)、`KORE_TERMINAL_HISTORY_SESSIONS`(50)。

## 输出通道：轮询而非 event

结论是轮询 `terminal.read`（增量读）+ `attach`/`cursor` 恢复，不用 event 推送：协议侧未登记
`terminal.output` 事件、内核也未接入插件事件到 SSE。前端按 30–80ms 轮询或输入后立即读即可。

## 安装与构建

独立 Go 模块，只依赖 `kore/pkg/pluginapi` 与标准库，不引入第三方依赖、不提交 `go.work`。
构建基线 `CGO_ENABLED=0 GOWORK=off GOAMD64=v1 -trimpath -buildvcs=false -ldflags="-s -w"`。
本地验证：`gofmt -l .`、`GOWORK=off go vet ./...`、`GOWORK=off go test -count=1 -timeout 200s ./...`、
`make dist`（位级可复现打包）。发布产物 `dist/terminal-<版本>.tar.gz` + `.sha256`。
漏洞不要开公开 issue，按 [SECURITY.md](https://github.com/kore-pl/kore-plugin-terminal/blob/main/SECURITY.md) 私下披露。
