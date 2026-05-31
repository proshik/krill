package templates

import "strconv"

// itoa форматирует int64 для подстановки в URL внутри шаблонов.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }
