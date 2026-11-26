package repo

import (
	"encoding/json"
	"errors"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func assignWebTicket(tx *gorm.DB, item domain.BotInteraction, ticketID string, create bool) error {
	if create {
		title := domain.NotificationText(item.Content, 180)
		return tx.Create(&domain.WebChatTicket{
			ID: ticketID, BotID: item.BotID, ConversationID: item.ConversationID,
			SenderID: item.SenderID, Title: title,
		}).Error
	}
	err := tx.Take(&domain.WebChatTicket{}, "id = ? AND bot_id = ? AND conversation_id = ? AND sender_id = ?",
		ticketID, item.BotID, item.ConversationID, item.SenderID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrWebChatTicketNotFound
	}
	return err
}

func (r *BotInteractionsRepo) WebRoutingCandidates(item domain.BotInteraction) ([]domain.WebChatTicket, error) {
	var tickets []domain.WebChatTicket
	if err := r.DB.Where("bot_id = ? AND conversation_id = ? AND sender_id = ?",
		item.BotID, item.ConversationID, item.SenderID).
		Order("created_at DESC, id DESC").Limit(5).Find(&tickets).Error; err != nil {
		return nil, err
	}
	// Tickets created before titles were stored can still be used as candidates.
	for i := range tickets {
		if tickets[i].Title != "" {
			continue
		}
		var first domain.BotInteraction
		if err := r.DB.Select("content").Where(
			"bot_id = ? AND conversation_id = ? AND sender_id = ? AND web_ticket_id = ?",
			item.BotID, item.ConversationID, item.SenderID, tickets[i].ID).
			Order("created_at, id").Limit(1).Find(&first).Error; err != nil {
			return nil, err
		}
		tickets[i].Title = domain.NotificationText(first.Content, 180)
	}
	return tickets, nil
}

func earlierWebTurns(tx *gorm.DB, item domain.BotInteraction) *gorm.DB {
	return tx.Where("bot_id = ? AND conversation_id = ? AND sender_id = ?", item.BotID, item.ConversationID, item.SenderID).
		Where("created_at < ? OR (created_at = ? AND id < ?)", item.CreatedAt, item.CreatedAt, item.ID)
}

func (r *BotInteractionsRepo) WebPendingClarification(item domain.BotInteraction) (string, error) {
	var prior domain.BotInteraction
	err := earlierWebTurns(r.DB, item).Select("id", "status", "web_route_action").
		Order("created_at DESC, id DESC").Take(&prior).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if prior.Status == "replied" && prior.WebRouteAction == "ask_clarification" {
		return prior.ID, err
	}
	return "", err
}

func (r *BotInteractionsRepo) SaveWebRouterInput(item domain.BotInteraction, owner, input string) error {
	return r.updateClaimedBridgeTurn(item, owner, map[string]any{"a2a_router_input": input}, false)
}

func (r *BotInteractionsRepo) SaveWebRouterTask(item domain.BotInteraction, owner, taskID string) error {
	return r.updateClaimedBridgeTurn(item, owner, map[string]any{"a2a_router_task_id": taskID}, false)
}

// ResolveWebRoute records the decision, creates or validates the ticket, and
// makes the turn available to the executor in one lease-fenced transaction.
func (r *BotInteractionsRepo) ResolveWebRoute(item domain.BotInteraction, owner string, decision domain.WebRouteDecision) error {
	return r.withBridgeLease(item.BotID, owner, func(tx *gorm.DB) error {
		var turn domain.BotInteraction
		if err := r.locking(tx).Where("id = ? AND bot_id = ? AND status = ?",
			item.ID, item.BotID, "running").Take(&turn).Error; err != nil {
			return err
		}
		if !turn.NeedsWebRoute() {
			return errors.New("web route was already resolved")
		}
		changes := map[string]any{
			"a2a_retry_at": nil, "a2a_retry_count": 0, "updated_at": time.Now().UTC(),
		}
		if turn.StopRequested {
			changes["status"] = "canceled"
			return tx.Model(&turn).Updates(changes).Error
		}
		switch decision.Action {
		case "create_ticket":
			decision.TicketID = WebChatTicketID(turn.ID)
			if err := assignWebTicket(tx, turn, decision.TicketID, true); err != nil {
				return err
			}
		case "append_to_ticket":
			err := assignWebTicket(tx, turn, decision.TicketID, false)
			if errors.Is(err, ErrWebChatTicketNotFound) {
				decision = domain.WebRouteDecision{Action: "ask_clarification", Question: "请确认要继续哪项任务？"}
			} else if err != nil {
				return err
			}
			if decision.ClarificationID != "" {
				var prior domain.BotInteraction
				err := earlierWebTurns(tx, turn).Select("id", "content", "input_document", "status", "web_route_action").
					Order("created_at DESC, id DESC").Take(&prior).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				if prior.ID != decision.ClarificationID || prior.Status != "replied" || prior.WebRouteAction != "ask_clarification" {
					decision = domain.WebRouteDecision{Action: "ask_clarification", Question: "请再说明要继续的任务和你希望我做的事。"}
				} else {
					snapshot, err := json.Marshal(domain.WebRouteContinuation{
						PreviousMessageID: prior.ID,
						Instruction:       prior.Content + "\n\n用户指定任务：" + turn.Content,
						Document:          prior.InputDocument,
					})
					if err != nil {
						return err
					}
					changes["web_continuation"] = string(snapshot)
				}
			}
		case "reply_in_thread", "ask_clarification":
		default:
			return errors.New("invalid web route action")
		}
		changes["web_route_action"] = decision.Action
		changes["web_ticket_id"] = decision.TicketID
		if decision.Action == "ask_clarification" {
			changes["status"], changes["reply"] = "replied", decision.Question
		} else {
			// The executor claims again under its conversation or ticket lane.
			changes["status"], changes["session_id"] = "queued", ""
		}
		return tx.Model(&turn).Updates(changes).Error
	})
}
