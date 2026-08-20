package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"fnos-enhance/internal/lander"
	"fnos-enhance/internal/linker"
	"fnos-enhance/internal/pipeline"
	"fnos-enhance/internal/renamer"
	"fnos-enhance/internal/transfer"
)

const usage = `fnosctl — 飞牛增强层 CLI

用法:
  fnosctl parse  <文本>                          解析文本中的网盘分享链接
  fnosctl rename <分类> <剧名目录> [中间层] <文件名>  演算规范落地路径
  fnosctl plan   <清单文件>                       批量规划（含消歧+碰撞检测），只读不落地
  fnosctl land   <挂载路径> [--source 子目录] [--cat 分类] [--alist] [--execute]
                 --alist = 走 Alist API 落地（光鸭必须用；只读挂载的网盘走这条）
                           需 ALIST_URL + (ALIST_TOKEN 或 ALIST_USER/ALIST_PASS)
                                                  扫描转存目录→规划→落地改名（默认 dry-run）
  fnosctl transfer <链接或包含链接的文本> [--execute]
                                                  转存网盘分享到自己网盘（默认 dry-run 只列举）
  fnosctl ingest <链接> --mount <挂载根> [--source 子目录] [--execute]
                                                  全链路：转存→等挂载可见→落地改名（默认 dry-run）

凭据一律从环境变量读取，源码不含任何密钥：
  TMDB_API_KEY           TMDB API Key（或用 TMDB_API_KEY_FILE 指向密钥文件）
  TMDB_API_KEY_FILE      密钥文件路径，默认 ~/.config/fnos-enhance/tmdb.key
  QUARK_COOKIE           夸克网盘 Cookie
  QUARK_TO_DIR_FID       夸克转存目标目录 fid（默认根目录）
  BAIDU_COOKIE           百度网盘 Cookie（含 BDUSS）
  BAIDU_TO_DIR           百度转存目标目录路径（默认 /）
  GUANGYA_ACCESS_TOKEN   光鸭 access_token
  GUANGYA_REFRESH_TOKEN  光鸭 refresh_token（配合 CLIENT_ID 可自动刷新）
  GUANGYA_CLIENT_ID      光鸭 client_id
  NAME_MAP_PATH          乱码名映射表 JSON 路径（如 quark-name-map.json）
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "parse":
		err = cmdParse(os.Args[2:])
	case "rename":
		err = cmdRename(os.Args[2:])
	case "plan":
		err = cmdPlan(os.Args[2:])
	case "land":
		err = cmdLand(os.Args[2:])
	case "transfer":
		err = cmdTransfer(os.Args[2:])
	case "ingest":
		err = cmdIngest(os.Args[2:])
	default:
		fmt.Print(usage)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// ------------------------------------------------------------
// parse：识别分享链接
// ------------------------------------------------------------

func cmdParse(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("缺少参数：待解析文本")
	}
	links := linker.ParseLinks(strings.Join(args, " "))
	if len(links) == 0 {
		fmt.Println("未识别到任何网盘分享链接")
		return nil
	}
	for i, l := range links {
		fmt.Printf("[%d] 类型=%s  ID=%s  提取码=%s\n", i+1, l.Type, l.ID, orDash(l.Pwd))
	}
	return nil
}

// ------------------------------------------------------------
// rename：单文件路径演算
// ------------------------------------------------------------

func cmdRename(args []string) error {
	var cat, dir, sub, file string
	switch len(args) {
	case 3:
		cat, dir, sub, file = args[0], args[1], "", args[2]
	case 4:
		cat, dir, sub, file = args[0], args[1], args[2], args[3]
	default:
		return fmt.Errorf("用法: fnosctl rename <分类> <剧名目录> [中间层] <文件名>")
	}

	info := renamer.ParsePackage(renamer.Category(cat), dir, sub, file)
	applyNameMap(info)
	enrichTMDB(info)

	fmt.Printf("片名     : %s\n", info.Title)
	fmt.Printf("年份     : %s\n", orDash(info.Year))
	fmt.Printf("TMDB     : %s\n", tmdbStr(info))
	if info.EpisodeFound {
		fmt.Printf("季/集    : S%02dE%02d\n", info.Season, info.Episode)
	} else {
		fmt.Printf("季/集    : (电影，无集号)\n")
	}
	if info.Version != "" {
		fmt.Printf("版本     : %s\n", info.Version)
	}
	if info.NeedsReview {
		fmt.Printf("⚠ 需人工确认: %s\n", info.ReviewReason)
		return nil
	}
	fmt.Printf("落地路径 : %s\n", info.BuildPath())
	return nil
}

// ------------------------------------------------------------
// plan：批量规划（只读，不落地）
// ------------------------------------------------------------

func cmdPlan(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("用法: fnosctl plan <清单文件> [--allow-no-tmdb]  （每行一条 分类/剧名/[中间层]/文件名）")
	}
	var listFile string
	for _, a := range args {
		if a == "--allow-no-tmdb" {
			allowNoTMDB = true
			continue
		}
		if listFile == "" {
			listFile = a
		}
	}
	if listFile == "" {
		return fmt.Errorf("缺少清单文件路径")
	}
	data, err := os.ReadFile(listFile)
	if err != nil {
		return err
	}

	type row struct {
		info *renamer.MediaInfo
		src  string
	}
	var rows []row
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "/")
		if len(parts) < 2 {
			continue
		}
		cat := renamer.Category(parts[0])
		var dir, sub, file string
		switch len(parts) {
		case 2:
			dir, sub, file = strings.TrimSuffix(parts[1], extOf(parts[1])), "", parts[1]
		case 3:
			dir, sub, file = parts[1], "", parts[2]
		default:
			dir, sub, file = parts[1], parts[2], parts[len(parts)-1]
		}
		info := renamer.ParsePackage(cat, dir, sub, file)
		applyNameMap(info)
		enrichTMDB(info) // 必须在消歧前：TMDB 会改变片名/年份，从而改变目标路径
		rows = append(rows, row{info, line})
	}

	// 落地前必做：批量消歧
	infos := make([]*renamer.MediaInfo, 0, len(rows))
	for _, r := range rows {
		infos = append(infos, r.info)
	}
	fixed := renamer.Disambiguate(infos)

	// 碰撞检测（安全闸）
	seen := map[string]string{}
	var okCount, reviewCount, collision int
	for _, r := range rows {
		p := r.info.BuildPath()
		if p == "" {
			reviewCount++
			fmt.Printf("[待确认] %s\n         原因: %s\n", r.src, orDash(r.info.ReviewReason))
			continue
		}
		if prev, dup := seen[p]; dup {
			collision++
			fmt.Printf("[碰撞!!] %s\n         与 %s 目标相同: %s\n", r.src, prev, p)
			continue
		}
		seen[p] = r.src
		okCount++
		fmt.Printf("[可入库] %s\n      -> %s\n", r.src, p)
	}

	fmt.Printf("\n合计 %d | 可入库 %d | 待确认 %d | 碰撞 %d | 消歧补标签 %d\n",
		len(rows), okCount, reviewCount, collision, fixed)
	if collision > 0 {
		return fmt.Errorf("存在 %d 处路径碰撞，禁止落地（会覆盖文件导致数据丢失）", collision)
	}
	return nil
}

// ------------------------------------------------------------
// land：扫描转存目录 → 规划 → 落地改名
// ------------------------------------------------------------

func cmdLand(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("用法: fnosctl land <挂载根> [--source 子目录] [--cat 分类] [--alist] [--allow-no-tmdb] [--execute]\n  挂载根 = 影视目录（如 /vol02/1000-1-a92fbdbc/影视；--alist 时为 Alist 虚拟路径如 /光鸭/影视）\n  --source = 转存文件所在子目录（如 0_待整理）\n  --alist  = 走 Alist API 落地（光鸭只读挂载必须用）\n  默认 dry-run，加 --execute 才真正改名")
	}

	mountRoot := args[0]
	sourceDir := ""
	categoryHint := ""
	execute := false
	useAlist := false

	for _, a := range args[1:] {
		switch {
		case a == "--execute":
			execute = true
		case a == "--allow-no-tmdb":
			allowNoTMDB = true
		case a == "--alist":
			useAlist = true
		case strings.HasPrefix(a, "--source="):
			sourceDir = strings.TrimPrefix(a, "--source=")
		case strings.HasPrefix(a, "--cat="):
			categoryHint = strings.TrimPrefix(a, "--cat=")
		}
	}

	if !execute {
		fmt.Println("⚠ DRY-RUN 模式（加 --execute 才真正执行改名）")
	}

	cfg := lander.Config{
		MountPaths:  lander.DefaultMountPaths(),
		SourceDir:   sourceDir,
		DryRun:      !execute,
		NameMap:     getNameMap(),
		TMDBClient:  getTMDB(),
		AllowNoTMDB: allowNoTMDB,
	}
	if useAlist {
		be, err := alistBackendFromEnv()
		if err != nil {
			return err
		}
		cfg.Backend = be
		fmt.Println("落地后端: Alist API（光鸭等只读挂载的网盘走这条）")
	}
	l := lander.New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Printf("扫描源目录: %s", mountRoot)
	if sourceDir != "" {
		fmt.Printf("/%s", sourceDir)
	}
	if categoryHint != "" {
		fmt.Printf("  分类=%s", categoryHint)
	}
	fmt.Println()

	plans, err := l.PlanFromDir(ctx, mountRoot, categoryHint)
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		fmt.Println("源目录无媒体文件")
		return nil
	}

	fmt.Printf("发现 %d 个文件，开始落地...\n\n", len(plans))

	result, err := l.Execute(ctx, plans)
	if err != nil {
		return err
	}

	if !execute {
		for _, p := range plans {
			if p.Info.NeedsReview {
				fmt.Printf("[跳过] %s  (%s)\n", p.SourcePath, p.Info.ReviewReason)
				continue
			}
			fmt.Printf("[规划] %s\n  -> %s\n", p.SourcePath, p.TargetPath)
		}
	}

	fmt.Printf("\n%s\n", result.Summary())
	return nil
}

// ------------------------------------------------------------
// transfer：转存网盘分享
// ------------------------------------------------------------

func cmdTransfer(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("用法: fnosctl transfer <链接或包含链接的文本> [--execute]\n  默认 dry-run（只列举分享内容不落盘），加 --execute 才真正转存")
	}

	execute := false
	var textParts []string
	for _, a := range args {
		if a == "--execute" {
			execute = true
			continue
		}
		if a == "--allow-no-tmdb" {
			allowNoTMDB = true
			continue
		}
		textParts = append(textParts, a)
	}
	text := strings.Join(textParts, " ")

	links := linker.ParseLinks(text)
	if len(links) == 0 {
		return fmt.Errorf("没识别到可用分享链接（仅支持 quark.cn/s/ · pan.baidu.com/s/ · guangyapan.com/s/）")
	}

	cfg := transferConfigFromEnv()
	if execute {
		if err := cfg.RequireCredentials(); err != nil {
			return err
		}
	}
	tr, err := transfer.New(cfg)
	if err != nil {
		return err
	}

	if !execute {
		fmt.Println("⚠ DRY-RUN 模式：只列举分享内容，不转存（加 --execute 真正转存）")
	}
	fmt.Printf("识别到 %d 个分享\n\n", len(links))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var okCount, failCount int
	for i, l := range links {
		fmt.Printf("[%d/%d] %s %s\n", i+1, len(links), l.Type, l.Link)
		res, err := tr.Transfer(ctx, l, !execute)
		if err != nil {
			failCount++
			fmt.Printf("  ✗ 失败: %v\n\n", err)
			continue
		}
		okCount++
		fmt.Printf("  ✓ %s | 顶层 %d 项 | 递归共 %d 个文件\n",
			res.Provider, len(res.Names), res.FileCount())
		for j, n := range res.Names {
			if j >= 5 {
				fmt.Printf("    ... 共 %d 项\n", len(res.Names))
				break
			}
			fmt.Printf("    - %s\n", n)
		}
		if execute {
			fmt.Printf("  已提交转存 %d 项\n", res.Transferred)
		}
		fmt.Println()
	}

	fmt.Printf("完成 | 成功 %d | 失败 %d\n", okCount, failCount)
	if failCount > 0 && okCount == 0 {
		return fmt.Errorf("全部转存失败")
	}
	return nil
}

// ------------------------------------------------------------
// ingest：全链路（转存 → 等挂载可见 → 落地改名）
// ------------------------------------------------------------

func cmdIngest(args []string) error {
	const help = `用法: fnosctl ingest <链接或文本> --mount <挂载根> [--source 子目录] [--execute]

  --mount    必填。影视目录根，目标路径以此为基准
             夸克: /vol02/1000-1-a92fbdbc/影视
             百度: /vol02/1000-1-cb415a99/影视
  --source   转存落点（相对 --mount），缺省为挂载根
  --execute  真正执行（默认 dry-run，不转存不改名）
  --wait     等挂载刷新的上限，如 5m（默认 5m）

⚠ 光鸭网盘不能作为落地目标：CloudFS 挂载只读，无法 rename（见 docs/ADR-001）
`
	execute := false
	var mount, source, wait string
	var textParts []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--execute":
			execute = true
		case "--allow-no-tmdb":
			allowNoTMDB = true
		case "--mount", "--source", "--wait":
			if i+1 >= len(args) {
				return fmt.Errorf("%s 缺少参数\n\n%s", args[i], help)
			}
			switch args[i] {
			case "--mount":
				mount = args[i+1]
			case "--source":
				source = args[i+1]
			case "--wait":
				wait = args[i+1]
			}
			i++
		default:
			if strings.HasPrefix(args[i], "--") {
				return fmt.Errorf("未知参数 %s\n\n%s", args[i], help)
			}
			textParts = append(textParts, args[i])
		}
	}
	if len(textParts) == 0 || mount == "" {
		return fmt.Errorf("缺少链接或 --mount\n\n%s", help)
	}

	// 挂载必须先可用，否则会转存成功但无处落地
	if st, err := os.Stat(mount); err != nil || !st.IsDir() {
		return fmt.Errorf("挂载根不可用: %s（挂载是否掉了？）", mount)
	}

	links := linker.ParseLinks(strings.Join(textParts, " "))
	if len(links) == 0 {
		return fmt.Errorf("没识别到可用分享链接")
	}
	for _, l := range links {
		if l.Type == linker.LinkGuangYa {
			fmt.Fprintf(os.Stderr,
				"警告: 光鸭链接 %s 可转存但**无法落地改名**（CloudFS 挂载只读）\n", l.ID)
		}
	}

	tcfg := transferConfigFromEnv()
	if execute {
		if err := tcfg.RequireCredentials(); err != nil {
			return err
		}
	}
	tr, err := transfer.New(tcfg)
	if err != nil {
		return err
	}

	l := lander.New(lander.Config{
		MountRoot:   mount,
		SourceDir:   source,
		DryRun:      !execute,
		NameMap:     getNameMap(),
		TMDBClient:  getTMDB(),
		AllowNoTMDB: allowNoTMDB,
	})

	p := &pipeline.Pipeline{
		Transferor: tr,
		Lander:     l,
		MountRoot:  mount,
		SourceDir:  source,
		Logf:       func(f string, a ...interface{}) { fmt.Printf(f, a...) },
	}
	if wait != "" {
		d, err := time.ParseDuration(wait)
		if err != nil {
			return fmt.Errorf("--wait 格式错误 %q（应如 5m / 90s）: %w", wait, err)
		}
		p.WaitTimeout = d
	}

	if !execute {
		fmt.Println("⚠ DRY-RUN：不转存、不改名，只跑通链路并报告（加 --execute 真正执行）")
	} else {
		fmt.Println("⚠ EXECUTE：将真实转存并改名，网盘改名不可逆")
	}
	fmt.Printf("挂载根: %s | 落点: %s | 识别到 %d 个分享\n\n",
		mount, orDefault(source, "(挂载根)"), len(links))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	results := p.Run(ctx, links, !execute)

	fmt.Println()
	for _, r := range results {
		if r.Err != nil {
			fmt.Printf("✗ %s [失败于 %s] %v\n", r.Link.Link, r.Stage, r.Err)
		}
	}
	fmt.Println(pipeline.Summary(results))

	for _, r := range results {
		if r.Err == nil {
			return nil // 至少一条成功
		}
	}
	return fmt.Errorf("全部链接失败")
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ------------------------------------------------------------
// 辅助
// ------------------------------------------------------------

// transferConfigFromEnv 从环境变量组装转存凭据（源码不含任何密钥）
func transferConfigFromEnv() transfer.Config {
	return transfer.Config{
		Quark: transfer.QuarkConfig{
			Cookie:   os.Getenv("QUARK_COOKIE"),
			ToDirFID: os.Getenv("QUARK_TO_DIR_FID"),
		},
		Baidu: transfer.BaiduConfig{
			Cookie: os.Getenv("BAIDU_COOKIE"),
			ToDir:  os.Getenv("BAIDU_TO_DIR"),
		},
		GuangYa: transfer.GuangYaConfig{
			AccessToken:  os.Getenv("GUANGYA_ACCESS_TOKEN"),
			RefreshToken: os.Getenv("GUANGYA_REFRESH_TOKEN"),
			ClientID:     os.Getenv("GUANGYA_CLIENT_ID"),
			ToDirID:      os.Getenv("GUANGYA_TO_DIR_ID"),
		},
	}
}

var (
	nameMap    *renamer.NameMap
	tmdbClient *renamer.TMDBClient
	tmdbOnce   bool
)

// getTMDB 共用客户端，使内置缓存生效（961 个文件只需 ~19 次查询）
func getTMDB() *renamer.TMDBClient {
	if !tmdbOnce {
		tmdbOnce = true
		if key := renamer.LoadAPIKey(); key != "" {
			tmdbClient = renamer.NewTMDBClient(key)
		} else {
			fmt.Fprintln(os.Stderr, "提示: 未配置 TMDB 密钥，跳过刮削补全")
		}
	}
	return tmdbClient
}

func getNameMap() *renamer.NameMap {
	if nameMap == nil {
		p := os.Getenv("NAME_MAP_PATH")
		if p == "" {
			nameMap = renamer.NewNameMap()
		} else {
			nm, err := renamer.LoadNameMap(p)
			if err != nil {
				fmt.Fprintf(os.Stderr, "警告: 名称映射表加载失败 (%v)\n", err)
			}
			nameMap = nm
		}
	}
	return nameMap
}

func applyNameMap(info *renamer.MediaInfo) {
	if nameMap == nil {
		p := os.Getenv("NAME_MAP_PATH")
		if p == "" {
			nameMap = renamer.NewNameMap()
		} else {
			nm, err := renamer.LoadNameMap(p)
			if err != nil {
				fmt.Fprintf(os.Stderr, "警告: 名称映射表加载失败 (%v)，继续但不做别名替换\n", err)
			}
			nameMap = nm
		}
	}
	nameMap.Apply(info)
}

var warnedTMDB = map[string]bool{}

// alistBackendFromEnv 从环境变量构造 Alist 落地后端。
//
// 光鸭必须走这条：它的 CloudDrive2 CloudFS 挂载只读，mkdir/rename 全失败。
// 只调 HTTP API，不挂 FUSE、不用缓存——改名不需要读文件内容。
func alistBackendFromEnv() (*lander.AlistBackend, error) {
	base := os.Getenv("ALIST_URL")
	if base == "" {
		return nil, fmt.Errorf("使用 --alist 需设置 ALIST_URL（如 http://127.0.0.1:5245）")
	}
	tok := os.Getenv("ALIST_TOKEN")
	user := orDefault(os.Getenv("ALIST_USER"), "admin")
	pass := os.Getenv("ALIST_PASS")
	if tok == "" && pass == "" {
		return nil, fmt.Errorf("使用 --alist 需设置 ALIST_TOKEN，或 ALIST_USER + ALIST_PASS")
	}
	return lander.NewAlistBackend(base, tok, user, pass), nil
}

// allowNoTMDB 由 --allow-no-tmdb 置位：允许 TMDB 未识别的条目照样落地
var allowNoTMDB bool

func enrichTMDB(info *renamer.MediaInfo) {
	c := getTMDB()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.Enrich(ctx, info); err != nil {
		// 同一片名只警告一次：一个 17 文件的分享会刷 17 行相同警告，
		// 961 文件批量跑时真正的问题会被洗掉
		if !warnedTMDB[info.Title] {
			warnedTMDB[info.Title] = true
			fmt.Fprintf(os.Stderr, "警告: TMDB 补全失败 [%s] (%v)\n", info.Title, err)
		}
	}
	// 用户决策：认不出就跳过，不硬着头皮落地。
	// 无 TMDB 标签飞牛刮不出海报，而云端改名不可逆，人工还得改回来。
	if info.TMDBID == 0 && !allowNoTMDB {
		info.NeedsReview = true
		info.ReviewReason = "TMDB 未识别，已跳过（确认后可用 NAME_MAP 映射或 --allow-no-tmdb 强制入库）"
	}
}

func extOf(s string) string {
	if i := strings.LastIndex(s, "."); i > 0 {
		return s[i:]
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func tmdbStr(info *renamer.MediaInfo) string {
	if info.TMDBID == 0 {
		return "-"
	}
	if info.TMDBType != "" {
		return fmt.Sprintf("{%s tmdb-%d}", info.TMDBType, info.TMDBID)
	}
	return fmt.Sprintf("{tmdb-%d}", info.TMDBID)
}

var _ = json.Marshal // 预留：后续 plan 输出 JSON 供 TG Bot 消费
