package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fnos-enhance/internal/lander"
	"fnos-enhance/internal/linker"
	"fnos-enhance/internal/transfer"
)

// fakeTransferor 可控的假转存器；转存"成功"后按 delay 再让文件出现在目录里，
// 用来复现真实卡点：转存提交成功 ≠ 挂载立刻可见。
type fakeTransferor struct {
	provider string
	names    []string
	err      error

	// appearIn 转存后多久让文件出现在 dir 里（模拟 rclone 目录缓存）
	appearIn time.Duration
	dir      string
	calls    atomic.Int32
	sawDry   atomic.Bool
}

func (f *fakeTransferor) Transfer(ctx context.Context, l linker.ShareLink, dryRun bool) (*transfer.TransferResult, error) {
	f.calls.Add(1)
	if dryRun {
		f.sawDry.Store(true)
	}
	if f.err != nil {
		return nil, f.err
	}
	if !dryRun && f.dir != "" {
		names := append([]string(nil), f.names...)
		dir, delay := f.dir, f.appearIn
		go func() {
			time.Sleep(delay)
			for _, n := range names {
				os.MkdirAll(filepath.Join(dir, n), 0o755)
				os.WriteFile(filepath.Join(dir, n, "S01E01.mp4"), []byte("x"), 0o644)
			}
		}()
	}
	return &transfer.TransferResult{
		Provider: f.provider, Link: l.Link, Names: f.names, DryRun: dryRun,
	}, nil
}

func newTestPipeline(t *testing.T, ft *fakeTransferor) (*Pipeline, string) {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "动漫")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	ft.dir = src

	l := lander.New(lander.Config{
		MountRoot: root,
		SourceDir: "动漫",
		DryRun:    false,
	})
	return &Pipeline{
		Transferor:   ft,
		Lander:       l,
		MountRoot:    root,
		SourceDir:    "动漫",
		WaitTimeout:  3 * time.Second,
		PollInterval: 50 * time.Millisecond,
	}, src
}

// 核心回归：转存后挂载有延迟，管道必须等，不能扫到空目录就当没事发生
func TestPipeline_WaitsForMountCacheDelay(t *testing.T) {
	ft := &fakeTransferor{
		provider: "夸克",
		names:    []string{"万古至尊：李云霄传"},
		appearIn: 400 * time.Millisecond, // 挂载 400ms 后才可见
	}
	p, _ := newTestPipeline(t, ft)

	res := p.Run(context.Background(),
		[]linker.ShareLink{{Type: linker.LinkQuark, ID: "abc", Link: "pan.quark.cn/s/abc"}}, false)

	if len(res) != 1 {
		t.Fatalf("期望 1 条结果，得到 %d", len(res))
	}
	r := res[0]
	if r.Err != nil {
		t.Fatalf("管道失败于 %s: %v", r.Stage, r.Err)
	}
	if len(r.Appeared) == 0 {
		t.Error("未等到挂载出现新条目——旧行为会直接 land 到空目录并静默成功")
	}
	if r.Land == nil || len(r.Land.Moved) == 0 {
		t.Errorf("未发生实际改名: %+v", r.Land)
	}
}

func TestPipeline_TimeoutGivesActionableError(t *testing.T) {
	// 转存"成功"但文件永远不出现（挂载路径配错 / 缓存不过期）
	ft := &fakeTransferor{provider: "夸克", names: []string{"永不出现"}, appearIn: time.Hour}
	p, _ := newTestPipeline(t, ft)
	p.WaitTimeout = 300 * time.Millisecond

	res := p.Run(context.Background(), []linker.ShareLink{{Type: linker.LinkQuark, ID: "a"}}, false)
	r := res[0]
	if r.Err == nil {
		t.Fatal("应超时报错")
	}
	if r.Stage != "wait" {
		t.Errorf("失败阶段应为 wait，得到 %q", r.Stage)
	}
	// 错误必须可操作：告诉用户去查什么
	msg := r.Err.Error()
	for _, kw := range []string{"超时", "dir-cache-time", "挂载"} {
		if !contains(msg, kw) {
			t.Errorf("错误信息缺少排查线索 %q: %s", kw, msg)
		}
	}
}

func TestPipeline_TransferFailureStopsBeforeLanding(t *testing.T) {
	ft := &fakeTransferor{provider: "夸克", err: errors.New("分享已失效")}
	p, src := newTestPipeline(t, ft)

	// 先放一个无关的旧文件，确认转存失败时它不会被动到
	untouched := filepath.Join(src, "旧剧集")
	os.MkdirAll(untouched, 0o755)
	os.WriteFile(filepath.Join(untouched, "a.mp4"), []byte("x"), 0o644)

	res := p.Run(context.Background(), []linker.ShareLink{{Type: linker.LinkQuark, ID: "a"}}, false)
	r := res[0]
	if r.Err == nil || r.Stage != "transfer" {
		t.Fatalf("应在 transfer 阶段失败，得到 stage=%q err=%v", r.Stage, r.Err)
	}
	if r.Land != nil {
		t.Error("转存失败后绝不能进入落地阶段")
	}
	if _, err := os.Stat(filepath.Join(untouched, "a.mp4")); err != nil {
		t.Error("转存失败却动了无关旧文件")
	}
}

func TestPipeline_DryRunTouchesNothing(t *testing.T) {
	ft := &fakeTransferor{provider: "夸克", names: []string{"某剧"}}
	p, src := newTestPipeline(t, ft)

	orig := filepath.Join(src, "后室 (2026)")
	os.MkdirAll(orig, 0o755)
	os.WriteFile(filepath.Join(orig, "S01E01.mp4"), []byte("x"), 0o644)

	res := p.Run(context.Background(), []linker.ShareLink{{Type: linker.LinkQuark, ID: "a"}}, true)
	if res[0].Err != nil {
		t.Fatalf("dry-run 不应失败: %v", res[0].Err)
	}
	if !ft.sawDry.Load() {
		t.Error("dryRun 标志未传给转存器")
	}
	if _, err := os.Stat(filepath.Join(orig, "S01E01.mp4")); err != nil {
		t.Error("dry-run 动了真实文件")
	}
}

func TestPipeline_DryRunHonorsCategoryHint(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "staging", "雄狮少年2")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "4K.高码.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	l := lander.New(lander.Config{MountRoot: root, SourceDir: "staging", DryRun: true})
	p := &Pipeline{
		Transferor:   &fakeTransferor{provider: "夸克"},
		Lander:       l,
		MountRoot:    root,
		SourceDir:    "staging",
		CategoryHint: "电影",
	}
	res := p.Run(context.Background(), []linker.ShareLink{{Type: linker.LinkQuark, ID: "a"}}, true)
	if res[0].Err != nil {
		t.Fatalf("dry-run 失败: %v", res[0].Err)
	}
	if res[0].Land == nil || len(res[0].Land.Plans) != 1 {
		t.Fatalf("期望 1 条规划，得到 %+v", res[0].Land)
	}
	wantPrefix := filepath.Join(root, "电影") + string(filepath.Separator)
	if !strings.HasPrefix(res[0].Land.Plans[0].TargetPath, wantPrefix) {
		t.Fatalf("--cat 电影未生效，目标路径: %s", res[0].Land.Plans[0].TargetPath)
	}
}

// 只对本次新出现的条目落地，不能顺手改动无关的旧数据
func TestPipeline_OnlyLandsNewEntries(t *testing.T) {
	ft := &fakeTransferor{
		provider: "夸克",
		names:    []string{"新剧 S01"},
		appearIn: 50 * time.Millisecond,
	}
	p, src := newTestPipeline(t, ft)

	// 一个命名不规范的旧目录：如果管道不做过滤，它也会被改名
	oldDir := filepath.Join(src, "W-老剧集")
	os.MkdirAll(oldDir, 0o755)
	oldFile := filepath.Join(oldDir, "S01E01.mp4")
	os.WriteFile(oldFile, []byte("x"), 0o644)

	res := p.Run(context.Background(), []linker.ShareLink{{Type: linker.LinkQuark, ID: "a"}}, false)
	if res[0].Err != nil {
		t.Fatalf("管道失败于 %s: %v", res[0].Stage, res[0].Err)
	}
	if _, err := os.Stat(oldFile); err != nil {
		t.Errorf("无关旧文件被改名了（应只处理本次新增）: %v", err)
	}
	for _, m := range res[0].Land.Moved {
		if contains(m, "老剧集") {
			t.Errorf("落地范围溢出到旧数据: %s", m)
		}
	}
}

func TestPipeline_MissingMountReportsClearly(t *testing.T) {
	ft := &fakeTransferor{provider: "夸克", names: []string{"x"}}
	p, _ := newTestPipeline(t, ft)
	p.MountRoot = "/definitely/not/mounted/12345"

	res := p.Run(context.Background(), []linker.ShareLink{{Type: linker.LinkQuark, ID: "a"}}, false)
	if res[0].Err == nil {
		t.Fatal("挂载不存在时应报错，而不是当成空目录")
	}
	if !contains(res[0].Err.Error(), "挂载") {
		t.Errorf("错误应指向挂载问题: %v", res[0].Err)
	}
	if ft.calls.Load() != 0 {
		t.Error("挂载不可用时不应先去转存（会转存成功但无处落地）")
	}
}

func TestPipeline_ContinuesAfterOneLinkFails(t *testing.T) {
	ft := &fakeTransferor{provider: "夸克", err: errors.New("失效")}
	p, _ := newTestPipeline(t, ft)

	res := p.Run(context.Background(), []linker.ShareLink{
		{Type: linker.LinkQuark, ID: "a"},
		{Type: linker.LinkQuark, ID: "b"},
	}, false)
	if len(res) != 2 {
		t.Errorf("单条失败不应中断其余链接，期望 2 条结果，得到 %d", len(res))
	}
}

func TestSummary(t *testing.T) {
	rs := []StageResult{
		{Land: &lander.Result{Moved: []string{"a", "b"}}},
		{Stage: "wait", Err: errors.New("超时")},
	}
	s := Summary(rs)
	for _, kw := range []string{"成功 1", "失败 1", "改名 2", "wait"} {
		if !contains(s, kw) {
			t.Errorf("汇总缺少 %q: %s", kw, s)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
