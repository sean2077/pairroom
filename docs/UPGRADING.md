# PairRoom 升级、迁移与回滚

升级的核心目标不是“新 binary 能启动”，而是 Room Event Log、Binding ownership、Vendor resume、Runtime policy 和回滚边界仍可证明。

## 1. 阅读变化

- 阅读 `CHANGELOG.md` 的 `Unreleased` 与目标版本章节；
- 检查 routing、store schema、Binding、Provider、daemon 和删除语义；
- 查看 [Runtime Compatibility](RUNTIME_COMPATIBILITY.md) 对当前 Claude/Codex 的要求。

当前 routing 只接受 `turns`。配置或持久化 Room 中出现 `manual`、`mentions`、`roundtable` 会失败；没有静默迁移。

## 2. 保存运行信息

```bash
pairroom version --json
pairroom daemon status
pairroom doctor --repo /absolute/path/to/project --json
```

保存脱敏配置、daemon install 参数、data root、关键 Project root 和 Vendor CLI 版本。不要保存 Token 到公开工单。

## 3. 停止写入

对关键 Room 先完成/取消当前 Turn并 Archive，或停止 Service：

```bash
pairroom daemon stop
# 前台 Service 使用 Ctrl-C / SIGTERM
```

确认旧进程已退出、`service.lock` 正常释放。不要在 active writer 存在时复制 data root。

## 4. 校验与备份

对每个关键 Room：

```bash
pairroom verify --data-dir /absolute/path/to/room --json
pairroom backup --data-dir /absolute/path/to/room --output /safe/path/room.tar.gz
```

此外复制 Service 配置与 `service-registry.json` 作为操作证据。Registry checkpoint 不替代 Room 备份。

至少对一个备份做恢复演练：

```bash
pairroom restore --input /safe/path/room.tar.gz --data-dir /tmp/room-restore-test
pairroom verify --data-dir /tmp/room-restore-test --json
```

## 5. 替换 binary

从源码：

```bash
git switch <target>
make check
make build
./dist/pairroom version --json
```

或使用对应平台 release artifact，验证 checksum 和 `version --json`。GitHub workflow artifact 下载后可能需要给 Linux/macOS binary 补 executable bit。

## 6. 前台首次启动

先不要直接恢复 daemon：

```bash
pairroom service \
  --config /absolute/path/to/pairroom.json \
  --data-root /absolute/path/to/data \
  --no-browser
```

检查：

- Registry rebuild / checkpoint 无错误；
- Project availability 与 Room lifecycle 正确；
- Binding ownership 无冲突；
- 关键 Room snapshot / verify 正常；
- archived Room 未被意外激活；
- pending deletion cleanup 没有新增异常。

再运行 Mock Service 或单 Room smoke，确认当前 binary 的控制面。

## 7. Native smoke

在非关键仓库完成：

- Claude 新 Session 的只读 Turn；
- Codex 新 Thread 的只读 Turn；
- Existing Binding 精确 resume；
- `error -> completed` 等 terminal boundary；
- interrupt / approval / reviewer policy；
- 必要时 Provider endpoint/model。

Doctor、Mock 和 cross-build 不能替代这一步。

## 8. 恢复 daemon

```bash
pairroom daemon install --force -- <saved service args>
pairroom daemon start
pairroom daemon status
pairroom daemon open
```

确认实际 binary、参数、log path 与 data root。`restart` 不更新安装定义。

## 9. 单 Room 到 Service

旧 `serve` Room 可以通过 Management import 登记：

- import 不搬移或改写 `events.jsonl`；
- 路径必须绝对且可验证；
- Room ID、Project canonical root 和 Binding ownership 仍去重；
- Existing Binding 只保留 PairRoom 绑定边界后的 Timeline。

不要同时让旧 `serve` 与新 Service 写同一个 Room 目录。

## 10. Breaking data change

若新版本写入旧版本不理解的 schema/event：

- 新版本必须明确拒绝不支持的旧状态或提供受测试迁移；
- 旧版本必须拒绝 future schema，而不是跳过；
- Changelog 与 PR 必须标出不可逆点；
- 回滚前从升级前备份恢复，不要用旧 binary 打开已被新版本写入的目录。

## 11. 回滚

1. 停止新版本；
2. 保存新版本 data root 的 forensic 副本；
3. 确认旧 binary 版本；
4. 恢复升级前 Room 备份和必要配置；
5. 恢复 daemon 定义；
6. 前台启动并 `verify`；
7. 再做 Mock / native smoke。

不要只替换 binary 而继续使用已发生不兼容写入的数据。

## 12. 验收清单

- [ ] 版本、commit、tag / Changelog 身份一致；
- [ ] 关键 Room verify 与备份恢复通过；
- [ ] Registry / Project / Binding ownership 正常；
- [ ] Mock smoke 通过；
- [ ] 当前 Vendor CLI native smoke 有真实证据或明确标记未运行；
- [ ] listener、Session、CSRF 与 Provider secret 边界未回退；
- [ ] 回滚步骤和备份位置已记录。
