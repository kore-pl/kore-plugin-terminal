# Changelog

## 0.2.0 (2026-11-23)

- feat(capability): `info.json` 声明 **方法级 `method_capabilities`**（§3.5 #12）——`list` / `read` / `attach` / `history` → `system:read`，`create` / `write` / `resize` / `close` / `history.clear` → `system:admin`；只读方法不再进写审计，插件级 capabilities 同步补 `system:read`
- feat(history): 新增 `terminal.history` / `terminal.history.clear`，把 `terminal.write` 的输入按会话持久化到 `<KORE_STATE_DIR>/history/session-<id>.json`（目录 0700、文件 0600、临时文件 + fsync + rename 原子写）；每会话条数、单条字节、保留会话数均可配（默认 200 / 4 KiB / 50），超出淘汰最旧，单条超出截断并标记；连续击键 250ms 合并落盘，close/回收/shutdown 强制落盘；历史落盘 best-effort（失败不拒绝终端输入，错误经 `storage_error` 如实返回），`KORE_TERMINAL_HISTORY=off` 或 `KORE_STATE_DIR` 缺失时停用并 WARN
- feat(reconnect): 新增 `terminal.attach` 与 `terminal.read.cursor`——会话维护**单调递增的绝对字节游标**，`read` 结果返回 `cursor`；断线重连时可从旧游标回放补读窗口内未确认的字节（`replay=true`），同一游标重复读幂等、不重复吐已读内容；游标早于窗口下界回 `E_CONFLICT`、超前回 `E_INVALID`；补读窗口 `KORE_TERMINAL_REPLAY_BYTES` 可配（默认 64 KiB，上限 4 MiB）
- feat(security): 会话**绑定创建者 `client_id`**（缺省 `default`，向后兼容），`write` / `read` / `resize` / `close` / `attach` / `history` 越权一律 `E_UNAUTHORIZED`，且不触碰归属方会话；`terminal.list` 支持按 `client_id` 过滤（不带仍为管理视图）
- docs: 明确会话存活语义（创建后存活至显式关闭 / shell 退出 / 空闲回收，回收为**惰性调用时驱动、无 ticker**；回收后不可恢复，`attach` 回 `E_NOTFOUND`）与 `idle_expires_at`；如实披露 `client_id` 是插件内逻辑归属标签、认证边界在内核 capability
- docs: 字号 / 全屏 / 主题 / 分屏属纯前端呈现项，不在本仓范围（见 README「范围边界」）
- test: 新增 capability 表驱动一致性、历史往返（写入→查询→条数上限→单条截断→清理→归属隔离→0600 权限→重启恢复）、重连游标（断→连→续读不重复不丢、游标过期、游标超前、超时回收后 attach 失败）、越权 attach / 读写 / 关闭、并发上限；e2e 增 attach 续读 / 历史往返 / 超时回收后 attach 失败
- build: 版本 0.1.1 → 0.2.0（新增方法 = minor），`info.json` / `manifest.json` 同步

## 0.1.1 (2026-11-23)

- build: 位级可复现打包（stage chmod + tar --mode 归一、size_bytes 收敛），二进制改嵌 manifest.json，消除同版本 sha 漂移

## 0.1.0 (2026-11-22)

- feat: 新建 terminal 插件骨架，复用 kore.plugin.v1 握手与会话循环
- feat: 标准库 syscall 自实现 PTY（`/dev/ptmx` + `TIOCSPTLCK` + `TIOCGPTN` + `/dev/pts/<n>`），不引 `creack/pty` 等第三方库
- feat: `terminal.create` 以 `Setsid` + `Setctty` 拉起登录 shell，进程组号等于 pid；会话登记到内存 map，默认上限 4 个，超出回 `E_RATELIMIT`
- feat: `terminal.write` / `terminal.read` 走非阻塞 master fd + `select` 零超时探活，读是增量语义（无数据回空串），写有 100ms 可写等待
- feat: `terminal.resize` 用 `TIOCSWINSZ` 改窗口并回读生效值；`terminal.close` 先 `kill(-pgid)` 再关闭 fd，`terminal.list` 给出会话快照
- feat: 空闲回收不依赖 ticker（G-19）——`create` / `write` / `read` 顺带清理超过 `KORE_TERMINAL_IDLE_SEC`（默认 1800s）的会话
- feat: read/write 的 `data` 统一 base64（终端输出可能含非 UTF-8 字节且多字节字符会跨帧截断，JSON 字符串会静默替换为 U+FFFD）
- sec: 子 shell 环境剔除 `KORE_PLUGIN_TOKEN`；任何输入内容不写日志；read 单次上限 1 MiB、write 单次上限 1 MiB；每个请求 5s 预算
- test: 单元测试真开 PTY（create/write/read/resize/close/上限/空闲清理/EOF），四类故障注入（kill/timeout/disk/cgroup），真实插件进程 E2E（UDS 握手 + 增量读 + 进程组清理 + `pgrep` 证明）
- build: 确定性打包（--sort=name --mtime=@0 --owner=0 --numeric-owner + gzip -9n），两次 dist sha256 一致
