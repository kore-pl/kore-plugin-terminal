# 配置

本插件没有独立的配置文件；行为通过环境变量与调用参数控制。

## 环境变量

| 变量 | 默认 | 范围 | 说明 |
| --- | --- | --- | --- |
| `KORE_TERMINAL_MAX_SESSIONS` | 4 | 1–16 | 同时存活会话上限，超出回 `E_RATELIMIT` |
| `KORE_TERMINAL_IDLE_SEC` | 1800 | 1–86400 | 空闲回收阈值（秒），惰性调用时驱动 |
| `KORE_TERMINAL_SHELL` | `/bin/bash` | 绝对路径 | create 未指定 shell 时的默认值 |
| `KORE_TERMINAL_REPLAY_BYTES` | 65536 | ≤4 MiB | 重连补读窗口字节数 |
| `KORE_TERMINAL_HISTORY` | on | on/off | 命令历史开关 |
| `KORE_TERMINAL_HISTORY_MAX` | 200 | ≤1000 | 每会话历史条数 |
| `KORE_TERMINAL_HISTORY_ENTRY_BYTES` | 4096 | ≤64 KiB | 单条历史字节上限 |
| `KORE_TERMINAL_HISTORY_SESSIONS` | 50 | ≤500 | 保留历史文件的会话数 |

## 状态目录与历史文件

- 历史目录：`<KORE_STATE_DIR>/history/`（0700）；文件：`session-<session_id>.json`（0600，原子写：临时文件 → fsync → rename）。
- `KORE_STATE_DIR` 缺失或 `KORE_TERMINAL_HISTORY=off` 时历史停用并 WARN，查询结果 `enabled=false`，不静默假装成功。
- 历史落盘 best-effort：失败不拒绝终端输入，错误经 `terminal.history.storage_error` 如实返回。

## 容量边界

单次读 1 MiB（`max_bytes` 缺省 64 KiB）、单次写 1 MiB、窗口 1..1000、每个请求 5s 预算；超限回 `E_INVALID`。`shell` 必须是存在的可执行文件绝对路径；`bash/sh/dash/zsh/ksh/mksh/fish` 以 `-l` 登录 shell 启动，其它二进制不加参数。
