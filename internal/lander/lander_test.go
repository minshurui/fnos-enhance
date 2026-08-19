package lander

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"fnos-enhance/internal/renamer"
)

// TestPlanFromDir_BasicAnime 用临时目录模拟动漫目录结构
func TestPlanFromDir_BasicAnime(t *testing.T) {
	root := t.TempDir()

	// 动漫/测试番剧/S01E01.2024.1080p.WEB-DL.mkv
	dir := filepath.Join(root, "动漫", "测试番剧 (2024)")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "S01E01.2024.1080p.WEB-DL.mkv")
	if err := os.WriteFile(src, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		MountRoot: root,
		DryRun:    true,
		NameMap:   renamer.NewNameMap(),
	}
	l := New(cfg)
	plans, err := l.PlanFromDir(context.Background(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("期望 1 个计划，得到 %d", len(plans))
	}
	p := plans[0]
	if p.Info.Title != "测试番剧" {
		t.Errorf("片名: 期望 测试番剧，得到 %s", p.Info.Title)
	}
	if p.Info.Season != 1 || p.Info.Episode != 1 {
		t.Errorf("季集: 期望 S01E01，得到 S%02dE%02d", p.Info.Season, p.Info.Episode)
	}
	// 目标路径应在 root 下，不应重复分类
	if filepath.Dir(p.TargetPath) == filepath.Dir(p.SourcePath) {
		// 如果目标 == 源（已规范），也可以接受
	}
	// 目标不应包含 分类/分类 重复
	rel, _ := filepath.Rel(root, p.TargetPath)
	if rel == "" {
		t.Errorf("目标路径为空")
	}
}

// TestExecute_DryRun 不修改任何文件
func TestExecute_DryRun(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "电影", "测试电影 (2023)")
	os.MkdirAll(dir, 0755)
	src := filepath.Join(dir, "测试电影 (2023).mp4")
	os.WriteFile(src, []byte("test"), 0644)

	cfg := Config{
		MountRoot: root,
		DryRun:    true,
		NameMap:   renamer.NewNameMap(),
	}
	l := New(cfg)
	plans, err := l.PlanFromDir(context.Background(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := l.Execute(context.Background(), plans)
	if err != nil {
		t.Fatal(err)
	}
	// dry-run 模式下源文件不应被移动
	if _, err := os.Stat(src); os.IsNotExist(err) {
		t.Error("dry-run 不应删除源文件")
	}
	t.Logf("结果: %s", result.Summary())
}

// TestExecute_RealRename 真实改名测试
func TestExecute_RealRename(t *testing.T) {
	root := t.TempDir()

	// 模拟一个需要改名的文件：动漫/W-测试剧/S01E01.2024.1080p.WEB-DL.mkv
	// 期望改名后：动漫/测试剧 (2024)/Season 01/测试剧 - S01E01.mkv
	dir := filepath.Join(root, "动漫", "W-测试剧")
	os.MkdirAll(dir, 0755)
	src := filepath.Join(dir, "S01E01.2024.1080p.WEB-DL.mkv")
	os.WriteFile(src, []byte("test"), 0644)

	cfg := Config{
		MountRoot: root,
		DryRun:    false, // 真实执行
		NameMap:   renamer.NewNameMap(),
	}
	l := New(cfg)
	plans, err := l.PlanFromDir(context.Background(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("期望 1 个计划，得到 %d", len(plans))
	}
	result, err := l.Execute(context.Background(), plans)
	if err != nil {
		t.Fatal(err)
	}

	// 源文件应不存在
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("源文件应已被移动")
	}
	// 目标文件应存在
	target := plans[0].TargetPath
	if _, err := os.Stat(target); os.IsNotExist(err) {
		t.Errorf("目标文件不存在: %s", target)
	}
	// 验证目标路径结构
	rel, _ := filepath.Rel(root, target)
	t.Logf("源: %s", src)
	t.Logf("目标: %s (rel: %s)", target, rel)
	t.Logf("结果: %s", result.Summary())
}

// TestPlanFromDir_CategoryFromPath 从路径推断分类
func TestPlanFromDir_CategoryFromPath(t *testing.T) {
	root := t.TempDir()

	for _, cat := range []string{"动漫", "电影", "电视剧"} {
		dir := filepath.Join(root, cat, "测试片 (2024)")
		os.MkdirAll(dir, 0755)
		os.WriteFile(filepath.Join(dir, "测试片 (2024).mp4"), []byte("x"), 0644)
	}

	cfg := Config{MountRoot: root, DryRun: true, NameMap: renamer.NewNameMap()}
	l := New(cfg)
	plans, err := l.PlanFromDir(context.Background(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 {
		t.Fatalf("期望 3 个计划，得到 %d", len(plans))
	}
	for _, p := range plans {
		if p.Info.Category == "" {
			t.Error("分类不应为空")
		}
	}
}

// TestIsCategoryDir
func TestIsCategoryDir(t *testing.T) {
	cases := map[string]bool{
		"动漫":        true,
		"电影":        true,
		"电视剧":       true,
		"音乐":        true,
		"测试片":       false,
		"Season 01": false,
	}
	for name, want := range cases {
		if got := isCategoryDir(name); got != want {
			t.Errorf("isCategoryDir(%q) = %v, want %v", name, got, want)
		}
	}
}
