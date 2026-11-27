package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jaredchao/jand/internal/relay"
	"github.com/jaredchao/jand/internal/transfer"
)

func demoCommand(ctx context.Context, args []string, stdin io.Reader, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("jand demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	noOpen := fs.Bool("no-open", false, "do not open the generated chat view in a browser")
	fast := fs.Bool("fast", false, "run without pacing pauses")
	yes := fs.Bool("yes", false, "non-interactive: accept the default decision automatically")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(out, `jand demo — 单人本地演练：看两个模拟 Agent 通过本地加密中继对齐接口并等你拍板

用法：
  jand demo [--fast] [--no-open] [--yes]

说明：
  - 在本机启动临时内存 Relay 与独立沙盒目录，不联网、不改动你的 $JAND_HOME 配置与真实历史。
  - 完整演练：端到端加密交接 -> 请求/回复 -> 发现字段冲突 -> 停下来等你拍板 -> 新版取代旧版交付 -> 双方完成并打开对话页面。`)
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "用法：jand demo [--fast] [--no-open] [--yes]")
		return 2
	}

	pause := func(d time.Duration) {
		if *fast || d <= 0 {
			return
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}

	// Start a real local in-memory relay on an ephemeral loopback port.
	cfg := relay.DefaultConfig()
	cfg.Version = version + "-demo"
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	r := relay.New(cfg)
	defer r.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(stderr, "启动本地演练中继失败：%v\n", err)
		return 1
	}
	srv := &http.Server{Handler: r}
	go srv.Serve(ln)
	defer srv.Close()
	relayURL := "http://" + ln.Addr().String()

	// Isolated temporary workspace so real $JAND_HOME is never touched.
	root, err := os.MkdirTemp("", "jand-demo-*")
	if err != nil {
		fmt.Fprintf(stderr, "创建演练沙盒失败：%v\n", err)
		return 1
	}
	keepView := false
	defer func() {
		if !keepView {
			os.RemoveAll(root)
		}
	}()
	hostDir := filepath.Join(root, "host")
	guestDir := filepath.Join(root, "guest")
	inboxDir := filepath.Join(root, "inbox")
	briefPath := filepath.Join(root, "handoff-users-api.md")
	brief := `# 交接：对齐 GET /api/v1/users 响应字段

## 背景
前端新版个人中心正在联调用户列表接口，本地 mock 与后端实际返回存在字段差异。

## 目标
双方 AI 直接核对现有字段与时间格式，把需要业务拍板的兼容策略单独列出，最终产出一致的 schema。
`
	if err := os.WriteFile(briefPath, []byte(brief), 0600); err != nil {
		fmt.Fprintf(stderr, "写入演练交接包失败：%v\n", err)
		return 1
	}

	fmt.Fprintln(out, "=== jand demo · 单人本地演练 ===")
	fmt.Fprintln(out, "场景：模拟前端 Agent（我方）与后端 Agent（对方）跨机对齐 GET /api/v1/users 接口")
	fmt.Fprintf(out, "环境：本机临时加密 Relay（%s），不影响你的真实配置与历史。\n\n", relayURL)

	// 1. Host creates encrypted handoff + chat invitation.
	var inviteLink, inviteCode, chatID string
	hostOpts := func() transfer.Options {
		return transfer.Options{RelayURL: relayURL, StateDir: hostDir, Emit: func(e transfer.Event) {
			if e.Event == "queued" {
				inviteLink, inviteCode, chatID = e.Link, e.Code, e.Chat
			}
		}}
	}
	guestOpts := func() transfer.Options {
		return transfer.Options{RelayURL: relayURL, OutputDir: inboxDir, StateDir: guestDir, Emit: func(transfer.Event) {}}
	}
	recv := func(side string, opts transfer.Options) bool {
		if err := transfer.ChatRecv(ctx, chatID, 0, nil, opts); err != nil {
			fmt.Fprintf(stderr, "%s收取演练消息失败：%v\n", side, err)
			return false
		}
		return true
	}

	goal := "对齐 GET /api/v1/users 响应字段，解决命名与时间格式冲突，交付确认版 schema"
	sendOpts := hostOpts()
	sendOpts.Chat = true
	sendOpts.Goal = goal
	sendOpts.Budget = 20
	if err := transfer.Send(ctx, briefPath, sendOpts); err != nil {
		fmt.Fprintf(stderr, "发起演练失败：%v\n", err)
		return 1
	}
	fmt.Fprintf(out, "[1/6] 我方 Agent 加密上传交接包并生成一次性链接：\n      %s\n", inviteLink)
	pause(450 * time.Millisecond)

	// 2. Guest claims the one-time packet and joins after user approval.
	if _, err := transfer.Receive(ctx, inviteCode, guestOpts()); err != nil {
		fmt.Fprintf(stderr, "对方领取失败：%v\n", err)
		return 1
	}
	if _, err := transfer.ChatJoin(ctx, inviteCode, guestOpts()); err != nil {
		fmt.Fprintf(stderr, "对方加入失败：%v\n", err)
		return 1
	}
	fmt.Fprintln(out, "[2/6] 对方 Agent 领取并解包 → 对方用户确认目标后同意加入对话。")
	pause(450 * time.Millisecond)

	// Drain host's opened/joined notifications.
	if !recv("我方", hostOpts()) {
		return 1
	}

	// 3. Host sends request h1 and draft delivery h2.
	if err := transfer.ChatSend(ctx, chatID, transfer.ChatMessage{
		Kind: "request",
		Text: "前端期望 GET /api/v1/users 返回 {id, nickname, avatar_url, created_at(ISO-8601)}，请对照后端代码核对差异。",
	}, hostOpts()); err != nil {
		fmt.Fprintf(stderr, "发送 h1 失败：%v\n", err)
		return 1
	}
	if err := transfer.ChatSend(ctx, chatID, transfer.ChatMessage{
		Kind: "delivery",
		Text: "// users-schema-draft.json（前端初稿）\n{\n  \"id\": \"string\",\n  \"nickname\": \"string\",\n  \"avatar_url\": \"string\",\n  \"created_at\": \"2026-11-26T10:00:00Z\"\n}",
	}, hostOpts()); err != nil {
		fmt.Fprintf(stderr, "发送 h2 失败：%v\n", err)
		return 1
	}
	fmt.Fprintln(out, "[3/6] 我方 Agent 发出核对请求（h1）与前端初稿 schema（h2）。")
	pause(500 * time.Millisecond)

	// 4. Guest reads and replies with 2 concrete findings.
	if !recv("对方", guestOpts()) {
		return 1
	}
	if err := transfer.ChatSend(ctx, chatID, transfer.ChatMessage{
		Kind:    "reply",
		ReplyTo: "h1",
		Text:    "对照后端实现查出 2 处不一致：\n1. created_at 目前返回 Unix 秒时间戳，后端可以改成 ISO-8601，没有历史依赖。\n2. 昵称字段线上叫 display_name，旧版 App 仍在读取；直接改名会破坏老客户端。请你们用户拍板：双字段并存兼容，还是直接废弃 display_name？",
	}, guestOpts()); err != nil {
		fmt.Fprintf(stderr, "发送 g1 失败：%v\n", err)
		return 1
	}
	fmt.Fprintln(out, "[4/6] 对方 Agent 查出 2 处不一致（g1）：时间格式可直接统一，但字段命名涉及老客户端兼容，需要你拍板。")
	pause(450 * time.Millisecond)

	// 5. Host reads peer reply, pauses for the real human decision!
	if !recv("我方", hostOpts()) {
		return 1
	}
	choice := askDemoDecision(*yes, stdin, out)

	var decisionSummary, finalSchema string
	if choice == 2 {
		decisionSummary = "我方用户拍板：仅保留 nickname，下个版本废弃 display_name（由移动端同步发版）。"
		finalSchema = "// users-schema-final.json（确认版 · 仅保留 nickname）\n{\n  \"id\": \"string\",\n  \"nickname\": \"string\",\n  \"avatar_url\": \"string\",\n  \"created_at\": \"2026-11-26T10:00:00Z\"\n}"
	} else {
		decisionSummary = "我方用户拍板：兼容旧客户端，后端同时返回 display_name 与 nickname，时间统一为 ISO-8601。"
		finalSchema = "// users-schema-final.json（确认版 · 兼容双字段）\n{\n  \"id\": \"string\",\n  \"display_name\": \"string (deprecated, 兼容旧客户端)\",\n  \"nickname\": \"string\",\n  \"avatar_url\": \"string\",\n  \"created_at\": \"2026-11-26T10:00:00Z\"\n}"
	}

	// Host delivers final schema superseding h2, and replies to g1.
	if err := transfer.ChatSend(ctx, chatID, transfer.ChatMessage{
		Kind:       "delivery",
		Supersedes: "h2",
		Text:       finalSchema,
	}, hostOpts()); err != nil {
		fmt.Fprintf(stderr, "发送 h3 失败：%v\n", err)
		return 1
	}
	if err := transfer.ChatSend(ctx, chatID, transfer.ChatMessage{
		Kind:    "reply",
		ReplyTo: "g1",
		Text:    decisionSummary + " 终版 schema 已作为 h3 交付（取代初稿 h2），请确认。",
	}, hostOpts()); err != nil {
		fmt.Fprintf(stderr, "发送 h4 失败：%v\n", err)
		return 1
	}
	fmt.Fprintf(out, "[5/6] 我方 Agent 按你的拍板交付终版 schema（h3 取代 h2），并通知对方。\n")
	pause(450 * time.Millisecond)

	// 6. Guest confirms and both sides report done -> Relay checkpoint -> Close.
	if !recv("对方", guestOpts()) {
		return 1
	}
	if err := transfer.ChatSend(ctx, chatID, transfer.ChatMessage{
		Kind:    "reply",
		ReplyTo: "h4",
		Text:    "确认收到终版 h3，后端契约测试已按最终口径对齐通过。",
	}, guestOpts()); err != nil {
		fmt.Fprintf(stderr, "发送 g2 失败：%v\n", err)
		return 1
	}
	if err := transfer.ChatDone(ctx, chatID, "已核对 2 处字段差异并按对方用户拍板更新契约测试，确认版见 h3。", guestOpts()); err != nil {
		fmt.Fprintf(stderr, "对方 done 失败：%v\n", err)
		return 1
	}
	if !recv("我方", hostOpts()) {
		return 1
	}
	if err := transfer.ChatDone(ctx, chatID, decisionSummary+" 终版 schema（h3，取代 h2）已由双方确认一致。", hostOpts()); err != nil {
		fmt.Fprintf(stderr, "我方 done 失败：%v\n", err)
		return 1
	}
	// Drain checkpoint event and close cleanly.
	if !recv("我方", hostOpts()) {
		return 1
	}
	if err := transfer.ChatClose(ctx, chatID, hostOpts()); err != nil {
		fmt.Fprintf(stderr, "关闭演练对话失败：%v\n", err)
		return 1
	}
	fmt.Fprintln(out, "[6/6] 双方均报告完成 → Relay 触发验收检查点 → 对话圆满结束。")

	// Render real view.html for the host side.
	summary, lines, err := transfer.ChatHistory(hostDir, chatID)
	if err != nil {
		fmt.Fprintf(stderr, "读取演练记录失败：%v\n", err)
		return 1
	}
	htmlPath := filepath.Join(root, "demo-view.html")
	if err := writeView(htmlPath, summary, lines, nil, os.ErrNotExist); err != nil {
		fmt.Fprintf(stderr, "生成对话页面失败：%v\n", err)
		return 1
	}
	keepView = true

	fmt.Fprintf(out, "\n演练完成！对话视图已生成：\n  %s\n", htmlPath)
	if !*noOpen && hasDisplay() {
		if err := openPage(htmlPath); err == nil {
			fmt.Fprintln(out, "已在浏览器中打开这次协作的全程视图。")
		}
	}
	fmt.Fprintln(out, "\n下一步：对你的 AI 说「用 jand 和同事的 AI 对一下今天的任务」，实战体验一次。")
	return 0
}

func askDemoDecision(nonInteractive bool, stdin io.Reader, out io.Writer) int {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, ">>> 需要你拍板（对方 AI 无权替你决定）：")
	fmt.Fprintln(out, "    [1] 兼容旧客户端：同时返回 display_name 和 nickname（推荐，直接回车）")
	fmt.Fprintln(out, "    [2] 坚持原方案：只保留 nickname，下个版本废弃 display_name")
	if nonInteractive || !isTerminalReader(stdin) {
		fmt.Fprintln(out, "    （非交互模式，自动选择 [1]）")
		fmt.Fprintln(out, "")
		return 1
	}
	fmt.Fprint(out, "    请输入 1 或 2 [默认 1]: ")
	sc := bufio.NewScanner(stdin)
	if sc.Scan() && strings.TrimSpace(sc.Text()) == "2" {
		fmt.Fprintln(out, "")
		return 2
	}
	fmt.Fprintln(out, "")
	return 1
}

func isTerminalReader(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}
