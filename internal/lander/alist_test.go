package lander

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// alistStub 模拟 Alist API，记录收到的调用顺序
type alistStub struct {
	calls []string
	dirs  map[string][]alistEntry
}

func newAlistStub() *alistStub {
	return &alistStub{dirs: map[string][]alistEntry{}}
}

func (s *alistStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	write := func(w http.ResponseWriter, data any) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(data)
		w.Write([]byte(`{"code":200,"message":"success","data":` + string(b) + `}`))
	}

	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]string{"token": "stub-token"})
	})
	mux.HandleFunc("/api/fs/list", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Path string `json:"path"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		write(w, map[string]any{"content": s.dirs[in.Path]})
	})
	mux.HandleFunc("/api/fs/mkdir", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Path string `json:"path"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		s.calls = append(s.calls, "mkdir "+in.Path)
		write(w, nil)
	})
	mux.HandleFunc("/api/fs/rename", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Path string `json:"path"`
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		s.calls = append(s.calls, "rename "+in.Path+" -> "+in.Name)
		write(w, nil)
	})
	mux.HandleFunc("/api/fs/move", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SrcDir string   `json:"src_dir"`
			DstDir string   `json:"dst_dir"`
			Names  []string `json:"names"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		s.calls = append(s.calls, "move "+in.SrcDir+" -> "+in.DstDir+" "+strings.Join(in.Names, ","))
		write(w, nil)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// 同目录改名只能产生一次 rename，绝不能触发 move
func TestAlist_SameDirRenameOnly(t *testing.T) {
	stub := newAlistStub()
	srv := stub.server(t)
	a := NewAlistBackend(srv.URL, "", "admin", "pw")

	if err := a.Rename(context.Background(), "/光鸭/影视/旧名.mkv", "/光鸭/影视/新名.mkv"); err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("同目录改名应只调 1 次接口，实际 %d 次: %v", len(stub.calls), stub.calls)
	}
	if stub.calls[0] != "rename /光鸭/影视/旧名.mkv -> 新名.mkv" {
		t.Errorf("调用不符: %s", stub.calls[0])
	}
}

// 跨目录必须「先在源目录改名，再移动」——顺序反了会有覆盖风险
func TestAlist_CrossDirRenamesBeforeMove(t *testing.T) {
	stub := newAlistStub()
	srv := stub.server(t)
	a := NewAlistBackend(srv.URL, "tok", "", "")

	err := a.Rename(context.Background(),
		"/光鸭/来自：分享/罪/01.mp4",
		"/光鸭/影视/动漫/罪 (2026)/Season 01/罪 - S01E01.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 2 {
		t.Fatalf("跨目录应为 2 步，实际 %d: %v", len(stub.calls), stub.calls)
	}
	if !strings.HasPrefix(stub.calls[0], "rename ") {
		t.Errorf("第一步必须是源目录内改名，实际: %s", stub.calls[0])
	}
	if !strings.HasPrefix(stub.calls[1], "move ") {
		t.Errorf("第二步必须是移动，实际: %s", stub.calls[1])
	}
	// 移动时提交的文件名必须已是规范名，否则移过去还是旧名
	if !strings.HasSuffix(stub.calls[1], "罪 - S01E01.mp4") {
		t.Errorf("move 应提交规范名，实际: %s", stub.calls[1])
	}
}

// 源与目标完全相同时不应产生任何 API 调用
func TestAlist_NoopWhenIdentical(t *testing.T) {
	stub := newAlistStub()
	srv := stub.server(t)
	a := NewAlistBackend(srv.URL, "tok", "", "")

	if err := a.Rename(context.Background(), "/光鸭/a/b.mkv", "/光鸭/a/b.mkv"); err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 0 {
		t.Errorf("同路径不应有任何调用，实际: %v", stub.calls)
	}
}

// Walk 必须递归进子目录，且返回相对路径
func TestAlist_WalkRecursesAndReturnsRelative(t *testing.T) {
	stub := newAlistStub()
	stub.dirs["/光鸭/影视"] = []alistEntry{
		{Name: "罪", IsDir: true},
		{Name: "散文件.mkv"},
	}
	stub.dirs["/光鸭/影视/罪"] = []alistEntry{
		{Name: "01.mp4"},
		{Name: "4K", IsDir: true},
		{Name: ".hidden.mp4"},
	}
	stub.dirs["/光鸭/影视/罪/4K"] = []alistEntry{{Name: "S01E07.mkv"}}

	srv := stub.server(t)
	a := NewAlistBackend(srv.URL, "tok", "", "")

	var got []string
	err := a.Walk(context.Background(), "/光鸭/影视", func(rel string) error {
		got = append(got, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"散文件.mkv": true, "罪/01.mp4": true, "罪/4K/S01E07.mkv": true,
	}
	if len(got) != len(want) {
		t.Fatalf("应遍历到 %d 个文件，实际 %d: %v", len(want), len(got), got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("意外的路径: %s（隐藏文件应被跳过，路径应相对 root）", g)
		}
	}
}

// 无凭据时必须明确报错，不能静默当成空目录
func TestAlist_NoCredentialsFailsLoudly(t *testing.T) {
	a := NewAlistBackend("http://127.0.0.1:1", "", "", "")
	err := a.Walk(context.Background(), "/光鸭", func(string) error { return nil })
	if err == nil {
		t.Fatal("无 token 也无用户名密码时必须报错")
	}
	if !strings.Contains(err.Error(), "ALIST") {
		t.Errorf("报错应提示需要设置哪些环境变量，实际: %v", err)
	}
}
