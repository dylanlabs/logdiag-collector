// SPDX-License-Identifier: GPL-3.0-or-later

// Package ui 提供本机网页界面的 HTTP 服务。
//
// 安全要点：只监听回环地址；首次打开用一次性令牌换取 Cookie；校验 Host 头防 DNS 重绑定；
// 修改类请求校验 Origin 防跨站请求；不允许被嵌入其他页面。
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/web"
)

// Options 是 New 的参数。
type Options struct {
	// Addr 是监听地址，默认 127.0.0.1:0（随机端口）。只允许回环地址。
	Addr string
	// IdleExit 是最后一个界面窗口关闭后等待多久退出，0 表示不自动退出。
	// 只有至少打开过一次界面后才会生效。
	IdleExit time.Duration
	// Logger 为 nil 时不记录日志。
	Logger *slog.Logger
	// Hub 把后台变化推给界面，为 nil 时新建一个。
	Hub *Hub
	// App 是界面背后的设备库、设置和任务中心；为 nil 时只有界面外壳和 /api/info（测试用）。
	App *App
	// Busy 报告是否还有后台任务。界面窗口都关闭后，等任务结束再退出。
	Busy func() bool
}

// Server 是本机界面服务。
type Server struct {
	opts    Options
	ln      net.Listener
	port    string
	srv     *http.Server
	mux     *http.ServeMux
	cookie  string // Cookie 名带端口：Cookie 不区分端口，多开时互不覆盖
	session string // Cookie 值

	mu          sync.Mutex
	first       string          // 第一个窗口用的启动令牌
	tokens      map[string]bool // 还没用过的一次性启动令牌
	instanceKey string          // 再次启动的程序请求开新窗口时用的密钥，见 instance.go
	clients     int
	idleTimer   *time.Timer
	idleOnce    sync.Once
	waitingBusy bool
	idle        chan struct{}
	stopping    chan struct{}
}

// New 开始监听，但还不处理请求；调用 Run 开始服务。
func New(opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Hub == nil {
		opts.Hub = NewHub()
	}
	host, _, err := net.SplitHostPort(opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("监听地址 %q 无效：%w", opts.Addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("出于安全考虑，界面只能监听本机回环地址（如 127.0.0.1），不能用 %q；局域网模式尚未开放", host)
	}
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("无法监听 %s：%w", opts.Addr, err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	first := randomHex(32)
	s := &Server{
		opts:     opts,
		ln:       ln,
		port:     port,
		cookie:   "ldc_" + port,
		session:  randomHex(32),
		first:    first,
		tokens:   map[string]bool{first: true},
		idle:     make(chan struct{}),
		stopping: make(chan struct{}),
		mux:      http.NewServeMux(),
	}
	s.mux.Handle("GET /", http.FileServerFS(web.FS))
	s.mux.HandleFunc("GET /api/info", s.handleInfo)
	s.mux.HandleFunc("GET /api/events", s.handleEvents)
	if opts.App != nil {
		s.routes(opts.App)
	}
	s.srv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(opts.Logger.Handler(), slog.LevelDebug),
	}
	return s, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// URL 返回界面地址（不含令牌）。
func (s *Server) URL() string {
	return "http://127.0.0.1:" + s.port + "/"
}

// LaunchURL 返回带一次性令牌的地址，用于打开第一个窗口。
func (s *Server) LaunchURL() string {
	return s.URL() + "?token=" + s.first
}

// reopen 为再开一个窗口准备新的一次性令牌。程序正在退出时返回 false。
func (s *Server) reopen() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.idle:
		return "", false
	default:
	}
	// 新窗口连上之前不要因为“窗口都关了”而退出：重新计时。
	if s.clients == 0 && s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = time.AfterFunc(s.opts.IdleExit, s.checkIdle)
	}
	t := randomHex(32)
	s.tokens[t] = true
	return s.URL() + "?token=" + t, true
}

// Run 处理请求，直到 ctx 结束或界面关闭超过 IdleExit。
func (s *Server) Run(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- s.srv.Serve(s.ln) }()
	select {
	case <-ctx.Done():
		s.opts.Logger.Info("收到退出信号")
	case <-s.idle:
		s.opts.Logger.Info("界面已关闭，程序退出")
	case err := <-errc:
		return err
	}
	close(s.stopping) // 让事件流先结束，否则 Shutdown 会一直等它
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.srv.Shutdown(shutdownCtx)
	<-errc
	return err
}

// ServeHTTP 在路由之前做安全检查。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")

	if !s.allowedHost(r.Host) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path == instancePath {
		s.handleInstance(w, r)
		return
	}
	if r.URL.Path == "/" && r.URL.Query().Has("token") {
		s.exchangeToken(w, r)
		return
	}
	if c, err := r.Cookie(s.cookie); err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.session)) != 1 {
		forbidden(w)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != "http://"+r.Host {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// allowedHost 防 DNS 重绑定：只接受用回环地址访问本端口的请求。
func (s *Server) allowedHost(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || port != s.port {
		return false
	}
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func (s *Server) exchangeToken(w http.ResponseWriter, r *http.Request) {
	given := r.URL.Query().Get("token")
	s.mu.Lock()
	ok := false
	for t := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(given), []byte(t)) == 1 {
			ok = true
			delete(s.tokens, t) // 只能用一次：别的进程从命令行参数里看到也没用
		}
	}
	s.mu.Unlock()
	if !ok {
		forbidden(w)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookie,
		Value:    s.session,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprint(w, `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>LogDiag Collector</title>`+
		`<p>请通过 LogDiag Collector 程序打开此页面。如果窗口已关闭，请重新运行程序。</p></html>`)
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(buildinfo.Get())
}

// handleEvents 是界面的事件流：任务进度、设备状态等变化都从这里推送。
// 连接数归零一段时间后程序自动退出。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	events, unsubscribe := s.opts.Hub.subscribe()
	defer unsubscribe()
	s.clientConnected()
	defer s.clientDisconnected()

	fmt.Fprint(w, "event: hello\ndata: {}\n\n")
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stopping:
			return
		case ev, ok := <-events:
			if !ok {
				return // 积压太多被断开，浏览器会重连并重新读取
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) clientConnected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients++
	s.waitingBusy = false
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

func (s *Server) clientDisconnected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients--
	if s.clients == 0 && s.opts.IdleExit > 0 {
		s.idleTimer = time.AfterFunc(s.opts.IdleExit, s.checkIdle)
	}
}

func (s *Server) checkIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients != 0 {
		return
	}
	if s.opts.Busy != nil && s.opts.Busy() {
		// 窗口关了但收集还没完：等任务结束再退出，期间重新打开界面也能看到进度。
		if !s.waitingBusy {
			s.waitingBusy = true
			s.opts.Logger.Info("界面已关闭，还有收集任务在进行，完成后自动退出；期间再次运行程序会重新打开界面")
		}
		s.idleTimer = time.AfterFunc(5*time.Second, s.checkIdle)
		return
	}
	s.idleOnce.Do(func() { close(s.idle) })
}
