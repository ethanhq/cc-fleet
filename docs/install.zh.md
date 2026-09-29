# 安装与维护

cc-fleet 的完整安装渠道与日常维护。最快的上手路径在 [README 快速安装](README_zh.md#快速安装),这里覆盖其余方式、安装器参数与维护命令。

## 前置:先装 Claude Code

cc-fleet 驱动的是官方 `claude` CLI,PATH 上没有就先装(已有可跳过):

- **macOS / Linux**:`curl -fsSL https://claude.ai/install.sh | bash`
- **Windows(PowerShell)**:`irm https://claude.ai/install.ps1 | iex`

## 一键脚本的覆盖参数

[README](README_zh.md#快速安装) 给的是最简一行。要覆盖安装器默认行为,在 `| sh -s --` 后追加参数:

```bash
curl -fsSL https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.sh | sh -s -- \
  --skill plugin \        # 怎么装 skill:plugin(默认,走 marketplace)| global(拷进 ~/.claude/skills)| none
  --scope user \          # 插件 scope:user(默认)| project | local
  --prefix ~/.local/bin \ # 二进制装到哪(默认 ~/.local/bin)
  --version v0.2.1         # 钉一个具体版本(默认最新)
```

Windows 上 PowerShell 安装器用环境变量覆盖:

```powershell
$env:CCF_VERSION = "v0.2.1"; $env:CCF_PREFIX = "$HOME\bin"; irm https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.ps1 | iex
```

在已有安装上重跑 `install.sh`(或 release 归档里的 `./install.sh`)是安全的,即使 cc-fleet 正在运行(例如队友的 `apiKeyHelper`、后台的 `ccf watch`):新二进制先拷到旧文件旁边的临时文件,再改名覆盖过去,运行中的进程继续用它原来的文件,新的也能正常启动。(就地覆盖文件会让 macOS 之后每次启动都杀掉它。)

## 从其他源安装

下面这些渠道**只装 CLI**,装完还需手动补装插件。npm / go / Releases 在 Linux、macOS、Windows 上通用:

| 渠道 | 命令 |
|------|------|
| npm | `npm install -g @ethanhq/cc-fleet` |
| go install | `go install github.com/ethanhq/cc-fleet/cmd/cc-fleet@latest` |
| [Releases](https://github.com/ethanhq/cc-fleet/releases) | 先按你的 OS / arch 下载压缩包,再 `tar -xzf cc-fleet-*.tar.gz && cd cc-fleet-*/ && ./install.sh`(Windows:解压 zip 后运行内附的 `install.ps1`) |
| 源码 *(Linux / macOS)* | `git clone https://github.com/ethanhq/cc-fleet.git && cd cc-fleet && make install` — 需要 `make`;Windows 上从源码安装请改用上面的 `go install` |

### 补装 Claude Code 插件

这是教 Claude *何时*委派的部分:三个 skill 分别教会它在合适的场景下调度 Workflow、Agent Team 与 Subagent,并判断该走哪条 lane。二选一安装:

- **在 Claude Code 里装(推荐)**:启动 `claude`,敲下面两条斜杠命令(或执行 `/plugin` 打开交互式 Marketplaces / Discover 面板):
  ```
  /plugin marketplace add ethanhq/cc-fleet
  /plugin install cc-fleet@ethanhq
  ```
- **命令行**:
  ```bash
  claude plugin marketplace add ethanhq/cc-fleet
  claude plugin install cc-fleet@ethanhq        # 默认装在 user scope;加 --scope project|local 可改
  ```

## 环境要求

Agent Team 模式**无法在 Windows 上使用**;其余功能(Subagent / Workflow / run / TUI)完全一致、无任何额外依赖。Provider Teammate 需要:

- **Claude Code ≥ 2.1.278**,终端版 `claude` — Claude 桌面 App、`claude -p` 和 SDK 会话没有 agent team,用不了;
- 这个 `claude` 运行在 **tmux 或 iTerm2** 里,Teammate 才能以 pane 打开。先装 tmux(macOS:`brew install tmux`;Debian / Ubuntu:`sudo apt install tmux`),再在里面启动 `claude`;
- **一次性配置**,然后重启 `claude`(首次运行 TUI 时它会提示并可替你完成):

  ```bash
  ccf teammate setup           # 列出将要做的改动
  ccf teammate setup --yes --teammate-mode tmux
  ```

  它会写入启动器 `~/.config/cc-fleet/bin/claude-teammate`、`~/.claude/agents/` 下的 `ccf-*` agent 定义,以及 `~/.claude/settings.json` 里的两个键(要求时还有 `teammateMode`)。`ccf teammate setup --remove --yes` 撤销;cc-fleet 换了位置后用 `ccf repair` 重新固定启动器;`ccf uninstall` 也会一并撤销。

## 维护

```bash
ccf doctor               # 体检:检查依赖、Provider、插件状态、teammate lane
ccf repair               # 重写 Provider profile;重新固定 teammate 启动器
ccf update               # 沿安装渠道自更新并刷新插件
ccf update rollback      # 回滚到上一个版本
ccf uninstall            # 重置配置与状态(保留二进制与 secret)
ccf uninstall --all      # 连二进制、插件一起清空
```

`ccf update` 会识别当初的安装渠道:脚本 / 手动安装就地换二进制(sha256 校验 + `--version` 冒烟),npm / go 则交回各自的包管理器,然后同一趟把插件一起刷新。只有可比较的正式版本才会触发更新,dev 构建原样保留。
