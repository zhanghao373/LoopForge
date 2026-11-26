package httpapi

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"agentflow/internal/tracker"
)

func TestLinearMCPGrantEncryptedAcrossRestart(t *testing.T) {
	t.Setenv("AGENTFLOW_LINEAR_MCP_DIR", t.TempDir())
	t.Setenv("AGENTFLOW_LINEAR_MCP_PUBLIC_URL", "http://127.0.0.1:18090")
	a := newLinearMCPAuth()
	if !a.enabled() || a.redirectURI != "http://127.0.0.1:18090"+linearMCPCallbackPath {
		t.Fatalf("unexpected callback: %s", a.redirectURI)
	}
	const secret = "app-session-secret"
	grant := tracker.LinearMCPGrant{ClientID: "client", AccessToken: "private-access-token", RefreshToken: "private-refresh-token", ExpiresAt: time.Now().Add(time.Hour)}
	if err := a.save(1, secret, grant); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(a.grantFile(1))
	if err != nil || bytes.Contains(stored, []byte(grant.AccessToken)) || bytes.Contains(stored, []byte(grant.RefreshToken)) {
		t.Fatalf("grant not encrypted: %v", err)
	}
	reopened := newLinearMCPAuth()
	if access, err := reopened.access(context.Background(), 1, secret); err != nil || access != grant.AccessToken {
		t.Fatalf("grant not restored: %v", err)
	}
	if _, err := reopened.access(context.Background(), 1, "wrong secret"); !errors.Is(err, tracker.ErrLinearMCPAuthorization) {
		t.Fatalf("wrong key accepted: %v", err)
	}
	if err := os.WriteFile(reopened.grantFile(2), stored, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.access(context.Background(), 2, secret); !errors.Is(err, tracker.ErrLinearMCPAuthorization) {
		t.Fatalf("grant reused by another account: %v", err)
	}
}

func TestLinearMCPCallbackVerifiesAndConsumesState(t *testing.T) {
	a := newLinearMCPAuth()
	a.flow[1] = linearMCPFlow{state: "nonce", projectID: "project", expires: time.Now().Add(time.Minute)}
	ctx := context.Background()
	if project, result := a.complete(ctx, 2, "secret", "nonce", "", "access_denied"); project != "" || result != "expired" {
		t.Fatal("another account could consume the flow")
	}
	if project, result := a.complete(ctx, 1, "secret", "wrong", "", "access_denied"); project != "" || result != "expired" {
		t.Fatal("invalid state accepted")
	}
	if project, result := a.complete(ctx, 1, "secret", "nonce", "", "access_denied"); project != "project" || result != "denied" {
		t.Fatal("expected denied flow to return to the original project")
	}
	if _, result := a.complete(ctx, 1, "secret", "nonce", "", "access_denied"); result != "expired" {
		t.Fatal("state was reusable")
	}
}
