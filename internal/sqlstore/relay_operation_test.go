package sqlstore

import (
	"testing"

	"github.com/sean2077/pairroom/internal/relay"
)

func TestRelayNarrowBindingCannotUseHumanQueueControls(t *testing.T) {
	store := &Store{cfg: Config{DeploymentID: "deployment"}}
	for _, kind := range []relay.EnterpriseOperationKind{relay.EnterpriseOperationRetry, relay.EnterpriseOperationCancel} {
		cmd := RelayCommand{Auth: AuthContext{Deployment: "deployment", UserID: "member", BindingGrantID: "narrow-grant"}, RoomID: "room", BindingID: "binding", Generation: 1, ClientID: "request", Intent: relay.EnterpriseOperationIntent{Kind: kind, MessageID: "other-message"}}
		if _, class := store.relayPlan(cmd); class != ClassAuthorization {
			t.Fatalf("Binding acquired human %s authority: %s", kind, class)
		}
	}
}

func TestRelayReportIdentityUsesOriginalSequenceAndCompletePayload(t *testing.T) {
	store := &Store{cfg: Config{DeploymentID: "deployment"}}
	command := RelayCommand{Auth: AuthContext{Deployment: "deployment", UserID: "member", BindingGrantID: "narrow-grant"}, RoomID: "room", BindingID: "binding", Generation: 2, ClientID: "request-one", Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationReport, ReportSeq: 7, Text: "@agent2 original text", ExpectedPeerGeneration: 5}}
	first, class := store.relayPlan(command)
	if class != "" {
		t.Fatal(class)
	}
	command.ClientID = "request-after-restart"
	second, class := store.relayPlan(command)
	if class != "" || first.key != second.key || first.hash != second.hash {
		t.Fatal("transport identity changed original Stop receipt")
	}
	command.Intent.Text = "@agent2 changed text"
	changed, class := store.relayPlan(command)
	if class != "" || first.key != changed.key || first.hash == changed.hash {
		t.Fatal("changed Stop payload can recover original receipt")
	}
	command.Intent.Text = "@agent2 original text"
	command.Intent.ExpectedPeerGeneration++
	changed, class = store.relayPlan(command)
	if class != "" || first.hash == changed.hash {
		t.Fatal("changed peer generation did not change intent")
	}
}
