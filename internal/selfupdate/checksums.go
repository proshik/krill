package selfupdate

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// parseChecksums parses a `sha256sum` listing (the release's checksums.txt)
// into a map of file name to lowercase hex digest. Accepted lines are
// "<64 hex>  <name>" (text mode) and "<64 hex> *<name>" (binary mode); blank
// lines are ignored. Any other line, and a name listed twice, is an error:
// the listing is the only integrity anchor for the binary about to run as
// root, so anything unexpected in it fails closed.
func parseChecksums(data []byte) (map[string]string, error) {
	sums := make(map[string]string)
	sc := bufio.NewScanner(bytes.NewReader(data))
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSuffix(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// 64 hex digits, a space, a mode character (' ' or '*'), a name.
		if len(line) < 67 || line[64] != ' ' || (line[65] != ' ' && line[65] != '*') {
			return nil, fmt.Errorf("checksums.txt line %d: malformed", lineNo)
		}
		sum := strings.ToLower(line[:64])
		if !isHex(sum) {
			return nil, fmt.Errorf("checksums.txt line %d: malformed digest", lineNo)
		}
		name := line[66:]
		if _, dup := sums[name]; dup {
			return nil, fmt.Errorf("checksums.txt line %d: duplicate entry for %q", lineNo, name)
		}
		sums[name] = sum
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("checksums.txt: %w", err)
	}
	return sums, nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
