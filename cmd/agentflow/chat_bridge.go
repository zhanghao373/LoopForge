package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/botinteraction"
	"agentflow/internal/httpapi"
	"agentflow/internal/repo"
	"agentflow/internal/runtime/a2a"
)

func startChatBridge(ctx context.Context, store *repo.Store) (func(), error) {
	rawPort := strings.TrimSpace(os.Getenv("AGENTFLOW_CHAT_BRIDGE_PORT"))
	if rawPort == "" {
		return func() {}, nil
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid AGENTFLOW_CHAT_BRIDGE_PORT")
	}
	inboundToken := os.Getenv("AGENTFLOW_CHAT_BRIDGE_TOKEN")
	runtimeToken := os.Getenv("AGENTFLOW_A2A_TOKEN")
	if len(inboundToken) < 32 || inboundToken != strings.TrimSpace(inboundToken) || inboundToken == runtimeToken {
		return nil, errors.New("chat bridge requires a separate bearer token of at least 32 bytes")
	}
	client, err := a2a.NewClient(strings.TrimSpace(os.Getenv("AGENTFLOW_A2A_RPC_URL")), runtimeToken)
	if err != nil {
		return nil, err
	}
	bridge, err := botinteraction.NewChatBridge(store, client,
		strings.TrimSpace(os.Getenv("AGENTFLOW_CHAT_BRIDGE_ID")),
		chatBridgeIDs(os.Getenv("AGENTFLOW_CHAT_BRIDGE_SENDERS")),
		chatBridgeIDs(os.Getenv("AGENTFLOW_CHAT_BRIDGE_GROUPS")))
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	server := &http.Server{
		Handler:           httpapi.ChatBridgeHandler(bridge, inboundToken),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	workerCtx, cancel := context.WithCancel(ctx)
	stopWorker := runChatWorker(workerCtx, bridge)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("chat bridge listener failed")
			cancel()
		}
	}()
	return func() {
		cancel()
		_ = server.Close()
		stopWorker()
	}, nil
}

func startWebChat(ctx context.Context, store *repo.Store) (*botinteraction.ChatBridge, func(), error) {
	senders := chatBridgeIDs(os.Getenv("AGENTFLOW_WEB_CHAT_USER_IDS"))
	if len(senders) == 0 {
		return nil, func() {}, nil
	}
	for _, sender := range senders {
		id, err := strconv.ParseInt(sender, 10, 64)
		if err != nil || id < 1 || strconv.FormatInt(id, 10) != sender {
			return nil, nil, errors.New("AGENTFLOW_WEB_CHAT_USER_IDS must contain positive LoopForge account IDs")
		}
	}
	token := os.Getenv("AGENTFLOW_WEB_CHAT_A2A_TOKEN")
	sopToken := os.Getenv("AGENTFLOW_A2A_TOKEN")
	executorURL := strings.TrimSpace(os.Getenv("AGENTFLOW_WEB_CHAT_ROOM_A2A_RPC_URL"))
	roomContext := executorURL != ""
	if roomContext && (token == "" || token == sopToken) {
		return nil, nil, errors.New("Room-backed web chat requires a separate AGENTFLOW_WEB_CHAT_A2A_TOKEN")
	}
	if token == "" {
		token = sopToken
	}
	if !roomContext {
		executorURL = strings.TrimSpace(os.Getenv("AGENTFLOW_A2A_RPC_URL"))
	}
	client, err := a2a.NewClient(executorURL, token)
	if err != nil {
		return nil, nil, err
	}
	var router *a2a.Client
	if routerToken := os.Getenv("AGENTFLOW_WEB_CHAT_ROUTER_A2A_TOKEN"); routerToken != "" {
		if routerToken == token || routerToken == os.Getenv("AGENTFLOW_A2A_TOKEN") {
			return nil, nil, errors.New("web chat router requires an independent A2A token")
		}
		router, err = a2a.NewClient(strings.TrimSpace(os.Getenv("AGENTFLOW_A2A_RPC_URL")), routerToken)
		if err != nil {
			return nil, nil, err
		}
	}
	pending, err := webChatLimit("AGENTFLOW_WEB_CHAT_MAX_PENDING_PER_USER", 8)
	if err != nil {
		return nil, nil, err
	}
	running, err := webChatLimit("AGENTFLOW_WEB_CHAT_MAX_RUNNING_PER_USER", 2)
	if err != nil {
		return nil, nil, err
	}
	bridge, err := botinteraction.NewWebChat(store, client, senders, botinteraction.WebChatLimits{
		MaxPending: pending, MaxRunning: running,
	}, router)
	if err != nil {
		return nil, nil, err
	}
	if roomContext {
		bridge.UseRoomContext()
	}
	return bridge, runChatWorker(ctx, bridge), nil
}

func webChatLimit(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return limit, nil
}

func runChatWorker(ctx context.Context, bridge *botinteraction.ChatBridge) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		bridge.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

func chatBridgeIDs(value string) []string {
	var ids []string
	for _, id := range strings.Split(value, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
