# PairRoom 富对话与图片设计

## 1. 目标

共享房间回答“你、Claude、Codex 在讨论什么”，Inspector 回答“每个 Agent 实际做了什么”。主时间线必须像 IM 一样易读，同时足以表达代码、表格、截图、图表和审查结论。

## 2. 支持内容

### 文本

- 标题、段落、换行和分隔线；
- 引用；
- 有序、无序和任务列表；
- Markdown 表格；
- 行内代码、粗体、斜体、删除线；
- fenced code block、语言标签和复制按钮；
- `http(s)` / `mailto` 链接；
- `@Driver`、`@Reviewer`、`@claude`、`@codex` 等单目标 mention。

### 会话操作

- 引用回复和跳转原消息；
- 聚焦单个 thread；
- 搜索全文；
- 全部/Agent/用户筛选；
- 复制消息；
- 长消息折叠；
- 从消息筛选关联 Turn/工具/命令；
- 日期分隔和未读/跳到最新。

### 图片

输入方式：

```text
文件选择
拖拽
剪贴板粘贴
Agent 在仓库生成并在最终 Markdown 中引用
```

支持：PNG、JPEG、GIF、WebP；单条消息最多 8 张，单张 5 MiB，总计 20 MiB；最长边不超过 8000 像素，总解码像素不超过 64 MP。

浏览能力：

- 消息内响应式缩略图；
- 名称、格式、尺寸和大小；
- 同消息画廊；
- 全屏灯箱；
- 前后切换；
- 50%–400% 缩放；
- 打开原图。

## 3. 原生 Agent 投递

PairRoom 不只把图显示在网页上。

### Claude

附件转成 Claude 多模态 image content blocks，图片置于文本之前。PairRoom 读取的是媒体库中的不可变副本，不把浏览器 object URL 或仓库临时路径写入消息。

### Codex

附件转成 App Server `localImage` 输入。绝对路径只存在于 PairRoom 与本机 Codex 进程的边界，不进入公共 snapshot、event log 或导出。

## 4. Agent 生成图片

当 Agent 最终回答包含：

```markdown
![测试趋势](docs/test-trend.png)
```

PairRoom 会：

1. 只解析本地相对图片候选；
2. 将路径 canonicalize；
3. 确认真实文件仍在当前仓库内；
4. 拒绝 symlink 逃逸、远程 URL 和主动内容；
5. 导入媒体库的不可变副本；
6. 将安全 Attachment 元数据附到 Agent 公共消息。

图片本身不会因为后续仓库文件被覆盖而悄悄改变。

## 5. 安全渲染

`richtext.js` 使用 DOM 节点构造，不把消息文本交给 `innerHTML` 执行。原始 HTML、script、事件属性和任意协议 URL 不会执行。

远程 Markdown 图片：

```markdown
![remote](https://example.com/pixel.png)
```

默认只显示占位和显式链接，不自动请求，以免通过 Referer/IP/请求时间泄漏房间阅读行为。

附件 API：

- 需要与房间相同的本地认证；
- 返回 `Content-Type`、ETag、`nosniff`；
- CSP 只允许 self/data/blob 图片；
- SVG/HTML 不接受为附件；
- 每次读取重新检查 SHA-256。

## 6. 数据生命周期

上传先创建 durable attachment；消息发送时只引用 opaque ID。

- 未发送的上传可删除；
- 一旦附件被 durable transcript 引用，删除 API 会拒绝；
- 取消上传会中止网络请求并清理浏览器 object URL；
- 导出只含元数据，不含本机绝对路径；
- 备份必须包含 `attachments/`。

## 7. 当前限制

- 不渲染 SVG、PDF、视频和任意文件附件；
- 不自动下载远程图片；
- 不提供图片区域标注/裁剪；
- 不对 GIF 动画逐帧分析；
- Agent 生成图片必须被最终回答明确引用，PairRoom 不扫描整个仓库。

这些限制优先保证会话可读、数据边界清晰和原生 Harness 输入可靠。
