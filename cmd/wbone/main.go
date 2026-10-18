// main.go WorkBuddy 管家 —— 一个进程装下两件事：
//
//	1. 账号切换：替换客户端登录态 + 重启 WorkBuddy（原 WbSwitch）
//	2. API 面板：workbuddy2api 网关与它的管理面板（internal/gwapi）
//
// 关键点：两者跑在同一个 http.Server、同一个端口上，所以面板不需要 iframe 跨源，
// 也不会出现「两个程序、两个窗口、两个端口」的割裂感。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	_ "embed"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gwapi"
)

const (
	appTitle   = "WorkBuddy 管家"
	appVersion = "1.0.0"
)

var browserCandidates = []string{
	`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
	`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	`C:\Program Files\Google\Chrome\Application\chrome.exe`,
	`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
}

//go:embed ui.html
var uiHTML []byte

var (
	lastSeen       atomic.Int64
	browser        *exec.Cmd
	browserProfile string
	apiKey         string // 注入页面，供面板 iframe 自动登录
	appPort        string
	stopAll        context.CancelFunc
)

func mustExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	return filepath.Dir(exe)
}

var exeDir = mustExeDir()

func baseDir() string { return exeDir }
func ensureDir(p string) {
	_ = os.MkdirAll(p, 0o755)
}

func logf(format string, args ...any) {
	p := filepath.Join(baseDir(), "data", "app.log")
	ensureDir(filepath.Dir(p))
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// ── 统一路由 ────────────────────────────────────────────────────────

// shellHTML 把 API key 注入页面：面板 iframe 带 #key= 打开后自动登录，
// 账号页也能直接调 /panel/api/overview 显示积分。
func shellHTML() []byte {
	return bytes.Replace(uiHTML, []byte("{{API_KEY}}"), []byte(apiKey), 1)
}

// decorate 在网关 handler 外层挂载统一界面："/" 是我们自己的页面，
// 其余路径（/v1/*、/panel/*、/healthz…）原样交给网关。
func decorate(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	registerAccounts(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			next.ServeHTTP(w, r)
			return
		}
		lastSeen.Store(time.Now().UnixNano())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(shellHTML())
	})
	return mux
}

// ── 配置与端口 ──────────────────────────────────────────────────────

// ensureConfig 首次运行自动落一份配置（与网关独立运行时的行为一致）。
func ensureConfig(path string) (*gwapi.Config, error) {
	cfg, err := gwapi.Load(path)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if _, werr := gwapi.WriteDefault(path); werr != nil {
		return nil, fmt.Errorf("生成配置失败：%w", werr)
	}
	// 桌面版默认只监听回环：面板与接口都只在本机使用，没必要暴露到局域网。
	// 仅在「刚生成」的这一份上收紧，用户自己改过的配置一律尊重。
	if b, rerr := os.ReadFile(path); rerr == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			if s, _ := m["listen"].(string); s == ":7863" || s == "0.0.0.0:7863" {
				m["listen"] = "127.0.0.1:7863"
				if out, e := json.MarshalIndent(m, "", "  "); e == nil {
					_ = os.WriteFile(path, out, 0o600)
				}
			}
		}
	}
	return gwapi.Load(path)
}

func listenPort(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 && i < len(addr)-1 {
		return addr[i+1:]
	}
	return "7863"
}

func baseURL() string { return "http://127.0.0.1:" + appPort + "/" }

// ownInstance 端口上跑的是不是我们自己（用于单实例复用窗口）。
func ownInstance() bool {
	cli := &http.Client{Timeout: 700 * time.Millisecond}
	resp, err := cli.Get(baseURL() + "accounts/api/state")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	return strings.Contains(string(buf[:n]), `"accounts"`)
}

func portFree(port string) bool {
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func waitReady(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ownInstance() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// ── 窗口 ────────────────────────────────────────────────────────────

func openWindow(url string) {
	// 每次启动用独立 profile 目录：复用同一目录时 Edge 会把请求交给已运行的旧进程，
	// 表现成「点了没反应」；退出时删除，不在磁盘上留垃圾。
	profile := filepath.Join(baseDir(), "data", fmt.Sprintf("browser-%d", os.Getpid()))
	ensureDir(profile)
	browserProfile = profile
	for _, b := range browserCandidates {
		if _, err := os.Stat(b); err != nil {
			continue
		}
		// 不要加 HideWindow：会导致 Chromium 把首个窗口建成隐藏状态。
		cmd := exec.Command(b,
			"--app="+url,
			"--user-data-dir="+profile,
			"--window-size=1180,800",
			"--no-first-run",
			"--no-default-browser-check",
			"--disable-features=Translate",
			// 关窗口即退出进程，不在托盘/后台常驻（否则管家永远等不到"窗口关闭"）。
			"--disable-background-mode",
			// 全部请求都在本机回环上：显式关掉系统代理。
			// 有些代理软件（Clash/Surge 的 TUN 或全局模式）会把 127.0.0.1 也劫持，
			// 结果窗口打开只剩一行 "upstream connect failed"，页面脚本不执行、心跳不发。
			"--no-proxy-server",
		)
		if err := cmd.Start(); err == nil {
			browser = cmd
			// 关进 Job：管家无论怎么退出（正常关闭 / 强杀 / 崩溃），
			// 这个窗口都会被系统一起收走，不会留成孤儿。
			if jerr := bindBrowserToJob(cmd.Process.Pid); jerr != nil {
				logf("窗口进程未能绑定到管家：%v", jerr)
			}
			return
		}
	}
	_ = runCmd("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// ── Win32 ───────────────────────────────────────────────────────────

var (
	u32             = syscall.NewLazyDLL("user32.dll")
	pEnumWindows    = u32.NewProc("EnumWindows")
	pGetWindowTextW = u32.NewProc("GetWindowTextW")
	pShowWindow     = u32.NewProc("ShowWindow")
	pSetForeground  = u32.NewProc("SetForegroundWindow")
	pPostMessageW   = u32.NewProc("PostMessageW")
	pMessageBoxW    = u32.NewProc("MessageBoxW")

	winTarget        string
	winFound         []uintptr
	enumWindowHandle = syscall.NewCallback(func(h, l uintptr) uintptr {
		buf := make([]uint16, 512)
		pGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if strings.Contains(syscall.UTF16ToString(buf), winTarget) {
			winFound = append(winFound, h)
		}
		return 1
	})
)

func findWindows(part string) []uintptr {
	winTarget = part
	winFound = nil
	pEnumWindows.Call(enumWindowHandle, 0)
	return winFound
}

// ensureVisible 主动把窗口恢复并提到前台：无控制台进程拉起 Chromium 时，
// 首个窗口可能继承到隐藏/最小化状态，表现为「进程在、标题对、屏幕上什么都没有」。
func ensureVisible(part string, tries int) uintptr {
	for i := 0; i < tries; i++ {
		if hs := findWindows(part); len(hs) > 0 {
			h := hs[0]
			pShowWindow.Call(h, 9) // SW_RESTORE
			pShowWindow.Call(h, 5) // SW_SHOW
			pSetForeground.Call(h)
			return h
		}
		time.Sleep(500 * time.Millisecond)
	}
	return 0
}

func msgBox(text string) {
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(appTitle)
	pMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), 0x10)
}

func shutdown() {
	if stopAll != nil {
		stopAll()
	}
	for _, h := range findWindows(appTitle) {
		pPostMessageW.Call(h, 0x0010) // WM_CLOSE
	}
	time.Sleep(600 * time.Millisecond)
	if browser != nil && browser.Process != nil {
		_ = browser.Process.Kill()
		time.Sleep(300 * time.Millisecond)
	}
	if browserProfile != "" {
		_ = os.RemoveAll(browserProfile)
	}
	os.Exit(0)
}

// ── 启动 ────────────────────────────────────────────────────────────

// cleanStaleBrowserProfiles 清理上次异常退出残留的浏览器临时目录。
//
// 每个实例用 data/browser-<pid> 当 Edge 的 user-data-dir，正常退出会删掉；
// 被任务管理器强杀时残留，而 Edge 会往里写几百 MB 缓存，久了很占地方。
// 只删一小时前的（正在跑的这个按定义还很新），删不掉就跳过。
func cleanStaleBrowserProfiles() {
	root := filepath.Join(baseDir(), "data")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Hour)
	n := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "browser-") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.RemoveAll(filepath.Join(root, e.Name())) == nil {
			n++
		}
	}
	if n > 0 {
		logf("清理了 %d 个残留的浏览器临时目录", n)
	}
}

// migrateFlat 把旧版扁平档案（profiles/<uid>.info）搬进目录结构。
func migrateFlat() {
	entries, err := os.ReadDir(profilesDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".info") {
			continue
		}
		uid := strings.TrimSuffix(e.Name(), ".info")
		ensureDir(profileDir(uid))
		_ = copyFileAtomic(filepath.Join(profilesDir(), e.Name()), snapPath(uid, "workbuddy-desktop"))
		_ = os.Remove(filepath.Join(profilesDir(), e.Name()))
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		uid := strings.TrimSuffix(e.Name(), ".json")
		ensureDir(profileDir(uid))
		_ = copyFileAtomic(filepath.Join(profilesDir(), e.Name()), metaPath(uid))
		_ = os.Remove(filepath.Join(profilesDir(), e.Name()))
	}
}

func main() {
	ensureDir(profilesDir())
	ensureDir(backupDir())
	ensureDir(filepath.Join(baseDir(), "data"))
	cleanStaleBrowserProfiles()
	migrateFlat()

	cfgPath := filepath.Join(baseDir(), "config.json")
	cfg, err := ensureConfig(cfgPath)
	if err != nil {
		logf("配置加载失败：%v", err)
		msgBox("配置文件读取失败：\n" + err.Error())
		return
	}
	apiKey = cfg.APIKey
	appPort = listenPort(cfg.Listen)

	// 单实例：已有管家在跑，就把那个窗口叫到前台，不重复占端口。
	//
	// 这里绝不能再 openWindow：本进程紧接着就退出了，新开出来的 Edge 窗口
	// 没有任何进程接管（它的父进程已经没了），于是变成一个关不掉的孤儿窗口。
	// 用户看到的就是"莫名多出一个 Edge 页面，关也关不掉"。
	if ownInstance() {
		logf("已有实例运行在 %s，唤出既有窗口", baseURL())
		if ensureVisible(appTitle, 6) == 0 {
			// 窗口确实没了（服务还在）才补一个，这时它会被本进程接管到退出为止。
			openWindow(baseURL())
			time.Sleep(1200 * time.Millisecond)
		}
		return
	}
	if !portFree(appPort) {
		logf("端口 %s 被占用", appPort)
		msgBox("端口 " + appPort + " 已被其他程序占用。\n\n请关闭占用该端口的程序，或修改 config.json 里的 listen 后重试。")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopAll = cancel
	gwErr := make(chan error, 1)
	go func() {
		gwErr <- gwapi.Run(ctx, cfgPath, decorate)
	}()

	lastSeen.Store(time.Now().UnixNano())
	if !waitReady(20 * time.Second) {
		logf("网关启动超时")
		msgBox("服务启动超时，请查看 data\\app.log。")
		shutdown()
	}

	openWindow(baseURL())
	logf("已启动：%s（客户端：%s）", baseURL(), findClient())

	go func() {
		if h := ensureVisible(appTitle, 30); h != 0 {
			logf("窗口已显示 hwnd=%d", h)
		} else {
			logf("警告：没有找到窗口")
		}
	}()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-gwErr:
			logf("网关退出：%v", err)
			msgBox("后台服务意外退出：\n" + fmt.Sprint(err))
			shutdown()
		case <-ticker.C:
			// 优先看浏览器进程：窗口关掉 → 进程退出 → 管家跟着退，不留后台。
			// 不能只靠心跳：页面若因代理/证书/网络策略加载失败，脚本根本不执行，
			// 心跳永远不来，程序会在 150 秒后自行消失（用户只会看到"点了没反应"）。
			if browser != nil && browser.Process != nil {
				if !pidAlive(browser.Process.Pid) {
					logf("窗口已关闭，退出")
					shutdown()
				}
				continue
			}
			// 兜底（用系统默认浏览器打开时拿不到进程）：靠页面心跳。
			if time.Since(time.Unix(0, lastSeen.Load())) > 150*time.Second {
				logf("页面已关闭，退出")
				shutdown()
			}
		}
	}
}

// pidAlive 用 GetExitCodeProcess 判断进程是否还在（STILL_ACTIVE=259）。
func pidAlive(pid int) bool {
	const processQueryLimitedInfo = 0x1000
	const stillActive = 259
	h, err := syscall.OpenProcess(processQueryLimitedInfo, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
