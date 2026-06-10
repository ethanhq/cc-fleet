# CLI 参考与高级用法

`cc-fleet` 二进制 CLI 是 skill 背后的引擎。大多数时候你让 Claude Code 用自然语言驱动它，但每个命令
也都能直接手动执行。运行 `cc-fleet <cmd> --help` 查看完整的参数列表。`ccf` 是 `cc-fleet` 的别名。

## 命令总览

| 命令 | 作用 |
|------|------|
| `cc-fleet` | 打开交互式 TUI（Model Providers 中心 + Agents Board 看板）。 |
| `init` | 创建配置目录树，可选添加第一个 provider（会执行健康检查）。 |
| `add <provider>` | 注册一个 provider 并探测其 `/v1/models` 端点。 |
| `edit <provider>` | 修改已有 provider 的字段。 |
| `remove <provider>` | 删除一个 provider 及其 profile（可选同时删除 secret）。 |
| `list` | 列出已配置的 provider 及状态和缓存信息。 |
| `models <provider>` | 列出某 provider 缓存的模型。 |
| `refresh <provider>` | 重新查询某 provider 的 `/v1/models` 并更新缓存。 |
| `keyget <provider>` | 取出 provider API key —— 由 Claude 的 `apiKeyHelper` 内部调用。 |
| `spawn <provider>` | 将 provider teammate 作为 tmux pane 拉起（Claude 层调用）。 |
| `subagent <provider>` | 运行一次性 headless 的 provider subagent。 |
| `run <provider>` | 在前台拉起一个交互式、后端为该 provider 的 `claude` 会话（你自己驱动）。 |
| `ps` | 列出存活的 cc-fleet teammate（`--json`、`--check` 查看健康状态）。 |
| `hide` / `show` | 隐藏 / 恢复某 teammate 的 tmux pane，不终止进程。 |
| `teardown <team\|%pane>` | 杀掉 teammate pane 并清理 team 状态。 |
| `doctor` | 执行健康检查（`--fix` 尝试安全修复）。 |
| `repair` | 从 `providers.toml` 重写每个 provider 的 profile JSON。 |
| `refresh-fingerprint` | 通过探针 agent 重新捕获 Claude Code 的 spawn 模板。 |
| `uninstall` | 删除所有 cc-fleet 配置和缓存状态（不影响二进制文件）。 |

## 从 CLI 注册 provider

TUI 是最便捷的方式，但你也可以脚本化注册。通过 stdin 传入 key，确保它不出现在 argv 或 shell 历史中：

```bash
printf '%s' "$DEEPSEEK_API_KEY" | cc-fleet add deepseek \
  --base-url https://api.deepseek.com/anthropic \
  --models-endpoint https://api.deepseek.com/v1/models \
  --default-model deepseek-chat \
  --secret-backend file --secret-ref deepseek.key --api-key-stdin
```

## Subagent —— 一次性 headless 调用

```bash
cc-fleet subagent deepseek --model deepseek-chat --prompt "总结这段日志" --json
```

- `--prompt-file <path>` —— 适用于较长或敏感的 prompt。
- `--background` —— detached 运行；用 `cc-fleet subagent-status` 轮询进度。
- `--resume <session_id>` —— 续接上一次 subagent，进行多轮对话。
- `--timeout` / `--max-turns` / `--max-budget-usd` —— 限制运行时长和费用上限。
- `--profile` —— `slim`（默认）镜像原生 subagent 上下文，首个请求远小于完整会话 prompt
  （工具集：Bash, Edit, Glob, Grep, Read, Skill, Write）；`slim-ro` 为只读镜像（Bash, Glob,
  Grep, Read, Skill）；`full` 恢复完整会话 prompt —— 仅用于与完整会话对比行为，或排查疑似
  slim 回归。
- `--tools` / `--skills` / `--mcp` —— 细化 slim 运行（与 `--profile full` 同时使用会被拒绝）。
  `--tools` 是整组替换而非追加：白名单之外的工具（如 WebSearch / WebFetch）必须显式列出，
  且 `--tools WebSearch` 会让 subagent 只剩 WebSearch 一个工具。MCP 默认按 profile 区分：
  `slim` 继承宿主 MCP 配置，`slim-ro` 走 `--strict-mcp-config`；显式传 `--mcp`（无论真假）
  均覆盖默认。

不需要 tmux，也不需要 agent-teams —— 纯 stdout 输入，结果输出。

## Interactive —— 用第三方模型跑你自己的会话

```bash
cc-fleet run deepseek                              # 在 deepseek 上开交互式 claude
cc-fleet run deepseek --model deepseek-reasoner
cc-fleet run deepseek --dangerously-skip-permissions
```

`cc-fleet run <provider>` 用一个交互式 `claude` 会话替换当前进程，后端 LLM 换成该 provider（profile 钉住
`apiKeyHelper` + base URL；模型取 provider 的 `default_model`，`--model` 可覆盖）。和 spawn / subagent
不同，这是**你自己**在用第三方模型，而非 Claude 委派。无需 tmux、无需 agent-teams，一个终端即可。

- `--permission-mode <mode>` / `--dangerously-skip-permissions` —— 设定会话的权限模式（二者互斥）。
  它直接 exec 二进制，所以你 `claude` 的 shell 别名里带的这类 flag 不会生效 —— 在这里传。
- `-- <claude args>` —— `--` 之后的内容都转发给 `claude`。

需要交互式终端；仅 macOS / Linux。

## Teammate —— spawn、查看、隐藏、清理

```bash
cc-fleet spawn deepseek --as worker --team squad --json   # 通常由 Claude 自动执行
cc-fleet ps --json --check                                # 列出 teammate 及健康状态
cc-fleet hide worker@squad                                # 将 pane 收起
cc-fleet show worker@squad                                # 将 pane 恢复
cc-fleet teardown squad --json                            # 回收 pane 并清理 team 状态
```

在 tmux 里，pane 在你的 leader 旁边分屏；不在 tmux 时，teammate 跑在 detached 的
`cc-fleet-swarm-<team>` server 里（通过 `tmux -L cc-fleet-swarm-<team> attach` 进入查看）。
`hide` / `show` 仅限 tmux 环境内使用。

**provider team 的清理顺序：** 先执行 `cc-fleet teardown <team>`（回收 tmux pane 和进程），
再执行原生 `TeamDelete`（它只删除 `~/.claude/teams/<team>/`）。单独执行 `TeamDelete` 会留下
孤儿 provider pane 继续消耗 key 配额。

## 多 key 与轮换

文件后端的 provider 可以存放多把 API key（`<provider>.keys.json`，权限 `0600`），每把可单独
启用/禁用，在 TUI 的 key 管理器里统一管理。`keyget` 是轮换的触发点 —— 策略按 provider 单独设置：

- `off` —— 始终使用第一把已启用的 key。
- `round_robin` —— 每次 spawn worker 时计数器递增一位。
- `random` —— 从已启用的 key 中随机选取。

禁用的 key 在选取前会被过滤掉。key 在所有界面和日志中均以打码形式显示（`sk-…238`）；
明文仅出现在 `keyget` 的 stdout 和密码式输入框中。

## Secret 后端

`--secret-backend` 决定 key 的存储位置：`file`（默认，存于 `~/.config/cc-fleet/secrets/`，权限 `0600`），
或由 `--secret-ref` 指向的外部管理器（1Password、Vault、keyring）。非 file 后端的 secret
由你通过该后端自己的 CLI 预先配置；cc-fleet 只在 `keyget` 时进行解析。

## 健康检查与修复

- `cc-fleet doctor` 执行健康检查；`--fix` 尝试安全修复。
- `cc-fleet repair` 从 `providers.toml` 重建 provider profile JSON。
- `cc-fleet refresh-fingerprint` 在 CC 升级导致 spawn 模板变化时重新捕获。

## 文件与路径

| 路径 | 内容 |
|------|------|
| `~/.config/cc-fleet/providers.toml` | provider 定义（权限 `0600`）。 |
| `~/.config/cc-fleet/secrets/` | 文件后端的 key（目录权限 `0700`，key 权限 `0600`，已加入 gitignore）。 |
| `~/.claude/profiles/` | 生成的各 provider spawn profile。 |
| `~/.claude/teams/<team>/` | 原生 team 状态（由 Claude 管理，cc-fleet 不干预）。 |
