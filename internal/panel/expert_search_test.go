// expert_search_test.go 钉住「按名字找专家」必须走关键字搜索，而不是扫推荐列表。
//
// 现场（2026-10-30）：WorkBuddy_Valorant（体验「无畏契约拼豆专家」）点完成报
// 「市场列表（20 条）里没找到「无畏契约拼豆专家」」。实测同一天的上游：
//   - MarketExpertList（无 keyword，reco_rank 前 20）→ 没有任何「无畏契约」相关专家
//   - MarketExpertSearch(keyword=无畏契约)      → ex_xQHHD1u5Jkua 排第一
// 也就是说专家是「掉出推荐位」而不是下架，扫推荐列表必然失配。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// expertMarketStub 模拟专家市场：无 keyword 时回一批「推荐位」专家（不含目标），
// 带 keyword 命中时回目标专家。
type expertMarketStub struct {
	targetID   string
	targetName string
	// searchBroken 让搜索接口直接 500（验证回落推荐列表的老路径不炸）。
	searchBroken bool

	listCalls, searchCalls atomic.Int32
}

func (s *expertMarketStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/market/expert/list") {
			http.Error(w, "not found", 404)
			return
		}
		var body struct {
			Keyword string `json:"keyword"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		write := func(experts string) {
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"experts":[` + experts + `]}}`))
		}
		if body.Keyword == "" {
			s.listCalls.Add(1)
			// 20 条推荐位专家：刻意全都与「无畏契约」无关（复刻真实上游）。
			var b strings.Builder
			for i := 0; i < 20; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"expert_id":"ex_filler` + string(rune('a'+i)) + `","display_name_zh":"推荐位专家` + string(rune('A'+i)) + `","profession_zh":"通用助手","expert_type":"agent"}`)
			}
			write(b.String())
			return
		}
		s.searchCalls.Add(1)
		if s.searchBroken {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"code":500,"msg":"boom"}`))
			return
		}
		if !strings.Contains(s.targetName, body.Keyword) && !strings.Contains("无畏契约", body.Keyword) {
			write("")
			return
		}
		write(`{"expert_id":"` + s.targetID + `","display_name_zh":"` + s.targetName + `","profession_zh":"` + s.targetName + `","expert_type":"agent"}`)
	}))
}

// newExpertSearchPanel 只依赖 Upstream 的面板实例（findExpertByKeyword 不碰 Pool）。
func newExpertSearchPanel(srv *httptest.Server) *Panel {
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Version: "test", Upstream: up})
}

// TestFindExpertByKeywordFindsDroppedExpert 推荐列表里没有，但关键字搜索能命中——
// 这正是无畏契约拼豆专家的现状，也是本文件存在的理由。
func TestFindExpertByKeywordFindsDroppedExpert(t *testing.T) {
	stub := &expertMarketStub{targetID: "ex_xQHHD1u5Jkua", targetName: "无畏契约拼豆专家"}
	srv := stub.server(t)
	defer srv.Close()

	p := newExpertSearchPanel(srv)
	a := &auth.Auth{UID: "u1", AccessToken: "at"}

	e, err := p.findExpertByKeyword(a, "无畏契约拼豆专家")
	if err != nil {
		t.Fatalf("findExpertByKeyword: %v", err)
	}
	if e == nil {
		t.Fatal("没找到专家：说明又退回了「只扫推荐列表」的老路")
	}
	if e.ExpertID != "ex_xQHHD1u5Jkua" {
		t.Errorf("expert_id=%q want ex_xQHHD1u5Jkua", e.ExpertID)
	}
	if stub.listCalls.Load() != 0 {
		t.Errorf("推荐列表调用 %d 次：命中搜索后不该再打推荐列表", stub.listCalls.Load())
	}
}

// TestFindExpertByKeywordWideKeyword 精确名搜不到时，按产品名（无畏契约）也能命中。
func TestFindExpertByKeywordWideKeyword(t *testing.T) {
	stub := &expertMarketStub{targetID: "ex_xQHHD1u5Jkua", targetName: "无畏契约拼豆专家"}
	srv := stub.server(t)
	defer srv.Close()

	p := newExpertSearchPanel(srv)
	if e, err := p.findExpertByKeyword(&auth.Auth{UID: "u1", AccessToken: "at"}, "无畏契约"); err != nil || e == nil {
		t.Fatalf("按「无畏契约」应能命中：e=%v err=%v", e, err)
	}
}

// TestFindExpertByKeywordFallsBackToList 搜索接口挂了也不能直接失败：
// 推荐列表里若恰好有目标（老专家仍在推荐位），仍应命中。
func TestFindExpertByKeywordFallsBackToList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Keyword string `json:"keyword"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Keyword != "" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"code":500,"msg":"boom"}`))
			return
		}
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"experts":[{"expert_id":"ex_inlist","display_name_zh":"无畏契约拼豆专家","profession_zh":"无畏契约拼豆专家","expert_type":"agent"}]}}`))
	}))
	defer srv.Close()

	p := newExpertSearchPanel(srv)
	e, err := p.findExpertByKeyword(&auth.Auth{UID: "u1", AccessToken: "at"}, "无畏契约拼豆专家")
	if err != nil {
		t.Fatalf("搜索失败时应回落列表而不是报错: %v", err)
	}
	if e == nil || e.ExpertID != "ex_inlist" {
		t.Fatalf("回落列表应命中 ex_inlist，got %+v", e)
	}
}

// TestFindExpertByKeywordNotFound 两处都没有时返回 (nil, nil)，由调用方给出可读错误。
func TestFindExpertByKeywordNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"experts":[]}}`))
	}))
	defer srv.Close()

	p := newExpertSearchPanel(srv)
	e, err := p.findExpertByKeyword(&auth.Auth{UID: "u1", AccessToken: "at"}, "无畏契约拼豆专家")
	if err != nil {
		t.Fatalf("空结果不算错误: %v", err)
	}
	if e != nil {
		t.Fatalf("应为 nil，got %+v", e)
	}
}
