package link_test

import (
	"testing"

	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/link"
	"github.com/proshik/krill/internal/selfupdate"
)

// The link reads selfupdate's kinds and the deployer's network prefix by
// value; they must not drift apart.
func TestSharedNames(t *testing.T) {
	if link.RollbackAbsent != selfupdate.RollbackAbsent || link.RollbackRelease != selfupdate.RollbackRelease {
		t.Fatal("rollback binary kinds differ between link and selfupdate")
	}
	if link.EgressNetworkPrefix != deploy.EgressNetworkPrefix {
		t.Fatal("egress network prefix differs between link and deploy")
	}
}
