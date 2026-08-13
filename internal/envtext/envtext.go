// Package envtext parses `applications.env_text` — the raw, order-preserving
// KEY=VALUE lines that are the single source of truth for an application's
// environment.
//
// It exists because the format was previously re-parsed at four call sites
// (the form save, the deploy-time injection, the API's key listing and the
// API's targeted line edit) with three different sets of semantics. Two of
// them did not recognise comments, so a line like "# DEBUG=1" was injected
// into the container as a variable literally named "# DEBUG" while the API
// correctly omitted it — the same file described two different environments
// depending on who read it.
//
// This package owns tokenization only. Policy stays with the caller: the form
// save reports duplicates, the deploy path lets the last occurrence win, and
// the targeted edit refuses to touch a key that appears twice.
package envtext

import "strings"

// Pair is one variable line, in the order it appears in the file.
type Pair struct {
	Key   string
	Value string
}

// IsComment reports whether a line is a comment. Leading whitespace is allowed
// before the '#', so an indented comment is still a comment.
func IsComment(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "#")
}

// KeyOf returns the variable name a line declares, and whether it declares one
// at all. Blank lines, comments and lines with no '=' declare nothing.
//
// It is the single predicate for "is this line a variable, and which one" —
// callers that rewrite the file line by line use it so their idea of a
// variable line matches the one used to build the deploy environment.
func KeyOf(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || IsComment(trimmed) {
		return "", false
	}
	k, _, ok := strings.Cut(trimmed, "=")
	if !ok {
		return "", false
	}
	if k = strings.TrimSpace(k); k == "" {
		return "", false
	}
	return k, true
}

// Parse returns every variable line in file order, including repeats — the
// caller decides what a repeated key means.
func Parse(raw string) []Pair {
	var out []Pair
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		key, ok := KeyOf(trimmed)
		if !ok {
			continue
		}
		_, value, _ := strings.Cut(trimmed, "=")
		out = append(out, Pair{Key: key, Value: strings.TrimSpace(value)})
	}
	return out
}

// Map collapses Parse into a map with the last occurrence winning, and also
// reports the keys that appeared more than once. A caller that must produce an
// environment regardless (the deploy path) ignores dups; a caller that can
// reject the input (the form save) uses them.
func Map(raw string) (map[string]string, []string) {
	env := map[string]string{}
	var dups []string
	for _, p := range Parse(raw) {
		if _, seen := env[p.Key]; seen {
			dups = append(dups, p.Key)
		}
		env[p.Key] = p.Value
	}
	return env, dups
}

// Keys returns the variable names in file order, including repeats.
func Keys(raw string) []string {
	pairs := Parse(raw)
	keys := make([]string, 0, len(pairs))
	for _, p := range pairs {
		keys = append(keys, p.Key)
	}
	return keys
}
