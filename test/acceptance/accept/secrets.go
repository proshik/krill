package accept

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"filippo.io/age"
	"golang.org/x/crypto/bcrypt"
)

// AgeIdentityRe matches an age X25519 identity (a private key).
var AgeIdentityRe = regexp.MustCompile(`AGE-SECRET-KEY-1[0-9A-Z]+`)

// NewAgeKey generates a key pair and writes it to dir/krill-backup-key.txt
// (mode 0600) in the format age-keygen writes. It returns the recipient (the
// public key), the file's path and the identity (for redaction only).
func NewAgeKey(dir string) (recipient, path, identity string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", "", err
	}
	recipient = id.Recipient().String()
	path = filepath.Join(dir, "krill-backup-key.txt")
	body := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n", time.Now().UTC().Format(time.RFC3339), recipient, id.String())
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", "", "", err
	}
	// WriteFile keeps the mode of an existing file.
	if err := os.Chmod(path, 0o600); err != nil {
		return "", "", "", err
	}
	return recipient, path, id.String(), nil
}

// Htpasswd returns one htpasswd line (bcrypt) for the registry's basic auth.
func Htpasswd(user, password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return user + ":" + string(h), nil
}

// ScanForSecrets returns a label for every secret found in text: "value #N"
// for values[N] and "age identity" for an age private key. The labels never
// contain the secret itself.
func ScanForSecrets(text string, values []string) []string {
	var hits []string
	for i, v := range values {
		if v != "" && strings.Contains(text, v) {
			hits = append(hits, fmt.Sprintf("value #%d", i))
		}
	}
	if AgeIdentityRe.MatchString(text) {
		hits = append(hits, "age identity")
	}
	return hits
}
