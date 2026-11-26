package botinteraction

import (
	"testing"

	"agentflow/internal/runtime/a2a"
)

func TestWebRouterRequiresUniqueReferenceInLatestMessage(t *testing.T) {
	candidates := []a2a.RoutingCandidate{
		{ID: "orders", Title: "订单金额核算任务"},
		{ID: "quote", Title: "报价金额核算任务"},
	}
	if !identifiesWebTicket(candidates, "继续订单金额核算", "orders", "订单金额核算") {
		t.Fatal("a unique exact reference should identify the ticket")
	}
	for _, check := range []struct{ message, target, reference string }{
		{"继续刚才那个任务", "orders", "订单金额核算"},
		{"继续金额核算", "orders", "金额核算"},
		{"继续订单金额核算", "quote", "订单金额核算"},
		{"继续订单金额核算", "foreign", "订单金额核算"},
	} {
		if identifiesWebTicket(candidates, check.message, check.target, check.reference) {
			t.Fatalf("ambiguous or foreign ticket was accepted: %+v", check)
		}
	}
}
