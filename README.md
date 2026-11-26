# CodexRM Agent

Windows / Linux 连接器：负责连接 Codex、本机设置页、安装和更新，以及 Linux 机器人。Android 本机 Codex 通过 `agent/` 包复用同一实现。

## 开发

```sh
go test ./...
node --test internal/agenthost/settings.test.mjs
go build ./cmd/codexrm-agent
```

`integration/relay` 验证真实 Agent 和 Server 的互操作；`go.mod` 固定测试使用的 Server 版本。升级 Server 依赖后，请重新运行这些测试。Windows 引导安装器在 `cmd/codexrm-setup`，离线安装器定义在 `build/installer`。正式更新前在 `internal/release/release.go` 递增 Agent 版本号和版本码，并在主仓库更新版本锁定。

**本仓库不发布 GitHub Releases**。安装包和更新统一从 [CodexRM 主仓库](https://github.com/CyL-Cly/CodexRM) 构建与发布，拆分前的提交历史也保留在主仓库。
