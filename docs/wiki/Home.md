# kore-plugin-terminal

kore 的 Web 终端插件：每个会话是一个**真实 PTY + 登录 shell**，不是命令执行包装器。写入即键盘输入，读取即终端输出（含转义序列），前端用 xterm.js 之类的终端模拟器对接即可。

- 协议：`kore.plugin.v1` · `lifecycle=resident` · 深度等级 `production`
- 能力：`system:read` + `system:admin` + `proc:write`，声明方法级 `method_capabilities`
- 0.2.0 起补齐方法级 capability、按会话持久化的命令历史、断线重连（读游标可恢复）

## 导航

- [Installation](Installation) — 安装与构建
- [Configuration](Configuration) — 环境变量与配置
- [Usage](Usage) — 协议方法与典型调用
- [FAQ](FAQ) — 常见问题
- [Changelog](Changelog) — 版本记录

项目主页与完整文档：<https://github.com/kore-pl/kore-plugin-terminal>
