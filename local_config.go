package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type localConfigFile struct {
	sections map[string]map[string]string
	order    []string
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
		cfg = globalConfig{Version: globalConfigVersion}
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
	cfg := localConfigFile{sections: map[string]map[string]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.TrimSpace(strings.Trim(trimmed, "[]"))
			cfg.ensureSection(section)
			continue
		}
		name, value, ok := strings.Cut(trimmed, "=")
		if !ok || section == "" {
			continue
		}
		cfg.sections[section][strings.TrimSpace(name)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return cfg, nil
}

func (c *localConfigFile) ensureSection(section string) {
	if c.sections == nil {
		c.sections = map[string]map[string]string{}
	}
	if _, ok := c.sections[section]; ok {
		return
	}
	c.sections[section] = map[string]string{}
	c.order = append(c.order, section)
}

func (c *localConfigFile) get(key string) (string, bool) {
	section, name := configSectionAndName(key)
	values, ok := c.sections[section]
	if !ok {
		return "", false
	}
	value, ok := values[name]
	return value, ok
}

func (c *localConfigFile) set(key, value string) {
	section, name := configSectionAndName(key)
	c.ensureSection(section)
	c.sections[section][name] = value
}

func (c *localConfigFile) unset(key string) {
	section, name := configSectionAndName(key)
	if values, ok := c.sections[section]; ok {
		delete(values, name)
	}
}

func (c *localConfigFile) keys() []string {
	var keys []string
	for _, section := range c.orderedSections() {
		for name := range c.sections[section] {
			keys = append(keys, configFullKey(section, name))
		}
	}
	sort.Strings(keys)
	return keys
}

func (c *localConfigFile) write(path string) error {
	var out strings.Builder
	for _, section := range c.orderedSections() {
		values := c.sections[section]
		if len(values) == 0 {
			continue
		}
		fmt.Fprintf(&out, "[%s]\n", section)
		var names []string
		for name := range values {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&out, "\t%s = %s\n", name, values[name])
		}
		out.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(out.String()), 0o644)
}

func (c *localConfigFile) orderedSections() []string {
	seen := map[string]struct{}{}
	var sections []string
	for _, section := range c.order {
		if _, ok := c.sections[section]; ok {
			sections = append(sections, section)
			seen[section] = struct{}{}
		}
	}
	for section := range c.sections {
		if _, ok := seen[section]; !ok {
			sections = append(sections, section)
		}
	}
	sort.SliceStable(sections, func(i, j int) bool {
		_, iSeen := seen[sections[i]]
		_, jSeen := seen[sections[j]]
		if iSeen != jSeen {
			return iSeen
		}
		return sections[i] < sections[j]
	})
	return sections
}

func configSectionAndName(key string) (string, string) {
	parts := strings.Split(key, ".")
	if len(parts) <= 2 {
		if len(parts) == 1 {
			return parts[0], ""
		}
		return parts[0], parts[1]
	}
	return fmt.Sprintf(`%s "%s"`, parts[0], parts[1]), strings.Join(parts[2:], ".")
}

func configFullKey(section, name string) string {
	if before, after, ok := strings.Cut(section, " "); ok {
		return before + "." + strings.Trim(after, `"`) + "." + name
	}
	return section + "." + name
}
