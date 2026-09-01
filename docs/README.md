# PairRoom 文档地图

本目录只保留需要持续维护的当前文档。路线图、一次性调研、旧 PR 说明、发布快照、冻结验证输出和过时截图由 Issue、PR、Release、CI 与 Git 历史保存，不继续占据现行文档树。

## 学习与使用

| 文档 | 唯一职责 |
|---|---|
| [GETTING_STARTED](GETTING_STARTED.md) | 从构建到完成第一个 Mock / native Room |
| [USER_GUIDE](USER_GUIDE.md) | Management Shell、Room View、消息、Workflow、归档和删除的操作方法 |
| [CONCEPTS](CONCEPTS.md) | Project、Room、Binding、Runtime、Message、Turn、FIFO、角色和审批语义 |
| [CONFIGURATION](CONFIGURATION.md) | JSON 配置、Agent override、Provider profile 与 cc-connect 引用导入 |
| [CLI_REFERENCE](CLI_REFERENCE.md) | 当前顶层命令、参数职责和退出边界 |
| [API_REFERENCE](API_REFERENCE.md) | Management / Room HTTP API、认证和 SSE |

## 设计与实现

| 文档 | 唯一职责 |
|---|---|
| [ARCHITECTURE](ARCHITECTURE.md) | 组件、状态权威、并发不变量和代码导航 |
| [STORAGE](STORAGE.md) | 数据布局、Event Log、Registry、附件、重启、备份和删除一致性 |
| [PROTOCOL](PROTOCOL.md) | Agent 输入输出、Delivery/Processing、handoff、控制标记和审批合同 |
| [RUNTIME_COMPATIBILITY](RUNTIME_COMPATIBILITY.md) | Claude/Codex 原生协议、能力探测与 Vendor 升级策略 |

## 运维与治理

| 文档 | 唯一职责 |
|---|---|
| [OPERATIONS](OPERATIONS.md) | Service/daemon、容量、备份、恢复、诊断和关闭 |
| [TROUBLESHOOTING](TROUBLESHOOTING.md) | 按症状定位启动、认证、Runtime、队列和数据问题 |
| [UPGRADING](UPGRADING.md) | Breaking change、升级前备份、迁移、验证和回滚 |
| [PRIVACY](PRIVACY.md) | 本机、浏览器、供应商和 diagnostics 数据流 |
| [RELEASING](RELEASING.md) | 版本、Changelog、构建产物与发布门禁 |
| [SECURITY](../SECURITY.md) | 威胁模型、认证、附件、审批和本地安全边界 |
| [CONTRIBUTING](../CONTRIBUTING.md) | 开发环境、不变量、测试矩阵、PR 和文档维护规则 |
| [SUPPORT](../SUPPORT.md) | Issue 资料、脱敏要求和支持范围 |
| [HISTORY_PROVENANCE](../HISTORY_PROVENANCE.md) | 解释早期重建提交的来源与哈希边界；仅承担历史真实性说明 |

## 内容所有权

同一事实只在一处详细解释：

- 用户“怎么做”属于 `GETTING_STARTED.md` 或 `USER_GUIDE.md`；
- 行为含义属于 `CONCEPTS.md`；
- 精确命令、配置和 HTTP route 属于对应 Reference；
- 代码结构与并发不变量属于 `ARCHITECTURE.md`；
- durable / ephemeral 数据边界属于 `STORAGE.md`；
- Agent wire 与控制标记属于 `PROTOCOL.md`；
- 执行步骤和事故处置属于 `OPERATIONS.md` / `TROUBLESHOOTING.md`；
- 安全、隐私、升级和发布各自由对应文档负责。

其他文档应链接到权威位置，而不是复制整段说明。顶层 [README](../README.md) 只负责产品定位和最短成功路径；[CHANGELOG](../CHANGELOG.md) 只记录版本历史；`HISTORY_PROVENANCE.md` 是唯一保留的历史说明例外。

## 代码到文档的维护映射

| 代码区域 | 必查文档 |
|---|---|
| `cmd/pairroom/` | `CLI_REFERENCE.md`、必要时 `CONFIGURATION.md` |
| `internal/service/` | `CONCEPTS.md`、`API_REFERENCE.md`、`OPERATIONS.md`、`STORAGE.md` |
| `internal/room/` | `CONCEPTS.md`、`ARCHITECTURE.md`、`PROTOCOL.md` |
| `internal/agent/` | `PROTOCOL.md`、`RUNTIME_COMPATIBILITY.md` |
| `internal/server/` | `USER_GUIDE.md`、`API_REFERENCE.md`、`SECURITY.md` |
| `internal/store/`、`internal/archive/`、`internal/attachment/` | `STORAGE.md`、`OPERATIONS.md`、`PRIVACY.md` |
| `internal/config/` | `CONFIGURATION.md` |

提交前运行：

```bash
make docs-check
```
