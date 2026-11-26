package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const linearMCPIssuer = "https://mcp.linear.app"
const linearMCPResource = linearMCPIssuer + "/mcp/readonly"

var ErrLinearMCPAuthorization = errors.New("请重新连接 Linear MCP")
var errLinearMCPRequest = errors.New("无法读取 Linear MCP，请稍后重试")
var errLinearMCPRegistration = errors.New("Linear MCP 注册失败，请检查本机回调地址或网络连接")

type LinearMCPOAuth struct {
	http *http.Client
}

func NewLinearMCPOAuth() *LinearMCPOAuth {
	return &LinearMCPOAuth{http: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type LinearMCPGrant struct {
	ClientID     string    `json:"clientId"`
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

func (l *LinearMCPOAuth) Register(ctx context.Context, redirectURI string) (string, error) {
	metadata := map[string]any{
		"client_name":                "LoopForge",
		"redirect_uris":              []string{redirectURI},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      "read",
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		return "", errLinearMCPRegistration
	}
	var result struct {
		ClientID string `json:"client_id"`
	}
	code, err := l.post(ctx, "/register", "application/json", bytes.NewReader(body), &result)
	if err != nil || (code != http.StatusCreated && code != http.StatusOK) || result.ClientID == "" {
		return "", errLinearMCPRegistration
	}
	return result.ClientID, nil
}

func (l *LinearMCPOAuth) AuthorizeURL(clientID, redirectURI, state, challenge string) string {
	values := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirectURI},
		"response_type": {"code"}, "scope": {"read"}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"resource": {linearMCPResource},
	}
	return linearMCPIssuer + "/authorize?" + values.Encode()
}

func (l *LinearMCPOAuth) Exchange(ctx context.Context, clientID, redirectURI, code, verifier string) (LinearMCPGrant, error) {
	return l.token(ctx, clientID, "", url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {redirectURI}, "code_verifier": {verifier},
	})
}

func (l *LinearMCPOAuth) Refresh(ctx context.Context, grant LinearMCPGrant) (LinearMCPGrant, error) {
	if grant.RefreshToken == "" {
		return LinearMCPGrant{}, ErrLinearMCPAuthorization
	}
	return l.token(ctx, grant.ClientID, grant.RefreshToken, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {grant.RefreshToken},
	})
}

func (l *LinearMCPOAuth) token(ctx context.Context, clientID, previousRefresh string, values url.Values) (LinearMCPGrant, error) {
	values.Set("client_id", clientID)
	values.Set("resource", linearMCPResource)
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		Scope        string `json:"scope"`
	}
	status, err := l.post(ctx, "/token", "application/x-www-form-urlencoded", strings.NewReader(values.Encode()), &result)
	if err != nil || status >= 500 {
		return LinearMCPGrant{}, errLinearMCPRequest
	}
	if status != http.StatusOK || result.AccessToken == "" {
		return LinearMCPGrant{}, ErrLinearMCPAuthorization
	}
	for _, scope := range strings.Fields(result.Scope) {
		if scope != "read" {
			return LinearMCPGrant{}, ErrLinearMCPAuthorization
		}
	}
	if result.RefreshToken == "" {
		result.RefreshToken = previousRefresh
	}
	var expiresAt time.Time
	if result.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	}
	return LinearMCPGrant{clientID, result.AccessToken, result.RefreshToken, expiresAt}, nil
}

func (l *LinearMCPOAuth) post(ctx context.Context, path, contentType string, body io.Reader, result any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, linearMCPIssuer+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	res, err := l.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	err = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(result)
	return res.StatusCode, err
}
