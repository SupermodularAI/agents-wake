package jsonl

import (
	"errors"
	"io"
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

func collect(reader io.Reader, maxLine int) ([]string, int, error) {
	var got []string
	skipped, err := Lines(reader, maxLine, func(line []byte) {
		got = append(got, string(line))
	})
	return got, skipped, err
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
