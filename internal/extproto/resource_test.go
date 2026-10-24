package extproto

// The fixture provisioner's live view and power switch.
//
// These two calls are optional capabilities, so nothing in the core's queue depends on them; what
// depends on them is the customer's server card. The fixture implements them so the card's branches are
// reachable without a real panel, and this test keeps the fixture honest: a fixture that silently
// stopped answering would make every card test pass against nothing.

import (
	"context"
	"strings"
	"testing"
)

// createFixtureResource creates one resource and returns its id.
func createFixtureResource(t *testing.T, c *Client) string {
	t.Helper()
	var out ProvisionCreateResult
	if err := c.Call(context.Background(), MethodProvisionerCreate, &ProvisionCreateParams{
		MutatingMeta:   MutatingMeta{OperationKey: "create:fixture:1"},
		SubscriptionID: "11111111-1111-4111-8111-111111111111",
		PlanID:         "small",
		Region:         "us-east",
		Quantity:       1,
	}, &out); err != nil {
		t.Fatalf("create: %v", err)
	}
	if out.ExternalID == "" {
		t.Fatal("the fixture returned no resource id")
	}
	return out.ExternalID
}

func TestFixtureResourceReportsTheLiveState(t *testing.T) {
	c := launchProv(t)
	id := createFixtureResource(t, c)

	var res ProvisionResourceResult
	if err := c.Call(context.Background(), MethodProvisionerResource,
		&ProvisionResourceParams{ExternalID: id}, &res); err != nil {
		t.Fatalf("resource: %v", err)
	}
	if res.ExternalID != id {
		t.Fatalf("external id = %q, want %q", res.ExternalID, id)
	}
	if res.Name == "" {
		t.Fatal("the fixture should report a name: the card's heading depends on it")
	}
	if res.Address() == "" {
		t.Fatal("the fixture should report an address: the card's address row depends on it")
	}
	if res.PanelURL == "" {
		t.Fatal("the fixture should report a deep link")
	}
	if res.Stats == nil {
		t.Fatal("the fixture should report metrics by default")
	}
	// A running server offers stop/restart/kill and never start.
	actions := make([]string, 0, len(res.CanPower))
	for _, a := range res.CanPower {
		actions = append(actions, string(a))
	}
	if got := strings.Join(actions, ","); got != "stop,restart,kill" {
		t.Fatalf("can_power = %q, want stop,restart,kill", got)
	}
}

func TestFixtureResourceCanReportNoMetrics(t *testing.T) {
	c := launchProv(t, "FAKE_PROV_NO_METRICS=1")
	id := createFixtureResource(t, c)

	var res ProvisionResourceResult
	if err := c.Call(context.Background(), MethodProvisionerResource,
		&ProvisionResourceParams{ExternalID: id}, &res); err != nil {
		t.Fatalf("resource: %v", err)
	}
	// The card's "no figures yet" branch must be reachable, and a stopped or unmeasured server must not
	// look like a server using nothing.
	if res.Stats != nil {
		t.Fatalf("stats = %+v, want none", res.Stats)
	}
}

func TestFixturePowerChangesTheReportedState(t *testing.T) {
	c := launchProv(t, "FAKE_PROV_POWER_STATE=running")
	id := createFixtureResource(t, c)

	var powered ProvisionPowerResult
	if err := c.Call(context.Background(), MethodProvisionerPower, &ProvisionPowerParams{
		MutatingMeta: MutatingMeta{OperationKey: "power:1"},
		ExternalID:   id,
		Action:       PowerStop,
	}, &powered); err != nil {
		t.Fatalf("power: %v", err)
	}
	if !powered.Changed {
		t.Fatal("stopping a running server is a change")
	}

	// The next read must agree, or the card would keep offering stop on a stopped server.
	var res ProvisionResourceResult
	if err := c.Call(context.Background(), MethodProvisionerResource,
		&ProvisionResourceParams{ExternalID: id}, &res); err != nil {
		t.Fatalf("resource: %v", err)
	}
	actions := make([]string, 0, len(res.CanPower))
	for _, a := range res.CanPower {
		actions = append(actions, string(a))
	}
	if got := strings.Join(actions, ","); got != "start" {
		t.Fatalf("can_power after a stop = %q, want start", got)
	}
}

func TestFixtureResourceRefusesAnUnknownResource(t *testing.T) {
	c := launchProv(t)

	// An unknown id is authoritative absence, not a retryable failure: the core must be able to tell
	// "the panel does not have it" from "the panel did not answer".
	err := c.Call(context.Background(), MethodProvisionerResource,
		&ProvisionResourceParams{ExternalID: "nope"}, &ProvisionResourceResult{})
	if err == nil {
		t.Fatal("an unknown resource must fail")
	}
	if !strings.Contains(err.Error(), "unknown resource") {
		t.Fatalf("error = %q, want an unknown-resource failure", err.Error())
	}
}

func TestPowerParamsValidateBeforeTheCall(t *testing.T) {
	// The core checks the action before it reaches an adapter, so a typo is a local refusal rather than
	// a round trip to a provider that may or may not understand it.
	if err := (ProvisionPowerParams{ExternalID: "x", Action: "nuke",
		MutatingMeta: MutatingMeta{OperationKey: "k"}}).Validate(); err == nil {
		t.Fatal("an action outside the four must be refused")
	}
	if err := (ProvisionPowerParams{ExternalID: "x", Action: PowerStart}).Validate(); err == nil {
		t.Fatal("a mutating call with no operation key must be refused")
	}
	if err := (ProvisionPowerParams{MutatingMeta: MutatingMeta{OperationKey: "k"},
		Action: PowerStart}).Validate(); err == nil {
		t.Fatal("a power call with no external id must be refused")
	}
	if err := (ProvisionPowerParams{ExternalID: "x", Action: PowerKill,
		MutatingMeta: MutatingMeta{OperationKey: "k"}}).Validate(); err != nil {
		t.Fatalf("a valid power request was refused: %v", err)
	}
}

func TestAddressRendersOneWay(t *testing.T) {
	cases := []struct {
		res  ProvisionResourceResult
		want string
	}{
		{ProvisionResourceResult{Host: "play.example.com", Port: 25565}, "play.example.com:25565"},
		{ProvisionResourceResult{Host: "play.example.com"}, "play.example.com"},
		{ProvisionResourceResult{Port: 25565}, ""},
		{ProvisionResourceResult{}, ""},
	}
	for _, tc := range cases {
		if got := tc.res.Address(); got != tc.want {
			t.Fatalf("Address() = %q, want %q", got, tc.want)
		}
	}
}
