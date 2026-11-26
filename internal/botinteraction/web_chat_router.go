package botinteraction

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/runtime/a2a"
)

const routeQuestion = "请确认这条消息是要继续哪项任务，还是发起一项新任务？"

type webRouterInput struct {
	a2a.RoutingInput
	ClarificationID string `json:"clarificationId,omitempty"`
}

func (b *ChatBridge) routerInput(item domain.BotInteraction, owner string) (webRouterInput, error) {
	var input webRouterInput
	if item.A2ARouterInput != "" {
		err := json.Unmarshal([]byte(item.A2ARouterInput), &input)
		return input, err
	}
	tickets, err := b.store.WebRoutingCandidates(item)
	if err != nil {
		return input, err
	}
	for _, ticket := range tickets {
		if ticket.Title != "" {
			input.Candidates = append(input.Candidates, a2a.RoutingCandidate{ID: ticket.ID, Title: ticket.Title})
		}
	}
	if clarifiedWebTicket(input.Candidates, item) != "" {
		input.ClarificationID, err = b.store.WebPendingClarification(item)
		if err != nil {
			return input, err
		}
		input.AwaitingTicketName = input.ClarificationID != ""
	}
	history, err := b.store.BotHistory(item.SessionID)
	if err != nil {
		return input, err
	}
	for _, previous := range history {
		if !previous.CreatedAt.Before(item.CreatedAt) &&
			(!previous.CreatedAt.Equal(item.CreatedAt) || previous.ID >= item.ID) {
			continue
		}
		user := domain.NotificationText(previous.Content, 360)
		assistant := domain.NotificationText(previous.Reply, 360)
		if user != "" && assistant != "" {
			input.History = append(input.History, a2a.ConversationTurn{User: user, Assistant: assistant})
		}
	}
	input.History = input.History[max(0, len(input.History)-2):]
	raw, err := json.Marshal(input)
	if err != nil {
		return input, err
	}
	return input, b.store.SaveWebRouterInput(item, owner, string(raw))
}

func (b *ChatBridge) runRouter(ctx context.Context, owner string, item domain.BotInteraction) error {
	if b.router == nil || (item.StopRequested && item.A2ARouterInput == "") {
		return b.store.ResolveWebRoute(item, owner, domain.WebRouteDecision{Action: "ask_clarification", Question: routeQuestion})
	}
	input, err := b.routerInput(item, owner)
	if err != nil {
		return err
	}
	execution, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var task a2a.Task
	if item.A2ARouterTaskID != "" {
		task, err = b.router.Get(execution, item.A2ARouterTaskID)
	} else {
		task, err = b.router.Send(execution, a2a.SendInput{
			MessageID:   repo.BotMessageID("web-chat-router", item.ID),
			Instruction: item.Content, Routing: &input.RoutingInput,
		})
		if err == nil {
			if err := b.store.SaveWebRouterTask(item, owner, task.ID); err != nil {
				return err
			}
		}
	}
	if err == nil {
		task, err = b.router.WaitWithStop(execution, task, func() (bool, error) {
			return b.store.BridgeStopRequested(item.BotID, item.ID)
		})
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if a2a.Retryable(err) {
		return b.store.RetryBridgeTurn(item, owner)
	}
	action, ticketID, question := webRouteDecision(task, input.RoutingInput, item.Content)
	decision := domain.WebRouteDecision{Action: action, TicketID: ticketID, Question: question}
	if input.ClarificationID != "" && action == "append_to_ticket" && clarifiedWebTicket(input.Candidates, item) == ticketID {
		decision.ClarificationID = input.ClarificationID
	}
	return b.store.ResolveWebRoute(item, owner, decision)
}

func clarifiedWebTicket(candidates []a2a.RoutingCandidate, item domain.BotInteraction) string {
	if item.InputDocument != "" {
		return ""
	}
	name := strings.Trim(strings.TrimSpace(item.Content), "。.!！?？")
	for _, candidate := range candidates {
		if identifiesWebTicket(candidates, item.Content, candidate.ID, name) {
			return candidate.ID
		}
	}
	return ""
}

func webRouteDecision(task a2a.Task, input a2a.RoutingInput, message string) (action, ticketID, question string) {
	if task.Status.State != "TASK_STATE_COMPLETED" {
		return "ask_clarification", "", routeQuestion
	}
	var report json.RawMessage
	for _, artifact := range task.Artifacts {
		for _, part := range artifact.Parts {
			if len(part.Data) != 0 {
				if report != nil {
					return "ask_clarification", "", routeQuestion
				}
				report = part.Data
			}
		}
	}
	var result struct {
		Action    string `json:"action"`
		TicketID  string `json:"ticketId"`
		Question  string `json:"question"`
		Reference string `json:"reference"`
	}
	if err := json.Unmarshal(report, &result); err == nil {
		switch result.Action {
		case "reply_in_thread", "create_ticket":
			if result.TicketID == "" && result.Question == "" && result.Reference == "" {
				return result.Action, "", ""
			}
		case "append_to_ticket":
			if result.Question == "" && identifiesWebTicket(input.Candidates, message, result.TicketID, result.Reference) {
				return result.Action, result.TicketID, ""
			}
		case "ask_clarification":
			if question := domain.NotificationText(result.Question, 1000); question != "" && result.TicketID == "" && result.Reference == "" {
				return result.Action, "", question
			}
		}
	}
	return "ask_clarification", "", routeQuestion
}

func identifiesWebTicket(candidates []a2a.RoutingCandidate, message, target, reference string) bool {
	reference = strings.ToLower(strings.TrimSpace(reference))
	length := utf8.RuneCountInString(reference)
	if length < 3 || length > 80 || !strings.Contains(strings.ToLower(message), reference) {
		return false
	}
	matched := ""
	for _, candidate := range candidates {
		if strings.Contains(strings.ToLower(candidate.Title), reference) {
			if matched != "" {
				return false
			}
			matched = candidate.ID
		}
	}
	return matched != "" && matched == target
}
