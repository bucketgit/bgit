package client

import (
	"context"
	"net/http"

	"github.com/bucketgit/bgit/protocol"
)

type Caller interface {
	PostJSON(ctx context.Context, endpoint string, request, response any, headers http.Header) error
}

type Endpoints struct{ caller Caller }

func NewEndpoints(caller Caller) *Endpoints { return &Endpoints{caller: caller} }

func (e *Endpoints) call(ctx context.Context, path string, request, response any) error {
	return e.caller.PostJSON(ctx, path, request, response, nil)
}

func (e *Endpoints) Authorize(ctx context.Context, request protocol.AuthRequest) (protocol.AuthResponse, error) {
	var response protocol.AuthResponse
	err := e.call(ctx, "/auth/check", request, &response)
	return response, err
}
func (e *Endpoints) AuthStatus(ctx context.Context, request protocol.AuthStatusRequest) (protocol.AuthStatus, error) {
	var response protocol.AuthStatus
	err := e.call(ctx, "/auth/status", request, &response)
	return response, err
}
func (e *Endpoints) ListRefs(ctx context.Context, repo protocol.Repository) (map[string]string, error) {
	var response protocol.RefsResponse
	err := e.call(ctx, "/refs/list", protocol.RefsRequest{Repo: repo}, &response)
	return response.Refs, err
}
func (e *Endpoints) UpdateRef(ctx context.Context, request protocol.RefUpdateRequest) error {
	return e.call(ctx, "/refs/update", request, nil)
}
func (e *Endpoints) ObjectCapability(ctx context.Context, request protocol.ObjectCapabilityRequest) (protocol.ObjectCapabilityResponse, error) {
	var response protocol.ObjectCapabilityResponse
	err := e.call(ctx, "/objects/capability", request, &response)
	return response, err
}

func (e *Endpoints) ListIssues(ctx context.Context, request protocol.IssueRequest) ([]protocol.Issue, error) {
	var response struct {
		Issues []protocol.Issue `json:"issues"`
	}
	err := e.call(ctx, "/issues/list", request, &response)
	return response.Issues, err
}
func (e *Endpoints) GetIssue(ctx context.Context, request protocol.IssueRequest) (protocol.Issue, error) {
	var response struct {
		Issue protocol.Issue `json:"issue"`
	}
	err := e.call(ctx, "/issues/view", request, &response)
	return response.Issue, err
}
func (e *Endpoints) CreateIssue(ctx context.Context, request protocol.IssueRequest) (protocol.Issue, error) {
	var response struct {
		Issue protocol.Issue `json:"issue"`
	}
	err := e.call(ctx, "/issues/create", request, &response)
	return response.Issue, err
}
func (e *Endpoints) UpdateIssue(ctx context.Context, request protocol.IssueRequest) error {
	return e.call(ctx, "/issues/update", request, nil)
}
func (e *Endpoints) CommentIssue(ctx context.Context, request protocol.IssueRequest) error {
	return e.call(ctx, "/issues/comment", request, nil)
}
func (e *Endpoints) CloseIssue(ctx context.Context, request protocol.IssueRequest) error {
	return e.call(ctx, "/issues/close", request, nil)
}
func (e *Endpoints) ReopenIssue(ctx context.Context, request protocol.IssueRequest) error {
	return e.call(ctx, "/issues/reopen", request, nil)
}
func (e *Endpoints) MoveIssue(ctx context.Context, request protocol.IssueRequest) error {
	return e.call(ctx, "/issues/move", request, nil)
}
func (e *Endpoints) ReorderIssue(ctx context.Context, request protocol.IssueRequest) error {
	return e.call(ctx, "/issues/reorder", request, nil)
}
func (e *Endpoints) TakeIssue(ctx context.Context, request protocol.IssueRequest) error {
	return e.call(ctx, "/issues/take", request, nil)
}
func (e *Endpoints) AssignIssue(ctx context.Context, request protocol.IssueRequest) error {
	return e.call(ctx, "/issues/assign", request, nil)
}
func (e *Endpoints) ArchiveIssue(ctx context.Context, request protocol.IssueRequest) error {
	return e.call(ctx, "/issues/archive", request, nil)
}
func (e *Endpoints) IssueAssignees(ctx context.Context, request protocol.IssueRequest) ([]string, error) {
	var response struct {
		Users []string `json:"users"`
	}
	err := e.call(ctx, "/issues/assignees", request, &response)
	return response.Users, err
}

func (e *Endpoints) ListPullRequests(ctx context.Context, request protocol.PullRequestRequest) ([]protocol.PullRequest, error) {
	var response protocol.PullRequestsResponse
	err := e.call(ctx, "/prs/list", request, &response)
	return response.PRs, err
}
func (e *Endpoints) GetPullRequest(ctx context.Context, request protocol.PullRequestRequest) (protocol.PullRequest, error) {
	var response protocol.PullRequestResponse
	err := e.call(ctx, "/prs/view", request, &response)
	return response.PR, err
}
func (e *Endpoints) CreatePullRequest(ctx context.Context, request protocol.PullRequestRequest) (protocol.PullRequest, error) {
	var response protocol.PullRequestResponse
	err := e.call(ctx, "/prs/create", request, &response)
	return response.PR, err
}
func (e *Endpoints) SyncPullRequests(ctx context.Context, request protocol.PullRequestRequest) (protocol.PullRequestsResponse, error) {
	var response protocol.PullRequestsResponse
	err := e.call(ctx, "/prs/sync", request, &response)
	return response, err
}
func (e *Endpoints) CommentPullRequest(ctx context.Context, request protocol.PullRequestRequest) (protocol.PullRequest, error) {
	return e.pullRequestMutation(ctx, "/prs/comment", request)
}
func (e *Endpoints) ReviewPullRequest(ctx context.Context, request protocol.PullRequestRequest) (protocol.PullRequest, error) {
	return e.pullRequestMutation(ctx, "/prs/review", request)
}
func (e *Endpoints) ReplyPullRequest(ctx context.Context, request protocol.PullRequestRequest) (protocol.PullRequest, error) {
	return e.pullRequestMutation(ctx, "/prs/reply", request)
}
func (e *Endpoints) MergePullRequest(ctx context.Context, request protocol.PullRequestRequest) (protocol.PullRequest, error) {
	return e.pullRequestMutation(ctx, "/prs/merge", request)
}
func (e *Endpoints) ClosePullRequest(ctx context.Context, request protocol.PullRequestRequest) (protocol.PullRequest, error) {
	return e.pullRequestMutation(ctx, "/prs/close", request)
}
func (e *Endpoints) ReopenPullRequest(ctx context.Context, request protocol.PullRequestRequest) (protocol.PullRequest, error) {
	return e.pullRequestMutation(ctx, "/prs/reopen", request)
}
func (e *Endpoints) pullRequestMutation(ctx context.Context, path string, request protocol.PullRequestRequest) (protocol.PullRequest, error) {
	var response protocol.PullRequestResponse
	err := e.call(ctx, path, request, &response)
	return response.PR, err
}

func (e *Endpoints) ListCIRuns(ctx context.Context, request protocol.CIRequest) ([]protocol.CIRun, error) {
	var response struct {
		Runs []protocol.CIRun `json:"runs"`
	}
	err := e.call(ctx, "/ci/list", request, &response)
	return response.Runs, err
}
func (e *Endpoints) GetCIRun(ctx context.Context, request protocol.CIRequest) (protocol.CIRun, error) {
	var response struct {
		Run protocol.CIRun `json:"run"`
	}
	err := e.call(ctx, "/ci/view", request, &response)
	return response.Run, err
}
func (e *Endpoints) RunCI(ctx context.Context, request protocol.CIRequest) (protocol.CIRun, error) {
	var response struct {
		Run protocol.CIRun `json:"run"`
	}
	err := e.call(ctx, "/ci/run", request, &response)
	return response.Run, err
}
func (e *Endpoints) CILogs(ctx context.Context, request protocol.CIRequest) (protocol.CILogResponse, error) {
	var response protocol.CILogResponse
	err := e.call(ctx, "/ci/logs", request, &response)
	return response, err
}
func (e *Endpoints) RotateCISecret(ctx context.Context, request protocol.CIRequest) error {
	return e.call(ctx, "/ci/secret/rotate", request, nil)
}

func (e *Endpoints) ListKeys(ctx context.Context, request protocol.KeyRequest) ([]protocol.Key, error) {
	var response protocol.KeysResponse
	err := e.call(ctx, "/keys/list", request, &response)
	return response.Keys, err
}
func (e *Endpoints) AddKey(ctx context.Context, request protocol.KeyRequest) error {
	return e.call(ctx, "/keys/add", request, nil)
}
func (e *Endpoints) RemoveKey(ctx context.Context, request protocol.KeyRequest) error {
	return e.call(ctx, "/keys/remove", request, nil)
}
func (e *Endpoints) SuspendKey(ctx context.Context, request protocol.KeyRequest, suspended bool) error {
	path := "/keys/unsuspend"
	if suspended {
		path = "/keys/suspend"
	}
	return e.call(ctx, path, request, nil)
}
func (e *Endpoints) CreateRepositoryInvite(ctx context.Context, request protocol.OwnerTransferRequest) (protocol.OwnerTransferResponse, error) {
	return e.ownerTransfer(ctx, "/keys/invite/create", request)
}
func (e *Endpoints) AcceptRepositoryInvite(ctx context.Context, request protocol.OwnerTransferRequest) (protocol.OwnerTransferResponse, error) {
	return e.ownerTransfer(ctx, "/keys/invite/accept", request)
}
func (e *Endpoints) CancelRepositoryInvite(ctx context.Context, request protocol.OwnerTransferRequest) error {
	return e.call(ctx, "/keys/invite/cancel", request, nil)
}
func (e *Endpoints) ListRepositoryInvites(ctx context.Context, request protocol.OwnerTransferRequest) ([]protocol.RepositoryInvite, error) {
	var response protocol.RepositoryInvitesResponse
	err := e.call(ctx, "/keys/invite/list", request, &response)
	return response.Invites, err
}

func (e *Endpoints) ListUsers(ctx context.Context, request protocol.RepositoryAdminRequest) ([]protocol.UserInfo, error) {
	var response protocol.UsersResponse
	err := e.call(ctx, "/broker/users/list", request, &response)
	return response.Users, err
}
func (e *Endpoints) UpsertUser(ctx context.Context, request protocol.RepositoryAdminRequest) (protocol.UserInfo, error) {
	var response struct {
		User protocol.UserInfo `json:"user"`
	}
	err := e.call(ctx, "/broker/users/upsert", request, &response)
	return response.User, err
}
func (e *Endpoints) DeleteUser(ctx context.Context, request protocol.RepositoryAdminRequest) error {
	return e.call(ctx, "/broker/users/delete", request, nil)
}
func (e *Endpoints) CreateBrokerUserInvite(ctx context.Context, request protocol.RepositoryAdminRequest) (protocol.OwnerTransferResponse, error) {
	var response protocol.OwnerTransferResponse
	err := e.call(ctx, "/broker/users/invite/create", request, &response)
	return response, err
}
func (e *Endpoints) AcceptBrokerUserInvite(ctx context.Context, request protocol.RepositoryAdminRequest) (protocol.OwnerTransferResponse, error) {
	var response protocol.OwnerTransferResponse
	err := e.call(ctx, "/broker/users/invite/accept", request, &response)
	return response, err
}
func (e *Endpoints) CancelBrokerUserInvite(ctx context.Context, request protocol.RepositoryAdminRequest) error {
	return e.call(ctx, "/broker/users/invite/cancel", request, nil)
}

func (e *Endpoints) ListTeams(ctx context.Context, request protocol.RepositoryAdminRequest) ([]protocol.Team, error) {
	var response protocol.TeamsResponse
	err := e.call(ctx, "/teams/list", request, &response)
	return response.Teams, err
}
func (e *Endpoints) ResolveTeam(ctx context.Context, request protocol.RepositoryAdminRequest) (protocol.Team, error) {
	var response struct {
		Team protocol.Team `json:"team"`
	}
	err := e.call(ctx, "/teams/resolve", request, &response)
	return response.Team, err
}
func (e *Endpoints) CreateTeam(ctx context.Context, request protocol.RepositoryAdminRequest) (protocol.Team, error) {
	var response struct {
		Team protocol.Team `json:"team"`
	}
	err := e.call(ctx, "/teams/create", request, &response)
	return response.Team, err
}
func (e *Endpoints) DeleteTeam(ctx context.Context, request protocol.RepositoryAdminRequest) error {
	return e.call(ctx, "/teams/delete", request, nil)
}
func (e *Endpoints) UpsertTeamMember(ctx context.Context, request protocol.RepositoryAdminRequest) error {
	return e.call(ctx, "/teams/member/upsert", request, nil)
}
func (e *Endpoints) RemoveTeamMember(ctx context.Context, request protocol.RepositoryAdminRequest) error {
	return e.call(ctx, "/teams/member/remove", request, nil)
}

func (e *Endpoints) ListRepositories(ctx context.Context, request protocol.RepositoryAdminRequest) ([]protocol.RepositoryInfo, error) {
	var response protocol.RepositoryListResponse
	err := e.call(ctx, "/repos/list", request, &response)
	return response.Repos, err
}
func (e *Endpoints) GetRepository(ctx context.Context, request protocol.RepositoryRequest) (protocol.RepositoryInfo, error) {
	var response protocol.RepositoryInfo
	err := e.call(ctx, "/repos/get", request, &response)
	return response, err
}
func (e *Endpoints) CreateRepository(ctx context.Context, request protocol.RepositoryAdminRequest) (protocol.RepositoryInfo, error) {
	var response protocol.RepositoryInfo
	err := e.call(ctx, "/repos/create", request, &response)
	return response, err
}
func (e *Endpoints) RepositoryInfo(ctx context.Context, request protocol.RepositoryAdminRequest) (protocol.AdminRepositoryInfoResponse, error) {
	var response protocol.AdminRepositoryInfoResponse
	err := e.call(ctx, "/repo/info", request, &response)
	return response, err
}
func (e *Endpoints) UpdateRepository(ctx context.Context, request protocol.RepositoryAdminRequest) error {
	return e.call(ctx, "/repo/update", request, nil)
}
func (e *Endpoints) RenameRepository(ctx context.Context, request protocol.RepositoryAdminRequest) error {
	return e.call(ctx, "/repo/rename", request, nil)
}
func (e *Endpoints) DeleteRepository(ctx context.Context, request protocol.RepositoryAdminRequest) error {
	return e.call(ctx, "/repo/delete", request, nil)
}
func (e *Endpoints) ReindexRepositoryMembers(ctx context.Context, request protocol.KeyRequest) error {
	return e.call(ctx, "/members/reindex", request, nil)
}
func (e *Endpoints) ListRepositoryTeams(ctx context.Context, request protocol.RepositoryAdminRequest) ([]protocol.RepositoryTeamGrant, error) {
	var response protocol.RepositoryTeamsResponse
	err := e.call(ctx, "/repo/teams/list", request, &response)
	return response.Teams, err
}
func (e *Endpoints) UpsertRepositoryTeam(ctx context.Context, request protocol.RepositoryAdminRequest) error {
	return e.call(ctx, "/repo/teams/upsert", request, nil)
}
func (e *Endpoints) RemoveRepositoryTeam(ctx context.Context, request protocol.RepositoryAdminRequest) error {
	return e.call(ctx, "/repo/teams/remove", request, nil)
}
func (e *Endpoints) ListRepositoryUsers(ctx context.Context, request protocol.RepositoryAdminRequest) ([]protocol.RepositoryUserGrant, error) {
	var response protocol.RepositoryUsersResponse
	err := e.call(ctx, "/repo/users/list", request, &response)
	return response.Users, err
}
func (e *Endpoints) UpsertRepositoryUser(ctx context.Context, request protocol.RepositoryAdminRequest) error {
	return e.call(ctx, "/repo/users/upsert", request, nil)
}
func (e *Endpoints) RemoveRepositoryUser(ctx context.Context, request protocol.RepositoryAdminRequest) error {
	return e.call(ctx, "/repo/users/remove", request, nil)
}

func (e *Endpoints) ConfirmOwnerTransfer(ctx context.Context, request protocol.OwnerTransferRequest) (protocol.OwnerTransferResponse, error) {
	return e.ownerTransfer(ctx, "/owners/transfer/confirm", request)
}
func (e *Endpoints) AcceptOwnerTransfer(ctx context.Context, request protocol.OwnerTransferRequest) (protocol.OwnerTransferResponse, error) {
	return e.ownerTransfer(ctx, "/owners/transfer/accept", request)
}
func (e *Endpoints) CancelOwnerTransfer(ctx context.Context, request protocol.OwnerTransferRequest) error {
	return e.call(ctx, "/owners/transfer/cancel", request, nil)
}
func (e *Endpoints) ownerTransfer(ctx context.Context, path string, request protocol.OwnerTransferRequest) (protocol.OwnerTransferResponse, error) {
	var response protocol.OwnerTransferResponse
	err := e.call(ctx, path, request, &response)
	return response, err
}

func (e *Endpoints) ListProtections(ctx context.Context, request protocol.Protection) ([]protocol.Protection, error) {
	var response struct {
		Protections []protocol.Protection `json:"protections"`
	}
	err := e.call(ctx, "/protection/list", request, &response)
	return response.Protections, err
}
func (e *Endpoints) UpsertProtection(ctx context.Context, request protocol.Protection) error {
	return e.call(ctx, "/protection/upsert", request, nil)
}
func (e *Endpoints) RemoveProtection(ctx context.Context, request protocol.Protection) error {
	return e.call(ctx, "/protection/remove", request, nil)
}
func (e *Endpoints) GetUserProfile(ctx context.Context, request protocol.UserProfileRequest) (protocol.UserProfileResponse, error) {
	var response protocol.UserProfileResponse
	err := e.call(ctx, "/profile/get", request, &response)
	return response, err
}
func (e *Endpoints) UpdateUserProfile(ctx context.Context, request protocol.UserProfileRequest) (protocol.UserProfileResponse, error) {
	var response protocol.UserProfileResponse
	err := e.call(ctx, "/profile/update", request, &response)
	return response, err
}

func (c *Client) endpoints() *Endpoints { return NewEndpoints(c) }
func (c *Client) Authorize(ctx context.Context, request protocol.AuthRequest) (protocol.AuthResponse, error) {
	return c.endpoints().Authorize(ctx, request)
}
func (c *Client) AuthStatus(ctx context.Context, request protocol.AuthStatusRequest) (protocol.AuthStatus, error) {
	return c.endpoints().AuthStatus(ctx, request)
}
func (c *Client) ListRefs(ctx context.Context, repo protocol.Repository) (map[string]string, error) {
	return c.endpoints().ListRefs(ctx, repo)
}
func (c *Client) UpdateRef(ctx context.Context, request protocol.RefUpdateRequest) error {
	return c.endpoints().UpdateRef(ctx, request)
}
func (c *Client) ObjectCapability(ctx context.Context, request protocol.ObjectCapabilityRequest) (protocol.ObjectCapabilityResponse, error) {
	return c.endpoints().ObjectCapability(ctx, request)
}
func (c *Client) ListIssues(ctx context.Context, request protocol.IssueRequest) ([]protocol.Issue, error) {
	return c.endpoints().ListIssues(ctx, request)
}
func (c *Client) ListPullRequests(ctx context.Context, repo protocol.Repository) ([]protocol.PullRequest, error) {
	return c.endpoints().ListPullRequests(ctx, protocol.PullRequestRequest{Repo: repo})
}
func (c *Client) ListCIRuns(ctx context.Context, repo protocol.Repository) ([]protocol.CIRun, error) {
	return c.endpoints().ListCIRuns(ctx, protocol.CIRequest{Repo: repo})
}
