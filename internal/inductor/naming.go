// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"fmt"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"regexp"
	"strings"
)

var nonSlug = regexp.MustCompile(`[^A-Za-z0-9]+`)
var nonFold = regexp.MustCompile(`[^a-z0-9]+`)

func casefold(s string) string { return cases.Fold().String(s) }
func Fold(s string) string     { return nonFold.ReplaceAllString(casefold(s), "") }
func Slug(s string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(s) {
		if r < 128 && r != '\'' && r != '`' {
			b.WriteRune(r)
		}
	}
	s = strings.Trim(strings.ToLower(nonSlug.ReplaceAllString(b.String(), "-")), "-")
	if s == "" {
		return "untitled"
	}
	return s
}
func ItemID(author, stem string) string { return author + "-" + Slug(stem) }
func Unique(name string, taken map[string]bool) (string, error) {
	if !taken[name] {
		taken[name] = true
		return name, nil
	}
	for n := 0; n < 1000; n++ {
		s := fmt.Sprintf("%s-%d", name, n)
		if !taken[s] {
			taken[s] = true
			return s, nil
		}
	}
	return "", fmt.Errorf("could not find a free name for %q", name)
}
