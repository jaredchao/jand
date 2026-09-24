package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/jaredchao/jand/internal/transfer"
)

// installRecord is $JAND_HOME/installed.json: what setup placed outside
// jand's own directory, so uninstall can take back exactly that.
type installRecord struct {
	Program       string   `json:"program,omitempty"`
	ClaudeSkill   string   `json:"claude_skill,omitempty"`
	ClaudeRules   []string `json:"claude_rules,omitempty"`
	ClaudeBackups []string `json:"claude_backups,omitempty"`
	// ClaudeSettingsCreated: setup created settings.json to hold the rule.
	ClaudeSettingsCreated bool   `json:"claude_settings_created,omitempty"`
	CodexAgents           string `json:"codex_agents,omitempty"`
}

// jandHomeEntries are everything jand keeps in $JAND_HOME.
var jandHomeEntries = []string{"config.json", "access-token", "installed.json", "chats", "workflows"}

func installRecordPath() string {
	return filepath.Join(transfer.HomeDir(), "installed.json")
}

func loadInstallRecord() installRecord {
	var r installRecord
	if data, err := os.ReadFile(installRecordPath()); err == nil {
		json.Unmarshal(data, &r)
	}
	return r
}

func (r installRecord) save() error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(transfer.HomeDir(), 0700); err != nil {
		return err
	}
	return os.WriteFile(installRecordPath(), append(data, '\n'), 0600)
}

func (r *installRecord) addClaudeRule(rule, backup string) {
	if !slices.Contains(r.ClaudeRules, rule) {
		r.ClaudeRules = append(r.ClaudeRules, rule)
	}
	if backup != "" {
		r.ClaudeBackups = append(r.ClaudeBackups, backup)
	}
}

// selfPath is this program's real location on disk.
func selfPath() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		return real
	}
	return self
}

// printFootprint lists every place jand uses on this machine, so a person
// knows what exists and what uninstall will take back.
func printFootprint(out io.Writer, cfg clientConfig, r installRecord) {
	row := func(label, value string) { fmt.Fprintf(out, "  %s %s\n", padWidth(label, 10), value) }
	fmt.Fprintln(out, "jand 在本机用到的位置（客户端不写日志；卸载用 jand uninstall，会先把历史打包给你）：")
	row("程序", firstSet(r.Program, selfPath()))
	row("配置", clientConfigPath())
	if cfg.AccessTokenFile != "" {
		row("访问令牌", cfg.AccessTokenFile+"（权限 600）")
	}
	row("对话记录", transfer.DefaultStateDir()+"（每个对话的记录、状态，以及 chat view 生成的网页）")
	row("收到的文件", firstSet(cfg.OutDir, "当前目录下的 received/"))
	if r.ClaudeSkill != "" {
		row("Claude", r.ClaudeSkill+"（skill）")
	}
	for _, rule := range r.ClaudeRules {
		row("", claudeSettingsPath()+" 中一条放行规则 "+rule)
	}
	for _, b := range r.ClaudeBackups {
		row("", "放行前的备份 "+b)
	}
	if r.CodexAgents != "" {
		row("Codex", r.CodexAgents+" 中带 jand 标记的一段")
	}
	row("安装记录", installRecordPath())
}

// uninstall removes what jand placed on this machine. History is packed
// into an archive first unless declined; received files are the user's own
// and stay unless the user asks, in which case they go into the archive too.
func uninstall(args []string, in io.Reader, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("jand uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("yes", false, "do not ask; keep received files and archive history unless told otherwise")
	noArchive := fs.Bool("no-archive", false, "do not pack the history first")
	deleteReceived := fs.Bool("delete-received", false, "also delete the received files (they are archived first)")
	dryRun := fs.Bool("dry-run", false, "only list what would be done")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: jand uninstall [--dry-run] [--yes] [--no-archive] [--delete-received]")
		return 2
	}
	p := &prompter{in: bufio.NewReader(in), out: out, yes: *yes}
	cfg, _ := loadClientConfig() // a broken config must not block removal
	rec := loadInstallRecord()
	home, _ := os.UserHomeDir()
	jandHome := transfer.HomeDir()

	var remove []string
	program := firstSet(rec.Program, selfPath())
	if exists(program) {
		remove = append(remove, program)
	}
	// Only what jand itself keeps in its directory: JAND_HOME may point
	// anywhere, even at a directory holding other things.
	for _, name := range jandHomeEntries {
		if p := filepath.Join(jandHome, name); exists(p) {
			remove = append(remove, p)
		}
	}
	// A token file the user pointed setup at is theirs; only the one setup
	// wrote into $JAND_HOME goes.
	ownToken := ""
	if t := cfg.AccessTokenFile; t != "" && exists(t) && !within(t, jandHome) {
		ownToken = t
	}
	skill := firstSet(rec.ClaudeSkill, filepath.Join(home, ".claude", "skills", "jand"))
	if data, err := os.ReadFile(filepath.Join(skill, "SKILL.md")); err == nil && strings.Contains(string(data), "help agent") {
		remove = append(remove, skill)
	}
	for _, b := range rec.ClaudeBackups {
		if exists(b) {
			remove = append(remove, b)
		}
	}
	var modify []string
	settingsPath := claudeSettingsPath()
	rules := claudeRulesPresent(settingsPath, rec.ClaudeRules)
	if len(rules) > 0 {
		modify = append(modify, fmt.Sprintf("%s：只删掉 %s，其他设置不动", settingsPath, strings.Join(rules, "、")))
	}
	codex := firstSet(rec.CodexAgents, filepath.Join(home, ".codex", "AGENTS.md"))
	if data, err := os.ReadFile(codex); err == nil && strings.Contains(string(data), codexBegin) {
		modify = append(modify, codex+"：只删掉带 jand 标记的那一段")
	} else {
		codex = ""
	}
	received := cfg.OutDir
	if received != "" && (!exists(received) || within(received, jandHome) || within(home, received) || filepath.Dir(received) == received) {
		received = "" // never offer to delete a home directory, one of its parents, or a root
	}

	fmt.Fprintln(out, "卸载 jand。以下内容会被处理：")
	fmt.Fprintln(out, "删除：")
	for _, r := range remove {
		fmt.Fprintf(out, "  %s\n", r)
	}
	if len(remove) == 0 {
		fmt.Fprintln(out, "  （没有找到）")
	}
	if len(modify) > 0 {
		fmt.Fprintln(out, "修改：")
		for _, m := range modify {
			fmt.Fprintf(out, "  %s\n", m)
		}
	}
	if received != "" {
		fmt.Fprintf(out, "收到的文件：%s（%d 个文件，是你的资料，默认保留）\n", received, countFiles(received))
	}
	if ownToken != "" {
		fmt.Fprintf(out, "保留：你自己提供的令牌文件 %s（不再需要的话请自行删除）\n", ownToken)
	}
	fmt.Fprintln(out, "客户端不写日志，Relay 上也不保存任何历史，所以没有别的地方要清理。")
	if *dryRun {
		return 0
	}

	delReceived := received != "" && *deleteReceived
	if received != "" && !p.yes {
		delReceived = p.confirm("也删除收到的文件吗？（删除前会一起打进备份包）", false)
	}
	archive := ""
	if !*noArchive && (p.yes || p.confirm("先把历史（对话记录、页面、配置、流程）打包备份吗？不含密钥和令牌", true)) {
		archive = filepath.Join(home, "jand-history-"+time.Now().Format("20060102-150405")+".tar.gz")
	}
	if !p.yes && !p.confirm("确认卸载？", false) {
		fmt.Fprintln(out, "已取消，什么都没改。")
		return 0
	}

	if archive != "" {
		extra := ""
		if delReceived {
			extra = received
		}
		n, err := packHistory(archive, jandHome, extra)
		if err != nil {
			fmt.Fprintf(stderr, "✗ 打包失败，已停止，什么都没删：%v\n", err)
			return 1
		}
		fmt.Fprintf(out, "✓ 历史已打包：%s（%d 个文件）\n", archive, n)
	}
	failed := false
	step := func(what string, err error) {
		if err != nil {
			failed = true
			fmt.Fprintf(stderr, "✗ %s：%v\n", what, err)
			return
		}
		fmt.Fprintf(out, "✓ %s\n", what)
	}
	if len(rules) > 0 {
		step("已从 Claude Code 设置中删除放行规则", removeClaudeRules(settingsPath, rules, rec.ClaudeSettingsCreated))
	}
	if codex != "" {
		step("已从 "+codex+" 删除 jand 那一段", removeCodexNote(codex))
	}
	for _, r := range remove {
		if r == program {
			continue // last, below
		}
		step("已删除 "+r, os.RemoveAll(r))
	}
	if delReceived {
		step("已删除收到的文件 "+received, os.RemoveAll(received))
	}
	if entries, err := os.ReadDir(jandHome); err == nil && len(entries) == 0 {
		os.Remove(jandHome)
	}
	if program != "" && exists(program) {
		if runtime.GOOS == "windows" {
			fmt.Fprintf(out, "! Windows 上程序不能删除正在运行的自己：请手动删除 %s，并从用户 PATH 中去掉 %s。\n", program, filepath.Dir(program))
		} else {
			step("已删除程序 "+program, os.Remove(program))
		}
	}
	if failed {
		return 1
	}
	fmt.Fprintln(out, "卸载完成。")
	return 0
}

// packHistory writes the chat transcripts and pages, the config, workflows
// and install record, plus extra (a directory) when given, into a tar.gz.
// Chat state, cursors and the access token are left out: they hold keys and
// tokens that are useless once jand is gone.
func packHistory(path, jandHome, extra string) (int, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 0, err
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	n := 0
	add := func(root, prefix string, keep func(rel string) bool) error {
		return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if !keep(filepath.ToSlash(rel)) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = prefix + "/" + filepath.ToSlash(rel)
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			src, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, src)
			src.Close()
			n++
			return err
		})
	}
	err = add(jandHome, "jand", func(rel string) bool {
		switch {
		case rel == "config.json", rel == "installed.json", strings.HasPrefix(rel, "workflows/"):
			return true
		case strings.HasPrefix(rel, "chats/"):
			return strings.HasSuffix(rel, ".transcript.jsonl") || strings.HasSuffix(rel, ".html")
		}
		return false
	})
	if err == nil && extra != "" {
		err = add(extra, "received", func(string) bool { return true })
	}
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = zw.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return n, err
}

func claudeRulesPresent(path string, recorded []string) []string {
	settings, _, err := readClaudeSettings(path)
	if err != nil {
		return nil
	}
	perms, _ := settings["permissions"].(map[string]any)
	allow, _ := perms["allow"].([]any)
	var found []string
	for _, r := range allow {
		if s, ok := r.(string); ok && slices.Contains(recorded, s) {
			found = append(found, s)
		}
	}
	return found
}

// removeClaudeRules takes jand's rules out of Claude Code's settings. A file
// setup created only for them, and left empty now, is removed.
func removeClaudeRules(path string, rules []string, created bool) error {
	settings, _, err := readClaudeSettings(path)
	if err != nil {
		return err
	}
	perms, _ := settings["permissions"].(map[string]any)
	allow, _ := perms["allow"].([]any)
	allow = slices.DeleteFunc(allow, func(r any) bool { s, _ := r.(string); return slices.Contains(rules, s) })
	if len(allow) == 0 {
		delete(perms, "allow")
	} else {
		perms["allow"] = allow
	}
	if len(perms) == 0 {
		delete(settings, "permissions")
	}
	if created && len(settings) == 0 {
		return os.Remove(path)
	}
	return writeClaudeSettings(path, settings)
}

func removeCodexNote(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	i, j := strings.Index(text, codexBegin), strings.Index(text, codexEnd)
	if i < 0 || j < i {
		return nil
	}
	text = strings.TrimRight(text[:i], "\n") + "\n" + strings.TrimLeft(text[j+len(codexEnd):], "\n")
	if strings.TrimSpace(text) == "" {
		text = ""
	}
	return os.WriteFile(path, []byte(text), 0644)
}

// versionCommand shows this program's version and place, where its config
// is, and whether the configured relay answers and speaks the same series.
func versionCommand(ctx context.Context, args []string, out, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: jand version   (jand --version prints only the number)")
		return 2
	}
	fmt.Fprintf(out, "jand %s\n", version)
	fmt.Fprintf(out, "  程序    %s\n", selfPath())
	cfg, err := loadClientConfig()
	if err != nil {
		fmt.Fprintf(out, "  配置    %s（读取失败：%v）\n", clientConfigPath(), err)
		return 0
	}
	relay, source := cfg.relayURL("", false)
	fmt.Fprintf(out, "  配置    %s\n", clientConfigPath())
	if source == "default" {
		fmt.Fprintf(out, "  Relay   未配置（运行 jand setup）\n")
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	h, err := checkRelay(ctx, relay)
	switch {
	case err != nil:
		fmt.Fprintf(out, "  Relay   %s：连不上（%v）\n", relay, err)
	case h.Version == "":
		fmt.Fprintf(out, "  Relay   %s：可用，版本早于 0.4.1（不报告版本号）；对话可能不兼容\n", relay)
	case minor(h.Version) != minor(version):
		fmt.Fprintf(out, "  Relay   %s：%s。交接兼容；对话协议不同系列，可能不兼容\n", relay, h.Version)
	default:
		access := "不需要访问令牌"
		if h.Access {
			access = "需要访问令牌"
		}
		fmt.Fprintf(out, "  Relay   %s：%s，兼容（%s）\n", relay, h.Version, access)
	}
	return 0
}

// padWidth pads s with spaces to w terminal columns, counting wide (CJK)
// characters as two.
func padWidth(s string, w int) string {
	n := 0
	for _, r := range s {
		if r >= 0x1100 && (r <= 0x115f || (r >= 0x2e80 && r <= 0xa4cf) || (r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) || (r >= 0xff00 && r <= 0xff60)) {
			n += 2
		} else {
			n++
		}
	}
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

func exists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Lstat(path)
	return err == nil
}

// within reports whether path lies inside dir.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func countFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return nil
	})
	return n
}
