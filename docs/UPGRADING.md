# Upgrading

PairRoom 的 CLI、Event Log、HTTP API 和 native adapter 会随官方 CLI 演进。升级应当被视为一次受控变更，而不是直接覆盖 binary。

## 升级前

1. 阅读 [CHANGELOG](../CHANGELOG.md)；
2. 停止或归档 active Room；
3. 创建并验证 PairRoom data backup；
4. 记录当前 binary、Claude Code、Codex 与配置版本；
5. 确保工作仓库没有未识别的 native side effect。

## 当前 breaking boundary

routing policy 只支持：

```json
{"routing_mode": "turns"}
```

`manual`、`mentions`、`roundtable` 不兼容、不会静默规范化。升级旧安装时：

- 配置文件显式改为 `turns`；
- CLI 自动化移除旧 `--routing` 值；
- 含旧 routing event 的持久化 Room 先备份，再重建；
- 不直接改写 JSONL 伪造迁移。

## 执行升级

替换 binary 后先运行：

```bash
pairroom version
pairroom service --mock
```

再验证：

- 配置可以严格解析；
- Project registry 可读取；
- 一个新 Mock Room 可以完成多 Turn FIFO；
- 备份验证通过；
- 真实模式先完成只读单 Agent Turn，再测试 reviewer / handoff。

## 回滚

回滚 binary 不等于回滚 Event Log。若新版本已写入旧版本不理解的 event：

1. 停止 Service；
2. 保存当前 data root；
3. 恢复升级前经过验证的完整备份；
4. 恢复匹配的 binary 与配置；
5. 重新验证 Binding 和仓库副作用。

不要混用新旧 data files。

## 文档和客户端

直接调用 HTTP API、解析 Event Log 或依赖 CLI 文案的外部工具，必须在升级时重新运行契约测试。`docs/API_REFERENCE.md` 中的 route inventory 和 `docs/CLI_REFERENCE.md` 中的 flag inventory由 `make docs-check` 对照当前源码。
