// main.go WorkBuddy 管家 —— 一个进程装下两件事：
//
//  1. 账号切换：替换客户端登录态 + 重启 WorkBuddy（原 WbSwitch）
//  2. API 面板：workbuddy2api 网关与它的管理面板（internal/gwapi）
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
	"io"
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
	appVersion = "1.0.9"
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
	out := bytes.Replace(uiHTML, []byte("{{API_KEY}}"), []byte(apiKey), -1)
	return bytes.Replace(out, []byte("{{VERSION}}"), []byte(appVersion), -1)
}

// decorate 在网关 handler 外层挂载统一界面："/" 是我们自己的页面，
// 其余路径（/v1/*、/panel/*、/healthz…）原样交给网关。
func decorate(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	registerAccounts(mux)
	registerUpdate(mux)
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

// localClient 访问本机 127.0.0.1 的专用客户端：强制不走代理。
//
// http.Client 缺省 Transport 的 Proxy 是 ProxyFromEnvironment，只要环境里存在
// HTTP_PROXY/HTTPS_PROXY（Clash、v2ray、公司代理、IDE 终端都会设），发往
// localhost 的自检请求也会被丢给代理。代理通常拒绝或拖到超时，于是
// ownInstance() 恒为 false → waitReady 撑满 20 秒 → 打印「网关启动超时」自己
// 退出，而服务其实一直在正常监听。用户看到的就是「双击没反应」。
// Edge 那边早就加了 --no-proxy-server，这里是同一课的 Go 侧版本。
func localClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: nil},
	}
}

// ownInstance 端口上跑的是不是我们自己（用于单实例复用窗口）。
//
// 打的是 /accounts/api/ping 而不是 /accounts/api/state：后者要扫一遍本地对话
// 数据库，WorkBuddy 客户端正锁着库时会卡好几秒，700ms 的超时必然踩空——
// 那样就会把「自己」认成「别的程序」，waitReady 空转、单实例逻辑也会走错。
// ping 只回一个身份标记，纯内存，快且稳。
func ownInstance() bool {
	resp, err := localClient(1500 * time.Millisecond).Get(baseURL() + "accounts/api/ping")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return strings.Contains(string(body), `"app":"wbone"`)
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
			// 全新 profile 首次启动时，Edge 会从系统里的默认浏览器导入扩展和书签：
			// Tampermonkey、IDM、TeraBox 这些全被搬进来，装完/更新完各自弹一次
			// 欢迎页——用户看到的就是「莫名启动 Edge 并打开一个奇怪的链接」。
			// 管家页面是纯 HTML/JS，不需要任何扩展，直接禁用，同时绕过导入流程。
			"--disable-extensions",
			"--disable-sync",
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
	k32             = syscall.NewLazyDLL("kernel32.dll")
	pEnumWindows    = u32.NewProc("EnumWindows")
	pGetWindowTextW = u32.NewProc("GetWindowTextW")
	pGetWindowPID   = u32.NewProc("GetWindowThreadProcessId")
	pShowWindow     = u32.NewProc("ShowWindow")
	pSetForeground  = u32.NewProc("SetForegroundWindow")
	pMessageBoxW    = u32.NewProc("MessageBoxW")
	pCreateSnapshot = k32.NewProc("CreateToolhelp32Snapshot")
	pProc32First    = k32.NewProc("Process32FirstW")
	pProc32Next     = k32.NewProc("Process32NextW")

	// 只认「自己拉起的浏览器进程树」里的窗口。
	//
	// 早先按标题子串匹配，把用户自己浏览器里的同名页面也算了进来：用户在
	// GitHub 上看 Release 页（标题含「WorkBuddy 管家」），管家一启动就对那个
	// 窗口发 SW_RESTORE（最大化被还原，看着"变小了"），退出时再 WM_CLOSE，
	// 直接把人家的窗口关掉。窗口归属必须按进程判断，标题只能当辅助条件。
	winTarget  string
	winAllowed map[uint32]bool
	winFound   []uintptr

	enumWindowHandle = syscall.NewCallback(func(h, l uintptr) uintptr {
		if winAllowed != nil {
			var pid uint32
			pGetWindowPID.Call(h, uintptr(unsafe.Pointer(&pid)))
			if !winAllowed[pid] {
				return 1
			}
		}
		buf := make([]uint16, 512)
		pGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if strings.Contains(syscall.UTF16ToString(buf), winTarget) {
			winFound = append(winFound, h)
		}
		return 1
	})
)

// processEntry32 对应 Win32 的 PROCESSENTRY32W，只用到进程号与父进程号。
type processEntry32 struct {
	Size            uint32
	CntUsage        uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	CntThreads      uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [260]uint16
}

// descendantPIDs 返回 root 的全部子孙进程（Chromium 的窗口往往挂在子进程上）。
func descendantPIDs(root uint32) []uint32 {
	const (
		th32csSnapProcess = 0x00000002
		invalidHandle     = ^uintptr(0)
	)
	snap, _, _ := pCreateSnapshot.Call(th32csSnapProcess, 0)
	if snap == 0 || snap == invalidHandle {
		return nil
	}
	defer syscall.CloseHandle(syscall.Handle(snap))

	children := map[uint32][]uint32{}
	var pe processEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	ok, _, _ := pProc32First.Call(snap, uintptr(unsafe.Pointer(&pe)))
	for ok != 0 {
		children[pe.ParentProcessID] = append(children[pe.ParentProcessID], pe.ProcessID)
		ok, _, _ = pProc32Next.Call(snap, uintptr(unsafe.Pointer(&pe)))
	}

	var out []uint32
	queue := []uint32{root}
	seen := map[uint32]bool{root: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out
}

// browserWindowPIDs 是本次拉起的浏览器进程 + 它的子孙。
func browserWindowPIDs() []uint32 {
	if browser == nil || browser.Process == nil {
		return nil
	}
	root := uint32(browser.Process.Pid)
	return append([]uint32{root}, descendantPIDs(root)...)
}

func pidSet(ids []uint32) map[uint32]bool {
	m := make(map[uint32]bool, len(ids))
	for _, v := range ids {
		m[v] = true
	}
	return m
}

func findWindows(part string, allow map[uint32]bool) []uintptr {
	winTarget = part
	winAllowed = allow
	winFound = nil
	pEnumWindows.Call(enumWindowHandle, 0)
	return winFound
}

// ensureVisibleSelf 把自己拉起的那个窗口恢复并提到前台。
//
// 无控制台进程拉起 Chromium 时，首个窗口可能继承到隐藏/最小化状态，需要主动
// SHOW 一下。但只在自家进程树里找——宁可找不到，也不能碰别人的窗口。
func ensureVisibleSelf(tries int) uintptr {
	for i := 0; i < tries; i++ {
		if hs := findWindows(appTitle, pidSet(browserWindowPIDs())); len(hs) > 0 {
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
	// 只结束自己拉起的那个浏览器进程，Job 会把它的子进程一并收走。
	// 这里刻意不再遍历窗口发 WM_CLOSE：标题相同不代表窗口是我们的，
	// 用户浏览器里开着同名页面时，那一下会直接关掉用户自己的标签页。
	if browser != nil && browser.Process != nil {
		_ = browser.Process.Kill()
		time.Sleep(400 * time.Millisecond)
	}
	if browserProfile != "" {
		_ = os.RemoveAll(browserProfile)
	}
	os.Exit(0)
}

// focusExisting 让已经在跑的那个实例把自己的窗口叫到前台。
//
// 为什么不让新进程自己去找窗口：只有宿主知道自己拉起了哪个浏览器进程，
// 新进程手上没有这个信息，只能按标题猜——而标题会撞车（用户浏览器里正好
// 开着含「WorkBuddy 管家」的页面），一猜就误伤。
func focusExisting() bool {
	cli := localClient(3 * time.Second)
	req, err := http.NewRequest(http.MethodPost, baseURL()+"accounts/api/focus", nil)
	if err != nil {
		return false
	}
	resp, err := cli.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Found bool `json:"found"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Found
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

	// 更新接棒：这个进程是老版本拉起的新版本，而老版本可能还在退出路上、
	// 端口没释放。必须先等它让出来——否则下面的 ownInstance() 会把「正在
	// 退出的老版本」当成活着的实例，新进程直接退出，用户看到的就是
	// 「更新完打不开了」。这是自动更新最容易踩的坑。
	handoffWait()

	// 老版本留下的 WorkBuddyOne.exe.old 在这里删；旧进程可能还没退干净，
	// 删不掉就过一会儿再试，失败的留着不影响使用。
	go cleanupOldExe()

	// 单实例：已有管家在跑，就把那个窗口叫到前台，不重复占端口。
	//
	// 这里绝不能再 openWindow：本进程紧接着就退出了，新开出来的 Edge 窗口
	// 没有任何进程接管（它的父进程已经没了），于是变成一个关不掉的孤儿窗口。
	// 用户看到的就是"莫名多出一个 Edge 页面，关也关不掉"。
	if ownInstance() {
		logf("已有实例运行在 %s，唤出既有窗口", baseURL())
		if !focusExisting() {
			// 对方没响应、或它自己手上也没有窗口。这里不再补开：本进程马上就要
			// 退出，新开的窗口没人接管，会变成关不掉的孤儿。老实例检测到浏览器
			// 进程已死会自行退出，用户再点一次图标即可。
			logf("既有实例未返回窗口，等待其自行退出")
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
		// 只是「没等到就绪」，不是致命错误：网关真正的启动失败会从 gwErr 回来，
		// 那时才该退出。这里照常开窗——服务可能只是初始化慢（比如客户端正锁着
		// 对话数据库），页面自己会重试。早先在这里弹框 + 退出，无人值守时
		// 会卡在一个没人点的对话框上，服务明明活着却用不了。
		logf("等待网关就绪超时（继续启动，服务可能仍在初始化）")
	}

	openWindow(baseURL())
	logf("已启动：%s（客户端：%s）", baseURL(), findClient())

	go func() {
		if h := ensureVisibleSelf(30); h != 0 {
			logf("窗口已显示 hwnd=%d", h)
		} else {
			logf("警告：没有找到窗口")
		}
	}()

	// 每天最多静默查一次更新。放在窗口出来之后、延迟几秒，
	// 不跟启动抢网络和 CPU；查到了只在侧栏亮一行，不会自己动手装。
	go func() {
		time.Sleep(6 * time.Second)
		autoCheckOnce()
	}()

	// 登录态保活：客户端每次续期都会换一份新的 refresh token 并作废旧的，
	// 但它只写活动的 .info，不写我们的 profiles/ 快照。切号时快照里若是旧的，
	// 切过去就会被要求重新登录。所以这里持续把活动登录态同步进快照。
	go refreshSnapshotLoop()

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
