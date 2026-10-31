package main

import "testing"

func TestReadOnly(t *testing.T) {
	for name, want := range map[string]bool{
		"bazel-review-fleet":  true,
		"/BAZEL-HISTORY-PR":   true,
		"bazel-post-report":   false,
		"BAZEL-POST-REPORT":   false,
		"/bazel-post-report":  false,
		" /Bazel-Post-Report": false,
		"review-fleet":        false,
	} {
		if _, ok := readOnly(name); ok != want {
			t.Errorf("readOnly(%q) = %v, want %v", name, ok, want)
		}
	}
}
