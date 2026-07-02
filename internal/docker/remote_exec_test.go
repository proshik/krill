package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/client"
)

func TestSelectClient(t *testing.T) {
	ctx := context.Background()
	local := &client.Client{} // sentinel identity, not used for calls
	remote := &client.Client{}

	// nil provider → local, noop release
	cli, release, err := selectClient(ctx, local, nil, "nodeA")
	if err != nil || cli != local {
		t.Fatalf("nil provider: want local, got cli=%p err=%v", cli, err)
	}
	release() // must not panic

	// provider returns (nil,nil,nil) → local node
	cli, release, err = selectClient(ctx, local, func(context.Context, string) (*client.Client, func() error, error) {
		return nil, nil, nil
	}, "control-plane")
	if err != nil || cli != local {
		t.Fatalf("local-node provider: want local, got cli=%p err=%v", cli, err)
	}
	release()

	// provider returns remote client + closeFn → remote, release invokes closeFn
	closed := false
	cli, release, err = selectClient(ctx, local, func(context.Context, string) (*client.Client, func() error, error) {
		return remote, func() error { closed = true; return nil }, nil
	}, "worker-1")
	if err != nil || cli != remote {
		t.Fatalf("remote provider: want remote, got cli=%p err=%v", cli, err)
	}
	release()
	if !closed {
		t.Fatal("release did not invoke closeFn")
	}

	// provider error → propagated, release noop
	sentinel := errors.New("ssh down")
	_, release, err = selectClient(ctx, local, func(context.Context, string) (*client.Client, func() error, error) {
		return nil, nil, sentinel
	}, "worker-2")
	if !errors.Is(err, sentinel) {
		t.Fatalf("provider error: want %v, got %v", sentinel, err)
	}
	release() // must not panic
}
