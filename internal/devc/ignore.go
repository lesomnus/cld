package devc

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Ignored reports whether a container should be left alone,
// by the "cld.ignore" label or by config glob patterns matched
// against the host-side workspace path.
func Ignored(labels map[string]string, local_folder string, globs []string) bool {
	if v, ok := labels[LabelIgnore]; ok && v != "false" {
		return true
	}
	for _, g := range globs {
		if MatchPath(g, local_folder) {
			return true
		}
	}
	return false
}

// MatchPath matches a path against a glob pattern.
// "**" crosses path separators; "*" and "?" do not.
// A leading "~/" in the pattern expands to the home directory.
func MatchPath(pattern string, path string) bool {
	if strings.HasPrefix(pattern, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			pattern = filepath.Join(h, pattern[2:])
		}
	}

	if !strings.Contains(pattern, "**") {
		ok, err := filepath.Match(pattern, path)
		return err == nil && ok
	}

	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				// "**/" also matches zero directories.
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString(`(?:.*/)?`)
				} else {
					b.WriteString(`.*`)
				}
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")

	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(path)
}
