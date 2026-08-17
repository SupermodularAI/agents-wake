// Package jsonl reads newline-delimited JSON one line at a time under an
// explicit length limit, recovering per line.
//
// A single oversized line must not cost a whole file's records: a harness writes
// one JSON object per line and one enormous tool result is ordinary, while losing
// every event around it is the difference between "collects nothing" and
// "collects zero".
package jsonl

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// errRead is deliberately valueless. An io error from a harness file carries that
// file's path, and a transcript path encodes the repository path it belongs to,
// so no read failure may be wrapped out of this package.
var errRead = errors.New("reading newline-delimited input")

// readBuffer is the working buffer. maxLine may exceed it: a longer line is
// accumulated across fills, and one that passes maxLine is discarded whole.
const readBuffer = 64 * 1024

// Lines calls visit once per line, without its terminator, and returns how many
// lines were discarded for exceeding maxLine. A line longer than maxLine is not
// delivered at all — never a prefix of one — and the lines before and after it
// are still delivered. line aliases the read buffer and is valid only until visit
// returns, so visit must copy anything it keeps.
func Lines(reader io.Reader, maxLine int, visit func(line []byte)) (int, error) {
	buffered := bufio.NewReaderSize(reader, readBuffer)
	skipped := 0
	// pending holds the head of a line one buffer fill could not return whole.
	var pending []byte
	oversized := false
	for {
		chunk, err := buffered.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if oversized || len(pending)+len(chunk) > maxLine {
				// Stop accumulating: the line is already too long to deliver, and
				// buffering the rest of it would be the cost this limit exists to cap.
				oversized, pending = true, pending[:0]
				continue
			}
			pending = append(pending, chunk...)
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return skipped, errRead
		}
		terminated := err == nil
		line := trimEOL(chunk)
		if len(pending) > 0 {
			// The head of this line arrived in an earlier fill; pending keeps its
			// capacity for the next long line rather than being reallocated per line.
			pending = append(pending, line...)
			line = pending
		}
		// oversized carries the case of a discarded line the input ended on with
		// nothing left in hand: a tail that is an exact multiple of readBuffer
		// leaves an empty final chunk, and the line still has to be counted.
		if len(line) > 0 || terminated || oversized {
			switch {
			case oversized || len(line) > maxLine:
				skipped++
			default:
				visit(line)
			}
		}
		if !terminated {
			return skipped, nil
		}
		oversized, pending = false, pending[:0]
	}
}

// trimEOL drops the line terminator, tolerating CRLF.
func trimEOL(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	return bytes.TrimSuffix(line, []byte{'\r'})
}
