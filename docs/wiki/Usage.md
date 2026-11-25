# 使用

## 协议方法（kore.plugin.v1）

| 方法 | 入参 | 结果要点 |
| --- | --- | --- |
| `kore.ping` | `{}` | `{status,uptime_s,goroutines,rss_kb}` |
| `kore.shutdown` | `{}` | 落盘历史、关闭全部会话后优雅退出 |
| `terminal.create` | `{client_id?,cols?,rows?,shell?}` | `{session_id,client_id,shell,pid,cols,rows}`，缺省 80×24、`/bin/bash` |
| `terminal.write` | `{session_id,client_id?,data}` | `data` 为 base64；输入记入历史 |
| `terminal.resize` | `{session_id,client_id?,cols,rows}` | `TIOCSWINSZ` 生效后回显 |
| `terminal.read` | `{session_id,client_id?,cursor?,max_bytes?}` | 增量读；带 `cursor` 从游标续读 |
| `terminal.attach` | `{session_id,client_id?,cursor?}` | 重连存活会话，校验游标可恢复 |
| `terminal.close` | `{session_id,client_id?}` | `kill(-pgid)` + 关 fd + 移出 map |
| `terminal.list` | `{client_id?}` | 不带为管理视图（全部），带则按归属过滤 |
| `terminal.history` | `{session_id?,client_id?,limit?}` | 命令历史查询（最新在前） |
| `terminal.history.clear` | `{session_id?,client_id?}` | 清理该调用方历史 |

## 数据编码

`terminal.write.data`、`terminal.read.data`、`terminal.history.entries[].data` 统一用标准 base64（带 `=` 填充）——终端输出可能含任意二进制与跨帧截断的多字节 UTF-8 序列，直接放进 JSON 会被静默替换成 U+FFFD。前端解码一次即可交给 `term.write(Uint8Array)`；非法 base64 回 `E_INVALID`。

## 典型流程

1. `terminal.create` → 拿 `session_id`（可选 `client_id` 做归属）。
2. `terminal.write`（base64 输入）→ `terminal.read` 增量取回输出。
3. 前端按 30–80ms 轮询 `terminal.read`（**不用 event 推送**：协议侧未登记 `terminal.output`、内核未把插件事件接到 SSE）。
4. 断线后带上次 `cursor` 调 `terminal.attach` / `terminal.read` 续读；窗口外的游标回 `E_CONFLICT`，需重新对齐。
5. 结束调 `terminal.close`（`kill(-pgid)` 清整棵进程树）。

## 会话归属

`client_id` 是插件内逻辑归属标签（不是认证凭据）：`write/read/resize/close/attach/history` 必须携带创建时的同一 `client_id`，越权回 `E_UNAUTHORIZED` 且不触碰归属方会话。真正的认证边界在内核 `method_capabilities` 授权。
