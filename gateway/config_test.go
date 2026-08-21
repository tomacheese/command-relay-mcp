package gateway

import (
	"reflect"
	"testing"
)

// TestParseAgentSecrets covers that the Gateway's agent authentication
// configuration must support more than one device credential pair, not
// just a single fixed AGENT_DEVICE_ID/AGENT_DEVICE_SECRET pair.
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
