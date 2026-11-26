package repo

import (
	"agentflow/internal/domain"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestBridgeThreadsAreScopedAndUseTheFirstMessage(t *testing.T) {
	var store *Store
	var err error
	if dsn := os.Getenv("AGENTFLOW_BOT_TEST_MYSQL_DSN"); dsn != "" {
		store, err = Open(dsn)
	} else {
		store, err = OpenTest()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	bot := fmt.Sprintf("web-chat:test-%d", time.Now().UnixNano())
	defer store.DB.Where("bot_id = ?", bot).Delete(&domain.BotConversation{})
	defer store.DB.Where("bot_id = ?", bot).Delete(&domain.BotInteraction{})
	for _, turn := range []struct{ sender, scope, text string }{
		{"1", "1/work-a", "First request"},
		{"1", "1/work-a", "Follow-up"},
		{"1", "1", "Default conversation"},
		{"11", "11/work-b", "Private request"},
	} {
		item := domain.BotInteraction{ID: BotMessageID(bot, turn.text), BotID: bot, SenderID: turn.sender, Content: turn.text}
		if ok, err := store.EnqueueBotInteraction(item, "single", turn.scope); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	threads, err := store.BridgeThreads(bot, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0] != (BridgeThread{ID: "work-a", Title: "First request"}) {
		t.Fatalf("unexpected threads: %+v", threads)
	}
}

func TestWebRouterHandoffHonorsStopAndTicketScope(t *testing.T) {
	store, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const bot, owner, sender = "web-router-test", "worker", "1"
	if ok, err := store.ClaimBotConnection(bot, owner, time.Now().UTC()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	enqueue := func(key, scope, ticket string, delegate bool) domain.BotInteraction {
		t.Helper()
		item := domain.BotInteraction{
			ID: BotMessageID(bot, key), BotID: bot, SenderID: sender, Content: key,
			WebTicketID: ticket, WebAutoRoute: !delegate,
		}
		if inserted, err := store.EnqueueWebChat(item, scope, 8, delegate); err != nil || !inserted {
			t.Fatal(inserted, err)
		}
		return item
	}
	claim := func(id string) domain.BotInteraction {
		t.Helper()
		item, err := store.ClaimBridgeTurn(bot, owner, 2)
		if err != nil || item == nil || item.ID != id {
			t.Fatalf("could not claim the expected turn: %v", err)
		}
		return *item
	}
	stopped := claim(enqueue("stop before handoff", sender, "", false).ID)
	if err := store.RequestBridgeStop(bot, sender, sender, stopped.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveWebRoute(stopped, owner, domain.WebRouteDecision{Action: "create_ticket"}); err != nil {
		t.Fatal(err)
	}
	saved, err := store.BotInteraction(stopped.ID)
	if err != nil || saved.Status != "canceled" || saved.WebTicketID != "" {
		t.Fatalf("stop lost before handoff: %+v %v", saved, err)
	}

	const differentTicket = "ticket-from-another-thread"
	delegated := claim(enqueue("explicit ticket", "1/another-thread", differentTicket, true).ID)
	if err := store.FinishBridgeTurn(delegated, owner, "done", "replied", false); err != nil {
		t.Fatal(err)
	}
	followup := claim(enqueue("invalid follow-up", sender, "", false).ID)
	if err := store.ResolveWebRoute(followup, owner, domain.WebRouteDecision{Action: "append_to_ticket", TicketID: differentTicket}); err != nil {
		t.Fatal(err)
	}
	saved, err = store.BotInteraction(followup.ID)
	if err != nil || saved.Status != "replied" || saved.WebRouteAction != "ask_clarification" ||
		saved.WebTicketID != "" || saved.Reply == "" {
		t.Fatalf("route did not reject the foreign ticket: %+v %v", saved, err)
	}
	if next, err := store.ClaimBridgeTurn(bot, owner, 2); err != nil || next != nil {
		t.Fatalf("clarification reached the executor queue: %v %v", next, err)
	}

	foreign := claim(enqueue("unrelated turn", "1/another-thread", "", false).ID)
	if prior, err := store.WebPendingClarification(foreign); err != nil || prior != "" {
		t.Fatalf("clarification leaked from a different origin: %q %v", prior, err)
	}
	if err := store.ResolveWebRoute(foreign, owner, domain.WebRouteDecision{Action: "append_to_ticket", TicketID: differentTicket, ClarificationID: followup.ID}); err != nil {
		t.Fatal(err)
	}
	foreign, err = store.BotInteraction(foreign.ID)
	if err != nil || foreign.Status != "replied" || foreign.WebRouteAction != "ask_clarification" || foreign.WebContinuation != "" {
		t.Fatalf("foreign clarification reached the executor: %+v %v", foreign, err)
	}
}

func TestWebChatAccountQuotaAndReservedExecutionSlot(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_BOT_TEST_MYSQL_DSN")
	var store *Store
	var err error
	if dsn != "" {
		store, err = Open(dsn)
	} else {
		store, err = OpenTest()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	replica := store
	if dsn != "" {
		replica, err = Open(dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer replica.Close()
	}
	bot := fmt.Sprintf("web-chat:quota-%d", time.Now().UnixNano())
	defer store.DB.Where("bot_id = ?", bot).Delete(&domain.BotConnection{})
	defer store.DB.Where("bot_id = ?", bot).Delete(&domain.BotConversation{})
	defer store.DB.Where("bot_id = ?", bot).Delete(&domain.BotInteraction{})
	register := func(suffix string) string {
		t.Helper()
		name := bot + suffix
		if err := store.PutUser(domain.User{LoginName: name}); err != nil {
			t.Fatal(err)
		}
		user, ok := store.UserByLoginName(name)
		if !ok {
			t.Fatal("missing fixture account")
		}
		t.Cleanup(func() { store.DB.Delete(&userRow{}, "id = ?", user.ID) })
		return fmt.Sprint(user.ID)
	}
	// Use non-seeded users and separate threads so the common account row is
	// the serialization point, even when two API connections submit at once.
	alice, bob := register("-a"), register("-b")
	items := make([]domain.BotInteraction, 6)
	outcomes := make([]error, len(items))
	var work sync.WaitGroup
	for i := range items {
		items[i] = domain.BotInteraction{ID: BotMessageID(bot, fmt.Sprint(i)), BotID: bot, SenderID: alice}
		work.Go(func() {
			connection := []*Store{store, replica}[i%2]
			inserted, err := connection.EnqueueWebChat(items[i], alice+"/"+fmt.Sprint(i), 3, false)
			if err == nil && !inserted {
				err = errors.New("new message was not inserted")
			}
			outcomes[i] = err
		})
	}
	work.Wait()
	var admitted int
	for i, err := range outcomes {
		if err == nil {
			admitted++
			if replay, retryErr := replica.EnqueueWebChat(items[i], alice+"/"+fmt.Sprint(i), 3, false); retryErr != nil || replay {
				t.Fatalf("replay at quota: inserted=%t err=%v", replay, retryErr)
			}
		} else if !errors.Is(err, ErrWebChatQuotaExceeded) {
			t.Fatal(err)
		}
	}
	if admitted != 3 {
		t.Fatalf("admitted %d turns, want 3", admitted)
	}

	const owner = "worker"
	if ok, err := store.ClaimBotConnection(bot, owner, time.Now().UTC()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	first, err := store.ClaimBridgeTurn(bot, owner, 1)
	if err != nil || first == nil || first.SenderID != alice {
		t.Fatalf("claim first account turn: %v", err)
	}
	if err := store.RetryBridgeTurn(*first, owner); err != nil {
		t.Fatal(err)
	}
	other := domain.BotInteraction{ID: BotMessageID(bot, "peer"), BotID: bot, SenderID: bob}
	if inserted, err := replica.EnqueueWebChat(other, bob, 3, false); err != nil || !inserted {
		t.Fatal(inserted, err)
	}
	if turn, err := store.ClaimBridgeTurn(bot, owner, 1); err != nil || turn == nil || turn.ID != other.ID {
		t.Fatalf("a deferred turn used another account's capacity: %v", err)
	}
	if turn, err := store.ClaimBridgeTurn(bot, owner, 1); err != nil || turn != nil {
		t.Fatalf("a second turn bypassed the reserved slot: %v", err)
	}
	if err := store.DB.Model(&domain.BotInteraction{}).Where("id = ?", first.ID).
		Update("a2a_retry_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	recovered, err := store.ClaimBridgeTurn(bot, owner, 1)
	if err != nil || recovered == nil || recovered.ID != first.ID {
		t.Fatalf("turn could not resume with its own reserved slot: %v", err)
	}
	if err := store.FinishBridgeTurn(*recovered, owner, "done", "replied", false); err != nil {
		t.Fatal(err)
	}
	if turn, err := store.ClaimBridgeTurn(bot, owner, 1); err != nil || turn == nil || turn.SenderID != alice || turn.ID == first.ID {
		t.Fatalf("next turn could not use the released slot: %v", err)
	}
}

func TestBridgeStopSurvivesRequeueOfPreviouslyClaimedTurn(t *testing.T) {
	store, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const bot, sender, owner = "test-bridge", "123", "worker"
	id := BotMessageID(bot, "turn")
	if ok, err := store.ClaimBotConnection(bot, owner, time.Now().UTC()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	item := domain.BotInteraction{ID: id, BotID: bot, SenderID: sender, Content: "work"}
	if ok, err := store.EnqueueBotInteraction(item, "single", sender); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if claimed, err := store.ClaimBotInteraction(bot, bot, owner); err != nil || claimed == nil {
		t.Fatal(claimed, err)
	}
	if err := store.RequeueBridgeTurns(bot, owner); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestBridgeStop(bot, sender, sender, id); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimBotInteraction(bot, bot, owner)
	if err != nil || claimed == nil || !claimed.StopRequested {
		t.Fatal(claimed, err)
	}
	if err := store.FinishBridgeTurn(*claimed, owner, "transport failure", "reply_failed", true); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("transport failure must preserve the pending stop: %v", err)
	}
	if err := store.FinishBridgeTurn(*claimed, owner, "", "canceled", false); err != nil {
		t.Fatal(err)
	}
	turns, err := store.BotHistory(claimed.SessionID)
	if err != nil || len(turns) != 0 {
		t.Fatalf("canceled turn leaked into future context: %+v, %v", turns, err)
	}
}

func TestBotConnectionLeaseAndInterruptedTurn(t *testing.T) {
	store, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if ok, err := store.ClaimBotConnection("external", "owner-a", now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, _ := store.ClaimBotConnection("external", "owner-b", now); ok {
		t.Fatal("two connections claimed")
	}
	if ok, _ := store.ClaimBotConnection("external", "owner-b", now.Add(time.Minute)); !ok {
		t.Fatal("expired lease unavailable")
	}
	if ok, _ := store.RenewBotConnection("external", "owner-a", "connected", "", now.Add(time.Minute)); ok {
		t.Fatal("stale owner renewed lease")
	}
	item := domain.BotInteraction{ID: BotMessageID("b", "m1"), BotID: "b", SenderID: "u", Content: "重试"}
	if ok, err := store.EnqueueBotInteraction(item, "group", "g"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, _ := store.EnqueueBotInteraction(item, "group", "g"); ok {
		t.Fatal("duplicate event")
	}
	claimed, err := store.ClaimBotInteraction("b", "external", "owner-b")
	if err != nil || claimed == nil {
		t.Fatal(err)
	}
	if err = store.RecoverBotInteractions("b"); err != nil {
		t.Fatal(err)
	}
	recovered, _ := store.ClaimBotInteraction("b", "external", "owner-b")
	if recovered == nil || recovered.Status != "ready" || recovered.Reply == "" {
		t.Fatal("interrupted turn was replayed")
	}
	if err = store.SaveBotReply(*claimed, "late reply", ""); err != nil {
		t.Fatal(err)
	}
	row, _ := store.BotInteraction(item.ID)
	if row.Reply == "late reply" {
		t.Fatal("stale model overwrote recovered reply")
	}
}

func TestBotCardDecisionCannotOverwriteConcurrentRunWrite(t *testing.T) {
	store, _ := OpenTest()
	defer store.Close()
	run := domain.Run{ID: "card-cas", Status: "waiting", Nodes: map[string]*domain.NodeRun{"gate": {NodeID: "gate", Status: "waiting_approval", Attempt: 1}}}
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	old, _ := store.RunState(run.ID)
	newer, _ := store.RunState(run.ID)
	newer.Nodes["gate"].Attempt = 2
	if err := store.PutRun(newer); err != nil {
		t.Fatal(err)
	}
	old.Status = "running"
	old.Nodes["gate"].Status = "succeeded"
	if err := store.PutRunAtRevision(old, old.Revision); err != ErrRunDecisionStale {
		t.Fatalf("stale decision: %v", err)
	}
	final, _ := store.RunState(run.ID)
	if final.Nodes["gate"].Attempt != 2 || final.Nodes["gate"].Status != "waiting_approval" {
		t.Fatal("stale card overwrote concurrent run")
	}
}

func TestBotInteractionMySQLLeaseCompetition(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_BOT_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("local Docker MySQL not configured")
	}
	store, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id := fmt.Sprintf("qa-%d", time.Now().UnixNano())
	defer store.DB.Where("bot_id = ?", id).Delete(&domain.BotConnection{})
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := store.ClaimBotConnection(id, fmt.Sprintf("owner-%d", i), time.Now().UTC())
			if err != nil {
				t.Error(err)
			}
			if ok {
				winners.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("connection owners=%d", winners.Load())
	}
	item := domain.BotInteraction{ID: BotMessageID(id, "message"), BotID: id, SenderID: "u", Content: "消息"}
	defer store.DB.Where("bot_id = ?", id).Delete(&domain.BotInteraction{})
	defer store.DB.Where("bot_id = ?", id).Delete(&domain.BotConversation{})
	if inserted, err := store.EnqueueBotInteraction(item, "group", "g"); err != nil || !inserted {
		t.Fatal(inserted, err)
	}
	if inserted, err := store.EnqueueBotInteraction(item, "group", "g"); err != nil || inserted {
		t.Fatal("mysql duplicate", inserted, err)
	}
}

func TestBotDefinitionCannotEnableSameExternalBotTwice(t *testing.T) {
	store, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := domain.NotificationBot{ID: "first", BotID: "external-shared", ProjectID: "p1", OwnerID: 42, Name: "first", Enabled: true, InteractionEnabled: true}
	second := domain.NotificationBot{ID: "second", BotID: first.BotID, ProjectID: "p2", OwnerID: 43, Name: "second", Enabled: true, InteractionEnabled: true}
	if err = store.SaveNotificationBot(first, "secret-first"); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveNotificationBot(second, "secret-second"); err != ErrInteractiveBotAlreadyExists {
		t.Fatalf("duplicate registration: %v", err)
	}
	preserved, _ := store.NotificationBot(first.ID)
	if preserved.ProjectID != "p1" || preserved.Name != "first" {
		t.Fatal("other registration overwrote first Bot")
	}
	first.Enabled = false
	if err = store.SaveNotificationBot(first, ""); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveNotificationBot(second, "secret-second"); err != nil {
		t.Fatal(err)
	}
}
