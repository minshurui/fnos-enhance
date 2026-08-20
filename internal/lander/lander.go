package lander

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"fnos-enhance/internal/linker"
	"fnos-enhance/internal/renamer"
)

// Plan 单个文件的落地计划
type Plan struct {
	SourcePath string // 源路径（rclone 挂载上的当前路径）
	TargetPath string // 目标路径（renamer.BuildPath() 输出）
	Info       *renamer.MediaInfo
	Note       string // 附加说明（如"消歧补标签"）
}

// Result 落地结果
type Result struct {
	Plans   []*Plan
	Moved   []string // 成功移动的文件
	Failed  []FailedFile
	Skipped []string // 跳过的文件（NeedsReview 或碰撞）
}

// FailedFile 失败记录
type FailedFile struct {
	Path  string
	Error string
}

// Config 落地器配置
type Config struct {
	// MountRoot rclone 挂载的影视目录根路径（目标路径以此为基准）
	// 例如夸克: /vol02/1000-1-a92fbdbc/影视
	MountRoot string
	// MountPaths 各网盘的 rclone 挂载根路径（兼容旧接口）
	MountPaths map[linker.LinkType]string
	// SourceDir 转存后的文件所在目录（相对挂载根，如 "0_待整理"）
	// 留空则扫描挂载根本身
	SourceDir string
	// DryRun 只规划不执行（默认 true，安全第一）
	DryRun bool
	// NameMap 乱码名映射表（可为 nil）
	NameMap *renamer.NameMap
	// TMDBClient TMDB 客户端（可为 nil）
	TMDBClient *renamer.TMDBClient

	// AllowNoTMDB 为 true 时，TMDB 识别失败仍然落地（默认 false = 跳过）
	AllowNoTMDB bool
}

// Lander 落地器
type Lander struct {
	cfg Config
}

func New(cfg Config) *Lander {
	if cfg.MountPaths == nil {
		cfg.MountPaths = make(map[linker.LinkType]string)
	}
	return &Lander{cfg: cfg}
}

// PlanFromDir 扫描源目录，生成落地计划（只读，不执行）
// mountRoot: rclone 挂载的影视目录根路径（目标路径以此为基准）
// categoryHint: 源目录的一级分类（动漫/电影/电视剧/音乐），留空则从路径推断
func (l *Lander) PlanFromDir(ctx context.Context, mountRoot, categoryHint string) ([]*Plan, error) {
	// 目标路径始终相对于 mountRoot（影视目录根），避免分类重复
	srcRoot := mountRoot
	if l.cfg.SourceDir != "" {
		srcRoot = filepath.Join(mountRoot, l.cfg.SourceDir)
	}

	var plans []*Plan
	var allInfos []*renamer.MediaInfo // 用于批量消歧

	type pending struct {
		plan *Plan
	}
	var pendings []pending

	err := filepath.Walk(srcRoot, func(fullPath string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}

		// 跳过隐藏文件
		if strings.HasPrefix(filepath.Base(fullPath), ".") {
			return nil
		}

		// 从完整路径提取分类/剧名目录/中间层/文件名
		rel, err := filepath.Rel(srcRoot, fullPath)
		if err != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 1 {
			return nil
		}

		var cat renamer.Category
		var dir, sub, file string

		// 如果路径以分类开头（动漫/电影/...），剥掉分类段
		if len(parts) > 1 && renamer.Category(parts[0]) != "" {
			cat = renamer.Category(parts[0])
			parts = parts[1:]
		} else if categoryHint != "" {
			cat = renamer.Category(categoryHint)
		} else {
			cat = renamer.CatAnime // 默认动漫（语料 95% 是动漫）
		}

		switch {
		case len(parts) >= 3:
			// 剧名目录/中间层/文件名
			dir, sub, file = parts[0], parts[1], parts[2]
		case len(parts) >= 2:
			// 剧名目录/文件名（无中间层）
			dir, sub, file = parts[0], "", parts[1]
		default:
			// 根目录散文件：用扫描根的目录名作为剧名目录
			dir = filepath.Base(srcRoot)
			if dir == "." || dir == "/" {
				return nil
			}
			sub = ""
		}

		file = filepath.Base(fullPath)
		info := renamer.ParsePackage(cat, dir, sub, file)

		// 应用乱码名映射
		if l.cfg.NameMap != nil {
			l.cfg.NameMap.Apply(info)
		}

		// TMDB 补全
		// 用户决策：认不出就跳过，留给人工。不能无 TMDB 也照落地——
		// 否则飞牛刮不出海报，而云端改名不可回滚，还得人工改回来。
		if l.cfg.TMDBClient != nil {
			if err := l.cfg.TMDBClient.Enrich(ctx, info); err != nil {
				fmt.Fprintf(os.Stderr, "警告: TMDB 补全失败 [%s] (%v)\n", info.Title, err)
			}
			if info.TMDBID == 0 && !l.cfg.AllowNoTMDB {
				info.NeedsReview = true
				info.ReviewReason = "TMDB 未识别，已跳过（确认后可用 NAME_MAP 映射或 --allow-no-tmdb 强制入库）"
			}
		}

		targetPath := info.BuildPath()
		if targetPath == "" {
			return nil
		}

		// 目标路径相对于挂载根
		fullTarget := filepath.Join(mountRoot, targetPath)
		// 源路径就是当前 fullPath
		plan := &Plan{
			SourcePath: fullPath,
			TargetPath: fullTarget,
			Info:       info,
		}
		pendings = append(pendings, pending{plan})
		allInfos = append(allInfos, info)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("扫描源目录失败: %w", err)
	}

	// 批量消歧（防同片多版本覆盖）
	renamer.Disambiguate(allInfos)

	// 消歧可能改变了 Edition 字段，重建目标路径
	for _, p := range pendings {
		newTarget := p.plan.Info.BuildPath()
		if newTarget != "" {
			p.plan.TargetPath = filepath.Join(mountRoot, newTarget)
		}
		plans = append(plans, p.plan)
	}

	return plans, nil
}

// Execute 执行落地计划
// 安全闸：先碰撞检测 → 再逐文件 MkdirAll + Rename
func (l *Lander) Execute(ctx context.Context, plans []*Plan) (*Result, error) {
	result := &Result{Plans: plans}

	// ① 碰撞检测（安全闸：云端改名不可回滚）
	seen := map[string]string{}
	var safe []*Plan
	for _, p := range plans {
		if p.Info.NeedsReview {
			result.Skipped = append(result.Skipped, p.SourcePath)
			continue
		}
		if prev, dup := seen[p.TargetPath]; dup {
			result.Failed = append(result.Failed, FailedFile{
				Path:  p.SourcePath,
				Error: fmt.Sprintf("路径碰撞: 与 %s 目标相同 (%s)，跳过以防覆盖", prev, p.TargetPath),
			})
			continue
		}
		seen[p.TargetPath] = p.SourcePath
		safe = append(safe, p)
	}

	if l.cfg.DryRun {
		// DryRun 模式：只报告，不执行
		for _, p := range safe {
			result.Moved = append(result.Moved, p.SourcePath+" → "+p.TargetPath)
		}
		return result, nil
	}

	// ② 逐文件执行
	for _, p := range safe {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}

		// 创建目标目录
		targetDir := filepath.Dir(p.TargetPath)
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			result.Failed = append(result.Failed, FailedFile{
				Path:  p.SourcePath,
				Error: fmt.Sprintf("创建目录失败 %s: %v", targetDir, err),
			})
			continue
		}

		// 如果源和目标相同，跳过
		if p.SourcePath == p.TargetPath {
			result.Skipped = append(result.Skipped, p.SourcePath)
			continue
		}

		// 检查目标是否已存在（幂等性）
		if _, err := os.Stat(p.TargetPath); err == nil {
			result.Skipped = append(result.Skipped, p.SourcePath+" (目标已存在)")
			continue
		}

		// 执行 rename/move
		if err := os.Rename(p.SourcePath, p.TargetPath); err != nil {
			result.Failed = append(result.Failed, FailedFile{
				Path:  p.SourcePath,
				Error: fmt.Sprintf("改名失败: %v", err),
			})
			continue
		}
		result.Moved = append(result.Moved, p.TargetPath)
	}

	return result, nil
}

// Summary 生成可读摘要
func (r *Result) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "计划 %d | 成功 %d | 跳过 %d | 失败 %d",
		len(r.Plans), len(r.Moved), len(r.Skipped), len(r.Failed))
	if len(r.Failed) > 0 {
		b.WriteString("\n失败明细:\n")
		for _, f := range r.Failed {
			fmt.Fprintf(&b, "  %s: %s\n", f.Path, f.Error)
		}
	}
	return b.String()
}

// isCategoryDir 判断目录名是否为一级分类（动漫/电影/电视剧/音乐）
func isCategoryDir(name string) bool {
	switch name {
	case "动漫", "电影", "电视剧", "音乐":
		return true
	}
	return false
}

// DefaultMountPaths 返回用户 NAS 的默认挂载路径
// 注意：这些路径只在 NAS 上有效，本地开发时无法访问
func DefaultMountPaths() map[linker.LinkType]string {
	return map[linker.LinkType]string{
		linker.LinkQuark: "/vol02/1000-1-a92fbdbc/影视",
		linker.LinkBaidu: "/vol02/1000-1-cb415a99/影视",
		// 光鸭: CloudDrive2 CloudFS 挂载，只读，暂不支持落地
	}
}

// MountPathForLinkType 根据链接类型返回挂载路径
func (l *Lander) MountPathForLinkType(lt linker.LinkType) string {
	return l.cfg.MountPaths[lt]
}

// 预留：后续实现 CD2 gRPC 客户端用于光鸭
var _ = sync.Mutex{}
