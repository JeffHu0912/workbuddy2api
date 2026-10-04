// oauth.go OAuth 设备授权登录（无 PKCE，state 由服务端签发）。
//
// 与 cmd/login 共用同一份 HTTP 实现：StartLogin 申请 state + authUrl，PollLogin 轮询
// 登录结果（含 11217「token not ready」归一）。管理面 OAuth 状态机（internal/oauth）
// 与 CLI（cmd/login）都委托到这里，保证指纹、端点、信封解析口径一致。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// Region 上游区域。
type Region string

const (
	// RegionCN 国内版（copilot.tencent.com / codebuddy.cn）。
	RegionCN Region = "cn"
	// RegionGlobal 国际版（workbuddy.ai）。
	RegionGlobal Region = "global"
)

// NormalizeRegion 规范化 region 字符串（大小写/空白容错）；非法值返回错误。
func NormalizeRegion(s string) (Region, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "cn":
		return RegionCN, nil
	case "global":
		return RegionGlobal, nil
	default:
		return "", fmt.Errorf("未知区域 %q（可选 cn | global）", s)
	}
}

// OAuth 设备授权端点（与 cmd/login、WBCenter 同一套路径）。
const (
	endpointAuthState    = "/v2/plugin/auth/state?platform=CLI"
	endpointAuthToken    = "/v2/plugin/auth/token?state="
	endpointLoginAccount = "/v2/plugin/login/account?state="

	// oauthClientUA 设备授权流的出站 UA（与 cmd/login 及官方 CLI 对齐）。
	oauthClientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

	// cnChatBaseDefault 国内版 chat 基址兜底（Client.ChatBaseCN 未设置时）。
	cnChatBaseDefault = "https://copilot.tencent.com"
)

// regionBases 返回 region 对应的 chat 基址与 Origin/Referer 基础域。
// global → (ChatBaseGlobal|default, https://www.workbuddy.ai)；cn → (ChatBaseCN|default,
// https://www.codebuddy.cn)。Origin 常量复用 headers.go 的 originRefererCN/Global。
func (c *Client) regionBases(region Region) (chatBase, origin string) {
	if region == RegionGlobal {
		return c.globalChatBase(), originRefererGlobal
	}
	if c != nil && c.ChatBaseCN != "" {
		return c.ChatBaseCN, originRefererCN
	}
	return cnChatBaseDefault, originRefererCN
}

// oauthHeaders 设置 OAuth 设备流共享请求头（不含 Origin/Referer——那由 setOAuthOrigin
// 按 region 选定的 origin 单独设置）。
func oauthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", oauthClientUA)
}

// setOAuthOrigin 设置 Origin/Referer（Referer 带尾斜杠，与上游 curl 抓包一致）。
func setOAuthOrigin(req *http.Request, origin string) {
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
}

// StartLogin 申请设备授权，返回 state 与用户需在浏览器打开的授权 URL。
func (c *Client) StartLogin(region Region) (state, authURL string, err error) {
	base, origin := c.regionBases(region)
	req, err := http.NewRequest(http.MethodPost, base+endpointAuthState, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", err
	}
	oauthHeaders(req)
	setOAuthOrigin(req, origin)
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", err
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return "", "", fmt.Errorf("授权响应解析失败: %w", err)
	}
	if st.State == "" {
		return "", "", fmt.Errorf("授权响应缺少 state")
	}
	// 上游偶发缺 authUrl（尤其 global），此时用 base 兜底拼一个登录页。
	url := st.AuthURL
	if url == "" {
		url = base + "/login?state=" + st.State + "&platform=CLI"
	}
	return st.State, url, nil
}

// PollLogin 轮询一次登录结果。
//
// 返回 (nil, nil) 表示用户尚未完成登录（预期状态，前端继续轮询）；返回非 nil *auth.Auth
// 表示登录成功（accessToken/uid 已填充，但尚未落盘）。只有 5xx / 网络错误才返回 error
// （终态失败）；其余 *Error（Status∈(0,500)，含 HTTP 200 + code=11217 token not ready）
// 一律归一为 (nil, nil) 继续 waiting。
func (c *Client) PollLogin(region Region, state string) (*auth.Auth, error) {
	if strings.TrimSpace(state) == "" {
		return nil, fmt.Errorf("缺少 state")
	}
	base, origin := c.regionBases(region)

	req, err := http.NewRequest(http.MethodGet, base+endpointAuthToken+state, nil)
	if err != nil {
		return nil, err
	}
	oauthHeaders(req)
	setOAuthOrigin(req, origin)
	data, err := c.doJSON(req)
	if err != nil {
		var ue *Error
		// 服务端「登录未完成」以业务 code != 0 表达（含 11217）；5xx/网络错误才是真失败。
		if errors.As(err, &ue) && ue.Status > 0 && ue.Status < 500 {
			return nil, nil
		}
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		// 拿不到 token 视为尚未完成，让前端继续轮询。
		return nil, nil
	}
	// global 上游可能不返回 domain，按 region 兜底写入，保证 Realm() 判得准。
	if tok.Domain == "" {
		if region == RegionGlobal {
			tok.Domain = "www.workbuddy.ai"
		} else {
			tok.Domain = "copilot.tencent.com"
		}
	}
	acct := &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Domain:       tok.Domain,
	}
	if tok.ExpiresIn > 0 {
		acct.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	// login/account 拿 uid / nickname / enterpriseId（带 Bearer；失败不视为登录失败）。
	acctReq, err := http.NewRequest(http.MethodGet, base+endpointLoginAccount+state, nil)
	if err == nil {
		oauthHeaders(acctReq)
		setOAuthOrigin(acctReq, origin)
		acctReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		if resp, err := c.HTTP.Do(acctReq); err == nil {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			var env apiEnvelope
			if json.Unmarshal(raw, &env) == nil && env.Code == 0 {
				var info struct {
					UID          string `json:"uid"`
					EnterpriseID string `json:"enterpriseId"`
					Nickname     string `json:"nickname"`
				}
				if json.Unmarshal(env.Data, &info) == nil {
					acct.UID = info.UID
					acct.EnterpriseID = info.EnterpriseID
					acct.Nickname = info.Nickname
				}
			}
		}
	}
	if acct.UID == "" {
		return nil, fmt.Errorf("登录成功但未能获取 uid，请稍后重试或改用 login.sh")
	}
	return acct, nil
}
