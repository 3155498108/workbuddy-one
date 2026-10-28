// accounts.go 客户端登录态的扫描、快照与切换。
//
// 只搬文件、不解密：客户端把每条产品线的登录态存在
//
//	%LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\<product>.info
//
// 其中 account.uid / uin 是明文，accessToken / refreshToken / nickname 是客户端
// 自己的 AES-GCM 信封。切号 = 把某账号的快照整份覆盖回去，令牌原样搬运。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// authDirRel 客户端登录态目录（相对 LOCALAPPDATA）。
const authDirRel = `CodeBuddyExtension\Data\Public\auth`

// accountSnapshotRel 客户端主账号快照：唯一能拿到明文昵称的地方。
const accountSnapshotRel = `.workbuddy\storage\skeleton\account-snapshot.json`

// primaryProduct 主客户端的登录态文件名（不含 .info）。判定「当前账号」时以它为准：
// 客户端真正在用的就是这条线路，其它受管线路可能是历史残留或旁支。
const primaryProduct = "workbuddy-desktop"

var clientCandidates = []string{
	`E:\WorkBuddy\WorkBuddy.exe`,
	`C:\Program Files\WorkBuddy\WorkBuddy.exe`,
	`C:\Program Files (x86)\WorkBuddy\WorkBuddy.exe`,
}

func localAppData() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return v
	}
	return filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
}

func authDir() string              { return filepath.Join(localAppData(), authDirRel) }
func profilesDir() string          { return filepath.Join(baseDir(), "profiles") }
func backupDir() string            { return filepath.Join(baseDir(), "backup") }
func profileDir(uid string) string { return filepath.Join(profilesDir(), uid) }
func metaPath(uid string) string   { return filepath.Join(profileDir(uid), "meta.json") }
func snapPath(uid, product string) string {
	return filepath.Join(profileDir(uid), product+".info")
}

// ── 登录态文件读写 ──────────────────────────────────────────────────

// prodFile 是一条产品线的当前登录态文件。
type prodFile struct {
	Product string // 文件名去掉 .info，如 workbuddy-desktop
	Path    string
	UID     string
	UIN     string
	Domain  string
}

func (p prodFile) label() string {
	switch {
	case strings.Contains(p.Domain, "workbuddy.ai"):
		return "国际版"
	case strings.Contains(p.Domain, "workbuddy.cn"):
		return "国内版"
	case p.Product == "workbuddy-desktop":
		return "主客户端"
	default:
		return p.Product
	}
}

// readSession 只取明文 uid/uin/domain，不动加密令牌。
func readSession(path string) (uid, uin, domain string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", err
	}
	var doc struct {
		Account struct {
			UID string `json:"uid"`
			UIN string `json:"uin"`
		} `json:"account"`
		Auth struct {
			Domain string `json:"domain"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", "", "", fmt.Errorf("登录态文件不是合法 JSON: %w", err)
	}
	if doc.Account.UID == "" {
		return "", "", "", fmt.Errorf("登录态文件里没有 account.uid")
	}
	return doc.Account.UID, doc.Account.UIN, doc.Auth.Domain, nil
}

// managed 只管理国内版线路；国际版（workbuddy-desktop-ai / *.workbuddy.ai）不碰。
func (p prodFile) managed() bool {
	return !strings.HasSuffix(p.Product, "-ai") && !strings.Contains(p.Domain, "workbuddy.ai")
}

// scanProducts 列出 auth 目录下所有受管产品线的登录态。
func scanProducts() []prodFile {
	entries, err := os.ReadDir(authDir())
	if err != nil {
		return nil
	}
	out := []prodFile{}
	for _, e := range entries {
		name := e.Name()
		// 形如 xxx.info.xxxx.observation.json 的旁路文件要跳过
		if e.IsDir() || !strings.HasSuffix(name, ".info") || strings.Contains(name, ".info.") {
			continue
		}
		path := filepath.Join(authDir(), name)
		uid, uin, domain, err := readSession(path)
		if err != nil || uid == "" {
			continue
		}
		p := prodFile{
			Product: strings.TrimSuffix(name, ".info"),
			Path:    path,
			UID:     uid,
			UIN:     uin,
			Domain:  domain,
		}
		if !p.managed() {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Product < out[j].Product })
	return out
}

// activeLine 返回权威的「当前账号」：以主客户端线路为准，没有主线路时才退回
// 其它受管线路（scanProducts 已按文件名排序，取第一条保证结果稳定）。
//
// 为什么要「权威」而不是「任一线路」：切号历史残留会让多条线路各指向不同账号。
// 那时「任一命中」既会把两个账号都标成正在使用，也会让切换前置检查误判成
// 「你已经在那个账号上」而直接拒绝切换——issue #1 的两个症状都来自这里。
func activeLine() (uid, product string) {
	var fallback prodFile
	haveFallback := false
	for _, p := range scanProducts() {
		if p.Product == primaryProduct {
			return p.UID, p.Product
		}
		if !haveFallback {
			fallback, haveFallback = p, true
		}
	}
	if haveFallback {
		return fallback.UID, fallback.Product
	}
	return "", ""
}

// sameAccount 判断切换目标是否就是当前账号。口径必须与界面上的「正在使用」
// 一致：目标不是权威线路上的那个，就真的切一次（顺带清掉残留线路）。
func sameAccount(uid string) bool {
	cur, _ := activeLine()
	return cur != "" && cur == uid
}

func copyFileAtomic(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return moveReplace(tmp, dst)
}

// moveReplace 用 MoveFileEx 覆盖式移动（os.Rename 在目标存在时会失败）。
func moveReplace(src, dst string) error {
	srcp, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	dstp, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")
	const replaceExisting = 0x1
	const writeThrough = 0x8
	r, _, e := proc.Call(uintptr(unsafe.Pointer(srcp)), uintptr(unsafe.Pointer(dstp)),
		uintptr(replaceExisting|writeThrough))
	if r == 0 {
		if err2 := os.Rename(src, dst); err2 == nil {
			return nil
		}
		return fmt.Errorf("替换文件失败: %v", e)
	}
	return nil
}

// primaryIdentity 读当前主账号的明文 uid 与昵称。
func primaryIdentity() (uid, nickname string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	raw, err := os.ReadFile(filepath.Join(home, accountSnapshotRel))
	if err != nil {
		return "", ""
	}
	var doc struct {
		Primary struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
		} `json:"primary"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return "", ""
	}
	return doc.Primary.UID, doc.Primary.Nickname
}

// ── 账号档案 ────────────────────────────────────────────────────────

type meta struct {
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname"`
	UIN      string    `json:"uin,omitempty"`
	SavedAt  time.Time `json:"saved_at"`
}

func writeMeta(m meta) {
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = os.MkdirAll(profileDir(m.UID), 0o755)
	_ = os.WriteFile(metaPath(m.UID), b, 0o600)
}

func readMeta(uid string) meta {
	var m meta
	if b, err := os.ReadFile(metaPath(uid)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	m.UID = uid
	return m
}

// snapshotCred 登录态文件里**明文可读**的几个时间戳（token 本身是客户端 AES 信封，
// 我们解不开，也不需要解开——只看时间）。
//
// 关键字段是 refreshExpiresAt：客户端的 refresh token 是「30 天滚动」的，
// 每成功续期一次就重置为「当时 + 30 天」。所以它能回答用户最关心的问题——
// 这个账号还有多久会要求重新登录。
type snapshotCred struct {
	AccessExpiresAt  int64 `json:"-"`
	RefreshExpiresAt int64 `json:"-"`
	LastRefreshAt    int64 `json:"-"`
}

// credOfPath 读一份 .info 的明文时间戳。文件可能是客户端没登录的空壳，读不到就返回零值。
func credOfPath(path string) snapshotCred {
	raw, err := os.ReadFile(path)
	if err != nil {
		return snapshotCred{}
	}
	var doc struct {
		Auth struct {
			ExpiresAt        int64 `json:"expiresAt"`
			RefreshExpiresAt int64 `json:"refreshExpiresAt"`
			LastRefreshTime  int64 `json:"lastRefreshTime"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return snapshotCred{}
	}
	return snapshotCred{
		AccessExpiresAt:  doc.Auth.ExpiresAt,
		RefreshExpiresAt: doc.Auth.RefreshExpiresAt,
		LastRefreshAt:    doc.Auth.LastRefreshTime,
	}
}

// msToSec 客户端写的是毫秒，统一换成秒给前端。0 保持 0（= 未知）。
func msToSec(ms int64) int64 {
	if ms <= 0 {
		return 0
	}
	return ms / 1000
}

// syncActiveSnapshots 把当前活动登录态同步进对应账号的快照，返回更新了几份。
//
// 为什么必须做这件事：客户端每次刷新 token 都会换一份新的 refresh token（旧的那份
// 随即作废），但它只把新的写进「活动」的 <product>.info，不会写我们的快照。
// 于是快照里的 token 会越来越旧，等切回该账号时客户端拿旧 token 去续期，
// 上游直接拒绝——用户看到的就是「切过去要求重新登录」。
//
// 判断依据用 refreshExpiresAt：只有活动文件比快照「更新」（到期时间更晚）才覆盖，
// 避免把一份已经失效的登录态覆盖到还有效的快照上。
func syncActiveSnapshots() int {
	n := 0
	for _, f := range scanProducts() {
		dst := snapPath(f.UID, f.Product)
		live := credOfPath(f.Path)
		if live.RefreshExpiresAt <= 0 {
			continue // 活动文件没有有效凭证，别污染快照
		}
		old := credOfPath(dst)
		if old.RefreshExpiresAt >= live.RefreshExpiresAt {
			continue // 快照已经一样新或更新
		}
		if err := os.MkdirAll(profileDir(f.UID), 0o755); err != nil {
			continue
		}
		if err := copyFileAtomic(f.Path, dst); err != nil {
			continue
		}
		n++
	}
	return n
}

// snapshotsOf 返回该账号保存过快照的产品线集合。
func snapshotsOf(uid string) []string {
	entries, err := os.ReadDir(profileDir(uid))
	if err != nil {
		return nil
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".info") {
			out = append(out, strings.TrimSuffix(e.Name(), ".info"))
		}
	}
	sort.Strings(out)
	return out
}

// accounts 汇总所有已保存的账号（一个 uid 一条），附带它覆盖的产品线。
func accounts() []accountResp {
	entries, err := os.ReadDir(profilesDir())
	if err != nil {
		return nil
	}
	live := map[string]prodFile{}
	labels := map[string]string{}
	for _, p := range scanProducts() {
		live[p.UID] = p
		labels[p.UID] = p.label()
	}
	// 当前账号只认一条权威线路（见 activeLine）：早期版本切号只覆盖「目标账号
	// 有快照的那些线路」，会在 auth 目录里留下指向旧账号的活动文件。那时这里用
	// map[uid] 收集，任一线路命中就标 active，于是两个账号都显示「正在使用」——
	// 前端对 active 的账号禁用按钮，用户切过去就切不回来（issue #1）。
	currentUID, _ := activeLine()

	out := []accountResp{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		uid := e.Name()
		prods := snapshotsOf(uid)
		if len(prods) == 0 {
			continue
		}
		var minExp int64
		m := readMeta(uid)
		if m.Nickname == "" {
			m.Nickname = "账号 " + shortUID(uid)
		}
		item := accountResp{
			UID:      uid,
			Nickname: m.Nickname,
			UIN:      m.UIN,
			Lines:    []string{},
		}
		if !m.SavedAt.IsZero() {
			item.SavedAt = m.SavedAt.Format(time.RFC3339)
		}
		for _, p := range prods {
			if l, ok := labels[p]; ok {
				item.Lines = append(item.Lines, l)
			} else if strings.Contains(p, "ai") {
				item.Lines = append(item.Lines, "国际版")
			} else {
				item.Lines = append(item.Lines, "国内版")
			}
			// 取所有线路里最早到期的那个作为整个账号的到期时间：
			// 只要有一条线路快过期，这个号就快需要重新登录了。
			c := credOfPath(snapPath(uid, p))
			if c.RefreshExpiresAt > 0 && (minExp == 0 || c.RefreshExpiresAt < minExp) {
				minExp = c.RefreshExpiresAt
			}
			if sec := msToSec(c.LastRefreshAt); sec > item.LastRefreshAt {
				item.LastRefreshAt = sec
			}
		}
		item.ExpiresAt = msToSec(minExp)
		if uid == currentUID {
			item.Active = true
			if item.UIN == "" {
				item.UIN = live[uid].UIN
			}
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		return out[i].SavedAt > out[j].SavedAt
	})
	return out
}

// saveCurrent 把每条产品线当前的登录态都存成快照。
func saveCurrent() (int, error) {
	files := scanProducts()
	if len(files) == 0 {
		return 0, fmt.Errorf("找不到客户端登录态，请先登录 WorkBuddy")
	}
	primUID, primName := primaryIdentity()
	n := 0
	for _, f := range files {
		if err := os.MkdirAll(profileDir(f.UID), 0o755); err != nil {
			continue
		}
		if err := copyFileAtomic(f.Path, snapPath(f.UID, f.Product)); err != nil {
			continue
		}
		m := readMeta(f.UID)
		m.UID = f.UID
		m.UIN = f.UIN
		m.SavedAt = time.Now()
		// 只有当前主账号能拿到明文昵称，其它线路保留已有名字
		if f.UID == primUID && primName != "" {
			m.Nickname = primName
		}
		if m.Nickname == "" {
			m.Nickname = "账号 " + shortUID(f.UID)
		}
		writeMeta(m)
		n++
	}
	return n, nil
}

// switchTo 用该账号的快照覆盖它所属的产品线；覆盖前先备份并保存当前登录态。
func switchTo(uid string) error {
	prods := snapshotsOf(uid)
	if len(prods) == 0 {
		return fmt.Errorf("这个账号没有保存过快照")
	}
	have := make(map[string]bool, len(prods))
	for _, p := range prods {
		have[p] = true
	}

	_ = os.MkdirAll(backupDir(), 0o755)
	stamp := time.Now().Format("20060102-150405")
	for _, p := range scanProducts() {
		_ = copyFileAtomic(p.Path, filepath.Join(backupDir(), p.Product+"-"+stamp+".info"))
	}
	_, _ = saveCurrent() // 正在用的账号先存好，避免丢号

	for _, prod := range prods {
		live := filepath.Join(authDir(), prod+".info")
		if err := copyFileAtomic(snapPath(uid, prod), live); err != nil {
			return fmt.Errorf("写入 %s 失败：%w", prod, err)
		}
	}

	// 目标账号没有快照的受管线路：把活动文件挪进 backup，绝不能留在原地。
	//
	// 留着的后果就是 issue #1：auth 目录里两条线路各指向不同账号，accounts() 把
	// 两个都标成「正在使用」，前端把两个按钮全禁掉 —— 用户切过去就切不回来，
	// 只能去客户端手动退出登录。
	//
	// 为什么是挪走而不是删掉：万一那条线路用户真的在另一个客户端上用着
	// （国内版 + 国际版并存是合法场景），删了就丢登录态；挪进 backup 还能捞回来。
	// 代价是那个客户端会要求重新登录一次——比整个切换界面卡死好。
	for _, p := range scanProducts() {
		if have[p.Product] || p.UID == uid {
			continue
		}
		stale := filepath.Join(backupDir(), p.Product+"-stale-"+stamp+".info")
		if err := moveReplace(p.Path, stale); err != nil {
			// 已经切好了，这里失败不该整体回滚；记一笔让日志能查到余下的隐患。
			logf("切换后未能挪走线路 %s（仍指向 %s）：%v", p.Product, shortUID(p.UID), err)
			continue
		}
		logf("切换后已挪走线路 %s（原指向 %s），备份在 backup/", p.Product, shortUID(p.UID))
	}
	return nil
}

// ── 客户端控制 ──────────────────────────────────────────────────────

func findClient() string {
	for _, p := range clientCandidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// runCmd 启动外部命令并隐藏控制台窗口。
// 本程序是无控制台 GUI 进程，直接 exec tasklist/taskkill 会额外弹出黑框控制台。
func runCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd
}

func clientRunning() bool {
	out, err := runCmd("tasklist", "/FI", "IMAGENAME eq WorkBuddy.exe", "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "workbuddy.exe")
}

func killClient() {
	_ = runCmd("taskkill", "/F", "/IM", "WorkBuddy.exe").Run()
	for i := 0; i < 40; i++ {
		if !clientRunning() {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func restartClient() error {
	exe := findClient()
	if exe == "" {
		return fmt.Errorf("没找到 WorkBuddy.exe，请手动启动客户端")
	}
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	return cmd.Start()
}

func shortUID(uid string) string {
	if len(uid) >= 8 {
		return uid[:8]
	}
	return uid
}

// ── 登录态寿命 ──────────────────────────────────────────────────────

// credOfAccount 汇总该账号所有线路里「最早到期」和「最近一次续期」的时间戳（毫秒）。
func credOfAccount(uid string) (lastRefresh, expires int64) {
	for _, p := range snapshotsOf(uid) {
		c := credOfPath(snapPath(uid, p))
		if c.LastRefreshAt > lastRefresh {
			lastRefresh = c.LastRefreshAt
		}
		if c.RefreshExpiresAt > 0 && (expires == 0 || c.RefreshExpiresAt < expires) {
			expires = c.RefreshExpiresAt
		}
	}
	return lastRefresh, expires
}

// credNote 给切换结果加一句登录态寿命说明。
//
// 为什么要在切号时就说：客户端每次续期都会换新 refresh token 并把旧的作废，
// 而快照只能记住「上次在客户端里跑过时」那一份。所以一个放了几天没动的账号，
// 切过去很可能直接被要求重新登录——这不是时间到了，是旧 token 被换掉了。
// 提前把天数说出来，用户就不会觉得「明明没过期怎么又要登录」。
func credNote(uid string) string {
	last, exp := credOfAccount(uid)
	if last <= 0 {
		return ""
	}
	idleDays := int(time.Since(time.UnixMilli(last)).Hours() / 24)
	if idleDays < 1 {
		return "" // 刚续过期，没什么好提醒的
	}
	note := fmt.Sprintf("（该账号上次续期是 %d 天前", idleDays)
	if exp > 0 {
		d := time.Until(time.UnixMilli(exp))
		switch {
		case d <= 0:
			note += "，登录态已过期"
		case d < 24*time.Hour:
			// 不能写成「还剩 0 天」：整数除法把不足一天截成 0，读起来像没过期。
			note += "，登录态不到 1 天了"
		default:
			note += fmt.Sprintf("，登录态还剩 %d 天", int(d.Hours()/24))
		}
	}
	note += "；万一被要求重新登录，登一次就会重新计时）"
	return note
}

// credWarnShort 报告该账号是否「快要（或已经）需要重新登录」。
// 界面拿它给卡片标黄/标红，阈值取 7 天：客户端续期窗口是 30 天，
// 7 天足够用户挑个时间切过去跑一次。
func credWarnShort(uid string) (days int, known bool) {
	_, exp := credOfAccount(uid)
	if exp <= 0 {
		return 0, false
	}
	return int(time.Until(time.UnixMilli(exp)).Hours() / 24), true
}

// syncSnapshotsSoon 在客户端刚启动并刷完 token 之后，把活动登录态同步进快照。
//
// 时序很重要：切号是「覆盖 .info → 启动客户端」，客户端启动后才用它自己的
// 流程续期，续期成功会把**新的** refresh token 写回活动 .info。那一刻我们的
// 快照就落后了——如果用户下次切回来之前没再切走（切走时会 saveCurrent），
// 就会拿到旧 token。所以这里在启动后补两次同步。
func syncSnapshotsSoon() {
	for _, d := range []time.Duration{8 * time.Second, 40 * time.Second} {
		time.Sleep(d)
		if n := syncActiveSnapshots(); n > 0 {
			logf("已同步 %d 份账号快照（客户端续期后的最新登录态）", n)
		}
	}
}

// refreshSnapshotLoop 管家运行期间持续跟踪登录态。
//
// 3 分钟一次，每次只读几个几 KB 的 JSON——客户端不会更勤地续期
// （access token 有效期是 28 天，它重启才刷），所以没必要更频繁。
func refreshSnapshotLoop() {
	if n := syncActiveSnapshots(); n > 0 {
		logf("已同步 %d 份账号快照", n)
	}
	t := time.NewTicker(3 * time.Minute)
	defer t.Stop()
	for range t.C {
		if n := syncActiveSnapshots(); n > 0 {
			logf("已同步 %d 份账号快照（登录态已续期）", n)
		}
	}
}
