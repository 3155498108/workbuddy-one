// convsync.go 让同一台机器上的多个账号共用一份本地对话历史。
//
// 客户端把所有会话记在 ~/.workbuddy/workbuddy.db 的 sessions 表里，
// 每条记录带 user_id，侧栏按 `user_id = 当前登录账号` 过滤。所以换号之后
// 历史看着像"清空了"，其实文件都还在，只是归属对不上。
//
// 客户端自己从不 UPDATE sessions.user_id（只在新会话 INSERT 时写一次），
// 因此把历史统一归到当前账号是稳定有效的：切换账号时改一次 user_id，
// 所有历史就都回到侧栏里。
//
// 只处理 transport='local'。transport='cloud' 的会话属于云端账号数据，
// 改本地反而会和云端拉取冲突。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
	"unsafe"
)

// ── 系统自带 SQLite ────────────────────────────────────────────────
//
// 为了改一个字段去引一个 10MB 的纯 Go SQLite 驱动不划算。Windows 10 1803+
// 自带 winsqlite3.dll，导出的就是标准 sqlite3_* 接口，够用且不增加体积。

var (
	sqliteDLL     = syscall.NewLazyDLL("winsqlite3.dll")
	sqliteOpen    = sqliteDLL.NewProc("sqlite3_open")
	sqliteClose   = sqliteDLL.NewProc("sqlite3_close")
	sqliteExec    = sqliteDLL.NewProc("sqlite3_exec")
	sqliteErrmsg  = sqliteDLL.NewProc("sqlite3_errmsg")
	sqliteChanges = sqliteDLL.NewProc("sqlite3_changes")
	sqliteFree    = sqliteDLL.NewProc("sqlite3_free")
)

type sqliteDB struct{ h uintptr }

func openSQLite(path string) (*sqliteDB, error) {
	if err := sqliteDLL.Load(); err != nil {
		return nil, fmt.Errorf("系统没有可用的 SQLite 组件：%w", err)
	}
	cs, err := syscall.BytePtrFromString(path)
	if err != nil {
		return nil, err
	}
	var h uintptr
	rc, _, _ := sqliteOpen.Call(uintptr(unsafe.Pointer(cs)), uintptr(unsafe.Pointer(&h)))
	if rc != 0 {
		return nil, fmt.Errorf("打开数据库失败（错误码 %d）", rc)
	}
	d := &sqliteDB{h: h}
	// 客户端可能正握着库：给足等待时间，别让同步因为一把锁直接失败。
	_ = d.exec("PRAGMA busy_timeout = 8000")
	return d, nil
}

func (d *sqliteDB) exec(sql string) error {
	cs, err := syscall.BytePtrFromString(sql)
	if err != nil {
		return err
	}
	var errPtr uintptr
	rc, _, _ := sqliteExec.Call(d.h, uintptr(unsafe.Pointer(cs)), 0, 0,
		uintptr(unsafe.Pointer(&errPtr)))
	if rc == 0 {
		return nil
	}
	msg := ""
	if errPtr != 0 {
		msg = cString(errPtr)
		sqliteFree.Call(errPtr)
	}
	if msg == "" && d != nil {
		if p, _, _ := sqliteErrmsg.Call(d.h); p != 0 {
			msg = cString(p)
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("错误码 %d", rc)
	}
	return fmt.Errorf("%s", msg)
}

func (d *sqliteDB) changes() int {
	n, _, _ := sqliteChanges.Call(d.h)
	return int(n)
}

func (d *sqliteDB) close() {
	if d != nil && d.h != 0 {
		sqliteClose.Call(d.h)
		d.h = 0
	}
}

func cString(p uintptr) string {
	n := 0
	for *(*byte)(unsafe.Pointer(p + uintptr(n))) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(unsafe.Pointer(p)), n))
}

// ── 会话归属 ────────────────────────────────────────────────────────

var uidPattern = regexp.MustCompile(`^[0-9a-zA-Z_-]{8,64}$`)

func userHome() string {
	if v := os.Getenv("USERPROFILE"); v != "" {
		return v
	}
	h, _ := os.UserHomeDir()
	return h
}

func workbuddyDBPath() string {
	return filepath.Join(userHome(), ".workbuddy", "workbuddy.db")
}

// convStat 是某个账号名下有多少条本地会话。
type convStat struct {
	UID   string `json:"uid"`
	Count int    `json:"count"`
}

// convSummary 是"本地一共有多少条对话、分别挂在谁名下"。
type convSummary struct {
	Total   int        `json:"total"`
	ByUID   []convStat `json:"by_uid"`
	DBPath  string     `json:"db_path"`
	Missing bool       `json:"missing"`
}

// scanConversations 只读统计，不改任何东西。
func scanConversations() convSummary {
	out := convSummary{ByUID: []convStat{}, DBPath: workbuddyDBPath()}
	if _, err := os.Stat(out.DBPath); err != nil {
		out.Missing = true
		return out
	}
	d, err := openSQLite(out.DBPath)
	if err != nil {
		out.Missing = true
		return out
	}
	defer d.close()

	rows, err := queryRows(d, `SELECT COALESCE(user_id, ''), COUNT(*) FROM sessions
		WHERE transport = 'local' AND deleted_at IS NULL GROUP BY 1 ORDER BY 2 DESC`)
	if err != nil {
		out.Missing = true
		return out
	}
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		n := 0
		fmt.Sscanf(r[1], "%d", &n)
		out.ByUID = append(out.ByUID, convStat{UID: r[0], Count: n})
		out.Total += n
	}
	return out
}

// claimConversations 把全部本地会话归到 uid 名下，返回改动条数。
//
// 会先把数据库整份备份一次（只在确实要改动时才备份），出问题可以还原。
func claimConversations(uid string) (int, error) {
	if !uidPattern.MatchString(uid) {
		return 0, fmt.Errorf("账号标识不合法：%s", uid)
	}
	path := workbuddyDBPath()
	if _, err := os.Stat(path); err != nil {
		return 0, fmt.Errorf("没找到对话数据库，先打开一次 WorkBuddy 再来")
	}

	// 先备份：这是唯一会被改写的文件，留一份退路。
	if err := backupDBOnce(path); err != nil {
		return 0, err
	}

	d, err := openSQLite(path)
	if err != nil {
		return 0, err
	}
	defer d.close()

	// WAL 模式下并发的读不会挡写；用 IMMEDIATE 明确拿写锁，避免半路被别人插进来。
	if err := d.exec("BEGIN IMMEDIATE"); err != nil {
		return 0, err
	}
	q := fmt.Sprintf(`UPDATE sessions SET user_id = '%s'
		WHERE transport = 'local' AND (user_id IS NULL OR user_id <> '%s')`,
		escapeSQL(uid), escapeSQL(uid))
	if err := d.exec(q); err != nil {
		_ = d.exec("ROLLBACK")
		return 0, err
	}
	n := d.changes()
	if err := d.exec("COMMIT"); err != nil {
		_ = d.exec("ROLLBACK")
		return 0, err
	}
	return n, nil
}

func escapeSQL(s string) string {
	out := make([]rune, 0, len(s)+8)
	for _, r := range s {
		if r == '\'' {
			out = append(out, '\'')
		}
		out = append(out, r)
	}
	return string(out)
}

// queryRows 执行查询并把结果按字符串取回来（只用于统计，量很小）。
func queryRows(d *sqliteDB, sql string) ([][]string, error) {
	cs, err := syscall.BytePtrFromString(sql)
	if err != nil {
		return nil, err
	}
	var rows [][]string
	cb := syscall.NewCallback(func(_ uintptr, argc uintptr, argv uintptr, _ uintptr) uintptr {
		vals := (*[16]uintptr)(unsafe.Pointer(argv))[:argc:argc]
		row := make([]string, 0, len(vals))
		for _, v := range vals {
			if v == 0 {
				row = append(row, "")
			} else {
				row = append(row, cString(v))
			}
		}
		rows = append(rows, row)
		return 0
	})
	var errPtr uintptr
	rc, _, _ := sqliteExec.Call(d.h, uintptr(unsafe.Pointer(cs)), cb, 0,
		uintptr(unsafe.Pointer(&errPtr)))
	if rc != 0 {
		msg := "查询失败"
		if errPtr != 0 {
			msg = cString(errPtr)
			sqliteFree.Call(errPtr)
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return rows, nil
}

// backupDBOnce 每天最多留一份数据库备份，避免目录膨胀。
func backupDBOnce(path string) error {
	dir := filepath.Join(baseDir(), "backup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dir, "workbuddy-db-"+time.Now().Format("20060102")+".db")
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("备份数据库失败：%w", err)
	}
	defer src.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("备份数据库失败：%w", err)
	}
	if _, err := out.ReadFrom(src); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("备份数据库失败：%w", err)
	}
	out.Close()
	return os.Rename(tmp, dst)
}

// ── 用户偏好 ────────────────────────────────────────────────────────

type prefs struct {
	ShareConversations bool `json:"share_conversations"`

	// 更新相关（update.go）。
	//
	// V1.0.7 起更新是全自动的（查到就装、装完自己重启），原来那两个开关
	// update_check_off / update_auto_apply 已经删掉——留着只会让人以为
	// 自己能关掉，实际关不掉。老 prefs.json 里残留这两个键会被忽略。
	LastUpdateCheck int64  `json:"last_update_check,omitempty"` // unix 秒
	UpdateFeed      string `json:"update_feed,omitempty"`       // 自定义更新源（GitHub API 形状）
	UpdateToken     string `json:"update_token,omitempty"`      // GitHub token，仅用于提高 API 额度
}

func prefsPath() string { return filepath.Join(baseDir(), "data", "prefs.json") }

func readPrefs() prefs {
	var p prefs
	if b, err := os.ReadFile(prefsPath()); err == nil {
		_ = json.Unmarshal(b, &p)
	}
	return p
}

func writePrefs(p prefs) {
	_ = os.MkdirAll(filepath.Dir(prefsPath()), 0o755)
	if b, err := json.MarshalIndent(p, "", "  "); err == nil {
		_ = os.WriteFile(prefsPath(), b, 0o644)
	}
}

// syncAfterSwitch 切号后（客户端已关闭）把历史归到新账号。
func syncAfterSwitch(uid string) {
	if !readPrefs().ShareConversations {
		return
	}
	n, err := claimConversations(uid)
	if err != nil {
		logf("对话同步失败：%v", err)
		return
	}
	logf("对话同步：%d 条本地会话已归到 %s", n, shortUID(uid))
}
