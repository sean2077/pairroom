# Documentation map

本目录只保留当前需要维护的文档。历史方案、一次性 review、发布快照和旧截图由 Git 历史保存，不在现行文档树中继续复制。

## 按任务阅读

| 目标 | 文档 | 唯一职责 |
|---|---|---|
| 第一次运行 | [GETTING_STARTED](GETTING_STARTED.md) | 从安装到完成第一个 Room |
| 理解行为 | [CONCEPTS](CONCEPTS.md) | Project、Room、Turn、FIFO、角色、Workflow 与审批语义 |
| 修改配置 | [CONFIGURATION](CONFIGURATION.md) | JSON 配置、Provider、runtime policy 与安全边界 |
| 查询命令 | [CLI_REFERENCE](CLI_REFERENCE.md) | 命令入口、参数发现方式与自动核验清单 |
| 调用 HTTP | [API_REFERENCE](API_REFERENCE.md) | Management / Room HTTP 与 SSE 契约 |
| 修改实现 | [ARCHITECTURE](ARCHITECTURE.md) | 组件、状态所有权、不变量与代码导航 |
| 理解持久化 | [STORAGE](STORAGE.md) | Event Log、Binding、附件、备份和重启恢复 |
| 部署与维护 | [OPERATIONS](OPERATIONS.md) | Service、Daemon、归档、删除、诊断和恢复 |
| 排查问题 | [TROUBLESHOOTING](TROUBLESHOOTING.md) | 按症状定位常见故障 |
| 跨版本升级 | [UPGRADING](UPGRADING.md) | Breaking change、备份、验证和回滚 |
| 修改 Agent 合同 | [PROTOCOL](PROTOCOL.md) | 输入封装、控制标记、handoff 与收敛规则 |

顶层 [README](../README.md) 只负责产品定位与最短上手路径；[CONTRIBUTING](../CONTRIBUTING.md) 只负责开发流程；[CHANGELOG](../CHANGELOG.md) 只记录版本历史。

## 内容边界

同一个事实只应有一个详细解释位置：

- 协作语义属于 `CONCEPTS.md`；
- 代码结构与并发不变量属于 `ARCHITECTURE.md`；
- 精确命令和接口名称属于 Reference 文档；
- 故障处置属于 `TROUBLESHOOTING.md`；
- 版本迁移属于 `UPGRADING.md`。

其他文档引用它，不复制整段说明。无法由测试或源码核验的短期计划应放在 Issue / PR，而不是长期 Reference。

## 维护规则

变更以下代码时必须同步对应文档：

| 代码区域 | 文档 |
|---|---|
| `internal/room/`、`internal/agent/` | `CONCEPTS.md`、`ARCHITECTURE.md`、`PROTOCOL.md` |
| `internal/config/`、Provider 解析 | `CONFIGURATION.md` |
| `cmd/pairroom/` | `CLI_REFERENCE.md` |
| `internal/server/`、`internal/service/` HTTP handler | `API_REFERENCE.md` |
| `internal/store/`、archive / backup | `STORAGE.md`、`OPERATIONS.md`、`UPGRADING.md` |

提交前运行：

```bash
make docs-check
```
