package local

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/bucketgit/bgit/protocol"
	"github.com/bucketgit/bgit/store"
)

const issuesPath = ".bucketgit/broker-state/v1/issues.json"

type issueState struct {
	NextID int              `json:"next_id"`
	Issues []protocol.Issue `json:"issues"`
}

type IssueFilter struct {
	Type            string
	IncludeArchived bool
}
type StoryMove struct {
	Lane    string
	AfterID *int
	Order   int
}

func (b *Broker) ListIssues(ctx context.Context, repo protocol.Repository, filter IssueFilter) ([]protocol.Issue, error) {
	state, err := b.loadIssues(ctx, repo)
	if err != nil {
		return nil, err
	}
	issues := make([]protocol.Issue, 0, len(state.Issues))
	for _, issue := range state.Issues {
		if filter.Type != "" && issue.Type != filter.Type {
			continue
		}
		if issue.Archived && !filter.IncludeArchived {
			continue
		}
		issues = append(issues, issue)
	}
	sortIssues(issues)
	return issues, nil
}

func (b *Broker) GetIssue(ctx context.Context, repo protocol.Repository, id int) (protocol.Issue, error) {
	state, err := b.loadIssues(ctx, repo)
	if err != nil {
		return protocol.Issue{}, err
	}
	issue, err := findIssue(&state, id)
	if err != nil {
		return protocol.Issue{}, err
	}
	return *issue, nil
}

func (b *Broker) CreateIssue(ctx context.Context, repo protocol.Repository, request protocol.IssueRequest, user string) (protocol.Issue, error) {
	unlock := b.LockRepository(repo)
	defer unlock()
	state, expected, err := b.loadIssuesObserved(ctx, repo)
	if err != nil {
		return protocol.Issue{}, err
	}
	title, body := strings.TrimSpace(request.Title), strings.TrimSpace(request.Body)
	if title == "" {
		title = summary(body)
	}
	if title == "" {
		return protocol.Issue{}, errors.New("issue title is required")
	}
	if state.NextID <= 0 {
		state.NextID = nextIssueID(state.Issues)
	}
	issueType := first(request.Type, "issue")
	lane, position := strings.TrimSpace(request.Lane), 0.0
	if issueType == "story" {
		lane = normalizeLane(lane)
		position = nextPosition(state.Issues, lane)
	}
	now := b.now().UTC().Format("2006-01-02T15:04:05Z07:00")
	user = first(strings.TrimSpace(user), "owner")
	issue := protocol.Issue{ID: state.NextID, Type: issueType, Title: title, Body: body, Status: "open", Lane: lane, Assignee: strings.TrimSpace(request.Assignee), Position: position, Author: user, CreatedAt: now, UpdatedAt: now, History: []protocol.IssueEvent{{User: user, Action: "created", At: now}}}
	state.NextID++
	state.Issues = append(state.Issues, issue)
	if issueType == "story" {
		normalizeOrder(&state, lane, 0)
		issue, _ = b.issueFromState(&state, issue.ID)
	}
	return issue, b.saveIssues(ctx, repo, expected, state)
}

func (b *Broker) UpdateIssue(ctx context.Context, repo protocol.Repository, request protocol.IssueRequest, user string) error {
	return b.mutateIssue(ctx, repo, request.ID, func(state *issueState, issue *protocol.Issue) error {
		if request.Type != "" {
			issue.Type = request.Type
		}
		if strings.TrimSpace(request.Title) != "" {
			issue.Title = strings.TrimSpace(request.Title)
		}
		if request.Body != "" {
			issue.Body = strings.TrimSpace(request.Body)
		}
		if issue.Title == "" && issue.Type == "story" {
			issue.Title = summary(issue.Body)
		}
		b.event(issue, user, "edited", "", "", "")
		return nil
	})
}

func (b *Broker) CommentIssue(ctx context.Context, repo protocol.Repository, id int, comment, user string) error {
	comment = strings.TrimSpace(comment)
	if comment == "" {
		return errors.New("comment is required")
	}
	return b.mutateIssue(ctx, repo, id, func(_ *issueState, issue *protocol.Issue) error {
		now := b.now().UTC().Format("2006-01-02T15:04:05Z07:00")
		issue.Comments = append(issue.Comments, protocol.IssueReply{User: first(user, "owner"), Body: comment, At: now})
		b.event(issue, user, "commented", "", "", "")
		return nil
	})
}

func (b *Broker) SetIssueClosed(ctx context.Context, repo protocol.Repository, id int, closed bool, user string) error {
	return b.mutateIssue(ctx, repo, id, func(_ *issueState, issue *protocol.Issue) error {
		status, action := "open", "reopened"
		if closed {
			status, action = "closed", "closed"
		}
		if issue.Status != status {
			issue.Status = status
			b.event(issue, user, action, "", "", "")
		}
		return nil
	})
}

func (b *Broker) MoveStory(ctx context.Context, repo protocol.Repository, id int, move StoryMove, user string) error {
	return b.mutateIssue(ctx, repo, id, func(state *issueState, issue *protocol.Issue) error {
		lane, err := parseLane(first(move.Lane, issue.Lane))
		if err != nil {
			return err
		}
		from := normalizeLane(issue.Lane)
		applyOrder(state, id, lane, move.AfterID, move.Order)
		if from != lane {
			normalizeOrder(state, from, id)
		}
		issue, _ = findIssue(state, id)
		b.event(issue, user, "reordered", from, lane, formatPosition(issue.Position))
		return nil
	})
}

func (b *Broker) MoveStoryToLaneEnd(ctx context.Context, repo protocol.Repository, id int, lane, user string) error {
	return b.mutateIssue(ctx, repo, id, func(state *issueState, issue *protocol.Issue) error {
		target, err := parseLane(first(lane, issue.Lane))
		if err != nil {
			return err
		}
		from := normalizeLane(issue.Lane)
		issue.Lane = target
		issue.Position = nextPosition(state.Issues, target)
		if from != target {
			normalizeOrder(state, from, id)
		}
		normalizeOrder(state, target, 0)
		issue, _ = findIssue(state, id)
		b.event(issue, user, "moved", from, target, formatPosition(issue.Position))
		return nil
	})
}

func (b *Broker) TakeStory(ctx context.Context, repo protocol.Repository, id int, user string) error {
	return b.mutateIssue(ctx, repo, id, func(state *issueState, issue *protocol.Issue) error {
		fromAssignee, fromLane := issue.Assignee, normalizeLane(issue.Lane)
		issue.Assignee = first(strings.TrimSpace(user), "owner")
		if fromLane == "backlog" {
			applyOrder(state, id, "doing", nil, 0)
			normalizeOrder(state, fromLane, id)
			issue, _ = findIssue(state, id)
		} else if issue.Position == 0 {
			normalizeOrder(state, fromLane, 0)
			issue, _ = findIssue(state, id)
		}
		b.event(issue, user, "assigned", fromAssignee, issue.Assignee, "")
		return nil
	})
}

func (b *Broker) AssignIssue(ctx context.Context, repo protocol.Repository, id int, assignee, user string) error {
	return b.mutateIssue(ctx, repo, id, func(_ *issueState, issue *protocol.Issue) error {
		from := issue.Assignee
		issue.Assignee = strings.TrimSpace(assignee)
		b.event(issue, user, "assigned", from, issue.Assignee, "")
		return nil
	})
}
func (b *Broker) ArchiveIssue(ctx context.Context, repo protocol.Repository, id int, archived bool, user string) error {
	return b.mutateIssue(ctx, repo, id, func(_ *issueState, issue *protocol.Issue) error {
		issue.Archived = archived
		action := "unarchived"
		if archived {
			action = "archived"
		}
		b.event(issue, user, action, "", "", "")
		return nil
	})
}

func (b *Broker) mutateIssue(ctx context.Context, repo protocol.Repository, id int, change func(*issueState, *protocol.Issue) error) error {
	unlock := b.LockRepository(repo)
	defer unlock()
	state, expected, err := b.loadIssuesObserved(ctx, repo)
	if err != nil {
		return err
	}
	issue, err := findIssue(&state, id)
	if err != nil {
		return err
	}
	if err := change(&state, issue); err != nil {
		return err
	}
	return b.saveIssues(ctx, repo, expected, state)
}
func (b *Broker) loadIssues(ctx context.Context, repo protocol.Repository) (issueState, error) {
	state, _, err := b.loadIssuesObserved(ctx, repo)
	return state, err
}
func (b *Broker) loadIssuesObserved(ctx context.Context, repo protocol.Repository) (issueState, store.ObjectState, error) {
	var state issueState
	expected, err := b.LoadJSONState(ctx, repo, issuesPath, &state)
	if err != nil {
		return state, store.ObjectState{}, err
	}
	if !expected.Exists {
		return issueState{NextID: 1}, expected, nil
	}
	if state.NextID <= 0 {
		state.NextID = nextIssueID(state.Issues)
	}
	return state, expected, nil
}
func (b *Broker) saveIssues(ctx context.Context, repo protocol.Repository, expected store.ObjectState, state issueState) error {
	if state.NextID <= 0 {
		state.NextID = nextIssueID(state.Issues)
	}
	return b.CompareAndSwapJSON(ctx, repo, issuesPath, expected, state)
}
func (b *Broker) issueFromState(state *issueState, id int) (protocol.Issue, error) {
	issue, err := findIssue(state, id)
	if err != nil {
		return protocol.Issue{}, err
	}
	return *issue, nil
}
func (b *Broker) event(issue *protocol.Issue, user, action, from, to, position string) {
	now := b.now().UTC().Format("2006-01-02T15:04:05Z07:00")
	issue.UpdatedAt = now
	issue.History = append(issue.History, protocol.IssueEvent{User: first(user, "owner"), Action: action, From: from, To: to, Position: position, At: now})
}

func findIssue(state *issueState, id int) (*protocol.Issue, error) {
	if id <= 0 {
		return nil, errors.New("issue id is required")
	}
	for i := range state.Issues {
		if state.Issues[i].ID == id {
			return &state.Issues[i], nil
		}
	}
	return nil, errors.New("issue not found")
}
func nextIssueID(issues []protocol.Issue) int {
	next := 1
	for _, issue := range issues {
		if issue.ID >= next {
			next = issue.ID + 1
		}
	}
	return next
}
func nextPosition(issues []protocol.Issue, lane string) float64 {
	count := 0
	for _, issue := range issues {
		if issue.Type == "story" && !issue.Archived && normalizeLane(issue.Lane) == lane {
			count++
		}
	}
	return float64(count + 1)
}
func applyOrder(state *issueState, id int, lane string, after *int, order int) {
	target, err := findIssue(state, id)
	if err != nil {
		return
	}
	target.Lane = lane
	stories := laneStories(state, lane, id)
	index := len(stories)
	if order > 0 {
		index = order - 1
		if index < 0 {
			index = 0
		}
		if index > len(stories) {
			index = len(stories)
		}
	} else if after != nil {
		index = 0
		if *after > 0 {
			index = len(stories)
			for i, story := range stories {
				if story.ID == *after {
					index = i + 1
					break
				}
			}
		}
	}
	stories = append(stories, nil)
	copy(stories[index+1:], stories[index:])
	stories[index] = target
	for i, story := range stories {
		story.Lane = lane
		story.Position = float64(i + 1)
	}
}
func normalizeOrder(state *issueState, lane string, exclude int) {
	for i, story := range laneStories(state, lane, exclude) {
		story.Position = float64(i + 1)
	}
}
func laneStories(state *issueState, lane string, exclude int) []*protocol.Issue {
	var stories []*protocol.Issue
	for i := range state.Issues {
		issue := &state.Issues[i]
		if issue.ID == exclude || issue.Type != "story" || issue.Archived || normalizeLane(issue.Lane) != lane {
			continue
		}
		stories = append(stories, issue)
	}
	sort.SliceStable(stories, func(i, j int) bool {
		if stories[i].Position != stories[j].Position {
			if stories[i].Position == 0 {
				return false
			}
			if stories[j].Position == 0 {
				return true
			}
			return stories[i].Position < stories[j].Position
		}
		return stories[i].ID < stories[j].ID
	})
	return stories
}
func sortIssues(issues []protocol.Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		left, right := laneIndex(normalizeLane(issues[i].Lane)), laneIndex(normalizeLane(issues[j].Lane))
		if left != right {
			return left < right
		}
		if issues[i].Position != issues[j].Position {
			if issues[i].Position == 0 {
				return false
			}
			if issues[j].Position == 0 {
				return true
			}
			return issues[i].Position < issues[j].Position
		}
		return issues[i].ID < issues[j].ID
	})
}
func laneIndex(lane string) int {
	for i, value := range []string{"backlog", "ready", "doing", "review", "done"} {
		if value == lane {
			return i
		}
	}
	return 5
}
func normalizeLane(value string) string {
	lane, err := parseLane(value)
	if err != nil {
		return "backlog"
	}
	return lane
}
func parseLane(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "backlog":
		return "backlog", nil
	case "ready", "todo", "to-do":
		return "ready", nil
	case "doing", "in-progress", "in_progress", "progress":
		return "doing", nil
	case "review", "in-review", "in_review":
		return "review", nil
	case "done", "closed":
		return "done", nil
	default:
		return "", errors.New("unknown board lane")
	}
}
func summary(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= 80 {
		return value
	}
	return strings.TrimSpace(string(runes[:79])) + "..."
}
func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
func formatPosition(value float64) string {
	const digits = "0123456789"
	whole := int64(value)
	fraction := int64((value-float64(whole))*1_000_000 + 0.5)
	result := intString(whole) + "."
	divisor := int64(100000)
	for divisor > 0 {
		result += string(digits[(fraction/divisor)%10])
		divisor /= 10
	}
	return result
}
func intString(value int64) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}
