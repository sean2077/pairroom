# PairRoom Language

Project-wide terms in `en` and `zh-CN`. Host-specific behavior is defined in [Core concepts](docs/CONCEPTS.md), not inferred from a shared label.

## Workspace and identity

**Project**:
A Service registration of a canonical local Git workspace, not a copied repository.
_Equivalent (zh-CN)_: 项目

**Room**:
A durable two-participant collaboration context belonging to a Project, with immutable host mode, stored collaboration instructions, Bindings, and messages.
_Equivalent (zh-CN)_: 协作房间

**Participant slot**:
One of two stable identities: ActorID `slot1` for Agent 1 or `slot2` for Agent 2, independent of Runtime.
_Equivalent (zh-CN)_: Agent 槽位
_Avoid (en)_: claude/codex slot IDs
_Avoid (zh-CN)_: Claude/Codex 槽位 ID

**Binding**:
The association between a Room participant and its exact identity: an admitted LAN member's remote key, or a native Runtime/session identity for a locally bound participant. Embedded materializes new sessions on accepted execution; Native associates the existing session at bind.
_Equivalent (zh-CN)_: 会话绑定

**Runtime**:
The Claude Code, Codex, Grok Build, or Gemini CLI harness selected for a participant.
_Equivalent (zh-CN)_: 原生运行时

**Room Runtime**:
The active PairRoom component serving a Room. Embedded owns vendor adapters; Native owns a relay engine/listener, not vendor processes. Suspending it does not delete the durable Room.
_Equivalent (zh-CN)_: Room 运行实例

**Turn**:
A native execution interval that may contain multiple model/tool events or accepted inputs. Embedded enforces one owner within a Room; Native does not schedule the original harnesses.
_Equivalent (zh-CN)_: 原生执行回合

## Collaboration

**Collaboration mode**:
The Room's creation-time instruction choice: default or custom, independent of host mode and permissions.
_Equivalent (zh-CN)_: 协作模式

**Lead**:
Agent 1's default planning/review responsibility, not a permission profile, mandatory delegation step, or mention handle.
_Equivalent (zh-CN)_: 主导者

**Executor**:
Agent 2's default implementation, verification, and technical-feedback responsibility.
_Equivalent (zh-CN)_: 执行者

**Mention handle**:
The public runtime-derived `@` identifier, with stable `0/1` suffixes when both slots use the same Runtime.
_Equivalent (zh-CN)_: 点名句柄
_Avoid (en)_: slot alias
_Avoid (zh-CN)_: 槽位别名

**Agent relay**:
Delivery of an Agent's published message to its peer. Automatic publication uses the complete addressed reply at the native response boundary; explicit Native publication uses the command target.
_Equivalent (zh-CN)_: Agent 接力
_Avoid (en)_: handoff
_Avoid (zh-CN)_: 交接

## Configuration

**Permission profile**:
An Embedded participant's effective native-tool policy selection, separate from responsibility. Native Room policy fields are display-only; the original harness owns effective permissions.
_Equivalent (zh-CN)_: 权限配置

**Agent pair profile**:
A named reusable template for both slots' Runtime and configuration selections, optionally the Service default for new Rooms. Creation copies it; later edits do not reconfigure existing Rooms or Native processes.
_Equivalent (zh-CN)_: Agent 组合配置

**Provider**:
The configuration/credential source behind a Runtime: native defaults or a supported read-only CC Switch Profile reference. Native host mode does not apply this selection to an existing process.
_Equivalent (zh-CN)_: Provider

**Provider reference**:
A durable secret-free CC Switch pointer. Its internal `cc-switch:<app_type>/<profile_id>` representation is not a display name.
_Equivalent (zh-CN)_: Provider 引用
_Avoid (en)_: provider string
_Avoid (zh-CN)_: Provider 字符串

**Provider name**:
The credential-redacted display name of a referenced Profile; it never replaces the stored reference.
_Equivalent (zh-CN)_: Provider 名称

## Hosting and transport

**Host mode**:
The immutable Room choice between PairRoom-owned adapters (`embedded`) and user-owned sessions (`native`). A desktop-owned embedded Service can serve either Room mode.
_Equivalent (zh-CN)_: 宿主模式

**Native host mode**:
A Room that relays and audits messages while users retain their original harness sessions, execution, and permissions.
_Equivalent (zh-CN)_: Native 宿主模式

**Park window**:
The bounded interval in which an approved Stop hook waits for inbox work before the native response ends; not an indefinite idle-wake guarantee.
_Equivalent (zh-CN)_: Park 等待窗口

**Foreground exchange**:
One explicit publication followed by collection of the next eligible inbox message in one CLI invocation; not an atomic or correlated request/reply transaction.
_Equivalent (zh-CN)_: 前台交换

**Publication gap**:
An observed jump in a binding generation's report sequence, not an inference from silence.
_Equivalent (zh-CN)_: 发布缺口

**Review version**:
An optional Git evidence observation attached to an explicit Native message; matching observations do not constitute approval or an atomic workspace snapshot.
_Equivalent (zh-CN)_: 评审版本

## LAN collaboration

**LAN sharing**:
A native Room created with `sharing: "lan"` and one local `owner_slot`, hosted on the separate LAN listener for exactly one admitted remote member; only the host needs a Service.
_Equivalent (zh-CN)_: 局域网共享

**Awaiting peer**:
The reserved other slot of a LAN Room before admission (`awaiting_peer`): no Runtime, session or mention handle is guessed for it, and it is never a reusable pair-template selection.
_Equivalent (zh-CN)_: 等待对方加入

**Remote key**:
The admitted member's public certificate fingerprint on a LAN member binding (`remote_key`); it identifies the member's machine, never a vendor session identity.
_Equivalent (zh-CN)_: 对方密钥

**Join request and receipt**:
A LAN invitation permits only a request to join; the host owner admits the exact request/key receipt supplied through a trusted channel, and that admitted key becomes the member binding's generation.
_Equivalent (zh-CN)_: 加入请求与加入回执

**Joined Room**:
The guest machine's local record of a Room it joined (`joined_rooms`): its membership, generation and own private key, usable through CLI/hooks with no local Service.
_Equivalent (zh-CN)_: 已加入的 Room

**Leave**:
Ends the guest's membership with the hosting Room; the host records the end durably.
_Equivalent (zh-CN)_: 退出

**Detach locally**:
Retires the local joined-Room record and its identity reservation without claiming the host received a leave.
_Equivalent (zh-CN)_: 本机断开
