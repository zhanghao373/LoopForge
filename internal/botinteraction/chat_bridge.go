package botinteraction

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/runtime/a2a"
)

var (
	ErrInvalidChatEvent = errors.New("invalid chat message")
	ErrChatNotAllowed   = errors.New("sender or conversation is not allowed")
	ErrChatConflict     = errors.New("message ID is already bound to different content")
)

type ChatEvent struct {
	MessageID     string `json:"messageId"`
	Kind          string `json:"kind"`
	ChatID        string `json:"chatId,omitempty"`
	SenderID      string `json:"senderId"`
	Mentioned     bool   `json:"mentioned,omitempty"`
	Text          string `json:"text"`
	InputDocument string `json:"-"`
	WebThread     string `json:"-"`
	WebTicketID   string `json:"-"`
	Delegate      bool   `json:"-"`
}

type ChatBridge struct {
	store       *repo.Store
	client      *a2a.Client
	router      *a2a.Client
	botID       string
	senders     map[string]bool
	groups      map[string]bool
	webStop     bool
	webLimits   WebChatLimits
	roomContext bool
}

type WebChatLimits struct {
	MaxPending int
	MaxRunning int
}

func NewChatBridge(store *repo.Store, client *a2a.Client, name string, senders, groups []string) (*ChatBridge, error) {
	if !validChatID(name) || len(name) > 100 {
		return nil, errors.New("chat bridge needs a name, an A2A client, and permitted senders")
	}
	return newChatBridge(store, client, "chat-bridge:"+name, senders, groups)
}

// NewWebChat keeps LoopForge accounts separate from external sender IDs.
func NewWebChat(store *repo.Store, client *a2a.Client, senders []string, limits WebChatLimits, router *a2a.Client) (*ChatBridge, error) {
	if limits.MaxRunning < 1 || limits.MaxPending < limits.MaxRunning {
		return nil, errors.New("web chat limits require 1 <= maximum running <= maximum pending")
	}
	bridge, err := newChatBridge(store, client, "web-chat:loopforge", senders, nil)
	if err != nil {
		return nil, err
	}
	bridge.webStop = true
	bridge.webLimits = limits
	bridge.router = router
	return bridge, nil
}

func newChatBridge(store *repo.Store, client *a2a.Client, botID string, senders, groups []string) (*ChatBridge, error) {
	if store == nil || client == nil || len(senders) == 0 {
		return nil, errors.New("chat bridge needs a store, an A2A client, and permitted senders")
	}
	b := &ChatBridge{store: store, client: client, botID: botID, senders: map[string]bool{}, groups: map[string]bool{}}
	for _, id := range senders {
		if !validChatID(id) {
			return nil, errors.New("invalid permitted sender")
		}
		b.senders[id] = true
	}
	for _, id := range groups {
		if !validChatID(id) {
			return nil, errors.New("invalid permitted group")
		}
		b.groups[id] = true
	}
	return b, nil
}

func (b *ChatBridge) AllowsSender(sender string) bool {
	return b.senders[sender]
}

// UseRoomContext is configured at startup for an executor that owns conversation history.
func (b *ChatBridge) UseRoomContext() {
	b.roomContext = true
}

func (b *ChatBridge) Stop(sender, thread, id string) error {
	if !b.webStop || !b.AllowsSender(sender) {
		return ErrChatNotAllowed
	}
	return b.store.RequestBridgeStop(b.botID, sender, webChatScope(sender, thread), id)
}

func (b *ChatBridge) DirectTurns(sender, thread string) ([]domain.BotInteraction, error) {
	if !b.AllowsSender(sender) {
		return nil, ErrChatNotAllowed
	}
	return b.store.BridgeDirectTurns(b.botID, sender, webChatScope(sender, thread))
}

func (b *ChatBridge) WebThreads(sender string) ([]repo.BridgeThread, error) {
	if !b.webStop || !b.AllowsSender(sender) {
		return nil, ErrChatNotAllowed
	}
	return b.store.BridgeThreads(b.botID, sender)
}

func webChatScope(sender, thread string) string {
	if thread == "" {
		return sender
	}
	return sender + "/" + thread
}

func validChatID(id string) bool {
	return id != "" && len(id) <= 191 && !strings.ContainsAny(id, "\x00\r\n") && id == strings.TrimSpace(id)
}

func (b *ChatBridge) Accept(event ChatEvent) (string, error) {
	if !validChatID(event.MessageID) || !validChatID(event.SenderID) ||
		len(event.Text) > 4000 || strings.TrimSpace(event.Text) == "" ||
		len(event.InputDocument) > 4000 || strings.ContainsRune(event.InputDocument, '\x00') {
		return "", ErrInvalidChatEvent
	}
	if !b.AllowsSender(event.SenderID) {
		return "", ErrChatNotAllowed
	}
	chat := event.SenderID
	switch event.Kind {
	case "single":
		if b.webStop {
			chat = webChatScope(event.SenderID, event.WebThread)
		}
	case "group":
		if !validChatID(event.ChatID) {
			return "", ErrInvalidChatEvent
		}
		if !event.Mentioned || !b.groups[event.ChatID] {
			return "", ErrChatNotAllowed
		}
		chat = event.ChatID
	default:
		return "", ErrInvalidChatEvent
	}
	item := domain.BotInteraction{
		ID: repo.BotMessageID(b.botID, event.MessageID), BotID: b.botID,
		SenderID: event.SenderID, Content: domain.NotificationText(event.Text, 4000),
		InputDocument: event.InputDocument,
		WebTicketID:   event.WebTicketID,
		WebAutoRoute:  b.router != nil && !event.Delegate && event.WebTicketID == "",
	}
	if event.Delegate || event.WebTicketID != "" {
		if b.webLimits.MaxPending == 0 || (event.Delegate && event.WebTicketID != "") {
			return "", ErrInvalidChatEvent
		}
		if event.Delegate {
			item.WebTicketID = repo.WebChatTicketID(item.ID)
		} else if _, err := hex.DecodeString(item.WebTicketID); err != nil ||
			len(item.WebTicketID) != 64 || item.WebTicketID != strings.ToLower(item.WebTicketID) {
			return "", ErrInvalidChatEvent
		}
	}
	var inserted bool
	var err error
	if b.webLimits.MaxPending > 0 {
		inserted, err = b.store.EnqueueWebChat(item, chat, b.webLimits.MaxPending, event.Delegate)
	} else {
		inserted, err = b.store.EnqueueBotInteraction(item, event.Kind, chat)
	}
	if err != nil {
		return "", err
	}
	if !inserted {
		saved, err := b.store.BotInteraction(item.ID)
		if err != nil {
			return "", err
		}
		sameDestination := saved.WebTicketID == item.WebTicketID
		if saved.WebAutoRoute {
			sameDestination = !event.Delegate && event.WebTicketID == ""
		}
		if saved.ConversationID != repo.BotConversationID(b.botID, event.Kind, chat) ||
			saved.SenderID != item.SenderID || saved.Content != item.Content ||
			saved.InputDocument != item.InputDocument || !sameDestination {
			return "", ErrChatConflict
		}
	}
	return item.ID, nil
}

func (b *ChatBridge) Lookup(id string) (string, string, error) {
	item, err := b.store.BridgeInteraction(b.botID, id)
	return item.Status, item.Reply, err
}

func (b *ChatBridge) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		owner := randomID()
		claimed, err := b.store.ClaimBotConnection(b.botID, owner, time.Now().UTC())
		if err == nil && claimed {
			err = b.runOwned(ctx, owner)
			_ = b.store.ReleaseBotConnection(b.botID, owner)
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("chat bridge: processing unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *ChatBridge) runOwned(ctx context.Context, owner string) error {
	if err := b.store.RequeueBridgeTurns(b.botID, owner); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ok, err := b.store.RenewBotConnection(b.botID, owner, "", "", time.Now().UTC())
				if err != nil || !ok {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-done
	}()
	lanes := 1
	if b.webStop {
		lanes = 2
	}
	results := make(chan error, lanes)
	for range lanes {
		go func() { results <- b.runTurns(ctx, owner) }()
	}
	err := <-results
	cancel()
	for range lanes - 1 {
		<-results
	}
	return err
}

func (b *ChatBridge) runTurns(ctx context.Context, owner string) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		item, err := b.store.ClaimBridgeTurn(b.botID, owner, b.webLimits.MaxRunning)
		if err != nil {
			return err
		}
		if item == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				continue
			}
		}
		if err := b.runTurn(ctx, owner, *item); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (b *ChatBridge) conversation(item domain.BotInteraction, owner string) ([]a2a.ConversationTurn, error) {
	if b.webStop && item.A2AConversation != "" {
		var turns []a2a.ConversationTurn
		err := json.Unmarshal([]byte(item.A2AConversation), &turns)
		return turns, err
	}
	history, err := b.store.BotHistory(item.SessionID)
	if err != nil {
		return nil, err
	}
	const maxTurns = 4
	turns := make([]a2a.ConversationTurn, 0, maxTurns)
	for _, message := range history[max(0, len(history)-maxTurns):] {
		previous, err := executionInput(message)
		if err != nil {
			return nil, err
		}
		priorUser := domain.NotificationText(previous.Instruction, 800)
		if message.SenderID != item.SenderID {
			priorUser = "【群成员】" + priorUser
		}
		turns = append(turns, a2a.ConversationTurn{
			User: priorUser, Assistant: domain.NotificationText(message.Reply, 800),
		})
	}
	if b.webStop {
		raw, err := json.Marshal(turns)
		if err != nil {
			return nil, err
		}
		if err := b.store.SaveBridgeConversation(item, owner, string(raw)); err != nil {
			return nil, err
		}
	}
	return turns, nil
}

func executionInput(item domain.BotInteraction) (domain.WebRouteContinuation, error) {
	input := domain.WebRouteContinuation{Instruction: item.Content, Document: item.InputDocument}
	if item.WebContinuation == "" {
		return input, nil
	}
	err := json.Unmarshal([]byte(item.WebContinuation), &input)
	return input, err
}

func (b *ChatBridge) runTurn(ctx context.Context, owner string, item domain.BotInteraction) error {
	if item.NeedsWebRoute() {
		return b.runRouter(ctx, owner, item)
	}
	execution, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var task a2a.Task
	var err error
	if b.webStop && item.A2ATaskID != "" {
		task, err = b.client.Get(execution, item.A2ATaskID)
	} else {
		turns := []a2a.ConversationTurn{}
		var roomContext *a2a.RoomContext
		if b.roomContext {
			scope := item.ConversationID
			if item.WebTicketID != "" {
				scope = repo.BotConversationID("web-chat-room", scope, item.WebTicketID)
			}
			roomContext = &a2a.RoomContext{
				AccountID: item.SenderID, Scope: scope,
			}
		} else {
			var historyErr error
			turns, historyErr = b.conversation(item, owner)
			if historyErr != nil {
				return historyErr
			}
		}
		input, inputErr := executionInput(item)
		if inputErr != nil {
			return inputErr
		}
		var documents map[string]string
		if input.Document != "" {
			documents = map[string]string{"attachment.txt": input.Document}
		}
		task, err = b.client.Send(execution, a2a.SendInput{
			MessageID: item.ID, Instruction: input.Instruction, Conversation: turns,
			Documents: documents, RoomContext: roomContext,
		})
		if err == nil && b.webStop {
			if linkErr := b.store.SaveBridgeTask(item, owner, task.ID); linkErr != nil {
				return linkErr
			}
		}
	}
	if err == nil {
		var stopRequested func() (bool, error)
		if b.webStop {
			stopRequested = func() (bool, error) {
				return b.store.BridgeStopRequested(item.BotID, item.ID)
			}
		}
		task, err = b.client.WaitWithStop(execution, task, stopRequested)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if b.webStop && err != nil {
		if a2a.Retryable(err) {
			return b.store.RetryBridgeTurn(item, owner)
		}
		stopping, readErr := b.store.BridgeStopRequested(item.BotID, item.ID)
		if readErr != nil {
			return readErr
		}
		if stopping {
			return b.store.RetryBridgeTurn(item, owner)
		}
	}
	reply, state := b.taskReply(task)
	return b.store.FinishBridgeTurn(item, owner, reply, state, err != nil)
}

func (b *ChatBridge) taskReply(task a2a.Task) (reply, state string) {
	if b.webStop && task.Status.State == "TASK_STATE_CANCELED" {
		return "", "canceled"
	}
	if task.Status.State == "TASK_STATE_COMPLETED" {
		for _, artifact := range task.Artifacts {
			for _, part := range artifact.Parts {
				if reply := domain.NotificationText(part.Text, 4000); reply != "" {
					return reply, "replied"
				}
			}
		}
	}
	if b.webStop && task.Status.State == "TASK_STATE_FAILED" && task.Status.Message != nil {
		for _, part := range task.Status.Message.Parts {
			if part.Text == "deadline_exceeded" {
				return "本次执行已超过时限。", "reply_failed"
			}
		}
	}
	return "本次没有收到有效回复，请稍后重试。", "reply_failed"
}
