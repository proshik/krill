package netguard

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "::1", "10.0.0.1", "192.168.1.1", "172.16.0.1", "169.254.169.254", "fe80::1", "fc00::1", "0.0.0.0", "100.64.0.1"}
	for _, s := range blocked {
		if !IsBlockedIP(net.ParseIP(s), false) {
			t.Errorf("%s should be blocked when allowPrivate=false", s)
		}
		if IsBlockedIP(net.ParseIP(s), true) {
			t.Errorf("%s should be allowed when allowPrivate=true", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34"} {
		if IsBlockedIP(net.ParseIP(s), false) {
			t.Errorf("public %s must not be blocked", s)
		}
	}
}

// TestDialContextRejectsLiteralPrivateIP exercises the resolve/dial path itself
// (not just IsBlockedIP): literal private/link-local/CGNAT IPs must be rejected
// with ErrBlocked before any dial is attempted, and allowPrivate=true must not
// short-circuit into ErrBlocked (it may still fail to connect, but not via the guard).
func TestDialContextRejectsLiteralPrivateIP(t *testing.T) {
	ctx := context.Background()

	blockedAddrs := []string{"127.0.0.1:80", "169.254.169.254:80", "100.64.0.1:80"}
	dial := DialContext(false)
	for _, addr := range blockedAddrs {
		_, err := dial(ctx, "tcp", addr)
		if err == nil {
			t.Fatalf("DialContext(false) to %s: expected error, got nil", addr)
		}
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("DialContext(false) to %s: expected ErrBlocked, got %v", addr, err)
		}
	}

	// With allowPrivate=true, the literal-IP branch must not reject via the
	// guard. The dial itself may still fail (no listener on these addresses
	// in the test sandbox), so only assert the error is not ErrBlocked.
	dialAllow := DialContext(true)
	for _, addr := range blockedAddrs {
		dctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		_, err := dialAllow(dctx, "tcp", addr)
		cancel()
		if err != nil && errors.Is(err, ErrBlocked) {
			t.Errorf("DialContext(true) to %s: got ErrBlocked, guard should be bypassed", addr)
		}
	}
}

// TestCheckHostLoopback exercises the hostname-resolve path: "localhost"
// resolves to a loopback address, which must be blocked unless allowPrivate.
func TestCheckHostLoopback(t *testing.T) {
	ctx := context.Background()

	err := CheckHost(ctx, "localhost", false)
	if err == nil {
		t.Fatal("CheckHost(localhost, false): expected error, got nil")
	}
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("CheckHost(localhost, false): expected ErrBlocked, got %v", err)
	}

	if err := CheckHost(ctx, "localhost", true); err != nil {
		t.Errorf("CheckHost(localhost, true): expected nil, got %v", err)
	}
}
