package inventory

// stripJSONComments removes // line comments and /* block */ comments that are not
// inside a string, leaving every other byte where it was.
//
// It exists because opencode's configuration file is JSONC by name: a strict
// encoding/json decode of a commented file fails, and a failed decode here reports
// "no servers", which is a silent wrong answer rather than a visible one. The file
// measured on the machine this was written for happens to parse as strict JSON —
// its only "//" are inside https:// URLs — which is exactly why a comment-unaware
// reader would look correct here and fail on somebody else's machine.
//
// Bytes inside a string are never touched, which is the whole difficulty: a URL is
// a comment marker in the wrong hands. An unterminated string leaves the rest of
// the input alone rather than swallowing it, so a malformed file fails the decode
// that follows instead of being silently truncated into a valid one.
func stripJSONComments(source []byte) []byte {
	stripped := make([]byte, 0, len(source))
	for index := 0; index < len(source); {
		switch {
		case source[index] == '"':
			end := endOfString(source, index)
			stripped = append(stripped, source[index:end]...)
			index = end
		case source[index] == '/' && index+1 < len(source) && source[index+1] == '/':
			for index < len(source) && source[index] != '\n' {
				index++
			}
		case source[index] == '/' && index+1 < len(source) && source[index+1] == '*':
			index += 2
			for index < len(source) && (source[index] != '*' || index+1 >= len(source) || source[index+1] != '/') {
				index++
			}
			// An unterminated block comment runs to the end, which is what a JSON
			// parser would make of it anyway; index lands past the input either way.
			index = min(index+2, len(source))
		default:
			stripped = append(stripped, source[index])
			index++
		}
	}
	return stripped
}

// endOfString returns the offset just past the string literal starting at start,
// or the end of the input when the string is never closed.
func endOfString(source []byte, start int) int {
	for index := start + 1; index < len(source); index++ {
		switch source[index] {
		case '\\':
			// An escape consumes the next byte whatever it is, which is what keeps
			// a \" from reading as the end of the string.
			index++
		case '"':
			return index + 1
		}
	}
	return len(source)
}
