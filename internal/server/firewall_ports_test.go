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
		got, err := controlPlaneServicePorts(addr, false)
		if err != nil || !reflect.DeepEqual(got.Public, want) || got.Gateway != nil {
			t.Fatalf("%q: got %v %v, want %v", addr, got, err, want)
		}
	}
	for _, bad := range []string{"", "8080", ":http", ":0", ":70000"} {
		if _, err := controlPlaneServicePorts(bad, true); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

// Closing direct access narrows the UI port to the gateway; it never drops it,
// since the gateway proxies the panel and polls its routes on that port.
func TestControlPlaneServicePortsCloseDirect(t *testing.T) {
	got, err := controlPlaneServicePorts(":8080", true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Public, []int{80, 443}) || !reflect.DeepEqual(got.Gateway, []int{8080}) {
		t.Fatalf("got %+v", got)
	}
}
