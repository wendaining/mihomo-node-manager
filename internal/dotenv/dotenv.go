// Package dotenv loads a minimal KEY=VALUE environment file. Variables that
// already exist in the process environment always win; the file only fills in
// the blanks. This keeps CPA credentials out of config.toml and out of git.
package dotenv

import (
	"fmt"
	"os"
	"strings"
)

// Load parses the file at path and exports every variable that is not already
// present in the process environment. A missing file is not an error; a
// malformed file is.
func Load(path string) error {
	entries, err := Parse(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if _, exists := os.LookupEnv(entry[0]); exists {
			continue
		}
		if err := os.Setenv(entry[0], entry[1]); err != nil {
			return fmt.Errorf("export %s: %w", entry[0], err)
		}
	}
	return nil
}

// Parse reads path and returns its KEY=VALUE pairs in file order.
func Parse(path string) ([][2]string, error) {
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries [][2]string
	for number, line := range strings.Split(string(contents), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, found := strings.Cut(trimmed, "=")
		if !found {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, number+1)
		}
		key = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "export "))
		if !validName(key) {
			return nil, fmt.Errorf("%s:%d: invalid variable name %q", path, number+1, key)
		}
		entries = append(entries, [2]string{key, unquote(strings.TrimSpace(value))})
	}
	return entries, nil
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for i, char := range name {
		switch {
		case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char == '_':
		case char >= '0' && char <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// unquote strips surrounding single or double quotes. Values are otherwise
// taken literally; inline comments are not supported.
func unquote(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}
