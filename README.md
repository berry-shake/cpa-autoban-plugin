# CPA Codex 429 Autoban Plugin

一个 CLIProxyAPI（CPA）插件：**Codex 凭证收到 429（限流）后自动移出号池，等对应限额窗口刷新后自动放回。**

本仓库是 [ysxk/codex-429-autoban](https://github.com/ysxk/codex-429-autoban) 的二次开发版本，封禁判断和调度逻辑沿用上游。相比上游的改动：

- 改用官方 `github.com/router-for-me/CLIProxyAPI/v8` SDK，不再在 `cpasdk/` 里本地复制一份 ABI/类型定义；
- 协商 RPC schema 到 6，管理接口返回的 JSON 不会再被宿主做 HTML 转义；
- 补上插件 logo，CPA 管理面板侧边栏和插件列表都会显示；
- 状态页重写：从 CPA 管理面板里打开时，直接复用面板记住的管理密钥，不用每次手动填；
- 管理密钥不再以明文存进 `localStorage`，旧版本留下的 `codex429AutobanManagementKey` 会在打开页面时清掉；
- 发布产物按 CPA 插件商店的规范打包（`autoban_<version>_<goos>_<goarch>.zip` + `checksums.txt`）。

插件 ID 从上游的 `codex-429-autoban` 改成了 `autoban`，和官方商店里的上游条目不会撞 ID。从上游版本迁移的步骤见[下文](#从上游-codex-429-autoban-迁移)。

## 它做什么

1. **检测 429**：每次请求完成后，插件读取用量记录。某个 **codex** 凭证收到 429 时，就触发封禁逻辑。
2. **判断封多久**：读取上游返回的 `x-codex-*` 响应头，判断打满的是 **5 小时窗口**还是 **周限额**，再用对应窗口的刷新时间作为解禁时间：
   - 5 小时窗口满了：等 5 小时窗口刷新后解禁；
   - 周窗口满了：等周窗口刷新后解禁；
   - 两个都满了：按较晚的那个（周）解禁。
3. **自动解禁**：CPA 每次选凭证时，插件把还没到解禁时间的凭证从候选里去掉；过了刷新时间就自动放回。
4. **手动放回号池**：如果你在 Codex 侧手动重置了额度或用了重置卡，可以在状态页或通过管理接口立即解除插件的封禁，不必等原来的 `reset-at`。
5. **只管 codex**：非 codex 凭证一律不干预，交给 CPA 原有逻辑处理。

## 怎么判断是 5 小时还是周限额

ChatGPT/Codex 后端在 429 时会返回一组自定义响应头（不是标准的 `x-ratelimit-*`）：

| 响应头 | 含义 |
|---|---|
| `x-codex-primary-window-minutes` | `300` = 5 小时窗口 |
| `x-codex-primary-reset-at` | 5 小时窗口刷新时间（Unix 秒） |
| `x-codex-primary-used-percent` | 5 小时窗口使用率（打满时为 100） |
| `x-codex-secondary-window-minutes` | `10080` = 7 天（周）窗口 |
| `x-codex-secondary-reset-at` | 周窗口刷新时间（Unix 秒） |
| `x-codex-secondary-used-percent` | 周窗口使用率 |

哪个窗口的 `used-percent` 到了 100，就用那个窗口的 `reset-at` 作为解禁时间。都没到 100 时，按窗口大小（300 / 10080 分钟）匹配带了刷新时间的那个窗口。

> 如果 429 响应里没有这些头（比如中间代理返回的伪 429），插件保守地按 5 小时封禁。

## 调度规则

CPA 插件没有定时器钩子，所以解禁是"惰性"的：每次 CPA 选凭证时，插件顺带检查解禁时间到了没有，到了就放回候选，效果等同于定时解禁。

`scheduler.pick` 的具体行为：

- **没有凭证被封**：返回 `Handled: false`，完全交给 CPA 原生路由（轮询策略、会话粘性、prompt cache 复用都不受影响）；
- **全部凭证被封**：同样返回 `Handled: false`，交给 CPA 原生的冷却、故障转移和报错处理；
- **部分凭证被封**：插件只在健康凭证中最高优先级的那一档里挑选。请求带有会话标识时，按加权哈希固定到同一个健康凭证上（保持会话粘性）；没有会话标识时，用平滑加权轮询分摊负载。

> **注意**：CPA 每次选凭证只调用**优先级最高**的那一个 scheduler 插件；它返回 `Handled: false` 时直接走内置路由，不会再轮到优先级更低的 scheduler 插件。所以如果同时启用了其他带 scheduler 能力的插件（比如 access-guard），而本插件的 `priority` 比它低，那么只有"429 后记录封禁"这一步会执行，选凭证时并不会跳过被封的凭证。

## 环境要求

- CLIProxyAPI v8（已在 v8.0.58 上验证），且开启了动态插件支持
- Linux amd64/arm64、macOS amd64/arm64 或 Windows amd64
- Unix 平台上需要启用 cgo 编译的 CPA

## 安装

### 通过插件商店（推荐）

在 CPA 配置里加上 berry-shake 的商店源：

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/berry-shake/CLIProxyAPI-Plugins-Store/main/registry.json
```

然后在管理面板的插件商店里找到 **Codex 429 Autoban**（ID `autoban`）安装，也可以直接调用管理接口：

```bash
curl -X POST -H "Authorization: Bearer $MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"version":"0.2.3"}' \
  http://127.0.0.1:8317/v0/management/plugin-store/autoban/install
```

安装后 CPA 会在 `config.yaml` 里写入类似这样的配置：

```yaml
plugins:
  enabled: true
  configs:
    autoban:
      enabled: true
      store:
        id: autoban
        name: Codex 429 Autoban
        author: ysxk / berry-shake
        version: 0.2.3
        release-tag: v0.2.3
        repository: https://github.com/berry-shake/cpa-autoban-plugin
        install:
          type: github-release
```

### 手动安装

1. 从 Release 下载对应平台的 zip，解压出 `autoban.<so|dylib|dll>`。
2. 放进对应平台的插件目录，例如：

   ```text
   plugins/linux/amd64/autoban.so
   plugins/linux/arm64/autoban.so
   plugins/darwin/amd64/autoban.dylib
   plugins/darwin/arm64/autoban.dylib
   plugins/windows/amd64/autoban.dll
   ```

3. 在 `config.yaml` 里启用：

   ```yaml
   plugins:
     enabled: true
     configs:
       autoban:
         enabled: true
   ```

> 如果 `config.yaml` 里已经有 `store.version`，CPA 只会加载文件名版本和它一致的插件文件。手动换文件时记得同时改 `store.version` 和 `release-tag`。

插件没有自己的配置项。

### 从上游 codex-429-autoban 迁移

两者 ID 不同，CPA 会把它们当成两个插件。装好本插件后，在管理面板里停用或卸载上游的 `codex-429-autoban`，或者把 `plugins.configs.codex-429-autoban.enabled` 改成 `false`。不要两个同时启用：它们各自维护一份封禁列表，都会参与选凭证。

封禁状态只在内存里，迁移后不会带过来。之前被封的凭证再次收到 429 时会被重新封禁。

## 状态页

在 CPA 管理面板的插件菜单里点 **Codex 429 Autoban** 打开，页面地址是：

```text
/v0/resource/plugins/autoban/status
```

- 从管理面板里打开时，页面会复用面板"记住密码"保存的管理密钥（官方面板和 CPAMP 的存储格式都支持）。只使用为当前站点保存的密钥，也不会往面板存储里写回任何内容。
- 直接打开页面、或者保存的密钥被宿主拒绝时，页面会让你输入管理密钥。手动输入的密钥只保存在页面内存里，刷新后需要重新输入。
- 页面列出当前被封禁的凭证、封禁窗口、封禁时间、解禁时间和剩余时长，每 30 秒自动刷新一次（页面在后台时暂停）。
- 每一行都可以单独"放回号池"；"全部放回"需要在 4 秒内再点一次确认。
- 支持 English / 中文，默认跟随浏览器语言，可以在右上角切换，选择记在 `localStorage` 的 `cliproxy-autoban-lang` 里。

## 管理接口

```text
GET  /v0/resource/plugins/autoban/status          状态页外壳，无需鉴权，不含数据
GET  /v0/resource/plugins/autoban/logo.png        插件 logo，无需鉴权
GET  /v0/management/plugins/autoban/bans          需要管理密钥
POST /v0/management/plugins/autoban/unban         需要管理密钥
POST /v0/management/plugins/autoban/unban-all     需要管理密钥
```

`/v0/management` 路由由 CPA 宿主先校验管理密钥（以及远程管理和 IP 封禁规则），通过后才转发给插件。缺少密钥或密钥错误时，宿主直接返回 401/403。

```bash
# 查看当前被插件移出号池的凭证
curl -H "Authorization: Bearer $MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/autoban/bans

# 把单个凭证放回号池（也可以用查询参数 ?auth_id=...）
curl -X POST -H "Authorization: Bearer $MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"auth_id":"<AUTH_ID>"}' \
  http://127.0.0.1:8317/v0/management/plugins/autoban/unban

# 清空插件的全部封禁
curl -X POST -H "Authorization: Bearer $MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/autoban/unban-all
```

`bans` 返回示例：

```json
{
  "plugin": "autoban",
  "version": "0.2.3",
  "now": "2026-11-07T14:21:36Z",
  "count": 1,
  "bans": [
    {
      "auth_id": "codex:apikey:aab191bd99ed",
      "window": "5h",
      "banned_at": "2026-11-07T14:21:35Z",
      "reset_at": "2026-11-07T15:21:19Z",
      "remaining_seconds": 3582
    }
  ]
}
```

`unban` / `unban-all` 返回 `{"ok":true,"auth_id":"...","removed":1,"status":{...}}`，其中 `removed` 是实际解除的条数，`status` 是解除后的 `bans` 结果。

> 和上游的 API 有两处不同：时间字段只保留 UTC RFC3339 格式（去掉了 `*_unix` 字段）；`removed` 改成了整数。

注意：这里清除的只是插件**内存里的封禁状态**。请在确认 Codex 侧额度已经恢复（比如手动重置额度、用了重置卡）之后再操作，否则凭证可能马上再次 429 并被重新封禁。

## 状态说明

- 封禁状态只保存在内存里，CPA 重启后清空（CPA 自身的冷却状态在重启后也会清空，两者一致）。在管理面板里停用再启用插件不会清空封禁状态。
- 手动放回号池只修改插件的内存状态，不会改动 Codex 侧额度，也不会修改 CPA 凭证文件。
- 封禁和解禁都会写日志，关键字是 `autoban:`。

## 本地构建

```bash
go test ./...
CGO_ENABLED=1 go build -buildmode=c-shared \
  -ldflags '-X main.pluginVersion=0.2.3-dev' \
  -o autoban-v0.2.3-dev.dylib .
```

Linux 用 `.so`，Windows 用 `.dll`。c-shared 构建会顺带生成一个 C 头文件，宿主用不到它。

推送 `v*` tag 后，GitHub Actions 会构建五个平台的产物并发布 Release。

## 许可

MIT。原作者 ysxk，二次开发 berry-shake，见 [LICENSE](LICENSE)。
