package docker

import (
	"context"
	"errors"
	"testing"
)

func TestResolveRefs(t *testing.T) {
	ids := map[string]string{"a": "id-a", "b": "id-b"}
	lookup := func(_ context.Context, name string) (string, error) {
		if id, ok := ids[name]; ok {
			return id, nil
		}
		return "", errObjectNotFound
	}
	in := []FileRef{{Name: "a", Target: "/x"}, {Name: "b", Target: "y", Mode: 0o400}}
	out, err := resolveRefs(context.Background(), in, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].ID != "id-a" || out[1].ID != "id-b" || out[1].Mode != 0o400 || out[0].Target != "/x" {
		t.Errorf("resolved = %+v", out)
	}
	if in[0].ID != "" {
		t.Error("resolveRefs mutated its input")
	}
	if _, err := resolveRefs(context.Background(), []FileRef{{Name: "missing"}}, lookup); !errors.Is(err, errObjectNotFound) {
		t.Errorf("missing object: err = %v", err)
	}
	if out, err := resolveRefs(context.Background(), nil, lookup); err != nil || out != nil {
		t.Errorf("nil refs: %v %v", out, err)
	}
}

func TestObjectInUse(t *testing.T) {
	if !objectInUse(errors.New(`Error response from daemon: rpc error: code = InvalidArgument desc = config 'x' is in use by the following service: krill-alloy-node`)) {
		t.Error("in-use error not recognized")
	}
	if objectInUse(errors.New("permission denied")) || objectInUse(nil) {
		t.Error("false positive")
	}
}
