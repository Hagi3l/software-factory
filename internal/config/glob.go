package config

import (
	"regexp"
	"strings"
)

// matchGlob reports whether a slash-separated path matches a glob pattern. It is the
// matcher behind Policy.TCBPaths, so the TCB-boundary globs (e.g. "internal/orchestrator/**")
// have one well-defined meaning shared by validation (which compiles each pattern to catch a
// malformed glob at startup) and the orchestrator (which tests a candidate's changed files
// against them).
//
// The wildcard semantics are the conventional ones: `**` matches any run of characters
// INCLUDING path separators (any number of path segments), while `*` and `?` match within a
// single segment only (`*` = any run of non-separator characters, `?` = exactly one). Every
// other character is matched literally. The match is anchored to the whole path.
func matchGlob(pattern, path string) bool {
	re, err := compileGlob(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(path)
}

// compileGlob translates a glob pattern into an anchored regexp. It is exported to
// validation through validateGlob so a malformed pattern (one that cannot compile) is a
// startup config error rather than a silently never-matching glob. The translation escapes
// regexp metacharacters in literal runs and maps `**` → `.*`, `*` → `[^/]*`, `?` → `[^/]`.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*") // `**` crosses path separators
				i += 2
				continue
			}
			b.WriteString("[^/]*") // `*` stays within a segment
			i++
		case '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// validateGlob reports whether pattern is a well-formed glob (it compiles). A non-empty
// pattern that cannot compile is a config fault caught at `harness validate` time.
func validateGlob(pattern string) bool {
	_, err := compileGlob(pattern)
	return err == nil
}
