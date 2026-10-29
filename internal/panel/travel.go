// travel.go 面板「猫猫旅行」状态接口。
//
// 只读：查每账号有无猫 + 旅行状态（idle / traveling / arrived）+ 可领奖励，
// 供任务中心展示「自动巡检」是否真的在跑。实际派出/领奖仍由调度器
// （固定时点 + 常驻巡检）或面板「立刻巡检」按钮触发，本接口不做任何写操作。
package panel

import (
	"net/http"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// travelRow 单账号旅行状态（面板展示口径）。
type travelRow struct {
	UID         string `json:"uid"`
	Nickname    string `json:"nickname"`
	HasCat      bool   `json:"has_cat"`
	CatName     string `json:"cat_name,omitempty"`
	State       string `json:"state,omitempty"` // idle / traveling / arrived
	Reward      int64  `json:"reward_credit"`
	DailyLimit  bool   `json:"daily_limit_reached"`
	ArriveAt    int64  `json:"arrive_at,omitempty"`
	HoursBucket int    `json:"duration_hours,omitempty"`
	Err         string `json:"error,omitempty"`
}

// travelStatus 返回全部账号的旅行状态 + 当前自动巡检间隔。
//
// 并发限 3：状态查询是两次上游 GET（buddy/info + travel/status），账号多时
// 不限速会一次打出几十个请求（与 packages/vouchers 同款口径）。
// 禁用账号跳过（disabled 不参与任何保号动作）；global / 企业版无成长体系，
// 直接给原因不发上游调用。
func (p *Panel) travelStatus(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	out := make([]travelRow, len(accts))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, st := range accts {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = p.travelRowOf(a)
		}(i, a)
	}
	wg.Wait()

	rows := make([]travelRow, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			rows = append(rows, it)
		}
	}
	autoMin := 0
	hours := []int(nil)
	if p.cfg.Scheduler != nil {
		autoMin = int(p.cfg.Scheduler.TravelAutoInterval() / time.Minute)
		hours = p.cfg.Scheduler.TravelHours()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"accounts":     rows,
		"auto_minutes": autoMin,
		"hours":        hours,
	})
}

// travelRowOf 单账号状态（两次 GET，任一失败只标该账号）。
func (p *Panel) travelRowOf(a *auth.Auth) travelRow {
	it := travelRow{UID: a.UID, Nickname: a.Nickname}
	switch {
	case a.IsGlobal():
		it.Err = "global 版无成长体系"
		return it
	case a.IsEnterprise():
		it.Err = "企业版无成长体系"
		return it
	}
	buddy, err := p.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		it.Err = err.Error()
		return it
	}
	if buddy == nil {
		it.Err = "还没有猫（巡检会自动领养）"
		return it
	}
	it.HasCat, it.CatName = true, buddy.Name
	ts, err := p.cfg.Upstream.TravelStatus(a)
	if err != nil {
		it.Err = err.Error()
		return it
	}
	it.State = ts.State
	it.Reward = ts.RewardCredit
	it.DailyLimit = ts.DailyLimitReached
	it.ArriveAt = ts.ArriveAt
	it.HoursBucket = ts.DurationHours
	return it
}
