# CLI 参考与高级用法

`cc-fleet` 二进制是 skill 背后的引擎。大多数时候由 Claude Code 用自然语言替你驱动,但每个命令也都能直接手动执行。`cc-fleet <cmd> --help` 永远是最权威的参数列表。`ccf` 是 `cc-fleet` 的别名。全局 `--verbose` flag 可对任何命令做步进追踪(输出到 stderr;TUI 则写入 `0600` 日志文件)。

## 命令总览

**Provider 与 key**

| 命令 | 作用 |
|------|------|
| `cc-fleet` | 打开交互式 TUI(provider 中心 + Agents Board 看板)。 |
| `init` | 创建配置目录树,可选添加第一个 provider(附带健康检查)。 |
| `add <provider>` | 注册一个 Anthropic 协议 provider 并探测其 `/v1/models` 端点。 |
| `edit <provider>` | 修改已有 provider 的字段(不探测)。 |
| `remove <provider>` | 删除 provider 及其 profile(`--keep-secret` 保留 key)。 |
| `list` | 列出已配置的 provider 及状态、缓存信息(`--json` 含默认 provider)。 |
| `default [provider]` | 查看 / 设置 / 清除全局默认 provider(`--unset`、`--force`)。 |
| `models <provider>` | 查看 provider 的模型档位(default / strong / fast 槽)。 |
| `refresh <provider>` | 重新查询 `/v1/models` 并更新缓存。 |
| `keyget <provider>` | 输出一次 provider API key——由 Claude 的 `apiKeyHelper` 调用。 |
| `codex add` / `login` / `logout` / `status` | 注册 ChatGPT 订阅 provider + 管理 cc-fleet 自己的 codex 登录(`--credential` 支持多凭证)。 |
| `codex-proxy status` / `stop` | 查看 / 停止本地转换 daemon(按需懒启动;`serve` 为内部命令)。 |

**执行 lane**

| 命令 | 作用 |
|------|------|
| `spawn [provider]` | 把 provider teammate 作为 tmux pane 拉起(仅 unix;省略 provider → 默认)。 |
| `subagent [provider]` | 运行一次性 headless 的 provider subagent(省略 provider → 默认)。 |
| `subagent-status <job>` | 查询后台任务;`--wait` 阻塞直到落定。 |
| `subagent-gc` | 清理已结束的 subagent 任务(`--older-than`、`--session`)。 |
| `run [provider]` | 拉起一个你自己驱动的交互式 provider `claude` 会话。 |
| `workflow …` | JS 编排命令组——见 [Workflows](#workflows)。 |

**舰队运维**

| 命令 | 作用 |
|------|------|
| `ps` | 列出存活的 cc-fleet teammate(`--json`、`--check` 检查 pane 健康)。 |
| `watch` | 以文本流持续输出整个舰队——teammate、任务、run。 |
| `hide` / `show` | 收起 / 恢复 teammate 的 tmux pane,不杀进程。 |
| `teardown <team\|%pane>` | 杀掉 teammate pane 并清理 team 状态。 |
| `doctor` | 健康检查——分 Core 与 Optional;仅 Core 失败才算整体失败。 |
| `repair` | 从 `providers.toml` 重写每个 provider 的 profile JSON。 |
| `refresh-fingerprint` | 通过探针 agent 重新捕获 Claude Code 的 spawn 模板。 |
| `update` | 沿安装渠道自更新二进制 + 刷新插件(`update rollback` 回滚)。 |
| `uninstall` | 重置 cc-fleet 状态(可重装);`--all` 连 skills、插件、二进制一起按安装方式卸载。 |

## 从 CLI 注册 provider

TUI 是最省事的路径,但 Anthropic 协议的注册也能脚本化。key 走 stdin,绝不出现在 argv 或 shell 历史里:

```bash
printf '%s' "$DEEPSEEK_API_KEY" | cc-fleet add deepseek \
  --base-url https://api.deepseek.com/anthropic \
  --models-endpoint https://api.deepseek.com/v1/models \
  --default-model deepseek-v4-flash \
  --secret-backend file --secret-ref deepseek.key --api-key-stdin
```

`add` 会同步探测 models 端点(3 秒),通过才落盘。模型档位相关的可选 flag: `--strong-model` / `--fast-model`(档位槽)、`--effort low|medium|high|xhigh|max`(推理强度)、`--default-permission`(`cc-fleet run` 会话的默认权限档)。之后用 `edit` 可以改这些,外加 `--key-rotation` 与 `--enable`/`--disable`。

**OpenAI 协议与 codex provider 在 TUI 里注册**(添加表单的 OpenAI 组与 CLI-auth 组)——`cc-fleet add` 没有 protocol flag。codex 的 CLI 路径见[Codex](#codex--用-chatgpt-订阅当-provider)。

## 默认 provider 与模型档位

`cc-fleet default <provider>` 设全局默认;此后所有不带 provider 的 `spawn` / `subagent` / `run` / workflow leaf 都解析到它(单独 `default` 查看,`--unset` 清除)。id `claude` 为原生 leaf 保留,永远不能设为默认——`cc-fleet default claude` 会以 `DEFAULT_PROVIDER_RESERVED` 拒绝。模型档位让 Claude 拿到稳定的"把手"而不用硬编码模型 ID:

- `--model strong` / `--model fast` / `--model default` 按档位表解析。
- 每个槽位可标 1M 上下文(`[1m]`),provider 可设 effort 档——TUI 表单或 `add`/`edit` flag 均可配置。

## Subagent —— 一次性 headless 调用

```bash
cc-fleet subagent deepseek --prompt "总结这段日志" --json
```

- `--prompt-file <path>`——大 prompt 或敏感 prompt 用文件传(`-` 读 stdin)。
- `--background`——detached 运行并打印 job id;`cc-fleet subagent-status <job> --wait --timeout 10m` 阻塞到任务落定。退出码:落定 `0`/`1`(按任务 envelope),`3` = leaf 被**held**(操作员挂起——去恢复它,别傻等),`124` = 到时仍未结束(心跳,不是失败), `130` = 被中断。
- `--resume <session_id>`——续接上一次 subagent,多轮工作。
- `--timeout`(默认 300s)/ `--max-turns` / `--max-budget-usd`——限制时长与费用。
- `--profile`——`slim`(默认)镜像原生 subagent 上下文,首请求远小于完整会话 prompt (工具:Bash、Edit、Glob、Grep、Read、Skill、Write);`slim-ro` 是只读镜像(Bash、Glob、Grep、Read、Skill);`full` 恢复完整会话 prompt——仅用于对照行为或排查疑似 slim 回归。
- `--tools` / `--skills` / `--mcp`——细化 slim 运行(与 `--profile full` 同用被拒)。`--tools` 是整组替换而非追加:`--tools WebSearch` 会让 subagent 只剩 WebSearch。`--skills` 是布尔值(默认 true;`--skills=false` 摘掉 Skill 工具)。MCP 默认按 profile 区分——`slim` 继承宿主 MCP 配置,`slim-ro` 走 `--strict-mcp-config`;显式 `--mcp` 一律覆盖。
- `subagent-gc` 清理已结束任务(默认 `--older-than 24h`;`--session <id>` 清一个会话的已结束任务,pin 过的除外)。

无需 tmux、无需 agent-teams——prompt 进,envelope 出。

**保留 leaf `claude`。**`cc-fleet subagent claude`(以及 workflow leaf 的 `provider: "claude"`)用你自己的 Claude Code 登录运行官方 `claude` CLI——没有 provider 行、没有 profile、没有任何 key 材料;子进程环境照常洗净凭证,所以它需要一个真实的本地登录。仅限显式点名:永不自动解析、不出现在 `list` 里,`cc-fleet add claude` 会被拒绝(`PROVIDER_RESERVED`)。`--model` 只接字面模型 id(`opus` / `sonnet` / 完整 id——档位关键词会被拒绝);省略则用你登录的默认档,通常是最贵的那档。它花的是你自己的订阅窗口——留给一两个综合节点,绝不要拿去做大规模扇出。

## Interactive —— 你自己驱动的 provider 会话

```bash
cc-fleet run deepseek                              # 在 deepseek 上开交互式 claude
cc-fleet run deepseek --model strong
cc-fleet run deepseek --dangerously-skip-permissions
```

`cc-fleet run [provider]` 用一个交互式 `claude` REPL 替换当前进程,后端换成该 provider (profile 钉住 `apiKeyHelper` + base URL;模型取 provider 的 `default_model`,`--model` 可覆盖)。与 spawn/subagent 不同,这是**你自己**在用 provider,不是 Claude 委派。

- `--permission-mode <mode>` / `--dangerously-skip-permissions`——会话权限档(互斥)。`run` 直接 exec 二进制,你给 `claude` 配的 shell 别名里的这类 flag 带不过来——在这里传。
- `-- <claude args>`——`--` 之后全部转发给 `claude`。

需要交互式终端。Linux、macOS、Windows 均可用。

## Teammate —— spawn、查看、隐藏、清理

```bash
cc-fleet spawn deepseek --as worker --team squad --json   # 通常由 Claude 执行
cc-fleet ps --json --check                                # 列出 teammate + pane 健康
cc-fleet hide worker@squad                                # 把 pane 收起
cc-fleet show worker@squad                                # 恢复
cc-fleet teardown squad --json                            # 回收 pane + team 状态
```

tmux 里,pane 在你的 lead 旁分屏;不在 tmux 时,teammate 跑在 detached 的 `cc-fleet-swarm-<team>` server 里(`tmux -L cc-fleet-swarm-<team> attach` 进入)。`hide` / `show` 仅限 tmux 内。teammate lane 仅 unix——Windows 上这些命令一律拒绝(`spawn`/`hide`/`show` 返回 `error_code: UNSUPPORTED_ON_WINDOWS`)。

spawn 进阶 flag:`--verify`/`--no-verify`(spawn 后的 settle 校验——仅当本机 Claude Code 比 spawn 配方新时才执行)、`--probe`/`--no-probe`(spawn 前 key 探测,默认开)、`--permission-mode`(不传则继承 lead 的权限档)。

**provider team 的清理顺序:**先 `cc-fleet teardown <team>`(回收 tmux pane 和进程),再原生 `TeamDelete`(它只删 `~/.claude/teams/<team>/`)。只跑 `TeamDelete` 会留下孤儿 provider pane 继续烧 key。

## Workflows

`cc-fleet workflow run <script.js>` 在 **detached 引擎**里执行 JS 编排脚本:`agent()` 的 leaf 就是 provider subagent,`parallel`/`pipeline`/`phase`/`budget` 与 Claude Code 原生 Workflow 工具一致,run 不依赖你的会话存活。完整脚本 API 见**[编写 workflow 脚本 ](workflows.md)**(英文);命令面:

```bash
RUN=$(cc-fleet workflow run audit.js)        # detached;只打印 run id
cc-fleet workflow run audit.js --foreground  # 前台跑(调试用)
cc-fleet workflow status "$RUN" --json       # manifest + 全部 leaf(run → phase → agent)
cc-fleet workflow list --json                # 所有 run,新的在前
cc-fleet workflow watch "$RUN"               # 流式输出事件直到终态
cc-fleet workflow wait "$RUN" --timeout 10m  # 静默阻塞直到 run 落定
cc-fleet workflow stop "$RUN"                # 收掉整个 run
cc-fleet workflow stop "$RUN" --leaf <job|label>     # 挂起一个 leaf(run 继续);--phase 挂整个 phase
cc-fleet workflow restart "$RUN" --leaf <job|label>  # 恢复 held 的 leaf;对已结束的 run 则是按键重放
cc-fleet workflow run audit.js --resume "$RUN"       # journal 重放——完成过的 leaf 直接命中缓存
cc-fleet workflow rm "$RUN" / prune          # 删除一个 run / 清掉所有无引擎的 run
```

- **`wait` 退出码:**`0` done/stopped · `1` failed 或 engine-gone · `3` **parked**(剩下的 leaf 全部 held——需要操作员介入)· `124` 超时(心跳快照,不是结论)· `130` 被中断· `2` IO/未知 run。挂在后台 shell 里,它的退出就是推送通知——不需要任何轮询。envelope 只带 outcome + 状态计数 + 花费;leaf 级细节在 `workflow status` 里。
- **held** 的 leaf(`stop --leaf` 或看板 `x`)无限期挂起——不是错误、不会重试; `restart --leaf` 原地重跑(同一 job id,attempt +1)。
- `run` 的 flag:`--max-concurrency`(默认 `min(16, cores-2)`)、`--budget-usd` / `--budget-tokens`(到顶后引擎不再铸新 leaf)、`--args-json`(脚本的 `args`)、`--no-persist-io`(关闭 prompt/answer 下钻)、`--saved`(跑保存过的脚本)。
- journal 按内容哈希记每个 leaf(provider + 模型 + prompt + schema + profile 形状), `--resume` 只重跑变过或没跑完的;失败的 leaf 永不进 journal。
- `workflow saved` 列出看板里保存过的脚本(`run --saved` 接受的名字); `workflow new <name> --phase <title>…` 铸一个带有序 phase 计划的空 run,用于把 `subagent --run-id/--phase` 任务手动归到同一棵看板树下。

## Codex —— 用 ChatGPT 订阅当 provider

codex provider 用你现有的 ChatGPT/Codex 订阅驱动 gpt-5.x——teammate、subagent、workflow leaf、`run` 全部可用:

```bash
cc-fleet codex add      # 注册 provider(端口 + 默认模型自动选好)
cc-fleet codex login    # 一次性设备码 OAuth(打印 URL + 验证码)
```

`claude` 进程对一个本地回环转换 daemon(`codex-proxy`,懒启动,闲置自退)说 Anthropic API;daemon 翻译成 OpenAI Responses API 调 ChatGPT 后端。OAuth bearer 只存在于 daemon 内部——`keyget` 发给 claude 的只是一个低价值的回环握手 secret,token 绝不进 env、argv 或任何 profile。cc-fleet 维护**自己**的 token 链(`codex login`),从不读写 `~/.codex` 的认证,codex CLI 的登录不受影响。

多份订阅可以共存:`codex add --name codex-work` 再注册一个 provider, `codex login|logout|status --credential <ref>` 独立管理每份凭证。同一个 daemon 还服务 TUI 里注册的 OpenAI 协议 provider(`openai-responses`、`openai-chat`)——每个 provider 一个端口,上游 key 同样的待遇。

> **非官方用法:**在 codex CLI 之外复用订阅可能违反 OpenAI 条款,账号可能被限流或封禁。`codex login` 会先要求明确确认;配额错误会带重置时间一并显示。

## 多 key 与轮换

file 后端的 provider 可以存多把 key(`<provider>.keys.json`,权限 `0600`),每把单独启停,在 TUI 的 key 管理器里维护。`keyget` 是轮换点——策略按 provider 设置:

- `off`——始终用第一把启用的 key。
- `round_robin`——每次拉起 worker 时计数器前进一位。
- `random`——从启用的 key 里随机选。

禁用的 key 在选取前就被过滤。key 在所有地方都打码显示(`sk-…238`);明文只出现在 `keyget` 的 stdout 和密码式输入框里。

## Secret 后端

`--secret-backend` 决定 key 存哪:`file`(默认,`0600` 存于 `~/.config/cc-fleet/secrets/`),或由 `--secret-ref` 指向的外部管理器——`pass`、`1password`、`vault`、系统 `keyring`。非 file 后端的 secret 由你用该后端自己的 CLI 预先配好;cc-fleet 只在 `keyget` 时解析。

## 健康、修复、更新

- `cc-fleet doctor`——健康检查,分 **Core**(配置、二进制、claude、profile、skill 等)与**Optional**(tmux 两项);只有 Core *失败*才让整体翻红(skill 检查只 WARN 不 FAIL)。doctor 从不代修——失败项会打印修复提示。
- `cc-fleet repair`——从 `providers.toml` 重建 provider profile JSON。
- `cc-fleet refresh-fingerprint --probe-team <team>`——CC 升级改了 spawn 模板时重新捕获(skill 会自动触发这个自愈流程)。
- `cc-fleet update`——按安装方式自更新:tarball 安装原地换二进制(校验和验证,留 `.previous` 供 `update rollback`),npm/go 安装交给各自的包管理器;同一趟顺手刷新插件。`--check` 只报告不动手;`--binary-only` 跳过插件刷新。Windows 上不可用——用 npm 或重新下 zip。
- `cc-fleet watch`——整个舰队的只读文本流(teammate + 任务 + run);`--interval`、`--timeout`、`--check`。
- `cc-fleet uninstall`——重置全部配置与状态(file 后端 secret 默认保留,`--wipe-secrets` 连同删除);裸 uninstall 不碰 skills、插件、二进制,之后可直接 `init` 重来。`uninstall --all` 是彻底卸载——skills、插件、最后二进制 + `ccf` 别名,按安装方式路由(npm 装的走 `npm uninstall -g`;进程内删不掉的——以及 Windows 上的一切——打印成手动命令)。`--all` 默认连 secret 一起清,显式 `--keep-secrets` 才保留;会先确认,非交互或 `--json` 调用必须带 `--yes`。

## 文件与路径

| 路径 | 内容 |
|------|------|
| `~/.config/cc-fleet/providers.toml` | provider 定义(权限 `0600`)。 |
| `~/.config/cc-fleet/secrets/` | file 后端的 key(目录 `0700`,key `0600`)。 |
| `~/.config/cc-fleet/subagent-jobs/` | 后台任务元数据 + 结果缓存。 |
| `~/.config/cc-fleet/subagent-jobs/runs/` | workflow run 的 manifest、journal、事件流。 |
| `~/.claude/profiles/` | 生成的各 provider spawn profile。 |
| `~/.claude/teams/<team>/` | 原生 team 状态(由 Claude 管理,cc-fleet 不动)。 |

设置了 `$XDG_CONFIG_HOME` 时,`~/.config/cc-fleet` 基路径随之切换。
