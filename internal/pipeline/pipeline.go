package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fnos-enhance/internal/lander"
	"fnos-enhance/internal/linker"
	"fnos-enhance/internal/transfer"
)

// Pipeline 把「转存 → 挂载可见 → 落地改名」串成一条链。
//
// 审计 P0-1（管道断裂）修复：在此之前 transfer 和 land 是两个互不相连
// 的 CLI 子命令，用户必须手工在中间等待并观察挂载，等于管道没接上。
//
// 链路中真实存在、必须显式处理的卡点：
//
//	transfer 提交成功 ≠ 文件在挂载里可见
//	  rclone WebDAV / CloudFS 都有目录缓存（默认几十秒到几分钟），
//	  转存刚完成时挂载里往往还看不到新目录。所以中间必须 **轮询等待**，
//	  不能 transfer 完就直接 land（会扫到空目录，静默无事发生）。
type Pipeline struct {
	Transferor transfer.Transferor
	Lander     *lander.Lander

	// MountRoot 落地基准（影视目录根）
	MountRoot string
	// SourceDir 转存落点（相对 MountRoot），留空则为挂载根
	SourceDir string

	// WaitTimeout 等待挂载出现新条目的上限，0 用默认 5 分钟
	WaitTimeout time.Duration
	// PollInterval 轮询间隔，0 用默认 10 秒
	PollInterval time.Duration

	// Logf 进度输出，nil 则丢弃
	Logf func(format string, a ...interface{})
}

// StageResult 单条链路的结果
type StageResult struct {
	Link linker.ShareLink

	Transfer *transfer.TransferResult
	Land     *lander.Result

	// Appeared 转存后在挂载里真正出现的顶层条目
	Appeared []string
	// Stage 失败发生在哪一步："transfer" / "wait" / "plan" / "land"，成功为 ""
	Stage string
	Err   error
}

func (p *Pipeline) logf(format string, a ...interface{}) {
	if p.Logf != nil {
		p.Logf(format, a...)
	}
}

func (p *Pipeline) waitTimeout() time.Duration {
	if p.WaitTimeout > 0 {
		return p.WaitTimeout
	}
	return 5 * time.Minute
}

func (p *Pipeline) pollInterval() time.Duration {
	if p.PollInterval > 0 {
		return p.PollInterval
	}
	return 10 * time.Second
}

func (p *Pipeline) srcRoot() string {
	if p.SourceDir == "" {
		return p.MountRoot
	}
	return filepath.Join(p.MountRoot, p.SourceDir)
}

// Run 处理一批链接。dryRun=true 时既不转存也不改名，只走通链路并报告。
//
// 返回每条链接的结果；单条失败不中断其余链接。
func (p *Pipeline) Run(ctx context.Context, links []linker.ShareLink, dryRun bool) []StageResult {
	out := make([]StageResult, 0, len(links))
	for i, l := range links {
		p.logf("[%d/%d] %s %s\n", i+1, len(links), l.Type, l.Link)
		out = append(out, p.runOne(ctx, l, dryRun))
	}
	return out
}

func (p *Pipeline) runOne(ctx context.Context, l linker.ShareLink, dryRun bool) StageResult {
	sr := StageResult{Link: l}

	// ---- 阶段 1: 转存 ----
	before, err := p.snapshot()
	if err != nil {
		sr.Stage, sr.Err = "transfer", fmt.Errorf("无法读取挂载目录 %s: %w（挂载是否掉了？）", p.srcRoot(), err)
		return sr
	}

	tres, err := p.Transferor.Transfer(ctx, l, dryRun)
	if err != nil {
		sr.Stage, sr.Err = "transfer", err
		return sr
	}
	sr.Transfer = tres
	p.logf("  ✓ 转存: %s | 顶层 %d 项 | 递归 %d 文件\n",
		tres.Provider, len(tres.Names), tres.FileCount())

	if dryRun {
		// dry-run 不会真的产生新文件，直接对现有目录做一次落地规划，
		// 让用户看到"如果转存成功会被改成什么样"
		return p.planOnly(ctx, sr)
	}

	// ---- 阶段 2: 等挂载可见（真实卡点，不能省）----
	appeared, err := p.waitForAppear(ctx, before, tres.Names)
	sr.Appeared = appeared
	if err != nil {
		sr.Stage, sr.Err = "wait", err
		return sr
	}
	p.logf("  ✓ 挂载已可见 %d 项: %s\n", len(appeared), strings.Join(truncateList(appeared, 3), ", "))

	// ---- 阶段 3+4: 规划 + 落地 ----
	plans, err := p.Lander.PlanFromDir(ctx, p.MountRoot, "")
	if err != nil {
		sr.Stage, sr.Err = "plan", err
		return sr
	}
	// 只对本次真正新出现的条目落地，避免动到无关旧数据
	plans = filterPlansByTopLevel(plans, appeared, p.srcRoot())
	if len(plans) == 0 {
		sr.Stage, sr.Err = "plan", fmt.Errorf("新出现 %d 项但规划为空（可能都已是规范命名，或分类无法识别）", len(appeared))
		return sr
	}
	p.logf("  → 规划 %d 个文件待改名\n", len(plans))

	lres, err := p.Lander.Execute(ctx, plans)
	if err != nil {
		sr.Stage, sr.Err = "land", err
		return sr
	}
	sr.Land = lres
	p.logf("  ✓ 落地: %s\n", lres.Summary())
	return sr
}

// planOnly dry-run 分支：不等待，直接对现有目录规划
func (p *Pipeline) planOnly(ctx context.Context, sr StageResult) StageResult {
	plans, err := p.Lander.PlanFromDir(ctx, p.MountRoot, "")
	if err != nil {
		sr.Stage, sr.Err = "plan", err
		return sr
	}
	p.logf("  → [dry-run] 当前目录可规划 %d 个文件\n", len(plans))
	sr.Land = &lander.Result{Plans: plans}
	return sr
}

// snapshot 记录源目录当前的顶层条目名
func (p *Pipeline) snapshot() (map[string]bool, error) {
	ents, err := os.ReadDir(p.srcRoot())
	if err != nil {
		return nil, err
	}
	m := make(map[string]bool, len(ents))
	for _, e := range ents {
		m[e.Name()] = true
	}
	return m, nil
}

// waitForAppear 轮询等待新条目出现在挂载里
//
// 优先等 expect（转存返回的名字）出现；网盘可能改名（重名加后缀），
// 所以也接受"任何新出现的条目"。
func (p *Pipeline) waitForAppear(ctx context.Context, before map[string]bool, expect []string) ([]string, error) {
	deadline := time.Now().Add(p.waitTimeout())
	want := make(map[string]bool, len(expect))
	for _, n := range expect {
		want[n] = true
	}

	ticker := time.NewTicker(p.pollInterval())
	defer ticker.Stop()

	var lastErr error
	for {
		now, err := p.snapshot()
		if err != nil {
			lastErr = err // 挂载可能瞬时抖动，继续重试
		} else {
			var fresh []string
			hitExpected := 0
			for name := range now {
				if before[name] {
					continue
				}
				fresh = append(fresh, name)
				if want[name] {
					hitExpected++
				}
			}
			// 期望的名字全到齐 → 立刻返回
			if len(want) > 0 && hitExpected == len(want) {
				return fresh, nil
			}
			// 出现了新条目但名字不完全匹配（网盘改名）→ 再等一轮确认稳定，
			// 避免在目录还在陆续出现时就开始改名
			if len(fresh) > 0 {
				select {
				case <-ctx.Done():
					return fresh, ctx.Err()
				case <-time.After(p.pollInterval()):
				}
				if after, err2 := p.snapshot(); err2 == nil {
					var stable []string
					for name := range after {
						if !before[name] {
							stable = append(stable, name)
						}
					}
					if len(stable) == len(fresh) {
						return stable, nil
					}
					fresh = stable
				}
			}
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				return nil, fmt.Errorf("等待挂载刷新超时（%v），且读目录持续失败: %w", p.waitTimeout(), lastErr)
			}
			return nil, fmt.Errorf("等待挂载刷新超时（%v）：转存已提交但挂载里看不到新文件。"+
				"常见原因：(1) rclone 目录缓存未过期，可降低 --dir-cache-time 或手工 rclone rc vfs/forget；"+
				"(2) 转存目标目录与挂载路径 %s 不是同一个目录", p.waitTimeout(), p.srcRoot())
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// filterPlansByTopLevel 只保留源路径落在本次新出现顶层条目内的计划
func filterPlansByTopLevel(plans []*lander.Plan, appeared []string, srcRoot string) []*lander.Plan {
	if len(appeared) == 0 {
		return nil
	}
	prefixes := make([]string, 0, len(appeared))
	for _, n := range appeared {
		prefixes = append(prefixes, filepath.Join(srcRoot, n))
	}
	var out []*lander.Plan
	for _, pl := range plans {
		for _, pre := range prefixes {
			if pl.SourcePath == pre || strings.HasPrefix(pl.SourcePath, pre+string(filepath.Separator)) {
				out = append(out, pl)
				break
			}
		}
	}
	return out
}

func truncateList(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string{}, s[:n]...), fmt.Sprintf("...共%d项", len(s)))
}

// Summary 汇总一批结果
func Summary(rs []StageResult) string {
	var ok, failed, renamed int
	byStage := map[string]int{}
	for _, r := range rs {
		if r.Err != nil {
			failed++
			byStage[r.Stage]++
			continue
		}
		ok++
		if r.Land != nil {
			renamed += len(r.Land.Moved)
		}
	}
	s := fmt.Sprintf("链路完成 | 成功 %d | 失败 %d | 累计改名 %d 个文件", ok, failed, renamed)
	if failed > 0 {
		var parts []string
		for st, n := range byStage {
			parts = append(parts, fmt.Sprintf("%s×%d", st, n))
		}
		s += " | 失败阶段: " + strings.Join(parts, " ")
	}
	return s
}
