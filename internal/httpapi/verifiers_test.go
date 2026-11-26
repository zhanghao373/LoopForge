package httpapi

import (
	"testing"

	"agentflow/internal/domain"
)

func TestVerifierVerdictRequiresExplicitBoolean(t *testing.T) {
	user := domain.User{CodexAPIKey: "test-secret-do-not-record"}
	for _, malformed := range []string{
		"passed",
		`{"reason":"only a reason"}`,
		`{"passed":"true","reason":"invalid Boolean"}`,
		`{"passed":true,"reason":""}`,
	} {
		if _, err := decodeVerifierVerdict(malformed, user); err == nil {
			t.Fatalf("accepted %s", malformed)
		}
	}
	result, err := decodeVerifierVerdict(`{"passed":false,"reason":"insufficient evidence","findings":["test-secret-do-not-record"]}`, user)
	if err != nil || result.Passed || len(result.Findings) != 1 || result.Findings[0] == user.CodexAPIKey {
		t.Fatalf("could not decode and redact verdict: %+v, %v", result, err)
	}
}
