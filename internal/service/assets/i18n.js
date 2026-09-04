(() => {
  'use strict';

  const STORAGE_KEY = 'pairroom.lang';
  const ZH_TO_EN = {
    '今天': 'Today',
    '昨天': 'Yesterday',
    '回复': 'Reply',
    '线程': 'Thread',
    '过程': 'Activity',
    '打断': 'Interrupt',
    '图片': 'Image',
    '失败': 'Failed',
    '停止': 'Stop',
    '复制': 'Copy',
    '拒绝': 'Reject',
    '文件': 'File',
    '重启': 'Restart',
    '启动': 'Start',
    '重试': 'Retry',
    '已排队': 'Queued',
    '处理中': 'Working',
    '已取消': 'Cancelled',
    '已停止': 'Stopped',
    '已跳过': 'Skipped',
    '已完成': 'Completed',
    '写入者': 'Writer',
    '已启动': 'Started',
    '发送中': 'Sending',
    '已重启': 'Restarted',
    '收起消息': 'Collapse',
    '等待处理': 'Waiting',
    '关闭通知': 'Disable notifications',
    '上传中…': 'Uploading…',
    '尚未生成': 'Not yet created',
    '平级协作': 'Peer collaboration',
    '图片消息': 'Image message',
    '重试中…': 'Retrying…',
    '正在输入': 'Typing',
    '复制失败': 'Copy failed',
    '提交回答': 'Submit answers',
    '处理失败': 'Processing failed',
    '允许一次': 'Allow once',
    '打开链接': 'Open link',
    '紧凑交接': 'Compact handoff',
    '加载图片…': 'Loading image…',
    '实时工作区': 'Live workspace',
    '请输入回答': 'Enter an answer',
    '消息已复制': 'Message copied',
    '替代旧指令': 'Supersede',
    '已请求打断': 'Interrupt requested',
    '待发送图片': 'Pending images',
    '本会话允许': 'Allow for this session',
    '展开完整消息': 'Expand message',
    '独立审查快照': 'Independent review snapshot',
    '启用桌面通知': 'Enable desktop notifications',
    '重新加载房间': 'Reload room',
    '含未提交改动': 'Has uncommitted changes',
    '无法加载房间': 'Unable to load room',
    '桌面通知未启用': 'Desktop notifications are off',
    '审批决定已提交': 'Approval submitted',
    '无法访问剪贴板': 'Clipboard is unavailable',
    '下一 Turn': 'Next turn',
    '已拒绝原生请求': 'Native request rejected',
    '轮次限制已保存': 'Turn limits saved',
    '桌面通知已启用': 'Desktop notifications enabled',
    '已被新指令取代': 'Superseded by a newer instruction',
    '查看完整原生请求': 'View full native request',
    '其他回答（可选）': 'Other answer (optional)',
    '读取 Diff…': 'Reading diff…',
    '批准执行当前计划': 'Approve executing the current plan',
    '已复制完整 ID': 'Copied full ID',
    '已开始新 Turn': 'Started a new turn',
    '正在加载协作时间线': 'Loading the collaboration timeline',
    '请等待图片上传完成': 'Wait for image uploads to finish',
    '仅查看这条讨论线程': 'View only this thread',
    '正在加载更早消息…': 'Loading earlier messages…',
    '图片已复制到剪贴板': 'Image copied to clipboard',
    '已注入当前 Turn': 'Injected into the current turn',
    '仅发送给 Codex': 'Send only to Codex',
    '浏览器不允许写入剪贴板': 'The browser blocked clipboard writes',
    'Driver · 实现': 'Driver · implement',
    'Peer · 平级讨论': 'Peer · equal discussion',
    '仅发送给 Claude': 'Send only to Claude',
    '请回答所有问题后再提交': 'Answer every question before submitting',
    '图片上传完成后才能发送': 'Send after image uploads finish',
    '当前浏览器不支持桌面通知': 'This browser does not support desktop notifications',
    '复制 Thread ID': 'Copy Thread ID',
    '引用的消息当前被筛选隐藏': 'The quoted message is hidden by the current filter',
    '当前浏览器不支持复制图片': 'This browser cannot copy images',
    '等待启动原生 Agent': 'Waiting to start the native agent',
    '请重试或移除上传失败的图片': 'Retry or remove images that failed to upload',
    '复制 Session ID': 'Copy Session ID',
    '桌面通知已启用，再次点击管理': 'Desktop notifications enabled; click again to manage them',
    '该消息暂时没有持久化工作摘要。': 'This message does not yet have a durable work summary.',
    'Reviewer · 独立审查': 'Reviewer · independent review',
    '原生保护 · Plan mode': 'Native protection · Plan mode',
    '本条消息的图片总大小不能超过 20 MiB': 'Images on this message cannot exceed 20 MiB total',
    '复制完整 ID，用于 resume 原生会话': 'Copy the full ID to resume the native session',
    '原生保护 · Read-only sandbox': 'Native protection · Read-only sandbox',
    '当前阶段正在等待你的选择；直接回复房间即可继续同一阶段。': 'The current stage is waiting for your choice; reply in the room to continue it.',
    '浏览器会话无效；请从 PairRoom 启动输出中的完整地址重新打开。': 'The browser session is invalid. Reopen the full URL from PairRoom startup output.',
    '原生 Session/Thread ID 将在首次被接受的 Turn 后生成': 'The native Session/Thread ID is created after the first accepted turn',
    '该消息仍在 Room FIFO 中；只移除这一项，不会打断任何原生 Turn': 'This message is still in the Room FIFO; removing it will not interrupt a native turn',
    '该角色按 Codex 当前 sandbox policy 工作，可能修改工作区。': 'This role uses the current Codex sandbox policy and may modify the workspace.',
    'Claude 发出了交互问题，但请求中没有可解析的问题列表。为安全起见只能拒绝。': 'Claude asked an interactive question without a parseable list; it can only be rejected.',
    'Agent 的 Turn、工具调用、命令、计划、Diff 和运行日志会显示在这里。': 'Agent turns, tool calls, commands, plans, diffs, and logs appear here.',
    '长时间无可观察事件；静默本身不等于停滞，请在 Inspector 判断后再打断或重试': 'No observable events for a while. Silence is not a stall by itself; inspect before interrupting or retrying.',
    '该角色按 Claude Code 当前 permission mode 工作，可能修改工作区。': 'This role uses the current Claude Code permission mode and may modify the workspace.',
    '浏览器会话或本地 Service 连接失败。从 PairRoom 启动输出中的完整地址重新打开，或重试。': 'The browser session or local Service connection failed. Reopen the full URL from PairRoom startup output, or retry.',
    '当前没有待处理审批。Claude 的工具权限/交互问题与 Codex 的命令、文件和权限请求都会显示在这里。': 'No pending approvals. Claude tool/permission prompts and Codex command, file, and permission requests appear here.',
    '没有匹配消息': 'No matching messages',
    '修改搜索、消息筛选或退出线程视图即可恢复完整时间线。': 'Change the search or filter, or leave the thread view, to restore the full timeline.',
    '该输入已进入原生 Runtime；取消可能中断该参与者当前整个 Turn，但不会删除 Room FIFO 中尚未提交的后续消息': 'This input is already in the native runtime. Cancelling may interrupt this participant’s current turn, but will not delete later unsubmitted Room FIFO items.',
    '开始一次三方协作': 'Start a three-party collaboration',
    '向 Claude Code 与 Codex 同时提出任务。它们保留各自原生 Harness，并在这个公共房间讨论；你可以随时插话或改变方向。': 'Give both agents a task. They keep their native harnesses and discuss in this shared room; you can interrupt or redirect at any time.',
    '空闲': 'Idle',
    '参与者': 'Participants',
    '轮次策略': 'Turn policy',
    '工作区': 'Workspace',
    '发送': 'Send',
    '关闭': 'Off',
    '保存轮次限制': 'Save turn limits',
    '退出 Room': 'Leave Room',
    '退出 Room；不会停止 Agent': 'Leave the Room; this does not stop either Agent',
    '切换主题': 'Toggle theme',
    '刷新房间': 'Refresh room',
    '筛选时间线': 'Filter timeline',
    '全部消息': 'All messages',
    '仅 Agent 对话': 'Agent conversation only',
    '仅我的消息': 'My messages only',
    '搜索消息': 'Search messages',
    '搜索讨论…': 'Search the discussion…',
    '跳到最新': 'Jump to latest',
    '线程视图': 'Thread view',
    '返回全部讨论': 'Back to all discussion',
    '消息接收者': 'Message recipients',
    '添加图片；也可粘贴或拖入': 'Add images; paste or drop also works',
    '向 Agent 发消息；Enter 发送，Shift+Enter 换行…': 'Message an Agent; Enter sends, Shift+Enter inserts a newline…',
    '发送消息': 'Send message',
    '发送方式': 'Send mode',
    '补充当前讨论': 'Append to the current discussion',
    '下一 Turn（独立任务）': 'Next turn (independent task)',
    '替代并取消旧指令': 'Supersede and cancel the old instruction',
    '发送给当前 Driver': 'Send to the current Driver',
    '释放图片以加入消息': 'Drop images to attach them',
    '工作检查器': 'Work inspector',
    '查看工具、命令、Diff 与审批': 'Inspect tools, commands, diffs, and approvals',
    '全部 Agent': 'All agents',
    '刷新 Diff': 'Refresh diff',
    '清除': 'Clear',
    '选择“刷新 Diff”查看工作区改动。': 'Choose Refresh diff to inspect workspace changes.',
    '读取 Git 状态…': 'Reading Git status…',
    '可随时介入、暂停或改向': 'Intervene, pause, or redirect at any time',
    '你、Claude Code 与 Codex 的公共时间线': 'Shared timeline for you and both agents',
    '单一轮次接力：同一时刻仅一个 Agent 工作；明确 @peer 或 HANDOFF + NEXT 才把下一轮交给对方。': 'Single-owner turns: only one Agent works at a time; an explicit @peer or HANDOFF + NEXT transfers the next turn.',
    '单次接力最大轮数': 'Maximum turns per chain',
    '无事件提醒（秒）': 'Stall warning (seconds)',
    '登录本地控制面': 'Sign in to the local control plane',
    '正在恢复浏览器会话…': 'Restoring the browser session…',
    '显示': 'Show',
    '隐藏': 'Hide',
    '登录 Management Shell': 'Sign in to Management Shell',
    '凭证只用于换取当前浏览器的 HttpOnly Session Cookie，不会写入 Web Storage。': 'The credential is used only to create this browser’s HttpOnly session cookie and is not written to Web Storage.',
    '浏览器会话已过期': 'The browser session expired',
    '补全 Binding': 'Complete bindings',
    '批量归档': 'Batch archive',
    '批量清理': 'Batch delete',
    '永久清除': 'Permanently delete',
    '恢复后才能打开': 'Restore it before opening',
    '概览': 'Overview',
    '设置': 'Settings',
    '正在连接': 'Connecting',
    '退出': 'Sign out',
    '确认': 'Confirm',
    '取消': 'Cancel',
    '打开': 'Open',
    '归档': 'Archive',
    '恢复': 'Restore',
    '重命名': 'Rename',
    '挂起': 'Suspend',
    '激活': 'Activate',
    '复制路径': 'Copy path',
    '尚未登录。请输入 Service Token，或运行 pairroom daemon open。': 'You are not signed in. Enter the Service Token, or run pairroom daemon open.',
    'Service Token 无效，请检查后重试。': 'The Service Token is invalid. Check it and retry.',
    'Management 会话未返回 CSRF 凭证，请重新登录。': 'The Management session did not return a CSRF token. Sign in again.',
    '英文': 'English',
    '中文': 'Chinese'
  };

  function detect() {
    try {
      const saved = localStorage.getItem(STORAGE_KEY);
      if (saved === 'en' || saved === 'zh') return saved;
    } catch (_error) {
      // Language is a presentation preference; a blocked Web Storage read is not fatal.
    }
    const language = String(navigator.language || navigator.userLanguage || '').toLowerCase();
    return language.startsWith('zh') ? 'zh' : 'en';
  }

  let lang = detect();

  function t(zh, en) {
    if (lang !== 'en') return zh || en || '';
    if (en) return en;
    return ZH_TO_EN[zh] || zh || '';
  }

  function setLang(next) {
    lang = next === 'en' ? 'en' : 'zh';
    try {
      localStorage.setItem(STORAGE_KEY, lang);
    } catch (_error) {
      // Persistence is best-effort.
    }
    apply(document);
    document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
    document.dispatchEvent(new CustomEvent('pairroom:lang', { detail: { lang } }));
  }

  function apply(root) {
    const scope = root || document;
    scope.querySelectorAll('[data-i18n]').forEach((el) => {
      const zh = el.getAttribute('data-i18n');
      const translated = t(zh);
      if (el.hasAttribute('data-i18n-placeholder')) {
        el.setAttribute('placeholder', translated);
        return;
      }
      if (el.hasAttribute('data-i18n-title')) {
        el.setAttribute('title', translated);
        if (el.hasAttribute('aria-label')) el.setAttribute('aria-label', translated);
        return;
      }
      el.textContent = translated;
    });
    const toggle = document.getElementById('language-button');
    if (toggle) {
      toggle.textContent = lang === 'en' ? '中' : 'EN';
      toggle.title = lang === 'en' ? t('切换到中文', 'Switch to Chinese') : t('切换到英文', 'Switch to English');
      toggle.setAttribute('aria-label', toggle.title);
    }
  }

  ZH_TO_EN['切换到中文'] = 'Switch to Chinese';
  ZH_TO_EN['切换到英文'] = 'Switch to English';

  window.PairRoomI18n = { get lang() { return lang; }, t, setLang, apply, STORAGE_KEY };
  document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
  document.addEventListener('click', (event) => {
    const button = event.target.closest('#language-button');
    if (!button) return;
    setLang(lang === 'en' ? 'zh' : 'en');
  });
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => apply(document), { once: true });
  } else {
    apply(document);
  }
})();
