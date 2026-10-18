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
		}
		if _, ok := live[uid]; ok {
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
