![cc-fleet — 把任意厂商 LLM(DeepSeek · GLM · Qwen · Kimi …)当作真正的 Claude Code teammate](docs/assets/cc-fleet-banner.png)

# cc-fleet

<p align="center"><strong>🤖 Spawn any vendor LLM — DeepSeek · GLM · Qwen · Kimi · MiniMax … — as real Claude Code teammates or ⚡ one-shot subagents 🚀</strong></p>

<div align="center">

[![Release](https://img.shields.io/github/v/release/ethanhq/cc-fleet?style=for-the-badge&color=2ea043&label=release)](https://github.com/ethanhq/cc-fleet/releases)
[![npm](https://img.shields.io/npm/v/cc-fleet?style=for-the-badge&color=cb3837)](https://www.npmjs.com/package/cc-fleet)
[![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20macOS-8957e5?style=for-the-badge)](https://github.com/ethanhq/cc-fleet/releases)
[![License](https://img.shields.io/badge/license-Apache%202.0-1f6feb?style=for-the-badge)](LICENSE)
[![Lang](https://img.shields.io/badge/Lang-English-d29922?style=for-the-badge)](README.md)

<img src="https://github.com/user-attachments/assets/d6312861-7626-4ac5-a9b8-39a1f6a4be2d" alt="cc-fleet demo" width="760" />

</div>

---

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

```bash
# 1. 在 ~/.config/cc-fleet/ 创建配置目录
cc-fleet init

# 2. 注册一个 vendor —— key 走 stdin,绝不进 argv / shell 历史
printf '%s' "$DEEPSEEK_API_KEY" | cc-fleet add deepseek \
  --base-url https://api.deepseek.com/anthropic \
  --models-endpoint https://api.deepseek.com/v1/models \
  --default-model deepseek-chat \
  --secret-backend file --secret-ref deepseek.key --api-key-stdin

# 3. 健康检查
cc-fleet doctor
```

然后直接用自然语言跟 Claude Code 说,skill 会自动路由:

> *"开一个 deepseek teammate 重构 parser 包,然后回报。"*
> &nbsp;&nbsp;→ 一个长期存活的厂商 **teammate**(tmux pane)。
>
> *"用 deepseek 总结这个 2000 行的日志文件。"*
> &nbsp;&nbsp;→ 一次性 **subagent**,结果直接返回。

Claude 自己决定用 teammate 还是 subagent,拉起厂商 worker 并协调它——你的主会话全程用自己的
Anthropic 认证。

## 工作原理

它捕获 Claude Code 自己的 spawn 模板(*fingerprint*),换上厂商 profile,在 tmux pane 里拉起
一个真正的 `claude` 进程——完整工具栈不变,只换模型后端。厂商 key 通过 profile 的 `apiKeyHelper`
(`cc-fleet keyget`)惰性取用,永不进入环境变量、argv 或 shell 历史。你主会话的认证从不被触碰,只有
teammate pane 用厂商计费。

## skill

二进制只是 CLI。要教 Claude Code **何时**委派,通过插件安装 skill(一行安装器默认就会做):
```bash
claude plugin marketplace add ethanhq/cc-fleet
claude plugin install cc-fleet@ethanhq
```

## 许可证

[Apache-2.0](LICENSE)。
