package renamer

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// ============================================================
// 单元测试：真实目录名解析（全部来自 NAS）
// ============================================================

func TestParseDirName_RealData(t *testing.T) {
	cases := []struct {
		dir    string
		title  string
		year   string
		tmdbID int
	}{
		// 脏名：首字母索引 + 全角括号作者
		{"T  吞噬星空（罗峰）", "吞噬星空", "", 0},
		{"X   仙逆（王麻子）", "仙逆", "", 0},
		// 资源站全量元数据
		{"完美世界 (2021){tv tmdb-124003}[S01][1080p][HEVC][AAC][中字][2.0](71.0GB 205个文件)", "完美世界", "2021", 124003},
		{"完美世界剧场版 (2024){tv tmdb-271413}[S01][2160p][HEVC][AAC][中字][2.0](6.2GB 3个文件)", "完美世界剧场版", "2024", 271413},
		// 已带 tmdb（两种写法）
		{"凡人修仙传(2020) {tmdb-288233}", "凡人修仙传", "2020", 288233},
		{"牧神记 (2024) {tmdb-236534}", "牧神记", "2024", 236534},
		// 电影：规范与不规范混杂
		{"流浪地球 (2019)", "流浪地球", "2019", 0},
		{"流浪地球2 (2023)", "流浪地球2", "2023", 0},
		{"火遮眼(2026)", "火遮眼", "2026", 0},
		{"Escape Artists(2005)", "Escape Artists", "2005", 0},
		{"色，戒 (2007)", "色，戒", "2007", 0},
		{"倩女幽魂2：人间道 (1988)", "倩女幽魂2：人间道", "1988", 0},
		{"长津湖之水门桥 (2022)", "长津湖之水门桥", "2022", 0},
		{"千香 (2026)", "千香", "2026", 0},
		{"哪吒之魔童闹海 (2025)", "哪吒之魔童闹海", "2025", 0},
	}

	for _, c := range cases {
		title, year, id, _ := ParseDirName(c.dir)
		if title != c.title {
			t.Errorf("ParseDirName(%q).title = %q, want %q", c.dir, title, c.title)
		}
		if year != c.year {
			t.Errorf("ParseDirName(%q).year = %q, want %q", c.dir, year, c.year)
		}
		if id != c.tmdbID {
			t.Errorf("ParseDirName(%q).tmdbID = %d, want %d", c.dir, id, c.tmdbID)
		}
	}
}

// ============================================================
// 中间层目录：真 Season vs 资源站分卷（必须扁平化）
// ============================================================

func TestParseSubDir_RealData(t *testing.T) {
	cases := []struct {
		sub    string
		kind   SubDirKind
		season int
	}{
		{"Season 1", SubSeason, 1},
		{"Season 6 星海飞驰", SubSeason, 6}, // 带副标题
		{"Season 7 外海风云", SubSeason, 7},
		{"Season 1 风起天南 重置版", SubSeason, 1},
		{"001-100集", SubVolume, 0}, // 分卷 → 丢弃
		{"101-200集", SubVolume, 0},
		{"001-100 4K", SubVolume, 0},
		{"101-150 4K", SubVolume, 0},
		{"201-230集", SubVolume, 0},
		{"合集篇", SubCompilation, 0},          // 剪辑版 → 需人工确认，不自动入库
		{"Specials 虚天战纪 导剪版", SubSeason, 0}, // Emby 约定：特典 = Season 00
		{"剧场版", SubMovie, 0},                // 独立电影条目
		{"", SubNone, 0},
	}
	for _, c := range cases {
		kind, season := ParseSubDir(c.sub)
		if kind != c.kind {
			t.Errorf("ParseSubDir(%q).kind = %v, want %v", c.sub, kind, c.kind)
		}
		if season != c.season {
			t.Errorf("ParseSubDir(%q).season = %d, want %d", c.sub, season, c.season)
		}
	}
}

// ============================================================
// P0-5 回归：广告清理不得误杀正常片名（.me/.tv/.cc 是高频普通词）
// ============================================================

func TestNoFalsePositive_TLDInTitle(t *testing.T) {
	cases := []struct{ file, wantTitle string }{
		{"Remember.me.2010.1080p.mkv", "Remember me"},
		{"Love.me.if.you.dare.2003.mkv", "Love me if you dare"},
		{"Tales.of.Herding.Gods.S01E01.2024.2160p.WEB-DL.H.264.AAC.mp4", "Tales of Herding Gods"},
	}
	for _, c := range cases {
		title, _, _, _, _, _ := ParseFileName(c.file)
		if title != c.wantTitle {
			t.Errorf("ParseFileName(%q).title = %q, want %q", c.file, title, c.wantTitle)
		}
	}
}

// ============================================================
// P0-2 回归：不得出现残壳（"流浪地球 ()"、"片名 ( )"）
// ============================================================

func TestNoEmptyShell(t *testing.T) {
	bad := []string{"()", "（）", "( )", "[]", "{}", " )", "( "}
	dirs := []string{
		"流浪地球 (2019)", "火遮眼(2026)", "Escape Artists(2005)",
		"完美世界 (2021){tv tmdb-124003}[S01][1080p](71.0GB 205个文件)",
		"T  吞噬星空（罗峰）",
	}
	for _, d := range dirs {
		title, _, _, _ := ParseDirName(d)
		for _, b := range bad {
			if strings.Contains(title, b) {
				t.Errorf("ParseDirName(%q) 残壳: title=%q 含 %q", d, title, b)
			}
		}
	}
}

// ============================================================
// P0-3 回归：纯技术名必须由目录名兜底出片名
// ============================================================

func TestTechOnlyFilename_FallsBackToDir(t *testing.T) {
	cases := []struct {
		dir, sub, file string
		wantPath       string
	}{
		{
			"T  吞噬星空（罗峰）", "", "S01E231.2020.2160p.WEB-DL.H265.10bit.HIFI2.0&DDP2.0.mp4",
			// 剧集不从文件名取年份（避免 2020/2024/2026 拆目录），年份由 TMDB 补全
			"动漫/吞噬星空/Season 01/吞噬星空 - S01E231.mp4",
		},
		{
			"T  吞噬星空（罗峰）", "001-100集", "S01E001.2020.2160p.WEB-DL.SDR.H265.8bit.AAC.mp4",
			"动漫/吞噬星空/Season 01/吞噬星空 - S01E01.mp4",
		},
		{
			"X   仙逆（王麻子）", "Season 6 星海飞驰", "S06E150.2023.2160p.WEB-DL.H265.10bit.mp4",
			"动漫/仙逆/Season 06/仙逆 - S06E150.mp4",
		},
		{
			"牧神记 (2024) {tmdb-236534}", "", "Tales.of.Herding.Gods.S01E01.2024.2160p.WEB-DL.H.264.AAC.mp4",
			"动漫/牧神记 (2024) {tmdb-236534}/Season 01/牧神记 - S01E01.mp4",
		},
		{
			"完美世界 (2021){tv tmdb-124003}[S01][1080p][HEVC][AAC][中字][2.0](71.0GB 205个文件)", "Season 1",
			"S01E001.2021.1080p.WEB-DL.HEVC.AAC.mp4",
			"动漫/完美世界 (2021) {tmdb-124003}/Season 01/完美世界 - S01E01.mp4",
		},
		// 电影：单文件目录
		{
			"流浪地球 (2019)", "", "流浪地球 (2019).mkv",
			"电影/流浪地球 (2019)/流浪地球 (2019).mkv",
		},
		{
			"火遮眼(2026)", "", "火遮眼(2026).mp4",
			"电影/火遮眼 (2026)/火遮眼 (2026).mp4",
		},
	}

	for _, c := range cases {
		cat := CatAnime
		if strings.HasPrefix(c.wantPath, "电影/") {
			cat = CatMovie
		}
		info := ParsePackage(cat, c.dir, c.sub, c.file)
		got := info.BuildPath()
		if got != c.wantPath {
			t.Errorf("ParsePackage(%q,%q,%q)\n  got  %q\n  want %q", c.dir, c.sub, c.file, got, c.wantPath)
		}
	}
}

// ============================================================
// 黄金语料回归：961 条真实文件，全量跑，统计识别率
// ============================================================

func TestGoldenCorpus(t *testing.T) {
	f, err := os.Open("testdata/corpus_files.txt")
	if err != nil {
		t.Skipf("语料缺失: %v", err)
	}
	defer f.Close()

	var total, ok, noTitle, shell, review int
	badSamples := []string{}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, "/")
		if len(parts) < 2 {
			continue
		}
		total++

		cat := Category(parts[0])
		var dir, sub, file string
		switch len(parts) {
		case 2: // 分类/文件
			dir, sub, file = strings.TrimSuffix(parts[1], filepathExt(parts[1])), "", parts[1]
		case 3: // 分类/剧名/文件
			dir, sub, file = parts[1], "", parts[2]
		default: // 分类/剧名/中间层/文件
			dir, sub, file = parts[1], parts[2], parts[len(parts)-1]
		}

		info := ParsePackage(cat, dir, sub, file)
		path := info.BuildPath()

		switch {
		case info.NeedsReview:
			review++
		case info.Title == "":
			noTitle++
			if len(badSamples) < 5 {
				badSamples = append(badSamples, "空片名: "+line)
			}
		case strings.Contains(path, "()") || strings.Contains(path, "（）") ||
			strings.Contains(path, " )") || strings.Contains(path, "( "):
			shell++
			if len(badSamples) < 5 {
				badSamples = append(badSamples, "残壳: "+path)
			}
		default:
			ok++
		}
	}

	rate := float64(ok) / float64(total) * 100
	t.Logf("黄金语料: 总计 %d | 自动入库 %d (%.1f%%) | 需人工确认 %d | 空片名 %d | 残壳 %d",
		total, ok, rate, review, noTitle, shell)
	for _, s := range badSamples {
		t.Logf("  样本 %s", s)
	}

	// 验收门槛：自动入库率 ≥ 95%；空片名与残壳必须为 0
	if rate < 95.0 {
		t.Errorf("自动入库率 %.1f%% < 95%% 门槛", rate)
	}
	if noTitle > 0 || shell > 0 {
		t.Errorf("空片名 %d / 残壳 %d，必须为 0", noTitle, shell)
	}
}

func filepathExt(s string) string {
	if i := strings.LastIndex(s, "."); i > 0 {
		return s[i:]
	}
	return ""
}

// ============================================================
// 不变量测试：同一部剧的所有文件必须落到同一个剧名目录
// 真实数据隱患：「吞噬星空」单集文件带 2020/2024/2026 三种年份，
// 若从文件名取年份会拆成 3 个目录 → 飞牛里变成 3 部番
// ============================================================

func TestFolderIdentityStable(t *testing.T) {
	f, err := os.Open("testdata/corpus_files.txt")
	if err != nil {
		t.Skipf("语料缺失: %v", err)
	}
	defer f.Close()

	// 原始剧名目录 → 生成的目标目录集合
	folders := map[string]map[string]int{}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		parts := strings.Split(line, "/")
		if len(parts) < 3 {
			continue // 只看有剧名目录的
		}
		cat := Category(parts[0])
		dir, sub, file := parts[1], "", parts[len(parts)-1]
		if len(parts) >= 4 {
			sub = parts[2]
		}
		info := ParsePackage(cat, dir, sub, file)
		path := info.BuildPath()
		if path == "" {
			continue
		}
		// 剧场版/合集篇是经用户确认的「故意拆分」（独立 TMDB 条目 / 方案 C），不算拆分异常
		if kind, _ := ParseSubDir(sub); kind == SubMovie || kind == SubCompilation {
			continue
		}
		// 取生成路径的前两段（分类/剧名目录）
		seg := strings.Split(path, "/")
		target := seg[0] + "/" + seg[1]
		if folders[dir] == nil {
			folders[dir] = map[string]int{}
		}
		folders[dir][target]++
	}

	for srcDir, targets := range folders {
		if len(targets) > 1 {
			t.Errorf("剧名目录 %q 被拆成 %d 个目标目录（会在飞牛里变成多部番）:", srcDir, len(targets))
			for tgt, n := range targets {
				t.Errorf("    %s  (%d 个文件)", tgt, n)
			}
		}
	}
	t.Logf("共检查 %d 个剧名目录，均映射到单一目标目录", len(folders))
}

// ============================================================
// 安全闸：不得有两个源文件映射到同一目标路径
// 碰撞 = 文件互相覆盖 = 数据丢失，落地前必须为零
// 真实数据：合集篇/001 4K.mp4 ~ 018 4K.mp4 无 S/E 标记，曾全部撞到同一名
// ============================================================

func TestNoPathCollision(t *testing.T) {
	f, err := os.Open("testdata/corpus_files.txt")
	if err != nil {
		t.Skipf("语料缺失: %v", err)
	}
	defer f.Close()

	target := map[string][]string{} // 目标路径 → 源文件列表
	total := 0
	type pair struct {
		info *MediaInfo
		src  string
	}
	var all []pair

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, "/")
		if len(parts) < 2 {
			continue
		}
		cat := Category(parts[0])
		var dir, sub, file string
		switch len(parts) {
		case 2:
			dir, sub, file = strings.TrimSuffix(parts[1], filepathExt(parts[1])), "", parts[1]
		case 3:
			dir, sub, file = parts[1], "", parts[2]
		default:
			dir, sub, file = parts[1], parts[2], parts[len(parts)-1]
		}
		info := ParsePackage(cat, dir, sub, file)
		all = append(all, pair{info, line})
	}

	// 真实管道行为：全量解析 → 批量消歧 → 再落地
	infos := make([]*MediaInfo, 0, len(all))
	for _, p := range all {
		infos = append(infos, p.info)
	}
	fixed := Disambiguate(infos)
	t.Logf("批量消歧：%d 个文件补上版本标签", fixed)

	for _, p := range all {
		path := p.info.BuildPath()
		if path == "" {
			continue
		}
		total++
		target[path] = append(target[path], p.src)
	}

	collisions := 0
	lost := 0
	for p, srcs := range target {
		if len(srcs) > 1 {
			collisions++
			lost += len(srcs) - 1
			if collisions <= 3 {
				t.Errorf("碰撞: %d 个源文件 → 同一目标 %q", len(srcs), p)
				for _, s := range srcs[:minInt(4, len(srcs))] {
					t.Errorf("      ← %s", s)
				}
			}
		}
	}
	if collisions > 0 {
		t.Errorf("共 %d 组碰撞，将丢失 %d 个文件（总计 %d）", collisions, lost, total)
	} else {
		t.Logf("%d 个文件全部映射到唯一路径，零碰撞", total)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ============================================================
// 乱码名映射（P1-4 功能回归修复验证）
// ============================================================

// ============================================================
// 用户决策验收（2026-08-19）
//   C: 合集篇 → 独立条目「片名 合集篇」
//   2: 剧场版 → 独立条目
//   4: 版本标记保留，如 神临之战 (年份) - 杜比音效.mkv
// ============================================================

func TestUserDecision_Compilation(t *testing.T) {
	// 方案 C：合集篇拆为独立条目，不再挂起待确认
	cases := []struct {
		dir, sub, file, want string
	}{
		{"T  吞噬星空（罗峰）", "合集篇", "001 4K.mp4",
			"动漫/吞噬星空 合集篇/Season 01/吞噬星空 合集篇 - S01E01.mp4"},
		{"T  吞噬星空（罗峰）", "合集篇", "018 4K.mp4",
			"动漫/吞噬星空 合集篇/Season 01/吞噬星空 合集篇 - S01E18.mp4"},
	}
	for _, c := range cases {
		info := ParsePackage(CatAnime, c.dir, c.sub, c.file)
		if info.NeedsReview {
			t.Errorf("合集篇已定为独立条目，不应再挂起: %s (%s)", c.file, info.ReviewReason)
			continue
		}
		if got := info.BuildPath(); got != c.want {
			t.Errorf("合集篇路径\n  got  %q\n  want %q", got, c.want)
		}
	}

	// 合集篇不得继承正片的 TMDB ID（否则两条目指向同一影片）
	info := ParsePackage(CatAnime, "凡人修仙传(2020) {tmdb-288233}", "合集篇", "001 4K.mp4")
	if info.TMDBID != 0 {
		t.Errorf("合集篇不应继承正片 TMDB ID，得到 %d", info.TMDBID)
	}
}

func TestUserDecision_Edition(t *testing.T) {
	cases := []struct {
		dir, sub, file, wantEdition, wantPath string
	}{
		// 用户指定格式：神临之战 (年份) - 杜比音效.mkv（年份待 TMDB 补全，此处无年份）
		{"X   仙逆（王麻子）", "剧场版", "神临之战 4K 杜比音效.mkv", "杜比音效",
			"动漫/神临之战/神临之战 - 杜比音效.mkv"},
		{"X   仙逆（王麻子）", "剧场版", "神临之战 4K.mp4", "",
			"动漫/神临之战/神临之战.mp4"},
		// 重置版在目录名里：不保留则日后存入原版 Season 1 会撞名覆盖
		{"凡人修仙传(2020) {tmdb-288233}", "Season 1 风起天南 重置版", "S01E001.风起天南1.4K.HDR.HEVC.mp4", "重置版",
			"动漫/凡人修仙传 (2020) {tmdb-288233}/Season 01/凡人修仙传 - S01E01 - 重置版.mp4"},
		// 导剪版特典
		{"凡人修仙传(2020) {tmdb-288233}", "Specials 虚天战纪 导剪版", "S00E22 - 虚天战纪 导演剪辑版（上）.mp4", "导演剪辑版",
			"动漫/凡人修仙传 (2020) {tmdb-288233}/Specials/凡人修仙传 - S00E22 - 导演剪辑版.mp4"},
	}
	for _, c := range cases {
		info := ParsePackage(CatAnime, c.dir, c.sub, c.file)
		if info.Edition != c.wantEdition {
			t.Errorf("Edition(%q) = %q, want %q", c.file, info.Edition, c.wantEdition)
		}
		if got := info.BuildPath(); got != c.wantPath {
			t.Errorf("路径(%q)\n  got  %q\n  want %q", c.file, got, c.wantPath)
		}
	}
}

func TestEdition_ExcludesNoise(t *testing.T) {
	// 中字(208)/SDR(232)/HDR(194) 频次极高，当版本标签会污染 200+ 文件名
	noisy := []string{
		"S01E001.2020.2160p.WEB-DL.SDR.H265.8bit.AAC.mp4",
		"{tv tmdb-124003}.2021.S01E100.第100集.1080p.SDR.H.265.25fps.AAC 2.0.mkv",
		"S01E001.风起天南1.4K.HDR.HEVC.mp4",
	}
	for _, f := range noisy {
		info := ParsePackage(CatAnime, "牛神记 (2024) {tmdb-236534}", "Season 1", f)
		if info.Edition != "" {
			t.Errorf("%q 不应产生版本标签，得到 %q", f, info.Edition)
		}
	}
}

func TestNameMap(t *testing.T) {
	nm := NewNameMap()
	// 来自现有 tg-media-bot 的 quark-name-map.json
	for alias, canonical := range map[string]string{
		"H 椛幵婂秀":   "花开锦秀",
		"花开锦绣":     "花开锦秀",
		"TCNY":     "天才女友",
		"R 亼魚":     "人鱼",
		"Z 喠 噐":    "重器",
		"玉.ting.谣": "御廷谣",
	} {
		nm.Set(alias, canonical)
	}

	cases := []struct{ in, want string }{
		{"H 椛幵婂秀", "花开锦秀"},
		{"H椛幵婂秀", "花开锦秀"}, // 容错：去空白后仍命中
		{"TCNY", "天才女友"},
		{"tcny", "天才女友"},    // 容错：大小写
		{"玉 ting 谣", "御廷谣"}, // 容错：点→空格后仍命中
		{"Z 喠 噐", "重器"},
		{"不存在的名字", "不存在的名字"}, // 未命中原样返回
	}
	for _, c := range cases {
		got, _ := nm.Resolve(c.in)
		if got != c.want {
			t.Errorf("NameMap.Resolve(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ============================================================
// TMDB 匹配校验：年份是最强判据
// 真实踩坑：搜「流浪地球」TMDB 首条返回「流浪地球2」，
// 盲取 Results[0] 会让 流浪地球(2019) 与 流浪地球2(2023) 共用 tmdb-842675
// ============================================================

func TestPickBest_YearDisambiguation(t *testing.T) {
	// 真实 TMDB 返回顺序（已实测）
	liulang := []TMDBResult{
		{ID: 842675, MediaType: "movie", Title: "流浪地球2", OriginalTitle: "流浪地球2", ReleaseDate: "2023-01-22"},
		{ID: 535167, MediaType: "movie", Title: "流浪地球", OriginalTitle: "流浪地球", ReleaseDate: "2019-02-05"},
		{ID: 1231322, MediaType: "movie", Title: "流浪地球3（上）", OriginalTitle: "流浪地球3（上）"},
	}
	nezha := []TMDBResult{
		{ID: 628202, MediaType: "movie", Title: "新封神之哪吒闹海", OriginalTitle: "新封神之哪吒闹海", ReleaseDate: "2019-01-01"},
		{ID: 74037, MediaType: "movie", Title: "哪吒闹海", OriginalTitle: "哪吒闹海", ReleaseDate: "1979-01-01"},
	}

	cases := []struct {
		query, year string
		results     []TMDBResult
		wantID      int
	}{
		{"流浪地球", "2019", liulang, 535167},  // 必须选第 2 条
		{"流浪地球2", "2023", liulang, 842675}, // 续集选第 1 条
		{"哪吒闹海", "1979", nezha, 74037},     // 必须选第 2 条
	}
	for _, c := range cases {
		got := pickBest(c.query, c.year, c.results)
		if got == nil {
			t.Errorf("pickBest(%q, %q) = nil，期望 id=%d", c.query, c.year, c.wantID)
			continue
		}
		if got.ID != c.wantID {
			t.Errorf("pickBest(%q, %q) = id %d (%s)，期望 id %d",
				c.query, c.year, got.ID, got.CNName(), c.wantID)
		}
	}
}

func TestPickBest_RejectsMismatch(t *testing.T) {
	// 年份差太多 / 片名毫不相干 → 必须拒绝，宁可没有 tmdb-id 也不能认错片
	results := []TMDBResult{
		{ID: 111, MediaType: "movie", Title: "完全不相干的片", ReleaseDate: "1999-01-01"},
	}
	if got := pickBest("吞噬星空", "2020", results); got != nil {
		t.Errorf("片名与年份均不匹配，应拒绝，却返回 id=%d", got.ID)
	}
	// 排除 person 类结果
	people := []TMDBResult{{ID: 222, MediaType: "person", Name: "吞噬星空"}}
	if got := pickBest("吞噬星空", "", people); got != nil {
		t.Errorf("person 类型应被排除，却返回 id=%d", got.ID)
	}
}

// 真实数据抓到的崩溃：夸克分享 "Z - 罪 - A" 触发
// pickBest 全率否决 → Search 返回 (nil, nil) → Enrich 解引用 nil → panic
func TestSearch_NeverReturnsNilNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 返回一个年份差极大的结果，pickBest 会全部否决
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[
			{"id":999,"name":"完全不相关的老片","first_air_date":"1950-01-01","media_type":"tv"}
		]}`))
	}))
	defer srv.Close()

	c := NewTMDBClient("dummy")
	c.BaseURL = srv.URL

	r, err := c.Search(context.Background(), "罪", "2024")
	if err == nil && r == nil {
		t.Fatal("Search 返回了 (nil, nil)：调用方必然 panic（这是真实崩溃的根因）")
	}
	if err != nil && r != nil {
		t.Error("同时返回结果和错误，语义不明")
	}
}

// Enrich 拿到无匹配时必须返回 error 而不是崩溃
func TestEnrich_NoMatchReturnsErrorNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[
			{"id":999,"name":"无关","first_air_date":"1950-01-01","media_type":"tv"}
		]}`))
	}))
	defer srv.Close()

	c := NewTMDBClient("dummy")
	c.BaseURL = srv.URL

	info := &MediaInfo{Title: "罪", Year: "2024", Category: CatAnime}
	err := c.Enrich(context.Background(), info) // 不得 panic
	if err == nil {
		t.Error("无匹配时应返回错误")
	}
	if info.TMDBID != 0 {
		t.Errorf("失败时不应写入 TMDBID，得到 %d", info.TMDBID)
	}
}

// 真实数据暴露：17 个文件同片名查不到时发了 17 次重复请求（只缓存成功结果）
// 961 文件批量跑会直接撞 TMDB 限流
func TestSearch_CachesNegativeResults(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	c := NewTMDBClient("dummy")
	c.BaseURL = srv.URL

	for i := 0; i < 10; i++ {
		if _, err := c.Search(context.Background(), "查不到的片名", "2026"); err == nil {
			t.Fatal("应返回错误")
		}
	}
	got := atomic.LoadInt32(&hits)
	// 中文一次 + 英文 fallback 一次 = 2；不该是 20
	if got > 2 {
		t.Errorf("失败结果未缓存：10 次查询发了 %d 个请求（应 ≤2），批量跑必撞限流", got)
	}
}

// 封死「nil 进缓存 → 缓存命中直接 return (nil,nil) → 调用方 panic」这条路
func TestSearch_CacheNeverYieldsNilNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"id":1,"name":"无关","first_air_date":"1950-01-01","media_type":"tv"}]}`))
	}))
	defer srv.Close()

	c := NewTMDBClient("dummy")
	c.BaseURL = srv.URL

	// 人为把 nil 塞进缓存，模拟未来改动引入的回归
	c.cache["毒药|2026"] = nil

	r, err := c.Search(context.Background(), "毒药", "2026")
	if err == nil && r == nil {
		t.Fatal("缓存里的 nil 被原样返回 → 调用方必 panic")
	}
}
