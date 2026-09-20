//go:build integration

package docker

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestTaskAddressesAndServiceContainerLabels(t *testing.T) {
	eng, err := NewEngine("")
	if err != nil {
		t.Fatal(err)
	}
	e := eng.(*dockerEngine)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	name := fmt.Sprintf("krill-test-addr-%d", time.Now().UnixNano())
	if _, err := e.NetworkEnsure(ctx, name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		if err := e.ServiceRemove(cleanup, name); err != nil {
			t.Error(err)
		}
		for cleanup.Err() == nil {
			if err := e.NetworkRemove(cleanup, name); err == nil {
				return
			}
			time.Sleep(time.Second)
		}
		t.Error("test overlay network cleanup timed out")
	})
	if err := e.ServiceDeploy(ctx, ServiceSpec{Name: name, Image: "busybox:1.38.0", Command: []string{"sh", "-c", `trap 'exit 0' TERM; sleep 3600 & wait`}, Replicas: 1, Network: name, ContainerLabels: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	labels, found, err := e.ServiceContainerLabels(ctx, name)
	if err != nil || !found || labels["k"] != "v" {
		t.Fatalf("container labels=%v found=%v err=%v", labels, found, err)
	}
	if _, found, err := e.ServiceContainerLabels(ctx, name+"-absent"); err != nil || found {
		t.Fatalf("missing service found=%v err=%v", found, err)
	}
	for ctx.Err() == nil {
		addrs, err := e.TaskAddresses(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, addr := range addrs {
			if addr.ServiceName == name && addr.Network == name {
				if net.ParseIP(addr.IP) == nil || addr.NodeHostname == "" {
					t.Fatalf("invalid task address: %+v", addr)
				}
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("no running task address before deadline")
}
