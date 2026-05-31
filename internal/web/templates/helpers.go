package templates

import (
	"sort"
	"strconv"
	"strings"
)

// itoa форматирует int64 для подстановки в URL внутри шаблонов.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// envText сериализует map env в текст KEY=VALUE по строке (детерминированно).
func envText(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(env[k])
		b.WriteByte('\n')
	}
	return b.String()
}
