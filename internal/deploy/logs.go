package deploy

import (
	"strings"
	"sync"
)

// DeployLogHub держит логи идущих деплоев: буфер + живые подписчики.
type DeployLogHub struct {
	mu    sync.Mutex
	feeds map[int64]*feed
}

type feed struct {
	buf  strings.Builder
	subs map[chan string]struct{}
}

func NewLogHub() *DeployLogHub {
	return &DeployLogHub{feeds: map[int64]*feed{}}
}

// Open регистрирует деплой.
func (h *DeployLogHub) Open(deployID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.feeds[deployID]; !ok {
		h.feeds[deployID] = &feed{subs: map[chan string]struct{}{}}
	}
}

// Writer возвращает io.Writer для деплоя (пишет в буфер и рассылает подписчикам).
func (h *DeployLogHub) Writer(deployID int64) *hubWriter {
	return &hubWriter{hub: h, deployID: deployID}
}

type hubWriter struct {
	hub      *DeployLogHub
	deployID int64
}

func (w *hubWriter) Write(p []byte) (int, error) {
	w.hub.mu.Lock()
	defer w.hub.mu.Unlock()
	f := w.hub.feeds[w.deployID]
	if f == nil {
		return len(p), nil // деплой уже закрыт — молча игнорируем
	}
	s := string(p)
	f.buf.WriteString(s)
	for ch := range f.subs {
		select {
		case ch <- s:
		default: // медленный подписчик — не блокируем воркер
		}
	}
	return len(p), nil
}

// Subscribe подписывается; сразу получает накопленный буфер первой строкой.
func (h *DeployLogHub) Subscribe(deployID int64) chan string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan string, 256)
	f := h.feeds[deployID]
	if f == nil {
		close(ch) // деплой не идёт
		return ch
	}
	if f.buf.Len() > 0 {
		ch <- f.buf.String()
	}
	f.subs[ch] = struct{}{}
	return ch
}

// Unsubscribe удаляет подписчика.
func (h *DeployLogHub) Unsubscribe(deployID int64, ch chan string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f := h.feeds[deployID]; f != nil {
		if _, ok := f.subs[ch]; ok {
			delete(f.subs, ch)
			close(ch)
		}
	}
}

// Close закрывает деплой: возвращает полный лог, закрывает подписчиков и удаляет фид.
func (h *DeployLogHub) Close(deployID int64) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.feeds[deployID]
	if f == nil {
		return ""
	}
	for ch := range f.subs {
		close(ch)
	}
	full := f.buf.String()
	delete(h.feeds, deployID)
	return full
}

// Active сообщает, идёт ли деплой (есть фид).
func (h *DeployLogHub) Active(deployID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.feeds[deployID]
	return ok
}
