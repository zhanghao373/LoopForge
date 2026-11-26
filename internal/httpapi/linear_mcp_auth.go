package httpapi

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentflow/internal/tracker"

	"github.com/gin-gonic/gin"
)

const linearMCPCallbackPath = "/api/sync/linear/mcp/callback"

type linearMCPFlow struct {
	state, verifier, clientID, redirectURI, projectID string
	expires                                           time.Time
}

type linearMCPAuth struct {
	client           *tracker.LinearMCPOAuth
	dir, redirectURI string
	mu               sync.Mutex
	flow             map[int64]linearMCPFlow
	clientID         string
}

func newLinearMCPAuth() *linearMCPAuth {
	a := &linearMCPAuth{
		client: tracker.NewLinearMCPOAuth(),
		dir:    strings.TrimSpace(os.Getenv("AGENTFLOW_LINEAR_MCP_DIR")),
		flow:   make(map[int64]linearMCPFlow),
	}
	base, err := url.Parse(strings.TrimSpace(os.Getenv("AGENTFLOW_LINEAR_MCP_PUBLIC_URL")))
	if err == nil && base.Host != "" && base.User == nil && base.RawQuery == "" && base.Fragment == "" && (base.Path == "" || base.Path == "/") {
		ip := net.ParseIP(base.Hostname())
		if base.Scheme == "https" || base.Scheme == "http" && (base.Hostname() == "localhost" || ip != nil && ip.IsLoopback()) {
			base.Path = linearMCPCallbackPath
			a.redirectURI = base.String()
		}
	}
	return a
}

func (a *linearMCPAuth) enabled() bool { return a.dir != "" && a.redirectURI != "" }

func (a *linearMCPAuth) begin(ctx context.Context, uid int64, projectID string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.clientID == "" {
		clientID, err := a.client.Register(ctx, a.redirectURI)
		if err != nil {
			return "", err
		}
		a.clientID = clientID
	}
	for id, flow := range a.flow {
		if time.Now().After(flow.expires) {
			delete(a.flow, id)
		}
	}
	state := base64.RawURLEncoding.EncodeToString(randomLinearBytes(32))
	verifier := base64.RawURLEncoding.EncodeToString(randomLinearBytes(32))
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	a.flow[uid] = linearMCPFlow{state, verifier, a.clientID, a.redirectURI, projectID, time.Now().Add(10 * time.Minute)}
	return a.client.AuthorizeURL(a.clientID, a.redirectURI, state, challenge), nil
}

func randomLinearBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func (a *linearMCPAuth) complete(ctx context.Context, uid int64, secret, state, code, oauthError string) (projectID, result string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	flow, ok := a.flow[uid]
	if !ok || time.Now().After(flow.expires) || subtle.ConstantTimeCompare([]byte(flow.state), []byte(state)) != 1 {
		return "", "expired"
	}
	delete(a.flow, uid)
	if oauthError != "" {
		return flow.projectID, "denied"
	}
	if code == "" {
		return flow.projectID, "failed"
	}
	grant, err := a.client.Exchange(ctx, flow.clientID, flow.redirectURI, code, flow.verifier)
	if err != nil || a.save(uid, secret, grant) != nil {
		return flow.projectID, "failed"
	}
	return flow.projectID, "connected"
}

func linearGrantCipher(secret string) (cipher.AEAD, error) {
	if secret == "" {
		return nil, errNoOAuthStateSecret
	}
	key := sha256.Sum256([]byte("LoopForge Linear MCP grant v1\x00" + secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (a *linearMCPAuth) grantFile(uid int64) string {
	return filepath.Join(a.dir, strconv.FormatInt(uid, 10)+".grant")
}

func (a *linearMCPAuth) load(uid int64, secret string) (tracker.LinearMCPGrant, error) {
	var grant tracker.LinearMCPGrant
	gcm, err := linearGrantCipher(secret)
	if err != nil {
		return grant, err
	}
	raw, err := os.ReadFile(a.grantFile(uid))
	if err != nil {
		return grant, err
	}
	size := gcm.NonceSize()
	if len(raw) < size {
		return grant, tracker.ErrLinearMCPAuthorization
	}
	plain, err := gcm.Open(nil, raw[:size], raw[size:], []byte(strconv.FormatInt(uid, 10)))
	if err != nil {
		return grant, tracker.ErrLinearMCPAuthorization
	}
	if json.Unmarshal(plain, &grant) != nil || grant.AccessToken == "" {
		return tracker.LinearMCPGrant{}, tracker.ErrLinearMCPAuthorization
	}
	return grant, nil
}

func (a *linearMCPAuth) save(uid int64, secret string, grant tracker.LinearMCPGrant) error {
	gcm, err := linearGrantCipher(secret)
	if err != nil {
		return err
	}
	body, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	nonce := randomLinearBytes(gcm.NonceSize())
	raw := gcm.Seal(nonce, nonce, body, []byte(strconv.FormatInt(uid, 10)))
	if err := os.MkdirAll(a.dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(a.dir, ".grant-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(raw)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), a.grantFile(uid))
}

func (a *linearMCPAuth) access(ctx context.Context, uid int64, secret string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	grant, err := a.load(uid, secret)
	if errors.Is(err, os.ErrNotExist) {
		return "", tracker.ErrLinearMCPAuthorization
	}
	if err != nil {
		if errors.Is(err, tracker.ErrLinearMCPAuthorization) {
			return "", err
		}
		return "", errors.New("读取 Linear MCP 连接失败")
	}
	if grant.ExpiresAt.IsZero() || time.Until(grant.ExpiresAt) > 45*time.Second {
		return grant.AccessToken, nil
	}
	refreshed, err := a.client.Refresh(ctx, grant)
	if err != nil {
		return "", err
	}
	if err := a.save(uid, secret, refreshed); err != nil {
		return "", errors.New("保存 Linear MCP 连接失败")
	}
	return refreshed.AccessToken, nil
}

func (h *syncHandlers) linearMCPIdentity(c *gin.Context) (int64, bool) {
	user, ok := h.requestUser(c.Request, false)
	c.Header("Cache-Control", "no-store")
	if !ok || user == nil || user.ID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先登录 LoopForge"})
		return 0, false
	}
	return user.ID, true
}

func (h *syncHandlers) linearMCPSecret() string {
	return h.getAuthClient().Config().SessionSecret
}

func (h *syncHandlers) linearMCPStatus(c *gin.Context) {
	uid, ok := h.linearMCPIdentity(c)
	if !ok {
		return
	}
	enabled := h.linearMCP.enabled()
	connected := false
	if enabled {
		h.linearMCP.mu.Lock()
		grant, err := h.linearMCP.load(uid, h.linearMCPSecret())
		h.linearMCP.mu.Unlock()
		connected = err == nil && (grant.RefreshToken != "" || grant.ExpiresAt.IsZero() || time.Now().Before(grant.ExpiresAt))
	}
	c.JSON(http.StatusOK, gin.H{"enabled": enabled, "connected": connected})
}

func (h *syncHandlers) linearMCPConnect(c *gin.Context) {
	uid, ok := h.linearMCPIdentity(c)
	if !ok {
		return
	}
	if !h.linearMCP.enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "这个 LoopForge 实例尚未配置 Linear MCP 回调"})
		return
	}
	var req struct {
		ProjectID string `json:"projectId"`
	}
	if !decodeLimit(c, &req, 4096) || !h.allowLinearImport(c, strings.TrimSpace(req.ProjectID)) {
		return
	}
	redirect, _ := url.Parse(h.linearMCP.redirectURI)
	if !strings.EqualFold(redirect.Host, c.Request.Host) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请从 " + redirect.Scheme + "://" + redirect.Host + " 打开 LoopForge 后再授权"})
		return
	}
	location, err := h.linearMCP.begin(c.Request.Context(), uid, strings.TrimSpace(req.ProjectID))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"authorizationUrl": location})
}

func (h *syncHandlers) linearMCPCallback(c *gin.Context) {
	uid, ok := h.linearMCPIdentity(c)
	if !ok {
		return
	}
	c.Header("Referrer-Policy", "no-referrer")
	projectID, result := h.linearMCP.complete(c.Request.Context(), uid, h.linearMCPSecret(), c.Query("state"), c.Query("code"), c.Query("error"))
	params := url.Values{"linearMCP": {result}}
	if projectID != "" {
		params.Set("projectId", projectID)
	}
	c.Redirect(http.StatusSeeOther, "/?"+params.Encode()+"#/issues")
}

func (h *syncHandlers) linearMCPDisconnect(c *gin.Context) {
	uid, ok := h.linearMCPIdentity(c)
	if !ok {
		return
	}
	h.linearMCP.mu.Lock()
	defer h.linearMCP.mu.Unlock()
	delete(h.linearMCP.flow, uid)
	if h.linearMCP.enabled() {
		if err := os.Remove(h.linearMCP.grantFile(uid)); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "暂时无法断开 Linear MCP"})
			return
		}
	}
	c.Status(http.StatusNoContent)
}
