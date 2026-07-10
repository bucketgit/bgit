package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RepositoryFile is the parsed representation of a repository's .git/config.
type RepositoryFile struct {
	sections map[string]map[string]string
	order    []string
}

func ReadRepositoryFile(path string) (*RepositoryFile, error) {
	value := &RepositoryFile{sections: map[string]map[string]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return value, nil
	}
	if err != nil {
		return nil, err
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.TrimSpace(strings.Trim(trimmed, "[]"))
			value.ensureSection(section)
			continue
		}
		name, setting, ok := strings.Cut(trimmed, "=")
		if !ok || section == "" {
			continue
		}
		value.sections[section][strings.TrimSpace(name)] = strings.Trim(strings.TrimSpace(setting), `"`)
	}
	return value, nil
}

func (c *RepositoryFile) Get(key string) (string, bool) {
	section, name := sectionAndName(key)
	values, ok := c.sections[section]
	if !ok {
		return "", false
	}
	value, ok := values[name]
	return value, ok
}

func (c *RepositoryFile) Set(key, value string) {
	section, name := sectionAndName(key)
	c.ensureSection(section)
	c.sections[section][name] = value
}

func (c *RepositoryFile) Unset(key string) {
	section, name := sectionAndName(key)
	if values, ok := c.sections[section]; ok {
		delete(values, name)
	}
}

func (c *RepositoryFile) Keys() []string {
	var keys []string
	for _, section := range c.orderedSections() {
		for name := range c.sections[section] {
			keys = append(keys, fullKey(section, name))
		}
	}
	sort.Strings(keys)
	return keys
}

func (c *RepositoryFile) Write(path string) error {
	var output strings.Builder
	for _, section := range c.orderedSections() {
		values := c.sections[section]
		if len(values) == 0 {
			continue
		}
		fmt.Fprintf(&output, "[%s]\n", section)
		names := make([]string, 0, len(values))
		for name := range values {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&output, "\t%s = %s\n", name, values[name])
		}
		output.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(output.String()), 0o644)
}

func (c *RepositoryFile) ensureSection(section string) {
	if c.sections == nil {
		c.sections = map[string]map[string]string{}
	}
	if _, ok := c.sections[section]; ok {
		return
	}
	c.sections[section] = map[string]string{}
	c.order = append(c.order, section)
}

func (c *RepositoryFile) orderedSections() []string {
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
		_, leftSeen := seen[sections[i]]
		_, rightSeen := seen[sections[j]]
		if leftSeen != rightSeen {
			return leftSeen
		}
		return sections[i] < sections[j]
	})
	return sections
}

func sectionAndName(key string) (string, string) {
	parts := strings.Split(key, ".")
	if len(parts) <= 2 {
		if len(parts) == 1 {
			return parts[0], ""
		}
		return parts[0], parts[1]
	}
	return fmt.Sprintf(`%s "%s"`, parts[0], parts[1]), strings.Join(parts[2:], ".")
}

func fullKey(section, name string) string {
	if before, after, ok := strings.Cut(section, " "); ok {
		return before + "." + strings.Trim(after, `"`) + "." + name
	}
	return section + "." + name
}
