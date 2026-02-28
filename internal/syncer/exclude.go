package syncer

import (
	"path/filepath"
	"strings"
)

func hasSegment(rel, seg string) bool {
	if seg == "" {
		return false
	}
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if p == seg {
			return true
		}
	}
	return false
}

func isExcluded(rel string, patterns []string) bool {
	rel = filepath.ToSlash(strings.TrimPrefix(rel, "./"))
	if rel == "" || rel == "." {
		return false
	}
	for _, p := range patterns {
		p = filepath.ToSlash(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if ok, _ := filepath.Match(p, rel); ok {
			return true
		}

		// prefix style: foo/* => foo and all descendants
		if strings.HasSuffix(p, "/*") {
			prefix := strings.TrimSuffix(p, "/*")
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				return true
			}
			// compatibility: ".git/*" and "node_modules/*" should match at any depth
			if !strings.Contains(prefix, "/") {
				if hasSegment(rel, prefix) {
					return true
				}
			}
		}
	}
	return false
}
