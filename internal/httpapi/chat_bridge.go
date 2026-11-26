package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"agentflow/internal/botinteraction"
	"gorm.io/gorm"
)

const ChatBridgePath = "/internal/chat-bridge/"

type chatBridgeHandlers struct {
	bridge *botinteraction.ChatBridge
	bearer [sha256.Size]byte
}

// ChatBridgeHandler accepts messages from an authenticated deployment-side
// transport. The transport is responsible for verifying its provider callbacks.
func ChatBridgeHandler(bridge *botinteraction.ChatBridge, bearer string) http.Handler {
	h := &chatBridgeHandlers{bridge: bridge, bearer: sha256.Sum256([]byte(bearer))}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+ChatBridgePath+"messages", h.accept)
	mux.HandleFunc("GET "+ChatBridgePath+"messages/{id}", h.lookup)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		candidate := sha256.Sum256([]byte(value))
		if bridge == nil || bearer == "" || !ok || subtle.ConstantTimeCompare(candidate[:], h.bearer[:]) != 1 {
			chatBridgeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (h *chatBridgeHandlers) accept(w http.ResponseWriter, r *http.Request) {
	var event botinteraction.ChatEvent
	if err := decodeChatJSON(w, r, &event); err != nil {
		chatBridgeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid chat event"})
		return
	}
	id, err := h.bridge.Accept(event)
	switch {
	case err == nil:
		chatBridgeJSON(w, http.StatusAccepted, map[string]string{"id": id})
	case errors.Is(err, botinteraction.ErrInvalidChatEvent):
		chatBridgeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid chat event"})
	case errors.Is(err, botinteraction.ErrChatNotAllowed):
		chatBridgeJSON(w, http.StatusForbidden, map[string]string{"error": "chat is not allowed"})
	case errors.Is(err, botinteraction.ErrChatConflict):
		chatBridgeJSON(w, http.StatusConflict, map[string]string{"error": "conflicting message ID"})
	default:
		chatBridgeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "message storage unavailable"})
	}
}

func decodeChatJSON(w http.ResponseWriter, r *http.Request, value any) error {
	input := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	input.DisallowUnknownFields()
	if err := input.Decode(value); err != nil {
		return err
	}
	if input.Decode(new(any)) != io.EOF {
		return errors.New("expected a single JSON value")
	}
	return nil
}

func (h *chatBridgeHandlers) lookup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) != 64 {
		chatBridgeJSON(w, http.StatusNotFound, map[string]string{"error": "message not found"})
		return
	}
	if _, err := hex.DecodeString(id); err != nil {
		chatBridgeJSON(w, http.StatusNotFound, map[string]string{"error": "message not found"})
		return
	}
	state, reply, err := h.bridge.Lookup(id)
	switch {
	case err == nil:
		chatBridgeJSON(w, http.StatusOK, map[string]string{"state": state, "reply": reply})
	case errors.Is(err, gorm.ErrRecordNotFound):
		chatBridgeJSON(w, http.StatusNotFound, map[string]string{"error": "message not found"})
	default:
		chatBridgeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "message storage unavailable"})
	}
}

func chatBridgeJSON(w http.ResponseWriter, status int, value map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
