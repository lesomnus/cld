package devc

// StripJSONC makes a JSONC document parsable by encoding/json:
// it blanks out // and /* */ comments and removes trailing commas,
// leaving everything inside string literals untouched.
func StripJSONC(src []byte) []byte {
	out := make([]byte, 0, len(src))

	const (
		code = iota
		in_string
		in_line_comment
		in_block_comment
	)

	state := code
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case code:
			switch {
			case c == '"':
				state = in_string
				out = append(out, c)
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state = in_line_comment
				i++
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state = in_block_comment
				i++
			case c == ',':
				// Drop the comma if the next code character closes the scope.
				if is_trailing_comma(src[i+1:]) {
					continue
				}
				out = append(out, c)
			default:
				out = append(out, c)
			}
		case in_string:
			out = append(out, c)
			if c == '\\' && i+1 < len(src) {
				i++
				out = append(out, src[i])
			} else if c == '"' {
				state = code
			}
		case in_line_comment:
			if c == '\n' {
				state = code
				out = append(out, c)
			}
		case in_block_comment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state = code
				i++
			}
		}
	}
	return out
}

// is_trailing_comma reports whether the next meaningful character
// after a comma is "}" or "]", skipping whitespace and comments.
func is_trailing_comma(rest []byte) bool {
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			continue
		case c == '/' && i+1 < len(rest) && rest[i+1] == '/':
			for i++; i < len(rest) && rest[i] != '\n'; i++ {
			}
		case c == '/' && i+1 < len(rest) && rest[i+1] == '*':
			i += 2
			for ; i+1 < len(rest); i++ {
				if rest[i] == '*' && rest[i+1] == '/' {
					i++
					break
				}
			}
		default:
			return c == '}' || c == ']'
		}
	}
	return false
}
