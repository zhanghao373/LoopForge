package repo

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

var (
	ErrWebChatQuotaExceeded  = errors.New("web chat account has too many unfinished turns")
	ErrWebChatTicketNotFound = errors.New("web chat ticket not found in this conversation")
)

func WebChatTicketID(messageID string) string {
	return BotMessageID("web-chat-ticket", messageID)
}

func (r *BotInteractionsRepo) EnqueueWebChat(item domain.BotInteraction, chat string, maxPending int, delegate bool) (bool, error) {
	userID, err := strconv.ParseInt(item.SenderID, 10, 64)
	if err != nil {
		return false, err
	}
	return r.enqueueBotInteraction(item, "single", chat, func(tx *gorm.DB) error {
		// Serialize admissions across an account's conversations and API replicas.
		if err := r.users.lockUser(tx, userID); err != nil {
			return err
		}
		err := tx.Select("id").Take(&domain.BotInteraction{}, "id = ?", item.ID).Error
		if err == nil {
			return gorm.ErrDuplicatedKey
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		pending := tx.Model(&domain.BotInteraction{}).Select("id").
			Where("bot_id = ? AND sender_id = ? AND status IN ?", item.BotID, item.SenderID, []string{"queued", "running"}).
			Limit(maxPending)
		var count int64
		if err := tx.Table("(?) AS pending", pending).Count(&count).Error; err != nil {
			return err
		}
		if count >= int64(maxPending) {
			return ErrWebChatQuotaExceeded
		}
		if item.WebTicketID != "" {
			item.ConversationID = BotConversationID(item.BotID, "single", chat)
			return assignWebTicket(tx, item, item.WebTicketID, delegate)
		}
		return nil
	})
}

func (r *BotInteractionsRepo) BridgeInteraction(botID, id string) (domain.BotInteraction, error) {
	var item domain.BotInteraction
	err := r.DB.First(&item, "id = ? AND bot_id = ?", id, botID).Error
	return item, err
}

type BridgeThread struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func (r *BotInteractionsRepo) BridgeThreads(botID, sender string) ([]BridgeThread, error) {
	prefix := sender + "/"
	var conversations []domain.BotConversation
	err := r.DB.Select("id", "chat_id").
		Where("bot_id = ? AND chat_type = ? AND chat_id LIKE ?", botID, "single", prefix+"%").
		Order("updated_at DESC, id").Limit(20).Find(&conversations).Error
	if err != nil {
		return nil, err
	}
	threads := make([]BridgeThread, 0, len(conversations))
	for _, conversation := range conversations {
		var first domain.BotInteraction
		err := r.DB.Select("content").
			Where("conversation_id = ? AND sender_id = ?", conversation.ID, sender).
			Order("created_at, id").Limit(1).Find(&first).Error
		if err != nil {
			return nil, err
		}
		threads = append(threads, BridgeThread{
			ID:    strings.TrimPrefix(conversation.ChatID, prefix),
			Title: domain.NotificationText(first.Content, 80),
		})
	}
	return threads, nil
}

func (r *BotInteractionsRepo) BridgeDirectTurns(botID, sender, scope string) ([]domain.BotInteraction, error) {
	var rows []domain.BotInteraction
	err := r.DB.Where("bot_id = ? AND conversation_id = ? AND sender_id = ?",
		botID, BotConversationID(botID, "single", scope), sender).
		Order("created_at DESC, id DESC").Limit(20).Find(&rows).Error
	slices.Reverse(rows)
	return rows, err
}

func (r *BotInteractionsRepo) RequestBridgeStop(botID, sender, scope, id string) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var item domain.BotInteraction
		if err := r.locking(tx).Where("id = ? AND bot_id = ? AND conversation_id = ? AND sender_id = ?",
			id, botID, BotConversationID(botID, "single", scope), sender).Take(&item).Error; err != nil {
			return err
		}
		if item.StopRequested || (item.Status != "queued" && item.Status != "running") {
			return nil
		}
		changes := map[string]any{"stop_requested": true, "updated_at": time.Now().UTC()}
		// Only a turn that has never been claimed can stop without contacting A2A.
		if item.Status == "queued" && item.SessionID == "" {
			changes["status"] = "canceled"
		} else if item.Status == "queued" && item.A2ARetryAt != nil {
			changes["a2a_retry_at"] = time.Now().UTC()
		}
		return tx.Model(&item).Updates(changes).Error
	})
}

func (r *BotInteractionsRepo) BridgeStopRequested(botID, id string) (bool, error) {
	var item domain.BotInteraction
	err := r.DB.Select("stop_requested").Where("id = ? AND bot_id = ?", id, botID).Take(&item).Error
	return item.StopRequested, err
}

// RequeueBridgeTurns recovers interrupted read-only A2A turns. Their durable
// message ID is reused, including when the original SendMessage reached A2A.
func (r *BotInteractionsRepo) RequeueBridgeTurns(botID, owner string) error {
	return r.withBridgeLease(botID, owner, func(tx *gorm.DB) error {
		return tx.Model(&domain.BotInteraction{}).Where("bot_id = ? AND status = ?", botID, "running").
			Updates(map[string]any{"status": "queued", "updated_at": time.Now().UTC()}).Error
	})
}

// SaveBridgeConversation freezes the history before sending the first request.
func (r *BotInteractionsRepo) SaveBridgeConversation(item domain.BotInteraction, owner, conversation string) error {
	return r.updateClaimedBridgeTurn(item, owner, map[string]any{"a2a_conversation": conversation}, false)
}

func (r *BotInteractionsRepo) SaveBridgeTask(item domain.BotInteraction, owner, taskID string) error {
	return r.updateClaimedBridgeTurn(item, owner, map[string]any{"a2a_task_id": taskID}, false)
}

func (r *BotInteractionsRepo) RetryBridgeTurn(item domain.BotInteraction, owner string) error {
	attempt := min(item.A2ARetryCount, 5)
	return r.updateClaimedBridgeTurn(item, owner, map[string]any{
		"status":          "queued",
		"a2a_retry_at":    time.Now().UTC().Add(time.Second << attempt),
		"a2a_retry_count": min(attempt+1, 5),
	}, false)
}

// FinishBridgeTurn makes a result available for the transport to deliver.
func (r *BotInteractionsRepo) FinishBridgeTurn(item domain.BotInteraction, owner, reply, status string, requireNoStop bool) error {
	return r.updateClaimedBridgeTurn(item, owner, map[string]any{
		"status": status, "reply": reply, "a2a_retry_at": nil,
	}, requireNoStop)
}

func (r *BotInteractionsRepo) updateClaimedBridgeTurn(item domain.BotInteraction, owner string, changes map[string]any, requireNoStop bool) error {
	return r.withBridgeLease(item.BotID, owner, func(tx *gorm.DB) error {
		query := tx.Model(&domain.BotInteraction{}).
			Where("id = ? AND bot_id = ? AND status = ?", item.ID, item.BotID, "running")
		if requireNoStop {
			query = query.Where("COALESCE(stop_requested, ?) = ?", false, false)
		}
		changes["updated_at"] = time.Now().UTC()
		result := query.Updates(changes)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
}

func (r *BotInteractionsRepo) withBridgeLease(botID, owner string, apply func(*gorm.DB) error) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var lease domain.BotConnection
		if err := r.locking(tx).Where("bot_id = ? AND owner = ? AND lease_until > ?", botID, owner, time.Now().UTC()).First(&lease).Error; err != nil {
			return err
		}
		return apply(tx)
	})
}
