package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Environment variable names.
const (
	EnvURL         = "MM_MCP_URL"
	EnvToken       = "MM_MCP_TOKEN"
	EnvTeam        = "MM_MCP_TEAM"
	EnvDownloadDir = "MM_MCP_DOWNLOAD_DIR"
	EnvUploadRoot  = "MM_MCP_UPLOAD_ROOT"
)

// Config is the validated server configuration.
type Config struct {
	// URL is the server URL without trailing slashes.
	URL string
	// Token is the personal access token. Never log or print it.
	Token string
	// Team is a team name or 26-char team id.
	Team string
	// DownloadDir is an absolute, existing, writable directory, or "" when unset.
	DownloadDir string
	// UploadRoot is an absolute directory with symlinks resolved, or "" when attachments are
	// disabled because the unset variable defaulted to / or the home directory.
	UploadRoot string
}

// Load reads the configuration through lookup (os.LookupEnv in production).
// All problems are collected into one single-line error, e.g.
// "set MM_MCP_URL and MM_MCP_TEAM; MM_MCP_UPLOAD_ROOT: ...".
// The token value never appears in the error.
func Load(lookup func(string) (string, bool)) (Config, error) {
	get := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}

	var missing, problems []string
	cfg := Config{
		URL:   strings.TrimRight(get(EnvURL), "/"),
		Token: get(EnvToken),
		Team:  get(EnvTeam),
	}

	if cfg.URL == "" {
		missing = append(missing, EnvURL)
	} else if err := checkURL(cfg.URL); err != nil {
		problems = append(problems, fmt.Sprintf("%s: %v", EnvURL, err))
	}
	if cfg.Token == "" {
		missing = append(missing, EnvToken)
	}
	if cfg.Team == "" {
		missing = append(missing, EnvTeam)
	}

	if dir := get(EnvDownloadDir); dir != "" {
		abs, err := checkDownloadDir(dir)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", EnvDownloadDir, err))
		}
		cfg.DownloadDir = abs
	}

	root, err := resolveUploadRoot(get(EnvUploadRoot))
	if err != nil {
		problems = append(problems, fmt.Sprintf("%s: %v", EnvUploadRoot, err))
	}
	cfg.UploadRoot = root

	if len(missing) > 0 {
		problems = append([]string{"set " + joinAnd(missing)}, problems...)
	}
	if len(problems) > 0 {
		return Config{}, errors.New(strings.Join(problems, "; "))
	}
	return cfg, nil
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("not a valid URL")
	}
	// Errors never quote user:password@, the query or the fragment (either may carry a secret);
	// the value shown is the URL without them.
	// A bare "#" leaves Fragment empty, so the fragment is detected on the raw value.
	hasQuery, hasFragment := u.RawQuery != "" || u.ForceQuery, strings.Contains(raw, "#")
	shown := raw
	if u.User != nil || hasQuery || hasFragment {
		bare := *u
		bare.User, bare.RawQuery, bare.ForceQuery, bare.Fragment, bare.RawFragment = nil, "", false, "", ""
		shown = bare.String()
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https in %q", shown)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host in %q", shown)
	}
	// The token is the only credential; userinfo would never be sent, yet it would surface in
	// tool output (the api tool names the base URL) and in transport errors.
	if u.User != nil {
		return fmt.Errorf("credentials (user:password@) are not allowed in %q", shown)
	}
	// The client appends /api/v4/... to the base URL; anything after the path breaks every request.
	if hasQuery {
		return fmt.Errorf("remove the query (?...) from %q", shown)
	}
	if hasFragment {
		return fmt.Errorf("remove the fragment (#...) from %q", shown)
	}
	if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/api/v4") {
		return fmt.Errorf("remove the trailing /api/v4 from %q", shown)
	}
	return nil
}

func checkDownloadDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := checkDir(abs); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(abs, ".mm-mcp-probe-*")
	if err != nil {
		return "", fmt.Errorf("%s is not writable", abs)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return abs, nil
}

// resolveUploadRoot resolves dir, or the working directory when dir is empty. A defaulted root
// that is / or the home directory, where GUI hosts start servers, yields "" (attachments disabled).
func resolveUploadRoot(dir string) (string, error) {
	defaulted := dir == ""
	if defaulted {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := checkDir(abs); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s", abs)
	}
	if defaulted && (resolved == string(filepath.Separator) || resolved == realHome()) {
		return "", nil
	}
	return resolved, nil
}

// realHome is the symlink-resolved home directory, or "" when it cannot be determined.
func realHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		return ""
	}
	return resolved
}

func checkDir(abs string) error {
	fi, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s does not exist", abs)
		}
		return fmt.Errorf("cannot access %s", abs)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", abs)
	}
	return nil
}

// joinAnd renders ["A","B","C"] as "A, B and C".
func joinAnd(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
