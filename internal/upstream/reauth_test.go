package upstream

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// gateway401 是线上的真实形态：APISIX/openresty 网关在 Authorization 不被认可时
// 直接返回 HTML，压根没有业务信封（对比业务层的 {"code":12153,...}）。
const gateway401 = `<html>
<head><title>401 Authorization Required</title></head>
<body>
<center><h1>401 Authorization Required</h1></center>
<hr><center>openresty</center>
<p><em>Powered by <a href="https://apisix.apache.org/">APISIX</a></em></p>
</body>
</html>`

func htmlResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// 网关层 401（HTML、无业务信封）必须归 ErrSessionDead。
// 落到 ErrClient 的后果是「只换号不罚」：死号留在池内反复被选中，池状态里 credits
// 还是上次的值、disabled=false，界面完全看不出这个号已经废了。
func TestClassifyGateway401IsSessionDead(t *testing.T) {
	if got := Classify(http.StatusUnauthorized, gateway401); got != ErrSessionDead {
		t.Fatalf("HTML 401 → %v，want ErrSessionDead", got)
	}
	// 空体 401（网关极简拦截）同样是登录态失效，不是「请求有问题」。
	if got := Classify(http.StatusUnauthorized, ""); got != ErrSessionDead {
		t.Fatalf("空体 401 → %v，want ErrSessionDead", got)
	}
	// 12153 走 marker 层，语义一致（这条是回归保护，不是新行为）。
	if got := Classify(http.StatusUnauthorized, `{"code":12153,"msg":"12153:offline session"}`); got != ErrSessionDead {
		t.Fatalf("12153 → %v，want ErrSessionDead", got)
	}
	// 带业务信封的其他 401 维持既有 ErrClient 语义：上游已经给了业务码，
	// 具体含义由调用方按 code 判，不该在网关侧一刀切成「重登」。
	if got := Classify(http.StatusUnauthorized, `{"code":9999,"msg":"bad token"}`); got != ErrClient {
		t.Fatalf("业务信封 401 → %v，want ErrClient", got)
	}
}

// 错误信息不能把整段 HTML 塞进去：那是日志一行变十行、界面 toast 变成一坨标签的元凶。
func TestErrBodySummaryCompactsHTML(t *testing.T) {
	got := errBodySummary(gateway401)
	if strings.Contains(got, "<html>") || strings.Contains(got, "</") {
		t.Fatalf("摘要里仍有 HTML 标签：%q", got)
	}
	if !strings.Contains(got, "401") {
		t.Fatalf("摘要丢了关键信息（401）：%q", got)
	}
	if len(got) > 120 {
		t.Fatalf("摘要过长（%d 字节）：%q", len(got), got)
	}
	// 非 HTML body 保持原样截断，错误码/文案一个字符都不能动。
	jsonBody := `{"code":12153,"msg":"refresh token is invalid"}`
	if got := errBodySummary(jsonBody); got != jsonBody {
		t.Fatalf("JSON body 被改写：%q", got)
	}
}

// 401 → 刷一次 token → 重试成功：access token 被别处轮换掉时能自愈，不必等用户重登。
func TestBillingMeterJSONReauthOnGateway401(t *testing.T) {
	billingCalls := 0
	refreshed := false
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			refreshed = true
			return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`), nil
		}
		billingCalls++
		if billingCalls == 1 {
			if got := r.Header.Get("Authorization"); got != "Bearer oldat" {
				return nil, errWrongAuth(got)
			}
			return htmlResp(401, gateway401), nil
		}
		// 第二次必须带上刷新后的 token，否则等于没刷新。
		if got := r.Header.Get("Authorization"); got != "Bearer newat" {
			return nil, errWrongAuth(got)
		}
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"totalCredits":1}}`), nil
	})
	a := &auth.Auth{
		AccessToken:  "oldat",
		RefreshToken: "oldrt",
		ExpiresAt:    1,
		FilePath:     filepath.Join(t.TempDir(), "a.json"),
	}
	_, err := c.billingMeterJSON(a, []string{"/v2/billing/meter/get-user-resource"}, http.MethodPost, map[string]any{})
	if err != nil {
		t.Fatalf("刷新后重试应当成功，得到：%v", err)
	}
	if !refreshed {
		t.Fatal("没有触发 token 刷新")
	}
	if billingCalls != 2 {
		t.Fatalf("billing 请求次数=%d，want 2（401 一次 + 重试一次）", billingCalls)
	}
}

// 刷新也失败（12153）时，返回的必须是**刷新的错误**——它指向「只能重登」；
// 把网关的 HTML 401 抛出去，用户只会以为软件坏了。
func TestBillingMeterJSONReauthFailsSurfacesRefreshError(t *testing.T) {
	billingCalls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			return jsonResp(401, `{"code":12153,"msg":"12153:refresh token is invalid","requestId":"x"}`), nil
		}
		billingCalls++
		return htmlResp(401, gateway401), nil
	})
	a := &auth.Auth{
		AccessToken:  "oldat",
		RefreshToken: "oldrt",
		ExpiresAt:    1,
		FilePath:     filepath.Join(t.TempDir(), "a.json"),
	}
	_, err := c.billingMeterJSON(a, []string{"/v2/billing/meter/daily-checkin"}, http.MethodPost, map[string]any{})
	if err == nil {
		t.Fatal("refresh 也失败时必须报错")
	}
	if !strings.Contains(err.Error(), "12153") {
		t.Fatalf("错误应来自刷新（12153），实际：%v", err)
	}
	if strings.Contains(err.Error(), "<html>") {
		t.Fatalf("错误里不该出现 HTML：%v", err)
	}
	// 401 只该重试一次：反复刷新只会在上游风控上刷次数。
	if billingCalls != 1 {
		t.Fatalf("billing 请求次数=%d，want 1（刷新失败即返回，不再重试）", billingCalls)
	}
}

func errWrongAuth(got string) error { return &authHeaderError{got: got} }

type authHeaderError struct{ got string }

func (e *authHeaderError) Error() string { return "unexpected Authorization: " + e.got }
