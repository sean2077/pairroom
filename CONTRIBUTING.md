# Contributing to PairRoom

## Development setup

```bash
git clone https://github.com/sean2077/pairroom.git
cd pairroom
make check
make smoke
```

`make check` 运行格式、静态检查、单元测试、race / dependency 检查和文档契约；`make smoke` 运行完整 Mock collaboration / recovery 场景。

## Change workflow

1. 从最新 `main` 创建短生命周期分支；
2. 先写清行为不变量和失败边界；
3. 修改最小必要代码与文档；
4. 增加覆盖真实状态转换的测试；
5. 运行验证并在 PR 中如实列出；
6. 通过 PR 合入，不直接向 `main` 推送。

并发与恢复相关修复至少覆盖：正常路径、取消、进程退出、重启、迟到 event 和重复回调。

## Documentation ownership

[docs/README.md](docs/README.md) 定义每份文档的唯一职责。不要把同一段协作语义复制到 README、架构、运维和故障排查中。

规则：

- 长期 Reference 只记录当前行为；计划和一次性审查放 Issue / PR；
- 不在 README 硬编码当前 release；
- CLI 参数以 `cmd/pairroom/` 为事实源；
- HTTP route 以 `internal/server/`、`internal/service/` 为事实源；
- JSON 字段以 `internal/config/` struct tag 为事实源；
- Event schema 以 `internal/model/types.go`、apply 代码和 `internal/store/` 为事实源；
- breaking change 同步更新 `CHANGELOG.md` 与 `docs/UPGRADING.md`。

提交前：

```bash
make docs-check
```

## Testing claims

清楚区分：

- unit / race test；
- Mock E2E；
- 构建与 cross-build；
- 真实 Claude Code / Codex native E2E。

没有运行官方 CLI 时，不得把 Mock 结果描述成 native E2E。

## Pull request

PR 应包含：问题、设计边界、用户可见变化、迁移影响、验证命令、未验证内容和恢复方式。大重构优先拆分为可独立审查的提交，但不要保留只为搬运 patch 或导出源码而存在的临时 workflow。
