# Codex Header Rewrite

<img src="web/icon.svg" width="56" height="56" alt="Codex Header Rewrite">

> CLIProxyAPI 原生插件 · Linux `amd64` / `arm64`

[下载 Release](https://github.com/tapaixx/codex-header-rewrite/releases) ·
[插件商店](https://github.com/tapaixx/CLIProxyAPI-Plugins-Store) ·
[构建状态](https://github.com/tapaixx/codex-header-rewrite/actions)

CLIProxyAPI 原生插件：只处理 **Codex credential**，按 `auth_index` 动态 Set / Override / Remove Request Header，并保存脱敏请求历史。

## 功能

- 使用 `request.intercept_after`，在 credential 选定后按 `selected_auth_index` 应用规则。
- 规则保存后下一请求立即生效，无需重启 CPA。
- bbolt 持久化规则。
- 每个 credential 保留最近 50 条 attempt，每页 10 / 20 / 50 条可选（默认 20）；点击记录从右侧抽屉打开详情，列表不动、所选行保持高亮。详情里 X-Codex-Turn-State 常驻显示，其下按 Request Header / Request Body / Response Header / Response Body 四个标签页分开。
- retry A → B 分别记录；被替换 attempt 标记 `switched`，不猜测 401/429。
- 记录重写前/后的 Request Header 与 upstream Response Header。
- Authorization、API key、token/secret/password 等在写盘前永久脱敏；`Cookie` / `Set-Cookie` 自 v0.20.1 起按明文记录（见「重试会带上会话 Cookie」）。
- 保存 request / response body（v0.21.0），但**会话内容被遮蔽**：请求体的 `input` / `messages` / `system`、响应体的 `output` / `content` / `tools` / `usage` 一律替换为 `[MASKED N bytes]`，Codex Responses 与 Claude Messages 两种格式都覆盖；只留下模型、instructions、reasoning、turn metadata、错误结构这些解释请求本身的字段。单个 body 最多保留 256 KB，超出截断并记录原始大小。
- 自定义测试请求：从凭证可用模型中选择模型、发送默认 `hi` 或自定义 JSON、预览改写结果，或向自定义端点发一次真实请求。
- 模型一致性核对：记录上游实际声明的模型，与发出的模型比对，不一致时标红。
- State 池开关与面板整理（v0.24.0）：「State 池维护」（总开关，默认开，关掉即冻结池子并停下探针与重试）和「注入 State」（默认关）移到 State 池卡片标题栏，与「启用改写」解耦；重试 / 探针代理列表各带「走代理」开关（默认关）；导航栏常驻六盏开关状态灯；邮箱默认脱敏、可用眼睛按钮查看；统一设计 token、圆角与字号刻度，浅色主题全部文字对比度 ≥ 4.5:1。
- 自动 State 探针（v0.23.0）：按凭证定时补池，与「拦截后重试」互斥；可配生效时段、模型、独立代理池、cookie 来源与间隔；探针历史单独保留 500 条。
- 回合状态（X-Codex-Turn-State）溯源：按套餐判定是否降智，合格值按「凭证 + 模型」持久化入池，发现跨账号回带并可按规则摘除；内置 Fernet 信封解码。
- 中文内嵌 UI，单文档零外部依赖；敏感数据接口走 CPA Management API。
- Linux amd64 / arm64 CI 与 tag Release。

## 安装

### 方式一：直接把本仓库作为插件源

仓库根目录的 [`registry.json`](registry.json) 使用 CLIProxyAPI 插件源标准格式，因此不需要先把条目提交到公共插件商店。
在 CPA 配置里直接添加本仓库的 registry：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/tapaixx/codex-header-rewrite/main/registry.json"
  configs:
    codex-header-rewrite:
      enabled: true
      priority: 100
      data_path: "plugins/data/codex-header-rewrite.db"
```

刷新插件商店后即可从这个自定义源看到 **Codex Header Rewrite**。修改 `registry.json` 本身不需要重新发版。

> 安装器下载的 Linux 二进制仍来自本仓库的 latest GitHub Release；`registry.json` 负责“发现插件”，不会从 `main` 源码现场编译动态库。

### 方式二：公共插件商店

CLIProxyAPI 从
[CLIProxyAPI-Plugins-Store](https://github.com/tapaixx/CLIProxyAPI-Plugins-Store)
的 `registry.json` 读到本仓库，再取 latest Release 里与主机平台匹配的 zip：

```text
codex-header-rewrite_<version>_linux_amd64.zip
codex-header-rewrite_<version>_linux_arm64.zip
checksums.txt
```

每个 zip 根目录只有一个 `codex-header-rewrite.so`，由宿主安装器解压并校验。

### 方式三：手动安装

下载对应架构的 `.so`，**必须重命名**后放入插件目录。文件名不是随便起的
—— CLIProxyAPI 直接从文件名解析插件 ID 和已安装版本：

| 插件目录里的文件名 | 宿主解析出的 ID | 宿主解析出的版本 |
|---|---|---|
| `codex-header-rewrite-v0.24.1.so` | `codex-header-rewrite` | `0.24.1` |
| `codex-header-rewrite.so` | `codex-header-rewrite` | 空 |
| `codex-header-rewrite-linux-amd64.so` | `codex-header-rewrite-linux-amd64` | 空 |

第三行是个陷阱：Release 里的 `.so` 资产就叫这个名字，**原样丢进插件目录，ID 会变成
`codex-header-rewrite-linux-amd64`**，和商店 `registry.json` 里的 `codex-header-rewrite`
对不上，商店就会一直认为这个插件没装，也不会提示更新。

推荐带版本号安装，宿主不加载插件时也能读到版本：

```bash
sha256sum --check codex-header-rewrite-linux-amd64.so.sha256
sudo install -m 0644 codex-header-rewrite-linux-amd64.so \
  /CLIProxyAPI/plugins/codex-header-rewrite-v0.24.1.so
```

升级时删掉旧的那个文件，只保留一个 `codex-header-rewrite*.so`。

CPA 配置：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    codex-header-rewrite:
      enabled: true
      priority: 100
      data_path: "plugins/data/codex-header-rewrite.db"
```

面板优先复用宿主管理面板已保存的管理密钥，读不到时才会提示填写；手工填写的 Key 只保存在当前标签页的 `sessionStorage`，不写入插件数据库。

## 商店更新、卸载与实际运行版本

CLIProxyAPI 判断「有更新」要同时满足三件事，任何一条不成立都不会提示：

1. **插件 ID 对得上**。ID 来自插件目录里的文件名（见上表），必须等于 registry 里的
   `codex-header-rewrite`。
2. **已安装版本非空且低于 Release 版本**。已安装版本取自文件名的 `-v<version>` 后缀；
   插件被成功加载时，改用插件注册上报的版本。版本为空时宿主**一律不提示更新**。
3. **安装来源可确认**。手动安装且只有一个商店提供该 ID 时按「假定同源」放行；如果配置里
   记录的来源和当前商店不一致，更新会被禁用。

另外宿主会把「最新 Release 版本」缓存 **1 小时**（GitHub 接口失败时还有退避重试），
所以刚发完版的一段时间内商店仍可能显示旧版本，这是缓存不是故障。

商店安装器会先把新版本写成 `codex-header-rewrite-v<version>.so` 并保存目标版本，随后才在
后台异步热切换插件。因此“安装成功”只说明文件与配置已经写入，不等于这一刻的新库已经开始
响应。插件面板顶栏的 **“运行 vX.Y.Z”** 来自当前实际处理请求的 `.so`，应以它为准；更新后
该值没有变化时，说明热切换尚未完成。

**v0.3.0 及更早版本有一个确定的热更新死锁**：它们没有实现宿主在替换 `.so` 前调用的
`plugin.quiesce`。旧实例继续持有 bbolt 独占文件锁，新实例注册时会一直等待同一个 DB；CPA 的
插件 apply 锁也随之被占住，因此会同时出现“商店显示更新成功但运行版本没变”和“卸载也一直
不完成”。v0.4.0 会在 quiesce 时刷盘并释放 DB 锁，修复后续热更新与卸载。

从已经卡住的 v0.3.0 恢复时，先重启 CPA 解除本次死锁，再从商店安装当前最新版；如果重启后仍
无法由商店替换，就停止 CPA，手动只保留最新的版本化 `.so`，再启动。卸载本身由
CPA 的 `DELETE /v0/management/plugins/{id}` 执行；较旧、不支持热卸载的 CPA 返回
`plugin_delete_requires_restart` 时，仍需停止 CPA 后删除文件并移除对应配置。

自检命令：

```bash
curl -s -H "Authorization: Bearer <management-key>" \
  http://<cpa-host>/v0/management/plugin-store \
  | jq '.plugins[] | select(.id=="codex-header-rewrite")
        | {installed, installed_version, registered, path, install_source_status, version, update_available}'
```

`installed=false` 说明文件名导致 ID 对不上；`installed_version` 为空说明文件名没带版本
且插件未成功加载；`install_source_status` 是 `different` 或 `unknown` 说明来源不匹配，
需要先卸载再从商店安装。

## 自动 State 探针（v0.23.0）

按时补池，而不是等降智了才反应。和「拦截后重试」**双向互斥** —— 两者都在给同一个池补货，同时开就是两个写者抢一个池。两者都与「启用改写」无关。

**任务**（每凭证一个，到期才跑，1 秒全局扫描）

```
开关关 / 不在生效时段 / now − 最近一次真实请求 > 活跃判定时间  → 跳过
集合 = 探针模型中「池里没有 state」或「state 超过 state_ttl_seconds」的
      按紧急度排序：从没入过池的最前，其余按入池时间升序
集合为空 → 结束
依序逐个模型发请求（header / payload 同重试），不降智则 state 连同会话与代理一起入池
                                              每个模型请求写一行探针历史
结束时把下次到期时间设为 now + 间隔
```

「下次 = **结束**时间 + 间隔」而不是固定节拍，所以相邻两次上游调用的间距恒等于你配的间隔。到期时间同时表达「正在跑」和「还没到期」，一个值不可能和自己不一致，所以不需要额外的重入锁。

**活跃判定与 cookie 新鲜度是两个时钟。** 前者只由**真实客户端流量**更新，探针自己的请求**不更新它** —— 否则探针会自己给自己续命，账号闲置几天还在烧额度。

**配置**

| 项 | 说明 |
|---|---|
| 生效时段 | 按浏览器时间填，存 UTC 分钟数；留空全天；**允许跨午夜**（本地工作时段换算成 UTC 后经常就是跨午夜的） |
| 探针模型 | 开启时必填，标签式维护 |
| 探针 SOCKS 代理 | 可选，与重试代理池可互相一键复制（复制是**替换**不是合并） |
| Cookie 来源 | 有代理时**必须显式选**，没有默认值 |
| 活跃判定时间 | 必填，秒 |
| 间隔 | 必填，秒，≥1，**无上限** |
| 解析代理出口 IP | 默认开；动态代理池模式下取不到真实值，留空 |

**Cookie 来源的三种模式不是偏好，是三种出口各自唯一正确的答案** —— Cloudflare 的 `__cf_bm` 绑定获取它的那个 IP，拿一个 IP 的令牌从另一个 IP 发出去比不带更可疑：

| 模式 | 用哪个罐 | 谁喂它 | 上游请求数 |
|---|---|---|---|
| 凭证级 | 直连出口那个罐 | 真实流量 | 1× |
| 静态代理 | 每个代理各一个罐 | 经过该代理的探针响应 | 1× |
| 动态代理池 | 不存 | 同一条连接上先预热再用 | **2×** |

动态代理池之所以能成立：SOCKS5 的出口 IP 在**一条 TCP 隧道的生命周期内必然固定**，所以「预热 + 使用」共用一个 transport 就保证了同一个出口。预热那次拿到的 state **不入池** —— 没带 cookie 拿到的 state 不是这次探针要的那个。

**一个罐只由「从它对应的出口回来的响应」更新**，所以走代理的探针永远不会污染真实流量填的那个罐。

**历史**：探针行**单独一个桶，保留最近 500 条**，在请求历史区域用标签页区分。共用那 50 条的话，间隔几秒的探针一小时内就会把真实请求全冲掉 —— 而那是这个插件唯一的可观测面。预热请求算作**同一行**的一个标记，不单独占一行，这样「500 条」才等于「最近 500 次探针」。行里记出口、入口机房（取自 `Cf-Ray` 尾部的 IATA 码，免费且对该次请求准确）、cookie 模式。

**配额**：面板上直接显示上游在每个响应里报的**真实用量**（`X-Codex-Primary-Used-Percent` 等），保存前另给一个按当前配置的估算。每次探针都是一次计费的真实调用。

## 请求与响应 Body（v0.21.0）

历史详情的抽屉里，**X-Codex-Turn-State**（客户端回带与上游返回的 state 解析）常驻在标签条**上方**，切换标签页时不会消失 —— 它是判断这次请求为什么这样的前提，不该藏在某一页后面。下面四个标签页顺序固定：**Request Header**（改写差异）、**Request Body**、**Response Header**、**Response Body**。标签条是 sticky 的，向下滚动时会钉在抽屉顶部。

**Body 可折叠（v0.21.8）**：格式化后的 JSON 是一棵可折叠的树，用的是原生 `<details>` —— 折叠交给浏览器，所以键盘可达、页内搜索能找到、也不存在一份需要和 DOM 对齐的开合状态。一个响应帧能嵌六层深，而读的人只要其中一支。

- 节点前两层默认展开：帧本身和它的 `response` 对象正是打开这一页要看的东西，把它们折起来等于每次都要点一下才看得到模型。更深处**超过 10 个子项**的节点默认折叠，一个长数组就只占一行 `▸ [ … 42 项 ]`。
- 每个 body 面板右上角有「全部展开 / 全部折叠 / 复制原文」。复制给的是**记录下来的原文**，不是渲染结果 —— 树好读但不好粘。

Request Header 标签页里「未改动的 N 个 Header」默认展开 —— 它折叠是因为从前详情是一条长列、这块压在最底下；现在它独占一个标签页，没有需要跳过的内容了。

**v0.21.5 修复：response body 里 `data:` 之后的 JSON 不格式化。** 显示时原本按 `event:` / `data:` 标记插入换行，但真实 Codex 请求的工具描述里就含有字面量 `` `data:` URL `` —— 换行被插进了 JSON 字符串中间，整条 JSON 从那里断开，后面全都不再格式化。现在改为按括号配对定位每个载荷（与 Go 侧同样的做法，字符串与转义都正确跳过），输入的每一个字节仍然原样出现一次。

详情里的 body 有语法着色：键用强调色、字符串用新增色、数字用覆盖色、布尔与 null 用移除色，被遮蔽的值单独高亮——它是文档里唯一一处不是上游原样内容的字符串。分帧丢失的流在**显示时**会把换行补回去（记录的字节不变）。

**按字段遮蔽**：写盘前替换为 `[MASKED N bytes]`，`N` 是被替换值序列化后的大小。遮蔽是递归的，嵌在任意层级的同名字段一样处理。两个方向遮的字段不同，各自只遮被指定的那些：

请求钩子跑在格式转换**之前**，所以 body 仍然是**客户端自己的格式**；响应侧的 chunk 也是客户端格式。两种格式都可能到达，字段名不同，所以两边都列上（不存在的字段自然不匹配）：

| 方向 | Codex Responses | Claude Messages |
|---|---|---|
| 请求体 | `input` | `messages`、`system` |
| 响应体 | `output`、`tools`、`usage` | `content`、`tools`、`usage` |

`tools` 之所以要遮：那是每一轮都重复一遍的同一份工具声明，约 15 KB，知道「带了工具」和把声明再读一遍信息量一样。`usage` 同理：这个插件不做用量核算，而真实响应里的逐项归因比整个帧的其余部分加起来还长。详情里的遮蔽说明会列出当次实际遮掉的字段名。

**响应体按帧遮蔽（v0.21.6）**：只有三类帧因为它们说的话被保留，其余整个载荷替换掉：

| 帧（Codex） | 帧（Claude） | 处理 | 理由 |
|---|---|---|---|
| `response.created` | `message_start` | 保留，按字段遮蔽后 | 模型在这里；Codex 另有 reasoning / service_tier |
| 终局帧（`response.completed` / `done` / `failed` / `incomplete` / `cancelled`） | `message_delta` | 保留，按字段遮蔽后 | 终局声明是模型判定优先采用的那条，还带停止原因 |
| `error` / `response.error` | `error` | 保留 | 上游报错原文，短且是唯一的事故记录 |
| 其余全部 | `content_block_*`、`message_stop`、`ping` | 载荷换成 `{"type":…,"masked":"N bytes"}` | `in_progress` 只是把 created 重复一遍；`output_text.delta` / `content_block_delta` 是正在流出的答案 |

这同时堵掉了按字段名遮蔽碰不到的地方：`delta` 字段带的也是输出正文，但它不叫 `output`。现在整帧都不留。

帧的类型优先读载荷自己的 `type`，读不到才回退到 `event:` 行；类型识别不出来的帧按字段名遮蔽而不是整块丢弃——它有可能是唯一带着声明的那一个。`event:` 行、`[DONE]` 与帧结构保持原样，所以遮蔽后的流仍然能被模型观测器正常解析（有测试同时校验模型和 effort）。

**载荷定位按括号配对，不按行**：宿主发来的流可能完全没有换行，按行扫描在那种 body 里一个 `data:` 都匹配不到 —— 那样等于把答案的每个碎片原样存了下来，与遮蔽的目的正好相反。解析不了的 body（非 JSON、非 SSE、被截断的片段）原样保留，绝不半改写。

**体积**：单个 body 最多保留 256 KB（`maxStoredBodyBytes`），超出部分丢弃并在详情里标「已截断」，同时显示原始大小。流式响应先按原样累积（上限 1 MB）再整体遮蔽——按 chunk 遮蔽会让跨 chunk 拆开的帧漏过去。

**存储位置**：body 存在独立的 `history_bodies` bucket 里，按 record ID 索引，**不随历史列表返回**。列表一页要解码桶里全部 50 条来排序，带上 body 就意味着渲染一个根本不显示它们的表格要读几十 MB。抽屉打开时才按 ID 单独取一次（`GET /codex-header-rewrite/history/body`）。body 跟着它所属的记录一起被淘汰，`清空历史`、删除凭证数据也会一并删掉。

装这个版本之前的旧记录没有 body，详情里显示「没有记录到 Request Body」，不是错误。

## 离线预览

`preview.html` 是把 `web/index.html` 原样打包、只把管理 API 换成内置假数据的单文件，双击就能在浏览器里看，不需要 CPA、凭证或数据库。里面有两个凭证、一条启用的规则、五条历史（注入成功 / 被拦截 + 过期回带 / 重试 / 模型不一致 / 429 失败）、两条池内 state，以及带遮蔽和截断标记的 Request / Response Body。

面板本身没有任何一处为预览做特判——预览渲染出来的就是插件渲染出来的。出错时预览会把错误直接印在页面底部，而不是静静地空着。改完面板重新生成：

```bash
node scripts/make-preview.mjs
```

## 测试请求

「测试请求」放在「Header 规则」页内、默认折叠，与规则编辑共用同一个 Header 差异预览；点开即用。它可以从当前凭证的可用模型里选择模型（宿主未返回列表时回退为手填），设置提示词、推理强度、流式开关、临时 Header、临时移除、自定义端点与原始 JSON 请求体。默认提示词是 `hi`，自动生成 Responses API JSON 请求体；填写原始 JSON 后会按原文发送。

两种执行方式：

- **预览 Header**：只在插件内解析，不读凭证、不发请求、不消耗额度，直接看到规则作用后的 Header 差异。
- **发送测试请求**：向指定端点发一次**真实请求**，需要二次确认。返回 HTTP 状态、上游 Response Header、延迟、上游实际模型，失败时附一段截断的上游错误文本；可选记入历史（标记为「测试」，与线上请求区分）。

两点边界：

- 插件调用 CPA 的 `host.http.do`，所以 socket、代理配置与请求观测都由 CPA 宿主负责；但它**不经过 Codex Provider Executor**。插件保留 JSON/SSE 必需的 `Content-Type` 与 `Accept`，不再自造 `User-Agent`；其余只带凭证身份和操作者明确添加的临时 Header。
- 端点可使用任意 `https` 地址，明文 `http` 仅允许 loopback。Codex 后端默认附带所选凭证；其他端点默认不读也不带凭证，只有操作者明确勾选后才发送 access token 与账号 ID。
- 端点不是 Codex 后端时（例如直接打 CPA 自己的网关），插件不发 Codex 身份头，也不附带凭证；目标主机需要什么（比如 CPA API key）用临时 Header 显式加。

## Codex Responses Lite

`X-OpenAI-Internal-Codex-Responses-Lite: true` 不只是一个 Header —— 它选中了一种模式，上游会据此**校验请求体**：

- `reasoning.context` 必须是 `all_turns`
- `parallel_tool_calls` 必须是 `false`

如果操作者在临时 Header 中显式加入这个 Header，而自动生成的请求体不满足上面两条，上游会返回：

```json
{"error":{"message":"X-OpenAI-Internal-Codex-Responses-Lite requires `reasoning.context` to be `all_turns`.",
 "type":"invalid_request_error","param":"reasoning.context","code":"unsupported_value"}}
```

现在插件在**最终出站 Header**（含规则改写后新增的）里检测到 Lite 模式时，会把自动生成的请求体调整为 `reasoning.context=all_turns` 且 `parallel_tool_calls=false`，并在结果里标出「Lite 模式已适配」。`reasoning.effort` 等你设置的值保持不变。

**原始 JSON 请求体不会被改写** —— 那是你手写的内容，插件只提示「Lite 模式：原始请求体按原样发送」，由你决定怎么改。Lite 模式也可以由请求体里的 `client_metadata.ws_request_header_x_openai_internal_codex_responses_lite` 选中，这种写法同样能被识别。

## 模型一致性核对

插件从响应载荷读取 `response.model`、`message.model` 或顶层 `model`，与发出的模型比对。支持完整 SSE（LF/CRLF）、跨块帧及 CPA 不带分隔符的逐事件回调。线上回调可能已经过 CPA 格式转换，其模型字段可能由 CPA 补写，因此这里只能核对响应报告的模型，不能证明底层实际服务模型。

| 显示 | 含义 |
|---|---|
| 一致 | 上游声明的模型与发出的模型相同（忽略大小写） |
| 模型不一致 | 上游声明了另一个模型 |
| 未获取到 | 插件没有读取到模型；可能未提供、未收到或无法解析，不能据此认定上游未声明 |
| 上游声明冲突 | 同一次响应里出现互相矛盾的声明，不猜测哪个为准 |

终局事件（`response.completed` 等）的声明优先于过程中的声明。最外层是 JSON **数组**时会逐个元素读取，事件被包在 `data` 键下时也会往里看一层——这两种形状以前会整体读不到。

**推理强度（v0.21.10 起按 CPA 的口径）**：这一项不再自己解释，而是移植 CPA 的 `thinking.ExtractReasoningEffort` —— 宿主自己记用量时用的就是它，所以面板显示的等级和宿主会记录的是同一个。

请求钩子跑在格式转换之前，body 是客户端自己的协议，五种说法都可能到：

| 来源 | 字段 |
|---|---|
| 模型名后缀 | `model(high)` / `model(16384)` / `model(none)` / `model(auto)` —— **优先级高于 body**，与宿主一致（后缀由执行器在本钩子之后才剥掉） |
| Codex / OpenAI Responses | `reasoning.effort`；`input[]` 里最后一个 `configuration_update` 的 `reasoning.effort` 更优先（会话中途改强度就靠它） |
| Claude Messages | `thinking.type`（`disabled` 压过一切）、`thinking.budget_tokens`、adaptive 时的 `output_config.effort` |
| OpenAI chat-completions | 顶层 `reasoning_effort` |
| Gemini / Antigravity | `generationConfig.thinkingConfig.thinkingLevel` / `thinkingBudget`（含 Google Python SDK 的 snake_case 写法，Antigravity 多一层 `request.`） |

**token 预算按宿主的阈值折算成等级**，而不是原样显示数字，这样不同客户端之间才可比：

| 预算 | 等级 |
|---|---|
| `-1` | auto |
| `0` | none |
| 1–512 | minimal |
| 513–1024 | low |
| 1025–8192 | medium |
| 8193–24576 | high |
| > 24576 | xhigh |

词表是 none / auto / minimal / low / medium / high / xhigh / max。「什么都没说」和「说了 none」是两个不同的答案，不会合并成一个。

请求与响应各自的等级记为 `request_effort` / `upstream_effort`

历史列表里模型名和 effort **都不会被遮住**：模型列的宽度按页面上最宽的那条链实测得出（`syncModelColumn`，与 sticky 偏移一样是测量而非猜测），表格自身的最小宽度跟着它走；视口放不下时由横向滚动让位，而不是裁掉文字。名字超过 520px 上限才退回省略号，完整值在详情里。

**v0.21.1 修复：SSE 分帧整个消失时也能读。** 有的宿主把 `event: response.created` 和 `data: {...}` 直接拼在一起、中间没有换行，按行解析会把整段当成一条「event 行」、里面一个 data 都找不到，结果是这条流里每一次模型声明全部丢失。现在解析不到 data 行时会退回**按括号配对**从每个 `data:` 标记后面取出第一个完整 JSON 值（字符串和转义都正确跳过，所以 `obfuscation` 里的花括号不会提前截断）。只截了一半的值不会被取走，留给下一个 chunk。载荷只在内存里解析，请求体与响应体都不入库。

**v0.20.3 修复：此前流式请求的上游模型恒为「未获取到」。** 观测器只在看到 SSE 的空行分隔符（`\n\n`）时才切出一帧，而 CPA 是**逐事件回调且不带分隔符**的，于是 `event: ...` + `data: ...` 这样的完整一帧永远等不到空行，被一路缓存到超过 64 KiB 后整块丢弃。现在每个 chunk 的尾部都会当场试解析一次：能解析说明本来就是完整的一帧，不能解析说明只是被截断，才继续缓存——跨 chunk 拆帧的行为不变。另外被拦截的降智响应现在也会读模型（chunk 仍然到达插件，只是不下发给客户端）。**v0.21.0 起后台重试也读模型**：它的响应体在内存里解析一次，取出模型名并保留一份遮蔽后的副本，和其他响应体同样的待遇。

> 参考实现说明：sub2api 的 `upstream_model_mismatch` 同样是读响应载荷声明的模型（`response.model` / `message.model` / `modelVersion`）后与发出的模型比对，不是按 Header 判断 —— Header 里没有这个信息。

## X-Codex-Turn-State 注入

上游在响应头里返回 `X-Codex-Turn-State`，客户端在同一回合的下一次请求原样回带。插件先观察、分类；只有不降智的 state 才“铸造”（写入 State 池）。一个 blob 能被复用要同时满足三件事：

1. **同一个凭证** —— 换号之后回带旧号返回的 blob，是只有代理链才会出现的矛盾。
2. **同一个模型** —— 同号但换了模型，blob 属于另一条回合链，上游同样用不了。
3. **还在有效期内** —— 由 `state_ttl_seconds` 决定，按凭证维护，单位秒，默认 **200**。

插件按这三条判定，并在历史里给出结论：

| 显示 | 含义 |
|---|---|
| 回带一致 | 同凭证、同模型，可以复用 |
| 跨账号回带 | 该 blob 来自另一个凭证，详情显示是哪一个 |
| 跨模型回带 | 同凭证但来自另一个模型，详情显示是哪个模型 |
| 回带来源未知 | 没记到来源（超出溯源窗口、未入池或发生在装插件之前）—— 是「未知」，不是「一致」 |
| 已过期 N 分钟 | 信封里的签发时间已超过该凭证的 `state_ttl_seconds` |
| 已摘除 | 注入开关打开，本次回带被判定不可复用并已摘掉 |

**为什么会出现跨号回带**：账号不是客户端选的。CPA 的 `routing.strategy` 默认为 `round-robin`，**按请求**轮换凭证，而 `routing.session-affinity` 默认关闭 —— 同一段对话的相邻两轮很可能由不同账号伺服。客户端只看到一个端点，它只是把上游给它的 `X-Codex-Turn-State` 原样带回来，于是 A 号铸的 state 被发给了 B 号。即使打开 `session-affinity`，CPA 在绑定凭证不可用时仍会自动故障转移（401 / 429 / 冷却），回合链照样换号。跨模型同理：state 绑在铸造它的模型上，会话中途换模型或发生回退后，回带的仍是旧模型的 state。单机直连 Codex 两种都不会发生——那里只有一个账号。

**State 连着它的会话入池（v0.22.0）**：池里的每一条不再只是一个 state，还带着它被铸造时的**会话 Cookie** —— 请求当时带的 Cookie，叠加这次铸造响应里 `Set-Cookie` 的赋值。一个 turn state 属于某个会话，两者分开都没多大用：拿一条 state 配一个上游已经翻过页的会话，和拿另一个凭证的 state 一样是矛盾的。

值存在持久池里（`persistedTurnState.cookie`），重启后随 state 一起恢复；这个字段之前的记录没有，读回来是「没有记录会话」而不是错误。State 池的详情里能看到它。

取值口径说明：只取 `Set-Cookie` 赋的那几个会漏掉会话的其余部分，而且大多数响应根本不下发 `Set-Cookie`，那样池里绝大部分条目会是空的。所以存的是**铸造那一刻的完整会话**：

| 情况 | 存下来的值 |
|---|---|
| 请求有 cookie，响应轮换了 session | 轮换后的完整会话 |
| 请求有 cookie，响应没有 `Set-Cookie` | 请求那份会话 |
| 请求没 cookie，响应下发了 | 只有响应下发的那些 |
| 两边都没有 | 空，详情显示「没有记录会话」 |

**注入时不会写入 `Cookie` 请求头。** 这条会话是记录，不是覆盖动作：池里那条可能已经过期，用它替掉客户端当前的会话会比不写更糟。它的用处是事后能看出某条 state 当时属于哪个会话。

**State 有效期（v0.20.0）**：`state_ttl_seconds` 是 `headerRule` 的字段，按凭证保存，取值 5–7200 秒，留空（存为 `0`）按默认 200 秒。之前这是编译进去的 1 小时常量，但这个窗口上游从没公开过，而且各账号表现不同，所以改成面板里可填的数字。两个凭证可以各填各的，同一条 blob 的年龄按当时服务这次请求的那条规则判定。超出范围的值保存时直接报错，不会被悄悄改写成别的数——否则面板上显示的就不是你填的那个窗口了。

窗口决定的不是「是否注入」：**过期的 state 照样会注入**，因为窗口是经验值。它决定的是什么时候把「注入后仍降智」当成这条 state 已经失效的证据（见下文）。把它调短，失效的 state 就更快被清出池子；调长则更保守。

**两个开关**，都在「请求历史 › State 池」卡片的标题栏上，改了即存：

- `maintain_state_pool`（**State 池维护**，默认开启）是池子的总开关。关掉即冻结：响应里的 state 只分类记录、不入池，不注入，自动探针和拦截后重试也停下；已有记录原样保留。早于该字段保存的规则读作开启。
- `inject_turn_state`（**注入 State**，默认关闭，只在维护开着时生效）。开启时插件从 State 池取「该凭证 + 当前模型」的合格 state 写入请求（池里没有就不写），并摘除确认来自其他凭证或其他模型的回带值；关闭时完全不碰这个 Header。早先以 `strip_foreign_turn_state` 保存的规则在读取时会折算成这个开关。

注入与「启用改写」无关：规则关着照样注入。规则**启用**且手工设置或移除了该 Header 时以手工为准；规则关着时它的 Set / Remove 一律不生效，也不拦注入。

关闭「启用改写」后，设置/覆盖和移除编辑区置灰且不可编辑，已有值保留；重新打开后可编辑，点击「保存规则」生效。响应侧拦截、后台重试和探针仍由各自开关控制。

**摘除的边界**：规则里的「不可复用时移除 X-Codex-Turn-State」只摘**跨号**和**跨模型**这两种确定不可复用的情况；**过期只提示、不摘除** —— 那个窗口是经验值不是文档约定，猜错会把本来还能用的回合链打断。来源未知时也不动它。

### 降智响应拦截

「请求历史」页的「拦截降智响应」按凭证保存（`reject_degraded_response`），独立于「启用改写」。拦截模型列表 `reject_degraded_models` 留空时匹配全部模型；非空时只按实际发出的模型 ID 精确匹配，不匹配客户端别名或模型前缀。仅当开关启用、模型命中且上游 state 判定为**疑似降智**时，这次响应才**不下发给客户端**：非流式替换为错误对象；流式第一个数据 chunk 换成终止性的 `error` 事件，之后全部丢弃。响应加上 `X-Codex-Header-Rewrite: rejected-degraded-turn-state`，历史里标「已拦截」。开启「拦截后重试」时，拦截的响应会**等重试跑完再下发**（最多 25 秒）：先返回错误，客户端会立刻自己重试，而那时池子还没补上，重来一次仍是降智。

开启拦截后才能配置「拦截后重试取 state」「最大重试次数」和模型列表。后台重试使用同凭证、同模型发送最小 `hi` 请求，最多 1–5 次（默认 2），获得合格 state 后提前结束，并将整轮重试记录为一行历史。重试范围与拦截范围一致；关闭拦截会保留配置值，但不再触发新的重试。后台重试不会重新执行原始用户请求。

**重试 SOCKS 代理（v0.14.0）**：开启拦截与重试后可配置 `retry_proxies`，每行一个 `socks5://host:port` 或 `socks5h://user:password@host:port`，用户名/密码中的特殊字符需 URL 编码。列表按凭证持久化，去空白与重复项；每次重试独立随机选择，允许连续选中相同代理。列表旁有「走代理」开关（`retry_proxy_enabled`，默认关）：关着或列表为空时**直接连接**，不继承 CPA、凭证或环境变量代理；列表在关着时仍会保存。探针的列表同理（`probe_proxy_enabled`）。面板把每条代理显示为可编辑标签，未聚焦时密码以 `***` 掩去，点进标签才显示原值；保存时若某行仍带掩码则保留已存值，不会把掩码写回。代理失败不回退直连，下一次重试重新随机选取。

当前 CPA 的 `host.http.do` 不支持逐请求覆盖代理，因此**仅后台取 state 的补发改由插件自身的 HTTP/1.1 传输发送**，不使用 CPA 的指纹处理；正常业务请求与手动测试路径不变。补发不跟随重定向，只读响应头并关闭响应体，单次超时 30 秒，插件卸载/重载会取消在途补发。只有 2xx 响应中的合格 state 才能入池；代理密码不会写入请求历史或连接错误，但代理配置本身含明文认证信息，应保护管理端访问及数据文件。

两个必须知道的边界：插件**改不了 HTTP 状态码**，所以客户端看到的是 200 里带着错误（流式客户端把 `error` 事件当作失败处理，这是它们本来就支持的路径；非流式客户端如何对待 200 + error 对象取决于客户端）；被拦截的请求**上游已经计了额度**，拦截省下的是一次降智的回答，不是这次调用。

### 不降智 State 池

每个新收到的 state 都先按凭证套餐分类：Team 套餐长度 **≤ 332** 字符、个人套餐（非 Team，如 Pro / Plus）长度 **≤ 292** 字符是不降智；超限值只留在历史并标为疑似降智。阈值只分 Team 与非 Team 两档，所以插件不必穷举个人套餐的名字，没见过的套餐名一律按 292 判。凭证完全没有套餐声明时既不判定也不入池：按短阈值猜会把合格的 Team state 误判成降智，按长阈值猜会把降智的个人 state 放进池子。合格 state 每拿到一次就铸造一次，同一「凭证 + 模型」只保留时间最新的一条；乱序到达不会让旧值覆盖新值。

凭证选择器直接显示每个凭证的套餐（Team / Pro / Plus…）：`/credentials` 列表在读取邮箱的同一次文档读取里一并解析 `chatgpt_plan_type`（id_token 优先，缺失时回退 access_token 里的同名 claim），所以不需要等该凭证跑过流量。仍显示「套餐未知」说明凭证文档里确实没有套餐声明，属于异常，值得检查该 auth 文件。

数据来自 `GET /codex-header-rewrite/turn-states`。面板同时显示凭证名称、稳定的 `auth_index`、模型、长度/阈值，点击任一行从右侧抽屉查看详情：判定字段、就地解码的 Fernet 信封（版本、签发时间、字节数、结构验证、入池来源）以及原始值。池把原始 state 持久化在配置的 bbolt `data_path`（CPA 工作目录为 `/CLIProxyAPI` 时，默认落在 `/CLIProxyAPI/plugins/data/codex-header-rewrite.db`），因此 CPA 重建、重启或插件更新后会恢复；插件目录随容器更新被整体替换时，应把 `/CLIProxyAPI/plugins/data` 挂到持久卷。

### 自动注入

池里有合格 state 时，插件可以直接把它写进出站请求的 `X-Codex-Turn-State`，这样这一轮就走在一个已知不降智的回合状态上。注入有四道门，缺一不注入：

1. **该凭证的「启用改写」开关是开着的** —— 注入属于改写的一部分，开关关着的凭证按客户端原样透传，插件一个字节都不动。
2. **凭证与模型都对得上** —— 只用同一「凭证 + 模型」池里的那条，跨号或跨模型的一律不用。
3. **套餐已知且该 state 合格** —— 套餐读不到、或长度超过该套餐阈值（疑似降智）都不注入。
4. **你没有在规则里手工写过这个 Header** —— 规则里「设置/覆盖」里钉了值，以你钉的为准；规则里「移除」了它，就保持移除，不会被池悄悄填回去。

**注入后的回执会反过来校验池子**：如果注入的那条 state 在注入时已经**超过该凭证的 `state_ttl_seconds`**，而这次上游铸回来的仍是**疑似降智**的 state，说明这条旧 state 已经带不动回合链了——它会立刻从池中删除（内存和 bbolt 一起），历史里标「注入后仍降智 · 已失效」，下一次请求不再注入它，让上游重新签发。窗口内的 state 出现同样结果时不动它：一次降智回合不足以否定一条新鲜的 state。若这期间池里已经换成了更新的 state，只删注入的那条，不误伤新的。

**重试会带上会话 Cookie（v0.20.1）**：重试要在上游看来是同一个调用方，而 Cookie 是原请求里唯一承载这层会话身份的东西。重试发出的 `Cookie` = **被拦截请求带的 Cookie**，再叠加**这次被拦截响应里 `Set-Cookie` 的赋值**——被拦下的那次响应本身就可能轮换或下发会话 cookie，只回放请求里那份旧的，等于用一个上游已经翻过页的会话去问。

合并规则：只取 `name=value`，`Path` / `Domain` / `Expires` / `Secure` / `HttpOnly` / `SameSite` 这些只描述浏览器该怎么存，不上线；同名覆盖且保持原位置，新签发的追加在后面；`Set-Cookie` 把值置空或 `Max-Age<=0` 视为删除，直接丢掉而不是回一个空值；同名多次赋值以最后一次为准；cookie 值里含 `=`（比如 base64）不会被截断。HTTP/2 把 `Cookie` 拆成多段时按 `; ` 拼回。请求和响应都没有 cookie 就不带这个头。

**Cookie 在历史里是明文（v0.20.1）**：`Cookie` 与 `Set-Cookie` 不再脱敏——要比对重试和线上请求各自用的是哪个会话，只能看到原值才行。代价要清楚：**数据库文件里就有可用的会话**，`data_path` 下那个 `.db` 的备份或拷贝等同于带走会话，请按凭证文件的标准对待它。`Authorization`、`Proxy-Authorization`、api-key、token/secret/password 等仍然脱敏。

> 顺带修掉一个脱敏漏洞：`redactHeaderValue` 原本会保留第一个空格之前的内容（为了让 `Authorization` 显示成 `Bearer [REDACTED]`），对没有 scheme 的敏感头会漏出第一段。现在只有 `Authorization` / `Proxy-Authorization` 保留 scheme，其余敏感头整体替换为 `[REDACTED]`。

**重试请求与线上请求并不等价**：重试发的是最小请求——只有 `Content-Type` / `Accept` / `Originator` / `Authorization` / `Chatgpt-Account-Id`（以及合并后的 `Cookie`），`tools: []`、提示词 `hi`，不带 `X-Codex-Turn-Metadata`、`X-Codex-Beta-Features`、`X-Openai-Internal-Codex-Responses-Lite`，也不带会话上下文；线上请求这些全都带，请求体可达数 MB。上游据此铸出的 state 长度不同，所以**重试拿到"不降智"不等于该账号已恢复**，详情页现在会把重试实际发出与收到的 Header 一并显示，便于自行比对。

命中注入的记录在历史里标「已注入」，差异视图里也能看到该 Header 是被新增或替换的。客户端本来带了一个不可复用的回带值时，注入会直接替换它 —— 一次响应里不会同时出现"设置"和"移除"同一个 Header 这种自相矛盾的指令。

### 信封解码

blob 是 Fernet token，版本号与签发时间在信封里明文可读，**不需要密钥、也不解密密文**。历史详情与解码面板显示同一组字段：摘要、对应模型、Base64 字符数、解码后总字节数、版本号、时间戳（Unix）、签发时间（本地与 UTC）、以及结构是否符合 Fernet（`1+8+16+16n+32`）。两个长度都保留 —— 字符数是线上传输的长度，字节数是信封真正的内容长度，被截断或重新编码过的 blob 只有在两者并排时才看得出来。「Turn-State 解码」面板可以粘贴任意 blob 单独解码，历史详情或 State 池里点「解码」会把原始 blob 带过去。

> 参考实现说明：sub2api / xy2api 按（下游会话 → 最近返回账号）记录，出站时剥离已知异账号的回带值。本插件改为**按 blob 摘要索引来源凭证与模型**，因此不依赖客户端是否带 `session-id`，能指出具体来自哪个凭证、哪个模型，也能识别同号跨模型这种它们不区分的情况。

## 重要限制

`request.intercept_after` 在 Codex Provider Executor 之前。历史里的“重写后 Request Header”是 interceptor 阶段快照，不是最终 socket transport Header。Codex executor 后续仍可能覆盖 `Authorization`、`User-Agent`、`Content-Type`、`Chatgpt-Account-Id`、`Originator`、`OpenAI-Beta` 等特殊字段。

## 开发

UI 开发前先阅读 [UI 设计基准与开发规范](docs/ui-design.md)，并对照
[已确认 UI 参考图](docs/ui/approved-ui-reference.svg)。这两份内容是后续继续优化 PC / 手机端 UI 时的设计基线；
普通样式调整不应无意中改变“无侧边栏、高信息密度、快速凭证切换、规则与 Header Diff 同屏”等核心原则。

```bash
go mod tidy
go test ./...
go test -tags localtest -modfile=go.localtest.mod ./...
```

面板是单个内嵌文档，没有构建步骤。改完可以把内联脚本抽出来做语法检查（CI 也会跑同一条）：

```bash
python3 -c "import re,pathlib;pathlib.Path('/tmp/panel.js').write_text(re.findall(r'<script>(.*?)</script>',pathlib.Path('web/index.html').read_text(),re.S)[-1])"
node --check /tmp/panel.js
```

## 发版

CI 是唯一门禁：`go test`、离线测试、`go vet`、c-shared 构建、双架构交叉构建，
以及打包规则本身（用空壳 `.so` 跑一遍 `scripts/package-release.sh` 和
`scripts/package_release_test.sh`）。Release 不重复这些测试，只做构建 →
打包 → 校验自己的产物 → 发布。

发版就是推一个 `v` 前缀 tag：

```bash
git tag v0.1.0
git push origin v0.1.0
```

Release 工作流按 tag 推导版本（去掉 `v`），用
`-ldflags "-X main.pluginVersion=<version>"` 把版本打进 `.so`，
然后发布 7 个资产：两个商店 zip、`checksums.txt`、两个架构 `.so`
和各自的 `.sha256`。

本地复现同一套产物（arm64 需要 `aarch64-linux-gnu-gcc`）：

```bash
make package VERSION=0.1.0
make package-test VERSION=0.1.0
```

打包是可重现的：`SOURCE_DATE_EPOCH` 固定 zip 内的时间戳，相同输入得到相同
zip 校验和。`registry.json` 里的 `version` 只是商店在取不到最新 Release 时的
展示兜底，发版时同步更新即可。
