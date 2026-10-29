package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// travelStub 模拟 growth 域全部端点，记录调用次数与请求参数。
type travelStub struct {
	buddy       string // /buddy/info 的 data.buddy 原文（"null" 或对象）
	state       string // /travel/status 的 data 原文
	firstStatus int    // /buddy/first 的 HTTP 状态码（非 0 时按业务错误返回）

	// stateAfterClaim 领奖之后再查状态时返回的 data 原文（空 = 复用 state）。
	// 用于验证「领完奖立刻再派一趟」这条链：真实上游 claim 后状态会变 idle。
	stateAfterClaim string

	infoCalls, statusCalls, departCalls atomic.Int32
	claimCalls, firstCalls, agreeCalls  atomic.Int32
	location, record                    atomic.Int64
}

func (s *travelStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/buddy/info":
			s.infoCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"msg":"ok","data":{"buddy":%s}}`, s.buddy)
		case "/activity/growth/buddy/travel/status":
			s.statusCalls.Add(1)
			st := s.state
			// 领奖后的第二次查询：真实上游此时已回到 idle，可用于验证「领完立刻再派」。
			if s.claimCalls.Load() > 0 && s.stateAfterClaim != "" {
				st = s.stateAfterClaim
			}
			fmt.Fprintf(w, `{"code":0,"msg":"ok","data":%s}`, st)
		case "/activity/growth/buddy/travel/depart":
			s.departCalls.Add(1)
			var body struct {
				LocationID int `json:"location_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.location.Store(int64(body.LocationID))
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case "/activity/growth/buddy/travel/claim":
			s.claimCalls.Add(1)
			var body struct {
				RecordID int64 `json:"record_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.record.Store(body.RecordID)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"reward_credit":9}}`))
		case "/activity/growth/buddy/first":
			s.firstCalls.Add(1)
			if s.firstStatus >= 400 {
				w.WriteHeader(s.firstStatus)
				w.Write([]byte(`{"code":400,"msg":"first_buddy task not completed yet"}`))
				return
			}
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":1,"name":"档案喵"}}}`))
		case "/activity/growth/buddy/agreement":
			s.agreeCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"agreed":true}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func (s *travelStub) server() *httptest.Server {
	return httptest.NewServer(s.handler())
}

// billingAndGrowthServer 同时模拟 billing（签到/余额/刷新）与 growth（旅行）端点，
// 供「签到收尾顺带跑旅行」这类跨域用例使用。
func billingAndGrowthServer(stub *travelStub) *httptest.Server {
	growth := stub.handler()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			growth.ServeHTTP(w, r)
		}
	}))
}

// fastTravel 关闭账号间限速，避免测试白等 800ms。
func fastTravel(t *testing.T) {
	t.Helper()
	old := travelAccountDelay
	travelAccountDelay = 0
	t.Cleanup(func() { travelAccountDelay = old })
}

// newTravelScheduler 构造 travel 相关依赖齐全的调度器。
func newTravelScheduler(t *testing.T, srv *httptest.Server, uids ...string) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22}}), p
}

// TestRunCheckinNowNoLongerTriggersTravel 签到收尾不再跑旅行（旅行已剥离为独立排程）。
// 旅行由独立时点（travel_hours）触发，与签到解耦。
func TestRunCheckinNowNoLongerTriggersTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunCheckinNow()

	// 签到不再顺带跑旅行：buddy/info 不应被调用。
	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("buddy/info calls=%d want 0（旅行已从签到剥离）", n)
	}
}

// TestRunTravelNowAdoptsOnNoBuddy 旅行独立排程：无猫 → 同意协议 + 领养。
func TestRunTravelNowAdoptsOnNoBuddy(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	if n := stub.infoCalls.Load(); n != 1 {
		t.Errorf("buddy/info calls=%d want 1", n)
	}
	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("buddy/first calls=%d want 1（无猫应尝试领养）", n)
	}
	if n := stub.agreeCalls.Load(); n != 1 {
		t.Errorf("buddy/agreement calls=%d want 1", n)
	}
}

// TestRunTravelCoversIdleAccount 旅行独立排程覆盖空闲账号并派出。
func TestRunTravelCoversIdleAccount(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":7,"name":"档案喵"}`,
		state: `{"state":"idle","daily_limit_reached":false}`}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")

	s.RunTravelNow()

	// 有猫 + idle + 未达上限 → 派出。
	if n := stub.departCalls.Load(); n != 1 {
		t.Errorf("depart calls=%d want 1", n)
	}
}

// TestRunKeepaliveDoesNotTriggerTravel 22 点保活不触发旅行：旅行独立排程。
func TestRunKeepaliveDoesNotTriggerTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunKeepaliveNow()

	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("buddy/info calls=%d want 0（保活不触发旅行）", n)
	}
	if n := stub.firstCalls.Load(); n != 0 {
		t.Errorf("buddy/first calls=%d want 0", n)
	}
}

// TestRunTravelStateMachine 表驱动覆盖状态机全部分支：每趟只做一个动作。
func TestRunTravelStateMachine(t *testing.T) {
	const buddyPresent = `{"id":7,"name":"档案喵 R"}`

	cases := []struct {
		name         string
		buddy        string
		state        string
		firstStatus  int
		wantInfo     int32
		wantStatus   int32
		wantDepart   int32
		wantClaim    int32
		wantFirst    int32
		wantAgree    int32
		wantLocation int64
		wantRecord   int64
		// stateAfterClaim 只在「arrived 领奖后」这一档有意义（见 travelStub 注释）：
		// 空 = 领奖后再查仍是原状态。
		stateAfterClaim string
	}{		{
			name: "无猫-领养成功", buddy: "null",
			wantInfo: 1, wantFirst: 1, wantAgree: 1,
		},
		{
			name: "无猫-门槛未达-静默跳过", buddy: "null", firstStatus: 400,
			wantInfo: 1, wantFirst: 1, wantAgree: 1,
		},
		{
			name: "有猫-idle-未达上限-派出", buddy: buddyPresent,
			state:    `{"state":"idle","daily_limit_reached":false}`,
			wantInfo: 1, wantStatus: 1, wantDepart: 1, wantLocation: 4,
		},
		{
			name: "有猫-idle-已达上限-跳过", buddy: buddyPresent,
			state:    `{"state":"idle","daily_limit_reached":true}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "有猫-traveling-跳过", buddy: buddyPresent,
			state:    `{"state":"traveling","record_id":42}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "有猫-arrived-领奖", buddy: buddyPresent,
			state:    `{"state":"arrived","record_id":42,"reward_credit":9}`,
			// status 两次：领奖前查状态 + 领奖后确认能否补派（stateAfterClaim 空 →
			// 仍返回 arrived，不满足 idle 条件，因此不补派）。
			wantInfo: 1, wantStatus: 2, wantClaim: 1, wantRecord: 42,
		},
		{
			name: "有猫-arrived-领奖后立刻再派出", buddy: buddyPresent,
			state:           `{"state":"arrived","record_id":42,"reward_credit":9}`,
			stateAfterClaim: `{"state":"idle","daily_limit_reached":false}`,
			wantInfo:        1, wantStatus: 2, wantClaim: 1, wantRecord: 42,
			wantDepart: 1, wantLocation: 4,
		},
		{
			name: "有猫-arrived-领奖后当日已派满-不补派", buddy: buddyPresent,
			state:           `{"state":"arrived","record_id":42,"reward_credit":9}`,
			stateAfterClaim: `{"state":"idle","daily_limit_reached":true}`,
			wantInfo:        1, wantStatus: 2, wantClaim: 1, wantRecord: 42,
		},
		{
			name: "有猫-arrived-缺record_id-跳过领奖", buddy: buddyPresent,
			state:    `{"state":"arrived","record_id":0}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "有猫-未知状态-跳过", buddy: buddyPresent,
			state:    `{"state":"teleporting"}`,
			wantInfo: 1, wantStatus: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastTravel(t)
			stub := &travelStub{buddy: tc.buddy, state: tc.state, firstStatus: tc.firstStatus, stateAfterClaim: tc.stateAfterClaim}
			srv := stub.server()
			defer srv.Close()

			s, _ := newTravelScheduler(t, srv, "u1")
			s.RunTravelNow()

			got := map[string]int32{
				"info": stub.infoCalls.Load(), "status": stub.statusCalls.Load(),
				"depart": stub.departCalls.Load(), "claim": stub.claimCalls.Load(),
				"first": stub.firstCalls.Load(), "agreement": stub.agreeCalls.Load(),
			}
			want := map[string]int32{
				"info": tc.wantInfo, "status": tc.wantStatus,
				"depart": tc.wantDepart, "claim": tc.wantClaim,
				"first": tc.wantFirst, "agreement": tc.wantAgree,
			}
			for k, w := range want {
				if got[k] != w {
					t.Errorf("%s calls=%d want %d", k, got[k], w)
				}
			}
			if tc.wantDepart > 0 && stub.location.Load() != tc.wantLocation {
				t.Errorf("location_id=%d want %d", stub.location.Load(), tc.wantLocation)
			}
			if tc.wantClaim > 0 && stub.record.Load() != tc.wantRecord {
				t.Errorf("record_id=%d want %d", stub.record.Load(), tc.wantRecord)
			}
		})
	}
}

// TestRunTravelAdoptThresholdTriedOncePerDay 门槛未达（400）当日只试一次，后续巡检静默跳过。
func TestRunTravelAdoptThresholdTriedOncePerDay(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null", firstStatus: 400}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()
	s.RunTravelNow()
	s.RunTravelNow()

	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("first calls=%d want 1（当日只试一次）", n)
	}
	if n := stub.agreeCalls.Load(); n != 1 {
		t.Errorf("agreement calls=%d want 1", n)
	}
	if n := stub.infoCalls.Load(); n != 3 {
		t.Errorf("info calls=%d want 3（仍每趟查有无猫）", n)
	}
}

// TestRunTravelAdoptTriedExpiresNextDay 跨自然日后允许重新尝试领养。
func TestRunTravelAdoptTriedExpiresNextDay(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null", firstStatus: 400}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.markAdoptTried("u1") // 当日已试过
	s.RunTravelNow()
	if n := stub.firstCalls.Load(); n != 0 {
		t.Fatalf("first calls=%d want 0（当日已试过）", n)
	}

	s.adoptTried["u1"] = "2000-01-01" // 模拟昨日记录
	s.RunTravelNow()
	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("first calls=%d want 1（跨日应重试）", n)
	}
}

// TestRunTravelSkipsDisabledAndFailedAccounts 禁用账号不查；单账号出错不影响后续账号。
func TestRunTravelSkipsDisabledAndFailedAccounts(t *testing.T) {
	fastTravel(t)
	var deadCalls, okDepart, disabledCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("X-User-Id") == "disabled":
			disabledCalls.Add(1)
			w.WriteHeader(401)
			w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
		case r.Header.Get("X-User-Id") == "dead":
			deadCalls.Add(1)
			w.WriteHeader(401)
			w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
		case r.URL.Path == "/activity/growth/buddy/info":
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/status":
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/depart":
			okDepart.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "disabled", "dead", "ok")
	p.Disable("disabled", "test")
	s.RunTravelNow()

	if n := deadCalls.Load(); n != 1 {
		t.Errorf("dead account calls=%d want 1（401 跳过本轮，不强刷 token）", n)
	}
	if n := disabledCalls.Load(); n != 0 {
		t.Errorf("disabled account calls=%d want 0", n)
	}
	// 前列账号失败不应中断遍历：末位 ok 账号照常完成派出。
	if n := okDepart.Load(); n != 1 {
		t.Errorf("ok account depart calls=%d want 1（失败账号不影响后续遍历）", n)
	}
	st, ok := p.Status("ok")
	if !ok {
		t.Fatal("ok 账号应在池中")
	}
	if st.Disabled {
		t.Errorf("ok 账号不应被影响: %+v", st)
	}
}

// TestRunTravelDisabledAccountSkipsAllCalls 禁用账号一个请求都不发。
func TestRunTravelDisabledAccountSkipsAllCalls(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "u1")
	p.Disable("u1", "test")
	s.RunTravelNow()

	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("info calls=%d want 0（禁用账号应跳过）", n)
	}
}

// TestRunTravelActionErrorsDoNotAbort 各环节上游报错只影响本账号本轮：不 panic、不中断遍历。
func TestRunTravelActionErrorsDoNotAbort(t *testing.T) {
	fastTravel(t)
	var okDepart atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-User-Id")
		fail := func() {
			w.WriteHeader(500)
			w.Write([]byte(`{"code":500,"msg":"boom"}`))
		}
		switch {
		case uid == "bdinfo": // 查有无猫失败
			fail()
		case uid == "status" && r.URL.Path == "/activity/growth/buddy/travel/status": // 查状态失败
			fail()
		case uid == "depart" && r.URL.Path == "/activity/growth/buddy/travel/depart": // 派出发失败
			fail()
		case uid == "claim" && r.URL.Path == "/activity/growth/buddy/travel/claim": // 领奖失败
			fail()
		case uid == "agree" && r.URL.Path == "/activity/growth/buddy/agreement": // 同意协议失败
			fail()
		case uid == "first" && r.URL.Path == "/activity/growth/buddy/first": // 领养非门槛类失败
			fail()
		case r.URL.Path == "/activity/growth/buddy/info":
			if uid == "claim" {
				w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
				return
			}
			if uid == "depart" || uid == "status" || uid == "ok" {
				w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/status":
			if uid == "claim" {
				w.Write([]byte(`{"code":0,"data":{"state":"arrived","record_id":42}}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/depart":
			okDepart.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		case r.URL.Path == "/activity/growth/buddy/agreement":
			w.Write([]byte(`{"code":0,"data":{"agreed":true}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "agree", "bdinfo", "claim", "depart", "first", "ok", "status")
	s.RunTravelNow() // 不应 panic

	// 末位账号（uid 排序后 ok 排在 status 前，但都在失败账号之后）照常完成派出。
	if n := okDepart.Load(); n == 0 {
		t.Errorf("depart calls=%d want >0（个别账号失败不应中断遍历）", n)
	}
}

// TestRunTravelLoopCancelsWhenDisabled 无任何整点任务时 Run 不空转，ctx 取消即返回。
func TestRunTravelLoopCancelsWhenDisabled(t *testing.T) {
	s := &Scheduler{cfg: Config{}, adoptTried: map[string]string{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后返回")
	}
}

// TestRunTravelLoopStopsOnCancel 有排程在等计时器时 ctx 取消应立即退出。
func TestRunTravelLoopStopsOnCancel(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, KeepaliveHours: []int{22}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond) // 让 Run 进入 select 等待
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后返回")
	}
}

// TestTravelDayAlignsCST 每日重置按 CST 自然日判定（UTC 17:00 已是次日 CST）。
func TestTravelDayAlignsCST(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"UTC 凌晨 = CST 当日", time.Date(2026, 9, 11, 2, 0, 0, 0, time.UTC), "2026-09-11"},
		{"UTC 16:00 = CST 次日 00:00", time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC), "2026-09-12"},
		{"UTC 15:59 仍在 CST 当日", time.Date(2026, 9, 11, 15, 59, 0, 0, time.UTC), "2026-09-11"},
	}
	for _, c := range cases {
		if got := travelDay(c.in); got != c.want {
			t.Errorf("%s: travelDay=%s want %s", c.name, got, c.want)
		}
	}
}

// waitTravelCalls 轮询等待某计数器达到期望值（巡检是 goroutine，不能靠固定 sleep）。
func waitTravelCalls(t *testing.T, name string, c *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s calls=%d want >=%d（超时）", name, c.Load(), want)
}

// TestStartTravelAutoRunsOnStartThenRepeats 常驻巡检：启动即补一趟（把上一进程
// 留下的 arrived/idle 收口），之后按间隔重复；间隔热改 0 后停止（只留固定时点）。
func TestStartTravelAutoRunsOnStartThenRepeats(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartTravelAuto(ctx, 40*time.Millisecond)

	waitTravelCalls(t, "info(启动即跑)", &stub.infoCalls, 1)
	waitTravelCalls(t, "info(周期重复)", &stub.infoCalls, 3)

	// 热改 0：停止周期巡检。先等在途那一趟跑完再取基准，避免把在途算成"没停"。
	s.SetTravelAutoInterval(0)
	time.Sleep(60 * time.Millisecond)
	base := stub.infoCalls.Load()
	time.Sleep(150 * time.Millisecond)
	if got := stub.infoCalls.Load(); got != base {
		t.Errorf("间隔改 0 后仍在巡检：%d → %d", base, got)
	}
	if s.TravelAutoInterval() != 0 {
		t.Errorf("TravelAutoInterval()=%v want 0", s.TravelAutoInterval())
	}
}

// TestStartTravelAutoDisabled interval<=0 不启动（travel_auto=false 的装配口径）。
func TestStartTravelAutoDisabled(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartTravelAuto(ctx, 0)
	time.Sleep(80 * time.Millisecond)
	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("interval=0 不应启动巡检，info calls=%d", n)
	}
}

// TestTravelHoursSnapshot 面板展示用的固定时点必须是副本（调用方改动不影响排程）。
func TestTravelHoursSnapshot(t *testing.T) {
	s := New(Config{TravelHours: []int{9, 21}})
	got := s.TravelHours()
	if len(got) != 2 || got[0] != 9 || got[1] != 21 {
		t.Fatalf("TravelHours=%v want [9 21]", got)
	}
	got[0] = 99
	if again := s.TravelHours(); again[0] != 9 {
		t.Errorf("返回值被写穿到配置：%v", again)
	}
}
