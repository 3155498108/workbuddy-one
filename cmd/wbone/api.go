// api.go 账号页的本地接口（挂在 /accounts/api/ 下，与网关同源同端口）。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type accountResp struct {
	UID      string   `json:"uid"`
	Nickname string   `json:"nickname"`
	UIN      string   `json:"uin,omitempty"`
	SavedAt  string   `json:"saved_at,omitempty"`
	Lines    []string `json:"lines"`
	Active   bool     `json:"active"`
}

type stateResp struct {
	OK       bool          `json:"ok"`
	Current  []accountResp `json:"current"`
	Running  bool          `json:"running"`
	Accounts []accountResp `json:"accounts"`
	AuthDir  string        `json:"auth_dir"`
	Client   string        `json:"client"`
	Port     string        `json:"port"`
	Version  string        `json:"version"`

	// 对话同步：所有账号共用一份本地对话历史。
	ShareConvs bool        `json:"share_convs"`
	Convs      convSummary `json:"convs"`
}

func buildState() stateResp {
	list := accounts()
	cur := []accountResp{}
	for _, a := range list {
		if a.Active {
			cur = append(cur, a)
		}
	}
	client := findClient()
	if client == "" {
		client = "未找到"
	}
	return stateResp{
		OK:       true,
		Current:  cur,
		Running:  clientRunning(),
		Accounts: list,
		AuthDir:  authDir(),
		Client:   client,
		Port:     appPort,
		Version:  appVersion,

		ShareConvs: readPrefs().ShareConversations,
		Convs:      scanConversations(),
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, msg string) {
	writeJSON(w, map[string]any{"ok": false, "error": msg})
}

func doSwitch(uid string, restart bool) (string, error) {
	if uid == "" {
		return "", fmt.Errorf("缺少账号标识")
	}
	if len(snapshotsOf(uid)) == 0 {
		return "", fmt.Errorf("这个账号没有保存过快照")
	}
	for _, p := range scanProducts() {
		if p.UID == uid {
			return "当前已经在这个账号上，无需切换", nil
		}
	}
	wasRunning := clientRunning()
	if wasRunning {
		killClient() // 先退出客户端，避免凭据文件被占用或在退出时被回写覆盖
	}
	if err := switchTo(uid); err != nil {
		if wasRunning {
			_ = restartClient()
		}
		return "", err
	}
	// 客户端此时已退出，正好改数据库：把本地历史全部归到新账号名下，
	// 切过去侧栏就能看到之前所有对话（开关关闭时这里什么都不做）。
	syncAfterSwitch(uid)
	if restart || wasRunning {
		if err := restartClient(); err != nil {
			return "已切换，但自动启动客户端失败，请手动打开 WorkBuddy", nil
		}
		return "已切换，WorkBuddy 正在重新启动", nil
	}
	return "已切换，下次启动 WorkBuddy 即为新账号", nil
}

// registerAccounts 把账号页的接口注册到网关的同一套路由上。
func registerAccounts(mux *http.ServeMux) {
	mux.HandleFunc("GET /accounts/api/state", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())
		writeJSON(w, buildState())
	})
	mux.HandleFunc("POST /accounts/api/ping", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /accounts/api/save", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())
		n, err := saveCurrent()
		if err != nil {
			writeErr(w, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "message": fmt.Sprintf("已保存 %d 条线路的登录态", n)})
	})
	mux.HandleFunc("POST /accounts/api/switch", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())
		var req struct {
			UID     string `json:"uid"`
			Restart bool   `json:"restart"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		msg, err := doSwitch(req.UID, req.Restart)
		if err != nil {
			writeErr(w, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "message": msg})
	})
	mux.HandleFunc("POST /accounts/api/rename", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())
		var req struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		name := strings.TrimSpace(req.Nickname)
		if req.UID == "" || name == "" {
			writeErr(w, "账号或名称不能为空")
			return
		}
		m := readMeta(req.UID)
		m.UID = req.UID
		m.Nickname = name
		writeMeta(m)
		writeJSON(w, map[string]any{"ok": true, "message": "已改名"})
	})
	mux.HandleFunc("POST /accounts/api/restart", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())
		if clientRunning() {
			killClient()
		}
		if err := restartClient(); err != nil {
			writeErr(w, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "message": "客户端已重启"})
	})
	mux.HandleFunc("POST /accounts/api/convsync", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())
		var req struct {
			Share bool `json:"share"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		p := readPrefs()
		p.ShareConversations = req.Share
		writePrefs(p)
		msg := "已关闭对话同步"
		if req.Share {
			msg = "已开启：以后每次切换账号，历史对话都会跟着过去"
		}
		writeJSON(w, map[string]any{"ok": true, "message": msg, "share": req.Share})
	})
	mux.HandleFunc("POST /accounts/api/convsync/apply", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())
		uid, name := primaryIdentity()
		if uid == "" {
			writeErr(w, "读不到当前账号，请先登录 WorkBuddy")
			return
		}
		wasRunning := clientRunning()
		if wasRunning {
			// 客户端握着数据库又在内存里缓存着会话列表，改完必须重启才会重新读。
			killClient()
		}
		n, err := claimConversations(uid)
		if err == nil && (wasRunning || readPrefs().ShareConversations) {
			_ = restartClient()
		}
		if err != nil {
			writeErr(w, err.Error())
			return
		}
		who := name
		if who == "" {
			who = shortUID(uid)
		}
		writeJSON(w, map[string]any{
			"ok":      true,
			"changed": n,
			"message": fmt.Sprintf("已把 %d 条对话归到「%s」名下", n, who),
		})
	})
	mux.HandleFunc("POST /accounts/api/focus", func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().UnixNano())
		// 切号后 WorkBuddy 客户端会重新启动并抢到前台，管家窗口被压在后面。
		// 用户以为管家没了就会再点一次图标——那正是产生孤儿窗口的源头。
		h := ensureVisibleSelf(4)
		writeJSON(w, map[string]any{"ok": true, "found": h != 0})
	})
	mux.HandleFunc("POST /accounts/api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
		go func() {
			time.Sleep(200 * time.Millisecond)
			shutdown()
		}()
	})
}
