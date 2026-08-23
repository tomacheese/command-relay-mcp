package main

import "testing"

func TestVersionRequested(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"command-relay-agent"}, false},
		{[]string{"command-relay-agent", "--version"}, true},
		{[]string{"command-relay-agent", "--landlock-exec", "--", "/bin/true"}, false},
	}
	for _, c := range cases {
		if got := versionRequested(c.args); got != c.want {
			t.Errorf("versionRequested(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}
