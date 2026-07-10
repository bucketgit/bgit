package web

import "testing"

func TestMatchRoutePrecedenceAndValues(t *testing.T) {
	tests := []struct {
		path  string
		route Route
		value string
	}{
		{"/", RouteHome, ""},
		{"/prs/new", RouteNewPR, ""},
		{"/prs/42", RoutePR, "42"},
		{"/api/commit/abc", RouteAPICommit, "abc"},
		{"/raw/docs/readme.md", RouteRaw, "docs/readme.md"},
		{"/missing", RouteUnknown, ""},
	}
	for _, test := range tests {
		match := MatchRoute(test.path)
		if match.Route != test.route || match.Value != test.value {
			t.Fatalf("MatchRoute(%q)=%#v want route=%q value=%q", test.path, match, test.route, test.value)
		}
	}
}
