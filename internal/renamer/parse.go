package renamer

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ============================================================
// 设计原则：结构化重建（Structural Rebuild）
//   解析 → 字段 → 丢弃原串 → 用字段拼装输出
// 严禁在原字符串上做"删词"（减法清洗会留下 "流浪地球 ()" 这种残壳）
// ============================================================

// Category 一级分类（依据 NAS 真实结构：电影/电视剧/动漫/音乐）
type Category string

const (
	CatMovie Category = "电影"
	CatTV    Category = "电视剧"
	CatAnime Category = "动漫"
	CatMusic Category = "音乐"
)

// MediaInfo 解析出的结构化字段
type MediaInfo struct {
	Category Category
	Title    string // 清洗后的规范片名
	Year     string // 4 位年份
	TMDBID   int    // 已知的 TMDB ID（0=未知）
	TMDBType string // "tv" | "movie" | ""

	Season       int
	SeasonFound  bool
	Episode      int
	EpisodeFound bool

	Version    string // 版本标记（v2 修正版），避免同集不同版本互相覆盖
	Edition    string // 版本区分标签（批量消歧时填充，如 2160p / 1080p）
	Resolution string // 解析出的分辨率，用于多版本消歧

	// 安全阀：无法确定归属的不自动落地，交人工确认（宁可不入库，不可覆盖）
	NeedsReview  bool
	ReviewReason string

	Ext string

	// 溯源（调试/审计用，不参与输出拼装）
	RawDir  string
	RawSub  string
	RawFile string
}

// IsMovie 是否按电影布局输出
func (m *MediaInfo) IsMovie() bool { return !m.EpisodeFound }

// ============================================================
// 技术标记识别：用于"片名 = 第一个技术标记之前的部分"
// ============================================================

var techRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)S\d{1,2}E\d{1,4}`),                                                    // S01E231
	regexp.MustCompile(`(?i)\bEP?\d{1,4}\b`),                                                      // E01 / EP01
	regexp.MustCompile(`第\d{1,4}[集话期]`),                                                           // 第81集
	regexp.MustCompile(`\b(19|20)\d{2}\b`),                                                        // 年份
	regexp.MustCompile(`(?i)\b\d{3,4}[pi]\b`),                                                     // 1080p / 2160i
	regexp.MustCompile(`(?i)\b4K\b`),                                                              // 4K
	regexp.MustCompile(`(?i)\b(WEB-?DL|WEBRip|BluRay|BDRip|HDRip|DVDRip|REMUX|SDR|HDR|DHR)\b`),    // 片源
	regexp.MustCompile(`(?i)(\bH\.?26[45]\b|\bx26[45]\b|\bHEVC\b|\bAVC\b|\b\d{1,2}bit\b)`),        // 编码
	regexp.MustCompile(`(?i)(\bAAC\b|\bFLAC\b|\bDTS\b|\bAtmos\b|\bTrueHD\b|\b(DDP?|HIFI)\d\.\d)`), // 音频
	regexp.MustCompile(`(国语|粤语|国粤|中字|中英|简繁|双语|多音轨)`),                                              // 语言
}

// firstTechIndex 返回最早出现的技术标记位置，-1 表示没有
func firstTechIndex(s string) int {
	idx := -1
	for _, re := range techRes {
		if loc := re.FindStringIndex(s); loc != nil {
			if idx == -1 || loc[0] < idx {
				idx = loc[0]
			}
		}
	}
	return idx
}

// ============================================================
// 目录名解析（片名主要来源：61% 的文件名里没有片名）
// ============================================================

var (
	// {tmdb-236534} 或 {tv tmdb-124003}
	reTMDBTag = regexp.MustCompile(`\{\s*(?:(tv|movie)\s+)?tmdb-(\d+)\s*\}`)
	// 资源站方括号元数据块 [S01][1080p][HEVC]...
	reBracketBlock = regexp.MustCompile(`\[[^\]]*\]`)
	// 体积统计 (71.0GB 205个文件)
	reVolumeStats = regexp.MustCompile(`\([\d.]+\s*[KMGT]B[^)]*\)`)
	// 年份（含紧贴括号的写法：凡人修仙传(2020)）
	reYear = regexp.MustCompile(`\(?\b((?:19|20)\d{2})\b\)?`)
	// 首字母索引前缀："T  吞噬星空" / "X   仙逆" / "W-万古至尊"
	// 只在后面跟汉字时才剥（避免误杀 X-Men）
	reIndexPrefix = regexp.MustCompile(`^[A-Za-z][\s\-]+(\p{Han})`)
	// 全角括号内容（作者/搬运者标注）：（罗峰）（王麻子）
	reFullWidthParen = regexp.MustCompile(`（[^）]*）`)
	// 多余空白
	reSpaces = regexp.MustCompile(`\s{2,}`)
)

// ParseDirName 从剧名目录解析片名/年份/TMDB ID
// 例：
//
//	"T  吞噬星空（罗峰）"                                    -> 吞噬星空
//	"完美世界 (2021){tv tmdb-124003}[S01][1080p]...(71.0GB 205个文件)" -> 完美世界 2021 tmdb=124003
//	"凡人修仙传(2020) {tmdb-288233}"                        -> 凡人修仙传 2020 tmdb=288233
func ParseDirName(dir string) (title, year string, tmdbID int, tmdbType string) {
	s := dir

	// 1. 抽取 TMDB 标记
	if m := reTMDBTag.FindStringSubmatch(s); m != nil {
		tmdbType = m[1]
		tmdbID, _ = strconv.Atoi(m[2])
		s = reTMDBTag.ReplaceAllString(s, " ")
	}

	// 2. 移除资源站噪音（方括号块、体积统计）
	s = reBracketBlock.ReplaceAllString(s, " ")
	s = reVolumeStats.ReplaceAllString(s, " ")

	// 3. 抽取年份
	if m := reYear.FindStringSubmatch(s); m != nil {
		year = m[1]
		s = reYear.ReplaceAllString(s, " ")
	}

	// 4. 去首字母索引前缀
	s = reIndexPrefix.ReplaceAllString(s, "$1")

	// 5. 去全角括号标注（作者/搬运者）
	s = reFullWidthParen.ReplaceAllString(s, " ")

	// 6. 若仍有技术标记，截断到第一个技术标记之前
	if i := firstTechIndex(s); i > 0 {
		s = s[:i]
	}

	title = normalizeTitle(s)
	return
}

// normalizeTitle 统一空白与边缘符号（不做词汇删除）
func normalizeTitle(s string) string {
	s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	s = reSpaces.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	// 清掉解析后可能孤立残留的成对空壳
	s = strings.NewReplacer("()", "", "（）", "", "[]", "", "{}", "").Replace(s)
	// 截断后可能留下悬空括号（如 "Nezha ... King ("），一并 Trim
	s = strings.Trim(s, " -_.·,()[]{}（）【】")
	return strings.TrimSpace(s)
}

// ============================================================
// 中间层目录解析：区分「真 Season 目录」与「资源站分卷目录」
// ============================================================

var (
	reSeasonDir = regexp.MustCompile(`(?i)^Season\s*(\d{1,2})`) // Season 1 / Season 6 星海飞驰
	reSeasonCN  = regexp.MustCompile(`^第\s*(\d{1,2})\s*季`)
	// 分卷目录：001-100集 / 101-200集 / 001-100 4K / 101-150 4K / 合集篇
	reVolumeDir = regexp.MustCompile(`^\d{1,4}\s*[-—~]\s*\d{1,4}|^\d{1,4}\s*集$`)
	// 合集/精华目录：另一种剪辑版，集号体系与正片不同，不可当正片入库
	reCompilationDir = regexp.MustCompile(`^(合集|全集|精华|回顾)`)
	// 版本标记：HEVC(v2) / [v2] / .v2.
	reVersionTag = regexp.MustCompile(`(?i)[\(\[\.]v(\d)[\)\]\.]?`)
	// 剧场版/特典目录：内部是独立电影，片名在文件名而非目录名
	reMovieDir = regexp.MustCompile(`^(剧场版|剧场|电影版|OVA|OAD)`)
	// Emby 官方特典目录 = Season 00（真实数据："Specials 虚天战纪 导剪版"）
	reSpecialsDir = regexp.MustCompile(`(?i)^(Specials?|SP\b|特典|番外)`)
	// 分辨率（多版本消歧用）
	reResolution = regexp.MustCompile(`(?i)\b(2160[pi]|1080[pi]|720[pi]|480[pi]|4K)\b`)
	// 版本区分标记白名单（数据驱动）：只收录真正区分版本的词。
	// 实测语料频次：中字 208 / SDR 232 / HDR 194 —— 这些是字幕与画质属性，
	// 当版本标签会污染 200+ 文件名，故排除。
	reEdition = regexp.MustCompile(`(杜比全景声|杜比视界|杜比音效|导演剪辑版|导剪版|重置版|修复版|加长版|未删减版|完整版)`)
)

// SubDirKind 中间层目录类型
type SubDirKind int

const (
	SubNone        SubDirKind = iota // 无中间层
	SubSeason                        // 真 Season 目录 → 取季号
	SubVolume                        // 区间分卷目录 → 扁平化丢弃
	SubCompilation                   // 合集/精华篇 → 剪辑版，需人工确认
	SubMovie                         // 剧场版/特典 → 按独立电影处理
	SubOther                         // 其他（保守当分卷处理）
)

// ParseSubDir 解析中间层目录
func ParseSubDir(sub string) (kind SubDirKind, season int) {
	if sub == "" {
		return SubNone, 0
	}
	if reMovieDir.MatchString(sub) {
		return SubMovie, 0
	}
	// Specials 先于 Season 判断：Emby 约定特典即 Season 00
	if reSpecialsDir.MatchString(sub) {
		return SubSeason, 0
	}
	if reCompilationDir.MatchString(sub) {
		return SubCompilation, 0
	}
	if m := reSeasonDir.FindStringSubmatch(sub); m != nil {
		season, _ = strconv.Atoi(m[1])
		return SubSeason, season
	}
	if m := reSeasonCN.FindStringSubmatch(sub); m != nil {
		season, _ = strconv.Atoi(m[1])
		return SubSeason, season
	}
	if reVolumeDir.MatchString(sub) {
		return SubVolume, 0
	}
	return SubOther, 0
}

// 纯数字前缀集号："001 4K.mp4" / "018 4K.mp4"（仅在剧集包上下文启用，避免误伤《2012》这类片名）
var reEpLeadNum = regexp.MustCompile(`^(\d{1,4})(?:\D|$)`)

// leadingEpisode 从文件名开头取集号；年份（1900-2099）不算集号
func leadingEpisode(file string) (int, bool) {
	m := reEpLeadNum.FindStringSubmatch(file)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n == 0 {
		return 0, false
	}
	if len(m[1]) == 4 && n >= 1900 && n <= 2099 {
		return 0, false // 是年份，不是集号
	}
	return n, true
}

// ============================================================
// 文件名解析：季/集号 + 兜底片名
// ============================================================

var (
	reSxxExx  = regexp.MustCompile(`(?i)S(\d{1,2})E(\d{1,4})`)
	reEpOnly  = regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9])EP?(\d{1,4})(?:[^0-9]|$)`)
	reEpCN    = regexp.MustCompile(`第(\d{1,4})[集话期]`)
	reSeasonF = regexp.MustCompile(`(?i)S(\d{1,2})(?:[^0-9E]|$)`)
)

// ParseFileName 解析文件名，返回季/集号与文件名中可提取的片名（可能为空）
func ParseFileName(file string) (title string, season, episode int, seasonFound, epFound bool, ext string) {
	name := file
	if i := strings.LastIndex(name, "."); i > 0 {
		ext = strings.ToLower(name[i+1:])
		name = name[:i]
	}

	// 季+集：S01E231
	if m := reSxxExx.FindStringSubmatch(name); m != nil {
		season, _ = strconv.Atoi(m[1])
		episode, _ = strconv.Atoi(m[2])
		seasonFound, epFound = true, true
	} else {
		// 仅集号
		if m := reEpCN.FindStringSubmatch(name); m != nil {
			episode, _ = strconv.Atoi(m[1])
			epFound = true
		} else if m := reEpOnly.FindStringSubmatch(name); m != nil {
			episode, _ = strconv.Atoi(m[1])
			epFound = true
		}
		// 仅季号
		if m := reSeasonF.FindStringSubmatch(name); m != nil {
			season, _ = strconv.Atoi(m[1])
			seasonFound = true
		}
	}

	// 片名 = 第一个技术标记之前的部分（纯技术名会得到空串，由目录名兜底）
	if i := firstTechIndex(name); i > 0 {
		title = normalizeTitle(name[:i])
	} else if i == -1 {
		title = normalizeTitle(name)
	}
	return
}

// ============================================================
// 联合识别：分类/剧名目录/[中间层]/文件名
// ============================================================

// ParsePackage 联合识别一个媒体文件
// category: 电影/电视剧/动漫；dir: 剧名目录；sub: 中间层（可空）；file: 文件名
func ParsePackage(category Category, dir, sub, file string) *MediaInfo {
	info := &MediaInfo{
		Category: category,
		RawDir:   dir,
		RawSub:   sub,
		RawFile:  file,
	}

	dTitle, dYear, tmdbID, tmdbType := ParseDirName(dir)
	info.TMDBID, info.TMDBType, info.Year = tmdbID, tmdbType, dYear

	fTitle, fSeason, fEp, fSeasonOK, fEpOK, ext := ParseFileName(file)
	info.Ext = ext
	info.Episode, info.EpisodeFound = fEp, fEpOK

	kind, sSeason := ParseSubDir(sub)

	// 分辨率（多版本消歧用）
	if m := reResolution.FindStringSubmatch(file); m != nil {
		info.Resolution = strings.ToLower(m[1])
	}
	// 版本标记：文件名优先，其次中间层目录
	// （真实数据：「神临之战 4K 杜比音效.mkv」在文件名；
	//   「Season 1 风起天南 重置版」在目录名，不保留则日后存入原版会撞名覆盖）
	if m := reEdition.FindStringSubmatch(file); m != nil {
		info.Edition = m[1]
	} else if m := reEdition.FindStringSubmatch(sub); m != nil {
		info.Edition = m[1]
	}

	// 版本标记：同一集的 v2 修正版必须保留区分，否则互相覆盖（真实数据：凡人修仙传 S09E180 有 v2）
	if m := reVersionTag.FindStringSubmatch(file); m != nil {
		info.Version = "v" + m[1]
	}

	// 合集/精华篇：集号体系与正片不同（合集篇 011 ≠ 正片 S01E011）。
	// 用户决策（方案 C）：拆为独立条目「片名 + 合集篇」，与正片彻底隔离
	if kind == SubCompilation {
		base := dTitle
		if base == "" {
			base = fTitle
		}
		suffix := normalizeTitle(reEdition.ReplaceAllString(sub, " "))
		if suffix == "" {
			suffix = "合集篇"
		}
		info.Title = strings.TrimSpace(base + " " + suffix)
		info.TMDBID, info.TMDBType = 0, "" // 独立条目，不继承正片 TMDB ID
		info.Year = ""
		// 集号：先用文件名解析，再用纯数字前缀兜底（合集篇/001 4K.mp4）
		if !info.EpisodeFound {
			if n, ok := leadingEpisode(file); ok {
				info.Episode, info.EpisodeFound = n, true
			}
		}
		if info.EpisodeFound {
			info.Season, info.SeasonFound = 1, true
		} else {
			info.NeedsReview = true
			info.ReviewReason = "合集篇文件无法解析出集号"
		}
		return info
	}

	// 剧场版/特典目录：内部是独立电影（如 吞噬星空/剧场版/决战原始星.2026）
	// 片名、年份均取自文件名，不继承剧名目录
	if kind == SubMovie {
		info.Title = fTitle
		info.EpisodeFound = false
		info.TMDBID, info.TMDBType = 0, "" // 剧场版是独立 TMDB 条目
		info.Year = ""
		if m := reYear.FindStringSubmatch(file); m != nil {
			info.Year = m[1]
		}
		return info
	}

	// 片名优先级：目录名 > 文件名（61% 文件名无片名）
	info.Title = dTitle
	if info.Title == "" {
		info.Title = fTitle
	}

	// 纯数字前缀集号兜底：仅区间分卷/真 Season 目录（合集篇已在上方走 NeedsReview）
	if !info.EpisodeFound && (kind == SubVolume || kind == SubSeason) {
		if n, ok := leadingEpisode(file); ok {
			info.Episode, info.EpisodeFound = n, true
		}
	}

	// 季号优先级：真 Season 目录 > 文件名；分卷目录一律丢弃
	switch {
	case kind == SubSeason:
		info.Season, info.SeasonFound = sSeason, true
	case fSeasonOK:
		info.Season, info.SeasonFound = fSeason, true
	}
	// Specials 目录（season=0）下的 S00Exx：以文件名季号为准
	if kind == SubSeason && sSeason == 0 && fSeasonOK {
		info.Season = fSeason
	}
	// 是剧集但没解析到季号 → 默认第 1 季
	if info.EpisodeFound && !info.SeasonFound {
		info.Season, info.SeasonFound = 1, true
	}

	// 年份兜底：仅电影允许从文件名取年份。
	// 剧集严禁从单集文件名取年份 —— 真实数据中「吞噬星空」单集文件带 2020/2024/2026 三种年份，
	// 若据此命名会把同一部剧拆成 3 个目录，飞牛里变成 3 部番。
	// 剧集的年份只能来自目录名或 TMDB（剧集级，稳定）。
	if info.Year == "" && !info.EpisodeFound {
		if m := reYear.FindStringSubmatch(file); m != nil {
			info.Year = m[1]
		}
	}
	return info
}

// ============================================================
// 输出拼装：只用字段，绝不碰原串
// ============================================================

var reIllegal = regexp.MustCompile(`[/\\:*?"<>|]`)

func sanitize(s string) string {
	return strings.TrimSpace(reIllegal.ReplaceAllString(s, ""))
}

// folderName 拼装剧名目录：片名 (年份) {tmdb-ID}
func (m *MediaInfo) folderName() string {
	var b strings.Builder
	b.WriteString(sanitize(m.Title))
	if m.Year != "" {
		b.WriteString(" (" + m.Year + ")")
	}
	if m.TMDBID > 0 {
		b.WriteString(" {tmdb-" + strconv.Itoa(m.TMDBID) + "}")
	}
	return b.String()
}

// BuildPath 生成规范落地路径
//
//	电影:      电影/片名 (年份) {tmdb-N}/片名 (年份).ext
//	剧集/动漫: 动漫/片名 (年份) {tmdb-N}/Season 01/片名 - S01E231.ext
//
// 返回空串 = 不可自动落地（片名缺失或 NeedsReview）
func (m *MediaInfo) BuildPath() string {
	if m.Title == "" || m.NeedsReview {
		return ""
	}
	folder := m.folderName()
	base := sanitize(m.Title)
	ext := ""
	if m.Ext != "" {
		ext = "." + m.Ext
	}
	// 版本后缀：同集 v2 修正版不得与原版同名（否则相互覆盖）
	ver := ""
	if m.Version != "" {
		ver = " - " + m.Version
	}
	if m.Edition != "" {
		ver += " - " + m.Edition
	}

	if m.IsMovie() {
		fileBase := base
		if m.Year != "" {
			fileBase += " (" + m.Year + ")"
		}
		return fmt.Sprintf("%s/%s/%s%s%s", m.Category, folder, fileBase, ver, ext)
	}

	season := m.Season
	if !m.SeasonFound {
		season = 1
	}
	// Emby 约定：Season 00 目录名为 Specials
	seasonDir := fmt.Sprintf("Season %02d", season)
	if season == 0 {
		seasonDir = "Specials"
	}
	return fmt.Sprintf("%s/%s/%s/%s - S%02dE%02d%s%s",
		m.Category, folder, seasonDir, base, season, m.Episode, ver, ext)
}

// ============================================================
// 批量规划：落地前必须先算全量、再消歧
// 逐文件独立决策无法发现碰撞（真实数据：哪吒 2160p 国语版与 1080p HBOMax 版同名）
// ============================================================

// Disambiguate 消除批量内的目标路径碰撞；无法自动消歧的标为 NeedsReview
// 返回发生消歧的文件数
func Disambiguate(infos []*MediaInfo) int {
	groups := map[string][]*MediaInfo{}
	for _, in := range infos {
		if p := in.BuildPath(); p != "" {
			groups[p] = append(groups[p], in)
		}
	}

	fixed := 0
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		// 优先用分辨率区分（Emby 多版本约定）
		seen := map[string]int{}
		for _, in := range g {
			seen[in.Resolution]++
		}
		allDistinct := len(seen) == len(g)
		for i, in := range g {
			switch {
			case allDistinct && in.Resolution != "":
				// 不覆盖已解析出的版本标记（如杜比音效），追加而非替换
				if in.Edition == "" {
					in.Edition = in.Resolution
				} else {
					in.Edition += " " + in.Resolution
				}
				fixed++
			default:
				// 分辨率也区分不了 → 不猜，交人工（首个保留，其余挂起）
				if i > 0 {
					in.NeedsReview = true
					in.ReviewReason = "与其他文件目标路径碰撞且无法自动区分，需人工确认"
				}
			}
		}
	}
	return fixed
}
