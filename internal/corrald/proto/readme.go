package proto

import (
	"fmt"
	"strings"
)

// ReadmeData holds installation metadata interpolated into the bilingual
// README.txt placed on target hosts and served by the gateway detail page.
type ReadmeData struct {
	Version    string
	Release    int64
	Gateway    string
	EnabledBy  string
	EnabledAt  string
	MachineID  string
	LibDir     string
	StateDir   string
	SocketPath string
	Files      []string
}

// RenderReadme generates the bilingual (Chinese + English) host README.txt
// content according to the Corral consent design. All commands use full paths
// because corrald never modifies PATH.
func RenderReadme(d ReadmeData) string {
	ver := fallback(d.Version, "dev")
	gw := fallback(d.Gateway, "(未指定 / unspecified)")
	by := fallback(d.EnabledBy, "(未指定 / unspecified)")
	at := fallback(d.EnabledAt, "(未指定 / unspecified)")
	mid := fallback(d.MachineID, "<machine-id>")
	libDir := fallback(d.LibDir, "~/.local/lib/corral")
	stateDir := fallback(d.StateDir, "~/.local/state/corral/corrald/"+mid)
	sockPath := fallback(d.SocketPath, stateDir+"/corrald.sock")
	binPath := strings.TrimRight(libDir, "/") + "/corrald"
	stateRoot := "~/.local/state/corral/corrald"
	if idx := strings.LastIndex(stateDir, "/"); idx > 0 {
		stateRoot = stateDir[:idx]
	}

	var fileLines strings.Builder
	if len(d.Files) > 0 {
		for _, f := range d.Files {
			fileLines.WriteString("  - ")
			fileLines.WriteString(f)
			fileLines.WriteByte('\n')
		}
	} else {
		fileLines.WriteString(fmt.Sprintf("  - %s/corrald-<version>-<release>/corrald\n", libDir))
		fileLines.WriteString(fmt.Sprintf("  - %s (符号链接 / symlink)\n", binPath))
		fileLines.WriteString(fmt.Sprintf("  - %s/README.txt\n", libDir))
		fileLines.WriteString(fmt.Sprintf("  - %s/ (状态目录 / state directory)\n", stateDir))
		fileLines.WriteString(fmt.Sprintf("  - %s (Unix 套接字 / Unix socket)\n", sockPath))
	}

	var b strings.Builder
	b.WriteString("Corral 主机辅助进程（corrald）说明\n")
	b.WriteString("====================================\n\n")
	b.WriteString("这是什么\n")
	b.WriteString("  corrald 是 Corral 的主机辅助进程，由用户授权后运行在当前系统账号下。\n")
	b.WriteString("  它通过现有 SSH 连接上的本地 Unix 套接字为 Corral 网关提供智能体状态与对话读取，\n")
	b.WriteString("  避免反复通过 SSH 执行 shell 脚本，减少连接名额占用并实现状态即时更新。\n\n")
	b.WriteString("当前安装信息\n")
	b.WriteString(fmt.Sprintf("  版本：%s（release %d）\n", ver, d.Release))
	b.WriteString(fmt.Sprintf("  来源网关：%s\n", gw))
	b.WriteString(fmt.Sprintf("  启用人：%s\n", by))
	b.WriteString(fmt.Sprintf("  启用时间：%s\n", at))
	b.WriteString(fmt.Sprintf("  机器标识：%s\n\n", mid))
	b.WriteString("读取的内容\n")
	b.WriteString("  - 进程列表，以及智能体进程的启动参数、打开的文件和少数用于定位配置目录的环境变量\n")
	b.WriteString("    （CLAUDE_CONFIG_DIR、CLAUDE_CODE_SESSION_ID、CODEX_HOME），其他环境变量不保存、不发送。\n")
	b.WriteString("  - 智能体的对话记录（例如 ~/.claude/projects/、~/.codex/sessions/ 等）。\n")
	b.WriteString("  - herdr 和 tmux 窗格的屏幕内容。\n")
	b.WriteString("  读取的内容只经当前 SSH 连接发给 Corral 网关，不发送给其他位置。\n\n")
	b.WriteString("代为执行的操作\n")
	b.WriteString("  - 只执行用户在 Corral 里发起的预定义操作：向窗格输入文字和按键、回答提问和审批卡片。\n")
	b.WriteString("  - 不接受任意命令，也不自行发起操作。\n")
	b.WriteString("  - 程序运行在当前用户的系统账号下，账号能做的事它在技术上都能做；此处列出的是程序承诺使用的范围，\n")
	b.WriteString("    后续版本如果扩大读取或操作范围，会先重新征得同意。\n\n")
	b.WriteString("运行身份与资源占用\n")
	b.WriteString("  - 运行身份：当前系统账号，不需要 root，不监听网络端口，不连接互联网。\n")
	b.WriteString(fmt.Sprintf("  - 安装目录：%s/\n", libDir))
	b.WriteString(fmt.Sprintf("  - 状态目录：%s/\n", stateDir))
	b.WriteString("  - 日志文件：corrald.log，单个文件上限 5 MB，最多保留一个旧文件（corrald.log.1），\n")
	b.WriteString("    只记录操作和错误，不记录对话内容和屏幕内容。\n")
	b.WriteString("  - 磁盘与内存预算：磁盘约 30 MB（更新时保留上一版本），日志最多 10 MB，内存上限 50 MB。\n")
	b.WriteString("  - 文件清单：\n")
	b.WriteString(fileLines.String())
	b.WriteByte('\n')
	b.WriteString("不做的事\n")
	b.WriteString("  - 不修改各智能体的配置文件，不安装钩子。\n")
	b.WriteString("  - 不改动 ~/.bashrc 等 shell 配置、PATH、crontab、systemd 用户服务或 ~/.ssh/。\n")
	b.WriteString("  - 不向任何地方上报使用统计。\n\n")
	b.WriteString("运行时间与更新\n")
	b.WriteString("  - Corral 网关连接这台主机时运行；没有任何网关连接持续 10 分钟后自动退出。\n")
	b.WriteString("  - 随 Corral 网关升级自动更新，经 SSH 上传并校验；扩大读取或操作范围前需重新确认。\n\n")
	b.WriteString("查看状态、停止与卸载（不加入 PATH，请使用完整路径）\n")
	b.WriteString(fmt.Sprintf("  查看状态：%s status\n", binPath))
	b.WriteString(fmt.Sprintf("  停止进程：%s stop\n", binPath))
	b.WriteString(fmt.Sprintf("  完全卸载：%s uninstall\n\n", binPath))
	b.WriteString("程序损坏时的手工清理步骤\n")
	b.WriteString(fmt.Sprintf("  1. 查看 %s/meta.json 中的 pid，确认对应进程为 corrald 后执行 kill <pid>。\n", stateDir))
	b.WriteString(fmt.Sprintf("  2. 删除本机状态目录与套接字：rm -rf %s %s\n", stateDir, sockPath))
	b.WriteString(fmt.Sprintf("  3. 如果共享家目录 %s/ 下已无其他机器目录，再删除程序文件：\n", stateRoot))
	b.WriteString(fmt.Sprintf("     rm -rf %s/corrald %s/corrald-* %s/README.txt\n", libDir, libDir, libDir))
	b.WriteString(fmt.Sprintf("     注意：不要删除 %s/ 下的其他目录（例如 code-server）。\n\n", libDir))

	b.WriteString("------------------------------------------------------------------------\n\n")

	b.WriteString("Corral Host Helper Daemon (corrald)\n")
	b.WriteString("===================================\n\n")
	b.WriteString("What this is\n")
	b.WriteString("  corrald is the Corral host helper daemon, enabled by user consent under this account.\n")
	b.WriteString("  It serves agent status and conversation reads over a local Unix domain socket\n")
	b.WriteString("  forwarded through the existing SSH connection, avoiding repeated SSH shell scripts.\n\n")
	b.WriteString("Installation details\n")
	b.WriteString(fmt.Sprintf("  Version:    %s (release %d)\n", ver, d.Release))
	b.WriteString(fmt.Sprintf("  Gateway:    %s\n", gw))
	b.WriteString(fmt.Sprintf("  Enabled by: %s\n", by))
	b.WriteString(fmt.Sprintf("  Enabled at: %s\n", at))
	b.WriteString(fmt.Sprintf("  Machine ID: %s\n\n", mid))
	b.WriteString("Data read\n")
	b.WriteString("  - Process list, agent command-line arguments, open files, and specific config-locating\n")
	b.WriteString("    environment variables (CLAUDE_CONFIG_DIR, CLAUDE_CODE_SESSION_ID, CODEX_HOME);\n")
	b.WriteString("    other environment variables are neither saved nor transmitted.\n")
	b.WriteString("  - Agent conversation logs (such as ~/.claude/projects/, ~/.codex/sessions/).\n")
	b.WriteString("  - herdr and tmux pane screen contents.\n")
	b.WriteString("  All data is sent only to the connected Corral gateway over SSH.\n\n")
	b.WriteString("Actions performed\n")
	b.WriteString("  - Only predefined user-initiated actions from Corral: sending text/keys to panes\n")
	b.WriteString("    and answering prompt/approval cards. Arbitrary shell commands are not accepted.\n")
	b.WriteString("  - The daemon runs under your user account (so it has the account's OS permissions);\n")
	b.WriteString("    the manifest defines its promised scope, and any scope growth requires re-consent.\n\n")
	b.WriteString("Identity, location, and limits\n")
	b.WriteString("  - Runs as this user account; no root, no network ports, no internet access.\n")
	b.WriteString(fmt.Sprintf("  - Binary directory: %s/\n", libDir))
	b.WriteString(fmt.Sprintf("  - State directory:  %s/\n", stateDir))
	b.WriteString("  - Logs: corrald.log, capped at 5 MB with at most one backup (corrald.log.1);\n")
	b.WriteString("    records operations and errors only, never conversations or screen contents.\n")
	b.WriteString("  - Budget: ~30 MB disk (retaining one previous release), <= 10 MB logs, <= 50 MB RSS.\n")
	b.WriteString("  - Files:\n")
	b.WriteString(fileLines.String())
	b.WriteByte('\n')
	b.WriteString("What it does not do\n")
	b.WriteString("  - Does not modify agent config files or install hooks.\n")
	b.WriteString("  - Does not edit shell rc files, PATH, crontab, systemd user services, or ~/.ssh/.\n")
	b.WriteString("  - Does not report telemetry anywhere.\n\n")
	b.WriteString("Lifecycle and updates\n")
	b.WriteString("  - Runs while a Corral gateway is connected; exits after 10 minutes with no connections.\n")
	b.WriteString("  - Updates automatically from the Corral gateway over SSH with checksum verification;\n")
	b.WriteString("    expanding reads, actions, writes, egress, budget, or optional capabilities requires consent.\n\n")
	b.WriteString("Commands (not added to PATH; use full paths)\n")
	b.WriteString(fmt.Sprintf("  Status:    %s status\n", binPath))
	b.WriteString(fmt.Sprintf("  Stop:      %s stop\n", binPath))
	b.WriteString(fmt.Sprintf("  Uninstall: %s uninstall\n\n", binPath))
	b.WriteString("Manual cleanup if the binary is damaged\n")
	b.WriteString(fmt.Sprintf("  1. Check pid in %s/meta.json, verify it is corrald, and run: kill <pid>\n", stateDir))
	b.WriteString(fmt.Sprintf("  2. Remove this host's state and socket: rm -rf %s %s\n", stateDir, sockPath))
	b.WriteString(fmt.Sprintf("  3. If no other machine directories remain under %s/, remove the shared binaries:\n", stateRoot))
	b.WriteString(fmt.Sprintf("     rm -rf %s/corrald %s/corrald-* %s/README.txt\n", libDir, libDir, libDir))
	b.WriteString(fmt.Sprintf("     Note: leave other directories under %s/ (such as code-server) untouched.\n", libDir))

	return b.String()
}

func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
