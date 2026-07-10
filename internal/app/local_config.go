package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	internalconfig "github.com/bucketgit/bgit/internal/config"
)

type localConfigFile struct {
	value *internalconfig.RepositoryFile
}

func (r *localRepository) config(args []string, stdout io.Writer) error {
	opts, err := parseConfigArgs(args)
	if err != nil {
		return err
	}
	path := filepath.Join(r.gitDir, "config")
	cfg, err := readLocalConfigFile(path)
	if err != nil {
		return err
	}
	if opts.list {
		for _, key := range cfg.keys() {
			value, _ := cfg.get(key)
			fmt.Fprintf(stdout, "%s=%s\n", key, value)
		}
		return nil
	}
	if opts.unset {
		if opts.key == "" {
			return errors.New("config --unset requires a key")
		}
		cfg.unset(opts.key)
		return cfg.write(path)
	}
	if opts.key == "" {
		return errors.New("config requires a key")
	}
	if opts.value == nil {
		value, ok := cfg.get(opts.key)
		if !ok {
			return nil
		}
		fmt.Fprintln(stdout, value)
		return nil
	}
	cfg.set(opts.key, *opts.value)
	return cfg.write(path)
}

type configOptions struct {
	key   string
	value *string
	list  bool
	unset bool
}

func parseConfigArgs(args []string) (configOptions, error) {
	var opts configOptions
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--local":
		case "--global", "--system", "--worktree":
			return opts, fmt.Errorf("unsupported config scope %s", arg)
		case "--list", "-l":
			opts.list = true
		case "--get", "--bool":
		case "--unset":
			opts.unset = true
		case "--add":
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unsupported config option %s", arg)
			}
			positional = append(positional, arg)
		}
	}
	if opts.list {
		if len(positional) != 0 {
			return opts, errors.New("config --list does not accept keys")
		}
		return opts, nil
	}
	if len(positional) < 1 || len(positional) > 2 {
		return opts, errors.New("usage: bgit config [--local] [--get|--unset|--list] key [value]")
	}
	opts.key = positional[0]
	if len(positional) == 2 {
		opts.value = &positional[1]
	}
	return opts, nil
}

func configArgsAreGlobal(args []string) bool {
	for _, arg := range args {
		if arg == "--global" {
			return true
		}
	}
	return false
}

func globalConfigCommand(args []string, stdout io.Writer) error {
	opts, err := parseGlobalConfigArgs(args)
	if err != nil {
		return err
	}
	path, err := defaultGlobalConfigPath()
	if err != nil {
		return err
	}
	cfg, err := readGlobalConfig(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		cfg = internalconfig.Global{Version: globalConfigVersion}
	}
	if opts.list {
		if cfg.Identity.Name != "" {
			fmt.Fprintf(stdout, "user.name=%s\n", cfg.Identity.Name)
		}
		if cfg.Identity.Email != "" {
			fmt.Fprintf(stdout, "user.email=%s\n", cfg.Identity.Email)
		}
		return nil
	}
	if opts.unset {
		switch opts.key {
		case "user.name":
			cfg.Identity.Name = ""
		case "user.email":
			cfg.Identity.Email = ""
		default:
			return fmt.Errorf("unsupported global config key %s", opts.key)
		}
		return writeGlobalConfig(path, cfg)
	}
	if opts.value == nil {
		switch opts.key {
		case "user.name":
			if cfg.Identity.Name != "" {
				fmt.Fprintln(stdout, cfg.Identity.Name)
			}
		case "user.email":
			if cfg.Identity.Email != "" {
				fmt.Fprintln(stdout, cfg.Identity.Email)
			}
		default:
			return fmt.Errorf("unsupported global config key %s", opts.key)
		}
		return nil
	}
	switch opts.key {
	case "user.name":
		cfg.Identity.Name = *opts.value
	case "user.email":
		if !identityEmailPattern.MatchString(*opts.value) {
			return fmt.Errorf("email address %q looks invalid", *opts.value)
		}
		cfg.Identity.Email = *opts.value
	default:
		return fmt.Errorf("unsupported global config key %s", opts.key)
	}
	return writeGlobalConfig(path, cfg)
}

func parseGlobalConfigArgs(args []string) (configOptions, error) {
	var filtered []string
	for _, arg := range args {
		if arg == "--global" {
			continue
		}
		filtered = append(filtered, arg)
	}
	return parseConfigArgs(filtered)
}

func readLocalConfigFile(path string) (localConfigFile, error) {
	value, err := internalconfig.ReadRepositoryFile(path)
	return localConfigFile{value: value}, err
}

func (c *localConfigFile) get(key string) (string, bool) {
	return c.value.Get(key)
}

func (c *localConfigFile) set(key, value string) {
	c.value.Set(key, value)
}

func (c *localConfigFile) unset(key string) {
	c.value.Unset(key)
}

func (c *localConfigFile) keys() []string {
	return c.value.Keys()
}

func (c *localConfigFile) write(path string) error {
	return c.value.Write(path)
}
