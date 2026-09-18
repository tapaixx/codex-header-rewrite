# Codex Header Rewrite

> CLIProxyAPI 原生插件 · Linux `amd64` / `arm64`

[下载 Release](https://github.com/tapaixx/codex-header-rewrite/releases) ·
[插件商店](https://github.com/tapaixx/CLIProxyAPI-Plugins-Store) ·
[构建状态](https://github.com/tapaixx/codex-header-rewrite/actions)

CLIProxyAPI 原生插件：只处理 **Codex credential**，按 `auth_index` 动态 Set / Override / Remove Request Header，并保存脱敏请求历史。

## 功能

- 使用 `request.intercept_after`，在 credential 选定后按 `selected_auth_index` 应用规则。
- 规则保存后下一请求立即生效，无需重启 CPA。
- bbolt 持久化规则。
- 每个 credential 保留最近 50 条 attempt，10 条/页；详情在所选记录下方原地展开。
- retry A → B 分别记录；被替换 attempt 标记 `switched`，不猜测 401/429。
- 记录重写前/后的 Request Header 与 upstream Response Header。
- Authorization、Cookie、API key、token/secret/password 等在写盘前永久脱敏。
- 不保存 request / response body。
- 自定义测试请求：从凭证可用模型中选择模型、复用安全的历史 Header 模板、预览改写结果，或向自定义端点发一次真实请求。
- 模型一致性核对：记录上游实际声明的模型，与发出的模型比对，不一致时标红。
- 回合状态（X-Codex-Turn-State）溯源：记录 blob 的铸造凭证，发现跨账号回带并可按规则摘除；内置 Fernet 信封解码。
- 中文内嵌 UI，单文档零外部依赖；敏感数据接口走 CPA Management API。
- Linux amd64 / arm64 CI 与 tag Release。

## 安装

两种方式，产物都来自同一个 tag Release。

**插件商店**：CLIProxyAPI 从
[CLIProxyAPI-Plugins-Store](https://github.com/tapaixx/CLIProxyAPI-Plugins-Store)
的 `registry.json` 读到本仓库，再取最新 Release 里与主机平台匹配的 zip：

```text
codex-header-rewrite_<version>_linux_amd64.zip
codex-header-rewrite_<version>_linux_arm64.zip
checksums.txt
```

每个 zip 根目录只有一个 `codex-header-rewrite.so`，由宿主安装器解压并校验。

**手动安装**：下载对应架构的 `.so`，**必须重命名**后放入插件目录。文件名不是随便起的
—— CLIProxyAPI 直接从文件名解析插件 ID 和已安装版本：

| 插件目录里的文件名 | 宿主解析出的 ID | 宿主解析出的版本 |
|---|---|---|
| `codex-header-rewrite-v0.6.1.so` | `codex-header-rewrite` | `0.6.1` |
| `codex-header-rewrite.so` | `codex-header-rewrite` | 空 |
| `codex-header-rewrite-linux-amd64.so` | `codex-header-rewrite-linux-amd64` | 空 |

第三行是个陷阱：Release 里的 `.so` 资产就叫这个名字，**原样丢进插件目录，ID 会变成
`codex-header-rewrite-linux-amd64`**，和商店 `registry.json` 里的 `codex-header-rewrite`
对不上，商店就会一直认为这个插件没装，也不会提示更新。

推荐带版本号安装，宿主不加载插件时也能读到版本：

```bash
sha256sum --check codex-header-rewrite-linux-amd64.so.sha256
sudo install -m 0644 codex-header-rewrite-linux-amd64.so \
  /CLIProxyAPI/plugins/codex-header-rewrite-v0.6.1.so
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

## 测试请求

面板的「测试请求」可以从当前凭证的可用模型里选择模型（宿主未返回列表时回退为手填），设置提示词、推理强度、流式开关、临时 Header、临时移除、自定义端点与原始 JSON 请求体。Header 模板默认使用插件生成的 Codex 基础 Header，也可载入当前历史页中规则生效前的 Header；凭证、账号 ID、Host、Content-Length 和 hop-by-hop 字段不会进入模板。

两种执行方式：

- **预览 Header**：只在插件内解析，不读凭证、不发请求、不消耗额度，直接看到规则作用后的 Header 差异。
- **发送测试请求**：向指定端点发一次**真实请求**，需要二次确认。返回 HTTP 状态、上游 Response Header、延迟、上游实际模型，失败时附一段截断的上游错误文本；可选记入历史（标记为「测试」，与线上请求区分）。

两点边界：

- 插件调用 CPA 的 `host.http.do`，所以 socket、代理配置与请求观测都由 CPA 宿主负责；但它**不经过 Codex Provider Executor**。Codex 端点额外使用稳定的 HTTP/1.1 Header 顺序并关闭自动压缩注入，这改善 HTTP 线级一致性，但不等于完整复刻 Codex CLI 的 TLS 指纹。
- 端点可使用任意 `https` 地址，明文 `http` 仅允许 loopback。Codex 后端默认附带所选凭证；其他端点默认不读也不带凭证，只有操作者明确勾选后才发送 access token 与账号 ID。
- **端点不是 Codex 后端时（例如直接打 CPA 自己的网关），插件不再拼 Codex 那套 Header**：只发 `Content-Type`、`Accept`、`User-Agent`，不发 `Originator`、不发凭证、不套用 Header 模板（模板选择器会自动停用并说明原因），也不套 Codex 的 HTTP/1.1 线级配置。那台主机会自己构造上游请求，重复一份只会互相打架。它需要什么（比如 CPA 的 API key）用临时 Header 显式加。

## Codex Responses Lite

`X-OpenAI-Internal-Codex-Responses-Lite: true` 不只是一个 Header —— 它选中了一种模式，上游会据此**校验请求体**：

- `reasoning.context` 必须是 `all_turns`
- `parallel_tool_calls` 必须是 `false`

从历史载入 Header 模板时会把真实 Codex 请求里的这个 Header 一起带上，而插件自动生成的请求体原本不满足上面两条，于是上游返回：

```json
{"error":{"message":"X-OpenAI-Internal-Codex-Responses-Lite requires `reasoning.context` to be `all_turns`.",
 "type":"invalid_request_error","param":"reasoning.context","code":"unsupported_value"}}
```

现在插件在**最终出站 Header**（含规则改写后新增的）里检测到 Lite 模式时，会把自动生成的请求体调整为 `reasoning.context=all_turns` 且 `parallel_tool_calls=false`，并在结果里标出「Lite 模式已适配」。`reasoning.effort` 等你设置的值保持不变。

**原始 JSON 请求体不会被改写** —— 那是你手写的内容，插件只提示「Lite 模式：原始请求体按原样发送」，由你决定怎么改。Lite 模式也可以由请求体里的 `client_metadata.ws_request_header_x_openai_internal_codex_responses_lite` 选中，这种写法同样能被识别。

## 模型一致性核对

上游实际服务的模型不在 Response Header 里，而在响应载荷里：流式事件的 `response.model`、非流式响应体的 `model`。插件按 SSE 帧读取载荷、只取出模型名，然后与发往上游的模型比对：

| 显示 | 含义 |
|---|---|
| 一致 | 上游声明的模型与发出的模型相同（忽略大小写） |
| 模型不一致 | 上游声明了另一个模型 |
| 未声明 | 上游一次都没声明模型 —— 是「未知」，不是「一致」 |
| 上游声明冲突 | 同一次响应里出现互相矛盾的声明，不猜测哪个为准 |

终局事件（`response.completed` 等）的声明优先于过程中的声明。载荷只在内存里解析，请求体与响应体都不入库。

> 参考实现说明：sub2api 的 `upstream_model_mismatch` 同样是读响应载荷声明的模型（`response.model` / `message.model` / `modelVersion`）后与发出的模型比对，不是按 Header 判断 —— Header 里没有这个信息。

## 回合状态守卫（X-Codex-Turn-State）

上游在响应头里铸造 `X-Codex-Turn-State`，客户端在同一回合的下一次请求原样回带。一个 blob 能被复用要同时满足三件事：

1. **同一个凭证** —— 换号之后回带旧号铸造的 blob，是只有代理链才会出现的矛盾。
2. **同一个模型** —— 同号但换了模型，blob 属于另一条回合链，上游同样用不了。
3. **还在有效期内** —— 经验窗口约 1 小时（不保证，上游未公开）。

插件按这三条判定，并在历史里给出结论：

| 显示 | 含义 |
|---|---|
| 回带一致 | 同凭证、同模型，可以复用 |
| 跨账号回带 | 该 blob 由另一个凭证铸造，详情显示是哪一个 |
| 跨模型回带 | 同凭证但由另一个模型铸造，详情显示是哪个模型 |
| 回带来源未知 | 没记到铸造方（重启、超出记录窗口或铸造发生在装插件之前）—— 是「未知」，不是「一致」 |
| 已过期 N 分钟 | 信封里的签发时间已超过复用窗口 |
| 已摘除 | 规则开启守卫，本次回带已被摘掉 |

**摘除的边界**：规则里的「不可复用时移除 X-Codex-Turn-State」只摘**跨号**和**跨模型**这两种确定不可复用的情况；**过期只提示、不摘除** —— 那个窗口是经验值不是文档约定，猜错会把本来还能用的回合链打断。来源未知时也不动它。

### 最近回合状态

面板的「最近回合状态」列出每个「凭证 + 模型」最新的一条 blob：摘要、铸造时间、已过多久、是否超出复用窗口。同一对凭证与模型再次铸造时**自动覆盖**旧记录 —— 旧的那条上游已经翻篇了。时间乱序到达时不会用旧的覆盖新的。

数据来自 `GET /codex-header-rewrite/turn-states`，是内存态：只存摘要不存 blob，2 小时过期、512 条上限，CPA 重启即清空。

### 信封解码

blob 是 Fernet token，版本号与铸造时间在信封里明文可读，**不需要密钥、也不解密密文**。历史详情与解码面板显示同一组字段：摘要、铸造模型、Base64 字符数、解码后总字节数、版本号、时间戳（Unix）、签发时间（本地与 UTC）、以及结构是否符合 Fernet（`1+8+16+16n+32`）。两个长度都保留 —— 字符数是线上传输的长度，字节数是信封真正的内容长度，被截断或重新编码过的 blob 只有在两者并排时才看得出来。「Turn-State 解码」面板可以粘贴任意 blob 单独解码，历史详情里点「解码」会把该条的原始 blob 带过去。

> 参考实现说明：sub2api / xy2api 按（下游会话 → 最近铸造账号）记录，出站时剥离已知异账号的回带值。本插件改为**按 blob 摘要索引铸造方与模型**，因此不依赖客户端是否带 `session-id`，能指出具体是哪个凭证、哪个模型铸造的，也能识别同号跨模型这种它们不区分的情况。

## 重要限制

`request.intercept_after` 在 Codex Provider Executor 之前。历史里的“重写后 Request Header”是 interceptor 阶段快照，不是最终 socket transport Header。Codex executor 后续仍可能覆盖 `Authorization`、`User-Agent`、`Content-Type`、`Chatgpt-Account-Id`、`Originator`、`OpenAI-Beta` 等特殊字段。

## 开发

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
