package cluster

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestJoinCommand(t *testing.T) {
	got := joinCommand("SWMTKN-1-abc", "10.0.0.1:2377")
	want := "docker swarm join --token SWMTKN-1-abc 10.0.0.1:2377"
	if got != want {
		t.Fatalf("joinCommand = %q, want %q", got, want)
	}
}

// Leave undoes a join whose bookkeeping failed. --force is required because the
// node is already a swarm member and would otherwise refuse to leave.
func TestLeaveCommand(t *testing.T) {
	got := leaveCommand()
	want := "docker swarm leave --force"
	if got != want {
		t.Fatalf("leaveCommand = %q, want %q", got, want)
	}
}

func TestHostKeyMatches(t *testing.T) {
	if !hostKeyMatches("", "AAAA") {
		t.Error("empty stored key must be accepted (accept-new)")
	}
	if !hostKeyMatches("AAAA", "AAAA") {
		t.Error("matching key must be accepted")
	}
	if hostKeyMatches("AAAA", "BBBB") {
		t.Error("mismatched key must be rejected")
	}
}

func TestDialVerifiedRequiresKey(t *testing.T) {
	// A malformed private key must fail fast before any network use.
	_, err := DialVerified(JoinSpec{Host: "127.0.0.1", User: "x", PrivateKey: []byte("not-a-key"), HostKey: "k"}, time.Second)
	if err == nil {
		t.Fatal("expected parse error for bad key")
	}
}

func TestDialVerifiedRejectsEmptyHostKey(t *testing.T) {
	// DialVerified is the strict variant: an empty HostKey must be rejected
	// immediately (before any network dial) to prevent silently accepting any key.
	_, err := DialVerified(JoinSpec{Host: "h", User: "u", PrivateKey: []byte("k"), HostKey: ""}, time.Second)
	if err == nil {
		t.Fatal("expected error for empty host key")
	}
	if !containsStr(err.Error(), "host key") {
		t.Fatalf("error should mention host key, got: %v", err)
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || strings.Contains(s, sub))
}

func TestSSHHandshakeCancellationClosesConnection(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pemKey, err := ssh.MarshalPrivateKey(key, "test")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, e := listener.Accept()
		if e == nil {
			accepted <- c
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		cl, e := DialVerifiedContext(ctx, JoinSpec{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, User: "test", PrivateKey: pem.EncodeToMemory(pemKey), HostKey: "expected-key"}, time.Second)
		if cl != nil {
			cl.Close()
		}
		done <- e
	}()
	conn := <-accepted
	defer conn.Close()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled handshake succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("SSH handshake ignored cancellation")
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err = io.Copy(io.Discard, conn)
	if err != nil {
		t.Fatalf("client connection remained open: %v", err)
	}
}

// A completed handshake must not leave its short deadline on the socket. Later
// channel opens remain governed by the operation context, even if the peer stalls.
func TestVerifiedSSHChannelCancellationAndHandshakeDeadline(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	channelSeen := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer raw.Close()
		cfg := &ssh.ServerConfig{NoClientAuth: true}
		cfg.AddHostKey(signer)
		conn, channels, requests, err := ssh.NewServerConn(raw, cfg)
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		go ssh.DiscardRequests(requests)
		for range channels {
			close(channelSeen)
			break
		} // deliberately never acknowledge channel open
		serverDone <- conn.Wait()
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cl, err := DialVerifiedContext(ctx, JoinSpec{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, User: "test", PrivateKey: pem.EncodeToMemory(private), HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))}, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	time.Sleep(150 * time.Millisecond)
	done := make(chan error, 1)
	go func() {
		c, e := cl.DialContext(ctx, "unix", "/var/run/docker.sock")
		if c != nil {
			c.Close()
		}
		done <- e
	}()
	select {
	case <-channelSeen:
	case err := <-done:
		t.Fatalf("handshake deadline poisoned session: %v", err)
	case <-time.After(time.Second):
		t.Fatal("channel not requested")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled channel succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("channel open ignored cancellation")
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("SSH connection leaked")
	}
}
