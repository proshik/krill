package deploy

import "testing"

func TestSettlingReportsOnlyTheWatchedDeploy(t *testing.T) {
	d := &Deployer{}
	if d.Settling(5) {
		t.Fatal("nothing is settling yet")
	}
	d.settling = map[int64]*settleWatch{42: {deployID: 5}}
	if !d.Settling(5) {
		t.Fatal("deploy 5 is being watched")
	}
	if d.Settling(6) {
		t.Fatal("deploy 6 is not")
	}
}
