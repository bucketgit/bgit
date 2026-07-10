package protocol

type Key struct {
	User      string `json:"user"`
	Role      string `json:"role"`
	PublicKey string `json:"public_key"`
	Source    string `json:"source,omitempty"`
	Suspended bool   `json:"suspended,omitempty"`
}
type OwnerRequest struct {
	User       string   `json:"user,omitempty"`
	Role       string   `json:"role,omitempty"`
	PublicKeys []string `json:"public_keys,omitempty"`
}

type RepositoryRequest struct {
	Repo       Repository `json:"repo"`
	AdminUser  string     `json:"admin_user,omitempty"`
	PublicKeys []string   `json:"public_keys,omitempty"`
	Role       string     `json:"role,omitempty"`
}

type KeyRequest struct {
	Repo       Repository `json:"repo"`
	User       string     `json:"user,omitempty"`
	Role       string     `json:"role,omitempty"`
	PublicKeys []string   `json:"public_keys,omitempty"`
	Key        string     `json:"key,omitempty"`
	Source     string     `json:"source,omitempty"`
}

type KeysResponse struct {
	Keys []Key `json:"keys"`
}

type RepositoryAdminRequest struct {
	Repo          Repository `json:"repo"`
	Description   string     `json:"description,omitempty"`
	DefaultBranch string     `json:"default_branch,omitempty"`
	Visibility    string     `json:"visibility,omitempty"`
	ReadOnly      *bool      `json:"read_only,omitempty"`
	IssuesEnabled *bool      `json:"issues_enabled,omitempty"`
	Logical       string     `json:"logical,omitempty"`
	TeamID        string     `json:"team_id,omitempty"`
	Name          string     `json:"name,omitempty"`
	UserID        string     `json:"user_id,omitempty"`
	User          string     `json:"user,omitempty"`
	Role          string     `json:"role,omitempty"`
	BrokerRole    string     `json:"broker_role,omitempty"`
	PublicKeys    []string   `json:"public_keys,omitempty"`
	Suspended     bool       `json:"suspended,omitempty"`
	BrokerURL     string     `json:"broker_url,omitempty"`
	Token         string     `json:"token,omitempty"`
}

type RepositoryInfo struct {
	Repo    Repository            `json:"repo"`
	Logical string                `json:"logical,omitempty"`
	Teams   []RepositoryTeamGrant `json:"teams,omitempty"`
}
type RepositoryListResponse struct {
	Repos []RepositoryInfo `json:"repos"`
}
type AdminRepositoryInfoResponse struct {
	Repo          Repository `json:"repo"`
	Description   string     `json:"description,omitempty"`
	DefaultBranch string     `json:"default_branch,omitempty"`
	Visibility    string     `json:"visibility,omitempty"`
	ReadOnly      bool       `json:"read_only,omitempty"`
	IssuesEnabled bool       `json:"issues_enabled,omitempty"`
}
type RepositoryTeamsResponse struct {
	Teams []RepositoryTeamGrant `json:"teams"`
}
type RepositoryUsersResponse struct {
	Users []RepositoryUserGrant `json:"users"`
}
type RepositoryUserGrant struct {
	UserID   string `json:"user_id,omitempty"`
	User     string `json:"user,omitempty"`
	Username string `json:"username,omitempty"`
	Role     string `json:"role,omitempty"`
}
type UsersResponse struct {
	Users []UserInfo `json:"users"`
}
type UserInfo struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	BrokerRole string `json:"broker_role"`
	Keys       []Key  `json:"keys,omitempty"`
	Suspended  bool   `json:"suspended,omitempty"`
	Pending    bool   `json:"pending,omitempty"`
}
type RepositoryInvitesResponse struct {
	Invites []RepositoryInvite `json:"invites"`
}
type RepositoryInvite struct {
	User      string `json:"user"`
	Role      string `json:"role"`
	ExpiresAt string `json:"expires_at"`
}
type TeamsResponse struct {
	Teams []Team `json:"teams"`
}
type Team struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Members []TeamMember `json:"members,omitempty"`
}
type TeamMember struct {
	UserID   string `json:"user_id,omitempty"`
	Username string `json:"username,omitempty"`
	Role     string `json:"role"`
}
type RepositoryTeamGrant struct {
	ID     string `json:"id,omitempty"`
	TeamID string `json:"team_id,omitempty"`
	Role   string `json:"role,omitempty"`
}

type OwnerTransferRequest struct {
	Repo      Repository `json:"repo"`
	User      string     `json:"user,omitempty"`
	Role      string     `json:"role,omitempty"`
	BrokerURL string     `json:"broker_url,omitempty"`
	Token     string     `json:"token,omitempty"`
}
type OwnerTransferResponse struct {
	Code          string `json:"code"`
	AcceptCommand string `json:"accept_command"`
	CancelCommand string `json:"cancel_command"`
	User          string `json:"user,omitempty"`
	Role          string `json:"role,omitempty"`
	Fingerprint   string `json:"fingerprint,omitempty"`
}
type Protection struct {
	Repo           Repository `json:"repo"`
	Ref            string     `json:"ref"`
	RequirePR      bool       `json:"require_pr"`
	AllowOverrides bool       `json:"allow_overrides"`
}

type PullRequest struct {
	ID        int               `json:"id,omitempty"`
	Title     string            `json:"title,omitempty"`
	Body      string            `json:"body,omitempty"`
	Source    string            `json:"source,omitempty"`
	Target    string            `json:"target,omitempty"`
	Status    string            `json:"status,omitempty"`
	Author    string            `json:"author,omitempty"`
	Version   string            `json:"version,omitempty"`
	UpdatedAt string            `json:"updated_at,omitempty"`
	Approvals int               `json:"approvals,omitempty"`
	Checks    []string          `json:"checks,omitempty"`
	Head      string            `json:"head,omitempty"`
	Comments  []PullRequestNote `json:"comments,omitempty"`
	Reviews   []PullRequestNote `json:"reviews,omitempty"`
	MergedBy  string            `json:"merged_by,omitempty"`
	MergedAt  string            `json:"merged_at,omitempty"`
	ClosedBy  string            `json:"closed_by,omitempty"`
	ClosedAt  string            `json:"closed_at,omitempty"`
}
type PullRequestNote struct {
	ID       int                  `json:"id,omitempty"`
	User     string               `json:"user,omitempty"`
	Body     string               `json:"body,omitempty"`
	State    string               `json:"state,omitempty"`
	Source   string               `json:"source,omitempty"`
	At       string               `json:"at,omitempty"`
	Comments []PullRequestComment `json:"comments,omitempty"`
	Replies  []PullRequestComment `json:"replies,omitempty"`
	Head     string               `json:"head,omitempty"`
}
type PullRequestComment struct {
	ID        int                  `json:"id,omitempty"`
	User      string               `json:"user,omitempty"`
	Body      string               `json:"body,omitempty"`
	File      string               `json:"file,omitempty"`
	Kind      string               `json:"kind,omitempty"`
	Side      string               `json:"side,omitempty"`
	Hunk      string               `json:"hunk,omitempty"`
	HunkIndex int                  `json:"hunk_index,omitempty"`
	OldStart  int                  `json:"old_start,omitempty"`
	NewStart  int                  `json:"new_start,omitempty"`
	Offset    int                  `json:"offset,omitempty"`
	Line      int                  `json:"line,omitempty"`
	LineText  string               `json:"line_text,omitempty"`
	LineHash  string               `json:"line_hash,omitempty"`
	Head      string               `json:"head,omitempty"`
	Outdated  bool                 `json:"outdated,omitempty"`
	At        string               `json:"at,omitempty"`
	Replies   []PullRequestComment `json:"replies,omitempty"`
}
type PullRequestRequest struct {
	Repo            Repository           `json:"repo"`
	ID              int                  `json:"id,omitempty"`
	PR              PullRequest          `json:"pr,omitempty"`
	Known           map[string]string    `json:"known,omitempty"`
	Merge           bool                 `json:"merge,omitempty"`
	DeleteBranch    bool                 `json:"delete_branch,omitempty"`
	Comment         string               `json:"comment,omitempty"`
	Review          string               `json:"review,omitempty"`
	Comments        []PullRequestComment `json:"comments,omitempty"`
	TargetNoteID    int                  `json:"target_note_id,omitempty"`
	TargetCommentID int                  `json:"target_comment_id,omitempty"`
}
type PullRequestsResponse struct {
	PRs     []PullRequest `json:"prs"`
	Deleted []int         `json:"deleted,omitempty"`
}
type PullRequestResponse struct {
	PR PullRequest `json:"pr"`
}

type CIRun struct {
	ID                int    `json:"id,omitempty"`
	Provider          string `json:"provider,omitempty"`
	Ref               string `json:"ref,omitempty"`
	Commit            string `json:"commit,omitempty"`
	Config            string `json:"config,omitempty"`
	Status            string `json:"status,omitempty"`
	Result            string `json:"result,omitempty"`
	URL               string `json:"url,omitempty"`
	Message           string `json:"message,omitempty"`
	ProviderBuildID   string `json:"provider_build_id,omitempty"`
	ProviderBuildName string `json:"provider_build_name,omitempty"`
	LogGroup          string `json:"log_group,omitempty"`
	LogStream         string `json:"log_stream,omitempty"`
	Author            string `json:"author,omitempty"`
	StartedAt         string `json:"started_at,omitempty"`
	FinishedAt        string `json:"finished_at,omitempty"`
	CreatedAt         string `json:"created_at,omitempty"`
	UpdatedAt         string `json:"updated_at,omitempty"`
}
type CIRequest struct {
	Repo     Repository `json:"repo"`
	ID       int        `json:"id,omitempty"`
	Provider string     `json:"provider,omitempty"`
	Ref      string     `json:"ref,omitempty"`
	Commit   string     `json:"commit,omitempty"`
	Config   string     `json:"config,omitempty"`
}
type CILogResponse struct {
	Run  CIRun  `json:"run"`
	Logs string `json:"logs"`
}

type Issue struct {
	ID        int          `json:"id,omitempty"`
	Type      string       `json:"type,omitempty"`
	Title     string       `json:"title,omitempty"`
	Body      string       `json:"body,omitempty"`
	Status    string       `json:"status,omitempty"`
	Lane      string       `json:"lane,omitempty"`
	Assignee  string       `json:"assignee,omitempty"`
	Position  float64      `json:"position,omitempty"`
	Archived  bool         `json:"archived,omitempty"`
	Author    string       `json:"author,omitempty"`
	CreatedAt string       `json:"created_at,omitempty"`
	UpdatedAt string       `json:"updated_at,omitempty"`
	Comments  []IssueReply `json:"comments,omitempty"`
	History   []IssueEvent `json:"history,omitempty"`
}
type IssueReply struct {
	User string `json:"user,omitempty"`
	Body string `json:"body,omitempty"`
	At   string `json:"at,omitempty"`
}
type IssueEvent struct {
	User     string `json:"user,omitempty"`
	Action   string `json:"action,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	At       string `json:"at,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Position string `json:"position,omitempty"`
}
type IssueRequest struct {
	Repo            Repository `json:"repo"`
	ID              int        `json:"id,omitempty"`
	Type            string     `json:"type,omitempty"`
	Title           string     `json:"title,omitempty"`
	Body            string     `json:"body,omitempty"`
	Lane            string     `json:"lane,omitempty"`
	Assignee        string     `json:"assignee,omitempty"`
	Comment         string     `json:"comment,omitempty"`
	AfterID         *int       `json:"after_id,omitempty"`
	Order           int        `json:"order,omitempty"`
	Archived        bool       `json:"archived,omitempty"`
	IncludeArchived bool       `json:"include_archived,omitempty"`
}

type AuthStatusRequest struct {
	Repo Repository `json:"repo"`
}
type Identity struct {
	User           string `json:"user,omitempty"`
	Source         string `json:"source,omitempty"`
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
	PublicKey      string `json:"public_key,omitempty"`
}
type AuthStatus struct {
	BrokerURL     string          `json:"broker_url,omitempty"`
	BrokerVersion string          `json:"broker_version,omitempty"`
	Repo          Repository      `json:"repo,omitempty"`
	Identity      Identity        `json:"identity,omitempty"`
	User          string          `json:"user,omitempty"`
	Role          string          `json:"role,omitempty"`
	Capabilities  map[string]bool `json:"capabilities,omitempty"`
	ResolvedAt    string          `json:"resolved_at,omitempty"`
	CachedAt      string          `json:"cached_at,omitempty"`
	Stale         bool            `json:"stale,omitempty"`
	Error         string          `json:"error,omitempty"`
}
type RepositoriesMineResponse struct {
	Repos []RepositoryMembership `json:"repos"`
}
type RepositoryMembership struct {
	RepoID         string     `json:"repo_id,omitempty"`
	Logical        string     `json:"logical,omitempty"`
	Repo           Repository `json:"repo,omitempty"`
	User           string     `json:"user,omitempty"`
	Role           string     `json:"role,omitempty"`
	Source         string     `json:"source,omitempty"`
	KeyFingerprint string     `json:"key_fingerprint,omitempty"`
	Suspended      bool       `json:"suspended,omitempty"`
	UpdatedAt      string     `json:"updated_at,omitempty"`
}

type RepositoryInfoRequest struct {
	Repo          Repository `json:"repo"`
	Description   string     `json:"description,omitempty"`
	DefaultBranch string     `json:"default_branch,omitempty"`
	Visibility    string     `json:"visibility,omitempty"`
	ReadOnly      bool       `json:"read_only,omitempty"`
	IssuesEnabled bool       `json:"issues_enabled"`
	Logical       string     `json:"logical,omitempty"`
	TeamID        string     `json:"team_id,omitempty"`
	Name          string     `json:"name,omitempty"`
	UserID        string     `json:"user_id,omitempty"`
	User          string     `json:"user,omitempty"`
	Role          string     `json:"role,omitempty"`
	BrokerRole    string     `json:"broker_role,omitempty"`
	PublicKeys    []string   `json:"public_keys,omitempty"`
}
type RepositoryInfoResponse struct {
	Repo          Repository `json:"repo"`
	Description   string     `json:"description"`
	DefaultBranch string     `json:"default_branch"`
	Visibility    string     `json:"visibility"`
	ReadOnly      bool       `json:"read_only"`
	IssuesEnabled bool       `json:"issues_enabled"`
}
type UserProfileRequest struct {
	Repo   Repository `json:"repo"`
	Bio    string     `json:"bio,omitempty"`
	Avatar string     `json:"avatar,omitempty"`
}
type UserProfileResponse struct {
	User    string           `json:"user"`
	Profile UserProfile      `json:"profile"`
	Keys    []UserProfileKey `json:"keys"`
}
type UserProfile struct {
	Bio    string `json:"bio,omitempty"`
	Avatar string `json:"avatar,omitempty"`
}
type UserProfileKey struct {
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Source      string `json:"source,omitempty"`
}

type Lane string

const (
	LaneBacklog Lane = "backlog"
	LaneReady   Lane = "ready"
	LaneDoing   Lane = "doing"
	LaneReview  Lane = "review"
	LaneDone    Lane = "done"
)

type PullRequestStatus string

const (
	PullRequestOpen   PullRequestStatus = "open"
	PullRequestMerged PullRequestStatus = "merged"
	PullRequestClosed PullRequestStatus = "closed"
)

type CIStatus string

const (
	CIQueued   CIStatus = "queued"
	CIBuilding CIStatus = "building"
	CIFinished CIStatus = "finished"
)
