# 安装与构建

独立 Go 模块，只依赖 `kore/pkg/pluginapi` 与标准库，不引入第三方依赖、不提交 `go.work` 与跨仓 `replace`。Go 1.26。

## 从源码构建

```sh
git clone https://github.com/kore-pl/kore-plugin-terminal.git
cd kore-plugin-terminal
make build          # 产出 bin/terminal
make check          # gofmt → go vet → go test
make dist           # 产出 dist/terminal-<版本>.tar.gz + .sha256
```

构建基线：`CGO_ENABLED=0 GOWORK=off GOAMD64=v1 -trimpath -buildvcs=false -ldflags="-s -w"`。产物是静态单二进制，不绑定构建机 glibc。`make dist` 位级可复现（条目固定、mtime/owner/group/mode 归一、`gzip -9n`），同一源码两次打包 sha256 一致。

## 安装到 kore

1. 用 `make dist` 产出 `dist/terminal-<版本>.tar.gz` 与 `.sha256`。
2. 由 kore 安装器按 `info.json`（包内首条目）读取插件声明，安装到插件状态目录。
3. 插件 socket 由主壳创建、`chown kore-terminal`、权限 `0600`。
4. 启动后 `kore.ping` 验证存活。

## 平台范围

PTY 实现在 `linux/amd64`、`linux/arm64`、`linux/riscv64`、`linux/loong64` 编译；其它平台按显式降级返回 `E_UNSUPPORTED`（不静默降级、不伪造成功）。

详见 [Usage](Usage) 与 [Configuration](Configuration)。
