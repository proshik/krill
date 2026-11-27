package server

import (
	"reflect"
	"testing"
)

func TestControlPlaneServicePorts(t *testing.T) {
	for addr, want := range map[string][]int{
		":8080":         {80, 443, 8080},
		"0.0.0.0:18080": {80, 443, 18080},
		"[::]:9000":     {80, 443, 9000},
	} {
		got, err := controlPlaneServicePorts(addr, false, 0)
		if err != nil || !reflect.DeepEqual(got.Public, want) || got.Gateway != nil {
			t.Fatalf("%q: got %v %v, want %v", addr, got, err, want)
		}
	}
	for _, bad := range []string{"", "8080", ":http", ":0", ":70000"} {
		if _, err := controlPlaneServicePorts(bad, true, 0); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

// Closing direct access narrows the UI port to the gateway; it never drops it,
// since the gateway proxies the panel and polls its routes on that port.
func TestControlPlaneServicePortsCloseDirect(t *testing.T) {
	got, err := controlPlaneServicePorts(":8080", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Public, []int{80, 443}) || !reflect.DeepEqual(got.Gateway, []int{8080}) {
		t.Fatalf("got %+v", got)
	}
}

// While the link role is not off its listener port is open to the gateway
// only, never to the world, whether or not the UI port is closed.
func TestControlPlaneServicePortsLink(t *testing.T) {
	got, err := controlPlaneServicePorts(":8080", false, 8445)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Public, []int{80, 443, 8080}) || !reflect.DeepEqual(got.Gateway, []int{8445}) {
		t.Fatalf("open UI: got %+v", got)
	}
	got, err = controlPlaneServicePorts(":8080", true, 8445)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Public, []int{80, 443}) || !reflect.DeepEqual(got.Gateway, []int{8080, 8445}) {
		t.Fatalf("closed UI: got %+v", got)
	}
}
