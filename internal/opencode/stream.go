package opencode

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// Forward bytes unchanged, but finish at the SSE terminator instead of waiting
// for a possibly never-arriving TCP EOF after the model has already finished.
func copyChatStream(dst io.Writer, src io.Reader) error {
	reader := bufio.NewReader(src)
	var line []byte
	var data []string
	eventBytes := 0
	for {
		chunk, err := reader.ReadSlice('\n')
		eventBytes += len(chunk)
		if eventBytes > maxEventBytes {
			return errors.New("OpenCode SSE event exceeds limit")
		}
		if len(chunk) > 0 {
			n, writeErr := dst.Write(chunk)
			if writeErr != nil {
				return writeErr
			}
			if n != len(chunk) {
				return io.ErrShortWrite
			}
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		line = line[:0]
		if text == "" {
			if strings.Join(data, "\n") == "[DONE]" {
				return nil
			}
			data, eventBytes = nil, 0
			continue
		}
		field, value, _ := strings.Cut(text, ":")
		if field == "data" {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
}
