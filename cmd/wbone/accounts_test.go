package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupAuthDirs 把 LOCALAPPDATA 和程序目录都指到临时目录，
// 让 authDir()/profilesDir() 落进沙箱。
func setupAuthDirs(t *testing.T) (authD, baseD string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	old := exeDir
	exeDir = root
	t.Cleanup(func() { exeDir = old })
	authD = authDir()
	if err := os.MkdirAll(authD, 0o755); err != nil {
		t.Fatal(err)
	}
	return authD, root
}

// writeInfo 造一份登录态文件。refreshIn 是 refresh token 还能活多久，
// lastIn 是上次续期距离现在多久（都用「现在 ± 时长」表示）。
func writeInfo(t *testing.T, path, uid string, refreshIn, lastIn time.Duration) {
	t.Helper()
	now := time.Now()
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":      "enc-access",
			"refreshToken":     "enc-refresh",
			"expiresAt":        now.Add(refreshIn).UnixMilli(),
			"refreshExpiresAt": now.Add(refreshIn).UnixMilli(),
			"lastRefreshTime":  now.Add(-lastIn).UnixMilli(),
			"domain":           "www.workbuddy.cn",
		},
		"account": map[string]any{"uid": uid, "uin": "123"},
	}
	b, _ := json.Marshal(doc)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCredOfPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "workbuddy-desktop.info")
	writeInfo(t, p, "uid-1", 30*24*time.Hour, 2*24*time.Hour)

	c := credOfPath(p)
	if c.RefreshExpiresAt <= 0 || c.LastRefreshAt <= 0 {
		t.Fatalf("时间戳没解析出来：%+v", c)
	}
	// 客户端写的是毫秒；这里只校验量级，避免时区/精度干扰。
	if got := time.Until(time.UnixMilli(c.RefreshExpiresAt)); got < 29*24*time.Hour || got > 31*24*time.Hour {
		t.Fatalf("到期时间偏差过大：%v", got)
	}

	// 打不开 / 不是 JSON 的文件必须安静地返回零值，不能 panic、不能当成有效凭证。
	if got := credOfPath(filepath.Join(dir, "nope.info")); got != (snapshotCred{}) {
		t.Fatalf("不存在的文件应返回零值，实际 %+v", got)
	}
	bad := filepath.Join(dir, "bad.info")
	_ = os.WriteFile(bad, []byte("not json"), 0o644)
	if got := credOfPath(bad); got != (snapshotCred{}) {
		t.Fatalf("坏文件应返回零值，实际 %+v", got)
	}
}

// TestSyncActiveSnapshots 登录态同步的两个方向都要对：
// 活动文件更新 → 覆盖快照；快照反而更新 → 绝对不能拿旧的盖掉。
func TestSyncActiveSnapshots(t *testing.T) {
	authD, baseD := setupAuthDirs(t)
	const uid = "6c19d937-6b93-4939-9499-2e18ec8bc3ce"
	live := filepath.Join(authD, "workbuddy-desktop.info")
	snap := snapPath(uid, "workbuddy-desktop")

	// 快照只有 10 天，活动文件已经续到 30 天 → 应覆盖
	writeInfo(t, snap, uid, 10*24*time.Hour, 20*24*time.Hour)
	writeInfo(t, live, uid, 30*24*time.Hour, 0)

	if n := syncActiveSnapshots(); n != 1 {
		t.Fatalf("应同步 1 份，实际 %d", n)
	}
	if c := credOfPath(snap); time.Until(time.UnixMilli(c.RefreshExpiresAt)) < 29*24*time.Hour {
		t.Fatalf("快照没有被更新到最新：%v", time.Until(time.UnixMilli(c.RefreshExpiresAt)))
	}

	// 反过来：快照 40 天、活动文件 30 天（客户端重启后反而短了）→ 必须保留更长的
	writeInfo(t, snap, uid, 40*24*time.Hour, 0)
	if n := syncActiveSnapshots(); n != 0 {
		t.Fatalf("不应覆盖更长的登录态，实际同步了 %d 份", n)
	}
	if c := credOfPath(snap); time.Until(time.UnixMilli(c.RefreshExpiresAt)) < 39*24*time.Hour {
		t.Fatal("快照被旧的登录态覆盖了")
	}

	// 活动文件没有有效凭证（客户端正在登录页）时，不能把快照冲掉
	writeInfo(t, live, uid, 0, 0)
	if n := syncActiveSnapshots(); n != 0 {
		t.Fatalf("空凭证不应触发同步，实际 %d", n)
	}
	_ = baseD
}

// TestSyncActiveSnapshotsCreatesProfile 首次同步要能自己建 profiles/<uid>/。
func TestSyncActiveSnapshotsCreatesProfile(t *testing.T) {
	authD, _ := setupAuthDirs(t)
	const uid = "c1b5a10a-1b99-4ac1-bfd6-3d918e6a5153"
	writeInfo(t, filepath.Join(authD, "workbuddy-desktop.info"), uid, 30*24*time.Hour, 0)

	if n := syncActiveSnapshots(); n != 1 {
		t.Fatalf("应同步 1 份，实际 %d", n)
	}
	if _, err := os.Stat(snapPath(uid, "workbuddy-desktop")); err != nil {
		t.Fatalf("快照没生成：%v", err)
	}
}

// TestCredNote 多久没续期要能如实说出来；刚续期的不要打扰用户。
func TestCredNote(t *testing.T) {
	_, _ = setupAuthDirs(t)
	const uid = "aaaa1111-2222-3333-4444-555566667777"
	_ = os.MkdirAll(profileDir(uid), 0o755)

	writeInfo(t, snapPath(uid, "workbuddy-desktop"), uid, 28*24*time.Hour, 2*24*time.Hour)
	note := credNote(uid)
	if !strings.Contains(note, "2 天前") {
		t.Fatalf("应说明 2 天前续期，实际：%q", note)
	}
	if !strings.Contains(note, "还剩 27 天") && !strings.Contains(note, "还剩 28 天") {
		t.Fatalf("应给出剩余天数，实际：%q", note)
	}

	// 刚续期（不到 1 天）→ 不提示
	writeInfo(t, snapPath(uid, "workbuddy-desktop"), uid, 30*24*time.Hour, time.Hour)
	if note := credNote(uid); note != "" {
		t.Fatalf("刚续期不该有提示，实际：%q", note)
	}

	// 已过期 → 明说过期，不能算出负数天数
	writeInfo(t, snapPath(uid, "workbuddy-desktop"), uid, -time.Hour, 40*24*time.Hour)
	note = credNote(uid)
	if !strings.Contains(note, "已过期") {
		t.Fatalf("过期状态应明说，实际：%q", note)
	}
	if strings.Contains(note, "还剩 -") {
		t.Fatalf("不该出现负数天数：%q", note)
	}
}

// TestCredWarnShort 卡片标黄/标红的阈值。
func TestCredWarnShort(t *testing.T) {
	_, _ = setupAuthDirs(t)
	const uid = "bbbb1111-2222-3333-4444-555566667777"
	_ = os.MkdirAll(profileDir(uid), 0o755)

	if _, known := credWarnShort(uid); known {
		t.Fatal("没有快照时应返回 known=false")
	}
	writeInfo(t, snapPath(uid, "workbuddy-desktop"), uid, 3*24*time.Hour, 0)
	days, known := credWarnShort(uid)
	if !known || days < 2 || days > 4 {
		t.Fatalf("剩余天数应约等于 3，实际 %d（known=%v）", days, known)
	}
}

// TestAccountsExposeExpiry 前端拿到的 expires_at 必须是秒（毫秒会让 JS 解析到几万年以后）。
func TestAccountsExposeExpiry(t *testing.T) {
	authD, _ := setupAuthDirs(t)
	const uid = "cccc1111-2222-3333-4444-555566667777"

	// 通过 saveCurrent 建快照，走的是和真实切换一样的路径
	writeInfo(t, filepath.Join(authD, "workbuddy-desktop.info"), uid, 30*24*time.Hour, 0)
	if _, err := saveCurrent(); err != nil {
		t.Fatal(err)
	}

	list := accounts()
	if len(list) != 1 {
		t.Fatalf("应列出 1 个账号，实际 %d", len(list))
	}
	a := list[0]
	if a.ExpiresAt < 1e9 || a.ExpiresAt > 4e9 {
		t.Fatalf("expires_at 应该是 Unix 秒，实际 %d", a.ExpiresAt)
	}
	if a.LastRefreshAt <= 0 {
		t.Fatalf("last_refresh_at 没填：%+v", a)
	}
	if d := time.Until(time.Unix(a.ExpiresAt, 0)); d < 29*24*time.Hour {
		t.Fatalf("剩余时间偏差过大：%v", d)
	}
	_ = fmt.Sprint(a)
}

// TestRealAccountsExpiry 用本机真实 profiles/ 验一遍（设 WBONE_REAL_ACCOUNTS=1 才跑）。
// 目的：确认毫秒/秒换算在真实数据上不会算出「还剩 20000 天」这种离谱结果。
func TestRealAccountsExpiry(t *testing.T) {
	if os.Getenv("WBONE_REAL_ACCOUNTS") != "1" {
		t.Skip("未设置 WBONE_REAL_ACCOUNTS")
	}
	old := exeDir
	exeDir = os.Getenv("WBONE_REAL_BASE")
	if exeDir == "" {
		t.Skip("未设置 WBONE_REAL_BASE")
	}
	defer func() { exeDir = old }()

	for _, a := range accounts() {
		left := time.Until(time.Unix(a.ExpiresAt, 0))
		if left > 40*24*time.Hour || left < -400*24*time.Hour {
			t.Fatalf("%s 剩余时间离谱：%v（expires_at=%d）", a.Nickname, left, a.ExpiresAt)
		}
		idle := time.Since(time.Unix(a.LastRefreshAt, 0))
		t.Logf("%-8s 剩余 %5.1f 天 | 上次续期 %.1f 天前 | %s",
			a.Nickname, left.Hours()/24, idle.Hours()/24, credNote(a.UID))
	}
}

// TestRoundDaysNoOffByOne 剩余天数必须四舍五入，不能截断。
//
// 现场：用户看到「登录态还剩 59 天」问是不是算错了。实际 refreshExpiresAt 是
// 今天 09:09 + 60 天整，剩余 59.999… 天，截断成 59。天数只做展示，但差一天会
// 让人怀疑整块数据。
func TestRoundDaysNoOffByOne(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want int
	}{
		{"刚续期的 60 天窗口", 60*24*time.Hour - time.Second, 60},
		{"59 天 23 小时", 59*24*time.Hour + 23*time.Hour, 60},
		{"剩余 59 天 12 小时", 59*24*time.Hour + 12*time.Hour, 60},
		{"剩余 59 天 11 小时", 59*24*time.Hour + 11*time.Hour, 59},
		{"不足一天按 1 天", 30 * time.Minute, 1},
		{"已过期保持负数", -time.Hour, 0},
	}
	for _, c := range cases {
		if got := roundDays(c.d); got != c.want {
			t.Errorf("%s: roundDays=%d want %d", c.name, got, c.want)
		}
	}
}

// TestUIUsesRoundedDays 界面那行「登录态还剩 N 天」必须与 Go 侧同口径（四舍五入）。
// 只截断的话，刚续期完的 60 天会显示成 59——这正是用户提的那个问题。
func TestUIUsesRoundedDays(t *testing.T) {
	raw, err := os.ReadFile("ui.html")
	if err != nil {
		t.Fatalf("读 ui.html: %v", err)
	}
	page := string(raw)
	if !strings.Contains(page, "Math.round(leftMs / 86400000)") {
		t.Error("卡片剩余天数应使用 Math.round(leftMs / 86400000)（截断会少一天）")
	}
	if strings.Contains(page, "Math.floor(leftMs / 86400000)") {
		t.Error("卡片剩余天数不应使用 Math.floor（59.99 天会显示成 59）")
	}
	// 窗口长度不能写死 30 天：上游 2026-10 起改成 60 天。
	if strings.Contains(page, "重新续 30 天") {
		t.Error("提示语里不该写死「重新续 30 天」，窗口天数应按数据算")
	}
}
