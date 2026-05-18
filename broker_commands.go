package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type brokerProfile struct {
	Provider      string
	Name          string
	Region        string
	QualifiedName string
	BrokerURL     string
}

func brokerAdminCommand(cfg config, args []string, stdout io.Writer) error {
	return brokerAdminCommandWithInput(cfg, args, os.Stdin, stdout)
}

func brokerAdminCommandWithInput(cfg config, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: bgit admin keys|owner|protect|members [args]\n\nCloud IAM administration moved to bgit direct admin.")
	}
	switch args[0] {
	case "keys":
		return brokerAdminKeysCommand(cfg, args[1:], stdin, stdout)
	case "repo":
		return errors.New("repo registration happens during bgit init; use bgit admin keys for user/key administration")
	case "owner":
		return brokerOwnerCommand(cfg, args[1:], stdout)
	case "protect":
		return brokerProtectionCommand(cfg, args[1:], stdout)
	case "members":
		return brokerMembersCommand(cfg, args[1:], stdout)
	case "grant-read", "grant-write", "grant-admin", "make-public", "make-private":
		return errors.New("cloud IAM administration moved to bgit direct admin")
	default:
		return fmt.Errorf("unknown admin command %q", args[0])
	}
}

func brokerMembersCommand(cfg config, args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] != "reindex" {
		return errors.New("usage: bgit admin members reindex")
	}
	return janitorMembersReindex(cfg, stdout)
}

func janitorCommand(cfg config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: bgit janitor members reindex")
	}
	switch args[0] {
	case "members":
		if len(args) == 2 && args[1] == "reindex" {
			return janitorMembersReindex(cfg, stdout)
		}
		return errors.New("usage: bgit janitor members reindex")
	default:
		return fmt.Errorf("unknown janitor command %q", args[0])
	}
}

func janitorMembersReindex(cfg config, stdout io.Writer) error {
	brokerURL := strings.TrimSpace(cfg.brokerURL)
	if brokerURL == "" {
		var err error
		brokerURL, err = brokerURLForCommand(sshSetupOptions{})
		if err != nil {
			return err
		}
	}
	if err := brokerPost(brokerURL, "/members/reindex", brokerKeyRequest{Repo: repoForBroker(cfg)}, nil); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "reindexed broker membership")
	return nil
}

func brokerInitCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	opts, repoName, err := parseBrokerInitArgs(args)
	if err != nil {
		return err
	}
	if !opts.noninteractive {
		opts.interactive = true
	}
	if opts.noninteractive {
		if strings.TrimSpace(opts.profile) == "" {
			return errors.New("init --noninteractive requires --profile PROFILE")
		}
		if strings.TrimSpace(repoName) == "" {
			return errors.New("init --noninteractive requires --repo NAME")
		}
	}
	global, path, err := loadGlobalConfigForInit(opts.configPath)
	if err != nil {
		return err
	}
	profiles := brokerProfilesFromGlobalConfig(global)
	if len(profiles) == 0 && opts.interactive {
		fmt.Fprint(stdout, "No broker profiles found. Run bgit setup now? [y/N] ")
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes") {
			cmd := exec.Command(os.Args[0], "setup", "--config", path)
			cmd.Stdin = stdin
			cmd.Stdout = stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("run bgit setup: %w", err)
			}
			global, path, err = loadGlobalConfigForInit(opts.configPath)
			if err != nil {
				return err
			}
			profiles = brokerProfilesFromGlobalConfig(global)
		}
	}
	if len(profiles) == 0 {
		return errors.New("no broker profiles configured; run bgit setup first")
	}
	target := "."
	if opts.directory != "" {
		target = opts.directory
	}
	identityName := ""
	identityEmail := ""
	if opts.interactive {
		initial := initDialogInitialState(target, global, repoName, opts.profile)
		identityName = initial.IdentityName
		identityEmail = initial.IdentityEmail
		result, err := brokerInitPrompt(stdin, stdout, initial, profiles)
		if err != nil {
			return err
		}
		if !result.Changed {
			fmt.Fprintln(stdout, "No changes made to the repository configuration.")
			return nil
		}
		if result.IdentityChanged && !result.RepoChanged && !result.ProfileChanged {
			if err := writeLocalIdentityConfig(target, result.IdentityName, result.IdentityEmail); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "Updated repository identity.")
			return nil
		}
		repoName = result.RepoName
		opts.profile = result.ProfileName
		identityName = result.IdentityName
		identityEmail = result.IdentityEmail
	}
	if strings.TrimSpace(repoName) == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		repoName = filepath.Base(wd) + ".git"
	}
	profile, err := selectBrokerProfileForCommand(profiles, opts.profile, opts.region, "bgit init")
	if err != nil {
		return err
	}
	if identityName == "" && identityEmail == "" {
		identity := initDialogInitialState(target, global, repoName, opts.profile)
		identityName = identity.IdentityName
		identityEmail = identity.IdentityEmail
	}
	return initBrokerWorktree(target, repoName, profile, identityName, identityEmail, stdout)
}

func brokerCloneCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	opts, repoName, err := parseBrokerInitArgs(args)
	if err != nil {
		return err
	}
	if opts.brokerURL == "" {
		brokerURL, parsedRepo, ok, err := parseBrokerCloneURL(repoName)
		if err != nil {
			return err
		}
		if ok {
			opts.brokerURL = brokerURL
			repoName = parsedRepo
		}
	}
	if strings.TrimSpace(repoName) == "" {
		return errors.New("usage: bgit clone <repo> [directory] [--profile PROFILE]\n       bgit clone https://broker.example.com/team/app.git [directory]\n       bgit clone --broker https://broker.example.com team/app.git [directory]")
	}
	if opts.brokerURL != "" {
		profile, err := brokerProfileForCloneURL(opts.brokerURL)
		if err != nil {
			return err
		}
		return brokerCloneWithProfile(opts, repoName, profile, stdout)
	}
	global, _, err := loadGlobalConfigForInit(opts.configPath)
	if err != nil {
		return err
	}
	profiles := brokerProfilesFromGlobalConfig(global)
	if len(profiles) == 0 {
		return errors.New("no broker profiles configured; run bgit setup first")
	}
	if opts.interactive {
		result, err := brokerInitPrompt(stdin, stdout, initDialogInitialState(".", global, repoName, opts.profile), profiles)
		if err != nil {
			return err
		}
		repoName = result.RepoName
		opts.profile = result.ProfileName
	}
	profile, err := selectBrokerProfileForCommand(profiles, opts.profile, opts.region, "bgit clone "+repoName)
	if err != nil {
		return err
	}
	return brokerCloneWithProfile(opts, repoName, profile, stdout)
}

func brokerCloneWithProfile(opts brokerInitOptions, repoName string, profile brokerProfile, stdout io.Writer) error {
	target := opts.directory
	if target == "" {
		target = strings.TrimSuffix(filepath.Base(strings.Trim(repoName, "/")), ".git")
	}
	if err := initBrokerWorktree(target, repoName, profile, "", "", io.Discard); err != nil {
		return err
	}
	if _, err := runGit(target, "fetch", "origin"); err != nil {
		return err
	}
	if _, err := runGit(target, "checkout", "--quiet", "-B", defaultBranch, "origin/"+defaultBranch); err != nil {
		_, _ = runGit(target, "checkout", "--quiet", "-B", defaultBranch)
	}
	fmt.Fprintf(stdout, "Cloned %s into '%s'\n", repoName, target)
	return nil
}

func brokerAdminKeysCommand(cfg config, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: bgit admin keys list|add|remove|suspend|import-github [args]")
	}
	if args[0] != "import-github" {
		return sshKeysCommand(cfg, args, stdout)
	}
	opts, err := parseImportGitHubKeysArgs(args[1:])
	if err != nil {
		return err
	}
	cfg, err = configForBrokerCommand(cfg)
	if err != nil {
		return err
	}
	brokerURL, err := brokerURLForCommand(sshSetupOptions{broker: opts.broker})
	if err != nil {
		return err
	}
	keys, err := fetchGitHubPublicKeys(context.Background(), opts.username)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("github user %s has no public SSH keys", opts.username)
	}
	if !opts.yes {
		fmt.Fprintf(stdout, "Import %d key(s) from github:%s as %s? [y/N] ", len(keys), opts.username, opts.role)
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
			return errors.New("import cancelled")
		}
	}
	if err := brokerAddKeysWithSource(brokerURL, cfg, opts.username, opts.role, "github:"+opts.username, keys); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "imported %d key(s) from github:%s with role %s\n", len(keys), opts.username, opts.role)
	return nil
}

type importGitHubKeysOptions struct {
	username string
	role     string
	broker   string
	yes      bool
}

func parseImportGitHubKeysArgs(args []string) (importGitHubKeysOptions, error) {
	opts := importGitHubKeysOptions{role: "read"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--role":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, err
			}
			opts.role = normalizeBrokerRole(value)
		case "--broker":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, err
			}
			opts.broker = value
		case "--yes", "-y":
			opts.yes = true
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unsupported import-github option %s", arg)
			}
			if opts.username != "" {
				return opts, errors.New("import-github accepts exactly one username")
			}
			opts.username = strings.TrimPrefix(strings.TrimSpace(arg), "@")
		}
	}
	if opts.username == "" {
		return opts, errors.New("usage: bgit admin keys import-github <username> [--role ROLE] [--yes]")
	}
	if !validBrokerRole(opts.role) || opts.role == "owner" {
		return opts, fmt.Errorf("invalid import role %q", opts.role)
	}
	return opts, nil
}

func fetchGitHubPublicKeys(ctx context.Context, username string) ([]string, error) {
	endpoint := "https://github.com/" + username + ".keys"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch %s: %s", endpoint, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return splitPublicKeyLines(string(data)), nil
}

func configForBrokerCommand(base config) (config, error) {
	cfg := base
	if localCfg, err := readLocalConfig("."); err == nil {
		cfg = mergeConfig(cfg, localCfg)
	}
	if strings.TrimSpace(cfg.brokerURL) == "" {
		if out, err := runGit(".", "config", "--get", "bucketgit.broker"); err == nil {
			cfg.brokerURL = strings.TrimSpace(string(out))
		}
	}
	if strings.TrimSpace(cfg.brokerURL) == "" {
		return config{}, errors.New("broker URL is required; run bgit setup/init first")
	}
	if strings.TrimSpace(cfg.logicalRepo) == "" {
		if out, err := runGit(".", "config", "--get", "bucketgit.logicalRepo"); err == nil {
			cfg.logicalRepo = strings.Trim(strings.TrimSpace(string(out)), "/")
		}
	}
	if cfg.origin == "" {
		cfg.origin = originForConfig(cfg)
	}
	return cfg, nil
}

type brokerOwnerTransferRequest struct {
	Repo brokerRepo `json:"repo"`
	User string     `json:"user"`
	Key  string     `json:"key"`
}

func brokerOwnerCommand(cfg config, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "transfer" {
		return errors.New("usage: bgit admin owner transfer --user USER KEY_OR_FINGERPRINT")
	}
	cfg, err := configForBrokerCommand(cfg)
	if err != nil {
		return err
	}
	user := ""
	key := ""
	for i := 1; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--user":
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return err
			}
			user = value
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unsupported owner transfer option %s", arg)
			}
			if key != "" {
				return errors.New("owner transfer accepts exactly one key")
			}
			key = arg
		}
	}
	if user == "" || key == "" {
		return errors.New("usage: bgit admin owner transfer --user USER KEY_OR_FINGERPRINT")
	}
	if err := brokerPost(cfg.brokerURL, "/owners/transfer", brokerOwnerTransferRequest{Repo: repoForBroker(cfg), User: user, Key: key}, nil); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "transferred owner role to %s\n", user)
	return nil
}

type brokerProtectionRequest struct {
	Repo           brokerRepo `json:"repo"`
	Ref            string     `json:"ref"`
	RequirePR      bool       `json:"require_pr"`
	AllowOverrides bool       `json:"allow_overrides"`
}

func brokerProtectionCommand(cfg config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: bgit admin protect add|list|remove [ref]")
	}
	cfg, err := configForBrokerCommand(cfg)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		var resp struct {
			Protections []brokerProtectionRequest `json:"protections"`
		}
		if err := brokerPost(cfg.brokerURL, "/protection/list", brokerProtectionRequest{Repo: repoForBroker(cfg)}, &resp); err != nil {
			return err
		}
		for _, protection := range resp.Protections {
			mode := "pr-required"
			if protection.AllowOverrides {
				mode += ",owner-admin-override"
			}
			fmt.Fprintf(stdout, "%s\t%s\n", protection.Ref, mode)
		}
		return nil
	case "add":
		ref := "refs/heads/main"
		allowOverrides := false
		for _, arg := range args[1:] {
			switch arg {
			case "--allow-owner-admin-override":
				allowOverrides = true
			default:
				if strings.HasPrefix(arg, "-") {
					return fmt.Errorf("unsupported protect option %s", arg)
				}
				ref = normalizeDestinationRef(arg)
			}
		}
		req := brokerProtectionRequest{Repo: repoForBroker(cfg), Ref: ref, RequirePR: true, AllowOverrides: allowOverrides}
		if err := brokerPost(cfg.brokerURL, "/protection/upsert", req, nil); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "protected %s\n", ref)
		return nil
	case "remove":
		if len(args) != 2 {
			return errors.New("usage: bgit admin protect remove <ref>")
		}
		req := brokerProtectionRequest{Repo: repoForBroker(cfg), Ref: normalizeDestinationRef(args[1])}
		if err := brokerPost(cfg.brokerURL, "/protection/remove", req, nil); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed protection for %s\n", req.Ref)
		return nil
	default:
		return fmt.Errorf("unknown protect command %q", args[0])
	}
}

type brokerPullRequest struct {
	ID        int                     `json:"id,omitempty"`
	Title     string                  `json:"title,omitempty"`
	Body      string                  `json:"body,omitempty"`
	Source    string                  `json:"source,omitempty"`
	Target    string                  `json:"target,omitempty"`
	Status    string                  `json:"status,omitempty"`
	Author    string                  `json:"author,omitempty"`
	Version   string                  `json:"version,omitempty"`
	UpdatedAt string                  `json:"updated_at,omitempty"`
	Approvals int                     `json:"approvals,omitempty"`
	Checks    []string                `json:"checks,omitempty"`
	Head      string                  `json:"head,omitempty"`
	Comments  []brokerPullRequestNote `json:"comments,omitempty"`
	Reviews   []brokerPullRequestNote `json:"reviews,omitempty"`
	MergedBy  string                  `json:"merged_by,omitempty"`
	MergedAt  string                  `json:"merged_at,omitempty"`
	ClosedBy  string                  `json:"closed_by,omitempty"`
	ClosedAt  string                  `json:"closed_at,omitempty"`
}

type brokerPullRequestNote struct {
	ID       int                        `json:"id,omitempty"`
	User     string                     `json:"user,omitempty"`
	Body     string                     `json:"body,omitempty"`
	State    string                     `json:"state,omitempty"`
	Source   string                     `json:"source,omitempty"`
	At       string                     `json:"at,omitempty"`
	Comments []brokerPullRequestComment `json:"comments,omitempty"`
	Replies  []brokerPullRequestComment `json:"replies,omitempty"`
	Head     string                     `json:"head,omitempty"`
}

type brokerPullRequestComment struct {
	ID        int                        `json:"id,omitempty"`
	User      string                     `json:"user,omitempty"`
	Body      string                     `json:"body,omitempty"`
	File      string                     `json:"file,omitempty"`
	Kind      string                     `json:"kind,omitempty"`
	Side      string                     `json:"side,omitempty"`
	Hunk      string                     `json:"hunk,omitempty"`
	HunkIndex int                        `json:"hunk_index,omitempty"`
	OldStart  int                        `json:"old_start,omitempty"`
	NewStart  int                        `json:"new_start,omitempty"`
	Offset    int                        `json:"offset,omitempty"`
	Line      int                        `json:"line,omitempty"`
	LineText  string                     `json:"line_text,omitempty"`
	LineHash  string                     `json:"line_hash,omitempty"`
	Head      string                     `json:"head,omitempty"`
	Outdated  bool                       `json:"outdated,omitempty"`
	At        string                     `json:"at,omitempty"`
	Replies   []brokerPullRequestComment `json:"replies,omitempty"`
}

type brokerPullRequestRequest struct {
	Repo            brokerRepo                 `json:"repo"`
	ID              int                        `json:"id,omitempty"`
	PR              brokerPullRequest          `json:"pr,omitempty"`
	Known           map[string]string          `json:"known,omitempty"`
	Merge           bool                       `json:"merge,omitempty"`
	DeleteBranch    bool                       `json:"delete_branch,omitempty"`
	Comment         string                     `json:"comment,omitempty"`
	Review          string                     `json:"review,omitempty"`
	Comments        []brokerPullRequestComment `json:"comments,omitempty"`
	TargetNoteID    int                        `json:"target_note_id,omitempty"`
	TargetCommentID int                        `json:"target_comment_id,omitempty"`
}

func prCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	_ = stdin
	if len(args) == 0 {
		return errors.New("usage: bgit pr create|list|view|checkout|diff|merge|close|reopen [args]")
	}
	cfg, err := configForBrokerCommand(config{})
	if err != nil {
		return err
	}
	switch args[0] {
	case "create":
		return prCreateCommand(cfg, args[1:], stdout)
	case "list":
		var resp struct {
			PRs []brokerPullRequest `json:"prs"`
		}
		if err := brokerPost(cfg.brokerURL, "/prs/list", brokerPullRequestRequest{Repo: repoForBroker(cfg)}, &resp); err != nil {
			return err
		}
		for _, pr := range resp.PRs {
			fmt.Fprintf(stdout, "#%d\t%s\t%s -> %s\t%s\n", pr.ID, pr.Status, pr.Source, pr.Target, pr.Title)
		}
		return nil
	case "view":
		pr, err := brokerGetPullRequest(cfg, args[1:])
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "#%d %s\nstatus: %s\nsource: %s\ntarget: %s\napprovals: %d\n", pr.ID, pr.Title, pr.Status, pr.Source, pr.Target, pr.Approvals)
		if strings.TrimSpace(pr.Body) != "" {
			fmt.Fprintf(stdout, "\n%s\n", pr.Body)
		}
		return nil
	case "close":
		id, err := parsePRIDArg(args[1:])
		if err != nil {
			return err
		}
		if err := brokerPost(cfg.brokerURL, "/prs/close", brokerPullRequestRequest{Repo: repoForBroker(cfg), ID: id}, nil); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "closed PR #%d\n", id)
		return nil
	case "reopen":
		id, err := parsePRIDArg(args[1:])
		if err != nil {
			return err
		}
		if err := brokerPost(cfg.brokerURL, "/prs/reopen", brokerPullRequestRequest{Repo: repoForBroker(cfg), ID: id}, nil); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "reopened PR #%d\n", id)
		return nil
	case "merge":
		id, err := parsePRIDArg(args[1:])
		if err != nil {
			return err
		}
		if err := brokerPost(cfg.brokerURL, "/prs/merge", brokerPullRequestRequest{Repo: repoForBroker(cfg), ID: id, Merge: true}, nil); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "merged PR #%d\n", id)
		return nil
	case "checkout":
		pr, err := brokerGetPullRequest(cfg, args[1:])
		if err != nil {
			return err
		}
		if _, err := runGit(".", "fetch", "origin", pr.Source+":"+pr.Source); err != nil {
			return err
		}
		_, err = runGit(".", "checkout", shortRefName(pr.Source))
		return err
	case "diff":
		pr, err := brokerGetPullRequest(cfg, args[1:])
		if err != nil {
			return err
		}
		source := shortRefName(pr.Source)
		target := shortRefName(pr.Target)
		if _, err := runGit(".", "fetch", "origin", pr.Source+":refs/remotes/origin/"+source, pr.Target+":refs/remotes/origin/"+target); err != nil {
			return err
		}
		out, err := runGit(".", "diff", "refs/remotes/origin/"+target+"..."+"refs/remotes/origin/"+source)
		if err != nil {
			return err
		}
		_, err = stdout.Write(out)
		return err
	default:
		return fmt.Errorf("unknown pr command %q", args[0])
	}
}

func prCreateCommand(cfg config, args []string, stdout io.Writer) error {
	pr := brokerPullRequest{Target: "refs/heads/main"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--title":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return err
			}
			pr.Title = value
		case "--body":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return err
			}
			pr.Body = value
		case "--source":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return err
			}
			pr.Source = normalizeDestinationRef(value)
		case "--target":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return err
			}
			pr.Target = normalizeDestinationRef(value)
		default:
			return fmt.Errorf("unsupported pr create option %s", arg)
		}
	}
	if pr.Source == "" {
		out, err := runGit(".", "branch", "--show-current")
		if err != nil {
			return err
		}
		pr.Source = branchRef(strings.TrimSpace(string(out)))
	}
	if pr.Title == "" {
		pr.Title = shortRefName(pr.Source) + " into " + shortRefName(pr.Target)
	}
	var resp struct {
		PR brokerPullRequest `json:"pr"`
	}
	if err := brokerPost(cfg.brokerURL, "/prs/create", brokerPullRequestRequest{Repo: repoForBroker(cfg), PR: pr}, &resp); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "created PR #%d %s\n", resp.PR.ID, resp.PR.Title)
	return nil
}

func brokerGetPullRequest(cfg config, args []string) (brokerPullRequest, error) {
	id, err := parsePRIDArg(args)
	if err != nil {
		return brokerPullRequest{}, err
	}
	var resp struct {
		PR brokerPullRequest `json:"pr"`
	}
	if err := brokerPost(cfg.brokerURL, "/prs/view", brokerPullRequestRequest{Repo: repoForBroker(cfg), ID: id}, &resp); err != nil {
		return brokerPullRequest{}, err
	}
	return resp.PR, nil
}

func parsePRIDArg(args []string) (int, error) {
	if len(args) != 1 {
		return 0, errors.New("PR command requires exactly one PR id")
	}
	id := parsePositiveInt(strings.TrimPrefix(args[0], "#"))
	if id <= 0 {
		return 0, fmt.Errorf("invalid PR id %q", args[0])
	}
	return id, nil
}

type brokerInitOptions struct {
	interactive    bool
	noninteractive bool
	profile        string
	region         string
	repo           string
	brokerURL      string
	configPath     string
	directory      string
}

func parseBrokerInitArgs(args []string) (brokerInitOptions, string, error) {
	var opts brokerInitOptions
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--interactive":
			opts.interactive = true
		case "--noninteractive":
			opts.noninteractive = true
		case "--profile":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, "", err
			}
			opts.profile = value
		case "--region":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, "", err
			}
			opts.region = value
		case "--repo":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, "", err
			}
			opts.repo = value
		case "--broker":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, "", err
			}
			opts.brokerURL = value
		case "--config":
			var err error
			value, i, err = optionValue(args, i, hasValue, value, name)
			if err != nil {
				return opts, "", err
			}
			opts.configPath = expandHome(value)
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, "", fmt.Errorf("unsupported init option %s", arg)
			}
			rest = append(rest, arg)
		}
	}
	if opts.interactive && opts.noninteractive {
		return opts, "", errors.New("init accepts either --interactive or --noninteractive, not both")
	}
	if opts.repo != "" {
		switch len(rest) {
		case 0:
			return opts, opts.repo, nil
		case 1:
			opts.directory = rest[0]
			return opts, opts.repo, nil
		default:
			return opts, "", errors.New("init accepts at most one directory when --repo is set")
		}
	}
	switch len(rest) {
	case 0:
		return opts, opts.repo, nil
	case 1:
		return opts, firstNonEmpty(opts.repo, rest[0]), nil
	case 2:
		opts.directory = rest[1]
		return opts, firstNonEmpty(opts.repo, rest[0]), nil
	default:
		return opts, "", errors.New("init accepts at most repository name and optional directory")
	}
}

func parseBrokerCloneURL(raw string) (string, string, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || (!strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://")) {
		return "", "", false, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", true, fmt.Errorf("parse broker clone URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", true, fmt.Errorf("unsupported broker clone URL scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", "", true, errors.New("broker clone URL must include a host")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", true, errors.New("broker clone URL must not include query parameters or a fragment")
	}
	repoName := strings.Trim(parsed.Path, "/")
	if repoName == "" {
		return "", "", true, errors.New("broker clone URL must include a logical repository path")
	}
	return parsed.Scheme + "://" + parsed.Host, repoName, true, nil
}

func brokerProfileForCloneURL(brokerURL string) (brokerProfile, error) {
	brokerURL = strings.TrimRight(strings.TrimSpace(brokerURL), "/")
	if brokerURL == "" {
		return brokerProfile{}, errors.New("--broker requires a broker URL")
	}
	parsed, err := url.Parse(brokerURL)
	if err != nil {
		return brokerProfile{}, fmt.Errorf("parse broker URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return brokerProfile{}, errors.New("--broker must be an http(s) URL")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return brokerProfile{}, errors.New("--broker must not include query parameters or a fragment")
	}
	return brokerProfile{
		Provider:      "gcs",
		Name:          parsed.Host,
		QualifiedName: "broker:" + brokerURL,
		BrokerURL:     brokerURL,
	}, nil
}

func loadGlobalConfigForInit(path string) (globalConfig, string, error) {
	var err error
	if path == "" {
		path, err = defaultGlobalConfigPath()
		if err != nil {
			return globalConfig{}, "", err
		}
	}
	cfg, err := readGlobalConfig(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return globalConfig{Version: globalConfigVersion}, path, nil
		}
		return globalConfig{}, path, err
	}
	return cfg, path, nil
}

func brokerProfilesFromGlobalConfig(cfg globalConfig) []brokerProfile {
	var profiles []brokerProfile
	for _, profile := range cfg.GCPProfiles {
		for _, region := range profile.Regions {
			if strings.TrimSpace(region.BrokerURL) == "" {
				continue
			}
			name := "gcp:" + profile.Name + "/" + region.Name
			profiles = append(profiles, brokerProfile{Provider: "gcs", Name: profile.Name, Region: region.Name, QualifiedName: name, BrokerURL: region.BrokerURL})
		}
	}
	for _, profile := range cfg.AWSProfiles {
		for _, region := range profile.Regions {
			if strings.TrimSpace(region.BrokerURL) == "" {
				continue
			}
			name := "aws:" + profile.Name + "/" + region.Name
			profiles = append(profiles, brokerProfile{Provider: "s3", Name: profile.Name, Region: region.Name, QualifiedName: name, BrokerURL: region.BrokerURL})
		}
	}
	return profiles
}

func selectBrokerProfile(profiles []brokerProfile, name string) (brokerProfile, error) {
	return selectBrokerProfileForCommand(profiles, name, "", "bgit")
}

func selectBrokerProfileForCommand(profiles []brokerProfile, name, region, command string) (brokerProfile, error) {
	if strings.TrimSpace(name) == "" {
		if len(profiles) == 1 {
			return profiles[0], nil
		}
		return brokerProfile{}, errors.New("multiple broker profiles configured; pass --profile")
	}
	name = strings.TrimSpace(name)
	region = strings.TrimSpace(region)
	var matches []brokerProfile
	for _, profile := range profiles {
		if region != "" && profile.Region != region {
			continue
		}
		if brokerProfileNameMatches(profile, name) {
			matches = append(matches, profile)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return brokerProfile{}, ambiguousBrokerProfileError(name, command, matches)
	}
	return brokerProfile{}, fmt.Errorf("broker profile %q not found", name)
}

func brokerProfileNameMatches(profile brokerProfile, name string) bool {
	providerName := providerProfileName(profile.Provider)
	candidates := []string{
		profile.QualifiedName,
		profile.Name,
		profile.Name + "." + profile.Region,
		providerName + ":" + profile.Name,
		providerName + ":" + profile.Name + "." + profile.Region,
		providerName + ":" + profile.Name + "/" + profile.Region,
	}
	for _, candidate := range candidates {
		if name == candidate {
			return true
		}
	}
	return false
}

func ambiguousBrokerProfileError(name, command string, matches []brokerProfile) error {
	var b strings.Builder
	fmt.Fprintf(&b, "broker profile %q is ambiguous.\nSpecify the region you want to use:\n", name)
	for _, profile := range matches {
		fmt.Fprintf(&b, "  %s --profile %s.%s\n", command, profile.Name, profile.Region)
	}
	return errors.New(strings.TrimRight(b.String(), "\n"))
}

func providerProfileName(provider string) string {
	if provider == "s3" {
		return "aws"
	}
	return "gcp"
}

type initDialogConfig struct {
	RepoName      string
	ProfileName   string
	IdentityName  string
	IdentityEmail string
	Existing      bool
}

type initDialogResult struct {
	RepoName        string
	ProfileName     string
	IdentityName    string
	IdentityEmail   string
	Changed         bool
	RepoChanged     bool
	ProfileChanged  bool
	IdentityChanged bool
}

func brokerInitPrompt(stdin io.Reader, stdout io.Writer, initial initDialogConfig, profiles []brokerProfile) (initDialogResult, error) {
	reader, ok := stdin.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(stdin)
	}
	return runInitDialogWithRaw(reader, stdin, stdout, initial, profiles)
}

type initDialogState struct {
	repoName             string
	initialRepoName      string
	profileName          string
	initialProfileName   string
	identityName         string
	initialIdentityName  string
	identityEmail        string
	initialIdentityEmail string
	existing             bool
	profiles             []brokerProfile
	selectedProfile      int
	initialProfile       int
	cursor               int
	button               int
	editingField         int
	editOriginal         string
	message              string
}

func runInitDialogWithRaw(reader *bufio.Reader, rawInput io.Reader, stdout io.Writer, initial initDialogConfig, profiles []brokerProfile) (initDialogResult, error) {
	rawMode, restore, err := setupDialogRawMode(rawInput)
	if err != nil {
		return initDialogResult{}, err
	}
	defer restore()
	selectedProfile := initDialogSelectedProfile(profiles, initial.ProfileName)
	state := initDialogState{
		repoName:             firstNonEmpty(strings.TrimSpace(initial.RepoName), defaultInitRepoName()),
		initialRepoName:      firstNonEmpty(strings.TrimSpace(initial.RepoName), defaultInitRepoName()),
		profileName:          strings.TrimSpace(initial.ProfileName),
		initialProfileName:   strings.TrimSpace(initial.ProfileName),
		identityName:         firstNonEmpty(strings.TrimSpace(initial.IdentityName), defaultBucketGitIdentityName),
		initialIdentityName:  firstNonEmpty(strings.TrimSpace(initial.IdentityName), defaultBucketGitIdentityName),
		identityEmail:        firstNonEmpty(strings.TrimSpace(initial.IdentityEmail), defaultBucketGitIdentityEmail()),
		initialIdentityEmail: firstNonEmpty(strings.TrimSpace(initial.IdentityEmail), defaultBucketGitIdentityEmail()),
		existing:             initial.Existing,
		profiles:             profiles,
		selectedProfile:      selectedProfile,
		initialProfile:       selectedProfile,
		button:               -1,
		editingField:         -1,
	}
	for {
		fmt.Fprint(stdout, renderInitDialogFrame(state, rawMode))
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return initDialogResult{}, errors.New("init canceled")
			}
			return initDialogResult{}, err
		}
		switch b {
		case 0x03:
			return initDialogResult{}, errors.New("init canceled")
		case 0x04:
			if state.editingField >= 0 {
				state.editingField = -1
				state.editOriginal = ""
				continue
			}
			if result, ok := state.deploy(); ok {
				return result, nil
			}
		case '\r', '\n':
			if state.editingField >= 0 {
				state.editingField = -1
				state.editOriginal = ""
				state.message = ""
				continue
			}
			if result, ok := state.activate(); ok {
				return result, nil
			} else if state.button == 1 {
				return initDialogResult{}, errors.New("init canceled")
			}
		case ' ':
			if state.editingField >= 0 {
				state.appendFieldByte(b)
			} else if result, ok := state.activate(); ok {
				return result, nil
			}
		case '\t':
			if state.editingField >= 0 {
				state.editingField = -1
				state.editOriginal = ""
			}
			state.tab()
		case 0x7f, 0x08:
			if state.editingField >= 0 {
				state.backspaceField()
			}
		case 0x1b:
			if state.editingField >= 0 {
				state.setFieldValue(state.editingField, state.editOriginal)
				state.editingField = -1
				state.editOriginal = ""
				state.message = ""
				continue
			}
			next, err := reader.ReadByte()
			if err != nil {
				return initDialogResult{}, errors.New("init canceled")
			}
			if next == '[' {
				last, err := reader.ReadByte()
				if err != nil {
					return initDialogResult{}, errors.New("init canceled")
				}
				switch last {
				case 'A':
					state.up()
				case 'B':
					state.down()
				}
				continue
			}
			return initDialogResult{}, errors.New("init canceled")
		default:
			if state.editingField >= 0 && b >= 32 && b <= 126 {
				state.appendFieldByte(b)
			}
		}
	}
}

func defaultInitRepoName() string {
	wd, err := os.Getwd()
	if err != nil {
		return "repo.git"
	}
	name := strings.TrimSpace(filepath.Base(wd))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "repo.git"
	}
	if !strings.HasSuffix(name, ".git") {
		name += ".git"
	}
	return name
}

func initDialogInitialState(target string, global globalConfig, repoName, profileName string) initDialogConfig {
	initial := initDialogConfig{
		RepoName:      firstNonEmpty(strings.TrimSpace(repoName), defaultInitRepoName()),
		ProfileName:   strings.TrimSpace(profileName),
		IdentityName:  firstNonEmpty(strings.TrimSpace(global.Identity.Name), defaultBucketGitIdentityName),
		IdentityEmail: firstNonEmpty(strings.TrimSpace(global.Identity.Email), defaultBucketGitIdentityEmail()),
	}
	gitDir := filepath.Join(target, ".git")
	configPath := filepath.Join(gitDir, "config")
	if _, err := os.Stat(configPath); err == nil {
		initial.Existing = true
	}
	cfg, err := readLocalConfigFile(configPath)
	if err != nil {
		return initial
	}
	if value, ok := cfg.get("bucketgit.logicalRepo"); ok && strings.TrimSpace(repoName) == "" {
		initial.RepoName = strings.TrimSpace(value)
	}
	if value, ok := cfg.get("bucketgit.profile"); ok && strings.TrimSpace(profileName) == "" {
		initial.ProfileName = strings.TrimSpace(value)
	}
	if value, ok := cfg.get("user.name"); ok && strings.TrimSpace(value) != "" {
		initial.IdentityName = strings.TrimSpace(value)
	}
	if value, ok := cfg.get("user.email"); ok && strings.TrimSpace(value) != "" {
		initial.IdentityEmail = strings.TrimSpace(value)
	}
	return initial
}

func initDialogSelectedProfile(profiles []brokerProfile, profileName string) int {
	if strings.TrimSpace(profileName) == "" {
		if len(profiles) == 1 {
			return 0
		}
		return -1
	}
	for i, profile := range profiles {
		if brokerProfileNameMatches(profile, strings.TrimSpace(profileName)) {
			return i
		}
	}
	return -1
}

func (s initDialogState) rows() int {
	return 3 + len(s.profiles)
}

func (s *initDialogState) up() {
	if s.editingField >= 0 {
		return
	}
	s.button = -1
	s.message = ""
	if s.rows() == 0 {
		return
	}
	if s.cursor == 0 {
		s.cursor = s.rows() - 1
		return
	}
	s.cursor--
}

func (s *initDialogState) down() {
	if s.editingField >= 0 {
		return
	}
	s.button = -1
	s.message = ""
	if s.rows() == 0 {
		return
	}
	s.cursor = (s.cursor + 1) % s.rows()
}

func (s *initDialogState) tab() {
	if s.editingField >= 0 {
		s.editingField = -1
		s.editOriginal = ""
	}
	s.message = ""
	if s.button == 1 {
		s.button = -1
		s.cursor = 0
		return
	}
	if s.button < 0 {
		s.button = 0
		return
	}
	s.button = (s.button + 1) % 2
}

func (s *initDialogState) activate() (initDialogResult, bool) {
	if s.button == 0 {
		return s.deploy()
	}
	if s.button == 1 {
		return initDialogResult{}, false
	}
	if s.cursor >= 0 && s.cursor <= 2 {
		s.editingField = s.cursor
		s.editOriginal = s.fieldValue(s.cursor)
		s.message = ""
		return initDialogResult{}, false
	}
	idx := s.cursor - 3
	if idx >= 0 && idx < len(s.profiles) {
		s.selectedProfile = idx
		s.profileName = s.profiles[idx].QualifiedName
	}
	return initDialogResult{}, false
}

func (s *initDialogState) deploy() (initDialogResult, bool) {
	repo := strings.TrimSpace(s.repoName)
	if repo == "" {
		s.message = "Enter a repository name before OK."
		return initDialogResult{}, false
	}
	if email := strings.TrimSpace(s.identityEmail); email != "" && !identityEmailPattern.MatchString(email) {
		s.message = "Email address looks invalid."
		return initDialogResult{}, false
	}
	result := s.result()
	if !result.Changed {
		return result, true
	}
	if (result.RepoChanged || result.ProfileChanged) && (s.selectedProfile < 0 || s.selectedProfile >= len(s.profiles)) {
		s.message = "Select a profile before OK."
		return initDialogResult{}, false
	}
	return result, true
}

func (s initDialogState) result() initDialogResult {
	profileName := strings.TrimSpace(s.profileName)
	if s.selectedProfile >= 0 && s.selectedProfile < len(s.profiles) {
		profileName = s.profiles[s.selectedProfile].QualifiedName
	}
	result := initDialogResult{
		RepoName:      strings.TrimSpace(s.repoName),
		ProfileName:   profileName,
		IdentityName:  strings.TrimSpace(s.identityName),
		IdentityEmail: strings.TrimSpace(s.identityEmail),
	}
	result.RepoChanged = result.RepoName != strings.TrimSpace(s.initialRepoName)
	result.ProfileChanged = result.ProfileName != strings.TrimSpace(s.initialProfileName)
	result.IdentityChanged = result.IdentityName != strings.TrimSpace(s.initialIdentityName) ||
		result.IdentityEmail != strings.TrimSpace(s.initialIdentityEmail)
	result.Changed = result.RepoChanged || result.ProfileChanged || result.IdentityChanged
	if !s.existing && result.ProfileName != "" {
		result.Changed = true
	}
	return result
}

func (s initDialogState) fieldValue(row int) string {
	switch row {
	case 1:
		return s.identityName
	case 2:
		return s.identityEmail
	default:
		return s.repoName
	}
}

func (s *initDialogState) setFieldValue(row int, value string) {
	switch row {
	case 1:
		s.identityName = value
	case 2:
		s.identityEmail = value
	default:
		s.repoName = value
	}
}

func (s *initDialogState) appendFieldByte(b byte) {
	s.message = ""
	value := s.fieldValue(s.editingField)
	if len(value) >= 80 {
		return
	}
	s.setFieldValue(s.editingField, value+string(b))
}

func (s *initDialogState) backspaceField() {
	s.message = ""
	value := s.fieldValue(s.editingField)
	if len(value) == 0 {
		return
	}
	s.setFieldValue(s.editingField, value[:len(value)-1])
}

func renderInitDialogFrame(state initDialogState, rawMode bool) string {
	rendered := renderInitDialogWithStyle(state, rawMode)
	if !rawMode {
		return rendered
	}
	rendered = strings.ReplaceAll(rendered, "\n", "\r\n")
	return "\x1b[?25l\x1b[H\x1b[2J" + rendered
}

func renderInitDialogWithStyle(state initDialogState, style bool) string {
	var lines []string
	lines = append(lines,
		"+------------------------------------------------------------+",
		"|                    BUCKETGIT INIT                          |",
		"+------------------------------------------------------------+",
		setupDialogRow("Configure repository"),
		"|                                                            |",
	)
	inputActive := state.editingField == 0
	inputStyle := setupDialogSectionStyle(style, state.button < 0 && state.cursor == 0)
	if style && inputActive {
		inputStyle += "\x1b[44;97m"
	}
	lines = append(lines, setupDialogRowStyled(fmt.Sprintf("%s Repository [%s]", initDialogMarker(state, 0), initDialogInputValue(state.repoName, 38, inputActive, style)), inputStyle))
	lines = append(lines, setupDialogRow(""))
	lines = append(lines, setupDialogRowStyled("Identity", setupDialogSectionStyle(style, state.button < 0 && (state.cursor == 1 || state.cursor == 2))))
	nameActive := state.editingField == 1
	nameStyle := setupDialogSectionStyle(style, state.button < 0 && state.cursor == 1)
	if style && nameActive {
		nameStyle += "\x1b[44;97m"
	}
	lines = append(lines, setupDialogRowStyled(fmt.Sprintf("%s Name  [%s]", initDialogMarker(state, 1), initDialogInputValue(state.identityName, 43, nameActive, style)), nameStyle))
	emailActive := state.editingField == 2
	emailStyle := setupDialogSectionStyle(style, state.button < 0 && state.cursor == 2)
	if style && emailActive {
		emailStyle += "\x1b[44;97m"
	}
	lines = append(lines, setupDialogRowStyled(fmt.Sprintf("%s Email [%s]", initDialogMarker(state, 2), initDialogInputValue(state.identityEmail, 43, emailActive, style)), emailStyle))
	if state.usesDefaultIdentity() {
		lines = append(lines, setupDialogRowStyled("  Configure name/email with bgit setup or bgit config.", setupDialogANSI(style, "33")))
	}
	lines = append(lines, setupDialogRow(""))
	lines = append(lines, setupDialogRowStyled("Profiles", setupDialogSectionStyle(style, state.button < 0 && state.cursor > 2)))
	for i, profile := range state.profiles {
		cursor := i + 3
		marker := initDialogMarker(state, cursor)
		checked := " "
		if state.selectedProfile == i {
			checked = "x"
		}
		lines = append(lines, setupDialogRowStyled(fmt.Sprintf("%s [%s] %-50s", marker, checked, profile.QualifiedName), setupDialogSectionStyle(style, state.button < 0 && state.cursor > 2)))
	}
	if len(state.profiles) == 0 {
		lines = append(lines, setupDialogRowStyled("  no profiles configured; run bgit setup", setupDialogSectionStyle(style, state.button < 0 && state.cursor > 2)))
	}
	if state.message != "" {
		lines = append(lines, setupDialogRowStyled(state.message, setupDialogANSI(style, "33")))
	}
	okStyle := ""
	cancelStyle := ""
	if style && state.button == 0 {
		okStyle = "\x1b[44;97m"
	}
	if style && state.button == 1 {
		cancelStyle = "\x1b[44;97m"
	}
	lines = append(lines,
		"|                                                            |",
		"+------------------------------------------------------------+",
		setupDialogRow(setupDialogButton("[ OK ]", okStyle)+"    "+setupDialogButton("[ Cancel ]", cancelStyle)),
		setupDialogRow("Enter edits/saves field  Space selects profile"),
		setupDialogRow("Tab fields/buttons  Ctrl-D OK  Esc cancel"),
		"+------------------------------------------------------------+",
	)
	return strings.Join(lines, "\n") + "\n"
}

func initDialogMarker(state initDialogState, row int) string {
	if state.button < 0 && state.cursor == row {
		return ">"
	}
	return " "
}

func (s initDialogState) usesDefaultIdentity() bool {
	return strings.TrimSpace(s.identityName) == defaultBucketGitIdentityName ||
		strings.TrimSpace(s.identityEmail) == defaultBucketGitIdentityEmail()
}

func initDialogInputValue(value string, width int, active, style bool) string {
	_ = style
	if active {
		if len(value) >= width {
			return value[len(value)-width+1:] + "|"
		}
		value += "|"
	}
	if len(value) > width {
		return value[len(value)-width:]
	}
	return value + strings.Repeat(" ", width-len(value))
}

func parsePositiveInt(value string) int {
	n := 0
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

func initBrokerWorktree(target, repoName string, profile brokerProfile, identityName, identityEmail string, stdout io.Writer) error {
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absTarget, 0o755); err != nil {
		return err
	}
	if _, err := runGit(absTarget, "init", "--initial-branch", defaultBranch); err != nil {
		if _, fallbackErr := runGit(absTarget, "init"); fallbackErr != nil {
			return err
		}
	}
	repoName = strings.Trim(repoName, "/")
	if !strings.HasSuffix(repoName, ".git") {
		repoName += ".git"
	}
	remoteURL := fmt.Sprintf("git@%s:%s", defaultSSHHost, repoName)
	sshCommand := "bgit ssh"
	if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" {
		sshCommand = exe + " ssh"
	}
	pairs := [][]string{
		{"bucketgit.broker", profile.BrokerURL},
		{"bucketgit.profile", profile.QualifiedName},
		{"bucketgit.region", profile.Region},
		{"bucketgit.provider", profile.Provider},
		{"bucketgit.logicalRepo", repoName},
		{"core.sshCommand", sshCommand},
	}
	if strings.TrimSpace(identityName) != "" {
		pairs = append(pairs, []string{"user.name", strings.TrimSpace(identityName)})
	}
	if strings.TrimSpace(identityEmail) != "" {
		pairs = append(pairs, []string{"user.email", strings.TrimSpace(identityEmail)})
	}
	for _, pair := range pairs {
		if _, err := runGit(absTarget, "config", "--local", pair[0], pair[1]); err != nil {
			return err
		}
	}
	if err := setGitOrigin(absTarget, remoteURL); err != nil {
		return err
	}
	if err := setGitBranchTracking(absTarget, defaultBranch, "origin"); err != nil {
		return err
	}
	if err := brokerUpsertLogicalRepo(profile.BrokerURL, profile.Provider, repoName); err != nil {
		fmt.Fprintf(stdout, "broker repo registration skipped: %v\n", err)
	}
	fmt.Fprintf(stdout, "Initialized broker-backed BucketGit repository in %s/\n", filepath.Join(absTarget, ".git"))
	fmt.Fprintf(stdout, "configured origin %s\n", remoteURL)
	return nil
}

func writeLocalIdentityConfig(target, name, email string) error {
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(absTarget, ".git")); errors.Is(err, os.ErrNotExist) {
		if _, err := runGit(absTarget, "init", "--initial-branch", defaultBranch); err != nil {
			if _, fallbackErr := runGit(absTarget, "init"); fallbackErr != nil {
				return err
			}
		}
	}
	if strings.TrimSpace(name) != "" {
		if _, err := runGit(absTarget, "config", "--local", "user.name", strings.TrimSpace(name)); err != nil {
			return err
		}
	}
	if strings.TrimSpace(email) != "" {
		if _, err := runGit(absTarget, "config", "--local", "user.email", strings.TrimSpace(email)); err != nil {
			return err
		}
	}
	return nil
}
