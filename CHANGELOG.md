# Changelog

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
