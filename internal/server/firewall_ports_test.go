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
		got, err := controlPlaneServicePorts(addr)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: got %v %v, want %v", addr, got, err, want)
		}
	}
	for _, bad := range []string{"", "8080", ":http", ":0", ":70000"} {
		if _, err := controlPlaneServicePorts(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}
