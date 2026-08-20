package lander

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type gyStub struct {
	calls    []string
	tree     map[string][]gyItem // parentID -> children
	failAuth int                 // 前 N 次 API 返回 401
	authHits int
}

func (s *gyStub) srv(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	ok := func(w http.ResponseWriter, data string) {
		w.Header().Set("Content-Type", "application/json")
		if data == "" {
			data = "{}"
		}
		w.Write([]byte(`{"code":0,"msg":"success","data":` + data + `}`))
	}

	mux.HandleFunc("/v1/auth/token", func(w http.ResponseWriter, r *http.Request) {
		s.calls = append(s.calls, "refresh")
		w.Write([]byte(`{"access_token":"NEW_AT","refresh_token":"NEW_RT"}`))
	})

	mux.HandleFunc("/userres/v1/file/get_file_list", func(w http.ResponseWriter, r *http.Request) {
		if s.authHits < s.failAuth {
			s.authHits++
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		var in struct{ ParentId string }
		json.NewDecoder(r.Body).Decode(&in)
		b, _ := json.Marshal(s.tree[in.ParentId])
		if s.tree[in.ParentId] == nil {
			b = []byte("[]")
		}
		ok(w, `{"total":`+itoaN(len(s.tree[in.ParentId]))+`,"list":`+string(b)+`}`)
	})

	mux.HandleFunc("/nd.bizuserres.s/v1/file/create_dir", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ ParentId, DirName string }
		json.NewDecoder(r.Body).Decode(&in)
		s.calls = append(s.calls, "create_dir "+in.DirName)
		id := "new-" + in.DirName
		s.tree[in.ParentId] = append(s.tree[in.ParentId], gyItem{FileID: id, FileName: in.DirName, ResType: 2})
		ok(w, `{"fileId":"`+id+`"}`)
	})

	mux.HandleFunc("/nd.bizuserres.s/v1/file/rename", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ FileId, NewName string }
		json.NewDecoder(r.Body).Decode(&in)
		s.calls = append(s.calls, "rename "+in.FileId+" -> "+in.NewName)
		ok(w, "")
	})

	mux.HandleFunc("/nd.bizuserres.s/v1/file/move_file", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			FileIds  []string
			ParentId string
		}
		json.NewDecoder(r.Body).Decode(&in)
		s.calls = append(s.calls, "move "+strings.Join(in.FileIds, ",")+" -> "+in.ParentId)
		ok(w, `{"taskId":"T1"}`)
	})

	mux.HandleFunc("/nd.bizuserres.s/v1/get_task_status", func(w http.ResponseWriter, r *http.Request) {
		s.calls = append(s.calls, "task_status")
		ok(w, `{"status":2}`)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func itoaN(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func newGY(t *testing.T, s *gyStub) *GuangYaBackend {
	t.Helper()
	srv := s.srv(t)
	g := NewGuangYaBackend("AT", "RT", "cid")
	g.APIBase = srv.URL
	g.AccountBase = srv.URL
	return g
}

// 根目录的 parentId 必须是空字符串，不是 "0" 或 "/"
func TestGY_RootIsEmptyString(t *testing.T) {
	s := &gyStub{tree: map[string][]gyItem{
		"": {{FileID: "d1", FileName: "影视", ResType: 2}},
	}}
	g := newGY(t, s)
	id, ok, err := g.resolveID(context.Background(), "/影视")
	if err != nil || !ok {
		t.Fatalf("解析失败: %v ok=%v", err, ok)
	}
	if id != "d1" {
		t.Errorf("应解析到 d1，实际 %s", id)
	}
}

// 401 必须自动刷新令牌并重试，且把新令牌回调出去持久化
func TestGY_AutoRefreshOn401(t *testing.T) {
	s := &gyStub{tree: map[string][]gyItem{"": {}}, failAuth: 1}
	g := newGY(t, s)
	var gotA, gotR string
	g.TokenSink = func(a, r string) { gotA, gotR = a, r }

	if _, err := g.listDir(context.Background(), "", true); err != nil {
		t.Fatalf("应自动刷新后成功，实际: %v", err)
	}
	if gotA != "NEW_AT" || gotR != "NEW_RT" {
		t.Errorf("新令牌应回调出去持久化，实际 access=%q refresh=%q", gotA, gotR)
	}
	if g.AccessToken != "NEW_AT" {
		t.Errorf("内存里的令牌应更新，实际 %s", g.AccessToken)
	}
}

// 跨目录必须先改名后移动，且移动后要等异步任务完成
func TestGY_CrossDirRenameThenMoveThenWait(t *testing.T) {
	s := &gyStub{tree: map[string][]gyItem{
		"":    {{FileID: "src", FileName: "来源", ResType: 2}, {FileID: "dst", FileName: "影视", ResType: 2}},
		"src": {{FileID: "f1", FileName: "01.mp4"}},
		"dst": {},
	}}
	g := newGY(t, s)
	err := g.Rename(context.Background(), "/来源/01.mp4", "/影视/罪 - S01E01.mp4")
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, c := range s.calls {
		if strings.HasPrefix(c, "rename ") || strings.HasPrefix(c, "move ") || c == "task_status" {
			seq = append(seq, strings.Split(c, " ")[0])
		}
	}
	want := []string{"rename", "move", "task_status"}
	if len(seq) != 3 || seq[0] != want[0] || seq[1] != want[1] || seq[2] != want[2] {
		t.Errorf("顺序应为 改名→移动→等任务，实际: %v", seq)
	}
}

// 同目录改名不应触发 move
func TestGY_SameDirNoMove(t *testing.T) {
	s := &gyStub{tree: map[string][]gyItem{
		"":  {{FileID: "d", FileName: "影视", ResType: 2}},
		"d": {{FileID: "f1", FileName: "旧.mp4"}},
	}}
	g := newGY(t, s)
	if err := g.Rename(context.Background(), "/影视/旧.mp4", "/影视/新.mp4"); err != nil {
		t.Fatal(err)
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "move ") {
			t.Errorf("同目录不应移动，实际调用: %v", s.calls)
		}
	}
}

// MkdirAll 对已存在目录必须幂等，不重复创建
func TestGY_MkdirIdempotent(t *testing.T) {
	s := &gyStub{tree: map[string][]gyItem{
		"":  {{FileID: "a", FileName: "影视", ResType: 2}},
		"a": {{FileID: "b", FileName: "动漫", ResType: 2}},
	}}
	g := newGY(t, s)
	if err := g.MkdirAll(context.Background(), "/影视/动漫"); err != nil {
		t.Fatal(err)
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "create_dir") {
			t.Errorf("已存在的目录不应重建，实际: %v", s.calls)
		}
	}
	// 只缺最后一层时，只建最后一层
	s.calls = nil
	if err := g.MkdirAll(context.Background(), "/影视/动漫/新剧 (2026)"); err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 1 || !strings.Contains(s.calls[0], "新剧 (2026)") {
		t.Errorf("应只建缺失的一层，实际: %v", s.calls)
	}
}

// HTTP 200 但 msg != success 必须判失败（业务码在 body 里）
func TestGY_BusinessErrorDespiteHTTP200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":40001,"msg":"file not found","data":{}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	g := NewGuangYaBackend("AT", "RT", "cid")
	g.APIBase = srv.URL
	err := g.post(context.Background(), "/nd.bizuserres.s/v1/file/rename", map[string]any{}, nil)
	if err == nil {
		t.Fatal("msg!=success 必须报错，不能因 HTTP 200 就当成功")
	}
	if !strings.Contains(err.Error(), "40001") {
		t.Errorf("错误应带业务码，实际: %v", err)
	}
}

// 空 access_token 必须明确报错
func TestGY_EmptyTokenFailsLoudly(t *testing.T) {
	g := NewGuangYaBackend("", "", "cid")
	err := g.post(context.Background(), "/x", map[string]any{}, nil)
	if err == nil || !strings.Contains(err.Error(), "access_token") {
		t.Errorf("应提示 access_token 为空，实际: %v", err)
	}
}

// 被限流时接口返回「成功但没有 list」，必须重试/报错，绝不能当成空目录
// —— 实测这个 bug 让 4 个目录 6 个文件被静默跳过
func TestGY_ThrottledEmptyListNotTreatedAsEmptyDir(t *testing.T) {
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/userres/v1/file/get_file_list", func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			// 第一次模拟被限流：msg=success、total>0，但没有 list 字段
			w.Write([]byte(`{"code":0,"msg":"success","data":{"total":3}}`))
			return
		}
		w.Write([]byte(`{"code":0,"msg":"success","data":{"total":3,"list":[
			{"fileId":"a","fileName":"1.mkv","resType":1},
			{"fileId":"b","fileName":"2.mkv","resType":1},
			{"fileId":"c","fileName":"3.mkv","resType":1}]}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	g := NewGuangYaBackend("AT", "RT", "cid")
	g.APIBase = srv.URL
	g.rl = newRateLimiter(0) // 测试里不真等
	items, err := g.listDir(context.Background(), "X", true)
	if err != nil {
		t.Fatalf("应重试后成功，实际: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("应拿到 3 个文件（重试后），实际 %d —— 这正是静默丢数据的 bug", len(items))
	}
	if hits < 2 {
		t.Error("应发生过重试")
	}
}

// 反复被限流时必须报错，不能返回空列表让调用方以为目录是空的
func TestGY_PersistentThrottleFailsLoudly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/userres/v1/file/get_file_list", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"success","data":{"total":5}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	g := NewGuangYaBackend("AT", "RT", "cid")
	g.APIBase = srv.URL
	g.rl = newRateLimiter(0)
	_, err := g.listDir(context.Background(), "X", true)
	if err == nil {
		t.Fatal("持续拿不到 list 必须报错，否则会静默少改名")
	}
	if !strings.Contains(err.Error(), "限流") {
		t.Errorf("错误应说明疑似限流，实际: %v", err)
	}
}

// 真正的空目录（total=0）不应报错
func TestGY_GenuinelyEmptyDirIsOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/userres/v1/file/get_file_list", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"success","data":{"total":0}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	g := NewGuangYaBackend("AT", "RT", "cid")
	g.APIBase = srv.URL
	g.rl = newRateLimiter(0)
	items, err := g.listDir(context.Background(), "X", true)
	if err != nil {
		t.Fatalf("total=0 的空目录应正常返回，实际: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("应为空，实际 %d", len(items))
	}
}
