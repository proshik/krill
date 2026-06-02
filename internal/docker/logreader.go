package docker

import (
	"io"

	"github.com/docker/docker/pkg/stdcopy"
)

// NewLogReader demultiplexes a Docker log stream (TTY=false) into a clean stream of lines.
// The caller MUST close the returned io.ReadCloser (Close), otherwise the
// background demultiplexing goroutine and the source log stream will leak.
func NewLogReader(rc io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		rc.Close()
		pw.CloseWithError(err)
	}()
	return pr
}
