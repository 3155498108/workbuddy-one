// travel.go 猫猫旅行巡检状态机：随旅行时点（travel_hours，默认 09 点）对池内每个可用账号单趟推进一次。
// 无猫 → 同意协议 + 领养；有猫 → 按 travel/status 分派 派出 / 领奖 / 跳过。
package scheduler

import (
	"fmt"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	// travelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，无最优解。
	travelLocationID = 4

	// travelStateIdle 空闲可派出；travelStateTraveling 在途；travelStateArrived 到站可领奖。
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// travelAccountDelay 账号间限速：全量账号约 40s，避免上游风控。测试可置 0。
var travelAccountDelay = 800 * time.Millisecond

// travelRetryDelay 巡检整趟失败（断网 / 上游 5xx / 代理没起）后的重试间隔。
// 猫猫旅行一天只有一次派出机会，等一个完整间隔（默认 60 分钟）才重试会把这一次
// 往后推很久，所以失败的趟用短退避。测试可缩短。
var travelRetryDelay = 5 * time.Minute

// activityAccountDelay 活跃上报账号间限速：与旅行同口径，避免上游风控。测试可置 0。
var activityAccountDelay = 800 * time.Millisecond

// adoptReportGap 领养前置上报后的等待：给上游事件处理留时间再发 buddy/first。
// 对齐 scripts/task_first_buddy.py 实测的 1.05s 间隔口径。测试可置 0。
var adoptReportGap = 1050 * time.Millisecond

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。中国无夏令时，固定 +8 即可，
// 不依赖容器 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// RunTravelNow 立即对池内所有可用账号执行一趟旅行巡检。
// 禁用账号跳过；暂停选号（paused）账号照常参与——旅行是纯 RPC（状态/派出/
// 领奖 + 领养前置上报），不发模型对话，与「让位防风控」不冲突；
// 401/查询失败只跳过该账号本轮（不强刷 token，交 22:00 keepalive）；
// 账号间限速 travelAccountDelay。
func (s *Scheduler) RunTravelNow() {
	s.RunTravelNowReport()
}

// RunTravelNowReport 同 RunTravelNow，另外返回 (账号数, 失败数)，供常驻巡检判断
// 「这一趟是不是全废了」。口径：只有真正打到上游并拿回错误才算失败；skip（在途 /
// 当日已派满 / 未知状态）不算——那是正常状态，重试也没用。
func (s *Scheduler) RunTravelNowReport() (total, failed int) {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}
		if a.IsEnterprise() {
			continue // 企业版门控：无成长体系（/activity/growth/* 上游一律 403），不发起任何上游调用
		}
		if !first {
			time.Sleep(travelAccountDelay)
		}
		first = false
		total++
		if err := s.travelOne(a); err != nil {
			failed++
		}
	}
	return total, failed
}

// travelOne 单账号单趟状态机：查有无猫 + 查状态 + 最多一个动作，不轮询不等待。
// 返回错误仅表示「这一趟没跑成」（网络/上游错误），skip 分支返回 nil。
func (s *Scheduler) travelOne(a *auth.Auth) error {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("travel %s: buddy-info: %v", logfmt.Label(a.UID, a.Nickname), err)
		return err
	}
	if buddy == nil {
		return s.travelAdopt(a)
	}
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		log.Printf("travel %s: status: %v", logfmt.Label(a.UID, a.Nickname), err)
		return err
	}
	switch ts.State {
	case travelStateArrived:
		claimed, err := s.travelClaim(a, ts)
		if err != nil {
			return err
		}
		if !claimed {
			return nil // 到站却没领成（缺 record_id 等异常）：不去补派，避免在异常态上再叠一个动作
		}
		// 领完奖是不是还能再派一趟？**先看领奖前那份状态里的当日上限**，能省一次无谓请求：
		// 猫猫旅行一天只有一次派出机会（上游 daily_limit_reached 按自然日 00:00 CST 重置），
		// 绝大多数情况（9 点派出 → 12 点到站 → 12 点领奖）此时上限已经用掉，再查也是 skip。
		//
		// 真正会补派的只有「跨天」这一种：昨天 21 点派出 → 今天 00:xx 到站 → 领奖时
		// 今天这一次还没用（daily_limit_reached=false）→ 顺手把今天的唯一一次用掉，
		// 不必干等到 9 点。所以这里传的是领奖前的状态，而不是无条件重查。
		return s.travelDepartAgain(a, !ts.DailyLimitReached)
	case travelStateIdle:
		return s.travelDepart(a, ts)
	case travelStateTraveling:
		log.Printf("travel %s: skip (traveling record=%d)", logfmt.Label(a.UID, a.Nickname), ts.RecordID)
		return nil
	default:
		log.Printf("travel %s: skip (unknown state %q)", logfmt.Label(a.UID, a.Nickname), ts.State)
		return nil
	}
}

// travelDepartAgain 领奖后补派今天的唯一一次（quotaFree=false 直接跳过，不发请求）。
//
// 为什么要传 quotaFree 而不是自己判断：领奖前后 daily_limit_reached 不会变（它记的是
// 「今天派过没有」，与领奖无关），所以领奖前那份状态已经给出答案——为 false 时再查一遍
// 必然是 skip，纯多一次上游请求 + 一行容易误读的日志。
func (s *Scheduler) travelDepartAgain(a *auth.Auth, quotaFree bool) error {
	if !quotaFree {
		return nil // 今天的一次已经用掉了：正常状态，不记日志（每天都会有）
	}
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		log.Printf("travel %s: re-depart status: %v", logfmt.Label(a.UID, a.Nickname), err)
		return err
	}
	if ts.State != travelStateIdle {
		return nil // 上游已经在途（并发触发/连点）：不重复派
	}
	if ts.DailyLimitReached {
		return nil
	}
	if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("travel %s: re-depart: %v", logfmt.Label(a.UID, a.Nickname), err)
		return err
	}
	log.Printf("travel %s: re-depart ok after claim location=%d", logfmt.Label(a.UID, a.Nickname), travelLocationID)
	return nil
}

// travelDepart 空闲且未达当日上限时派出（每日 1 次，自然日 00:00 CST 重置）。
func (s *Scheduler) travelDepart(a *auth.Auth, ts *upstream.TravelState) error {
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", logfmt.Label(a.UID, a.Nickname))
		return nil
	}
	if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("travel %s: depart: %v", logfmt.Label(a.UID, a.Nickname), err)
		return err
	}
	log.Printf("travel %s: depart ok location=%d", logfmt.Label(a.UID, a.Nickname), travelLocationID)
	return nil
}

// travelClaim 到站领奖（必须带 record_id）。返回 (是否领到, 错误)。
// 错误 = 这一趟没跑成（可重试）；claimed=false + err=nil = 数据异常按 skip 处理。
func (s *Scheduler) travelClaim(a *auth.Auth, ts *upstream.TravelState) (bool, error) {
	if ts.RecordID == 0 {
		// 数据异常（到站却没有记录 id）：重试也不会变好，按 skip 处理不触发短重试。
		log.Printf("travel %s: claim skipped (arrived but no record_id)", logfmt.Label(a.UID, a.Nickname))
		return false, nil
	}
	reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v", logfmt.Label(a.UID, a.Nickname), ts.RecordID, err)
		return false, err
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", logfmt.Label(a.UID, a.Nickname), ts.RecordID, reward)
	return true, nil
}

// travelAdopt 无猫时领养，链路：report → agreement → buddy/first。
//
// report 必须先跑（scripts/task_first_buddy.py 实测）：一条 chat_request_send 上报
// 点亮 growth 连登并**解锁 first_buddy 任务**；未上报时 buddy/first 会返回
// 400 "first_buddy task not completed yet"——该门槛的真实来源是"当日无活跃上报"，
// 不是账号问题（report.go 注释亦明确「解锁 first_buddy 任务（领养前置）」）。
// conversation 门槛未达标仍属预期行为，记一次当日已试后静默跳过，不再重试。
func (s *Scheduler) travelAdopt(a *auth.Auth) error {
	if s.adoptTriedToday(a.UID) {
		return nil
	}
	// 前置：解锁 first_buddy 任务（幂等；失败不阻塞，让 buddy/first 按既有错误路径暴露）。
	if err := s.cfg.Upstream.ReportChatActivity(a, fmt.Sprintf("wb2api-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		log.Printf("travel %s: adopt preflight report: %v", logfmt.Label(a.UID, a.Nickname), err)
	} else {
		time.Sleep(adoptReportGap) // 给上游事件处理留时间（对齐脚本实测的 1.05s 间隔口径）
	}
	if err := s.cfg.Upstream.BuddyAgreement(a); err != nil {
		log.Printf("travel %s: agreement: %v", logfmt.Label(a.UID, a.Nickname), err)
		return err
	}
	err := s.cfg.Upstream.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", logfmt.Label(a.UID, a.Nickname))
		return nil
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(a.UID)
		log.Printf("travel %s: adopt skipped (conversation threshold not reached, retry tomorrow)", logfmt.Label(a.UID, a.Nickname))
		return nil
	default:
		log.Printf("travel %s: adopt: %v", logfmt.Label(a.UID, a.Nickname), err)
		return err
	}
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达。
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptTried[uid] = travelDay(time.Now())
}
