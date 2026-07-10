package cli

import (
	"fmt"
	"io"
)

const usageText = `usage: bgit <command> [args]. These are common BucketGit commands:

start a repository
   setup      Connect a cloud account and deploy or update BucketGit
   init       Create a local Git repository backed by BucketGit
   clone      Clone a BucketGit repository into a new directory

local web UI
   web        Browse and manage a repository locally

work on the current change
   add        Add file contents to the index
   mv         Move or rename a file, directory, or symlink
   restore    Restore working tree files
   rm         Remove files from the working tree and index

examine history and state
   diff       Show changes between commits, commit and working tree, etc
   grep       Print lines matching a pattern
   log        Show commit logs
   show       Show objects
   status     Show the working tree status

grow, mark, and tweak history
   branch     List, create, or delete branches
   checkout   Switch branches or restore paths
   commit     Record changes to the repository
   merge      Join development histories together
   reset      Reset HEAD, index, or working tree state
   tag        Create, list, delete, or verify tags

collaborate
   fetch      Download objects and refs from BucketGit
   pull       Fetch and integrate with the current branch
   push       Update remote refs and upload objects
   ls-remote  List remote refs
   pr         Create, review, merge, and close pull requests
   ci         Run and inspect broker CI builds
   board      Manage the repository task board
   issue      Create, comment on, close, and reopen issues

administer
   whoami     Show broker identity, role, and capabilities for this repo
   repos      List repositories visible to local SSH keys
   admin      Manage broker-backed users, keys, owners, and protection
   janitor    Run broker maintenance and repair tasks
   broker     Delete or decommission deployed broker infrastructure
   direct     Run direct bucket recovery and administration commands

Run "bgit help <command>" or "bgit direct help" for details.
`

func Usage(output io.Writer) error {
	_, err := io.WriteString(output, usageText)
	return err
}

func UsageWithBanner(output io.Writer, version string) error {
	if _, err := fmt.Fprintf(output,
		"         _____________________________________________________________\n"+
			"        | ▄▄▄▄▄▄▄                                   ▄▄▄▄▄▄▄           |\n"+
			"        | ███▀▀███▄             ▄▄            ██   ███▀▀▀▀▀  ▀▀  ██   |\n"+
			"________| ███▄▄███▀ ██ ██ ▄████ ██ ▄█▀ ▄█▀█▄ ▀██▀▀ ███       ██ ▀██▀▀ |________\n"+
			"\\       | ███  ███▄ ██ ██ ██    ████   ██▄█▀  ██   ███  ███▀ ██  ██   |       /\n"+
			" \\      | ████████▀ ▀██▀█ ▀████ ██ ▀█▄ ▀█▄▄▄  ██   ▀██████▀  ██▄ ██   |      /\n"+
			" /      |_____________________________________________________________|      \\\n"+
			"/________)   [ bucketgit.com / github.com/bucketgit / bgit %s ]   (________\\\n"+
			"\n", version); err != nil {
		return err
	}
	return Usage(output)
}

type Intent struct {
	Command string
	Args    []string
	Help    bool
	Version bool
}

func ParseIntent(arguments []string) Intent {
	intent := Intent{}
	if len(arguments) == 0 {
		return intent
	}
	intent.Command = arguments[0]
	intent.Args = append([]string(nil), arguments[1:]...)
	intent.Help = len(intent.Args) == 1 && (intent.Args[0] == "help" || intent.Args[0] == "-h" || intent.Args[0] == "--help")
	intent.Version = len(intent.Args) == 1 && (intent.Args[0] == "version" || intent.Args[0] == "-v" || intent.Args[0] == "--version")
	return intent
}
