package domain

import "time"

type BotConnection struct {
	BotID      string    `json:"botId" gorm:"primaryKey;size:191"`
	Owner      string    `json:"-" gorm:"size:64"`
	LeaseUntil time.Time `json:"-"`
	State      string    `json:"state" gorm:"size:32"`
	LastError  string    `json:"lastError,omitempty" gorm:"size:255"`
	UpdatedAt  time.Time `json:"updatedAt"`
}
type BotConversation struct {
	ID        string    `json:"id" gorm:"primaryKey;size:64"`
	BotID     string    `json:"botId" gorm:"index;size:191"`
	ChatID    string    `json:"chatId" gorm:"size:191"`
	ChatType  string    `json:"chatType" gorm:"size:16"`
	SessionID string    `json:"sessionId" gorm:"size:64"`
	UpdatedAt time.Time `json:"updatedAt"`
}
type BotInteraction struct {
	ID              string     `json:"id" gorm:"primaryKey;index:idx_bot_conversation_turn,priority:3;size:64"`
	BotID           string     `json:"botId" gorm:"index:idx_bot_queue,priority:1;index:idx_bot_sender_status,priority:1;size:191"`
	ConversationID  string     `json:"conversationId" gorm:"index:idx_bot_conversation_turn,priority:1;size:64"`
	SessionID       string     `json:"sessionId" gorm:"index;size:64"`
	WebTicketID     string     `json:"-" gorm:"size:64"`
	WebAutoRoute    bool       `json:"-" gorm:"default:false"`
	WebRouteAction  string     `json:"-" gorm:"size:32"`
	WebContinuation string     `json:"-" gorm:"type:text"`
	SenderID        string     `json:"senderId" gorm:"index:idx_bot_sender_status,priority:2;size:191"`
	RequestID       string     `json:"-" gorm:"size:191"`
	Content         string     `json:"content" gorm:"type:text"`
	InputDocument   string     `json:"-" gorm:"type:text"`
	A2AConversation string     `json:"-" gorm:"column:a2a_conversation;type:text"`
	A2ATaskID       string     `json:"-" gorm:"column:a2a_task_id;type:text"`
	A2ARouterInput  string     `json:"-" gorm:"column:a2a_router_input;type:text"`
	A2ARouterTaskID string     `json:"-" gorm:"column:a2a_router_task_id;type:text"`
	A2ARetryAt      *time.Time `json:"-" gorm:"column:a2a_retry_at"`
	A2ARetryCount   int        `json:"-" gorm:"column:a2a_retry_count;default:0"`
	Quote           string     `json:"quote,omitempty" gorm:"type:text"`
	Action          string     `json:"action,omitempty" gorm:"size:32"`
	CardTaskID      string     `json:"cardTaskId,omitempty" gorm:"size:128"`
	DeliveryID      string     `json:"deliveryId,omitempty" gorm:"size:64"`
	Status          string     `json:"status" gorm:"index:idx_bot_queue,priority:2;index:idx_bot_sender_status,priority:3;size:32"`
	StopRequested   bool       `json:"-" gorm:"default:false"`
	Reply           string     `json:"reply,omitempty" gorm:"type:text"`
	LastError       string     `json:"lastError,omitempty" gorm:"size:255"`
	CreatedAt       time.Time  `json:"createdAt" gorm:"index:idx_bot_queue,priority:3;index:idx_bot_conversation_turn,priority:2"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

func (i BotInteraction) NeedsWebRoute() bool {
	return i.WebAutoRoute && i.WebRouteAction == ""
}

type WebRouteDecision struct {
	Action, TicketID, Question, ClarificationID string
}

type WebRouteContinuation struct {
	PreviousMessageID string `json:"previousMessageId"`
	Instruction       string `json:"instruction"`
	Document          string `json:"document,omitempty"`
}

type WebChatTicket struct {
	ID             string    `gorm:"primaryKey;size:64"`
	BotID          string    `gorm:"size:191"`
	ConversationID string    `gorm:"index:idx_web_ticket_scope,priority:1;size:64"`
	SenderID       string    `gorm:"index:idx_web_ticket_scope,priority:2;size:191"`
	Title          string    `gorm:"size:200"`
	CreatedAt      time.Time `gorm:"index:idx_web_ticket_scope,priority:3"`
}
