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
- 每个 credential 保留最近 50 条 attempt，10 条/页。
- retry A → B 分别记录；被替换 attempt 标记 `switched`，不猜测 401/429。
- 记录重写前/后的 Request Header 与 upstream Response Header。
- Authorization、Cookie、API key、token/secret/password 等在写盘前永久脱敏。
- 不保存 request / response body。
- 自定义测试请求：预览规则改写结果（不发请求），或用该凭证向 Codex 发一次真实请求。
- 模型一致性核对：记录上游实际声明的模型，与发出的模型比对，不一致时标红。
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
| `codex-header-rewrite-v0.3.0.so` | `codex-header-rewrite` | `0.3.0` |
| `codex-header-rewrite.so` | `codex-header-rewrite` | 空 |
| `codex-header-rewrite-linux-amd64.so` | `codex-header-rewrite-linux-amd64` | 空 |

第三行是个陷阱：Release 里的 `.so` 资产就叫这个名字，**原样丢进插件目录，ID 会变成
`codex-header-rewrite-linux-amd64`**，和商店 `registry.json` 里的 `codex-header-rewrite`
对不上，商店就会一直认为这个插件没装，也不会提示更新。

推荐带版本号安装，宿主不加载插件时也能读到版本：

```bash
sha256sum --check codex-header-rewrite-linux-amd64.so.sha256
sudo install -m 0644 codex-header-rewrite-linux-amd64.so \
  /CLIProxyAPI/plugins/codex-header-rewrite-v0.3.0.so
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

## 商店为什么可能看不到更新

CLIProxyAPI 判断「有更新」要同时满足三件事，任何一条不成立都不会提示：

1. **插件 ID 对得上**。ID 来自插件目录里的文件名（见上表），必须等于 registry 里的
   `codex-header-rewrite`。
2. **已安装版本非空且低于 Release 版本**。已安装版本取自文件名的 `-v<version>` 后缀；
   插件被成功加载时，改用插件注册上报的版本。版本为空时宿主**一律不提示更新**。
3. **安装来源可确认**。手动安装且只有一个商店提供该 ID 时按「假定同源」放行；如果配置里
   记录的来源和当前商店不一致，更新会被禁用。

另外宿主会把「最新 Release 版本」缓存 **1 小时**（GitHub 接口失败时还有退避重试），
所以刚发完版的一段时间内商店仍可能显示旧版本，这是缓存不是故障。

自检命令：

```bash
curl -s -H "Authorization: Bearer <management-key>" \
  http://<cpa-host>/v0/management/plugin-store \
  | jq '.plugins[] | select(.id=="codex-header-rewrite")
        | {installed, installed_version, install_source_status, version, update_available}'
```

`installed=false` 说明文件名导致 ID 对不上；`installed_version` 为空说明文件名没带版本
且插件未成功加载；`install_source_status` 是 `different` 或 `unknown` 说明来源不匹配，
需要先卸载再从商店安装。

## 测试请求

面板的「测试请求」面板可以自定义模型、提示词、推理强度、流式开关、临时 Header、临时移除，以及（高级）Codex 后端路径和原始 JSON 请求体。

两种执行方式：

- **预览 Header**：只在插件内解析，不读凭证、不发请求、不消耗额度，直接看到规则作用后的 Header 差异。
- **发送测试请求**：用该凭证向 Codex 发一次**真实请求**，消耗真实额度，需要二次确认。返回 HTTP 状态、上游 Response Header、延迟、上游实际模型，失败时附一段截断的上游错误文本；可选记入历史（标记为「测试」，与线上请求区分）。

两点边界：

- 测试请求由插件通过宿主直接发往 Codex 后端，**不经过 Codex Provider Executor**。所以它验证的是规则本身是否按预期改写，而线上请求仍可能被 executor 覆盖特殊字段。
- 目标地址被限定在 `https://chatgpt.com/backend-api/codex/` 前缀内，只能改路径。请求携带 bearer token，任意目标地址等于把凭证交给第三方。

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
