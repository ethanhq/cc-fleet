# CLI 参考与高级用法

`cc-fleet` 二进制是 skill 背后的引擎。大多数时候由 Claude Code 用自然语言替你驱动,但每个命令也都能直接手动执行。`cc-fleet <cmd> --help` 始终是最准确的参数列表。`ccf` 是 `cc-fleet` 的别名。全局 `--verbose` flag 可对任何命令做步进追踪(输出到 stderr;TUI 则写入 `0600` 日志文件)。

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
| `export` | 把 provider 名册导出成不含密钥的版本化 TOML 包(`--out`、`--provider`、`--json`)。 |
| `import <bundle>` | 应用 provider 包 —— 整体校验后再合并(`--force` 覆盖并备份;`--json`)。 |
| `models <provider>` | 查看 provider 的模型档位(default / strong / fast 槽)。 |
| `refresh <provider>` | 重新查询 `/v1/models` 并更新缓存。 |
| `keyget <provider>` | 输出一次 provider API key — 由 Claude 的 `apiKeyHelper` 调用。 |
| `codex add` / `login` / `logout` / `status` | 注册 ChatGPT 订阅 provider + 管理 cc-fleet 自己的 codex 登录(`--credential` 支持多凭证)。 |
| `codex-proxy status` / `stop` | 查看 / 停止本地转换 daemon(按需懒启动;`serve` 为内部命令)。 |

**执行 lane**

| 命令 | 作用 |
|------|------|
| `teammate setup` | 一次性:在 Claude Code 原生 agent teams 上开启 provider teammate(启动器 shim、`ccf-*` agent 定义、两个 settings 键)。 |
| `teammate check [provider]` | 检查当前 Claude Code 会话能否起 provider teammate,并输出它的 `agent_type`(省略 provider → 默认)。 |
| `subagent [provider]` | 运行一次性 headless 的 provider subagent(省略 provider → 默认)。 |
| `subagent-status <job>` | 查询后台任务;`--wait` 阻塞直到落定。 |
| `subagent-gc` | 清理已结束的 subagent 任务(`--older-than`、`--session`)。 |
| `run [provider]` | 拉起一个你自己驱动的交互式 provider `claude` 会话。 |
| `workflow …` | JS 编排命令组 — 见 [Workflows](#workflows)。 |

**舰队运维**

| 命令 | 作用 |
|------|------|
| `ps` | 列出 Claude Code 起的 provider teammate,每行带 `state`(`--json`、`--check` 检查 pane 健康)。 |
| `watch` | 以文本流持续输出整个舰队 — teammate、任务、run。 |
| `hide` / `show <%N\|name@team>` | 收起 / 恢复 provider teammate 的 tmux pane,不杀进程。 |
| `teardown <%N\|name@team\|team>` | 逐个复核身份后杀掉 provider teammate;从不改 Claude Code 的 team 文件。 |
| `doctor` | 健康检查 — 分 Core 与 Optional;仅 Core 失败才算整体失败。 |
| `repair` | 从 `providers.toml` 重写每个 provider 的 profile JSON;重新固定 teammate 启动器 shim。 |
| `update` | 沿安装渠道自更新二进制 + 刷新插件(`update rollback` 回滚)。 |
| `uninstall` | 重置 cc-fleet 状态(可重装);`--all` 连 skills、插件、二进制一起按安装方式卸载。 |

**已移除的命令。**`spawn` 和 `refresh-fingerprint` 已删除:队友现在由 Claude Code 自己起,命令行也由它自己拼,没有 spawn 配方可捕获了。两者保留为隐藏的桩命令,不论带什么参数都输出 `{"ok":false,"error_code":"COMMAND_REMOVED",…}` 并以 1 退出。

| 已移除 | 改用 |
|--------|------|
| `cc-fleet spawn <p> --as <name> --team <t>` | `cc-fleet teammate check <p> --json`,然后在 lead 里 `Agent({name, subagent_type: <agent_type>, prompt})`(见 [Teammate](#teammate--原生-agent-teamsprovider-做后端))。spawn 的 flag(`--as`、`--team`、`--model`、`--color`、`--probe`、`--verify`、`--permission-mode` 等)没有替代:队友的命名、配色、布局由 Claude Code 负责,权限档由它传 lead 的;模型用槽位选(`--slot strong` → `ccf-<p>.strong`)。 |
| `cc-fleet refresh-fingerprint [--probe-team <t>]` | 无需刷新。出问题时跑 `cc-fleet doctor`。 |
| 原生 `TeamCreate` / `TeamDelete` | 不需要:每个终端 `claude` 会话自带一个团队,会话退出时由 Claude Code 删除。 |

## 从 CLI 注册 provider

TUI 是最省事的路径,但 Anthropic 协议的注册也能脚本化。key 走 stdin,不会出现在 argv 或 shell 历史里:

```bash
printf '%s' "$DEEPSEEK_API_KEY" | cc-fleet add deepseek \
  --base-url https://api.deepseek.com/anthropic \
  --models-endpoint https://api.deepseek.com/v1/models \
  --default-model deepseek-v4-flash \
  --secret-backend file --secret-ref deepseek.key --api-key-stdin
```

`add` 会同步探测 models 端点(最多 10 秒),通过才落盘。模型档位相关的可选 flag: `--strong-model` / `--fast-model`(档位槽)、`--effort low|medium|high|xhigh|max`(推理强度)、`--default-permission`(`cc-fleet run` 会话的默认权限档)。之后用 `edit` 可以改这些,外加 `--key-rotation` 与 `--enable`/`--disable`。

开启 provider teammate 之后,`add` / `edit` / `remove` 还会同步 `ccf-*` agent 定义。同步失败,或者已有一个不带 cc-fleet 标记的同名 `ccf-*.md`(不会被覆盖)时,命令本身的结果不受影响,`--json` 里会多一个 `teammate_sync_error` 字符串;`cc-fleet repair` 会重试同步,遇到这类文件会打印告警(`--json` 里列在 `agent_defs.conflicts`)。

**OpenAI 协议与 codex provider 在 TUI 里注册**(添加表单的 OpenAI 组与 CLI-auth 组) — `cc-fleet add` 没有 protocol flag。codex 的 CLI 路径见[Codex](#codex--用-chatgpt-订阅当-provider)。

## 默认 provider 与模型档位

`cc-fleet default <provider>` 设全局默认;此后所有不带 provider 的 `teammate check` / `subagent` / `run` / workflow leaf 都解析到它(单独 `default` 查看,`--unset` 清除)。id `claude` 为原生 leaf 保留,不能设为默认 — `cc-fleet default claude` 会以 `PROVIDER_NAME_INVALID` 拒绝。模型档位让 Claude 拿到稳定的"把手"而不用硬编码模型 ID:

- `--model strong` / `--model fast` / `--model default` 按档位表解析。
- 每个槽位可标 1M 上下文(`[1m]`),provider 可设 effort 档 — TUI 表单或 `add`/`edit` flag 均可配置。

## 导出 / 导入 provider 名册

把 provider 名册作为不含密钥的版本化 TOML 包在机器之间搬运 —— 可 review、可 diff、可入私有 dotfiles 仓库。包里**绝不含密钥**。

```bash
cc-fleet export --out fleet-providers.toml          # 全部 provider
cc-fleet export --provider deepseek,glm > roster.toml
cc-fleet import fleet-providers.toml                # 在新机器上应用
```

随包走的:每个 provider 的配置(protocol、base URL、OpenAI-protocol 的 upstream URL、models endpoint、模型档位、effort、默认权限、key 轮换、enabled、secret 后端 + 引用)与全局默认。不走的:**密钥本身**(file 后端的密钥留在源机;`pass`/`1password`/`vault`/`keyring` 只带引用),以及 **codex** provider(其登录是机器本地的 —— 导出时省略、导入时跳过)。

`import` 在写入任何东西之前先整体校验,坏包不会动你的名册。与现有 provider 冲突的行默认跳过,除非 `--force`;`--force` 会先备份 `providers.toml`,再用一次原子写替换。daemon-backed(OpenAI-protocol)provider 的 loopback `base_url` 在目标机重新派生 —— 随包走的是它的 `upstream_url`。导入只写配置、绝不联网;profile 在之后重建(派生缓存 —— 若该步报错,重跑 `repair`)。

> 只导入你信任的包:它的 base URL、models endpoint、secret 引用会驱动后续本地 secret 管理器读取与带密钥的请求。

导入后收尾:

```bash
printf '%s' "$KEY" | cc-fleet edit deepseek --api-key-stdin   # 重新录入 file 后端密钥
cc-fleet codex login                                          # 任何 codex provider
cc-fleet doctor
```

文件里的 `bundle_version` 是包格式版本 —— 刻意与 `providers.toml` 的 `version`(schema 版本)区分开。

## Subagent — 一次性 headless 调用

```bash
cc-fleet subagent deepseek --prompt "总结这段日志" --json
```

- `--prompt-file <path>` — 大 prompt 或敏感 prompt 用文件传(`-` 读 stdin)。
- `--background` — detached 运行并打印 job id;`cc-fleet subagent-status <job> --wait --timeout 10m` 阻塞到任务落定。退出码:落定 `0`/`1`(按任务 envelope),`3` = leaf 被**held**(操作员挂起 — 去恢复它,不必干等),`124` = 到时仍未结束(心跳,不是失败), `130` = 被中断。
- `--resume <session_id>` — 续接上一次 subagent,多轮工作。
- `--timeout`(默认 300s)/ `--max-turns` / `--max-budget-usd` — 限制时长与费用。
- `--profile` — `slim`(默认)镜像原生 subagent 上下文,首请求远小于完整会话 prompt (工具:Bash、Edit、Glob、Grep、Read、Skill、Write);`slim-ro` 是只读镜像(Bash、Glob、Grep、Read、Skill);`full` 恢复完整会话 prompt — 仅用于对照行为或排查疑似 slim 回归。
- `--tools` / `--skills` / `--mcp` — 细化 slim 运行(与 `--profile full` 同用被拒)。`--tools` 是整组替换而非追加:`--tools WebSearch` 会让 subagent 只剩 WebSearch。`--skills` 是布尔值(默认 true;`--skills=false` 摘掉 Skill 工具)。MCP 默认按 profile 区分 — `slim` 继承宿主 MCP 配置,`slim-ro` 走 `--strict-mcp-config`;显式 `--mcp` 一律覆盖。
- `subagent-gc` 清理已结束任务(默认 `--older-than 24h`;`--session <id>` 清一个会话的已结束任务,pin 过的除外)。

无需 tmux、无需 agent-teams — prompt 进,envelope 出。

**保留 leaf `claude`。**`cc-fleet subagent claude`(以及 workflow leaf 的 `provider: "claude"`)用你自己的 Claude Code 登录运行官方 `claude` CLI — 没有 provider 行、没有 profile、没有任何 key 材料;子进程环境照常清掉凭证,所以它需要一个真实的本地登录。仅限显式点名:不会自动解析、不出现在 `list` 里,`cc-fleet add claude` 会被拒绝(`PROVIDER_NAME_INVALID`)。`--model` 只接字面模型 id(`fable` / `opus` / `sonnet` / 完整 id — 档位关键词会被拒绝);省略则用你登录的默认档,通常是最贵的那档。它花的是你自己的订阅窗口 — 留给一两个综合节点,不要拿去做大规模扇出。

## Interactive — 你自己驱动的 provider 会话

```bash
cc-fleet run deepseek                              # 在 deepseek 上开交互式 claude
cc-fleet run deepseek --model strong
cc-fleet run deepseek --dangerously-skip-permissions
```

`cc-fleet run [provider]` 用一个交互式 `claude` REPL 替换当前进程,后端换成该 provider — **省略 provider 时解析到全局默认**(profile 钉住 `apiKeyHelper` + base URL;模型取 provider 的 `default_model`,`--model` 可覆盖)。与 teammate、subagent 不同,这是**你自己**在用 provider,不是 Claude 委派。

- `--permission-mode <mode>` / `--dangerously-skip-permissions` — 会话权限档(互斥)。`run` 直接 exec 二进制,你给 `claude` 配的 shell 别名里的这类 flag 带不过来 — 在这里传。
- `--no-probe` — 跳过启动前的端点协议检查(启动前 `run` 会看 Anthropic 协议 provider 的端点对 `POST /v1/messages` 的应答;回 404 且非 Anthropic 错误体的 — 典型是被当成 Anthropic 协议添加的 OpenAI-only 端点 — 会带修复提示直接拒绝,而不是让 claude 报笼统的 "model may not exist")。
- `-- <claude args>` — `--` 之后全部转发给 `claude`。

需要交互式终端。Linux、macOS、Windows 均可用。

## Teammate — 原生 agent teams,provider 做后端

provider teammate 是 Claude Code 自己用 `Agent` 工具起的队友,agent 类型由 cc-fleet 安装:`ccf-<provider>`,以及 strong / fast 槽位的模型与 default 不同时另有的 `ccf-<provider>.strong` / `.fast`。团队、pane、收件箱、`SendMessage`、权限继承和清理都归 Claude Code;cc-fleet 只负责把 pane 里的 `claude` 路由到 provider。

**适用范围:**在 tmux 或 iTerm2 里运行的终端 `claude`(Claude Code ≥ 2.1.278),并且 `teammateMode` 落到 pane(tmux,或在 tmux/iTerm2 里的 auto)。Claude 桌面 App、`claude -p` 和 SDK 会话没有 agent team,进程内(in-process)队友也无法路由 — 这些场景改用 `subagent` / `workflow`。Windows 上不可用(`teammate`、`hide`、`show`、`teardown` 返回 `UNSUPPORTED_ON_WINDOWS`)。

**一次性配置:**

```bash
cc-fleet teammate setup                          # 列出将要做的改动,不改任何东西(BAD_ARGS)
cc-fleet teammate setup --yes                    # 执行
cc-fleet teammate setup --yes --teammate-mode tmux   # teammateMode 未设置或为 in-process 时一并设为 tmux
cc-fleet teammate setup --remove --yes           # 撤销(保留 shim 文件)
```

`setup --yes` 依次写入:启动器 shim `~/.config/cc-fleet/bin/claude-teammate`(0755)、每个启用的 provider 一个 `~/.claude/agents/ccf-*.md`,以及 `~/.claude/settings.json` 里的 `env.CLAUDE_CODE_TEAMMATE_COMMAND`(指向 shim)和 `env.CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS="1"`(已开启则不动)。`--teammate-mode` 默认 `keep`;`tmux` 不会改动已有的 `auto` / `tmux` / `iterm2`。`CLAUDE_CODE_TEAMMATE_COMMAND` 已指向别的程序时返回 `SETUP_CONFLICT`(`--force` 覆盖);存在没有 cc-fleet 标记的同名 `ccf-*.md` 时也返回 `SETUP_CONFLICT`,这种文件即使带 `--force` 也不覆盖。完成后重启 `claude`(JSON 里的 `restart_required`)。`--remove` 删除启动器设置和 cc-fleet 的定义,并把 lane 标为未启用;shim 保留(运行中的会话仍指向它),`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS` 和 `teammateMode` 保持原样。`settings.json` 就地编辑:键顺序、symlink 和文件权限都保留。

**每个队友(由 skill 完成):**

```bash
cc-fleet teammate check deepseek --slot strong --json   # 在 lead 的 Bash 工具里运行
# → {"ok":true,"protocol":1,"agent_type":"ccf-deepseek.strong","team":"session-7c8f769b","backend_hint":"tmux",…}
```

然后在 lead 里 `Agent({name: "worker-1", subagent_type: "ccf-deepseek.strong", prompt: "…"})`。一定要传 `name`;`ccf-*` 类型不要传 `model`、`isolation`、`cwd` — 插件的 `PreToolUse(Agent)` hook 会拦下这类调用(以及 `check` 会拒绝的任何 `ccf-*` 调用)。`check` 在 `ok` 时以 0 退出,否则以 1 退出,并给出稳定的 `error_code` + `detail`:`TEAMMATE_LANE_UNAVAILABLE`(没有 lead 会话、Claude Code 太旧、会话没有团队 — 桌面 App / `-p` / SDK)、`TEAMMATE_SETUP_REQUIRED`(启动器或定义缺失、被他人占用)、`LEAD_RESTART_REQUIRED`(setup 晚于这个 `claude` 启动)、`TEAMMATE_MODE_IN_PROCESS`、`CLAUDE_NOT_FOUND`,以及与 `subagent` 共用的 provider 错误码。`--no-probe` 跳过 provider 可达性探测。

**查看、收起、结束:**

```bash
cc-fleet ps --json --check                       # 带 state 和 pane 健康的行
cc-fleet hide worker-1@session-7c8f769b          # 把 pane 收进 claude-hidden 会话
cc-fleet show %42 --socket /private/tmp/tmux-501/default
cc-fleet teardown worker-1@session-7c8f769b --json
cc-fleet teardown session-7c8f769b --json        # 该团队的全部 provider teammate
```

- **`ps`** 只列出 cc-fleet 能确认归属的队友:会话团队里 `agentType` 为 `ccf-*`、且 `--settings` 是对应 provider profile 的成员;显示启动器失败行的死 pane;绕过了启动器的 `ccf-*` 队友;0.3.x 的队友(`legacy:true`)。原生队友从不列出,即使它在 provider lead 手下。行字段:`agent_id, name, team, pane_id, provider, model, pid, tmux_socket_path, backend (tmux|in-process|unknown), lead_pid, lead_session_id, state, error_code, hidden, legacy`,带 `--check` 时另有 `status / error_class / detail`。`state` 取值:`running`;`orphaned`(lead 会话已不在);`failed`(启动器拒绝启动,见 `error_code`);`bypassed`(没经过启动器,实际跑在 Claude 上而不是 provider — 用 `TaskStop` 停掉)。表格多一列 `STATE`。
- **破坏性改名:**行字段 `tmux_socket`(`-L` 的 socket 名,在 tmux 内时为空)改为 `tmux_socket_path`(socket 的绝对路径,总是有值)。把 `tmux -L <tmux_socket> …` 改成 `tmux -S <tmux_socket_path> …`,例如 `tmux -S "$path" capture-pane -p -t %42`。
- **目标写法**(`hide`、`show`、`teardown`):pane id `%N`(同一个 pane id 出现在多个 tmux server 上时加 `--socket <tmux_socket_path>`,否则返回 `AMBIGUOUS_TARGET`),或 agent id `name@team`;`teardown` 还接受整个 `team`。0.3.x 的 `team`、`team/member` 写法在 `hide` / `show` 上返回 `BAD_ARGS`。
- **`teardown`** 在击杀前逐个复核身份(pane、精确 argv、进程启动时间),然后杀 pane 并回收进程。输出:`{ok, target, killed:[{agent_id, pane_id, tmux_socket_path, pid}], skipped:[{agent_id, pane_id, reason}], error_code, error_msg, suggestion}`;`reason` 为 `IDENTITY_MISMATCH`(不动它)或 `IN_PROCESS`(在 lead 里用 `TaskStop`)。目标已无可杀的对象时返回 `ok:true` 和空的 `killed`。它从不碰 lead、原生队友和 `~/.claude/teams`。0.3.x 的 `panes`、`members`、`killed_pids`、`team_removed`、`warnings` 字段已删除。
- **`hide` / `show`** 总是输出单个对象 `{ok, action, agent_id, team, name, pane_id, tmux_socket_path, hidden, error_code, error_msg, suggestion}`。原窗口记在 pane 上(tmux 选项 `@ccf_origin`),不写文件。`show` 放回 pane 时不移动焦点:键盘仍在 lead,当前窗口也不变。只支持 tmux pane:detached swarm server 上的队友返回 `SWARM_UNSUPPORTED`,tmux 之外的返回 `BACKEND_UNSUPPORTED`。
- **结束队友:**让它自己关闭(原生 `shutdown_request`),或在 lead 里 `TaskStop`。`teardown` 用于孤儿(lead 崩溃)、0.3.x 遗留 pane,以及用不了 `TaskStop` 的情况。lead 正常退出时,Claude Code 会自己清掉 pane 和团队目录 — 不再有 `TeamDelete` 这一步。

## Workflows

`cc-fleet workflow run <script.js>` 在 **detached 引擎**里执行 JS 编排脚本:`agent()` 的 leaf 就是 provider subagent,`parallel`/`pipeline`/`phase`/`budget` 与 Claude Code 原生 Workflow 工具一致,run 不依赖你的会话存活。完整脚本 API 见**[编写 workflow 脚本 ](workflows.md)**(英文);命令面:

```bash
RUN=$(cc-fleet workflow run audit.js)        # detached;只打印 run id
cc-fleet workflow run audit.js --foreground  # 前台跑(调试用)
cc-fleet workflow status "$RUN" --json       # manifest + 全部 leaf(run → phase → agent)
cc-fleet workflow result "$RUN" --label <leaf> --json  # 读某个完成 leaf 的答案(status/wait 不带答案)
cc-fleet workflow list --json                # 所有 run,新的在前
cc-fleet workflow watch "$RUN"               # 流式输出事件直到终态
cc-fleet workflow wait "$RUN" --timeout 10m  # 静默阻塞直到 run 落定
cc-fleet workflow stop "$RUN"                # 收掉整个 run
cc-fleet workflow stop "$RUN" --leaf <job|label>     # 挂起一个 leaf(run 继续);--phase 挂整个 phase
cc-fleet workflow restart "$RUN" --leaf <job|label>  # 恢复 held 的 leaf;对已结束的 run 则是按键重放
cc-fleet workflow run audit.js --resume "$RUN"       # journal 重放 — 完成过的 leaf 直接命中缓存
cc-fleet workflow rm "$RUN" / prune          # 删除一个 run / 清掉所有无引擎的 run
```

- **`wait` 退出码:**`0` done/stopped · `1` failed 或 engine-gone · `3` **parked**(剩下的 leaf 全部 held — 需要操作员介入)· `124` 超时(心跳快照,不是结论)· `130` 被中断· `2` IO/未知 run。挂在后台 shell 里,它的退出就是推送通知 — 不需要任何轮询。envelope 只带 outcome + 状态计数 + 花费;leaf 级细节在 `workflow status` 里。
- **held** 的 leaf(`stop --leaf` 或看板 `x`)无限期挂起 — 不是错误、不会重试; `restart --leaf` 原地重跑(同一 job id,attempt +1)。
- `run` 的 flag:`--max-concurrency`(默认 `min(16, cores-2)`)、`--budget-usd` / `--budget-tokens`(到顶后引擎不再铸新 leaf)、`--args-json`(脚本的 `args`)、`--no-persist-io`(关闭 prompt/answer 下钻)、`--saved`(跑保存过的脚本)。
- journal 按内容哈希记每个 leaf(provider + 模型 + prompt + schema + profile 形状), `--resume` 只重跑变过或没跑完的;失败的 leaf 不会进 journal。
- `--resume` 和 `restart` 在 run 启动时的目录(记在 manifest 里)运行剩下的 leaf,而不是调用者的当前目录;该目录已不存在时 run 以明确的错误失败。
- `isolation: "worktree"` 的 leaf 留下改动时,会在删除 worktree 之前把改动存成分支 `cc-fleet/wf-<job>-a<attempt>`(见[编写 workflow 脚本](workflows.md#isolated-worktrees))。run 被 stop 或 kill 之后,下一次 `restart` / `--resume` 的清理或 `workflow rm` / `prune` 会把没保存的改动抢救到 `cc-fleet/wf-salvage-*` 分支;存不下来的目录保留并带 `.cc-fleet-keep` 标记,`rm` / `prune` / `restart` 会在 stderr 上为每个保留的目录打一行。cc-fleet 从不删除这些分支。
- `workflow saved` 列出看板里保存过的脚本(`run --saved` 接受的名字); `workflow new <name> --phase <title>…` 铸一个带有序 phase 计划的空 run,用于把 `subagent --run-id/--phase` 任务手动归到同一棵看板树下。

## Codex — 用 ChatGPT 订阅当 provider

codex provider 用你现有的 ChatGPT/Codex 订阅驱动 gpt-5.x — teammate、subagent、workflow leaf、`run` 全部可用:

```bash
cc-fleet codex add      # 注册 provider(端口 + 默认模型自动选好)
cc-fleet codex login    # 一次性设备码 OAuth(打印 URL + 验证码)
```

`claude` 进程对一个本地回环转换 daemon(`codex-proxy`,懒启动,闲置自退)说 Anthropic API;daemon 翻译成 OpenAI Responses API 调 ChatGPT 后端。OAuth bearer 只存在于 daemon 内部 — `keyget` 发给 claude 的只是一个低价值的回环握手 secret,token 不会进 env、argv 或任何 profile。cc-fleet 维护**自己**的 token 链(`codex login`),不读写 `~/.codex` 的认证,codex CLI 的登录不受影响。

多份订阅可以共存:`codex add --name codex-work` 再注册一个 provider, `codex login|logout|status --credential <ref>` 独立管理每份凭证。同一个 daemon 还服务 TUI 里注册的 OpenAI 协议 provider(`openai-responses`、`openai-chat`) — 每个 provider 一个端口,上游 key 同样的待遇。

> **非官方用法:**在 codex CLI 之外复用订阅可能违反 OpenAI 条款,账号可能被限流或封禁。`codex login` 会先要求明确确认;配额错误会带重置时间一并显示。

## 多 key 与轮换

file 后端的 provider 可以存多把 key(`<provider>.keys.json`,权限 `0600`),每把单独启停,在 TUI 的 key 管理器里维护。`keyget` 是轮换点 — 策略按 provider 设置:

- `off` — 始终用第一把启用的 key。
- `round_robin` — 每次拉起 worker 时计数器前进一位。
- `random` — 从启用的 key 里随机选。

禁用的 key 在选取前就被过滤。key 在所有地方都打码显示(`sk-…238`);明文只出现在 `keyget` 的 stdout 和密码式输入框里。

## Secret 后端

`--secret-backend` 决定 key 存哪:`file`(默认,`0600` 存于 `~/.config/cc-fleet/secrets/`),或由 `--secret-ref` 指向的外部管理器 — `pass`、`1password`、`vault`、系统 `keyring`。非 file 后端的 secret 由你用该后端自己的 CLI 预先配好;cc-fleet 只在 `keyget` 时解析。

## 健康、修复、更新

- `cc-fleet doctor` — 健康检查,分 **Core**(配置、二进制、claude、profile、skill 等)与**Optional**(tmux、已 attach 的会话,以及 check 8「teammate lane (optional)」:shim、shim 固定的 cc-fleet 路径、agent 定义、`teammateMode`、agent teams 开关;lane 未开启时为 OK 并提示 not set up);只有 Core *失败*才让整体判为失败(skill 检查只 WARN 不 FAIL)。check 4 在 PATH 或 `~/.local/share/claude/versions` 下找 `claude`,版本低于 provider teammate 所需的 2.1.278 时会注明。doctor 不替你动手修 — 失败项会打印修复提示。
- `cc-fleet repair` — 从 `providers.toml` 重建 provider profile JSON;lane 已开启或 shim 已存在时,把 shim 重新固定到当前二进制并恢复权限,lane 已开启时同步 `ccf-*` 定义。`--json` 多出 `shim`、`shim_repinned` 和 `agent_defs`(`written` / `removed` / `unchanged`)。从不改 `settings.json`。
- `cc-fleet update` — 按安装方式自更新:tarball 安装原地换二进制(校验和验证,留 `.previous` 供 `update rollback`),npm/go 安装交给各自的包管理器;同一趟顺手刷新插件。`--check` 只报告不动手;`--binary-only` 跳过插件刷新。Windows 上不可用 — 用 npm 或重新下 zip。
- `cc-fleet watch` — 整个舰队的只读文本流(teammate + 任务 + run);`--interval`、`--timeout`、`--check`。
- `cc-fleet uninstall` — 先像 `teammate setup --remove` 那样撤销 teammate lane(shim 路径列在 `kept` 里:所有 `claude` 会话都重启过之后可以删),再重置全部配置与状态(file 后端 secret 默认保留,`--wipe-secrets` 连同删除);裸 uninstall 不碰 skills、插件、二进制,之后可直接 `init` 重来。`uninstall --all` 是彻底卸载 — skills、插件、最后二进制 + `ccf` 别名,按安装方式路由(npm 装的走 `npm uninstall -g`;进程内删不掉的 — 以及 Windows 上的一切 — 打印成手动命令)。`--all` 默认连 secret 一起清,显式 `--keep-secrets` 才保留;会先确认,非交互或 `--json` 调用必须带 `--yes`。

## 文件与路径

| 路径 | 内容 |
|------|------|
| `~/.config/cc-fleet/providers.toml` | provider 定义(权限 `0600`)。 |
| `~/.config/cc-fleet/secrets/` | file 后端的 key(目录 `0700`,key `0600`)。 |
| `~/.config/cc-fleet/subagent-jobs/` | 后台任务元数据 + 结果缓存。 |
| `~/.config/cc-fleet/subagent-jobs/runs/` | workflow run 的 manifest、journal、事件流。 |
| `~/.config/cc-fleet/bin/claude-teammate` | teammate 启动器 shim(`teammate setup`、`repair` 写入)。 |
| `~/.claude/profiles/` | 生成的各 provider profile(teammate、subagent、`run` 的 `--settings`)。 |
| `~/.claude/agents/ccf-*.md` | provider teammate 的 agent 定义(由 cc-fleet 管理,带 `managed-by: cc-fleet` 标记)。 |
| `~/.claude/settings.json` | cc-fleet 写 `env.CLAUDE_CODE_TEAMMATE_COMMAND`、`env.CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS`,以及仅在要求时写 `teammateMode`(`teammate setup`)。 |
| `~/.claude/teams/<team>/` | 原生 team 状态 — 归 Claude Code 所有;cc-fleet 只读。 |

设置了 `$XDG_CONFIG_HOME` 时,`~/.config/cc-fleet` 基路径随之切换;`~/.claude` 下的路径遵循 `$CLAUDE_CONFIG_DIR`(profile 仍在 `~/.claude/profiles`)。
