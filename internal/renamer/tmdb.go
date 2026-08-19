package renamer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LoadAPIKey 读取 TMDB Key，优先级：TMDB_API_KEY > TMDB_API_KEY_FILE > 默认路径
// 使用文件方式可避免密钥进入环境变量、进程列表与 shell 历史
func LoadAPIKey() string {
	if k := strings.TrimSpace(os.Getenv("TMDB_API_KEY")); k != "" {
		return k
	}
	paths := []string{os.Getenv("TMDB_API_KEY_FILE")}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, home+"/.config/fnos-enhance/tmdb.key")
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if b, err := os.ReadFile(p); err == nil {
			if k := strings.TrimSpace(string(b)); k != "" {
				return k
			}
		}
	}
	return ""
}

// TMDBResult TMDB 查询结果
type TMDBResult struct {
	ID            int     `json:"id"`
	MediaType     string  `json:"media_type"`
	Title         string  `json:"title"` // 电影中文名
	Name          string  `json:"name"`  // 剧集中文名
	OriginalTitle string  `json:"original_title"`
	OriginalName  string  `json:"original_name"`
	ReleaseDate   string  `json:"release_date"`
	FirstAirDate  string  `json:"first_air_date"`
	Popularity    float64 `json:"popularity"`
}

func (t *TMDBResult) CNName() string {
	if t.Title != "" {
		return t.Title
	}
	if t.Name != "" {
		return t.Name
	}
	return t.OriginalTitle
}

func (t *TMDBResult) Year() string {
	d := t.ReleaseDate
	if d == "" {
		d = t.FirstAirDate
	}
	if len(d) >= 4 {
		return d[:4]
	}
	return ""
}

// TMDBClient 并发安全的 TMDB 客户端（P1-3：原实现裸 map 无锁）
type TMDBClient struct {
	apiKey string
	http   *http.Client

	mu    sync.RWMutex
	cache map[string]*TMDBResult
}

func NewTMDBClient(apiKey string) *TMDBClient {
	return &TMDBClient{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 10 * time.Second},
		cache:  make(map[string]*TMDBResult),
	}
}

// Search 按片名搜索并**校验**后返回最佳匹配（并发安全缓存 + context 超时）
// year 为已知年份（可空），是区分系列片与续集的关键判据
func (c *TMDBClient) Search(ctx context.Context, name, year string) (*TMDBResult, error) {
	key := name + "|" + year
	c.mu.RLock()
	if r, ok := c.cache[key]; ok {
		c.mu.RUnlock()
		return r, nil
	}
	c.mu.RUnlock()

	if c.apiKey == "" {
		return nil, fmt.Errorf("TMDB API Key 未配置")
	}

	u := fmt.Sprintf("https://api.themoviedb.org/3/search/multi?api_key=%s&query=%s&language=zh-CN",
		c.apiKey, url.QueryEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 区分「认证/限流失败」与「确实无结果」——
	// 原实现把 401 静默降级成“无结果”，导致无效 Key 跑了很久才被发现
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("TMDB 认证失败(401)：API Key 无效或已失效，请更新密钥")
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("TMDB 限流(429)：请降低并发或稍后重试")
	default:
		return nil, fmt.Errorf("TMDB 异常响应: HTTP %d", resp.StatusCode)
	}

	var sr struct {
		Results []TMDBResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, err
	}
	if len(sr.Results) == 0 {
		// 中文无结果时 fallback 英文搜索（真实案例："Nezha Conquers the Dragon King" 搜中文库零结果）
		if r, err := c.searchLang(ctx, name, year, "en-US"); err == nil {
			return r, nil
		}
		return nil, fmt.Errorf("TMDB 无结果: %s", name)
	}

	best := pickBest(name, year, sr.Results)
	if best == nil {
		// 中文候选均不匹配时 fallback 英文搜索
		if r, err := c.searchLang(ctx, name, year, "en-US"); err == nil {
			return r, nil
		}
		return nil, fmt.Errorf("TMDB 候选均与「%s (%s)」不匹配，拒绝套用（避免认错片）", name, year)
	}

	c.mu.Lock()
	c.cache[key] = best
	c.mu.Unlock()
	return best, nil
}

// pickBest 候选打分：年份为最强判据，其次片名相似度，最后才是 TMDB 自身排序。
//
// 为何必须校验（真实踩坑）：
//
//	搜「流浪地球」      -> [1] 流浪地球2 (2023)      [2] 流浪地球 (2019)
//	搜「Nezha Conquers」 -> [1] 新封神之哪吒闹海 (2019) [2] 哪吒闹海 (1979)
//
// 两例的正确答案都在第 2 位，盲取首条会把整座媒体库污染成错片。
func pickBest(query, year string, results []TMDBResult) *TMDBResult {
	q := normKey(query)
	var best *TMDBResult
	bestScore := -1 << 30

	for i := range results {
		r := &results[i]
		if r.MediaType != "" && r.MediaType != "movie" && r.MediaType != "tv" {
			continue // 排除 person 等非影视结果
		}
		score := 0

		// ① 年份（最强判据）
		if year != "" {
			if ry := r.Year(); ry != "" {
				switch d := absDiff(year, ry); {
				case d == 0:
					score += 1000
				case d == 1:
					score += 200 // 上映/开播跳年属常见
				case d <= 3:
					score += 20 // 年份标注误差（真实案例：倩女幽魂2 文件名 1988 / TMDB 1990）
				default:
					continue // 差 4 年以上 → 判定不是同一部，剔除
				}
			}
		}

		// ② 片名相似度
		matched := false
		for _, n := range []string{r.Title, r.Name, r.OriginalTitle, r.OriginalName} {
			if n == "" {
				continue
			}
			nk := normKey(n)
			switch {
			case nk == q:
				score += 500
				matched = true
			case strings.Contains(nk, q) || strings.Contains(q, nk):
				score += 100
				matched = true
			}
		}
		if !matched {
			continue // 片名毫不相干 → 剔除
		}

		// ③ TMDB 自身排序（最弱，仅同分决胜）
		score += 10 - i

		if score > bestScore {
			best, bestScore = r, score
		}
	}
	return best
}

func absDiff(a, b string) int {
	x, err1 := strconv.Atoi(a)
	y, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil {
		return 99
	}
	if x > y {
		return x - y
	}
	return y - x
}

// searchLang 用指定语言搜索 TMDB（供 Search 的英文 fallback 调用）
func (c *TMDBClient) searchLang(ctx context.Context, name, year, lang string) (*TMDBResult, error) {
	u := fmt.Sprintf("https://api.themoviedb.org/3/search/multi?api_key=%s&query=%s&language=%s",
		c.apiKey, url.QueryEscape(name), lang)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TMDB HTTP %d", resp.StatusCode)
	}
	var sr struct {
		Results []TMDBResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, err
	}
	if len(sr.Results) == 0 {
		return nil, fmt.Errorf("TMDB 无结果: %s", name)
	}
	return pickBest(name, year, sr.Results), nil
}

// Enrich 用 TMDB 补全字段：仅在缺失时填充，已有 {tmdb-ID} 则跳过查询
// 同时校正 Category（TMDB 的 media_type 比文件名推断更权威）
func (c *TMDBClient) Enrich(ctx context.Context, info *MediaInfo) error {
	if info.TMDBID > 0 {
		return nil // 目录名已带 {tmdb-ID}，无需查询
	}
	if info.Title == "" {
		return fmt.Errorf("片名为空，无法查询")
	}
	r, err := c.Search(ctx, info.Title, info.Year)
	if err != nil {
		return err
	}
	info.TMDBID = r.ID
	info.TMDBType = r.MediaType
	if cn := r.CNName(); cn != "" {
		info.Title = cn
	}
	if info.Year == "" {
		info.Year = r.Year()
	}
	// TMDB 说是电影，但文件名解析出集号 → 以 TMDB 为准降级为电影
	if r.MediaType == "movie" && info.EpisodeFound && info.Category != CatAnime {
		info.EpisodeFound = false
	}
	return nil
}
