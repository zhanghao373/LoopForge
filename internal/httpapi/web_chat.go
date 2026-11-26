package httpapi

import (
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"agentflow/internal/botinteraction"
	"agentflow/internal/repo"
	webassets "agentflow/web"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type webChatHandlers struct {
	*deps
	service *botinteraction.ChatBridge
}

var webThreadPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (h *webChatHandlers) sender(c *gin.Context) string {
	c.Header("Cache-Control", "no-store")
	user, ok := h.requestUser(c.Request, false)
	if !ok || user == nil || user.IsGuest || user.ID == 0 {
		return ""
	}
	stored, ok := h.repo.User(user.ID)
	if !ok || !strings.EqualFold(stored.LoginName, user.LoginName) {
		return ""
	}
	return strconv.FormatInt(user.ID, 10)
}

func (h *webChatHandlers) page(c *gin.Context) {
	sender := h.sender(c)
	if sender == "" {
		c.Redirect(http.StatusFound, "/login.html?returnUrl="+url.QueryEscape(c.Request.URL.RequestURI()))
		return
	}
	if !h.service.AllowsSender(sender) {
		webChatError(c, botinteraction.ErrChatNotAllowed)
		return
	}
	page, err := webassets.Files.ReadFile("agent-chat-assets/agent-chat.html")
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", page)
}

func (h *webChatHandlers) threads(c *gin.Context) {
	sender := h.sender(c)
	if sender == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先登录"})
		return
	}
	threads, err := h.service.WebThreads(sender)
	if err != nil {
		webChatError(c, err)
		return
	}
	c.JSON(http.StatusOK, threads)
}

func webChatThread(c *gin.Context) (string, bool) {
	thread := c.Query("thread")
	if thread == "" || webThreadPattern.MatchString(thread) {
		return thread, true
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "会话标识无效"})
	return "", false
}

type webChatMessage struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	State       string `json:"state"`
	Reply       string `json:"reply,omitempty"`
	HasDocument bool   `json:"hasDocument,omitempty"`
	TicketID    string `json:"ticketId,omitempty"`
	RouteAction string `json:"routeAction,omitempty"`
}

func (h *webChatHandlers) messages(c *gin.Context) {
	sender := h.sender(c)
	if sender == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先登录"})
		return
	}
	thread, valid := webChatThread(c)
	if !valid {
		return
	}
	if c.Request.Method == http.MethodGet {
		turns, err := h.service.DirectTurns(sender, thread)
		if err != nil {
			webChatError(c, err)
			return
		}
		result := make([]webChatMessage, 0, len(turns))
		for _, turn := range turns {
			state := turn.Status
			if turn.StopRequested && (state == "queued" || state == "running") {
				state = "stopping"
			} else if state == "queued" && turn.A2ARetryAt != nil {
				state = "reconnecting"
			} else if state == "running" && turn.NeedsWebRoute() {
				state = "routing"
			}
			result = append(result, webChatMessage{
				ID: turn.ID, Text: turn.Content, State: state, Reply: turn.Reply,
				HasDocument: turn.InputDocument != "",
				TicketID:    turn.WebTicketID,
				RouteAction: turn.WebRouteAction,
			})
		}
		c.JSON(http.StatusOK, result)
		return
	}
	if !webChatSameOrigin(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "请求来源不允许"})
		return
	}
	var input struct {
		MessageID string `json:"messageId"`
		Text      string `json:"text"`
		Document  string `json:"document"`
		TicketID  string `json:"ticketId"`
		Delegate  bool   `json:"delegate"`
	}
	if err := decodeChatJSON(c.Writer, c.Request, &input); err != nil ||
		input.MessageID == "" || input.MessageID != strings.TrimSpace(input.MessageID) || len(input.MessageID) > 128 {
		webChatError(c, botinteraction.ErrInvalidChatEvent)
		return
	}
	id, err := h.service.Accept(botinteraction.ChatEvent{
		MessageID: sender + ":" + input.MessageID, Kind: "single", SenderID: sender, Text: input.Text,
		InputDocument: input.Document, WebThread: thread,
		WebTicketID: input.TicketID, Delegate: input.Delegate,
	})
	if err != nil {
		webChatError(c, err)
		return
	}
	result := gin.H{"id": id}
	if input.Delegate {
		result["ticketId"] = repo.WebChatTicketID(id)
	} else if input.TicketID != "" {
		result["ticketId"] = input.TicketID
	}
	c.JSON(http.StatusAccepted, result)
}

func (h *webChatHandlers) stop(c *gin.Context) {
	sender := h.sender(c)
	if sender == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先登录"})
		return
	}
	if !webChatSameOrigin(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "请求来源不允许"})
		return
	}
	thread, valid := webChatThread(c)
	if !valid {
		return
	}
	id := c.Param("id")
	if _, err := hex.DecodeString(id); len(id) != 64 || err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "消息不存在"})
		return
	}
	if err := h.service.Stop(sender, thread, id); err != nil {
		webChatError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"id": id})
}

func webChatSameOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && origin.Host == r.Host && origin.User == nil &&
		origin.Path == "" && origin.RawQuery == "" && origin.Fragment == "" &&
		(origin.Scheme == "https" || origin.Scheme == "http") &&
		(!secureRequest(r) || origin.Scheme == "https")
}

func webChatError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, botinteraction.ErrInvalidChatEvent):
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入有效的消息"})
	case errors.Is(err, botinteraction.ErrChatNotAllowed):
		c.JSON(http.StatusForbidden, gin.H{"error": "该账号未开通对话试用"})
	case errors.Is(err, botinteraction.ErrChatConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "消息标识重复"})
	case errors.Is(err, repo.ErrWebChatQuotaExceeded):
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "待处理消息已达上限，请等待或停止一条后重试"})
	case errors.Is(err, repo.ErrWebChatTicketNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "此会话中没有这个任务"})
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "消息不存在"})
	default:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "对话服务暂不可用"})
	}
}
