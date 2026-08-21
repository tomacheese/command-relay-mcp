package gateway

import (
	"reflect"
	"testing"
)

// TestParseAgentSecrets covers base spec §23's Gateway "agent
// authentication configuration" setting: it must support more than one
// device credential pair (base spec §1's multi-device management goal),
// not just the single AGENT_DEVICE_ID/AGENT_DEVICE_SECRET pair V1 shipped
// with.
func TestParseAgentSecrets(t *testing.T) {
	got := parseAgentSecrets("pine:secret-1,willow:secret-2")
	want := map[string]string{"pine": "secret-1", "willow": "secret-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAgentSecrets = %+v, want %+v", got, want)
	}
}

func TestParseAgentSecrets_EmptyIsEmptyMap(t *testing.T) {
	got := parseAgentSecrets("")
	if len(got) != 0 {
		t.Fatalf("parseAgentSecrets(\"\") = %+v, want empty", got)
	}
}

func TestParseAgentSecrets_IgnoresMalformedEntries(t *testing.T) {
	got := parseAgentSecrets("pine:secret-1,malformed,willow:secret-2")
	want := map[string]string{"pine": "secret-1", "willow": "secret-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAgentSecrets = %+v, want %+v", got, want)
	}
}
