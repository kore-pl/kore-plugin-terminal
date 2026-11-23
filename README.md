# kore-plugin-terminal

kore 的 Web 终端插件（生产级）：每个会话是一个**真实 PTY + 登录 shell**，不是命令
执行包装器。写入即键盘输入，读取即终端输出（含转义序列），前端直接用 xterm.js 之类的
终端模拟器对接即可。

0.2.0 起补齐三件事：**方法级 capability**（§3.5 #12）、**命令历史**（按会话持久化到
插件状态目录）、**断线重连**（读游标可恢复 + attach）。字号 / 全屏等纯前端呈现项不在
本仓范围（见文末「范围边界」）。

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
| `kore.shutdown` | `{}` | `{ok:true}` | 落盘历史、关闭全部会话后优雅退出 |
| `kore.reload` | `{}` | `{ok:true,config_digest:""}` | 会话不中断，空操作 |
| `terminal.create` | `{client_id?,cols?,rows?,shell?}` | `{session_id,client_id,shell,pid,cols,rows}` | 缺省 80×24、`/bin/bash`、`default` |
| `terminal.write` | `{session_id,client_id?,data}` | `{written}` | `data` 为 **base64**；输入记入历史 |
| `terminal.resize` | `{session_id,client_id?,cols,rows}` | `{cols,rows}` | `TIOCSWINSZ` 生效后回显 |
| `terminal.read` | `{session_id,client_id?,cursor?,max_bytes?}` | `{data,bytes,cursor,replay,eof}` | 增量读；带 `cursor` 则从游标续读 |
| `terminal.attach` | `{session_id,client_id?,cursor?}` | `{session_id,client_id,attached,cursor,replay_bytes,eof,shell,pid,cols,rows,created_at,last_active,idle_expires_at}` | 重连存活会话，校验游标可恢复 |
| `terminal.close` | `{session_id,client_id?}` | `{closed:true}` | `kill(-pgid)` + 关 fd + 移出 map |
| `terminal.list` | `{client_id?}` | `{sessions,count,max_sessions}` | 不带 `client_id` 为管理视图（全部会话），带则按归属过滤 |
| `terminal.history` | `{session_id?,client_id?,limit?}` | `{enabled,client_id,session_id,entries,count,limit,max_entries,storage_error?}` | 命令历史查询（最新在前，`data` 为 base64） |
| `terminal.history.clear` | `{session_id?,client_id?}` | `{cleared_sessions,cleared_entries}` | 清理该调用方历史（单会话或全部） |

### 数据编码：base64（唯一口径，README 即契约）

`terminal.write.data`、`terminal.read.data` 与 `terminal.history.entries[].data` 统一用
**标准 base64（带 `=` 填充）**：

- 终端输出可能含任意二进制与**被增量读截断的多字节 UTF-8 序列**；直接放进 JSON 字符串
  会被 `json.Marshal` 静默替换成 U+FFFD，造成数据损坏。
- 前端 `xterm.js` 本来就需要字节流（`term.write(Uint8Array)`），base64 解码一次即可。
- 非法 base64 回 `E_INVALID`；`bytes` 是解码后的字节数，可用来自检。

## 方法级 capability（§3.5 #12）

`info.json` 声明 `method_capabilities`，内核按方法取用（授权 + 审计粒度），
只读方法不再进写审计：

| 方法 | capability |
| --- | --- |
| `terminal.list` / `terminal.read` / `terminal.attach` / `terminal.history` | `system:read` |
| `terminal.create` / `terminal.write` / `terminal.resize` / `terminal.close` / `terminal.history.clear` | `system:admin` |

声明与实现的唯一真源是 `methods.go` 的 `terminalVerbs`；`capability_test.go` 做表驱动
一致性门禁（覆盖、取值、内核同口径校验、dispatch 可识别）。未声明的方法回退插件级
`capabilities`（`system:read` + `system:admin` + `proc:write`）。

## 会话归属与越权拒绝

- `terminal.create` 可带 `client_id`（字母/数字/`-_.:@`，≤64 字节，缺省 `default`）；
  **会话创建后归属不可变**。`write` / `read` / `resize` / `close` / `attach` / `history`
  必须携带同一 `client_id`，否则回 `E_UNAUTHORIZED`：
  - 归属校验落点：`internal/terminal/session.go`（`Session.checkOwner`）+
    `internal/terminal/manager.go`（`Manager.session` / `Manager.Close`）；
  - 越权失败**不影响归属方**：不触碰活跃时间、不关闭会话、不返回会话内容。
- **信任模型（如实披露）**：`client_id` 是插件内的**逻辑归属标签，不是认证凭据**。
  协议面（`pkg/pluginapi`）没有把调用方身份转发给插件，插件也无法区分同一条内核连接
  上的不同调用方；真正的认证边界是内核的 `method_capabilities` 授权（当前单管理员）。
  需要多用户强隔离时应由内核转发身份（协议演进）或按用户拆插件实例。
- `terminal.list` 不带 `client_id` 仍返回全部会话（保持 0.1.x 的管理视图语义）；
  带 `client_id` 时只返回该调用方创建的会话。

## 断线重连与读游标

### 会话存活语义（明示）

- 会话创建后**一直存活**，直到：显式 `terminal.close`、shell 自行退出、插件退出，或
  **空闲回收**。
- 空闲回收阈值 `KORE_TERMINAL_IDLE_SEC`（默认 1800s）。"最近活跃"由
  `create / read / write / resize / attach` 刷新；超过阈值即被回收。
- **回收是惰性/调用时驱动**（G-19，无 ticker）：`create / read / write / resize /
  close / attach / list` 每次调用顺带摘除超时会话，并在锁外 `kill(-pgid)`。
- **回收后不可恢复**：进程树已杀、fd 已关，`attach` / `read` 回 `E_NOTFOUND`，
  必须重新 `create`。`attach` 返回的 `idle_expires_at` 是当前阈值下的回收时刻，
  客户端可据此提前续期（一次 `attach` 即刷新活跃时间）。

### 读游标

- 每个会话维护**单调递增的绝对字节游标** `offset`（已交付字节数），只由 `read` 推进；
  `read` / `attach` 结果都返回 `cursor`，客户端保存"下次期望位置"。
- 带 `cursor` 的 `read` 语义：
  - `cursor == offset`：增量读新输出（与不带游标一致，旧行为）；
  - `cursor < offset` 且落在**补读窗口**内：回放 `[cursor, offset)` 的字节
    （`replay=true`），用于"服务端已读出但响应在断线中丢失"的续读；
  - `cursor < 窗口下界`：`E_CONFLICT`（`ErrCursorExpired`，游标已过期，需重新对齐）；
  - `cursor > offset`：`E_INVALID`（游标超前，状态不一致）。
- 补读窗口 `KORE_TERMINAL_REPLAY_BYTES`（默认 64 KiB，上限 4 MiB）只保留**最近交付**
  的字节；未交付的输出仍在 PTY 内核缓冲里，会话存活期内不会丢。
- 同一游标重复读返回同一段字节（幂等），**不会重复吐更早的已读内容**，也不会推进游标。
- `terminal.attach` 校验归属与游标可恢复性后返回快照；`replay_bytes` 告诉客户端
  当前可补读多少字节。窗口外的游标直接 `E_CONFLICT`，不静默截断。

## 命令历史

- **只记录 `terminal.write` 的输入内容**（实际写入的字节，含部分写入的前缀），
  **按会话分组**，与 `create / read / resize / close` 无关。
- 存储契约：
  - 目录：`<KORE_STATE_DIR>/history/`（插件状态目录由内核注入，如
    `/var/lib/kore/plugins/terminal/history`），目录 **0700**；
  - 文件：`session-<session_id>.json`（**0600**，原子写：同目录临时文件 → fsync →
    rename），包含 schema 版本、会话 id、创建者、时间与条目；
  - 条目：`seq`（会话内单调递增）、`at`、`data`（base64）、`original_bytes`、
    `truncated`。
- 上限与清理（全部可配）：
  - 每会话条数上限 `KORE_TERMINAL_HISTORY_MAX`（默认 200，上限 1000），超出淘汰最旧；
  - 单条字节上限 `KORE_TERMINAL_HISTORY_ENTRY_BYTES`（默认 4096，上限 64 KiB），
    超出截断并置 `truncated=true`；
  - 保留会话数上限 `KORE_TERMINAL_HISTORY_SESSIONS`（默认 50，上限 500），超出按
    mtime 淘汰最旧的历史文件；
  - `terminal.history.clear` 显式清理（按会话或该调用方全部）；
  - `KORE_TERMINAL_HISTORY=off` 关闭历史；`KORE_STATE_DIR` 缺失时同样停用并在启动
    日志给出 WARN（不静默假装成功，查询结果 `enabled=false`）。
- 写入节奏：同一会话两次落盘之间至少间隔 250ms（合并连续击键）；`close` /
  会话回收 / `kore.shutdown` 会强制落盘。历史落盘是 **best-effort**：失败不会拒绝
  终端输入，错误通过 `terminal.history.storage_error` 如实返回。
- **隐私**：历史里可能含用户输入的口令。插件从不把输入写日志；历史文件 0600、
  目录 0700；但仍建议不要在终端里直接粘贴明文口令。

## 安全与资源

- **会话上限**：默认 4（`KORE_TERMINAL_MAX_SESSIONS`，1–16），超出回 `E_RATELIMIT`；
  超限检查在读锁内完成，并发 create 不会突破上限。
- **空闲回收**：默认 1800s（`KORE_TERMINAL_IDLE_SEC`，1–86400），惰性触发，见上文。
- **越权拒绝**：见「会话归属与越权拒绝」。
- **进程树清理**：`terminal.close` 先 `kill(-pgid)`（Setsid 后 pgid = pid），3s 未退再补
  一刀，最后关 master fd；插件收到 `kore.shutdown` / 连接断开时同样关闭全部会话。
- **不记录输入到日志**：日志只记会话 id 与字节数；历史是显式的、可清理的状态文件。
- **token 隔离**：子 shell 的环境变量里剔除 `KORE_PLUGIN_TOKEN`，避免协议凭据泄漏进终端。
- **容量边界**：单次读 1 MiB（`max_bytes` 缺省 64 KiB）、单次写 1 MiB、窗口 1..1000、
  每个请求 5s 预算；超限回 `E_INVALID`。
- **shell 校验**：`shell` 必须是存在的可执行文件绝对路径；`bash/sh/dash/zsh/ksh/mksh/fish`
  以 `-l` 登录 shell 启动，其它二进制不加参数。

### 环境变量

| 变量 | 默认 | 范围 | 说明 |
| --- | --- | --- | --- |
| `KORE_TERMINAL_MAX_SESSIONS` | 4 | 1–16 | 同时存活会话上限 |
| `KORE_TERMINAL_IDLE_SEC` | 1800 | 1–86400 | 空闲回收阈值（秒） |
| `KORE_TERMINAL_SHELL` | `/bin/bash` | 绝对路径 | create 未指定 shell 时的默认值 |
| `KORE_TERMINAL_REPLAY_BYTES` | 65536 | ≤4 MiB | 重连补读窗口字节数 |
| `KORE_TERMINAL_HISTORY` | on | on/off | 命令历史开关 |
| `KORE_TERMINAL_HISTORY_MAX` | 200 | ≤1000 | 每会话历史条数 |
| `KORE_TERMINAL_HISTORY_ENTRY_BYTES` | 4096 | ≤64 KiB | 单条历史字节上限 |
| `KORE_TERMINAL_HISTORY_SESSIONS` | 50 | ≤500 | 保留历史文件的会话数 |

## event 还是轮询？

**结论：轮询 `terminal.read`（增量读）+ `attach`/`cursor` 恢复，不用 event 推送。** 依据：

- `pkg/pluginapi/envelope.go` 有 `TypeEvent` 帧，但 `docs/PLUGIN-PROTOCOL.md` §8
  规定事件名必须已登记；自定义 `terminal.output` 属于未登记事件，协议侧不收。
- 内核实现侧也未接入：`internal/core/bus/call.go` 对插件事件的处置是
  `"插件事件暂未接入 SSE，丢弃"`，HTTP 面 `GET /api/v1/events` 目前是 501。

因此输出通道是 `terminal.read`：调用即返回**自上次读取以来新增的输出**（带游标则从
游标续读），没有数据返回空串（不是错误、不阻塞），`eof=true` 表示 shell 已退出且输出
已读完。前端按 30–80ms 轮询或输入后立即读即可获得交互级体验。

## 生命周期

`lifecycle=resident`：终端必须常驻，不能像 lazy 插件那样在会话空闲时被回收——否则
用户正开着的 shell 会被宿主杀掉。`resources` 按 12 MiB / 25% CPU / 64 pids 预算。

## 范围边界

- **字号 / 全屏 / 主题 / 分屏** 属纯前端呈现项：由主壳前端（或后续 `web/`）负责，
  不进入本插件仓——本插件只保证 PTY 字节流、窗口尺寸（`resize` → `TIOCSWINSZ`）与
  会话生命周期语义。
- **多标签**：由前端为每个标签开一个 `session_id` 表达，后端不需要额外方法。
- `terminal.list` 的 `max_sessions` 只回显上限值，不提供修改能力（改上限走环境变量）。

## 测试（全真跑）

```sh
gofmt -l .
GOWORK=off go vet ./...
GOWORK=off go test -count=1 -timeout 200s ./...
make dist
```

- `internal/terminal/*_test.go`：真实 PTY——create 后 write 读到执行结果、
  `stty size` 证明 resize 生效、close 后 pid 消失、会话上限、空闲回收、EOF、非法参数；
   另含**历史往返**（写入/查询/条数上限/单条截断/清理/归属隔离/0600 权限/重启恢复）、
  **重连游标**（回放不重复不丢、游标过期、游标超前、超时回收后 attach 失败）、
  **越权拒绝**与 `client_id` 校验。
- `tests/fault/`：四类故障注入（kill / timeout / disk / cgroup），验证方法级错误返回
  与"清理路径永不被阻断"。
- `tests/e2e/`：真实插件二进制 + 假内核（UDS 上回 `kore.hello`），跑 create → write →
  增量 read → resize → `pgrep` 证明进程树 → close；另含 attach 续读、跨调用方越权、
  命令历史往返与超时回收后 attach 失败。日志里输出 `E2E_*` 证据行。
- `capability_test.go`：`method_capabilities` 声明 vs 实现一致性（表驱动）。

## 构建与打包

`Makefile` 的 `dist` 目标显式列出 `info.json`、`bin/terminal`、`SHA256SUMS`，用
`--sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner --mode='u+rwX,go+rX'`
+ `gzip -9n` 保证位级可复现；`info.json.size_bytes` 按真实 `.tar.gz` 字节数迭代回填
并与包内值收敛校验。构建基线：
`CGO_ENABLED=0 GOWORK=off GOAMD64=v1 -trimpath -buildvcs=false -ldflags="-s -w"`。

MIT License — pengtang1024

<!-- oss:begin -->
## 开源与贡献

[![CI](https://github.com/kore-pl/kore-plugin-terminal/actions/workflows/ci.yml/badge.svg)](https://github.com/kore-pl/kore-plugin-terminal/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)

本仓库是 **kore** 的 `Web 终端插件`（id `terminal`），独立仓库、独立版本线、独立 CI（少触发：push main / tag / PR / 手动）。内核与协议见 [kore](https://github.com/kore-pl/kore)。

- **许可**：MIT（inbound = outbound，不要求 CLA）；提交用 `git commit -s` 签 DCO。
- **贡献**：[CONTRIBUTING.md](CONTRIBUTING.md)（构建、门禁、发版、边界）。
- **安全**：[SECURITY.md](SECURITY.md)——漏洞不要开公开 issue。
- **行为准则**：[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)。
- **兼容性**：`protocol: kore.plugin.v1` · `requires_kore: >=0.1.0 <0.2.0` · `lifecycle: resident` · 深度等级 `production`。
- **验证方式**：本地 `make check` 优先（CI 只在 push main / tag / PR / 手动跑；发布前 `make dist` 连跑两次核对 sha 一致）。
- **发布产物**：`make dist` → `dist/terminal-0.2.0.tar.gz` + `.sha256`（位级可复现，连跑两次 sha 相同）。
- **相关仓库**：[索引 kore-plugins](https://github.com/kore-pl/kore-plugins) · [协议文档](https://github.com/kore-pl/kore/blob/main/docs/PLUGIN-PROTOCOL.md) · [SDK 指南](https://github.com/kore-pl/kore/blob/main/docs/PLUGIN-SDK.md)

<!-- oss:end -->
