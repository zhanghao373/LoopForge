package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentflow/internal/auth"
	"agentflow/internal/botinteraction"
	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/notification"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"agentflow/internal/runtime/a2a"
)

func TestWebChatPermissionsAndConversation(t *testing.T) {
	model := newChatTestModel(t)
	defer model.Close()
	testWebChat(t, model.URL, "test")
}

func testWebChat(t *testing.T, endpoint, runtimeToken string) {
	t.Helper()
	t.Setenv("AGENTFLOW_SESSION_SECRET", "web-chat-fixture-secret")
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_SCHEDULER_ENABLED", "false")
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	users := map[string]*http.Cookie{}
	var allowed []string
	for _, name := range []string{"alice", "bob", "outsider"} {
		if err := store.PutUser(domain.User{LoginName: name}); err != nil {
			t.Fatal(err)
		}
		user, _ := store.UserByLoginName(name)
		value, err := auth.NewClientFromEnv().SignUser(&auth.User{ID: user.ID, LoginName: name, AuthTime: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		users[name] = &http.Cookie{Name: authCookieName, Value: value}
		if name != "outsider" {
			allowed = append(allowed, strconv.FormatInt(user.ID, 10))
		}
	}
	client, err := a2a.NewClient(endpoint, runtimeToken)
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := botinteraction.NewWebChat(store, client, allowed, botinteraction.WebChatLimits{
		MaxPending: 2, MaxRunning: 2,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := engine.New(store, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces")))
	n := notification.New(store)
	router, background := NewWithNotificationsAndBackground(ctx, store, e, nil, "", "", n, BackgroundOptions{WebChat: bridge})
	defer background.Stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		bridge.Run(ctx)
	}()
	defer func() { cancel(); <-done }()

	send := func(login, method, path, origin, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://chat.test"+path, strings.NewReader(body))
		if cookie := users[login]; cookie != nil {
			r.AddCookie(cookie)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s %s: got %d, want %d", login, method, path, w.Code, want)
		}
		return w
	}
	const api = "/api/agent-chat/messages"
	const sameOrigin = "http://chat.test"
	for _, trial := range []struct {
		login   string
		allowed bool
	}{{"alice", true}, {"outsider", false}} {
		var shell struct {
			Capabilities map[string]bool `json:"capabilities"`
		}
		result := send(trial.login, http.MethodGet, "/api/bootstrap", "", "", 200)
		if err := json.Unmarshal(result.Body.Bytes(), &shell); err != nil {
			t.Fatal(err)
		}
		if got := shell.Capabilities["agentChat"]; got != trial.allowed {
			t.Fatalf("%s agentChat capability = %t, want %t", trial.login, got, trial.allowed)
		}
	}
	send("", http.MethodGet, "/agent-chat", "", "", 302)
	send("", http.MethodGet, api, "", "", 401)
	send("outsider", http.MethodGet, api, "", "", 403)
	page := send("alice", http.MethodGet, "/agent-chat", "", "", 200)
	if !strings.Contains(page.Body.String(), "/agent-chat-assets/agent-chat.js") {
		t.Fatal("web route served no conversation page")
	}
	send("alice", http.MethodGet, "/agent-chat-assets/agent-chat.js", "", "", 200)
	const first = `{"messageId":"web-first","text":"remember moon"}`
	send("alice", http.MethodPost, api, "https://somewhere.test", first, 403)
	send("alice", http.MethodPost, api, sameOrigin, `{"messageId":"web-first","senderId":"bob","text":"remember moon"}`, 400)
	// Different runs of this test can share the optional external Runtime.
	firstID := strconv.FormatInt(time.Now().UnixNano(), 10)
	firstBody := strings.Replace(first, "web-first", firstID, 1)
	send("alice", http.MethodPost, api, sameOrigin, firstBody, 202)
	send("alice", http.MethodPost, api, sameOrigin, firstBody, 202)
	await := func(login string, expected []string) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			w := send(login, http.MethodGet, api, "", "", 200)
			var turns []webChatMessage
			if err := json.Unmarshal(w.Body.Bytes(), &turns); err != nil {
				t.Fatal(err)
			}
			if len(turns) == len(expected) && (len(turns) == 0 || turns[len(turns)-1].State == "replied") {
				for i, want := range expected {
					if turns[i].Reply != want {
						t.Fatalf("%s turn %d: got %q, want %q", login, i, turns[i].Reply, want)
					}
				}
				return
			}
			time.Sleep(35 * time.Millisecond)
		}
		t.Fatalf("%s did not receive all replies", login)
	}
	await("alice", []string{"remembered moon"})
	await("bob", nil)
	second := `{"messageId":"` + firstID + `-2","text":"what did I say?"}`
	send("alice", http.MethodPost, api, sameOrigin, second, 202)
	send("bob", http.MethodPost, api, sameOrigin, second, 202)
	await("alice", []string{"remembered moon", "you said moon"})
	await("bob", []string{"new conversation"})

	cancel()
	<-done
	queued := send("alice", http.MethodPost, api, sameOrigin, `{"messageId":"stop-queued","text":"Skip this turn"}`, 202)
	var receipt struct{ ID string }
	if err := json.Unmarshal(queued.Body.Bytes(), &receipt); err != nil || receipt.ID == "" {
		t.Fatalf("missing queued message id: %v", err)
	}
	stop := api + "/" + receipt.ID + "/cancel"
	send("", http.MethodPost, stop, sameOrigin, "", 401)
	send("bob", http.MethodPost, stop, sameOrigin, "", 404)
	send("alice", http.MethodPost, stop, "http://other.test", "", 403)
	send("alice", http.MethodPost, stop, sameOrigin, "", 202)
	send("alice", http.MethodPost, stop, sameOrigin, "", 202)
	var turns []webChatMessage
	if err := json.Unmarshal(send("alice", http.MethodGet, api, "", "", 200).Body.Bytes(), &turns); err != nil {
		t.Fatal(err)
	}
	if last := turns[len(turns)-1]; last.ID != receipt.ID || last.State != "canceled" || last.Reply != "" {
		t.Fatalf("queued stop did not persist: %#v", last)
	}

	const otherThread = api + "?thread=3f586dfe-a930-4211-b056-2cc35906adf0"
	const quotaOne = `{"messageId":"quota-one","text":"One"}`
	const quotaTwo = `{"messageId":"quota-two","text":"Two"}`
	const quotaThree = `{"messageId":"quota-three","text":"Three"}`
	one := send("alice", http.MethodPost, api, sameOrigin, quotaOne, 202)
	two := send("alice", http.MethodPost, otherThread, sameOrigin, quotaTwo, 202)
	send("alice", http.MethodPost, api, sameOrigin, quotaThree, 429)
	if retry := send("alice", http.MethodPost, otherThread, sameOrigin, quotaTwo, 202); retry.Body.String() != two.Body.String() {
		t.Fatal("retry changed the admitted message")
	}
	send("alice", http.MethodPost, api, sameOrigin, quotaTwo, 409)
	send("bob", http.MethodPost, api, sameOrigin, quotaThree, 202)
	if err := json.Unmarshal(one.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	send("alice", http.MethodPost, api+"/"+receipt.ID+"/cancel", sameOrigin, "", 202)
	send("alice", http.MethodPost, api, sameOrigin, quotaThree, 202)
}
