package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"fnos-enhance/internal/linker"
	"fnos-enhance/internal/transfer"
	"fnos-enhance/internal/watcher"
)

// quarkLister 用免凭据的公开分享列举接口喂给 watcher
type quarkLister struct{ t transfer.Transferor }

func (q quarkLister) ListShare(ctx context.Context, link string) ([]watcher.ShareItem, error) {
	links := linker.ParseLinks(link)
	if len(links) == 0 {
		return nil, fmt.Errorf("无法解析链接: %s", link)
	}
	// dryRun=true 只列举不转存
	res, err := q.t.Transfer(ctx, links[0], true)
	if err != nil {
		return nil, err
	}
	out := make([]watcher.ShareItem, 0, len(res.Entries))
	for _, e := range res.Entries {
		out = append(out, watcher.ShareItem{
			ID: e.ID, Name: e.Name, Size: e.Size, IsDir: e.IsDir,
		})
	}
	return out, nil
}

// officialSaver 用官方 OAuth 转存新增内容
type officialSaver struct {
	q *transfer.QuarkOfficialTransferor
}

func (o officialSaver) SaveNew(ctx context.Context, link string, ids []string) (int, error) {
	links := linker.ParseLinks(link)
	if len(links) == 0 {
		return 0, fmt.Errorf("无法解析链接: %s", link)
	}
	// 官方 saveas 目前整份转存；网盘侧对已存在条目会去重，
	// 因此重复提交不会重复占空间（实测 30 项 = 22 新增 + 8 去重）。
	res, err := o.q.Transfer(ctx, links[0], false)
	if err != nil {
		return 0, err
	}
	return res.Transferred, nil
}

func cmdSub(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(`用法:
  fnosctl sub add <链接> [--title 备注] [--cat 分类] [--interval 5m]
  fnosctl sub list
  fnosctl sub check [--execute] [--interval 10m] [--loop]
  fnosctl sub off <链接>          停用（不删除记录）

订阅文件: ~/.config/fnos-enhance/subs.json（可直接编辑）
首次订阅只建立基线，之后只转存"新增"的集，不会把历史片库拖回来`)
	}

	path := os.Getenv("FNOS_SUBS_FILE")
	if path == "" {
		p, err := watcher.DefaultStorePath()
		if err != nil {
			return err
		}
		path = p
	}
	store, err := watcher.OpenStore(path)
	if err != nil {
		return err
	}

	switch args[0] {
	case "add":
		return subAdd(store, args[1:])
	case "list":
		return subList(store)
	case "off":
		if len(args) < 2 {
			return fmt.Errorf("用法: fnosctl sub off <链接>")
		}
		if !store.Disable(args[1], "用户手动停用") {
			return fmt.Errorf("没有这条订阅: %s", args[1])
		}
		fmt.Println("已停用（记录保留，可重新 add 启用）")
		return store.Save()
	case "check":
		return subCheck(store, args[1:])
	default:
		return fmt.Errorf("未知子命令: %s", args[0])
	}
}

func subAdd(store *watcher.Store, args []string) error {
	var link, title, cat string
	var iv time.Duration
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--title" && i+1 < len(args):
			i++
			title = args[i]
		case args[i] == "--cat" && i+1 < len(args):
			i++
			cat = args[i]
		case args[i] == "--interval" && i+1 < len(args):
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("间隔格式错误 %q（示例 30s / 5m / 1h）: %w", args[i], err)
			}
			iv = d
		case !strings.HasPrefix(args[i], "--"):
			link = args[i]
		}
	}
	if link == "" {
		return fmt.Errorf("缺少分享链接")
	}
	if len(linker.ParseLinks(link)) == 0 {
		return fmt.Errorf("这不是支持的分享链接: %s", link)
	}
	sub, created := store.Add(link, title, cat)
	if iv > 0 {
		sub.Interval = iv
	}
	if created {
		fmt.Printf("已订阅: %s\n", link)
		fmt.Println("提示: 首次 check 只建立基线（记录现有内容），之后才转存新增")
	} else {
		fmt.Printf("订阅已存在，已更新备注: %s\n", link)
	}
	return store.Save()
}

func subList(store *watcher.Store) error {
	subs := store.List()
	if len(subs) == 0 {
		fmt.Println("还没有订阅。用 fnosctl sub add <链接> 添加")
		return nil
	}
	fmt.Printf("共 %d 条订阅\n\n", len(subs))
	for _, s := range subs {
		status := "启用"
		if s.Disabled {
			status = "已停用"
		}
		fmt.Printf("[%s] %s\n", status, orDefault(s.Title, "(无备注)"))
		fmt.Printf("  链接: %s\n", s.Link)
		fmt.Printf("  分类: %s | 已记录 %d 个条目\n", orDefault(s.Category, "自动"), len(s.Seen))
		if !s.LastCheck.IsZero() {
			fmt.Printf("  上次检查: %s\n", s.LastCheck.Format("2006-01-02 15:04:05"))
		}
		if !s.LastNew.IsZero() {
			fmt.Printf("  上次更新: %s\n", s.LastNew.Format("2006-01-02 15:04:05"))
		}
		if s.LastError != "" {
			fmt.Printf("  最近错误: %s (连续失败 %d 次)\n", s.LastError, s.FailCount)
		}
		fmt.Println()
	}
	return nil
}

func subCheck(store *watcher.Store, args []string) error {
	execute := false
	loop := false
	interval := 10 * time.Minute
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--execute":
			execute = true
		case args[i] == "--loop":
			loop = true
		case args[i] == "--interval" && i+1 < len(args):
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("间隔格式错误 %q: %w", args[i], err)
			}
			interval = d
		}
	}

	if !execute {
		fmt.Println("⚠ DRY-RUN：只报告新增，不转存（加 --execute 才真转存）")
	}

	tcfg := transferConfigFromEnv()
	tr, err := transfer.New(tcfg)
	if err != nil {
		return err
	}
	lister := quarkLister{t: tr}

	var saver watcher.Saver
	if execute {
		q, err := officialQuarkFromEnv()
		if err != nil {
			return fmt.Errorf("转存需要官方通道: %w", err)
		}
		saver = officialSaver{q: q}
	} else {
		saver = noopSaver{}
	}

	for {
		now := time.Now()
		subs := store.List()
		due := 0
		for _, s := range subs {
			if !s.NextDue(now, interval) {
				continue
			}
			due++
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			res := watcher.Check(ctx, s, lister, saver, !execute)
			cancel()

			name := orDefault(s.Title, s.Link)
			switch {
			case res.Err != nil:
				fmt.Printf("✗ %s: %v\n", name, res.Err)
			case res.Baseline:
				if execute {
					fmt.Printf("◆ %s: 已建立基线，记录 %d 个现有条目（不转存，之后只追新增）\n",
						name, res.BaselineCount)
				} else {
					fmt.Printf("◆ %s: 首次检查，现有 %d 个条目。dry-run 不写基线，\n"+
						"    用 --execute 建立基线后才会开始追更（建基线本身不转存）\n",
						name, res.BaselineCount)
				}
			case len(res.NewItems) == 0:
				fmt.Printf("· %s: 无更新\n", name)
			default:
				fmt.Printf("★ %s: 发现 %d 个新增\n", name, len(res.NewItems))
				for i, it := range res.NewItems {
					if i >= 10 {
						fmt.Printf("    ... 共 %d 个\n", len(res.NewItems))
						break
					}
					fmt.Printf("    %s (%s)\n", it.Name, humanSize(it.Size))
				}
				if execute {
					fmt.Printf("  → 已转存 %d 项\n", res.Saved)
				}
			}
		}
		if err := store.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "警告: 保存订阅状态失败: %v\n", err)
		}
		if due == 0 {
			fmt.Println("没有到期需要检查的订阅")
		}
		if !loop {
			return nil
		}
		time.Sleep(30 * time.Second)
	}
}

type noopSaver struct{}

func (noopSaver) SaveNew(context.Context, string, []string) (int, error) { return 0, nil }

func humanSize(n int64) string {
	const u = 1024
	if n < u {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := []string{"KB", "MB", "GB", "TB"}
	f := float64(n)
	for _, unit := range units {
		f /= u
		if f < u {
			return fmt.Sprintf("%.1f %s", f, unit)
		}
	}
	return fmt.Sprintf("%.1f PB", f/u)
}
