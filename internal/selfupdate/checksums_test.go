package selfupdate

import (
	"strings"
	"testing"
)

const (
	hashA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	hashB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func TestParseChecksums_Forms(t *testing.T) {
	data := hashA + "  krill-linux-amd64\n" +
		"\n" +
		strings.ToUpper(hashB) + " *krill-linux-arm64\n" +
		"   \n" +
		hashA + "  krill-cli_0.2.0_linux_amd64.tar.gz\n"

	got, err := parseChecksums([]byte(data))
	if err != nil {
		t.Fatalf("parseChecksums() error = %v", err)
	}
	want := map[string]string{
		"krill-linux-amd64":                  hashA,
		"krill-linux-arm64":                  hashB,
		"krill-cli_0.2.0_linux_amd64.tar.gz": hashA,
	}
	if len(got) != len(want) {
		t.Fatalf("parseChecksums() = %v, want %v", got, want)
	}
	for name, sum := range want {
		if got[name] != sum {
			t.Errorf("entry %q = %q, want %q", name, got[name], sum)
		}
	}
}

func TestParseChecksums_CRLF(t *testing.T) {
	got, err := parseChecksums([]byte(hashA + "  krill-linux-amd64\r\n"))
	if err != nil {
		t.Fatalf("parseChecksums() error = %v", err)
	}
	if got["krill-linux-amd64"] != hashA {
		t.Errorf("entry = %q, want %q", got["krill-linux-amd64"], hashA)
	}
}

func TestParseChecksums_Empty(t *testing.T) {
	got, err := parseChecksums(nil)
	if err != nil {
		t.Fatalf("parseChecksums(nil) error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parseChecksums(nil) = %v, want empty", got)
	}
}

func TestParseChecksums_Malformed(t *testing.T) {
	cases := map[string]string{
		"garbage line":        hashA + "  krill-linux-amd64\nnot a checksum line\n",
		"short hex":           hashA[:63] + "  krill-linux-amd64\n",
		"long hex":            hashA + "0  krill-linux-amd64\n",
		"non-hex":             strings.Repeat("g", 64) + "  krill-linux-amd64\n",
		"single space":        hashA + " krill-linux-amd64\n",
		"missing name":        hashA + "  \n",
		"missing binary name": hashA + " *\n",
		"hash only":           hashA + "\n",
		"duplicate name":      hashA + "  krill-linux-amd64\n" + hashB + "  krill-linux-amd64\n",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := parseChecksums([]byte(data)); err == nil {
				t.Errorf("parseChecksums() = %v, want an error", got)
			}
		})
	}
}
