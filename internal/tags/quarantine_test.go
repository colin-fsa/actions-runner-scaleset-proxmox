package tags

import "testing"

func TestQuarantineMarkerFailsClosed(t *testing.T) {
	if got := QuarantineTag(1800000000); got != "gh-quarantine-until-1800000000" {
		t.Fatalf("unexpected tag: %s", got)
	}
	for _, s := range []string{"gh-scaleset;gh-quarantine-until-1800000000", "gh-quarantine-until-broken"} {
		if !HasQuarantine(s) {
			t.Fatalf("must preserve %q", s)
		}
	}
	if HasQuarantine("gh-scaleset;other") {
		t.Fatal("unrelated tags matched")
	}
}
