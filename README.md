# Tematcha

独立仓库，不是 [letcode](https://github.com/letr007/letcode) 本体的一部分。通过 ACP（`letcode acp`）驱动引擎，界面用 Bubble Tea 自绘。不要把本仓库推到 letcode 上游；需要对照引擎时最多 fork letcode。

```
go run .
```

引擎路径和工作区目前写在 `main.go` 的常量里。无 TTY 自检：`-widthcheck`、`-feedtest`、`-permtest`、`-navtest`、`-statustest`、`-paneltest`、`-scrolltest`、`-cmdtest`。
