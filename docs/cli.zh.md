# CLI 参考与高级用法

`cc-fleet` 二进制是 skill 背后的引擎。大多数时候你让 Claude Code 用自然语言驱动它,但每个命令
也都能直接用。运行 `cc-fleet <cmd> --help` 查看权威的参数列表。`ccf` 是 `cc-fleet` 的别名。

## 命令总览

| 命令 | 作用 |
|------|------|
| `cc-fleet` | 打开交互式 TUI(vendors 中心 + agent-status 看板)。 |
| `init` | 创建配置目录树,可选添加第一个 vendor(会跑健康检查)。 |
| `add <vendor>` | 注册一个 vendor 并探测其 `/v1/models` 端点。 |
| `edit <vendor>` | 修改已有 vendor 的字段。 |
| `remove <vendor>` | 删除一个 vendor 及其 profile(可选连带 secret)。 |
| `list` | 列出已配置的 vendor 及状态、缓存信息。 |
| `models <vendor>` | 列出某 vendor 缓存的模型。 |
| `refresh <vendor>` | 重新查询某 vendor 的 `/v1/models` 并更新缓存。 |
| `keyget <vendor>` | 取出 vendor API key —— 由 Claude 的 `apiKeyHelper` 内部调用。 |
| `spawn <vendor>` | 把 vendor teammate 拉成一个 tmux pane(Claude 层)。 |
| `subagent <vendor>` | 运行一次性 headless 的 vendor subagent。 |
| `ps` | 列出存活的 cc-fleet teammate(`--json`、`--check` 查健康)。 |
| `hide` / `show` | 隐藏 / 恢复某 teammate 的 tmux pane,不杀进程。 |
| `teardown <team\|%pane>` | 杀掉 teammate pane 并清理 team 状态。 |
| `doctor` | 跑健康检查(`--fix` 尝试安全修复)。 |
| `repair` | 从 `vendors.toml` 重写每个 vendor 的 profile JSON。 |
| `refresh-fingerprint` | 通过一个探针 agent 重新捕获 Claude Code 的 spawn 模板。 |
| `uninstall` | 删除所有 cc-fleet 配置 + 缓存状态(不动二进制)。 |

## 从 CLI 注册 vendor

TUI 是最省事的路径,但你也可以脚本化注册。key 走 stdin,绝不进 argv 或 shell 历史:

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

- `--prompt-file <path>` —— 大的或敏感的 prompt。
- `--background` —— detached 运行;用 `cc-fleet subagent-status` 轮询。
- `--resume <session_id>` —— 续接上一次 subagent 做多轮。
- `--timeout` / `--max-turns` / `--max-budget-usd` —— 给运行时长和成本封顶。

它不需要 tmux、也不需要 agent-teams —— 纯 stdout 进、结果出。

## Teammate —— spawn、查看、隐藏、拆除

```bash
cc-fleet spawn deepseek --as worker --team squad --json   # 通常 Claude 自己做
cc-fleet ps --json --check                                # 列 teammate + 健康
cc-fleet hide worker@squad                                # 把 pane 收起来
cc-fleet show worker@squad                                # 再放回来
cc-fleet teardown squad --json                            # 收 pane + team 状态
```

在 tmux 里,pane 在你的 leader 旁边切分;不在 tmux 时,teammate 跑在 detached 的
`cc-fleet-swarm-<team>` server 里(用 `tmux -L cc-fleet-swarm-<team> attach` 进去看)。
`hide` / `show` 仅限 tmux 内。

**vendor team 的清理顺序:** **先** `cc-fleet teardown <team>`(它收 tmux pane / 进程),
**再**用原生 `TeamDelete`(它只删 `~/.claude/teams/<team>/`)。只跑 `TeamDelete` 会留下孤儿
vendor pane 继续用 key 计费。

## 多 key 与轮换

文件后端的 vendor 可以放多把 API key(`<vendor>.keys.json`,权限 `0600`),每把可单独
启用/禁用,从 TUI 的 key 管理器里管。`keyget` 是轮换点 —— 策略按 vendor 设:

- `off` —— 永远用第一把启用的 key。
- `round_robin` —— 每次 spawn worker 时计数器前进一格。
- `random` —— 随机挑一把启用的 key。

禁用的 key 在挑选前就被过滤掉。key 在所有地方都打码显示(`sk-…238`);明文只会出现在
`keyget` 的 stdout 和密码式输入框里。

## Secret 后端

`--secret-backend` 决定 key 存哪:`file`(默认,`~/.config/cc-fleet/secrets/` 下 `0600`),
或由 `--secret-ref` 指向的外部管理器(1Password、Vault、keyring)。非 file 后端的 secret
由你通过该后端自己的 CLI 提供;cc-fleet 只在 `keyget` 时解析它。

## 健康与修复

- `cc-fleet doctor` 跑检查;`--fix` 尝试安全修复。
- `cc-fleet repair` 从 `vendors.toml` 重建 vendor profile JSON。
- `cc-fleet refresh-fingerprint` 在 CC 升级改了 spawn 模板时重新捕获它。

## 文件与位置

| 路径 | 内容 |
|------|------|
| `~/.config/cc-fleet/vendors.toml` | vendor 定义(权限 `0600`)。 |
| `~/.config/cc-fleet/secrets/` | 文件后端的 key(目录 `0700`、key `0600`,已 gitignore)。 |
| `~/.claude/profiles/` | 生成的各 vendor spawn profile。 |
| `~/.claude/teams/<team>/` | 原生 team 状态(由 Claude 管,不是 cc-fleet)。 |
