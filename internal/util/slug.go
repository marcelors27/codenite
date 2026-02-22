package util

import (
	"fmt"
	"regexp"
	"strings"
)

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func Slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "task"
	}
	return s
}

func BranchName(taskID, title string) string {
	return fmt.Sprintf("ai/todoist-%s-%s", Slug(taskID), Slug(title))
}
