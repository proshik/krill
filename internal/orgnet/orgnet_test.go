package orgnet

import "testing"

func TestName(t *testing.T) {
	if got := Name(7); got != "krill-org-7" {
		t.Fatalf("Name(7) = %q", got)
	}
}
