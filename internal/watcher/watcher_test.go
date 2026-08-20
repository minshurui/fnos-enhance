package watcher

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type fakeLister struct {
	items []ShareItem
	err   error
	calls int
}

func (f *fakeLister) ListShare(ctx context.Context, link string) ([]ShareItem, error) {
	f.calls++
	return f.items, f.err
}

type fakeSaver struct {
	gotIDs []string
	calls  int
	err    error
	n      int
}

func (f *fakeSaver) SaveNew(ctx context.Context, link string, ids []string) (int, error) {
	f.calls++
	f.gotIDs = ids
	return f.n, f.err
}

func item(id, name string, size int64) ShareItem {
	return ShareItem{ID: id, Name: name, Size: size}
}

// 首次订阅必须只建基线，不能把整个历史片库拖回来
func TestFirstSubscribeOnlyBaselines(t *testing.T) {
	sub := &Sub{Link: "x", Seen: map[string]bool{}}
	l := &fakeLister{items: []ShareItem{item("1", "E01.mkv", 100), item("2", "E02.mkv", 100)}}
	sv := &fakeSaver{n: 2}

	res := Check(context.Background(), sub, l, sv, false)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if sv.calls != 0 {
		t.Errorf("首次订阅不应转存（会把整个历史片库拖回来），实际调用 %d 次", sv.calls)
	}
	if len(res.NewItems) != 0 {
		t.Errorf("首次订阅不应报新增，实际 %d", len(res.NewItems))
	}
	if len(sub.Seen) != 2 {
		t.Errorf("应建立 2 条基线，实际 %d", len(sub.Seen))
	}
}

// 第二次出现新集才转存，且只转新的那一集
func TestOnlyNewEpisodesAreSaved(t *testing.T) {
	sub := &Sub{Link: "x", Seen: map[string]bool{}}
	l := &fakeLister{items: []ShareItem{item("1", "E01.mkv", 100)}}
	sv := &fakeSaver{n: 1}
	Check(context.Background(), sub, l, sv, false) // 建基线

	l.items = append(l.items, item("2", "E02.mkv", 100))
	res := Check(context.Background(), sub, l, sv, false)
	if len(res.NewItems) != 1 || res.NewItems[0].Name != "E02.mkv" {
		t.Fatalf("应只发现 E02，实际 %+v", res.NewItems)
	}
	if len(sv.gotIDs) != 1 || sv.gotIDs[0] != "2" {
		t.Errorf("应只转存新条目 id=2，实际 %v", sv.gotIDs)
	}
}

// dry-run 绝不能修改 Seen —— 否则预演一次，真跑时就不转存了
func TestDryRunDoesNotPolluteState(t *testing.T) {
	sub := &Sub{Link: "x", Seen: map[string]bool{"id:1": true}}
	l := &fakeLister{items: []ShareItem{item("1", "E01.mkv", 100), item("2", "E02.mkv", 100)}}
	sv := &fakeSaver{n: 1}

	res := Check(context.Background(), sub, l, sv, true)
	if len(res.NewItems) != 1 {
		t.Fatalf("应报告 1 个新增，实际 %d", len(res.NewItems))
	}
	if sv.calls != 0 {
		t.Error("dry-run 不应转存")
	}
	if sub.Seen["id:2"] {
		t.Error("dry-run 竟然标记了已见 —— 真跑时就不会转存了")
	}
}

// 转存失败不能标记已见，否则这一集永远丢了
func TestSaveFailureKeepsItemUnseen(t *testing.T) {
	sub := &Sub{Link: "x", Seen: map[string]bool{"id:1": true}}
	l := &fakeLister{items: []ShareItem{item("1", "E01.mkv", 100), item("2", "E02.mkv", 100)}}
	sv := &fakeSaver{err: errors.New("网盘空间已满")}

	res := Check(context.Background(), sub, l, sv, false)
	if res.Err == nil {
		t.Fatal("应返回转存错误")
	}
	if sub.Seen["id:2"] {
		t.Error("转存失败却标记已见 —— 这一集会永久丢失")
	}
}

// 同名但大小变了应视为新版本（换压制/换清晰度）
func TestSameNameDifferentSizeIsNew(t *testing.T) {
	a := Fingerprint("", "E01.mkv", 100)
	b := Fingerprint("", "E01.mkv", 200)
	if a == b {
		t.Error("同名不同大小应产生不同指纹（通常是换了版本）")
	}
}

// 列举失败要累计失败次数并退避，不能每分钟撞一次失效分享
func TestFailureBackoff(t *testing.T) {
	sub := &Sub{Link: "x", Seen: map[string]bool{"a": true}}
	l := &fakeLister{err: errors.New("分享已失效")}
	sv := &fakeSaver{}

	for i := 0; i < 3; i++ {
		Check(context.Background(), sub, l, sv, false)
	}
	if sub.FailCount != 3 {
		t.Errorf("应累计 3 次失败，实际 %d", sub.FailCount)
	}
	// 失败 3 次后间隔应被放大
	sub.LastCheck = time.Now().Add(-2 * time.Minute)
	if sub.NextDue(time.Now(), time.Minute) {
		t.Error("连续失败后应退避，不该立即重试")
	}
}

// 成功一次后失败计数必须清零
func TestSuccessResetsFailCount(t *testing.T) {
	sub := &Sub{Link: "x", Seen: map[string]bool{"a": true}, FailCount: 4}
	l := &fakeLister{items: []ShareItem{item("1", "E01.mkv", 1)}}
	Check(context.Background(), sub, l, &fakeSaver{n: 1}, false)
	if sub.FailCount != 0 {
		t.Errorf("成功后应清零，实际 %d", sub.FailCount)
	}
}

// 停用的订阅不参与检查，但记录保留（不删除）
func TestDisabledSubIsSkippedNotDeleted(t *testing.T) {
	sub := &Sub{Link: "x", Disabled: true}
	if sub.NextDue(time.Now().Add(time.Hour), time.Minute) {
		t.Error("停用的订阅不该被检查")
	}
}

// 同一分享的不同写法（带 # 锚点、带斜杠、带 http）应视为同一条
func TestLinkNormalizationPreventsDuplicates(t *testing.T) {
	s := &Store{subs: map[string]*Sub{}}
	s.Add("https://pan.quark.cn/s/abc123#/list/share", "剧A", "动漫")
	_, created := s.Add("http://pan.quark.cn/s/abc123/", "", "")
	if created {
		t.Error("同一分享的不同写法被当成两条订阅了")
	}
	if len(s.List()) != 1 {
		t.Errorf("应只有 1 条订阅，实际 %d", len(s.List()))
	}
}

// 重复订阅不应清空 Seen（否则会把已入库的重新转存一遍）
func TestReAddKeepsSeen(t *testing.T) {
	s := &Store{subs: map[string]*Sub{}}
	sub, _ := s.Add("https://pan.quark.cn/s/abc", "剧A", "动漫")
	sub.MarkSeen([]string{"id:1", "id:2"})

	again, created := s.Add("https://pan.quark.cn/s/abc", "剧A改名", "")
	if created {
		t.Error("不应新建")
	}
	if len(again.Seen) != 2 {
		t.Errorf("重复订阅不应清空已见记录，实际 %d", len(again.Seen))
	}
	if again.Title != "剧A改名" {
		t.Error("备注应可更新")
	}
}

// 存盘再读回必须完整保留状态
func TestStoreRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "subs.json")
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := s.Add("https://pan.quark.cn/s/xyz", "完美世界", "动漫")
	sub.MarkSeen([]string{"id:9"})
	sub.Interval = 5 * time.Minute
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get("https://pan.quark.cn/s/xyz#/list/share")
	if !ok {
		t.Fatal("读回失败（链接规范化应能匹配）")
	}
	if got.Title != "完美世界" || !got.Seen["id:9"] || got.Interval != 5*time.Minute {
		t.Errorf("状态未完整保留: %+v", got)
	}
}

// 首次运行时订阅文件不存在，应视为空库而不是报错
func TestOpenStoreMissingFileIsEmpty(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("文件不存在应视为空库，实际报错: %v", err)
	}
	if len(s.List()) != 0 {
		t.Error("应为空")
	}
}

// dry-run 必须完全无副作用：不改 LastCheck、不进退避
// 否则预演一次就要等一个周期才能真跑
func TestDryRunMutatesNothing(t *testing.T) {
	sub := &Sub{Link: "x", Seen: map[string]bool{"id:1": true}}
	l := &fakeLister{items: []ShareItem{item("1", "E01.mkv", 1), item("2", "E02.mkv", 1)}}

	Check(context.Background(), sub, l, &fakeSaver{}, true)
	if !sub.LastCheck.IsZero() {
		t.Error("dry-run 不该更新 LastCheck（会让真跑被推迟一个周期）")
	}
	if !sub.LastNew.IsZero() {
		t.Error("dry-run 不该更新 LastNew")
	}

	// 失败时也不该进退避
	sub2 := &Sub{Link: "y", Seen: map[string]bool{"a": true}}
	Check(context.Background(), sub2, &fakeLister{err: errors.New("boom")}, &fakeSaver{}, true)
	if sub2.FailCount != 0 {
		t.Errorf("dry-run 失败不该累计退避计数，实际 %d", sub2.FailCount)
	}
}
