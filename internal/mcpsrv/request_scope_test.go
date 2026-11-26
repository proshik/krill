package mcpsrv

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/proshik/krill/internal/api"
)

// TestRequestScopedEndsWithTheRequest: a waiting tool runs on its session's
// context, so only the carrying request's done channel tells it the caller
// left.
func TestRequestScopedEndsWithTheRequest(t *testing.T) {
	reqCtx, leave := context.WithCancel(context.Background())
	ti, err := bindSession(api.WithIdentity(reqCtx, api.Identity{TokenID: 7}), "", nil)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{TokenInfo: ti}}
	ctx, cancel := requestScoped(context.Background(), req)
	defer cancel()
	leave()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the call's context outlived its request")
	}

	// Without request info the context is the session's own.
	ctx2, cancel2 := requestScoped(context.Background(), &mcp.CallToolRequest{Extra: &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{}}})
	defer cancel2()
	if ctx2.Err() != nil {
		t.Fatal("ended without a request to end it")
	}
}
