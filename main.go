// server-switcher: unix domain socket / host:port で公開されている複数サーバーを束ねるローカル用リバースプロキシ。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const usage = `Usage: server-switcher <command> <instance> [args]

instance ごとに独立したリバースプロキシ（接続先・default・listen アドレス）を持てる。

Commands:
  serve <instance> [-listen ADDR] [-header NAME] [-cookie NAME]
                                    リバースプロキシを起動。-listen / -header / -cookie を指定すると
                                    instance の設定に保存され、次回以降は省略可
                                    (未設定時: 127.0.0.1:8080, X-Backend, X-Backend)
                                    接続先の選択は header > cookie > default の優先順
  ui [-listen ADDR] [-domain DOMAIN]
                                    全 instance の routing cookie をセットして接続先を切り替える画面を出す
                                    簡易サーバーを起動 (default: 127.0.0.1:8090)。
                                    cookie は port を区別しないため、proxy と同じ host で開けば proxy にも効く。
                                    -domain を指定すると cookie の Domain 属性に使う (例: example.test でサブドメイン共有)
  add <instance> <name> <target>    接続先を登録 (target: unix:/path/to.sock | host:port | http://host:port)
  rm <instance> <name>              接続先を解除
  default <instance> <name>         default 接続先を切り替え
  list [instance]                   接続先を表示 (instance 省略時は全 instance)
  delete <instance>                 instance の設定を削除

Config: $SERVER_SWITCHER_CONFIG_DIR/<instance>.json (default dir: ~/.config/server-switcher)
`

const (
	defaultListen   = "127.0.0.1:8080"
	defaultHeader   = "X-Backend"
	defaultCookie   = "X-Backend"
	defaultUIListen = "127.0.0.1:8090"
)

type Config struct {
	Listen   string            `json:"listen,omitempty"`
	Header   string            `json:"header,omitempty"`
	Cookie   string            `json:"cookie,omitempty"`
	Default  string            `json:"default"`
	Backends map[string]string `json:"backends"`
}

func (c *Config) listen() string {
	if c.Listen != "" {
		return c.Listen
	}
	return defaultListen
}

func (c *Config) header() string {
	if c.Header != "" {
		return c.Header
	}
	return defaultHeader
}

func (c *Config) cookie() string {
	if c.Cookie != "" {
		return c.Cookie
	}
	return defaultCookie
}

func configDir() string {
	if d := os.Getenv("SERVER_SWITCHER_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return filepath.Join(home, ".config", "server-switcher")
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return s[0] != '.'
}

func configPath(instance string) (string, error) {
	if !validName(instance) {
		return "", fmt.Errorf("invalid instance name %q (allowed: [A-Za-z0-9._-])", instance)
	}
	return filepath.Join(configDir(), instance+".json"), nil
}

func instances() ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(configDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(p), ".json"))
	}
	sort.Strings(names)
	return names, nil
}

// listenAddr は "unix:/path" または "host:port" を (network, addr) に分解する。
func listenAddr(s string) (string, string) {
	if strings.HasPrefix(s, "unix:") {
		return "unix", strings.TrimPrefix(s, "unix:")
	}
	return "tcp", s
}

// running は listen アドレスに既に何かが待ち受けているかを返す。
func running(listen string) bool {
	network, addr := listenAddr(listen)
	c, err := net.DialTimeout(network, addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{Backends: map[string]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Backends == nil {
		cfg.Backends = map[string]string{}
	}
	return cfg, nil
}

// saveConfig は一時ファイル経由で atomic に書き込む（serve 中のプロキシが中途半端な内容を読まないように）。
func saveConfig(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// target をパースして (network, address) を返す。
func parseTarget(t string) (network, addr string, err error) {
	switch {
	case strings.HasPrefix(t, "unix:"):
		addr = strings.TrimPrefix(strings.TrimPrefix(t, "unix:"), "//")
		if addr == "" {
			return "", "", fmt.Errorf("empty socket path: %q", t)
		}
		return "unix", addr, nil
	case strings.HasPrefix(t, "http://"):
		addr = strings.TrimSuffix(strings.TrimPrefix(t, "http://"), "/")
	default:
		addr = t
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return "", "", fmt.Errorf("invalid target %q: %w", t, err)
	}
	return "tcp", addr, nil
}

// ---- proxy ----

type ctxKey struct{}

type route struct {
	name, network, addr string
}

type Proxy struct {
	path   string
	header string
	cookie string

	mu      sync.Mutex
	cfg     *Config
	modTime time.Time

	tmu        sync.Mutex
	transports map[string]*http.Transport // key: network + "|" + addr
}

// current は設定ファイルが更新されていれば再読み込みして返す。
func (p *Proxy) current() (*Config, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var mt time.Time
	if st, err := os.Stat(p.path); err == nil {
		mt = st.ModTime()
	}
	if p.cfg == nil || !mt.Equal(p.modTime) {
		cfg, err := loadConfig(p.path)
		if err != nil {
			if p.cfg != nil {
				log.Printf("config reload failed, keep previous: %v", err)
				return p.cfg, nil
			}
			return nil, err
		}
		p.cfg, p.modTime = cfg, mt
	}
	return p.cfg, nil
}

func (p *Proxy) transport(network, addr string) *http.Transport {
	key := network + "|" + addr
	p.tmu.Lock()
	defer p.tmu.Unlock()
	if t, ok := p.transports[key]; ok {
		return t
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	t := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, addr)
		},
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	p.transports[key] = t
	return t
}

// RoundTrip は Rewrite で context に積んだ route に応じた Transport に振り分ける。
func (p *Proxy) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Context().Value(ctxKey{}).(route)
	return p.transport(r.network, r.addr).RoundTrip(req)
}

func (p *Proxy) resolve(req *http.Request) (route, int, error) {
	cfg, err := p.current()
	if err != nil {
		return route{}, http.StatusInternalServerError, err
	}
	name := req.Header.Get(p.header)
	if name == "" && p.cookie != "" {
		if c, err := req.Cookie(p.cookie); err == nil {
			name = c.Value
		}
	}
	if name == "" {
		name = cfg.Default
		if name == "" {
			return route{}, http.StatusBadGateway, errors.New("no default backend configured")
		}
	}
	target, ok := cfg.Backends[name]
	if !ok {
		return route{}, http.StatusBadGateway, fmt.Errorf("unknown backend %q", name)
	}
	network, addr, err := parseTarget(target)
	if err != nil {
		return route{}, http.StatusBadGateway, err
	}
	return route{name: name, network: network, addr: addr}, 0, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	rt, code, err := p.resolve(req)
	if err != nil {
		log.Printf("%s %s -> error: %v", req.Method, req.URL.RequestURI(), err)
		http.Error(w, "server-switcher: "+err.Error(), code)
		return
	}
	log.Printf("%s %s -> %s (%s:%s)", req.Method, req.URL.RequestURI(), rt.name, rt.network, rt.addr)

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Host ヘッダは元のまま残す（アプリ側のホスト判定を壊さないため）
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = rt.name // 実際の接続先は Transport が決める
			if rt.network == "tcp" {
				pr.Out.URL.Host = rt.addr
			}
			pr.Out.Header.Del(p.header)
			stripCookie(pr.Out, p.cookie)
			pr.SetXForwarded()
			pr.Out = pr.Out.WithContext(context.WithValue(pr.Out.Context(), ctxKey{}, rt))
		},
		Transport: p,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("backend %s error: %v", rt.name, err)
			http.Error(w, fmt.Sprintf("server-switcher: backend %q: %v", rt.name, err), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, req)
}

// stripCookie はルーティング用 cookie を除いて Cookie ヘッダを組み直す。
func stripCookie(req *http.Request, name string) {
	if name == "" || req.Header.Get("Cookie") == "" {
		return
	}
	cookies := req.Cookies()
	req.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != name {
			req.AddCookie(c)
		}
	}
}

// ---- cookie switcher UI ----

var uiTmpl = template.Must(template.New("ui").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>server-switcher</title>
<style>
body{font-family:system-ui,sans-serif;max-width:48em;margin:2em auto;padding:0 1em}
table{border-collapse:collapse;width:100%}
td{padding:.4em .6em;border-bottom:1px solid #ddd}
tr.active{background:#e6f4ea;font-weight:bold}
code{color:#555}
.err{color:#c00}
h2 small{font-weight:normal;color:#555}
</style></head><body>
<h1>server-switcher</h1>
{{range .Instances}}<section id="{{.Name}}">
<h2>{{.Name}} <small>{{.Listen}}{{if .Running}} (running){{else}} (stopped){{end}}</small></h2>
{{if .Error}}<p class="err">{{.Error}}</p>{{else}}
<p>cookie <code>{{.Cookie}}</code> = {{if .Current}}<b>{{.Current}}</b>{{else}}(未設定: default の {{if .Default}}<b>{{.Default}}</b>{{else}}<span class="err">未設定</span>{{end}} を使用){{end}}
{{if .Unknown}}<span class="err">(この instance には未登録の接続先です)</span>{{end}}</p>
{{if .SharedWith}}<p class="err">cookie 名が {{range $i, $n := .SharedWith}}{{if $i}}, {{end}}{{$n}}{{end}} と共通のため、切り替えるとそちらにも効きます（別名にするには <code>serve {{.Name}} -cookie NAME</code>）</p>{{end}}
<form method="post">
<input type="hidden" name="instance" value="{{.Name}}">
<table>
{{range .Backends}}<tr{{if .Active}} class="active"{{end}}>
<td>{{.Name}}{{if .IsDefault}} <small>(default)</small>{{end}}</td>
<td><code>{{.Target}}</code></td>
<td><button name="backend" value="{{.Name}}"{{if .Active}} disabled{{end}}>切り替え</button></td>
</tr>{{else}}<tr><td>(接続先が登録されていません)</td></tr>{{end}}
</table>
<p><button name="backend" value=""{{if not .Current}} disabled{{end}}>cookie を削除 (default に戻す)</button></p>
</form>{{end}}
</section>{{else}}<p>(no instances)</p>{{end}}
</body></html>
`))

type uiBackend struct {
	Name, Target      string
	Active, IsDefault bool
}

type uiInstance struct {
	Name, Listen, Cookie, Current, Default, Error string
	Running, Unknown                              bool
	SharedWith                                    []string
	Backends                                      []uiBackend
}

func buildUIInstance(r *http.Request, name string, cfg *Config) uiInstance {
	in := uiInstance{Name: name, Listen: cfg.listen(), Cookie: cfg.cookie(), Default: cfg.Default, Running: running(cfg.listen())}
	if c, err := r.Cookie(in.Cookie); err == nil {
		in.Current = c.Value
		_, ok := cfg.Backends[c.Value]
		in.Unknown = !ok
	}
	effective := in.Current
	if effective == "" {
		effective = cfg.Default
	}
	names := make([]string, 0, len(cfg.Backends))
	for n := range cfg.Backends {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		in.Backends = append(in.Backends, uiBackend{
			Name: n, Target: cfg.Backends[n], Active: n == effective, IsDefault: n == cfg.Default,
		})
	}
	return in
}

// uiHandler は全 instance の routing cookie を付け外しする画面を返す。
// cookie は port を区別しないので、同じ host で proxy にアクセスすればそのまま効く。
func uiHandler(domain string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		case http.MethodPost:
			instance, b := r.PostFormValue("instance"), r.PostFormValue("backend")
			path, err := configPath(instance)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if _, err := os.Stat(path); err != nil {
				http.Error(w, fmt.Sprintf("unknown instance %q", instance), http.StatusBadRequest)
				return
			}
			cfg, err := loadConfig(path)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			c := &http.Cookie{Name: cfg.cookie(), Path: "/", Domain: domain, SameSite: http.SameSiteLaxMode}
			if b == "" {
				c.MaxAge = -1
			} else {
				if _, ok := cfg.Backends[b]; !ok {
					http.Error(w, fmt.Sprintf("unknown backend %q in instance %q", b, instance), http.StatusBadRequest)
					return
				}
				c.Value = b
				c.MaxAge = 365 * 24 * 60 * 60
			}
			http.SetCookie(w, c)
			log.Printf("%s: cookie %s=%q", instance, c.Name, b)
			http.Redirect(w, r, "/#"+instance, http.StatusSeeOther)
			return
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		names, err := instances()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var page struct{ Instances []uiInstance }
		byCookie := map[string][]string{}
		for _, n := range names {
			path, _ := configPath(n)
			cfg, err := loadConfig(path)
			if err != nil {
				page.Instances = append(page.Instances, uiInstance{Name: n, Error: err.Error()})
				continue
			}
			in := buildUIInstance(r, n, cfg)
			byCookie[in.Cookie] = append(byCookie[in.Cookie], n)
			page.Instances = append(page.Instances, in)
		}
		for i := range page.Instances {
			in := &page.Instances[i]
			for _, n := range byCookie[in.Cookie] {
				if n != in.Name && in.Error == "" {
					in.SharedWith = append(in.SharedWith, n)
				}
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := uiTmpl.Execute(w, page); err != nil {
			log.Printf("render: %v", err)
		}
	})
}

func cmdUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	listen := fs.String("listen", defaultUIListen, "listen address (host:port)")
	domain := fs.String("domain", "", "Domain attribute of the cookie (default: host only)")
	fs.Parse(args)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("%w (change it with `ui -listen ADDR`)", err)
	}
	log.SetPrefix("[ui] ")
	log.Printf("open http://%s/ (config dir: %s)", *listen, configDir())
	return http.Serve(ln, uiHandler(*domain))
}

// ---- commands ----

func cmdServe(instance string, args []string) error {
	path, err := configPath(instance)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "", "listen address (host:port or unix:/path/to.sock); saved to instance config")
	header := fs.String("header", "", "routing header name; saved to instance config")
	cookie := fs.String("cookie", "", "routing cookie name; saved to instance config")
	fs.Parse(args)

	// 明示指定されたものは instance の設定に保存して次回以降も使う
	if *listen != "" || *header != "" || *cookie != "" {
		if err := mutate(instance, func(c *Config) error {
			if *listen != "" {
				c.Listen = *listen
			}
			if *header != "" {
				c.Header = *header
			}
			if *cookie != "" {
				c.Cookie = *cookie
			}
			return nil
		}); err != nil {
			return err
		}
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}

	p := &Proxy{path: path, header: cfg.header(), cookie: cfg.cookie(), transports: map[string]*http.Transport{}}
	if _, err := p.current(); err != nil {
		return err
	}

	network, addr := listenAddr(cfg.listen())
	if network == "unix" {
		if running(cfg.listen()) {
			return fmt.Errorf("%s is already in use (instance %q already running?)", cfg.listen(), instance)
		}
		os.Remove(addr)
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return fmt.Errorf("%w (another instance uses this address? change it with `serve %s -listen ADDR`)", err, instance)
	}
	log.SetPrefix("[" + instance + "] ")
	log.Printf("listening on %s (header: %s, cookie: %s, config: %s)", cfg.listen(), p.header, p.cookie, path)
	return http.Serve(ln, p)
}

func mutate(instance string, fn func(*Config) error) error {
	path, err := configPath(instance)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	if err := fn(cfg); err != nil {
		return err
	}
	return saveConfig(path, cfg)
}

func printInstance(instance string) error {
	path, err := configPath(instance)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("unknown instance %q", instance)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	state := "stopped"
	if running(cfg.listen()) {
		state = "running"
	}
	fmt.Printf("%s  listen=%s header=%s cookie=%s [%s]\n", instance, cfg.listen(), cfg.header(), cfg.cookie(), state)
	if len(cfg.Backends) == 0 {
		fmt.Println("    (no backends)")
		return nil
	}
	names := make([]string, 0, len(cfg.Backends))
	for n := range cfg.Backends {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		mark := " "
		if n == cfg.Default {
			mark = "*"
		}
		fmt.Printf("  %s %-20s %s\n", mark, n, cfg.Backends[n])
	}
	return nil
}

func cmdList(args []string) error {
	if len(args) == 1 {
		return printInstance(args[0])
	}
	names, err := instances()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("(no instances)")
		return nil
	}
	for i, n := range names {
		if i > 0 {
			fmt.Println()
		}
		if err := printInstance(n); err != nil {
			return err
		}
	}
	return nil
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]
	need := func(n int) error {
		if len(rest) != n {
			return fmt.Errorf("%s: expected %d argument(s)\n\n%s", cmd, n, usage)
		}
		return nil
	}
	switch cmd {
	case "serve":
		if len(rest) < 1 {
			return fmt.Errorf("serve: instance name required\n\n%s", usage)
		}
		return cmdServe(rest[0], rest[1:])
	case "ui":
		return cmdUI(rest)
	case "add":
		if err := need(3); err != nil {
			return err
		}
		name, target := rest[1], rest[2]
		if !validName(name) {
			return fmt.Errorf("invalid backend name %q (allowed: [A-Za-z0-9._-])", name)
		}
		if _, _, err := parseTarget(target); err != nil {
			return err
		}
		return mutate(rest[0], func(c *Config) error {
			c.Backends[name] = target
			if c.Default == "" {
				c.Default = name // 最初の登録は自動で default に
			}
			return nil
		})
	case "rm":
		if err := need(2); err != nil {
			return err
		}
		name := rest[1]
		return mutate(rest[0], func(c *Config) error {
			if _, ok := c.Backends[name]; !ok {
				return fmt.Errorf("unknown backend %q in instance %q", name, rest[0])
			}
			delete(c.Backends, name)
			if c.Default == name {
				c.Default = ""
				fmt.Fprintf(os.Stderr, "warning: default backend removed; set a new one with `default %s <name>`\n", rest[0])
			}
			return nil
		})
	case "default":
		if err := need(2); err != nil {
			return err
		}
		name := rest[1]
		return mutate(rest[0], func(c *Config) error {
			if _, ok := c.Backends[name]; !ok {
				return fmt.Errorf("unknown backend %q in instance %q", name, rest[0])
			}
			c.Default = name
			return nil
		})
	case "list", "ls":
		if len(rest) > 1 {
			return fmt.Errorf("list: too many arguments\n\n%s", usage)
		}
		return cmdList(rest)
	case "delete":
		if err := need(1); err != nil {
			return err
		}
		path, err := configPath(rest[0])
		if err != nil {
			return err
		}
		if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("unknown instance %q", rest[0])
		} else {
			return err
		}
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

func main() {
	log.SetFlags(log.Ltime)
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
