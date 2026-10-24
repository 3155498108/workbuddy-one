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
	"context"
	"encoding/json"
	"errors"
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
	"sync/atomic"
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

	// updateHeaderTimeout 下载请求等响应头的上限（连不上/被拦要快速失败，别干等）。
	// 12 秒是权衡：国内直连 GitHub 失败时多半是「TCP 连上了但一直没响应」，
	// 等 20 秒太久，用户看着「下载 0%」会以为卡死；而真正能下通的源，
	// 首字节基本都在 3 秒内到。
	updateHeaderTimeout = 12 * time.Second
)

// 下面几个做成 var 而不是 const，是为了让测试能把「25 秒」压成毫秒级
// 去验证停滞检测和重试，否则跑一次测试要等半分钟。
var (
	// updateStallTimeout 下载期间「没有收到任何新字节」的上限。
	// 这里必须看进展、不能看总时长：GitHub 的资产走 objects.githubusercontent.com，
	// 国内经代理常见 100~300 KB/s，4 MB 包要几十秒。之前和查版本共用一个
	// Timeout=20s 的 client，等于给整个下载压了 20 秒死线，慢一点就报
	// 「context deadline exceeded ... while reading body」——v1.0.3 起的更新失败全是它。
	updateStallTimeout = 25 * time.Second
	updateTries        = 3
	updateRetryWait    = 3 * time.Second
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
	GotBytes  int64  `json:"got_bytes,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
	Checking  bool   `json:"checking"`
	Applying  bool   `json:"applying"`
	Error     string `json:"error,omitempty"`
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
	// updGot 本次下载已落盘的字节数。用原子变量单独存，是因为 io.Copy 的
	// 循环里每块都要更新，走 updMu 会把状态接口拖慢。
	updGot atomic.Int64
)

// ── 查询 ────────────────────────────────────────────────────────────

const (
	// updateDefaultFeed 官方默认源。匿名调用 api.github.com 是**按 IP 每小时
	// 60 次**，多开几次「检查更新」就会 403 rate limit——所以默认路径根本不
	// 碰这个接口，见 fetchRelease。
	updateDefaultFeed = "https://api.github.com/repos/" + updateRepo + "/releases/latest"
	// updateLatestPage 会 302 到 /releases/tag/vX.Y.Z。读这个跳转不消耗 API 配额。
	updateLatestPage = "https://github.com/" + updateRepo + "/releases/latest"
)

// updateMirrors 公共加速前缀。国内直连 github.com 常常连得上、但一跳到
// objects.githubusercontent.com 就卡死或只有几十 KB/s，所以直连失败（或太慢）
// 时必须能换路。这些反代用法统一是「前缀 + 完整原始地址」。
//
// 顺序按实测速度排（2026-10 测 v1.0.7 包，取首 1 MiB）：
// gh-proxy.com 666 KB/s、ghfast.top 416 KB/s、ghproxy.net 92 KB/s。
// github.moeyy.xyz 已失效（连接直接被拒），不要加回来。
// 不放心的源可以删掉，也可以用 prefs 的 update_mirror 或环境变量
// WBONE_UPDATE_MIRROR 指定自建源。
var updateMirrors = []string{
	"https://gh-proxy.com",
	"https://ghfast.top",
	"https://ghproxy.net",
}

// mirrorURL 把原始 GitHub 地址套到加速前缀上。空前缀 = 直连，原样返回。
func mirrorURL(prefix, raw string) string {
	prefix = strings.TrimRight(strings.TrimSpace(prefix), "/")
	if prefix == "" || !strings.Contains(prefix, "://") {
		return raw
	}
	return prefix + "/" + raw
}

// downloadSources 给出「下载这个资产」要依次尝试的完整地址。
// 上次成功的源排第一：国内一旦换到镜像，后续更新就不必再等直连超时一次。
func downloadSources(raw string) []string {
	out := make([]string, 0, len(updateMirrors)+2)
	seen := map[string]bool{}
	add := func(p string) {
		u := mirrorURL(p, raw)
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	if s := strings.TrimSpace(os.Getenv("WBONE_UPDATE_MIRROR")); s != "" {
		add(s)
	}
	if s := readPrefs().UpdateMirror; strings.TrimSpace(s) != "" {
		add(s)
	}
	add("") // 直连
	for _, m := range updateMirrors {
		add(m)
	}
	return out
}

// releasePages 查版本要依次尝试的页面地址（直连 + 各加速源）。
// 查版本同样是「国内不通」的重灾区，不能只赌 github.com。
func releasePages() []string {
	out := make([]string, 0, len(updateMirrors)+1)
	add := func(p string) {
		u := mirrorURL(p, updateLatestPage)
		for _, e := range out {
			if e == u {
				return
			}
		}
		out = append(out, u)
	}
	add("")
	for _, m := range updateMirrors {
		add(m)
	}
	return out
}

func updateFeedURL() string {
	if s := strings.TrimSpace(os.Getenv("WBONE_UPDATE_FEED")); s != "" {
		return s
	}
	if s := strings.TrimSpace(readPrefs().UpdateFeed); s != "" {
		return s
	}
	return updateDefaultFeed
}

// githubToken 可选的 GitHub token。配了就走认证配额（5000/小时），
// 不配也能用——默认路径压根不走 API。
func githubToken() string {
	if s := strings.TrimSpace(os.Getenv("WBONE_UPDATE_TOKEN")); s != "" {
		return s
	}
	return strings.TrimSpace(readPrefs().UpdateToken)
}

// updateClient 查 Release 元数据的 client。这一步只是拿几 KB 的 JSON，
// 20 秒总超时正好——连不上就该早点报错。
//
// **不要拿它去下载安装包**：Client.Timeout 计的是「整个请求」的时长（含读完 body），
// 慢速下载会被它从中间掐断。下载走 updateDownloadClient。
func updateClient() *http.Client { return &http.Client{Timeout: 20 * time.Second} }

// updateDownloadClient 下载安装包专用：不设总时长，改由两件事兜底——
// 响应头由 Transport.ResponseHeaderTimeout 管，读到一半卡住由 downloadAttempt
// 里的停滞检测管。这样「慢但活着」的下载能一直跑完，「卡死」的能在 25 秒内死掉。
func updateDownloadClient() *http.Client {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{Timeout: 0}
	}
	cp := tr.Clone()
	cp.ResponseHeaderTimeout = updateHeaderTimeout
	return &http.Client{Timeout: 0, Transport: cp}
}

// fetchRelease 取最新版本信息。
//
// 默认源**先走零配额路径**：读 github.com/releases/latest 的 302 Location 拿 tag，
// 再按固定命名规则拼出下载地址。全程不碰 api.github.com，也就不会被
// 「匿名每小时 60 次」的限流卡住（用户实际报过 403 rate limit）。
//
// 自定义 feed（update_feed / WBONE_UPDATE_FEED）仍按 GitHub API 的 JSON 形状解析。
func fetchRelease() (*ghRelease, error) {
	feed := updateFeedURL()
	if feed != updateDefaultFeed {
		return fetchReleaseAPI(feed)
	}
	rel, err := fetchReleaseFast(releasePages())
	if err == nil {
		return rel, nil
	}
	// 直连和所有加速源都没拿到（企业代理改写跳转、页面结构变了等）再走 API，
	// 这时才可能吃限流——但至少不是主路径。
	logf("查版本失败（直连 + %d 个加速源）：%v，改用 GitHub API", len(updateMirrors), err)
	return fetchReleaseAPI(feed)
}

// fetchReleaseFast 并发去问所有源，取窗口内版本号最大的那个。
//
// 为什么并发：国内直连 github.com 经常是「连接建立后干等」，串行试五个源
// 要等一分多钟，用户会以为按钮卡死了。
// 为什么取最大版本而不是取最先返回：公共加速源会缓存旧页面，先到的未必最新。
func fetchReleaseFast(pages []string) (*ghRelease, error) {
	type result struct {
		rel *ghRelease
		err error
	}
	ch := make(chan result, len(pages))
	for _, p := range pages {
		go func(p string) {
			rel, err := releaseFromRedirectURL(p)
			ch <- result{rel, err}
		}(p)
	}

	var best *ghRelease
	var lastErr error
	// 总窗口 12 秒：正常情况 1~3 秒就有应答，这里只是防止某个源吊着不回，
	// 把「检查更新」卡死。比单源的 10 秒超时略长，保证窗口关掉前每源都已出结果。
	timer := time.NewTimer(12 * time.Second)
	defer timer.Stop()
	for i := 0; i < len(pages); i++ {
		select {
		case r := <-ch:
			if r.err != nil {
				if lastErr == nil {
					lastErr = r.err
				}
				continue
			}
			if best == nil || newerTag(r.rel.Tag, best.Tag) {
				best = r.rel
			}
		case <-timer.C:
			i = len(pages) // 跳出：用已经拿到的
		}
	}
	if best != nil {
		return best, nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的更新源")
	}
	return nil, lastErr
}

// newerTag a 是否比 b 新。
func newerTag(a, b string) bool {
	return versionLess(strings.TrimPrefix(b, "v"), strings.TrimPrefix(a, "v"))
}

// releaseFromRedirect 用「releases/latest 的 302」拿 tag，零 API 配额。
func releaseFromRedirect() (*ghRelease, error) { return releaseFromRedirectURL(updateLatestPage) }

func releaseFromRedirectURL(page string) (*ghRelease, error) {
	req, err := http.NewRequest(http.MethodGet, page, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "WorkBuddyOne/"+appVersion)
	c := &http.Client{
		// 与 fetchReleaseFast 的窗口对齐：单源最多 10 秒，窗口 12 秒，
		// 保证窗口关掉之前每个源都已经给出结果（成功或超时）。
		// 国内不通的源永远是「连上了但不回」，多等没有意义。
		Timeout: 10 * time.Second,
		// 关键：不要跟随跳转，Location 里就有 tag。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 GitHub 失败：%w", err)
	}
	defer resp.Body.Close()

	if tag := tagFromReleaseURL(resp.Header.Get("Location")); tag != "" {
		return releaseForTag(tag), nil
	}
	// 有些代理会把 302 拆掉直接回 200 页面内容，再从正文里找一次。
	if resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		if tag := tagFromReleaseURL(string(body)); tag != "" {
			return releaseForTag(tag), nil
		}
		return nil, fmt.Errorf("GitHub 返回了页面但找不到版本号（HTTP %d）", resp.StatusCode)
	}
	return nil, fmt.Errorf("GitHub 未按预期跳转（HTTP %d）", resp.StatusCode)
}

// tagFromReleaseURL 从 "/releases/tag/v1.0.6" 这类地址（或整页 HTML）里抠出 tag。
func tagFromReleaseURL(s string) string {
	const marker = "/releases/tag/"
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	tag := s[i+len(marker):]
	if j := strings.IndexAny(tag, "\"?#/' "); j >= 0 {
		tag = tag[:j]
	}
	tag = strings.TrimSpace(tag)
	// 只接受 vX.Y.Z 形状，避免把 "latest" 之类当成版本。
	if !strings.HasPrefix(tag, "v") || len(versionNums(tag)) == 0 {
		return ""
	}
	return tag
}

// releaseForTag 由 tag 直接拼出资产地址。仓库的资产命名是固定的
// WorkBuddyOne-vX.Y.Z-win64.zip，所以不需要 API 告诉我们有哪些资产。
// Size 留 0：下载层会以 zip 的 CRC 兜底完整性，拿不到大小只是少一道预检。
func releaseForTag(tag string) *ghRelease {
	v := strings.TrimPrefix(tag, "v")
	asset := updateAssetPrefix + v + updateAssetSuffix
	return &ghRelease{
		Tag:  tag,
		Page: "https://github.com/" + updateRepo + "/releases/tag/" + tag,
		Assets: []ghAsset{{
			Name: asset,
			URL:  "https://github.com/" + updateRepo + "/releases/download/" + tag + "/" + asset,
		}},
	}
}

// fetchReleaseAPI 走 GitHub REST API（GitHub API 形状的自定义源也走这里）。
func fetchReleaseAPI(feed string) (*ghRelease, error) {
	req, err := http.NewRequest(http.MethodGet, feed, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "WorkBuddyOne/"+appVersion)
	if tok := githubToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := updateClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接更新源失败（检查网络或代理）：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		// 这基本只有一个原因：匿名接口按 IP 限流（60 次/小时）。
		// 说清楚怎么办，别把 GitHub 的英文原文糊到用户脸上。
		return nil, fmt.Errorf("GitHub 接口被限流了（匿名每小时 60 次，按 IP 算）。" +
			"稍等一小时再试即可，或在 data/prefs.json 里加 \"update_token\": \"<你的 GitHub token>\" 提高额度")
	}
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

	// 只有「确实没有新版本」才记检查时间。
	// 查到新版本却没装成（下载失败、进程被杀）时不记——否则要空等 24 小时
	// 才会重试，用户就永远卡在这个版本上了。
	if !updState.HasUpdate {
		p := readPrefs()
		p.LastUpdateCheck = time.Now().Unix()
		writePrefs(p)
	}
	return updState
}

func currentUpdateState() updateState {
	updMu.Lock()
	s := updState
	updMu.Unlock()
	s.Current = appVersion
	s.Feed = updateFeedURL()
	if s.Applying {
		s.GotBytes = updGot.Load()
	}
	return s
}

// autoCheckOnce 启动时的后台检查：24 小时内查过就跳过。
//
// 更新是**全自动**的：查到新版本直接下载、替换、重启，没有开关。
// （1.0.6 之前有个「自动安装」勾选，默认关。结果大部分人永远不勾，
// 于是一直停留在老版本上——而老版本的更新本身还是坏的，形成死循环。）
func autoCheckOnce() {
	if p := readPrefs(); p.LastUpdateCheck > 0 &&
		time.Since(time.Unix(p.LastUpdateCheck, 0)) < updateAutoGap {
		return
	}
	st := checkUpdate(true)
	if !st.HasUpdate {
		return
	}
	logf("发现新版本 v%s（当前 v%s），开始自动更新", st.Latest, appVersion)
	// 这一步会关窗口、换 exe、重启自己，所以放后台，别堵住启动流程。
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

	updGot.Store(0)
	used, err := downloadAnySource(url, zipPath, updateMaxZipBytes, want)
	if err != nil {
		return "", err
	}
	// 记住这次成功的源。国内一旦换到加速源，后面每次更新就不必再在
	// 直连上白等一轮超时——这个体验差别很大。
	if p := readPrefs(); p.UpdateMirror != mirrorOf(used, url) {
		p.UpdateMirror = mirrorOf(used, url)
		writePrefs(p)
	}
	staged, err := extractExe(zipPath, dir)
	if err != nil {
		return "", err
	}
	_ = os.Remove(zipPath) // 装完就不留 4MB 的包在数据目录里
	logf("已下载 v%s 安装包并取出程序：%s", latest, filepath.Base(staged))
	return staged, nil
}

// sourceLabel 给日志/报错用的源名字。直连和加速源在用户看来是两回事。
func sourceLabel(u, raw string) string {
	if p := mirrorOf(u, raw); p != "" {
		return "加速源 " + strings.TrimPrefix(p, "https://")
	}
	if host := hostOf(u); host != "" {
		return host
	}
	return "直连"
}

func hostOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if j := strings.IndexAny(u, "/?#"); j >= 0 {
		u = u[:j]
	}
	return u
}

// mirrorOf 由「实际下载地址」反解出用了哪个加速前缀；直连返回空串。
func mirrorOf(used, raw string) string {
	if used == raw {
		return ""
	}
	if strings.HasSuffix(used, "/"+raw) {
		return strings.TrimSuffix(used, "/"+raw)
	}
	return ""
}

// downloadAnySource 依次尝试「上次成功的源 → 直连 → 各加速源」，直到下完。
//
// 换源时一律删掉残包重下：不同源对 Range 的处理不一致，把一个源的下半截
// 拼到另一个源上，只会得到一个 CRC 对不上的包。
// 但同一个源内部如果已经下到字节再断，就走续传——那是「通、只是慢」的源，
// 换掉反而更亏。
func downloadAnySource(raw, dst string, limit, want int64) (string, error) {
	srcs := downloadSources(raw)
	var fails []string
	for i, u := range srcs {
		if i > 0 {
			_ = os.Remove(dst)
			logf("上一个下载源不可用，换 %s", sourceLabel(u, raw))
		}
		for try := 1; try <= updateTries; try++ {
			err := downloadOnce(u, dst, limit, want)
			if err == nil {
				if i > 0 {
					logf("已通过 %s 下载完成", sourceLabel(u, raw))
				}
				return u, nil
			}
			if !downloadRetriable(err) {
				fails = append(fails, sourceLabel(u, raw)+"："+err.Error())
				break // 这个源明确不行（4xx/内容不对），换下一个
			}
			// 一个字节都没下来 = 这个源根本连不上，别在它身上耗重试次数。
			if st, serr := os.Stat(dst); serr != nil || st.Size() == 0 {
				fails = append(fails, sourceLabel(u, raw)+"："+err.Error())
				break
			}
			if try == updateTries {
				fails = append(fails, sourceLabel(u, raw)+"："+err.Error())
				break
			}
			logf("%s 下载中断（%v），%v 后续传", sourceLabel(u, raw), err, updateRetryWait)
			time.Sleep(updateRetryWait)
		}
	}
	_ = os.Remove(dst)
	if len(fails) > 4 {
		fails = fails[:2] // 报错别把五个源的原因全糊上去，留最容易看懂的前两条
	}
	return "", fmt.Errorf("所有下载源都失败了（%s）。可以稍后重试，或到 Release 页手动下载",
		strings.Join(fails, "；"))
}

// ── 下载 ────────────────────────────────────────────────────────────

// downloadFatal 标记「重试也没意义」的失败：HTTP 4xx、包结构不对、
// 体积注定超上限。其余（连接断、卡住、下不全）都值得再来一次。
type downloadFatal struct{ err error }

func (e *downloadFatal) Error() string { return e.err.Error() }
func (e *downloadFatal) Unwrap() error { return e.err }

func fatalDownload(format string, a ...any) error {
	return &downloadFatal{fmt.Errorf(format, a...)}
}

func downloadRetriable(err error) bool {
	var f *downloadFatal
	return !errors.As(err, &f)
}

// downloadFile 把安装包下到 dst。
//
// 三条原则，对应以前踩过的坑：
//  1. 不设总时长。慢速下载是合法的，只有「停止收到数据」才算失败。
//  2. 断了续传。GitHub 资产走 objects.githubusercontent.com，国内一次下完
//     4 MB 并不容易，整包重来纯属浪费。
//  3. 字节数对不上就再来一轮。半包解包会以「安装包里没有 exe」这种
//     莫名其妙的形式暴露，不如在下载层就拦住。
func downloadFile(url, dst string, limit, want int64) error {
	var lastErr error
	for try := 1; try <= updateTries; try++ {
		if try > 1 {
			logf("下载未完成（%v），%v 后重试第 %d 次", lastErr, updateRetryWait, try-1)
			time.Sleep(updateRetryWait)
		}
		err := downloadOnce(url, dst, limit, want)
		if err == nil {
			return nil
		}
		lastErr = err
		if !downloadRetriable(err) {
			_ = os.Remove(dst)
			return err
		}
	}
	_ = os.Remove(dst) // 残包留着会被下一次当成续传起点，反而更坏
	return lastErr
}

func downloadOnce(url, dst string, limit, want int64) error {
	have := int64(0)
	if st, err := os.Stat(dst); err == nil {
		have = st.Size()
	}
	// 已有的部分不可能比目标还大——要么上次下坏了，要么尺寸对不上，一律重来。
	if have > limit || (want > 0 && have > want) {
		_ = os.Remove(dst)
		have = 0
	}
	if want > 0 && have == want {
		return nil // 上次其实已经下全了
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &downloadFatal{err}
	}
	req.Header.Set("User-Agent", "WorkBuddyOne/"+appVersion)
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := updateDownloadClient().Do(req)
	if err != nil {
		return fmt.Errorf("连接下载地址失败：%w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		// 续传，have 保持
	case http.StatusOK:
		have = 0 // 服务器不认 Range（或被代理改写成 200），只能从头写
	case http.StatusRequestedRangeNotSatisfiable:
		return nil // 手上的已经覆盖全部内容
	default:
		return fatalDownload("下载失败：HTTP %d", resp.StatusCode)
	}
	// 拿到的明显不是安装包（错误页、跳转页通常只有几百字节），别浪费时间重试。
	if want > 0 && resp.ContentLength > 0 && resp.ContentLength < want/2 {
		return fatalDownload("下载地址返回的内容只有 %d 字节（应为 %d），可能被网络拦截了",
			resp.ContentLength, want)
	}

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return &downloadFatal{fmt.Errorf("写入临时文件失败：%w", err)}
	}
	if have == 0 {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return &downloadFatal{fmt.Errorf("写入临时文件失败：%w", err)}
		}
	}
	if _, err := f.Seek(have, io.SeekStart); err != nil {
		f.Close()
		return &downloadFatal{fmt.Errorf("写入临时文件失败：%w", err)}
	}

	guard, stopGuard := newStallGuard(resp.Body, updateStallTimeout, cancel)
	n, cerr := io.Copy(f, io.LimitReader(guard, limit-have))
	stopGuard()
	got := have + n
	updGot.Store(got)

	// 先 Sync 再 Close：换 exe 之前数据必须真的落盘。
	if serr := f.Sync(); cerr == nil && serr != nil {
		cerr = serr
	}
	if clErr := f.Close(); cerr == nil && clErr != nil {
		cerr = clErr
	}

	if guard.hit.Load() {
		return fmt.Errorf("下载卡住了：%d 秒没有收到任何数据（已下 %d/%d 字节）",
			int(updateStallTimeout/time.Second), got, want)
	}
	if cerr != nil {
		return fmt.Errorf("下载中断：%w", cerr)
	}
	if got >= limit && (want <= 0 || got < want) {
		return fatalDownload("安装包超过 %d MB 上限，已放弃", limit>>20)
	}
	if want > 0 && got < want {
		return fmt.Errorf("下载不完整（%d/%d 字节）", got, want)
	}
	return nil
}

// stallGuard 给下载流加「停滞检测」。
//
// 为什么不用 http.Client.Timeout：那是整个请求的总时长，慢速但活着的下载会被
// 它从中间掐断，正是 v1.0.3 起更新失败的原因。Transport 也没有「读停滞」这个
// 粒度，所以自己盯最后一次收到数据的时刻。
type stallGuard struct {
	r    io.Reader
	hit  atomic.Bool
	last atomic.Int64
}

func newStallGuard(r io.Reader, stall time.Duration, onStall func()) (*stallGuard, func()) {
	g := &stallGuard{r: r}
	g.last.Store(time.Now().UnixNano())
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if time.Since(time.Unix(0, g.last.Load())) > stall {
					g.hit.Store(true)
					onStall() // 掐掉请求，让 io.Copy 立刻返回
					return
				}
			}
		}
	}()
	var once sync.Once
	return g, func() { once.Do(func() { close(stop) }) }
}

func (g *stallGuard) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	if n > 0 {
		g.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// ── 解包 ────────────────────────────────────────────────────────────

// zipBase 取 zip 条目里的文件名。
//
// 必须先把反斜杠换成斜杠：Windows 上不少打包工具（旧版 PowerShell 的
// Compress-Archive 是典型）用 `\` 当路径分隔符，而 path.Base 只认 `/`，
// 于是 "dir\WorkBuddyOne.exe" 会被当成一个「名字里带反斜杠的文件」，
// 和 WorkBuddyOne.exe 永远匹配不上——现象就是包明明是好的，却报
// 「安装包里没有 WorkBuddyOne.exe」。
func zipBase(name string) string {
	return path.Base(strings.ReplaceAll(name, `\`, "/"))
}

// extractExe 从 zip 里取出 WorkBuddyOne.exe。zip 每条记录都有 CRC，
// 解压过程天然完成完整性校验，所以拿出来的长度一定和打包时一致。
func extractExe(zipPath, dir string) (string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fatalDownload("安装包损坏或不是 zip：%w", err)
	}
	defer zr.Close()

	// 第一轮按名字找；顺带记下所有 .exe，供兜底用。
	var target *zip.File
	var exes []*zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := zipBase(f.Name)
		if strings.EqualFold(base, updateExeName) {
			target = f
			break
		}
		if strings.EqualFold(path.Ext(base), ".exe") {
			exes = append(exes, f)
		}
	}
	// 兜底：包里只有一个 exe 就用它。用户重新打包、顺手改了 exe 名的
	// 情况不该让更新彻底卡死——PE 头校验还在后面把关。
	if target == nil && len(exes) == 1 {
		target = exes[0]
		logf("安装包里没有 %s，但只有一个 exe（%s），按它处理", updateExeName, zipBase(target.Name))
	}
	if target == nil {
		if len(exes) > 1 {
			names := make([]string, 0, len(exes))
			for _, f := range exes {
				names = append(names, zipBase(f.Name))
			}
			return "", fatalDownload("安装包里有多个 exe（%s），不确定该用哪个", strings.Join(names, "、"))
		}
		return "", fatalDownload("安装包里没有 %s（包内只有：%s）", updateExeName, zipListing(zr))
	}

	rc, err := target.Open()
	if err != nil {
		return "", fmt.Errorf("读取安装包失败：%w", err)
	}
	defer rc.Close()

	tmp := filepath.Join(dir, updateExeName+".new")
	out, err := os.Create(tmp)
	if err != nil {
		return "", &downloadFatal{err}
	}
	n, cerr := io.Copy(out, rc)
	if cerr == nil {
		cerr = out.Sync()
	}
	if clErr := out.Close(); cerr == nil {
		cerr = clErr
	}
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

// zipListing 把包内条目压成一行人话，塞进报错里。
// 以后再遇到「包里没找到 exe」，日志本身就能说明拿到的是什么。
func zipListing(zr *zip.ReadCloser) string {
	const max = 6
	names := make([]string, 0, max)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if len(names) == max {
			names = append(names, "…")
			break
		}
		names = append(names, zipBase(f.Name))
	}
	if len(names) == 0 {
		return "空包"
	}
	return strings.Join(names, "、")
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
	// 备份名必须唯一，不能固定用 ".old"。
	//
	// 固定名撞过一个很隐蔽的状态：上一次更新把「正在运行的 exe」改名成 .old
	// 并删除之后，这个名字处于 delete-pending（进程映像还被引用着）——目录里
	// 看不见它，但它既不能被创建，也不能作为 MOVEFILE_REPLACE_EXISTING 的目标。
	// 于是这次的 os.Rename 直接返回 Access is denied，更新原地失败。
	// 用户看到的就是「点更新 → 窗口没了 → 程序再也没起来」。
	// 实测日志：11:24:04「无法移动当前程序：rename ... Access is denied」。
	old := backupPath(target)

	if err := os.Rename(target, old); err != nil {
		return fmt.Errorf("无法移动当前程序（目录只读，或被杀毒软件锁住）：%w", err)
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

// backupPath 给「被替换掉的旧 exe」起一个绝不会重名的备份名。
//
// 为什么不能用固定名 ".old"：上一次更新把正在运行的 exe 改名成 .old 并删除后，
// 那个名字会停在 delete-pending 状态（进程映像还被引用）。目录里看不到它，
// 但它既不能被创建、也不能作为 MOVEFILE_REPLACE_EXISTING 的目标 —— 于是
// 下一次 rename 直接 Access is denied。pid + 自增序号保证永不撞名，
// 而 cleanupOldExe 用 "*.old-*" 通配统一清理。
func backupPath(target string) string {
	return fmt.Sprintf("%s.old-%d-%d", target, os.Getpid(), updBackupSeq.Add(1))
}

var updBackupSeq atomic.Int64

// cleanupOldExe 删掉上一次更新留下的备份。
// 旧进程可能还没退干净，所以失败就等一会儿再试，删不掉也无所谓——下次启动还会再试。
func cleanupOldExe() {
	target := selfExePath()
	// 1.0.7 及以前用固定名 .old；1.0.8 起带时间戳后缀。两种都清。
	candidates := []string{target + ".old"}
	if m, err := filepath.Glob(target + ".old-*"); err == nil {
		candidates = append(candidates, m...)
	}
	for _, old := range candidates {
		for i := 0; i < 20; i++ {
			err := os.Remove(old)
			if err == nil {
				logf("已清理更新备份 %s", filepath.Base(old))
				break
			}
			if os.IsNotExist(err) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
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
		// 留一点时间把响应和提示送出去。
		time.Sleep(1200 * time.Millisecond)

		// 顺序是刻意的：**先换文件并拉起新进程，成功了才关窗口**。
		// 以前是先 Kill 浏览器再换文件，于是换文件一失败就变成
		// 「窗口没了、进程也被窗口退出回调带走、新版本根本没起来」——
		// 用户看到的就是「更新完就再也不启动了」。失败时必须原样留着窗口。
		if err := applyStaged(staged); err != nil {
			logf("自动更新失败：%v", err)
			setApplying(false, err.Error())
			_ = os.Remove(staged) // 残包不留，下次重下
			return
		}
		logf("已更新到 v%s，新版本已拉起，关闭旧窗口", latest)
		if browser != nil && browser.Process != nil {
			_ = browser.Process.Kill()
		}
		// 等窗口退出回调打完日志再走；端口随本进程退出释放，新进程在 handoffWait 里等它。
		time.Sleep(400 * time.Millisecond)
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
}
