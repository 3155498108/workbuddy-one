// update.go 软件自我更新：查 Release → 下载 zip → 换掉自己的 exe → 拉起新版本。
//
// 设计上的几条取舍：
//
//  1. 默认只查、不自动装。替换 exe 必然要重启管家，用户正在切号时被重启会打断操作，
//     所以自动检查默认只负责在侧栏亮一个「有新版本」，装不装由用户点。想要全自动的
//     人可以在侧栏把「自动安装」打开（prefs 里的 update_auto_apply），之后每天启动
//     后查到新版直接换、自己重启。
//  2. 装的是 zip 不是裸 exe。zip 自带 CRC，解压时能发现下载被截断/被中间页顶掉；
//     裸 exe 拿不到任何完整性信息。
//  3. 运行的 exe 允许改名、不允许覆盖。所以换文件的手法是「旧的改名 .old → 新的
//     改名为正式名」，任何一步失败都回滚，用户手上始终有一个能启动的 exe。
//  4. 新进程起来时旧进程可能还没退干净（端口还占着）。用环境变量 HANDOFF 让它
//     先等端口释放再启动服务，否则新进程会因为「端口被占用」直接退出——那就真
//     的变成「更新完打不开了」。
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// updateRepo 更新源仓库；资产名固定为 WorkBuddyOne-vX.Y.Z-win64.zip。
	updateRepo        = "3155498108/workbuddy-one"
	updateAssetPrefix = "WorkBuddyOne-v"
	updateAssetSuffix = "-win64.zip"
	updateExeName     = "WorkBuddyOne.exe"

	updateMaxZipBytes = 120 << 20 // 安装包上限，防止更新源返回异常大的东西
	updateMinExeBytes = 1 << 20   // 小于 1 MiB 一定不是本程序
	updateAutoGap     = 24 * time.Hour
	updateCacheGap    = 5 * time.Minute
	updateHandoffEnv  = "WBONE_UPDATE_HANDOFF"
)

// updateState 是给界面看的更新状态。检查结果缓存在内存里，
// 界面刷新只读缓存，不会每次都去打 GitHub（匿名 API 一小时只给 60 次）。
type updateState struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	HasUpdate bool   `json:"has_update"`
	Notes     string `json:"notes,omitempty"`
	Page      string `json:"page,omitempty"`
	Asset     string `json:"asset,omitempty"`
	AssetSize int64  `json:"asset_size,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
	Checking  bool   `json:"checking"`
	Applying  bool   `json:"applying"`
	Error     string `json:"error,omitempty"`
	AutoCheck bool   `json:"auto_check"`
	AutoApply bool   `json:"auto_apply"`
	Feed      string `json:"feed,omitempty"`
}

// ghAsset / ghRelease 只取用得到的字段，GitHub 返回的其他内容一律忽略。
type ghAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
}

type ghRelease struct {
	Tag    string    `json:"tag_name"`
	Body   string    `json:"body"`
	Page   string    `json:"html_url"`
	At     string    `json:"published_at"`
	Assets []ghAsset `json:"assets"`
}

var (
	updMu       sync.Mutex
	updState    = updateState{Current: appVersion}
	updAssetURL string
	updChecked  time.Time
)

// ── 查询 ────────────────────────────────────────────────────────────

func updateFeedURL() string {
	if s := strings.TrimSpace(os.Getenv("WBONE_UPDATE_FEED")); s != "" {
		return s
	}
	if s := strings.TrimSpace(readPrefs().UpdateFeed); s != "" {
		return s
	}
	return "https://api.github.com/repos/" + updateRepo + "/releases/latest"
}

// updateClient 更新请求要访问 GitHub，**必须**走系统代理通道
// （国内直连 api.github.com 基本不通），所以这里用默认 Transport，
// 与 localClient() 刻意相反——那个是给自己人的，这个是给外网的。
func updateClient() *http.Client { return &http.Client{Timeout: 20 * time.Second} }

func fetchRelease() (*ghRelease, error) {
	req, err := http.NewRequest(http.MethodGet, updateFeedURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "WorkBuddyOne/"+appVersion)
	resp, err := updateClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接更新源失败（检查网络或代理）：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("更新源返回 %d：%s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var r ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("更新源响应解析失败：%w", err)
	}
	if strings.TrimSpace(r.Tag) == "" {
		return nil, fmt.Errorf("更新源没有返回版本号")
	}
	return &r, nil
}

// pickAsset 在资产里挑 Windows 包。不按「第一个」挑——源码包、校验文件都在
// 同一个列表里，按名字和版本号挑才不会下错东西。
func pickAsset(r *ghRelease) (name, url string, size int64) {
	bestVer := ""
	for _, a := range r.Assets {
		n := a.Name
		if !strings.HasPrefix(n, updateAssetPrefix) || !strings.HasSuffix(n, updateAssetSuffix) {
			continue
		}
		v := strings.TrimSuffix(strings.TrimPrefix(n, updateAssetPrefix), updateAssetSuffix)
		if name == "" || versionLess(bestVer, v) {
			name, url, size, bestVer = n, a.URL, a.Size, v
		}
	}
	return
}

// checkUpdate 查询更新源。force=false 时 5 分钟内复用上次结果。
func checkUpdate(force bool) updateState {
	updMu.Lock()
	if updState.Checking || updState.Applying {
		s := updState
		updMu.Unlock()
		return s
	}
	if !force && !updChecked.IsZero() && time.Since(updChecked) < updateCacheGap {
		s := updState
		updMu.Unlock()
		return s
	}
	updState.Checking = true
	updState.Error = ""
	updMu.Unlock()

	rel, err := fetchRelease()

	updMu.Lock()
	defer updMu.Unlock()
	updState.Checking = false
	updChecked = time.Now()
	updState.CheckedAt = updChecked.Format("2006-01-02 15:04")
	if err != nil {
		updState.Error = err.Error()
		updState.HasUpdate = false
		return updState
	}
	latest := strings.TrimPrefix(strings.TrimSpace(rel.Tag), "v")
	updState.Latest = latest
	updState.HasUpdate = versionLess(appVersion, latest)
	updState.Page = rel.Page
	// 更新说明只留前 600 字：一个 release 的正文动辄几 KB，
	// 而 /accounts/api/update 是界面每次刷新都会拉的接口。
	updState.Notes = truncateRunes(strings.TrimSpace(rel.Body), 600)
	updState.Asset, updAssetURL, updState.AssetSize = pickAsset(rel)

	// 记下检查时间，供「每天最多自动查一次」使用。
	p := readPrefs()
	p.LastUpdateCheck = time.Now().Unix()
	writePrefs(p)
	return updState
}

func currentUpdateState() updateState {
	updMu.Lock()
	s := updState
	updMu.Unlock()
	s.Current = appVersion
	s.AutoCheck = autoCheckEnabled()
	s.AutoApply = autoApplyEnabled()
	s.Feed = updateFeedURL()
	return s
}

func autoCheckEnabled() bool { return !readPrefs().UpdateCheckOff }

// autoApplyEnabled 查到新版本后是否直接安装（含自动重启）。默认关。
func autoApplyEnabled() bool { return readPrefs().UpdateAutoApply }

// autoCheckOnce 启动后台静默检查：没开开关、或 24 小时内查过就跳过。
// 开了「自动安装」时，查到新版本就在这里直接换掉自己并重启——不需要用户点。
func autoCheckOnce() {
	if !autoCheckEnabled() {
		return
	}
	if p := readPrefs(); p.LastUpdateCheck > 0 &&
		time.Since(time.Unix(p.LastUpdateCheck, 0)) < updateAutoGap {
		return
	}
	st := checkUpdate(true)
	if !st.HasUpdate {
		return
	}
	logf("发现新版本 v%s（当前 v%s）", st.Latest, appVersion)
	if !autoApplyEnabled() {
		return
	}
	// 自动安装：这一步会关窗口、换 exe、重启自己，所以放在后台，别堵住启动流程。
	logf("自动安装已开启，开始下载 v%s", st.Latest)
	setApplying(true, "")
	latest := st.Latest
	go func() {
		if err := beginApply(latest); err != nil {
			logf("自动更新失败：%v", err)
			setApplying(false, err.Error())
			return
		}
		logf("已下载 v%s，正在替换程序并重启", latest)
	}()
}

// ── 版本比较 ────────────────────────────────────────────────────────

// truncateRunes 按字符（不是字节）截断，避免把一个汉字切成半个。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// versionLess a 是否比 b 旧。只比数字段，"1.0.2-beta" 按 1.0.2 算。
func versionLess(a, b string) bool {
	pa, pb := versionNums(a), versionNums(b)
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func versionNums(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	out := []int{}
	for _, seg := range strings.Split(v, ".") {
		digits := ""
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if c < '0' || c > '9' {
				break
			}
			digits += string(c)
		}
		if digits == "" {
			break
		}
		n, _ := strconv.Atoi(digits)
		out = append(out, n)
	}
	return out
}

// ── 下载与替换 ──────────────────────────────────────────────────────

func updateDir() string { return filepath.Join(baseDir(), "data", "update") }

// selfExePath 取当前正在运行的可执行文件。用 os.Executable 而不是「目录 + 固定名」：
// 用户把 exe 改了名也照样能更新到自己身上。
func selfExePath() string {
	if p, err := os.Executable(); err == nil && p != "" {
		return p
	}
	return filepath.Join(exeDir, updateExeName)
}

// downloadAndStage 下载安装包并取出里面的 exe，返回待替换的临时文件路径。
// 只有这一步会失败得比较频繁（网络、包损坏），所以放在响应之前同步做，
// 出错能立刻回给界面；换文件那步才放到响应之后。
func downloadAndStage() (string, error) {
	updMu.Lock()
	asset, url, want := updState.Asset, updAssetURL, updState.AssetSize
	latest := updState.Latest
	updMu.Unlock()

	if asset == "" || url == "" {
		return "", fmt.Errorf("更新源里没有 Windows 安装包，请稍后重试或到 Release 页手动下载")
	}
	dir := updateDir()
	ensureDir(dir)
	zipPath := filepath.Join(dir, asset)

	if err := downloadFile(url, zipPath, updateMaxZipBytes); err != nil {
		return "", err
	}
	if want > 0 {
		if st, err := os.Stat(zipPath); err == nil && st.Size() != want {
			return "", fmt.Errorf("下载不完整（%d/%d 字节），请重试", st.Size(), want)
		}
	}
	staged, err := extractExe(zipPath, dir)
	if err != nil {
		return "", err
	}
	_ = os.Remove(zipPath) // 装完就不留 4MB 的包在数据目录里
	logf("已下载 v%s 安装包并取出程序：%s", latest, filepath.Base(staged))
	return staged, nil
}

func downloadFile(url, dst string, limit int64) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "WorkBuddyOne/"+appVersion)
	resp, err := updateClient().Do(req)
	if err != nil {
		return fmt.Errorf("下载失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载失败：HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("写入临时文件失败：%w", err)
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit))
	if err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("下载中断：%w", err)
	}
	if n <= 0 {
		_ = os.Remove(dst)
		return fmt.Errorf("下载到 0 字节")
	}
	return nil
}

// extractExe 从 zip 里取出 WorkBuddyOne.exe。zip 每条记录都有 CRC，
// 解压过程天然完成完整性校验，所以拿出来的长度一定和打包时一致。
func extractExe(zipPath, dir string) (string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("安装包损坏或不是 zip：%w", err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if f.FileInfo().IsDir() || path.Base(f.Name) != updateExeName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("读取安装包失败：%w", err)
		}
		tmp := filepath.Join(dir, updateExeName+".new")
		out, err := os.Create(tmp)
		if err != nil {
			rc.Close()
			return "", err
		}
		n, cerr := io.Copy(out, rc)
		rc.Close()
		out.Close()
		if cerr != nil {
			_ = os.Remove(tmp)
			return "", fmt.Errorf("解压失败：%w", cerr)
		}
		if n < updateMinExeBytes {
			_ = os.Remove(tmp)
			return "", fmt.Errorf("安装包里的程序只有 %d 字节，不完整", n)
		}
		if err := checkPEFile(tmp); err != nil {
			_ = os.Remove(tmp)
			return "", err
		}
		return tmp, nil
	}
	return "", fmt.Errorf("安装包里没有 %s", updateExeName)
}

// checkPEFile 确认拿到的是一个 Windows 可执行文件，而不是错误页或残缺文件。
func checkPEFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	var h [2]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return fmt.Errorf("文件头读取失败：%w", err)
	}
	if h[0] != 'M' || h[1] != 'Z' {
		return fmt.Errorf("下载到的不是 Windows 程序（文件头 %q），可能被网络拦截了", string(h[:]))
	}
	return nil
}

// applyStaged 把暂存的新 exe 换成正式程序。
//
// 关键的 Windows 语义：正在运行的 exe 可以被改名，不能被覆盖。于是先把旧的
// 挪成 .old，再把新的挪到位。中途任何一步失败都回滚，绝不让用户处于
// 「exe 不见了」的状态。
func applyStaged(newExe string) error {
	target := selfExePath()
	old := target + ".old"
	_ = os.Remove(old)

	if err := os.Rename(target, old); err != nil {
		return fmt.Errorf("无法移动当前程序：%w（目录只读或被占用？）", err)
	}
	if err := os.Rename(newExe, target); err != nil {
		_ = os.Rename(old, target)
		return fmt.Errorf("替换失败：%w", err)
	}
	if err := startDetached(target); err != nil {
		// 新的起不来，把旧的放回去，至少还能用。
		_ = os.Rename(target, newExe)
		_ = os.Rename(old, target)
		return fmt.Errorf("启动新版本失败：%w", err)
	}
	return nil
}

// startDetached 拉起替换后的程序，并告知它是「接棒启动」——
// 旧进程此刻可能还在退出路上、端口没释放，它需要先等一等。
func startDetached(exe string) error {
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	cmd.Env = append(os.Environ(), updateHandoffEnv+"=1")
	return cmd.Start()
}

// handoffWait 接棒启动时先等端口释放，避免新版本因为「端口被占用」自杀。
// 最多等 15 秒，超时就照常往下走（宁可报端口占用，也不要静默不动）。
func handoffWait() {
	if os.Getenv(updateHandoffEnv) == "" {
		return
	}
	for i := 0; i < 60; i++ {
		if portFree(appPort) {
			logf("接棒启动：端口 %s 已空闲（等待 %d 次）", appPort, i)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	logf("接棒启动：等待端口 %s 超时，继续尝试启动", appPort)
}

// cleanupOldExe 删掉上一次更新留下的 .old。旧进程可能还没退干净，
// 所以失败就等一会儿再试，删不掉也无所谓——下次启动还会再试。
func cleanupOldExe() {
	old := selfExePath() + ".old"
	for i := 0; i < 20; i++ {
		err := os.Remove(old)
		if err == nil {
			logf("已清理更新备份 %s", filepath.Base(old))
			return
		}
		if os.IsNotExist(err) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func setApplying(on bool, errMsg string) {
	updMu.Lock()
	updState.Applying = on
	if errMsg != "" {
		updState.Error = errMsg
	}
	updMu.Unlock()
}

// beginApply 下载当前最新版并安排「关窗口 → 换 exe → 重启」。
//
// 同步做下载（失败能立刻返回给调用方），替换放到返回之后——那一步做完进程就没了，
// 没法再回话。返回时 staged 已落盘、替换任务已在后台排队。
func beginApply(latest string) error {
	staged, err := downloadAndStage()
	if err != nil {
		return err
	}
	go func() {
		// 留一点时间把响应和提示送出去，再关窗口。
		time.Sleep(1200 * time.Millisecond)
		if browser != nil && browser.Process != nil {
			_ = browser.Process.Kill()
		}
		if err := applyStaged(staged); err != nil {
			logf("自动更新失败：%v", err)
			setApplying(false, err.Error())
			return
		}
		logf("已更新到 v%s，交给新版本接棒", latest)
		// 不删 browserProfile：此刻目录还被系统占着，新进程启动时
		// cleanStaleBrowserProfiles 会收拾。端口随进程退出释放。
		os.Exit(0)
	}()
	return nil
}

// ── HTTP 接口 ───────────────────────────────────────────────────────

func registerUpdate(mux *http.ServeMux) {
	// 只读当前状态（含上次检查的缓存），界面刷新用，不触发网络。
	mux.HandleFunc("GET /accounts/api/update", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())
		writeJSON(w, currentUpdateState())
	})
	// 主动检查。
	mux.HandleFunc("POST /accounts/api/update/check", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())
		st := checkUpdate(true)
		if st.Error != "" {
			writeErr(w, st.Error)
			return
		}
		st.Current = appVersion
		st.AutoCheck = autoCheckEnabled()
		writeJSON(w, st)
	})
	// 下载 + 替换 + 重启。下载在本次响应里同步完成（失败能立刻回给界面），
	// 换文件放到响应之后——那一步做完进程就没了，没法再回话。
	mux.HandleFunc("POST /accounts/api/update/apply", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())

		updMu.Lock()
		if updState.Applying {
			updMu.Unlock()
			writeErr(w, "正在更新中，请稍候")
			return
		}
		if !updState.HasUpdate {
			updMu.Unlock()
			writeErr(w, "当前已经是最新版本")
			return
		}
		latest := updState.Latest
		updState.Applying = true
		updState.Error = ""
		updMu.Unlock()

		if err := beginApply(latest); err != nil {
			setApplying(false, err.Error())
			writeErr(w, err.Error())
			return
		}
		writeJSON(w, map[string]any{
			"ok":         true,
			"message":    "已下载 v" + latest + "，正在替换程序并重启",
			"restarting": true,
			"latest":     latest,
		})
	})
	// 打开/关闭「查到就自动安装」。打开后会连重启一起做掉，用户什么都不用点。
	mux.HandleFunc("POST /accounts/api/update/auto_apply", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())
		var req struct {
			Enabled bool `json:"enabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		p := readPrefs()
		p.UpdateAutoApply = req.Enabled
		// 自动安装依赖自动检查：只开安装在关掉检查的话，永远查不到、也就不会装。
		if req.Enabled {
			p.UpdateCheckOff = false
		}
		writePrefs(p)
		msg := "已关闭自动安装，更新改为提示后由你点"
		if req.Enabled {
			msg = "已开启自动安装：每天启动后查到新版本会直接更新并重启"
		}
		writeJSON(w, map[string]any{"ok": true, "message": msg, "enabled": req.Enabled})
	})
	// 关掉/打开每日自动检查。
	mux.HandleFunc("POST /accounts/api/update/auto", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())
		var req struct {
			Enabled bool `json:"enabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		p := readPrefs()
		p.UpdateCheckOff = !req.Enabled
		writePrefs(p)
		msg := "已关闭自动检查更新"
		if req.Enabled {
			msg = "已开启：每天启动后自动检查一次"
		}
		writeJSON(w, map[string]any{"ok": true, "message": msg, "enabled": req.Enabled})
	})
}
