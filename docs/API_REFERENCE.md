# HTTP and event API reference

    PairRoom 的浏览器 UI 使用本地 HTTP API 与 SSE。CLI / UI 是优先入口；直接调用 API 的客户端应把它视为随 PairRoom release 演进的本地控制面，而不是永久稳定的公网 SaaS API。

    ## 安全边界

    - 默认绑定 loopback；
    - 非 loopback 部署必须配置 token，并自行提供可信网络边界；
    - API 不应返回 Provider secret 或附件的绝对宿主路径；
    - destructive request 必须明确 Project / Room identity，并遵守 archive、active Turn 和 Binding 前置条件。

    ## 资源族

    Management API 负责 Project、Room 注册、Binding、archive、backup 和 Service diagnostics。Room API 负责 message、Turn、participant、role、approval、retry、cancel、attachment 和 event stream。应用内 Room 标签走 Management 同源 surface：`/api/v1/rooms/{room}/surface/…` 使用 Management Session，由服务端注入 Runtime bearer；`PATCH /api/v1/runtime-policy` 只调整并发 Runtime 上限；`POST /api/v1/rooms/{room}/open-browser` 在 Runtime 就绪后打开系统浏览器。归档 Room 没有 surface，也不能外开。

    状态类请求返回当前 projection；命令类请求先记录可审计事件，再异步驱动 native runtime。HTTP 成功只表示控制面接受了命令，最终执行结果应从 message processing、Turn summary 或 SSE event 判断。

    ## SSE 与重连

    durable event 带单调序号，可用于断线后续传；高频 text delta / command output 等 transient telemetry 可以是非持久事件，断线后不保证逐 token 重放。客户端重连后应重新获取 snapshot，再从 durable sequence 继续。

    ## 当前源码路由清单

    下列 path / prefix 从 `internal/server/` 和 `internal/service/` 自动提取。动态 ID 和方法约束以 handler 实现与测试为准。

    <!-- generated:routes -->
    <details>
    <summary>展开当前路由与前缀</summary>

    - `/api/`
- `/api/v1/`
- `/api/v1/approvals/`
- `/api/v1/approvals/approval-1`
- `/api/v1/approvals/approval-1/extra`
- `/api/v1/attachments`
- `/api/v1/attachments/`
- `/api/v1/attachments/../../etc/passwd`
- `/api/v1/events`
- `/api/v1/events?token=secret`
- `/api/v1/export`
- `/api/v1/export?format=json`
- `/api/v1/export?format=json&include_events=1`
- `/api/v1/export?format=markdown`
- `/api/v1/git/diff`
- `/api/v1/git/status`
- `/api/v1/health`
- `/api/v1/import`
- `/api/v1/maintenance/room-deletions/retry`
- `/api/v1/messages`
- `/api/v1/messages/`
- `/api/v1/messages/message-1/cancel`
- `/api/v1/messages/message-1/retry`
- `/api/v1/messages?before_seq=%d&limit=4`
- `/api/v1/messages?before_seq=nope`
- `/api/v1/participants/`
- `/api/v1/participants/claude/interrupt`
- `/api/v1/participants/claude/role`
- `/api/v1/participants/claude/stop`
- `/api/v1/projects`
- `/api/v1/projects/`
- `/api/v1/rooms/`
- `/api/v1/rooms/batch-archive`
- `/api/v1/rooms/batch-delete`
- `/api/v1/rooms/{room}/surface`
- `/api/v1/rooms/{room}/surface/{path...}`
- `/api/v1/runtime-policy`
- `/api/v1/service`
- `/api/v1/service?token=management-secret`
- `/api/v1/session`
- `/api/v1/settings`
- `/api/v1/snapshot`
- `/api/v1/snapshot?message_limit=1`
- `/api/v1/snapshot?message_limit=3`
- `/api/v1/snapshot?token=secret`
    </details>
    <!-- /generated:routes -->

    ## 客户端兼容原则

    1. 忽略未知 JSON 字段；
    2. 不根据 UI 文案推导状态机；
    3. destructive operation 后重新读取 projection；
    4. 不把 transient event 当作 durable receipt；
    5. release 升级前阅读 [Upgrading](UPGRADING.md)。
