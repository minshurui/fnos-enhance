package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sub 一条订阅
type Sub struct {
	// Link 分享链接
	Link string `json:"link"`
	// Title 备注名（人看的）
	Title string `json:"title"`
	// Category 分类提示：电影/电视剧/动漫/音乐
	Category string `json:"category,omitempty"`
	// Interval 检查间隔；0 表示用全局默认
	Interval time.Duration `json:"interval,omitempty"`

	// Seen 已见过的条目指纹（fid 优先，缺失时用 名称|大小）
	Seen map[string]bool `json:"seen"`
	// LastCheck 上次检查时间
	LastCheck time.Time `json:"last_check,omitempty"`
	// LastNew 上次发现新内容的时间
	LastNew time.Time `json:"last_new,omitempty"`
	// FailCount 连续失败次数（用于退避）
	FailCount int `json:"fail_count,omitempty"`
	// Disabled 失效分享可以关掉而不删除记录
	Disabled bool `json:"disabled,omitempty"`
	// LastError 最近一次错误
	LastError string `json:"last_error,omitempty"`
}

// Store 订阅持久化。
//
// 用单个 JSON 文件而不是数据库：订阅量级是几十条，
// 用户要能直接用编辑器看和改。写入走临时文件 + rename 保证原子性。
type Store struct {
	path string
	mu   sync.Mutex
	subs map[string]*Sub // key = 规范化链接
}

// DefaultStorePath 默认订阅文件位置
func DefaultStorePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "fnos-enhance", "subs.json"), nil
}

// OpenStore 打开（或创建）订阅库
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, subs: map[string]*Sub{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil // 首次运行
		}
		return nil, err
	}
	var list []*Sub
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("订阅文件损坏 %s: %w", path, err)
	}
	for _, sub := range list {
		if sub.Seen == nil {
			sub.Seen = map[string]bool{}
		}
		s.subs[normLink(sub.Link)] = sub
	}
	return s, nil
}

// normLink 规范化链接用作主键，避免同一分享被订阅两次
func normLink(l string) string {
	l = strings.TrimSpace(l)
	l = strings.TrimPrefix(l, "https://")
	l = strings.TrimPrefix(l, "http://")
	if i := strings.IndexAny(l, "#?"); i >= 0 {
		l = l[:i]
	}
	return strings.TrimRight(l, "/")
}

// Save 原子写回磁盘
func (s *Store) Save() error {
	s.mu.Lock()
	list := make([]*Sub, 0, len(s.subs))
	for _, sub := range s.subs {
		list = append(list, sub)
	}
	s.mu.Unlock()

	sort.Slice(list, func(i, j int) bool { return list[i].Link < list[j].Link })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	// 先写临时文件再 rename：中途崩溃不会留下半个订阅表
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Add 新增订阅；已存在时只补备注不清 Seen（避免重复入库）
func (s *Store) Add(link, title, category string) (*Sub, bool) {
	key := normLink(link)
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.subs[key]; ok {
		if title != "" {
			cur.Title = title
		}
		if category != "" {
			cur.Category = category
		}
		cur.Disabled = false
		return cur, false
	}
	sub := &Sub{Link: link, Title: title, Category: category, Seen: map[string]bool{}}
	s.subs[key] = sub
	return sub, true
}

// Get 取一条订阅
func (s *Store) Get(link string) (*Sub, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[normLink(link)]
	return sub, ok
}

// List 按链接排序返回全部订阅
func (s *Store) List() []*Sub {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Sub, 0, len(s.subs))
	for _, sub := range s.subs {
		out = append(out, sub)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Link < out[j].Link })
	return out
}

// Disable 停用而不删除（用户明确要求不乱删东西）
func (s *Store) Disable(link string, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[normLink(link)]
	if !ok {
		return false
	}
	sub.Disabled = true
	sub.LastError = reason
	return true
}

// Fingerprint 条目指纹。
//
// 优先用网盘侧 ID；但有些分享每次列举返回的 ID 会变，
// 所以 ID 缺失时退化成「名称|大小」——比只用名称稳，
// 因为同名不同大小通常意味着换了版本，应当视为新内容。
func Fingerprint(id, name string, size int64) string {
	if id != "" {
		return "id:" + id
	}
	return fmt.Sprintf("ns:%s|%d", name, size)
}

// Diff 返回 entries 中尚未见过的部分（不修改 Seen）
func (sub *Sub) Diff(fps []string) []string {
	var newOnes []string
	for _, fp := range fps {
		if !sub.Seen[fp] {
			newOnes = append(newOnes, fp)
		}
	}
	return newOnes
}

// MarkSeen 把指纹标记为已见
func (sub *Sub) MarkSeen(fps []string) {
	if sub.Seen == nil {
		sub.Seen = map[string]bool{}
	}
	for _, fp := range fps {
		sub.Seen[fp] = true
	}
}

// NextDue 判断是否该检查了。
//
// 连续失败时指数退避：失效的分享不该每分钟去撞一次，
// 既浪费也容易触发风控。上限 1 小时。
func (sub *Sub) NextDue(now time.Time, defaultInterval time.Duration) bool {
	if sub.Disabled {
		return false
	}
	iv := sub.Interval
	if iv <= 0 {
		iv = defaultInterval
	}
	if sub.FailCount > 0 {
		backoff := iv << uint(min(sub.FailCount, 6))
		if backoff > time.Hour {
			backoff = time.Hour
		}
		iv = backoff
	}
	return now.Sub(sub.LastCheck) >= iv
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Lister 列举分享内容的能力（由 transfer 层提供）
type Lister interface {
	// ListShare 列举分享；返回 (id, name, size) 三元组
	ListShare(ctx context.Context, link string) ([]ShareItem, error)
}

// ShareItem 分享内的一个条目
type ShareItem struct {
	ID    string
	Name  string
	Size  int64
	IsDir bool
}

// Saver 转存新内容的能力
type Saver interface {
	// SaveNew 把指定条目转存进自己网盘；ids 为空表示整份转存
	SaveNew(ctx context.Context, link string, ids []string) (int, error)
}

// CheckResult 一次检查的结果
type CheckResult struct {
	Sub      *Sub
	NewItems []ShareItem
	Saved    int
	Err      error
	// Baseline 本次是首次检查（建立基线，不转存）
	Baseline bool
	// BaselineCount 基线记录的条目数
	BaselineCount int
}

// Check 检查单条订阅，发现新内容则转存。
//
// dryRun 时只报告不转存、也不更新 Seen —— 否则一次预演就会把
// 新内容标记成已见，真正跑的时候反而不转存了。
func Check(ctx context.Context, sub *Sub, l Lister, sv Saver, dryRun bool) CheckResult {
	res := CheckResult{Sub: sub}
	now := time.Now()

	items, err := l.ListShare(ctx, sub.Link)
	if err != nil {
		// dry-run 不写状态：预演不该让订阅进入退避、也不该推后下次检查时间
		if !dryRun {
			sub.FailCount++
			sub.LastError = err.Error()
			sub.LastCheck = now
		}
		res.Err = err
		return res
	}

	// 首次订阅：把现有内容全部标记为已见，只追后续更新。
	// 否则一订阅就把整个历史片库拖回来，不是用户想要的「秒存更新」。
	first := len(sub.Seen) == 0

	var fps []string
	fpToItem := map[string]ShareItem{}
	for _, it := range items {
		if it.IsDir {
			continue
		}
		fp := Fingerprint(it.ID, it.Name, it.Size)
		fps = append(fps, fp)
		fpToItem[fp] = it
	}

	newFps := sub.Diff(fps)
	for _, fp := range newFps {
		res.NewItems = append(res.NewItems, fpToItem[fp])
	}

	if !dryRun {
		sub.FailCount = 0
		sub.LastError = ""
		sub.LastCheck = now
	}

	if first {
		// 建立基线，不转存。
		// dry-run 不写基线（保持预演无副作用），但要如实告知，
		// 否则用户会一直看到"无更新"而不知道基线还没建。
		if !dryRun {
			sub.MarkSeen(fps)
		}
		res.NewItems = nil
		res.Baseline = true
		res.BaselineCount = len(fps)
		return res
	}
	if len(newFps) == 0 {
		return res
	}
	if dryRun {
		return res // 预演不转存、不标记、不改时间戳
	}
	sub.LastNew = now

	var ids []string
	for _, it := range res.NewItems {
		if it.ID != "" {
			ids = append(ids, it.ID)
		}
	}
	n, err := sv.SaveNew(ctx, sub.Link, ids)
	res.Saved = n
	if err != nil {
		res.Err = err
		// 转存失败不标记已见，下次重试
		return res
	}
	sub.MarkSeen(newFps)
	return res
}
