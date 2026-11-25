# 常见问题（FAQ）

## 为什么用 base64 而不是直接传字符串？

终端输出可能含任意二进制字节，多字节 UTF-8 字符还可能被增量读截断在两帧之间；直接放进 JSON 字符串会被 `json.Marshal` 静默替换成 U+FFFD，造成数据损坏。base64 解码一次即可交给 xterm.js 的 `term.write(Uint8Array)`。

## 为什么不用 SSE/event 推送终端输出？

协议侧 `docs/PLUGIN-PROTOCOL.md` §8 规定事件名必须已登记，自定义 `terminal.output` 属于未登记事件；内核实现也未把插件事件接到 SSE（`GET /api/v1/events` 目前 501）。因此输出通道是轮询 `terminal.read` + `attach`/`cursor` 恢复。前端按 30–80ms 轮询或输入后立即读即可获得交互级体验。

## 会话什么时候会被回收？

创建后一直存活，直到显式 `terminal.close`、shell 自行退出、插件退出或空闲回收（默认 1800s）。回收是**惰性/调用时驱动**（无 ticker）：`create/read/write/resize/close/attach/list` 调用时顺带摘除超时会话。回收后不可恢复，`attach`/`read` 回 `E_NOTFOUND`，必须重新 `create`。`attach` 返回的 `idle_expires_at` 可用于提前续期。

## 断线后输出会丢吗？

补读窗口 `KORE_TERMINAL_REPLAY_BYTES`（默认 64 KiB）保留最近交付的字节，带游标在其中可回放；未交付的输出仍在 PTY 内核缓冲里、会话存活期内不会丢。窗口外的游标回 `E_CONFLICT`，需重新对齐。

## 命令历史会记录口令吗？

只记录 `terminal.write` 的实际输入字节，若用户在终端里输入口令确实会进历史文件。插件从不把输入写日志、历史文件 0600/目录 0700，但仍建议不要粘贴明文口令；可用 `terminal.history.clear` 清理或用 `KORE_TERMINAL_HISTORY=off` 关闭。

## 能在 macOS / Windows 上跑吗？

PTY 实现按 Linux 固定的 `/dev/ptmx` 流程写，只在 `linux/amd64`、`linux/arm64`、`linux/riscv64`、`linux/loong64` 编译；其它平台显式返回 `E_UNSUPPORTED`，不静默降级、不伪造成功。
