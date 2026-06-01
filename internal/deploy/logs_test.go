package deploy

import (
	"sync"
	"testing"
	"time"
)

func TestLogHubBufferThenLive(t *testing.T) {
	h := NewLogHub()
	h.Open(1)
	w := h.Writer(1)
	w.Write([]byte("line1\n"))

	// Поздний подписчик получает накопленный буфер.
	sub := h.Subscribe(1)
	got := readWithTimeout(t, sub) // "line1\n"
	if got != "line1\n" {
		t.Fatalf("buffer replay = %q", got)
	}

	// Живая строка приходит подписчику.
	w.Write([]byte("line2\n"))
	got = readWithTimeout(t, sub)
	if got != "line2\n" {
		t.Fatalf("live = %q", got)
	}

	// Close отдаёт полный лог и закрывает канал.
	full := h.Close(1)
	if full != "line1\nline2\n" {
		t.Fatalf("full log = %q", full)
	}
	if _, open := <-sub; open {
		t.Error("subscriber channel must be closed after Close")
	}
}

func TestLogHubConcurrent(t *testing.T) {
	h := NewLogHub()
	h.Open(2)
	w := h.Writer(2)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.Write([]byte("x")) }()
	}
	// Подписчики приходят и уходят конкурентно.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s := h.Subscribe(2); h.Unsubscribe(2, s) }()
	}
	wg.Wait()
	_ = h.Close(2)
}

func readWithTimeout(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(time.Second):
		t.Fatal("timeout reading subscriber")
		return ""
	}
}
