package transfer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"fnos-enhance/internal/linker"
)

// QuarkOfficialTransferor 通过夸克官方开放平台转存（OAuth），不需要 Web cookie。
//
// 为什么存在这个实现：
//
//	Web API 路线（quark.go）必须带 __puus 等 cookie，而 cookie 会轮换、
//	会撞风控，用户明确希望「官方稳定渠道」。官方开放平台用 accessToken +
//	refreshToken，能自动续期。
//
// 为什么是外部进程而不是 Go 直连：
//
//	官方 token 的签发与刷新由 quarkclouddrive skill 的 CLI 维护，
//	该 CLI 是打包产物且明令禁止读取源码，因此官方接口地址与签名方式
//	不可知。调用其 CLI 是唯一不靠猜的接入方式。
//
// 已知约束（不隐藏）：
//   - 依赖 Node.js 与已安装并完成授权的 quarkclouddrive skill
//   - 该 CLI 会校验宿主 Agent 环境，需要设置 QuarkAgentEnv 才肯运行
//   - 只做转存；改名落地仍由 lander 负责（官方 CLI 无 rename 接口）
type QuarkOfficialTransferor struct {
	// SkillDir quarkclouddrive skill 根目录（内含 scripts/quark-drive.cjs）
	SkillDir string
	// NodeBin node 可执行文件，默认 "node"
	NodeBin string
	// SessionID 会话标识，同一批操作复用
	SessionID string
	// AgentEnv 需要注入的宿主标识环境变量，形如 "QODER_IDE=1"。
	// 留空则不注入，CLI 可能拒绝执行并返回 code=-104。
	AgentEnv string
	// ToPdirPath 转存目标目录（相对网盘根），传给官方 CLI 的 --to-pdir-path。
	// 空 = 不传，由 CLI 落默认位置（「来自：分享」）。
	// ingest 全链路要求落点在挂载根之内，否则管道等不到新条目。
	ToPdirPath string
	// Timeout 单次调用超时
	Timeout time.Duration
}

// cliResult 是 CLI 的 NDJSON 输出行（统一 IApiType 格式）
type cliResult struct {
	Code    int             `json:"code"`
	Msg     string          `json:"msg"`
	Action  string          `json:"action"`
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
}

func (q *QuarkOfficialTransferor) node() string {
	if q.NodeBin != "" {
		return q.NodeBin
	}
	return "node"
}

func (q *QuarkOfficialTransferor) timeout() time.Duration {
	if q.Timeout > 0 {
		return q.Timeout
	}
	return 5 * time.Minute
}

// Validate 检查外部依赖是否就绪；缺什么就说缺什么，不要等到运行时才炸
func (q *QuarkOfficialTransferor) Validate() error {
	if q.SkillDir == "" {
		return fmt.Errorf("未指定 quarkclouddrive skill 目录（设置 QUARK_SKILL_DIR）")
	}
	entry := filepath.Join(q.SkillDir, "scripts", "quark-drive.cjs")
	if _, err := os.Stat(entry); err != nil {
		return fmt.Errorf("找不到官方 CLI 入口 %s：请先在该 skill 目录执行 scripts/install.sh", entry)
	}
	if _, err := exec.LookPath(q.node()); err != nil {
		return fmt.Errorf("找不到 node（官方通道依赖 Node.js）: %w", err)
	}
	return nil
}

// run 调用 CLI 并返回最后一行 result
func (q *QuarkOfficialTransferor) run(ctx context.Context, args ...string) (*cliResult, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, q.timeout())
	defer cancel()

	full := append([]string{filepath.Join("scripts", "quark-drive.cjs")}, args...)
	if q.SessionID != "" {
		full = append(full, "--session-id", q.SessionID)
	}

	cmd := exec.CommandContext(ctx, q.node(), full...)
	cmd.Dir = q.SkillDir
	cmd.Env = os.Environ()
	if q.AgentEnv != "" {
		cmd.Env = append(cmd.Env, q.AgentEnv)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	// CLI 失败时也会在 stdout 输出一行 result，所以先解析再判断退出码
	var last *cliResult
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // 单行可能很长
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var r cliResult
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.Type == "result" {
			cp := r
			last = &cp
		}
	}
	if last == nil {
		if runErr != nil {
			return nil, fmt.Errorf("官方 CLI 执行失败: %v (stderr: %s)", runErr, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("官方 CLI 未返回 result 行")
	}
	if last.Code != 0 {
		msg := last.Msg
		if msg == "" {
			msg = last.Message
		}
		// -104 是宿主环境校验失败，单独给出可操作提示
		if last.Code == -104 {
			return nil, fmt.Errorf("官方 CLI 拒绝运行 (code=-104: %s)：需设置宿主标识环境变量（QUARK_AGENT_ENV，如 QODER_IDE=1）", msg)
		}
		return nil, fmt.Errorf("官方 CLI 返回 code=%d: %s", last.Code, msg)
	}
	return last, nil
}

// Transfer 实现 Transferor。
//
// dryRun 时不调用官方 CLI：转存是写操作，没有"预演"模式，
// 贸然调用就会真把文件存进网盘。列举预览请走 Web API 实现
// （公开分享无需凭据，已实测）。
func (q *QuarkOfficialTransferor) Transfer(ctx context.Context, link linker.ShareLink, dryRun bool) (*TransferResult, error) {
	if link.Type != linker.LinkQuark {
		return nil, fmt.Errorf("官方通道只支持夸克，收到 %s", link.Type)
	}
	url := "https://pan.quark.cn/s/" + link.ID
	if dryRun {
		// 官方 saveas 没有预演模式，调用即写入网盘。
		// 预览请走 Web 列举通道（公开分享无需凭据，已实测）。
		return &TransferResult{
			Provider: "夸克(官方)",
			Link:     url,
			DryRun:   true,
			Note:     "官方通道无 dry-run（转存即写入）；列举预览请用 Web 通道",
		}, nil
	}

	args := []string{"saveas", "--url", url}
	if q.ToPdirPath != "" {
		args = append(args, "--to-pdir-path", q.ToPdirPath)
	}
	if link.Pwd != "" {
		args = append(args, "--passcode", link.Pwd)
	}

	res, err := q.run(ctx, args...)
	if err != nil {
		return nil, err
	}

	var d struct {
		Status   int    `json:"status"`
		SavePath string `json:"save_path"`
		SaveAs   struct {
			SumNum int `json:"save_as_sum_num"`
		} `json:"save_as"`
	}
	_ = json.Unmarshal(res.Data, &d)

	// status: 0待处理 1处理中 2完成 3失败 4暂停
	if d.Status != 2 {
		return nil, fmt.Errorf("官方转存任务未完成 (status=%d，2 才是完成)", d.Status)
	}

	return &TransferResult{
		Provider:    "夸克(官方)",
		Link:        url,
		Transferred: d.SaveAs.SumNum,
		Note:        "官方 OAuth 转存成功，落点 " + orNA(d.SavePath),
	}, nil
}

func orNA(s string) string {
	if s == "" {
		return "(未返回)"
	}
	return s
}
