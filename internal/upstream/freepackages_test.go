package upstream

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// ---------------------------------------------------------------------------
// describeBody：HTML 错误页识别（借鉴 WBCenter）—— 网关层（openresty/apisix）
// 拦截返回 HTML 时换成可操作的人话，非 HTML 保持原文截断。
// ---------------------------------------------------------------------------

func TestDescribeBodyHTMLOpenresty(t *testing.T) {
	html := `<html><head><title>403 Forbidden</title></head><body><center>openresty</center></body></html>`
	got := describeBody(http.StatusForbidden, html)
	if !strings.Contains(got, "token") {
		t.Errorf("403 HTML 应提示 token 失效，got=%q", got)
	}
	if strings.Contains(got, "<html") {
		t.Errorf("不应再带 HTML 标签，got=%q", got)
	}
}

func TestDescribeBodyHTMLNotFound(t *testing.T) {
	got := describeBody(http.StatusNotFound, "<html>404</html>")
	if !strings.Contains(got, "不存在") {
		t.Errorf("404 HTML 应提示接口不存在，got=%q", got)
	}
}

func TestDescribeBodyHTMLOtherStatus(t *testing.T) {
	got := describeBody(http.StatusBadGateway, "  <html>502</html>  ")
	if !strings.Contains(got, "HTML") {
		t.Errorf("5xx HTML 应走通用文案，got=%q", got)
	}
}

func TestDescribeBodyNonHTMLUnchanged(t *testing.T) {
	body := `{"code":11135,"msg":"replace the image"}`
	if got := describeBody(http.StatusBadRequest, body); got != body {
		t.Errorf("非 HTML 应原样返回，got=%q want=%q", got, body)
	}
}

// TestDescribeBodyViaDoJSON 端到端：doJSON 的 *Error.Msg 已转人话，但分类不变。
func TestDescribeBodyViaDoJSON(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader("<html><body>Forbidden openresty</body></html>")),
		}, nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("403 应返回错误")
	}
	if strings.Contains(err.Error(), "<html") {
		t.Errorf("Msg 不应带 HTML 标签: %v", err)
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("Msg 应含 token 处置建议: %v", err)
	}
}

// ---------------------------------------------------------------------------
// IsAlreadyCheckin 增量 marker（"签到过" / "code=10001"）。
// ---------------------------------------------------------------------------

func TestIsAlreadyCheckinExtraMarkers(t *testing.T) {
	cases := []string{
		`{"code":10001,"msg":"今天签到过了，明天再来吧"}`,
		`{"code":10001,"msg":""}`,
	}
	for i, body := range cases {
		c := testClient(func(r *http.Request) (*http.Response, error) {
			return jsonResp(200, body), nil
		})
		if !IsAlreadyCheckin(c.DailyCheckin(&auth.Auth{AccessToken: "at"})) {
			t.Errorf("case %d (%s) 应判为 already", i, body)
		}
	}
}

// ---------------------------------------------------------------------------
// DailyFreePackages：今日套餐切片查询（借鉴 WBCenter）。
// ---------------------------------------------------------------------------

// TestDailyFreePackagesSliceDetails 嵌套 SlicePeriodUsageDetails 形态 + Precise 优先。
func TestDailyFreePackagesSliceDetails(t *testing.T) {
	var gotPath, gotBody string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return jsonResp(200, `{"code":0,"data":{"Accounts":[{
			"PackageCode":"pkg_a","PackageName":"每日免费",
			"SlicePeriodUsageDetails":[{
				"SlicePeriodCapacitySizePrecise":100.5,
				"SlicePeriodCapacityUsedPrecise":40.25,
				"SlicePeriodCapacityRemainPrecise":60.25
			}]
		}]}}`), nil
	})
	packs, err := c.DailyFreePackages(&auth.Auth{AccessToken: "at"}, []string{"pkg_a"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if gotPath != "/billing/meter/get-user-resource-free-packages" {
		t.Errorf("path=%q，应无 /v2 前缀", gotPath)
	}
	if !strings.Contains(gotBody, "SlicePeriodStartTime") || !strings.Contains(gotBody, `"pkg_a"`) {
		t.Errorf("请求体缺切片窗/套餐码: %s", gotBody)
	}
	if len(packs) != 1 {
		t.Fatalf("packs=%d want 1", len(packs))
	}
	p := packs[0]
	if p.Code != "pkg_a" || p.Name != "每日免费" {
		t.Errorf("code/name 错: %+v", p)
	}
	if p.Total != 100.5 || p.Used != 40.25 || p.Remaining != 60.25 {
		t.Errorf("Precise 字段应优先: %+v", p)
	}
}

// TestDailyFreePackagesRowLevelCycle 无嵌套 details 时回退行级 CycleCapacity*，
// 且 total==0 时用 remain+used 兜底。
func TestDailyFreePackagesRowLevelCycle(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Packages":[{
			"packageCode":"pkg_b","packageName":"周包",
			"CycleCapacityRemainPrecise":70,"CycleCapacityUsedPrecise":30
		}]}}`), nil
	})
	packs, err := c.DailyFreePackages(&auth.Auth{AccessToken: "at"}, []string{"pkg_b"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(packs) != 1 {
		t.Fatalf("packs=%d want 1", len(packs))
	}
	p := packs[0]
	if p.Total != 100 { // remain+used 兜底
		t.Errorf("total 兜底错: %+v", p)
	}
	if p.Used != 30 || p.Remaining != 70 {
		t.Errorf("used/remain 错: %+v", p)
	}
}

// TestDailyFreePackagesEmptyCodes 空套餐码直接报错，不发请求。
func TestDailyFreePackagesEmptyCodes(t *testing.T) {
	called := false
	c := testClient(func(r *http.Request) (*http.Response, error) {
		called = true
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	if _, err := c.DailyFreePackages(&auth.Auth{AccessToken: "at"}, nil); err == nil {
		t.Error("空 PackageCodes 应报错")
	}
	if called {
		t.Error("空 PackageCodes 不应发请求")
	}
}

// TestDailyFreePackagesWorkbuddyCNBase workbuddy.cn 域强制改写 base（WBCenter 兼容分支）。
func TestDailyFreePackagesWorkbuddyCNBase(t *testing.T) {
	var gotURL string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		return jsonResp(200, `{"code":0,"data":{"Accounts":[]}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", Domain: "https://www.workbuddy.cn"}
	if _, err := c.DailyFreePackages(a, []string{"x"}); err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.HasPrefix(gotURL, "https://www.workbuddy.cn/") {
		t.Errorf("workbuddy.cn 域应强制改写 base, got=%q", gotURL)
	}
}

// TestPackageCodes 去重保序、滤空（用与 userResourceResp 匹配的响应结构）。
func TestPackageCodes(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"TotalDosage":0,"Accounts":[
			{"PackageCode":"pkg1","PackageName":"A"},
			{"PackageCode":"pkg2","PackageName":"B"},
			{"PackageCode":"pkg1","PackageName":"A-dup"},
			{"PackageCode":"","PackageName":"empty"}
		]}}}}`), nil
	})
	codes, err := c.PackageCodes(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(codes) != 2 || codes[0] != "pkg1" || codes[1] != "pkg2" {
		t.Errorf("codes=%v want [pkg1 pkg2]（去重保序滤空）", codes)
	}
}
