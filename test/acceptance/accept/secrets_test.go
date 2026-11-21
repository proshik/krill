package accept

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"filippo.io/age"
	"golang.org/x/crypto/bcrypt"
)

func TestNewAgeKeyRoundTrip(t *testing.T) {
	recipient, path, identity, err := NewAgeKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v (err %v), want 0600", st.Mode().Perm(), err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	if lines[1] != "# public key: "+recipient || lines[2] != identity {
		t.Fatalf("key file lines %q", lines)
	}
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		t.Fatal(err)
	}
	var enc bytes.Buffer
	w, err := age.Encrypt(&enc, r)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "state")
	w.Close()
	ids, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	rd, err := age.Decrypt(&enc, ids...)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(rd); string(got) != "state" {
		t.Fatalf("round trip gave %q", got)
	}
}

func TestHtpasswdVerifies(t *testing.T) {
	line, err := Htpasswd("accept", "pw")
	if err != nil {
		t.Fatal(err)
	}
	user, hash, ok := strings.Cut(line, ":")
	if !ok || user != "accept" || !strings.HasPrefix(hash, "$2") {
		t.Fatalf("htpasswd line %q", line)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("pw")); err != nil {
		t.Fatal(err)
	}
}

func TestScanForSecrets(t *testing.T) {
	values := []string{"", "s3cr3t-value"}
	text := "log line s3cr3t-value and AGE-SECRET-KEY-1QQQQ"
	hits := ScanForSecrets(text, values)
	if strings.Join(hits, ",") != "value #1,age identity" {
		t.Fatalf("hits %v", hits)
	}
	for _, h := range hits {
		if strings.Contains(h, "s3cr3t") || strings.Contains(h, "AGE-SECRET") {
			t.Fatalf("label %q carries the secret", h)
		}
	}
	if hits := ScanForSecrets("clean text", values); len(hits) != 0 {
		t.Fatalf("clean text gave %v", hits)
	}
}
