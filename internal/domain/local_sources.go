package domain

import (
	"fmt"
	"path"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// LocalSourceSelection reads both the original single-directory field and
// its replacement. Older run snapshots retain their root checkout layout.
func LocalSourceSelection(single string, multiple []string) []string {
	if len(multiple) > 0 {
		return multiple
	}
	if single != "" {
		return []string{single}
	}
	return nil
}

// LocalSourceDirectories maps selected directories to the paths users and
// scripts see in the run. Multiple inputs share one Git baseline at the root.
func LocalSourceDirectories(sources []string) []string {
	switch len(sources) {
	case 0:
		return nil
	case 1:
		return []string{"."}
	}
	directories := make([]string, 0, len(sources))
	used := make(map[string]bool, len(sources))
	key := func(name string) string { return strings.ToLower(norm.NFC.String(name)) }
	for _, source := range sources {
		base := strings.Map(func(ch rune) rune {
			if unicode.IsControl(ch) || ch == '\\' {
				return '_'
			}
			return ch
		}, path.Base(source))
		name := base
		for suffix := 2; used[key(name)]; suffix++ {
			stem := base
			if len(stem) > 220 {
				stem = "directory"
			}
			name = fmt.Sprintf("%s-%d", stem, suffix)
		}
		used[key(name)] = true
		directories = append(directories, path.Join("sources", name))
	}
	return directories
}
