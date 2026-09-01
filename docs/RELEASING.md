# PairRoom 发布指南

本页是 maintainer 的发布门禁。开发验证规则见 [Contributing](../CONTRIBUTING.md)，用户升级步骤见 [Upgrading](UPGRADING.md)。

## 版本身份

以下必须一致：

- `VERSION`；
- `internal/version.Current`；
- Git tag `vX.Y.Z`；
- `CHANGELOG.md` 中唯一非空的 `## [vX.Y.Z] — YYYY-MM-DD` 章节；
- binary `pairroom version --json`。

README 不固定当前版本；未发布变化只写入 `Unreleased`。

## 发布前证据

```bash
make docs-check
make check
make smoke
```

还需记录：

- 当前 Claude Code/Codex 版本；
- 真实 native smoke 场景与结果，或明确 `not run`；
- 关键 Room 的 verify / backup / restore 演练；
- breaking schema、routing、Binding、daemon 或 Provider 变化；
- 回滚方案和升级前备份边界。

Mock、cross-build 和 doctor 不替代 native smoke。

## Changelog

把 `Unreleased` 中用户可见变化移动到目标版本章节，按 Added / Changed / Fixed / Security 分类。不要复制 PR 描述、实现过程或冻结测试输出；链接 Issue/PR 作为历史细节。

`make release` 会调用 release contract，拒绝缺失、重复或空版本章节。

## 生成发布包

```bash
make release
```

它执行验证并生成多平台 payload，包括支持的 Linux amd64、Windows amd64、macOS arm64 / amd64 binary、source archive、checksum、SBOM/provenance 和 version evidence。它不创建或推送 tag。

检查：

- 产物名称唯一且带版本/平台；
- checksum 与文件匹配；
- Linux/macOS binary executable；
- `version --json` 显示目标 commit；
- source archive 不含本地数据、Token 或构建垃圾。

## CI 产物

普通 CI 在 verify 通过后构建四个平台 artifact，并重新下载完整集合验证名称、checksum、文件格式与 Linux version metadata。Workflow artifact 是 commit 级证据，保留期有限，不等同于 GitHub Release。

## Tag 与 Release

1. 确认工作树、main 与目标 commit；
2. 创建精确 `vX.Y.Z` tag；
3. 推送 tag；
4. Release workflow 重新验证 version/changelog identity 和产物；
5. 检查 GitHub Release notes、assets 与 checksum；
6. 从发布资产做一次干净安装 / `version --json` / Mock smoke；
7. 发布升级说明和 breaking change。

不要复用 tag 或替换已发布资产而不改变版本。

## 发布验收

- [ ] source、tag、VERSION、Changelog 与 binary identity 一致；
- [ ] unit/race/vet/docs/contract/Mock smoke 通过；
- [ ] 四个平台产物与汇总校验通过；
- [ ] archive、attachment、Registry、delete/recovery 边界有当前测试；
- [ ] Security/Privacy/Upgrade 文档覆盖用户可见变化；
- [ ] 当前 Vendor native smoke 有真实记录或明确未运行；
- [ ] rollback 和已验证备份可用；
- [ ] Release assets 可下载且 checksum 可复现。
