# PairRoom v0.1.0 验证记录

验证日期：2026-08-13

## 静态与自动测试

执行：

```bash
gofmt -w $(find . -name '*.go' -type f)
node --check internal/server/assets/app.js
go test ./...
go test -race ./...
go vet ./...
```

结果：全部通过。

覆盖的关键行为包括：

- 配置默认值与严格 JSON 解析。
- @mention 与消息 envelope。
- 用户消息向指定 Agent 投递。
- Mentions 模式的 Agent → Peer 接力。
- Roundtable 控制标记与 hop 上限。
- 新用户消息抑制陈旧自动接力。
- session/thread ID 恢复，临时运行状态重置。
- JSONL sequence、重放和 metadata。
- 崩溃留下半行 JSON 后，重新打开会截断损坏尾部并可继续安全追加。
- Codex 审批响应及权限请求 fail-closed。
- API health、message、snapshot、Bearer Token 和跨源拒绝。

## Mock 端到端验证

使用预编译 Linux 二进制启动一个临时 Git 仓库：

```bash
pairroom-linux-amd64 serve \
  --repo <temporary-repo> \
  --data-dir <temporary-data> \
  --listen 127.0.0.1:7351 \
  --mock \
  --no-browser
```

随后：

1. 等待 `/api/v1/health` 返回 200。
2. 向 Claude 与 Codex 发送同一条中文 `@all` 消息。
3. 等待双方完成首轮，并通过 @mention 互相接力。
4. 读取 snapshot、Git status、Git diff、metadata 与事件日志。
5. 正常终止 daemon。

观测结果：

```json
{
  "messages": 5,
  "events": 86,
  "latest_seq": 86,
  "initial_delivery": {
    "claude": "started",
    "codex": "started"
  },
  "participants": {
    "claude": {
      "state": "idle",
      "role": "driver",
      "session_id": "mock-claude"
    },
    "codex": {
      "state": "idle",
      "role": "reviewer",
      "session_id": "mock-codex"
    }
  },
  "git_status_contains_demo": true,
  "git_diff_contains_change": true,
  "metadata_exists": true,
  "event_log_lines": 86
}
```

## 跨平台构建

以下目标均完成编译：

```text
linux/amd64
windows/amd64
darwin/arm64
darwin/amd64
```

SHA-256 位于 `dist/SHA256SUMS`。

## 当前环境无法完成的验证

本次执行容器中没有安装或登录 `claude` 与 `codex`。`pairroom doctor` 的结果为：Git 可用，Claude Code、Codex 和 Codex App Server 不存在。

因此：

- Mock、房间、路由、持久化、Web/API、Git Inspector 与协议解析已经实际执行验证。
- Claude/Codex Adapter 按当前官方结构化协议实现并可编译，但没有在本容器中进行真实账号登录后的端到端 Turn。
- 首次在真实开发机试用时，应先运行 `pairroom doctor`，再在非关键仓库验证各自当前 CLI 版本、权限模式和恢复行为。
