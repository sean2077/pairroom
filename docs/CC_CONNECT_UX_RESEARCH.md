# cc-connect 管理体验调研与 PairRoom 适配决策

调研日期：2026-08-16

基线：

- PairRoom `main`: `95259bd42b6c6d43564c78aa9de8d5a237fb8602`
- cc-connect `main`: `6c8607980016c5eccaf5a3aa1f53dbe78e4f9c5d`

本调研只借鉴 cc-connect 的多 Project / multi-workspace 管理、Web Admin 信息架构和操作反馈，不改变 PairRoom 的双 Agent Room、统一审批、公共时间线、Binding 独占和非抢占 Runtime 安全边界。

## 1. 两个产品的管理对象不同

cc-connect 的核心管理对象是“一个 Project 对应一个 Agent 配置及若干平台、Session 与 Workspace”。它的 Web Admin 已形成 Dashboard、Projects、Sessions、Providers、Bridge、Skills、Cron、System 等分区，并在 Project 列表和详情页展示 Agent 类型、平台、Session 数量以及 Agent 类型相关配置。

PairRoom 的核心管理对象是：

```text
PairRoom Service
└── Project（canonical Git worktree root）
    └── Room（公共协作时间线）
        ├── Claude Session Binding
        ├── Codex Thread Binding
        └── 独立 Runtime / Event Log / Attachment Store
```

因此，PairRoom 不能直接复制 cc-connect 的“Project 即 Agent 实例”拓扑。适配后的页面必须让用户先理解 Service 资源、Project 边界和 Room 状态，再进入具体 Room View。

## 2. 值得借鉴的体验模式

| cc-connect 模式 | 价值 | PairRoom 适配 |
|---|---|---|
| Dashboard 聚合版本、运行状态、Project 和最近 Session | 打开后台即可判断系统是否可用、下一步去哪里 | Overview 聚合 Project/Room/Runtime 数量、待处理项、活动 Runtime 和最近 Project |
| Project 卡片和详情页 | 列表适合扫描，详情适合渐进展开 | Projects 卡片展示 canonical path、Room 数、运行状态；详情页承载 Room 创建和生命周期操作 |
| 侧边导航分区 | 避免所有控制塞进单页 | Overview、Projects、Runtimes、Settings 四个稳定路由 |
| 设置页按主题分组 | 将常用偏好、运行配置、系统运维分开 | Interface、Runtime、Daemon、Service、Safety boundaries 五类设置内容 |
| 统一 loading、empty、error、toast 反馈 | 长操作和失败不再依赖浏览器原生弹窗 | 连接横幅、空态、按钮 busy 状态、Toast、语义化 Dialog |
| 能力感知的设置项 | 不向不支持的 Agent 展示错误选项 | Service snapshot 明确返回 capabilities；未支持的删除、路径浏览和热变更在 UI 中解释并禁用 |
| 状态/日志/配置运维入口 | 管理页面同时承担诊断入口 | Daemon 分区提供 status、logs、restart 和重装定义命令说明；诊断导出默认脱敏 |
| 响应式卡片与紧凑信息密度 | 手机端仍可完成远程管理 | 窄屏侧栏变抽屉，Runtime 表格变成带字段标签的卡片，不产生横向溢出 |

## 3. 明确不照搬的部分

### 3.1 不采用单 Agent Project 拓扑

PairRoom 的一个 Room 必须同时拥有 Claude 和 Codex 两侧 Binding，并共享 PairRoom Event Log。Project 不能退化为一个 Agent 配置容器。

### 3.2 不让 Web 设置热改 Runtime policy

当前 Runtime limit、idle timeout、shutdown timeout 和 Agent CLI 配置属于 Service 进程启动定义。设置页显示“当前生效值”和可复制命令，但不伪装成已持久化的表单。后台服务的参数变更必须重新执行完整的 `pairroom daemon install --force ...`，而不是把新参数附加到 `daemon restart`。

### 3.3 不提供服务器目录浏览和隐式扫描

Project Registration 继续只接受用户显式输入的绝对路径，并由服务端解析 canonical Git worktree root。Web Shell 不展示服务器目录树，也不扫描常用开发目录。

### 3.4 不增加破坏性删除

首版仍只有 Room create、rename、archive、restore；Project removal 与 Room deletion 明确作为不支持能力展示。归档 Room 仍保留 Event Log、附件和 Binding Identity 所有权。

### 3.5 不让管理页抢占活动 Turn

手动“挂起”只允许：

- 取消尚未启动的排队 Runtime；
- 挂起没有活动 Turn 的 idle Runtime。

忙碌 Runtime 返回冲突，不会为了腾出容量而 interrupt Agent。清理状态无法证明安全的 failed Runtime 也拒绝手动重试挂起。

### 3.6 不从 Web 页面自停或自重启当前 Service

当前页面依赖正在运行的 Service 提供认证和控制 API。为避免请求结果不确定、浏览器误判和旧 lock 处理不透明，页面只展示经过验证的命令，不提供“停止自身”或“重启自身”按钮。

## 4. 本轮多轮迭代

### 第一轮：管理信息架构

- 将原单页卡片列表升级为可路由 Management Shell。
- 增加 Overview、Projects、Runtimes、Settings。
- 增加全局 Project/Room 搜索、Project 过滤和归档 Room 显示开关。
- 将 Register Project、Import Legacy Room、Create/Rename/Archive/Restore/Binding completion 全部改为表单或 Dialog，移除 `window.prompt` / `window.confirm`。
- 增加连接状态横幅、统一空态、Toast、键盘 Escape 关闭和移动端抽屉导航。

### 第二轮：Runtime 可观测性与安全控制

- `GET /api/v1/service` 返回有效 Runtime policy、聚合 summary 和 capabilities。
- Runtime 状态增加 `occupies_capacity`，区分“有记录”与“真正占用容量”。
- Overview 展示容量使用、忙碌/排队/失败数量和需要关注的问题。
- Runtimes 页面展示 FIFO queue position、busy、last used、错误和 Room 所属 Project。
- 增加 `POST /api/v1/rooms/{room}/suspend`，只执行安全的 idle suspend 或 queued cancel。
- 修复 390 px 窄屏 Runtime 表格横向溢出，改为标签化卡片布局。

### 第三轮：设置、诊断与 daemon 运维

- Interface：system/light/dark、comfortable/compact、默认 10 秒自动刷新（可选 5/10/30/60 秒或关闭）、打开 Room 行为。
- Runtime：只读展示当前 limit、idle timeout、poll interval、close timeout，并给出 foreground/daemon 更新示例。
- Daemon：展示 status、logs、restart、重新安装完整服务定义和 stale-lock 恢复边界。
- Service：展示版本、commit、build date、data root、健康状态和脱敏诊断导出。
- Safety boundaries：通过 capabilities 清楚解释 Project removal、Room deletion、服务器路径浏览、Runtime 热修改为什么不可用。

## 5. API 与兼容性原则

新增 snapshot 字段是向后兼容的：

```json
{
  "runtime_policy": {
    "limit": 2,
    "idle_timeout_seconds": 900,
    "poll_interval_milliseconds": 500,
    "close_timeout_seconds": 10
  },
  "summary": {
    "projects": 3,
    "rooms": 7,
    "runtime_capacity_used": 2,
    "queued_runtimes": 1,
    "attention_items": 2
  },
  "capabilities": {
    "legacy_import": true,
    "runtime_suspend": true,
    "runtime_policy_mutation": false,
    "project_removal": false,
    "room_deletion": false,
    "server_path_browser": false
  }
}
```

新 Management Shell 对旧 snapshot 也有 fallback，缺少 summary/policy/capabilities 时会在浏览器端按现有 Project、Room 和 Runtime 数据计算或显示未知，而不是崩溃。

## 6. 后续优先级

本轮没有扩张后端事实模型。后续更有价值的方向是：

1. 提供真正的 daemon read-only status API，减少页面命令指导与实际运行状态之间的距离；
2. 增加 Runtime 历史指标和最近失败记录，但不把高频遥测写入 Room Event Log；
3. 在正式产品测试中加入多 Project 大数据集、键盘导航和屏幕阅读器回归；
4. 等 Project scope 或 monorepo 子目录成为正式需求后，再扩展 Project Identity，而不是在 UI 层先制造“workspace”别名。

## 7. 主要参考位置

PairRoom：

- https://github.com/sean2077/pairroom/commits/main
- https://github.com/sean2077/pairroom/blob/main/docs/MULTI_ROOM_SERVICE.md
- https://github.com/sean2077/pairroom/blob/main/docs/OPERATIONS.md
- https://github.com/sean2077/pairroom/blob/main/internal/service/management.go
- https://github.com/sean2077/pairroom/blob/main/internal/service/runtime.go
- https://github.com/sean2077/pairroom/tree/main/internal/service/assets
- https://github.com/sean2077/pairroom/blob/main/cmd/pairroom/daemon.go

cc-connect：

- https://github.com/chenhg5/cc-connect/commits/main
- https://github.com/chenhg5/cc-connect/blob/main/web/src/pages/Dashboard.tsx
- https://github.com/chenhg5/cc-connect/blob/main/web/src/pages/Projects/ProjectList.tsx
- https://github.com/chenhg5/cc-connect/blob/main/web/src/pages/Projects/ProjectDetail.tsx
- https://github.com/chenhg5/cc-connect/blob/main/web/src/pages/System/GlobalSettings.tsx
- https://github.com/chenhg5/cc-connect/blob/main/web/src/pages/System/Config.tsx
