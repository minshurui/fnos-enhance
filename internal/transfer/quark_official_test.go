package transfer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fnos-enhance/internal/linker"
)

// fakeSkill 造一个假的 skill 目录，用 shell 脚本冒充 node，
// 这样能在不依赖真实 CLI 的情况下验证参数拼装与输出解析。
func fakeSkill(t *testing.T, stdout string, exitCode int) *QuarkOfficialTransferor {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 入口文件只需存在，真正执行的是下面的假 node
	if err := os.WriteFile(filepath.Join(dir, "scripts", "quark-drive.cjs"), []byte("//stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeNode := filepath.Join(dir, "fakenode")
	script := "#!/bin/sh\ncat > \"" + dir + "/args.txt\" <<'EOF'\nEOF\nprintf '%s' \"$*\" > \"" + dir + "/args.txt\"\ncat <<'JSONEOF'\n" + stdout + "\nJSONEOF\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(fakeNode, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &QuarkOfficialTransferor{
		SkillDir: dir,
		NodeBin:  fakeNode,
		AgentEnv: "STUB_AGENT=1",
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	return string(rune('0' + i))
}

func readArgs(t *testing.T, q *QuarkOfficialTransferor) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(q.SkillDir, "args.txt"))
	if err != nil {
		return ""
	}
	return string(b)
}

// dry-run 绝不能真调 CLI —— 官方 saveas 没有预演模式，调用即写入网盘
func TestQuarkOfficial_DryRunNeverInvokesCLI(t *testing.T) {
	q := fakeSkill(t, `{"code":0,"msg":"成功","type":"result","data":{}}`, 0)
	res, err := q.Transfer(context.Background(),
		linker.ShareLink{Type: linker.LinkQuark, ID: "abc123"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun {
		t.Error("dry-run 结果必须标记 DryRun")
	}
	if got := readArgs(t, q); got != "" {
		t.Errorf("dry-run 竟然调用了 CLI（会真把文件写进网盘！）参数: %s", got)
	}
}

// 成功路径：status=2 才算完成，并解析出条目数与落点
func TestQuarkOfficial_SuccessParsesResult(t *testing.T) {
	out := `{"code":0,"msg":"成功","action":"saveas","type":"result","data":{"status":2,"save_path":"来自：分享","save_as":{"save_as_sum_num":22}}}`
	q := fakeSkill(t, out, 0)
	res, err := q.Transfer(context.Background(),
		linker.ShareLink{Type: linker.LinkQuark, ID: "d4dc6878059c"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Transferred != 22 {
		t.Errorf("条目数应为 22，实际 %d", res.Transferred)
	}
	if !strings.Contains(res.Note, "来自：分享") {
		t.Errorf("Note 应含落点路径，实际: %s", res.Note)
	}
	args := readArgs(t, q)
	if !strings.Contains(args, "saveas") || !strings.Contains(args, "d4dc6878059c") {
		t.Errorf("参数拼装不对: %s", args)
	}
}

// status 非 2 必须判失败，不能当成功——否则后续落地会扑空
func TestQuarkOfficial_NonCompleteStatusFails(t *testing.T) {
	out := `{"code":0,"msg":"成功","type":"result","data":{"status":3,"save_as":{"save_as_sum_num":0}}}`
	q := fakeSkill(t, out, 0)
	_, err := q.Transfer(context.Background(),
		linker.ShareLink{Type: linker.LinkQuark, ID: "x"}, false)
	if err == nil {
		t.Fatal("status=3(失败) 必须返回错误")
	}
	if !strings.Contains(err.Error(), "status=3") {
		t.Errorf("错误信息应带上 status，实际: %v", err)
	}
}

// -104 是宿主环境校验失败，必须给出可操作提示而非原样抛出
func TestQuarkOfficial_AgentGateGivesActionableHint(t *testing.T) {
	out := `{"code":-104,"msg":"无法识别当前 Agent 环境，禁止继续使用","type":"result","data":{}}`
	q := fakeSkill(t, out, 0)
	_, err := q.Transfer(context.Background(),
		linker.ShareLink{Type: linker.LinkQuark, ID: "x"}, false)
	if err == nil {
		t.Fatal("code=-104 必须报错")
	}
	if !strings.Contains(err.Error(), "QUARK_AGENT_ENV") {
		t.Errorf("应提示需设置宿主标识环境变量，实际: %v", err)
	}
}

// 非夸克链接必须拒绝，不能静默当成夸克处理
func TestQuarkOfficial_RejectsNonQuark(t *testing.T) {
	q := fakeSkill(t, `{"code":0,"type":"result","data":{}}`, 0)
	_, err := q.Transfer(context.Background(),
		linker.ShareLink{Type: linker.LinkBaidu, ID: "x"}, false)
	if err == nil {
		t.Fatal("百度链接走官方夸克通道必须报错")
	}
}

// 依赖缺失要在 Validate 就说清楚缺什么
func TestQuarkOfficial_ValidateReportsMissingDeps(t *testing.T) {
	q := &QuarkOfficialTransferor{}
	if err := q.Validate(); err == nil || !strings.Contains(err.Error(), "QUARK_SKILL_DIR") {
		t.Errorf("未配置目录时应提示 QUARK_SKILL_DIR，实际: %v", err)
	}
	q2 := &QuarkOfficialTransferor{SkillDir: t.TempDir()}
	if err := q2.Validate(); err == nil || !strings.Contains(err.Error(), "install.sh") {
		t.Errorf("缺 CLI 入口时应提示先跑 install.sh，实际: %v", err)
	}
}
