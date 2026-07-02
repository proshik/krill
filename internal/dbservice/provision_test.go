package dbservice

import "testing"

func TestValidIdent(t *testing.T) {
	ok := []string{"shop", "_x", "a1_b2", "a"}
	bad := []string{"", "1abc", "Shop", "a-b", "a b", "тест", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} // 64 chars
	for _, s := range ok {
		if !ValidIdent(s) {
			t.Errorf("ValidIdent(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidIdent(s) {
			t.Errorf("ValidIdent(%q) = true, want false", s)
		}
	}
}

func TestSanitizeIdent(t *testing.T) {
	cases := map[string]string{"My Shop": "my_shop", "1abc": "db_1abc", "app-db": "app_db", "ПРИВЕТ": "db"}
	for in, want := range cases {
		if got := SanitizeIdent(in); got != want {
			t.Errorf("SanitizeIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateDBSQL(t *testing.T) {
	got := createDBSQL("shop", "shop_u", "p_w-9")
	want := "CREATE USER \"shop_u\" PASSWORD 'p_w-9';\n" +
		"CREATE DATABASE \"shop\" OWNER \"shop_u\";\n" +
		"REVOKE CONNECT ON DATABASE \"shop\" FROM PUBLIC;\n"
	if got != want {
		t.Fatalf("createDBSQL:\n%q\nwant:\n%q", got, want)
	}
}

func TestDropDBSQL(t *testing.T) {
	got := dropDBSQL("shop", "shop_u")
	want := "DROP DATABASE IF EXISTS \"shop\" WITH (FORCE);\nDROP USER IF EXISTS \"shop_u\";\n"
	if got != want {
		t.Fatalf("dropDBSQL:\n%q\nwant:\n%q", got, want)
	}
}
