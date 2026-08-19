package style

import "testing"

func TestPaintIsANoOpWhenNotPretty(t *testing.T) {
	if got := Paint(false, Red, "x"); got != "x" {
		t.Errorf("Paint(false, ...) = %q, want unstyled text", got)
	}
}

func TestPaintWrapsInCodeAndResetWhenPretty(t *testing.T) {
	want := Red + "x" + Reset
	if got := Paint(true, Red, "x"); got != want {
		t.Errorf("Paint(true, ...) = %q, want %q", got, want)
	}
}

func TestPaintNeverColorsEmptyText(t *testing.T) {
	if got := Paint(true, Red, ""); got != "" {
		t.Errorf("Paint(true, Red, \"\") = %q, want an untouched empty string", got)
	}
}

func TestHeadingIsBoldAndLime(t *testing.T) {
	want := Bold + Lime + "x" + Reset
	if got := Heading(true, "x"); got != want {
		t.Errorf("Heading(true, %q) = %q, want %q", "x", got, want)
	}
	if got := Heading(false, "x"); got != "x" {
		t.Errorf("Heading(false, %q) = %q, want unstyled text", "x", got)
	}
}

func TestVisibleWidthSkipsANSIEscapes(t *testing.T) {
	colored := Paint(true, Bold+Red, "abc")
	if got := VisibleWidth(colored); got != 3 {
		t.Errorf("VisibleWidth(%q) = %d, want 3", colored, got)
	}
	if got := VisibleWidth("abc"); got != 3 {
		t.Errorf("VisibleWidth(plain) = %d, want 3", got)
	}
}

func TestPadAlignsByVisibleWidthNotByteLength(t *testing.T) {
	colored := Paint(true, Red, "ab")
	padded := Pad(colored, 5)
	if got := VisibleWidth(padded); got != 5 {
		t.Errorf("VisibleWidth(Pad(colored, 5)) = %d, want 5", got)
	}
	if got := Pad("ab", 5); got != "ab   " {
		t.Errorf("Pad(plain, 5) = %q, want %q", got, "ab   ")
	}
	if got := Pad("abcde", 3); got != "abcde" {
		t.Errorf("Pad() shrank text longer than width: got %q", got)
	}
}
