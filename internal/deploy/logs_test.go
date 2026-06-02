package deploy

import (
	"strings"
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

func TestLogBufferTruncates(t *testing.T) {
	h := NewLogHub()
	h.Open(1)
	w := h.Writer(1)
	// Write well beyond head+tail cap (256 KiB) so the middle is dropped.
	w.Write([]byte(strings.Repeat("A", 400*1024)))
	tailMark := "TAIL-MARKER-LINE\n"
	w.Write([]byte(tailMark))
	full := h.Close(1)
	if len(full) > 300*1024 {
		t.Fatalf("buffer not bounded: %d bytes", len(full))
	}
	if !strings.Contains(full, "truncated") {
		t.Error("expected a truncation marker when middle bytes are dropped")
	}
	if !strings.HasPrefix(full, "AAA") {
		t.Error("head must be preserved")
	}
	if !strings.HasSuffix(full, tailMark) {
		t.Error("tail must be preserved")
	}
}

func TestLogBufferSmallUntouched(t *testing.T) {
	h := NewLogHub()
	h.Open(2)
	h.Writer(2).Write([]byte("hello\n"))
	if got := h.Close(2); got != "hello\n" {
		t.Errorf("small log altered: %q", got)
	}
}

func TestLogBufferNoMarkerWhenFits(t *testing.T) {
	// Content larger than head cap but total <= head+tail cap: nothing is
	// dropped, so head+tail is the COMPLETE log and there must be NO marker.
	h := NewLogHub()
	h.Open(3)
	body := strings.Repeat("B", 150*1024) // > 128 KiB head, < 256 KiB total
	h.Writer(3).Write([]byte(body))
	full := h.Close(3)
	if strings.Contains(full, "truncated") {
		t.Error("must NOT show a truncation marker when nothing was dropped")
	}
	if full != body {
		t.Error("head+tail must equal the full content when nothing dropped")
	}
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
