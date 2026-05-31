package docker

import (
	"io"

	"github.com/docker/docker/pkg/stdcopy"
)

// NewLogReader демультиплексирует Docker-лог-стрим (TTY=false) в чистый поток строк.
func NewLogReader(rc io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		rc.Close()
		pw.CloseWithError(err)
	}()
	return pr
}
