package inventory

import "testing"

func TestStripsLineComments(t *testing.T) {
	source := "{\n  // leading\n  \"a\": 1, // trailing\n  \"b\": 2\n}"
	if got := string(stripJSONComments([]byte(source))); got == source {
		t.Fatalf("no comment was removed from:\n%s", source)
	} else if contains(got, "leading") || contains(got, "trailing") {
		t.Fatalf("a comment survived:\n%s", got)
	}
}

func TestStripsBlockComments(t *testing.T) {
	got := string(stripJSONComments([]byte(`{/* a block */ "a": 1 /* another */}`)))
	if contains(got, "block") || contains(got, "another") {
		t.Fatalf("a block comment survived: %s", got)
	}
}

func TestLeavesCommentMarkersInsideStrings(t *testing.T) {
	// A URL is a comment marker in the wrong hands, and this is the case that
	// makes a naive stripper eat the rest of the file.
	for _, source := range []string{
		`{"$schema":"https://opencode.ai/config.json"}`,
		`{"a":"/* not a comment */","b":1}`,
		`{"a":"he said \"//\" here","b":2}`,
	} {
		if got := string(stripJSONComments([]byte(source))); got != source {
			t.Errorf("stripJSONComments(%s) = %s, want it unchanged", source, got)
		}
	}
}

func TestHandlesAnUnterminatedString(t *testing.T) {
	// Returned as-is rather than corrupted into something that parses: a malformed
	// file must fail the decode that follows, not be repaired into a wrong answer.
	source := `{"a":"unterminated`
	if got := string(stripJSONComments([]byte(source))); got != source {
		t.Fatalf("stripJSONComments(%s) = %s, want it unchanged", source, got)
	}
}

func TestStrippingPreservesEverythingElse(t *testing.T) {
	source := `{"a":1,"b":[2,3],"c":{"d":"e"}}`
	if got := string(stripJSONComments([]byte(source))); got != source {
		t.Fatalf("stripJSONComments(%s) = %s, want it unchanged", source, got)
	}
}

func contains(value, want string) bool {
	for index := 0; index+len(want) <= len(value); index++ {
		if value[index:index+len(want)] == want {
			return true
		}
	}
	return false
}
