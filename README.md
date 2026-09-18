# Tematcha

独立仓库：不是 [letcode](https://github.com/letr007/letcode) 本体的一部分。界面用 Bubble Tea 自绘，通过 ACP（`letcode acp`）驱动引擎。

## 与 letcode 本体的关系（硬规矩）

- `..\letcode\` 是上游克隆，**只读对照**：不改源码、不提交、不推送、不 fork、不提 issue/PR；将来确有必要，先经用户同意。
- 本仓库的代码、提交、文件**绝不**写进 letcode 克隆目录；引擎只以外部进程方式驱动（`letcode acp`），前端不链接、不补丁引擎源码。
- 引擎的工作区常量指向 `demo-lab/`（见 `main.go`），工具调用改的是演示沙盒，不是 letcode 本体。
- 上游克隆里唯一会出现的写入是本地构建产物 `target/`（`cargo build` 生成，被上游仓库自身忽略）。

```
go run .
```

引擎路径和工作区目前写在 `main.go` 的常量里。无 TTY 自检：`-widthcheck`、`-mdtest`、`-feedtest`、`-permtest`、`-navtest`、`-statustest`、`-paneltest`、`-scrolltest`、`-cmdtest`、`-histtest`、`-sesstest`、`-queuetest`、`-todostest`、`-themetest`、`-compacttest`、`-agenttest`、`-echotest`、`-elicittest`、`-copytest`；真机探针（起引擎但不进 TUI）：`-sessions`（列会话）、`-load <sessionId>`（载入并统计重放）。
