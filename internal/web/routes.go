package web

import "strings"

type Route string

const (
	RouteUnknown             Route = ""
	RouteHome                Route = "home"
	RouteLogo                Route = "logo"
	RouteFavicon             Route = "favicon"
	RouteEvents              Route = "events"
	RouteAPIState            Route = "api-state"
	RouteAPIMe               Route = "api-me"
	RouteAPICommitAction     Route = "api-action-commit"
	RouteAPIStageAction      Route = "api-action-stage"
	RouteAPIUnstageAction    Route = "api-action-unstage"
	RouteAPIDiscardAction    Route = "api-action-discard"
	RouteAPIUncommitAction   Route = "api-action-uncommit"
	RouteAPIPushAction       Route = "api-action-push"
	RouteAPIPullAction       Route = "api-action-pull"
	RouteAPIPRAction         Route = "api-action-pr"
	RouteAPIIssueAction      Route = "api-action-issues"
	RouteAPIBoardAction      Route = "api-action-board"
	RouteAPICIAction         Route = "api-action-ci"
	RouteAPISettingsAction   Route = "api-action-settings"
	RouteAPIUserProfile      Route = "api-user-profile"
	RouteAPIDiff             Route = "api-diff"
	RouteAPIRefs             Route = "api-refs"
	RouteAPITree             Route = "api-tree"
	RouteAPICommits          Route = "api-commits"
	RouteAPIPRs              Route = "api-prs"
	RouteAPIIssues           Route = "api-issues"
	RouteAPISettings         Route = "api-settings"
	RouteAPISettingsFragment Route = "api-settings-fragment"
	RouteAPIBlob             Route = "api-blob"
	RouteAPICommit           Route = "api-commit"
	RouteCommits             Route = "commits"
	RoutePRs                 Route = "prs"
	RouteNewPR               Route = "new-pr"
	RoutePR                  Route = "pr"
	RouteIssues              Route = "issues"
	RouteIssue               Route = "issue"
	RouteBoard               Route = "board"
	RouteStory               Route = "story"
	RouteCI                  Route = "ci"
	RouteSettings            Route = "settings"
	RouteUserSettings        Route = "user-settings"
	RouteAdmin               Route = "admin"
	RouteArchive             Route = "archive"
	RouteCommit              Route = "commit"
	RouteTree                Route = "tree"
	RouteBlob                Route = "blob"
	RouteRaw                 Route = "raw"
)

type Match struct {
	Route Route
	Value string
}

func MatchRoute(path string) Match {
	route := strings.TrimPrefix(path, "/")
	exact := map[string]Route{
		"": RouteHome, "assets/bgit-mark.png": RouteLogo, "favicon.ico": RouteFavicon,
		"events": RouteEvents, "api/state": RouteAPIState, "api/me": RouteAPIMe,
		"api/actions/commit": RouteAPICommitAction, "api/actions/stage": RouteAPIStageAction,
		"api/actions/unstage": RouteAPIUnstageAction, "api/actions/discard": RouteAPIDiscardAction,
		"api/actions/uncommit": RouteAPIUncommitAction, "api/actions/push": RouteAPIPushAction,
		"api/actions/pull": RouteAPIPullAction, "api/actions/pr": RouteAPIPRAction,
		"api/actions/issues": RouteAPIIssueAction, "api/actions/board": RouteAPIBoardAction,
		"api/actions/ci": RouteAPICIAction, "api/actions/settings": RouteAPISettingsAction,
		"api/user/profile": RouteAPIUserProfile, "api/diff": RouteAPIDiff, "api/refs": RouteAPIRefs,
		"api/tree": RouteAPITree, "api/commits": RouteAPICommits, "api/prs": RouteAPIPRs,
		"api/issues": RouteAPIIssues, "api/settings": RouteAPISettings,
		"api/settings-fragment": RouteAPISettingsFragment, "api/blob": RouteAPIBlob,
		"commits": RouteCommits, "prs": RoutePRs, "prs/new": RouteNewPR, "issues": RouteIssues,
		"board": RouteBoard, "ci": RouteCI, "settings": RouteSettings,
		"user/settings": RouteUserSettings, "user/settings/": RouteUserSettings,
		"admin": RouteAdmin, "archive.zip": RouteArchive,
	}
	if matched, ok := exact[route]; ok {
		return Match{Route: matched}
	}
	for _, prefix := range []struct {
		Value string
		Route Route
	}{
		{"api/commit/", RouteAPICommit}, {"prs/", RoutePR}, {"issues/", RouteIssue},
		{"board/", RouteStory}, {"commit/", RouteCommit}, {"tree/", RouteTree},
		{"blob/", RouteBlob}, {"raw/", RouteRaw},
	} {
		if strings.HasPrefix(route, prefix.Value) {
			return Match{Route: prefix.Route, Value: strings.TrimPrefix(route, prefix.Value)}
		}
	}
	return Match{}
}
