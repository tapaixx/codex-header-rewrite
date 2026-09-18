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
- 中文内嵌 UI；敏感数据接口走 CPA Management API。
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

**手动安装**：下载对应架构的 `.so`，重命名放入插件目录：

```text
plugins/codex-header-rewrite.so
```

直接下载时用同一 Release 的 `codex-header-rewrite-linux-<arch>.so.sha256`
校验，例如：

```bash
sha256sum --check codex-header-rewrite-linux-amd64.so.sha256
```

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

首次打开插件页面时输入 CPA Management Key。Key 只保存在当前标签页的 `sessionStorage`。

## 重要限制

`request.intercept_after` 在 Codex Provider Executor 之前。历史里的“重写后 Request Header”是 interceptor 阶段快照，不是最终 socket transport Header。Codex executor 后续仍可能覆盖 `Authorization`、`User-Agent`、`Content-Type`、`Chatgpt-Account-Id`、`Originator`、`OpenAI-Beta` 等特殊字段。

## 开发

```bash
go mod tidy
go test ./...
go test -tags localtest -modfile=go.localtest.mod ./...
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
