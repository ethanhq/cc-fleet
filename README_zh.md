![cc-fleet — 把任意厂商 LLM(DeepSeek · GLM · Qwen · Kimi …)当作真正的 Claude Code teammate](docs/assets/cc-fleet-banner.png)

# 🚢 cc-fleet

<p align="center"><strong>🤖 Spawn any vendor LLM — DeepSeek · GLM · Qwen · Kimi · MiniMax … — as real Claude Code teammates or ⚡ one-shot subagents 🚀</strong></p>

<div align="center">

[![Release](https://img.shields.io/github/v/release/ethanhq/cc-fleet?style=for-the-badge&color=2ea043&label=release)](https://github.com/ethanhq/cc-fleet/releases)
[![npm](https://img.shields.io/npm/v/cc-fleet?style=for-the-badge&color=cb3837)](https://www.npmjs.com/package/cc-fleet)
[![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20macOS-8957e5?style=for-the-badge)](https://github.com/ethanhq/cc-fleet/releases)
[![License](https://img.shields.io/badge/license-Apache%202.0-1f6feb?style=for-the-badge)](LICENSE)
[![Lang](https://img.shields.io/badge/Lang-English-d29922?style=for-the-badge)](README.md)

</div>

---

<div align="center">

<img src="https://github.com/user-attachments/assets/d6312861-7626-4ac5-a9b8-39a1f6a4be2d" alt="cc-fleet demo" width="760" />

</div>

厂商 worker 就是**真正的 Claude Code teammate**——和原生 teammate 一样驱动——只是把 LLM
后端换成任意提供 Anthropic 兼容 API 的厂商。你主会话自己的认证(OAuth 订阅或 API key)完全不受
影响;厂商 worker 用厂商的 API key 计费,key 通过 `apiKeyHelper` 取用,永不进入环境变量、argv
或 shell 历史。

`cc-fleet` 是一个小巧的 Go CLI 加一个 Claude Code skill。CLI 负责管理各厂商的 profile、通过
`apiKeyHelper` 派发 API key、在 tmux pane 里拉起 teammate 会话;skill 则教 Claude Code
**何时**把活委派给这些 teammate。

## 环境要求

- **Claude Code**(`claude` CLI)在你的 PATH 上。
- **tmux** —— 厂商 teammate 跑在 tmux pane 里。
- **macOS 或 Linux**,amd64 或 arm64 —— 已测试平台。Windows 理论上可跑一次性 **subagent**
  模式,但尚未测试。
- **teammate** 模式需要 Claude Code 的 agent-teams 已启用。在全局 `~/.claude/settings.json`
  里打开并重启 Claude Code(cc-fleet 首次运行也会提示你):
  ```json
  { "env": { "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1" } }
  ```
  一次性 **subagent** 模式不需要它。

## 快速安装

**一行命令(推荐)**
```bash
curl -fsSL https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.sh | sh
```
下载预编译二进制,安装 `cc-fleet` + `ccf` 别名,并通过 Claude Code 插件装上 skill。参数(放在
`| sh -s --` 之后):`--skill plugin|global|none`、`--scope user|project|local`、
`--prefix DIR`、`--version vX.Y.Z`。

**npm**
```bash
npm install -g cc-fleet      # 或一次性运行:npx cc-fleet
```

**go install**
```bash
go install github.com/ethanhq/cc-fleet/cmd/cc-fleet@latest
ln -sf "$(go env GOPATH)/bin/cc-fleet" "$(go env GOPATH)/bin/ccf"   # 可选 ccf 别名
```

**预编译 tarball** —— 从 [Releases](https://github.com/ethanhq/cc-fleet/releases) 下载:
```bash
tar -xzf cc-fleet-*.tar.gz && cd cc-fleet-*/ && ./install.sh
```

**从源码构建**
```bash
git clone https://github.com/ethanhq/cc-fleet.git && cd cc-fleet && make install
```

## 快速上手

直接运行 `cc-fleet`(或别名 `ccf`),不带参数即可进入交互式 TUI:

```bash
cc-fleet
```

在 TUI 里注册一个 vendor —— 填名字、Anthropic 兼容的 base URL、models 端点、默认模型,并粘贴
API key。key 会以 `0600` 写到 `~/.config/cc-fleet/secrets/` 下,**绝不**经过 argv 或 shell
历史。

<p align="center"><img src="docs/assets/tui-add-vendor.png" alt="cc-fleet TUI —— 添加 vendor 表单" width="760" /></p>

配置目录树在首次保存时自动创建,所以没有单独的 init 步骤。TUI 还会列出你的 vendor,可以编辑它们、
给同一个 vendor 管理多把 key。

<p align="center"><img src="docs/assets/tui-vendors.png" alt="cc-fleet TUI —— vendor 列表" width="760" /></p>

按 `tab` 可以切到 **Agent status** 看板 —— 它按 session → team 分组列出每个存活的 teammate,
显示其 vendor、模型、pane、PID、健康状态、是否隐藏,还有一份 subagent 任务列表。在这里可以
隐藏(`h`)/ 显示(`s`)teammate 的 pane,或刷新(`r`)。

<p align="center"><img src="docs/assets/tui-agent-status.png" alt="cc-fleet TUI —— agent status 看板" width="760" /></p>

注册好至少一个 vendor 后,直接用自然语言跟 Claude Code 说就行。skill 会读你的请求、决定怎么
执行 —— 一共两种执行模式。

### Teammate 模式 —— 一个长期存活、在你团队里的厂商 worker

> *"开一个 deepseek teammate 重构 parser 包,然后回报。"*

把厂商当作**真正的 Claude Code agent-team teammate** 来跑:

- Claude 调用原生 `TeamCreate`;cc-fleet 在一个 tmux pane 里拉起厂商自己的 `claude` 进程。
- Claude 用原生 `SendMessage` 驱动它 —— 你派任务,它干活并回报。
- teammate **跨多轮一直存活**,可以不断派后续工作;同时开好几个还能把活并行铺开。
- 你的主会话仍用自己的认证 —— 只有 teammate pane 用厂商 key 计费(经 `apiKeyHelper`)。

> [!NOTE]
> Teammate 模式需要 Claude Code 的 agent-teams 已启用 —— 见 [环境要求](#环境要求)。

先进入一个 tmux 会话,这样 teammate 才能在你的 leader 旁边切分出 pane:

```bash
tmux new-session -s cc-fleet
```

<p align="center"><img src="docs/assets/teammate-panes.png" alt="cc-fleet teammate —— 左侧 leader,右侧 deepseek 与 glm teammate pane" width="760" /></p>

> 左边是你的 leader 会话,右边各有一个 `deepseek` 和一个 `glm` teammate 跑在自己的 pane 里 ——
> 每个都是真正的 `claude` 进程,用 `SendMessage` 驱动,像原生 teammate 一样回报。

> [!TIP]
> **不在 tmux 里?** cc-fleet 会把 teammate 跑在一个 detached 的 `cc-fleet-swarm-<team>`
> server 里 —— 一样用原生 `TeamCreate` / `SendMessage` 驱动,只是 pane 不在屏幕上。想看就
> `tmux -L cc-fleet-swarm-<team> attach`。

### Subagent 模式 —— 一次性、headless 调用

> *"用 deepseek 总结这个 2000 行的日志文件。"*

`cc-fleet subagent <vendor>` 以 headless 方式调用厂商模型,同步返回结果 —— **没有 pane、没有
team、也不需要 agent-teams**。最适合一次性调研/分析,以及把互不依赖的任务批量铺开。

| 参数 | 用途 |
|------|------|
| `--background` | detached 运行;用 `cc-fleet subagent-status` 轮询 |
| `--resume <id>` | 续接上一次 subagent(多轮) |
| `--max-budget-usd` / `--max-turns` | 给成本和时长封顶 |

> [!NOTE]
> 你不用手动选模式 —— Claude 会根据请求性质自己决定 teammate 还是 subagent,拉起厂商 worker
> 并替你协调好。

### 更多示例提示词

- *"开一个 glm teammate 和一个 deepseek teammate,各自总结自己模型的强项,然后对比两者。"*
- *"用 deepseek review `internal/spawn` 的 diff,列出你发现的 bug。"*
- *"把 kimi、qwen、glm 三个 subagent 并行铺到这三个文件上,汇总结果。"*
- *"拉一个 deepseek teammate 把测试套件改成表驱动形式,然后回报。"*

## CLI 与高级用法

Claude 会替你驱动 CLI,但每个命令也都能手动用 —— 多 key 轮换、`hide`/`show`、后台/可续接的
subagent、secret 后端、teardown 顺序等等。详见 **[CLI 参考与高级用法](docs/cli.zh.md)**,或运行
`cc-fleet <cmd> --help`。

## skill

二进制只是 CLI。要教 Claude Code **何时**委派,通过插件安装 skill(一行安装器默认就会做):
```bash
claude plugin marketplace add ethanhq/cc-fleet
claude plugin install cc-fleet@ethanhq
```

## 许可证

[Apache-2.0](LICENSE)。
