package link

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/testutil"
)

func relayStore(t *testing.T) (*Store, KeyPair) {
	t.Helper()
	st := NewStore(testutil.NewTestDB(t))
	if err := st.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	ca, err := NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return st, ca
}

func csr(t *testing.T) (string, string) {
	t.Helper()
	k, c, err := NewCSR()
	if err != nil {
		t.Fatal(err)
	}
	return k, c
}

// The relay's answer can be lost after its commit: the client retries with
// the same key and must get the same certificate. Another key is refused, and
// only one peer exists.
func TestRedeemRetryWithSameKey(t *testing.T) {
	st, ca := relayStore(t)
	ctx := context.Background()
	sec, err := st.CreateCode(ctx, "msk")
	if err != nil {
		t.Fatal(err)
	}
	_, req := csr(t)
	first, peer, err := st.Redeem(ctx, sec, req, ca)
	if err != nil || peer != "msk" {
		t.Fatalf("redeem: %v (peer %q)", err, peer)
	}
	again, _, err := st.Redeem(ctx, sec, req, ca)
	if err != nil {
		t.Fatalf("retry with the same key: %v", err)
	}
	if again != first {
		t.Fatal("a retry with the same key must get the same certificate")
	}
	_, other := csr(t)
	if _, _, err := st.Redeem(ctx, sec, other, ca); !errors.Is(err, ErrCodeRefused) {
		t.Fatalf("another key: %v, want ErrCodeRefused", err)
	}
	peers, err := st.Peers(ctx)
	if err != nil || len(peers) != 1 {
		t.Fatalf("peers %v, %v: want exactly one", peers, err)
	}
	fp, _ := CertFingerprint(first)
	if peers[0].CertSha256 != fp || peers[0].Name != "msk" {
		t.Fatalf("peer %+v does not hold the issued certificate", peers[0])
	}
}

// Two redemptions with different keys race: exactly one wins.
func TestRedeemRace(t *testing.T) {
	st, ca := relayStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		name := "peer" + string(rune('a'+i))
		sec, err := st.CreateCode(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		_, a := csr(t)
		_, b := csr(t)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j, req := range []string{a, b} {
			wg.Add(1)
			go func() { defer wg.Done(); _, _, errs[j] = st.Redeem(ctx, sec, req, ca) }()
		}
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("%s: want one winner, got %v / %v", name, errs[0], errs[1])
		}
		for _, e := range errs {
			if e != nil && !errors.Is(e, ErrCodeRefused) {
				t.Fatalf("loser got %v, want ErrCodeRefused", e)
			}
		}
	}
}

func TestRedeemRefusals(t *testing.T) {
	st, ca := relayStore(t)
	ctx := context.Background()
	_, req := csr(t)
	if _, _, err := st.Redeem(ctx, "no-such-secret-at-all-xxxxxxxxxxxxx", req, ca); !errors.Is(err, ErrCodeRefused) {
		t.Fatalf("unknown code: %v", err)
	}
	sec, err := st.CreateCode(ctx, "msk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, "UPDATE link_pairing_codes SET expires_at = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Redeem(ctx, sec, req, ca); !errors.Is(err, ErrCodeRefused) {
		t.Fatalf("expired code: %v", err)
	}
	if _, _, err := st.Redeem(ctx, sec, "not a csr", ca); !errors.Is(err, ErrCodeRefused) {
		t.Fatalf("bad csr: %v", err)
	}
	// A code is for a new peer only.
	sec2, err := st.CreateCode(ctx, "spb")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Redeem(ctx, sec2, req, ca); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateCode(ctx, "spb"); !errors.Is(err, ErrPeerExists) {
		t.Fatalf("code for an existing peer: %v", err)
	}
	// Only the secret's hash is stored.
	var n int
	if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM link_pairing_codes WHERE secret_sha256 = $1 OR secret_sha256 = $2", sec, sec2).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a plain secret is stored (%d rows, %v)", n, err)
	}
}

func TestCodeLivesTenMinutes(t *testing.T) {
	st, _ := relayStore(t)
	ctx := context.Background()
	if _, err := st.CreateCode(ctx, "msk"); err != nil {
		t.Fatal(err)
	}
	var d time.Duration
	var secs float64
	if err := st.pool.QueryRow(ctx, "SELECT extract(epoch FROM expires_at - created_at) FROM link_pairing_codes").Scan(&secs); err != nil {
		t.Fatal(err)
	}
	d = time.Duration(secs * float64(time.Second))
	if d < CodeLifetime-time.Second || d > CodeLifetime+time.Second {
		t.Fatalf("code lives %v, want %v", d, CodeLifetime)
	}
}

// Keys are stored encrypted when a secret key is set; a renewal keeps the
// session's certificate valid until the first connect with the new one.
func TestRenewKeepsPreviousUntilConnect(t *testing.T) {
	st, ca := relayStore(t)
	ctx := context.Background()
	sec, _ := st.CreateCode(ctx, "msk")
	_, req := csr(t)
	cert, _, err := st.Redeem(ctx, sec, req, ca)
	if err != nil {
		t.Fatal(err)
	}
	oldFP, _ := CertFingerprint(cert)
	peer, err := st.PeerByFingerprint(ctx, oldFP)
	if err != nil {
		t.Fatal(err)
	}
	_, req2 := csr(t)
	next, err := st.Renew(ctx, peer, oldFP, req2, ca)
	if err != nil {
		t.Fatal(err)
	}
	newFP, _ := CertFingerprint(next)
	for _, fp := range []string{oldFP, newFP} {
		if _, err := st.PeerByFingerprint(ctx, fp); err != nil {
			t.Fatalf("fingerprint %s… not valid after renewal: %v", fp[:8], err)
		}
	}
	// The first answer was lost: the session (still on the old certificate)
	// renews again; the old one stays valid, the lost one does not.
	_, req3 := csr(t)
	third, err := st.Renew(ctx, peer, oldFP, req3, ca)
	if err != nil {
		t.Fatalf("second renewal from the old certificate: %v", err)
	}
	thirdFP, _ := CertFingerprint(third)
	if err := st.markConnected(ctx, peer.ID, thirdFP); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PeerByFingerprint(ctx, oldFP); !errors.Is(err, ErrPeerGone) {
		t.Fatalf("old certificate after a connect with the new one: %v, want refused", err)
	}
	if _, err := st.PeerByFingerprint(ctx, thirdFP); err != nil {
		t.Fatal(err)
	}
}
