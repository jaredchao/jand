package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestGuideRendering(t *testing.T) {
	both := renderGuide("/opt/jand", "https://relay.example.com", "")
	for _, leftover := range []string{"__JAND__", "__RELAY_URL__", "packaging-note", "<!--", "BUILD-INFO", "QUICKSTART"} {
		if strings.Contains(both, leftover) {
			t.Fatalf("rendered guide still contains %q", leftover)
		}
	}
	if !strings.Contains(both, "/opt/jand chat recv") || !strings.Contains(both, "https://relay.example.com") ||
		!strings.Contains(both, "能在后台命令结束时唤起你") || !strings.Contains(both, "不能在后台命令结束时唤起你") {
		t.Fatal("guide without a mode must keep both ways of waiting")
	}
	bg := renderGuide("jand", "r", "background")
	poll := renderGuide("jand", "r", "poll")
	if strings.Contains(bg, "改为轮询") || !strings.Contains(bg, "放到后台运行") {
		t.Fatal("background guide")
	}
	if strings.Contains(poll, "放到后台运行，处理完事件") || !strings.Contains(poll, "改为轮询") || !strings.Contains(poll, "jand chat watch") {
		t.Fatal("poll guide")
	}
}

func TestHelpAgentTopic(t *testing.T) {
	t.Setenv("JAND_HOME", t.TempDir())
	var out, stderr bytes.Buffer
	if code := run(context.Background(), []string{"help", "agent"}, &out, &stderr); code != 0 || !strings.HasPrefix(out.String(), "# jand · 给 Agent 的使用说明") {
		t.Fatalf("help agent: %d %q", code, stderr.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"help"}, &out, &stderr); code != 0 || !strings.Contains(out.String(), "jand help agent") {
		t.Fatalf("help should point to the agent topic: %q", out.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"help", "template"}, &out, &stderr); code != 0 || !strings.HasPrefix(out.String(), "# 任务交接包") {
		t.Fatalf("help template: %q", out.String())
	}
	if code := run(context.Background(), []string{"help", "nope"}, &out, &stderr); code != 2 {
		t.Fatalf("unknown topic: %d", code)
	}
}
