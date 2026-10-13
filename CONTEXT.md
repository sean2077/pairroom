# PairRoom Language

Project-wide terms in `en` and `zh-CN`. Host-specific behavior is defined in [Core concepts](docs/CONCEPTS.md), not inferred from a shared label.

## Workspace and identity

**Project**:
A Service registration of a canonical local Git workspace, not a copied repository. In the planned enterprise Native service the same term names a logical Room scope with no server-local Git workspace; neither mode infers the other's meaning from the term.
_Equivalent (zh-CN)_: 项目

**Room**:
A durable two-participant collaboration context with immutable host mode, stored collaboration instructions, Bindings, and messages. Local Rooms belong to a Project; an enterprise Room may have no logical Project.
_Equivalent (zh-CN)_: 协作房间

**Participant slot**:
One of two stable identities: ActorID `slot1` for Agent 1 or `slot2` for Agent 2, independent of Runtime.
_Equivalent (zh-CN)_: Agent 槽位
_Avoid (en)_: claude/codex slot IDs
_Avoid (zh-CN)_: Claude/Codex 槽位 ID

**Binding**:
The association between a Room participant and its exact identity: an admitted LAN member's remote key, or a native Runtime/session identity for a locally bound participant. Embedded materializes new sessions on accepted execution; Native associates the existing session at bind. New Native Rooms persist the actual Runtime in each Binding generation; replacing the participant starts a new generation.
_Equivalent (zh-CN)_: 会话绑定

**Binding generation**:
The monotonically increasing identity version of a Native slot. Credentials, publications and inbox deliveries refer to a particular generation, so replacing a participant cannot authorize an earlier participant's effects.
_Equivalent (zh-CN)_: 绑定代次

**Runtime**:
The Claude Code, Codex, Grok Build, or Gemini CLI harness used by a participant. Embedded selects it at creation; new Native Rooms detect it from the actual session when the participant joins.
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
The public `@` identifier used to address a participant. Native v9 uses stable `@agent1` and `@agent2` slot handles. Existing Native v8 and Embedded use runtime-derived handles, with stable `0/1` suffixes when both slots use the same Runtime.
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
A named reusable template for both slots' Runtime and configuration selections, optionally the Service default for new Embedded Rooms. Embedded creation copies it; new Native Rooms do not consume it. Later edits do not reconfigure existing Rooms or Native processes.
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
The immutable Room choice between user-owned sessions (`native`, the default for new Rooms) and PairRoom-owned adapters (`embedded`). A desktop-owned embedded Service can serve either Room mode.
_Equivalent (zh-CN)_: 宿主模式

**Native host mode**:
A Room that relays and audits messages while users retain their original harness sessions, execution, and permissions.
_Equivalent (zh-CN)_: Native 宿主模式

**Park window**:
The bounded interval in which an approved response hook (`Stop`, or Gemini `AfterAgent`) waits for inbox work before the native response ends; not an indefinite idle-wake guarantee.
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

The following terms describe the existing LAN mode. Enterprise authority uses the separate definitions below and does not reinterpret LAN membership keys.

**LAN sharing**:
A native Room created with `sharing: "lan"` and one local `owner_slot`, hosted on the separate LAN listener for exactly one admitted remote member; only the host needs a Service.
_Equivalent (zh-CN)_: 局域网共享

**Awaiting peer**:
The reserved other slot of a LAN Room before admission (`awaiting_peer`): no Runtime or session is guessed for it, and it is never a reusable pair-template selection. Native v9 still exposes its stable slot handle.
_Equivalent (zh-CN)_: 等待对方加入

**Awaiting binding**:
A vacant local slot in a new Native Room (`awaiting_binding`), with a stable slot handle and no selected Runtime, session or harness capability.
_Equivalent (zh-CN)_: 等待绑定

**Remote key**:
The admitted guest's public-key fingerprint on a LAN member binding (`remote_key`), authenticated by its per-Room client certificate. It identifies that membership key, not a global machine identity or a vendor session.
_Equivalent (zh-CN)_: 对方密钥

**Join request and receipt**:
A LAN invitation permits only a request to join; the host owner admits the exact request/key receipt supplied through a trusted channel. Approval binds that key to the Room's reserved slot and membership generation.
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

## Enterprise identity and authority

These terms belong to the approved enterprise service design; definitions alone do not claim the service or its acceptance gates are complete.

**Organization**:
The single configured identity and resource scope of an enterprise deployment. Sharing an Organization does not grant access to another User's Rooms.
_Equivalent (zh-CN)_: 组织

**User**:
A stable internal human principal mapped from a configured OIDC connection and exact issuer/subject. Email, display name and an identity provider's role claim do not establish application authority.
_Equivalent (zh-CN)_: 用户

**Admission**:
An explicit application decision permitting an authenticated identity, device or Room member to enter its named scope. Authentication alone is not admission, and Room admission discloses full-history visibility.
_Equivalent (zh-CN)_: 准入

**Device registration**:
An application-authorized endpoint installation and OS-user context belonging to a User, without a claim of hardware attestation.
_Equivalent (zh-CN)_: 设备注册

**Agent instance**:
A stable native-session record within one Device registration, identified remotely by an opaque source key. The raw vendor session identity remains on the endpoint, and Runtime is metadata rather than authority.
_Equivalent (zh-CN)_: Agent 实例

**Browser session**:
PairRoom's revocable authenticated browser authority for one deployment and User. It is distinct from the identity provider's tokens, device grants and Binding grants.
_Equivalent (zh-CN)_: 浏览器会话

**Device grant**:
Application-issued CLI authority bound to the deployment, trusted origin, User, Device and an explicit scope, after browser-approved enrollment.
_Equivalent (zh-CN)_: 设备授权

**Binding grant**:
Narrow enterprise authority for one Room, slot, Binding and generation. Current User, Device, membership and Binding state are checked before operations or receipt disclosure.
_Equivalent (zh-CN)_: 绑定授权

**Ownership pending recovery**:
The state of a Room whose Owner is disabled: ownership and history remain, members retain only their existing ACL, and ordinary Owner actions are unavailable. Re-enabling the User restores only untransferred ownership, while revoked credentials stay revoked.
_Equivalent (zh-CN)_: 所有权待恢复

**Governance recovery**:
The separately authorized, freshly reauthenticated administrator action that transfers a disabled Owner's Room to an existing eligible active Member, with atomic audit and no administrator content access.
_Equivalent (zh-CN)_: 治理恢复
