package jsonl

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

func TestLinesDeliversEveryCompleteLine(t *testing.T) {
	got, skipped, err := collect(strings.NewReader("a\nbb\nccc\n"), 8)

	if err != nil {
		t.Fatalf("Lines() error = %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	assertLines(t, got, "a", "bb", "ccc")
}

func TestLinesDeliversAFinalLineWithoutATerminator(t *testing.T) {
	got, skipped, err := collect(strings.NewReader("a\nbb"), 8)

	if err != nil {
		t.Fatalf("Lines() error = %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	assertLines(t, got, "a", "bb")
}

func TestLinesDeliversAnInteriorEmptyLine(t *testing.T) {
	got, skipped, err := collect(strings.NewReader("a\n\nb\n"), 8)

	if err != nil {
		t.Fatalf("Lines() error = %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	assertLines(t, got, "a", "", "b")
}

func TestLinesKeepsTheLinesAroundAnOversizedLine(t *testing.T) {
	input := "short\n" + strings.Repeat("x", 64) + "\ntail\n"

	got, skipped, err := collect(strings.NewReader(input), 16)

	if err != nil {
		t.Fatalf("Lines() error = %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	assertLines(t, got, "short", "tail")
}

func TestLinesRecoversFromAnOversizedLineSpanningBufferFills(t *testing.T) {
	input := "short\n" + strings.Repeat("x", 200*1024) + "\ntail\n"

	got, skipped, err := collect(strings.NewReader(input), 1024)

	if err != nil {
		t.Fatalf("Lines() error = %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	assertLines(t, got, "short", "tail")
}

func TestLinesCountsAnUnterminatedOversizedTail(t *testing.T) {
	input := "ok\n" + strings.Repeat("x", 100)

	got, skipped, err := collect(strings.NewReader(input), 8)

	if err != nil {
		t.Fatalf("Lines() error = %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	assertLines(t, got, "ok")
}

func TestLinesCountsAnUnterminatedOversizedTailEndingOnABufferBoundary(t *testing.T) {
	// A tail whose length is an exact multiple of readBuffer is consumed entirely
	// by the last buffer-full chunk, so the final read returns an empty slice at
	// EOF. The discarded line has to be counted from the oversized flag; counting
	// it from the bytes still in hand reports no blindness at all.
	for _, size := range []int{readBuffer, 2 * readBuffer} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			input := "ok\n" + strings.Repeat("x", size)

			got, skipped, err := collect(strings.NewReader(input), 8)

			if err != nil {
				t.Fatalf("Lines() error = %v", err)
			}
			if skipped != 1 {
				t.Errorf("skipped = %d, want 1", skipped)
			}
			assertLines(t, got, "ok")
		})
	}
}

func TestLinesNeverDeliversAFragmentOfAnOversizedLine(t *testing.T) {
	input := "short\nswordfish" + strings.Repeat("x", 200*1024) + "swordfish\ntail\n"

	got, skipped, err := collect(strings.NewReader(input), 1024)

	if err != nil {
		t.Fatalf("Lines() error = %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	for _, line := range got {
		if strings.Contains(line, "swordfish") {
			t.Fatalf("a fragment of the oversized line was delivered: %q", line)
		}
	}
	assertLines(t, got, "short", "tail")
}

func TestLinesReportsAReadFailureWithoutItsContent(t *testing.T) {
	failing := &failingReader{
		content: "secret-content",
		err:     errors.New("/home/u/.claude/projects/x.jsonl: boom"),
	}

	_, _, err := collect(failing, 1024)

	if err == nil {
		t.Fatal("Lines() error = nil, want a read failure")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), ".claude") {
		t.Fatalf("Lines() error carries source content or a source path: %v", err)
	}
}

type failingReader struct {
	content string
	err     error
	done    bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.done {
		return 0, f.err
	}
	f.done = true
	n := copy(p, f.content)
	return n, f.err
}

func TestLinesReportsEachLineStartOffset(t *testing.T) {
	// "a\n" is bytes 0-1, "bb\r\n" is 2-5, the empty line is 6, "ccc" starts at 7.
	var got []int64
	if _, err := Lines(strings.NewReader("a\nbb\r\n\nccc"), 16, func(offset int64, _ []byte) {
		got = append(got, offset)
	}); err != nil {
		t.Fatalf("Lines() error = %v", err)
	}

	assertOffsets(t, got, 0, 2, 6, 7)
}

func TestLinesReportsTheOffsetAfterASkippedLine(t *testing.T) {
	// An oversized line is never delivered, but the bytes it occupied still move the
	// offset: a cursor built from the line after it would otherwise point inside it.
	oversized := strings.Repeat("x", 40)
	var got []int64
	skipped, err := Lines(strings.NewReader("ok\n"+oversized+"\ntail\n"), 8, func(offset int64, _ []byte) {
		got = append(got, offset)
	})

	if err != nil || skipped != 1 {
		t.Fatalf("Lines() = %d, %v; want 1 skipped and no error", skipped, err)
	}
	assertOffsets(t, got, 0, int64(len(oversized))+4)
}

func TestLinesReportsOffsetsAcrossALineLongerThanTheReadBuffer(t *testing.T) {
	// The offset has to survive a line accumulated over several buffer fills.
	long := strings.Repeat("x", readBuffer*2+5)
	var got []int64
	if _, err := Lines(strings.NewReader("a\n"+long+"\nb\n"), readBuffer*3, func(offset int64, _ []byte) {
		got = append(got, offset)
	}); err != nil {
		t.Fatalf("Lines() error = %v", err)
	}

	assertOffsets(t, got, 0, 2, int64(len(long))+3)
}

func collect(reader io.Reader, maxLine int) ([]string, int, error) {
	var got []string
	skipped, err := Lines(reader, maxLine, func(_ int64, line []byte) {
		got = append(got, string(line))
	})
	return got, skipped, err
}

func assertOffsets(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("offsets = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("offsets = %v, want %v", got, want)
		}
	}
}

func assertLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("lines = %q, want %q", got, want)
		}
	}
}
