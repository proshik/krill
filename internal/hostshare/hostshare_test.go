package hostshare

import (
	"errors"
	"testing"
)

func TestOverlaps(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"/api", "/api", true},
		{"/api", "/api/v2", true},
		{"/api/v2", "/api", true},
		{"/api", "/apiary", false},
		{"/api", "/app", false},
		{"/a/b", "/a/bc", false},
	}
	for _, c := range cases {
		if got := Overlaps(c.a, c.b); got != c.want {
			t.Errorf("Overlaps(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func conflictKey(t *testing.T, err error) string {
	t.Helper()
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("want *ConflictError, got %v", err)
	}
	return ce.Key
}

func TestCheck(t *testing.T) {
	web := Claim{DomainID: 1, AppID: 10, AppName: "web", EnvID: 5} // catch-all
	api := Claim{DomainID: 2, AppID: 11, AppName: "api", EnvID: 5, Paths: []string{"/api"}}

	t.Run("free host", func(t *testing.T) {
		if err := Check(nil, Claim{AppID: 1, EnvID: 5}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("paths next to a catch-all", func(t *testing.T) {
		if err := Check([]Claim{web}, Claim{AppID: 11, AppName: "api", EnvID: 5, Paths: []string{"/api"}}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("disjoint paths", func(t *testing.T) {
		if err := Check([]Claim{web, api}, Claim{AppID: 12, EnvID: 5, Paths: []string{"/apiary"}}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("another environment", func(t *testing.T) {
		err := Check([]Claim{web}, Claim{AppID: 20, EnvID: 6, Paths: []string{"/x"}})
		if k := conflictKey(t, err); k != KeyHostTaken {
			t.Fatalf("key = %s", k)
		}
		if ce := err.(*ConflictError); len(ce.Args) != 0 {
			t.Fatalf("a foreign host must not name its app: %v", ce.Args)
		}
	})
	t.Run("same app twice", func(t *testing.T) {
		if k := conflictKey(t, Check([]Claim{api}, Claim{AppID: 11, EnvID: 5, Paths: []string{"/other"}})); k != KeyHostOnThisApp {
			t.Fatalf("key = %s", k)
		}
	})
	t.Run("second catch-all", func(t *testing.T) {
		err := Check([]Claim{web}, Claim{AppID: 12, EnvID: 5})
		if k := conflictKey(t, err); k != KeyCatchAllTaken {
			t.Fatalf("key = %s", k)
		}
		if ce := err.(*ConflictError); len(ce.Args) != 1 || ce.Args[0] != "web" {
			t.Fatalf("args = %v", ce.Args)
		}
	})
	t.Run("nested path in another app", func(t *testing.T) {
		err := Check([]Claim{web, api}, Claim{AppID: 12, EnvID: 5, Paths: []string{"/api/v2"}})
		if k := conflictKey(t, err); k != KeyPathOverlap {
			t.Fatalf("key = %s", k)
		}
		if ce := err.(*ConflictError); len(ce.Args) != 3 || ce.Args[0] != "/api/v2" || ce.Args[1] != "/api" || ce.Args[2] != "api" {
			t.Fatalf("args = %v", ce.Args)
		}
	})
	t.Run("editing own row does not conflict with itself", func(t *testing.T) {
		if err := Check([]Claim{web, api}, Claim{DomainID: 2, AppID: 11, EnvID: 5, Paths: []string{"/api", "/api/v2"}}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("own row turned catch-all while another catch-all exists", func(t *testing.T) {
		if k := conflictKey(t, Check([]Claim{web, api}, Claim{DomainID: 2, AppID: 11, EnvID: 5})); k != KeyCatchAllTaken {
			t.Fatalf("key = %s", k)
		}
	})
	t.Run("foreign environment wins over a same-environment conflict", func(t *testing.T) {
		foreign := Claim{DomainID: 9, AppID: 99, AppName: "secret", EnvID: 7}
		if k := conflictKey(t, Check([]Claim{foreign, web}, Claim{AppID: 12, EnvID: 5})); k != KeyHostTaken {
			t.Fatalf("key = %s", k)
		}
	})
}

func TestCheckRedirectTakesTheWholeHost(t *testing.T) {
	redir := Claim{DomainID: 1, AppID: 1, AppName: "old", EnvID: 1, Redirect: true}
	if got := conflictKey(t, Check([]Claim{redir}, Claim{AppID: 2, EnvID: 1, Paths: []string{"/api"}})); got != KeyHostRedirects {
		t.Fatalf("adding beside a redirect: %s", got)
	}
	plain := Claim{DomainID: 1, AppID: 1, AppName: "web", EnvID: 1, Paths: []string{"/web"}}
	if got := conflictKey(t, Check([]Claim{plain}, Claim{AppID: 2, EnvID: 1, Paths: []string{"/api"}, Redirect: true})); got != KeyHostRedirects {
		t.Fatalf("redirecting a shared host: %s", got)
	}
	if err := Check([]Claim{redir}, redir); err != nil {
		t.Fatalf("a redirect row alone on its host: %v", err)
	}
}
