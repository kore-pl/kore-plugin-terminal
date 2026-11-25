# 版本记录

完整变更记录见仓库 [CHANGELOG.md](https://github.com/kore-pl/kore-plugin-terminal/blob/main/CHANGELOG.md)。最近版本：

## 0.2.2 (2026-11-25)

- 新增七语言文档（zh/en/es/fr/de/ru/pt）、README 语言切换行与安全/CONTRIBUTING 工程化文件；纯文档与开源工程标准化，不改运行时行为。

## 0.2.1 (2026-11-23)

- 补齐 G-20 故障注入：`attach` / `history` / `history.clear` 三个新方法此前零 fault 覆盖；仅测试与版本变更。

## 0.2.0 (2026-11-23)

- 新增方法级 `method_capabilities`（§3.5 #12）；新增 `terminal.history` / `terminal.history.clear`（按会话持久化的命令历史）；新增 `terminal.attach` 与 `terminal.read.cursor`（断线重连补读）；会话绑定创建者 `client_id`，越权回 `E_UNAUTHORIZED`。

## 0.1.1 (2026-11-23)

- 位级可复现打包，二进制改嵌 manifest.json。

## 0.1.0 (2026-11-22)

- 首版：标准库自实现 PTY、`terminal.create/write/read/resize/close/list`、setsid 进程组清理、四类故障注入与真实插件进程 E2E。
