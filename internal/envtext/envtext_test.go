package envtext

import (
	"reflect"
	"testing"
)

// The bug this package was extracted to fix: two of the four former parsers
// did not recognise comments, so a commented-out line became a variable whose
// name began with '#'. The deploy path used one of those, so a container
// really did receive it.
func TestCommentsAreNotVariables(t *testing.T) {
	raw := "# DEBUG=1\n   # indented=yes\nPORT=8080"

	if got := Keys(raw); !reflect.DeepEqual(got, []string{"PORT"}) {
		t.Fatalf("Keys = %v, want [PORT]", got)
	}
	env, _ := Map(raw)
	if _, bad := env["# DEBUG"]; bad {
		t.Fatalf("a commented line became the variable %q: %v", "# DEBUG", env)
	}
	if len(env) != 1 || env["PORT"] != "8080" {
		t.Fatalf("Map = %v, want {PORT:8080}", env)
	}
}

func TestParsePreservesFileOrderAndRepeats(t *testing.T) {
	got := Parse("B=2\nA=1\nB=3")
	want := []Pair{{"B", "2"}, {"A", "1"}, {"B", "3"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %v, want %v", got, want)
	}
}

func TestMapLastWinsAndReportsDuplicates(t *testing.T) {
	env, dups := Map("A=1\nA=2\nB=3")
	if env["A"] != "2" {
		t.Fatalf("last occurrence should win, got A=%q", env["A"])
	}
	if !reflect.DeepEqual(dups, []string{"A"}) {
		t.Fatalf("dups = %v, want [A]", dups)
	}
}

func TestSkipsLinesThatDeclareNothing(t *testing.T) {
	env, dups := Map("\n\n   \nnot-a-pair\n=novalue\n   =  \nOK=1")
	if len(env) != 1 || env["OK"] != "1" {
		t.Fatalf("Map = %v, want {OK:1}", env)
	}
	if dups != nil {
		t.Fatalf("dups = %v, want none", dups)
	}
}

func TestValueKeepsInnerEqualsAndIsTrimmed(t *testing.T) {
	env, _ := Map("  DSN = postgres://u:p@h/db?a=b  \nEMPTY=")
	if env["DSN"] != "postgres://u:p@h/db?a=b" {
		t.Fatalf("DSN = %q", env["DSN"])
	}
	if v, ok := env["EMPTY"]; !ok || v != "" {
		t.Fatalf("EMPTY = %q, present=%v; an explicitly empty value is still a variable", v, ok)
	}
}

func TestKeyOf(t *testing.T) {
	cases := []struct {
		line string
		key  string
		ok   bool
	}{
		{"PORT=8080", "PORT", true},
		{"  PORT = 8080", "PORT", true},
		{"# PORT=8080", "", false},
		{"   # PORT=8080", "", false},
		{"", "", false},
		{"   ", "", false},
		{"PORT", "", false},
		{"=8080", "", false},
	}
	for _, c := range cases {
		key, ok := KeyOf(c.line)
		if key != c.key || ok != c.ok {
			t.Errorf("KeyOf(%q) = (%q,%v), want (%q,%v)", c.line, key, ok, c.key, c.ok)
		}
	}
}
