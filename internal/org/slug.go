package org

import "strings"

// Slugify приводит человекочитаемое имя к slug: lowercase, [a-z0-9-],
// последовательности разделителей схлопываются в один дефис, края тримятся.
// Возвращает "" если валидных символов нет (вызывающая сторона должна это проверить).
func Slugify(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '-' || r == '_' || r == '.':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			// не-ascii и спецсимволы игнорируются
		}
	}
	return strings.TrimRight(b.String(), "-")
}
