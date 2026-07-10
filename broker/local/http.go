package local

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/bucketgit/bgit/protocol"
)

func (b *Broker) Handler() http.Handler { return http.HandlerFunc(b.serveHTTP) }

func (b *Broker) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(response, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, 8<<20))
	decoder.DisallowUnknownFields()
	switch request.URL.Path {
	case "/auth/check":
		var value protocol.AuthRequest
		if err := decoder.Decode(&value); err != nil {
			writeError(response, http.StatusBadRequest, "invalid JSON")
			return
		}
		result, err := b.Authorize(request.Context(), value)
		writeResult(response, result, err)
		return
	case "/refs/list":
		var value protocol.RefsRequest
		if !decodeRequest(response, decoder, &value) {
			return
		}
		if !b.requireHTTPAccess(response, request, value.Repo, "read") {
			return
		}
		state, err := b.ReconcileRepository(request.Context(), value.Repo, "http-sync")
		writeResult(response, protocol.RefsResponse{Refs: state.Refs}, err)
		return
	case "/refs/update":
		var value protocol.RefUpdateRequest
		if !decodeRequest(response, decoder, &value) {
			return
		}
		identity, ok := b.httpIdentity(response, request, value.Repo, "write")
		if !ok {
			return
		}
		err := b.UpdateRef(request.Context(), value, identity.User)
		writeResult(response, map[string]bool{"ok": err == nil}, err)
		return
	case "/objects/read", "/objects/list":
		var value protocol.ObjectRequest
		if !decodeRequest(response, decoder, &value) {
			return
		}
		if !b.requireHTTPAccess(response, request, value.Repo, "read") {
			return
		}
		if request.URL.Path == "/objects/read" {
			data, err := b.ReadObject(request.Context(), value.Repo, value.Path)
			writeResult(response, protocol.ObjectResponse{Data: base64.StdEncoding.EncodeToString(data)}, err)
		} else {
			paths, err := b.ListObjects(request.Context(), value.Repo, value.Prefix)
			writeResult(response, protocol.ObjectResponse{Paths: paths}, err)
		}
		return
	default:
		if strings.HasPrefix(request.URL.Path, "/issues/") {
			b.serveIssuesHTTP(response, request, decoder)
			return
		}
		writeError(response, http.StatusNotFound, "unknown broker endpoint")
	}
}

func (b *Broker) serveIssuesHTTP(response http.ResponseWriter, request *http.Request, decoder *json.Decoder) {
	var value protocol.IssueRequest
	if !decodeRequest(response, decoder, &value) {
		return
	}
	operation := "write"
	if request.URL.Path == "/issues/list" || request.URL.Path == "/issues/view" {
		operation = "read"
	}
	identity, ok := b.httpIdentity(response, request, value.Repo, operation)
	if !ok {
		return
	}
	var result any = map[string]bool{"ok": true}
	var err error
	switch request.URL.Path {
	case "/issues/list":
		var issues []protocol.Issue
		issues, err = b.ListIssues(request.Context(), value.Repo, IssueFilter{Type: value.Type, IncludeArchived: value.IncludeArchived})
		result = map[string]any{"issues": issues}
	case "/issues/view":
		var issue protocol.Issue
		issue, err = b.GetIssue(request.Context(), value.Repo, value.ID)
		result = map[string]any{"issue": issue}
	case "/issues/create":
		var issue protocol.Issue
		issue, err = b.CreateIssue(request.Context(), value.Repo, value, identity.User)
		result = map[string]any{"issue": issue}
	case "/issues/update":
		err = b.UpdateIssue(request.Context(), value.Repo, value, identity.User)
	case "/issues/comment":
		err = b.CommentIssue(request.Context(), value.Repo, value.ID, value.Comment, identity.User)
	case "/issues/close":
		err = b.SetIssueClosed(request.Context(), value.Repo, value.ID, true, identity.User)
	case "/issues/reopen":
		err = b.SetIssueClosed(request.Context(), value.Repo, value.ID, false, identity.User)
	case "/issues/reorder":
		err = b.MoveStory(request.Context(), value.Repo, value.ID, StoryMove{Lane: value.Lane, AfterID: value.AfterID, Order: value.Order}, identity.User)
	case "/issues/move":
		if value.AfterID != nil {
			err = b.MoveStory(request.Context(), value.Repo, value.ID, StoryMove{Lane: value.Lane, AfterID: value.AfterID}, identity.User)
		} else {
			err = b.MoveStoryToLaneEnd(request.Context(), value.Repo, value.ID, value.Lane, identity.User)
		}
	case "/issues/take":
		err = b.TakeStory(request.Context(), value.Repo, value.ID, identity.User)
	case "/issues/assign":
		err = b.AssignIssue(request.Context(), value.Repo, value.ID, value.Assignee, identity.User)
	case "/issues/archive":
		err = b.ArchiveIssue(request.Context(), value.Repo, value.ID, value.Archived, identity.User)
	default:
		writeError(response, http.StatusNotFound, "unknown broker endpoint")
		return
	}
	writeResult(response, result, err)
}

func (b *Broker) requireHTTPAccess(response http.ResponseWriter, request *http.Request, repo protocol.Repository, operation string) bool {
	_, ok := b.httpIdentity(response, request, repo, operation)
	return ok
}
func (b *Broker) httpIdentity(response http.ResponseWriter, request *http.Request, repo protocol.Repository, operation string) (protocol.Identity, bool) {
	authorized, err := b.Authorize(request.Context(), protocol.AuthRequest{Repo: repo, Operation: operation})
	if err != nil || !authorized.Allowed {
		writeError(response, http.StatusForbidden, operation+" access denied")
		return protocol.Identity{}, false
	}
	return protocol.Identity{User: authorized.User}, true
}
func decodeRequest(response http.ResponseWriter, decoder *json.Decoder, target any) bool {
	if err := decoder.Decode(target); err != nil {
		writeError(response, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}
func writeResult(response http.ResponseWriter, value any, err error) {
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, fs.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(response, status, err.Error())
		return
	}
	response.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(response).Encode(value)
}
func writeError(response http.ResponseWriter, status int, message string) {
	response.Header().Set("content-type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]string{"error": message})
}
