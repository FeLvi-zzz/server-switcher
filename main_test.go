package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func backend(t *testing.T, name string, ln net.Listener) {
	t.Helper()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, name+" "+r.Host+" "+r.Header.Get("X-Backend")+r.Header.Get("Cookie"))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

func get(t *testing.T, url, header string) (int, string) {
	t.Helper()
	return getWithCookie(t, url, header, "")
}

func getWithCookie(t *testing.T, url, header, cookie string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if header != "" {
		req.Header.Set("X-Backend", header)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestProxy(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "sw") // unix socket パス長制限のため短いパスを使う
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("SERVER_SWITCHER_CONFIG_DIR", dir)
	cfgPath := filepath.Join(dir, "hoge.json")

	sock := filepath.Join(dir, "a.sock")
	uln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	backend(t, "A", uln)
	tln, _ := net.Listen("tcp", "127.0.0.1:0")
	backend(t, "B", tln)

	for _, args := range [][]string{
		{"add", "hoge", "a", "unix:" + sock},
		{"add", "hoge", "b", tln.Addr().String()},
		{"add", "foo", "a", tln.Addr().String()}, // 別 instance は同名でも独立
	} {
		if err := run(args); err != nil {
			t.Fatal(err)
		}
	}

	p := &Proxy{path: cfgPath, header: "X-Backend", cookie: "sw", transports: map[string]*http.Transport{}}
	ts := httptest.NewServer(p)
	defer ts.Close()
	host := ts.Listener.Addr().String()

	cases := []struct {
		header, want string
		code         int
	}{
		{"", "A " + host + " ", 200}, // 最初に add したものが default、Host は維持、header は除去
		{"b", "B " + host + " ", 200},
		{"a", "A " + host + " ", 200},
	}
	for _, c := range cases {
		code, body := get(t, ts.URL, c.header)
		if code != c.code || body != c.want {
			t.Errorf("header=%q: got %d %q, want %d %q", c.header, code, body, c.code, c.want)
		}
	}
	if code, _ := get(t, ts.URL, "nope"); code != 502 {
		t.Errorf("unknown backend: got %d, want 502", code)
	}

	// cookie でも切り替えられ、ルーティング用 cookie だけ除去される。header が優先
	cookieCases := []struct {
		header, cookie, want string
		code                 int
	}{
		{"", "sw=b", "B " + host + " ", 200},
		{"", "x=1; sw=b; y=2", "B " + host + " x=1; y=2", 200},
		{"a", "sw=b", "A " + host + " ", 200},
		{"", "other=b", "A " + host + " other=b", 200},
		{"", "sw=nope", "", 502},
	}
	for _, c := range cookieCases {
		code, body := getWithCookie(t, ts.URL, c.header, c.cookie)
		if code != c.code || (c.code == 200 && body != c.want) {
			t.Errorf("header=%q cookie=%q: got %d %q, want %d %q", c.header, c.cookie, code, body, c.code, c.want)
		}
	}

	// default 切り替えが再起動なしで反映される
	time.Sleep(10 * time.Millisecond)
	if err := run([]string{"default", "hoge", "b"}); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, ts.URL, ""); body != "B "+host+" " {
		t.Errorf("after default switch: got %q", body)
	}

	if err := run([]string{"rm", "hoge", "a"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, ts.URL, "a"); code != 502 {
		t.Errorf("removed backend: got %d, want 502", code)
	}
	if err := run([]string{"default", "hoge", "a"}); err == nil {
		t.Error("default to unknown backend should fail")
	}
	if err := run([]string{"add", "hoge", "x", "localhost"}); err == nil {
		t.Error("invalid target should fail")
	}

	// foo instance は hoge の変更の影響を受けない
	foo, err := loadConfig(filepath.Join(dir, "foo.json"))
	if err != nil {
		t.Fatal(err)
	}
	if foo.Default != "a" || len(foo.Backends) != 1 {
		t.Errorf("foo instance affected: %+v", foo)
	}
	names, _ := instances()
	if len(names) != 2 || names[0] != "foo" || names[1] != "hoge" {
		t.Errorf("instances: %v", names)
	}
	if err := run([]string{"add", "../evil", "a", "127.0.0.1:1"}); err == nil {
		t.Error("invalid instance name should fail")
	}
	if err := run([]string{"delete", "foo"}); err != nil {
		t.Fatal(err)
	}
	if names, _ := instances(); len(names) != 1 {
		t.Errorf("after delete: %v", names)
	}
}
