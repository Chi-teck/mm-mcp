package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secret = "s3cr3t-token-value"

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoad(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(tmp, link); err != nil {
		t.Fatal(err)
	}
	realTmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	realCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}

	base := func(extra map[string]string) map[string]string {
		m := map[string]string{EnvURL: "https://mm.example.com", EnvToken: secret, EnvTeam: "team"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr string
	}{
		{
			name: "minimal",
			env:  base(nil),
			want: Config{URL: "https://mm.example.com", Token: secret, Team: "team", UploadRoot: realCwd},
		},
		{
			name: "trims whitespace and trailing slashes",
			env: map[string]string{
				EnvURL: "  https://mm.example.com/// \n", EnvToken: " " + secret + "\t", EnvTeam: " team ",
			},
			want: Config{URL: "https://mm.example.com", Token: secret, Team: "team", UploadRoot: realCwd},
		},
		{
			name: "all optional set",
			env:  base(map[string]string{EnvDownloadDir: tmp, EnvUploadRoot: link}),
			want: Config{
				URL: "https://mm.example.com", Token: secret, Team: "team",
				DownloadDir: tmp, UploadRoot: realTmp,
			},
		},
		{
			name: "subpath kept",
			env:  base(map[string]string{EnvURL: "https://mm.example.com/api/v4x/"}),
			want: Config{URL: "https://mm.example.com/api/v4x", Token: secret, Team: "team", UploadRoot: realCwd},
		},
		{
			name: "upload root slash",
			env:  base(map[string]string{EnvUploadRoot: "/"}),
			want: Config{URL: "https://mm.example.com", Token: secret, Team: "team", UploadRoot: "/"},
		},
		{
			name:    "all missing",
			env:     map[string]string{},
			wantErr: "set MM_MCP_URL, MM_MCP_TOKEN and MM_MCP_TEAM",
		},
		{
			name:    "blank counts as missing",
			env:     map[string]string{EnvURL: " / ", EnvToken: "  ", EnvTeam: "t"},
			wantErr: "set MM_MCP_URL and MM_MCP_TOKEN",
		},
		{
			name:    "bad scheme",
			env:     base(map[string]string{EnvURL: "ftp://mm.example.com"}),
			wantErr: `MM_MCP_URL: scheme must be http or https in "ftp://mm.example.com"`,
		},
		{
			name:    "no host",
			env:     base(map[string]string{EnvURL: "https://"}),
			wantErr: `MM_MCP_URL: missing host in "https:"`,
		},
		{
			name:    "credentials refused, not quoted",
			env:     base(map[string]string{EnvURL: "https://user:hunter2@mm.example.com"}),
			wantErr: `MM_MCP_URL: credentials (user:password@) are not allowed in "https://mm.example.com"`,
		},
		{
			name:    "bad scheme hides credentials",
			env:     base(map[string]string{EnvURL: "ftp://user:hunter2@mm.example.com"}),
			wantErr: `MM_MCP_URL: scheme must be http or https in "ftp://mm.example.com"`,
		},
		{
			name:    "query refused, not quoted",
			env:     base(map[string]string{EnvURL: "https://mm.example.com?token=hunter2"}),
			wantErr: `MM_MCP_URL: remove the query (?...) from "https://mm.example.com"`,
		},
		{
			name:    "empty query refused",
			env:     base(map[string]string{EnvURL: "https://mm.example.com/mm?"}),
			wantErr: `MM_MCP_URL: remove the query (?...) from "https://mm.example.com/mm"`,
		},
		{
			name:    "fragment refused, not quoted",
			env:     base(map[string]string{EnvURL: "https://mm.example.com/#hunter2"}),
			wantErr: `MM_MCP_URL: remove the fragment (#...) from "https://mm.example.com/"`,
		},
		{
			name:    "empty fragment refused",
			env:     base(map[string]string{EnvURL: "https://mm.example.com#"}),
			wantErr: `MM_MCP_URL: remove the fragment (#...) from "https://mm.example.com"`,
		},
		{
			name:    "api prefix refused",
			env:     base(map[string]string{EnvURL: "https://mm.example.com/api/v4"}),
			wantErr: `MM_MCP_URL: remove the trailing /api/v4 from "https://mm.example.com/api/v4"`,
		},
		{
			name:    "api prefix with slash and subpath refused",
			env:     base(map[string]string{EnvURL: "https://mm.example.com/mm/api/v4/"}),
			wantErr: `MM_MCP_URL: remove the trailing /api/v4 from "https://mm.example.com/mm/api/v4"`,
		},
		{
			name:    "download dir missing",
			env:     base(map[string]string{EnvDownloadDir: filepath.Join(tmp, "nope")}),
			wantErr: "MM_MCP_DOWNLOAD_DIR: " + filepath.Join(tmp, "nope") + " does not exist",
		},
		{
			name:    "upload root is a file",
			env:     base(map[string]string{EnvUploadRoot: file}),
			wantErr: "MM_MCP_UPLOAD_ROOT: " + file + " is not a directory",
		},
		{
			name: "collects every problem",
			env: map[string]string{
				EnvToken: secret, EnvURL: "mm.example.com",
				EnvDownloadDir: file, EnvUploadRoot: filepath.Join(tmp, "nope"),
			},
			wantErr: "set MM_MCP_TEAM; " +
				`MM_MCP_URL: scheme must be http or https in "mm.example.com"; ` +
				"MM_MCP_DOWNLOAD_DIR: " + file + " is not a directory; " +
				"MM_MCP_UPLOAD_ROOT: " + filepath.Join(tmp, "nope") + " does not exist",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(env(tt.env))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error %q, got nil", tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("error:\n got %q\nwant %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "\n") {
					t.Fatalf("error leaks token or spans lines: %q", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("config:\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestLoadDefaultUploadRootUnsafe(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	base := map[string]string{EnvURL: "http://localhost:8065", EnvToken: secret, EnvTeam: "t"}
	explicit := func(root string) map[string]string {
		m := map[string]string{EnvUploadRoot: root}
		for k, v := range base {
			m[k] = v
		}
		return m
	}

	tests := []struct {
		name, cwd string
		env       map[string]string
		want      string
	}{
		{"defaulted to /", "/", base, ""},
		{"defaulted to home", home, base, ""},
		{"explicit /", home, explicit("/"), "/"},
		{"explicit home", "/", explicit(home), home},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(tt.cwd)
			got, err := Load(env(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			if got.UploadRoot != tt.want {
				t.Fatalf("UploadRoot = %q, want %q", got.UploadRoot, tt.want)
			}
		})
	}
}

func TestLoadRelativeDownloadDir(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, "dl"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmp)
	got, err := Load(env(map[string]string{
		EnvURL: "http://localhost:8065", EnvToken: secret, EnvTeam: "t", EnvDownloadDir: "dl",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(tmp, "dl"); got.DownloadDir != want {
		t.Fatalf("DownloadDir = %q, want %q", got.DownloadDir, want)
	}
}

func TestLoadDownloadDirNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	_, err := Load(env(map[string]string{
		EnvURL: "http://localhost", EnvToken: secret, EnvTeam: "t", EnvDownloadDir: dir,
	}))
	if want := "MM_MCP_DOWNLOAD_DIR: " + dir + " is not writable"; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestLoadDownloadDirSideEffects(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "nope")
	if _, err := Load(env(map[string]string{
		EnvURL: "http://localhost", EnvToken: secret, EnvTeam: "t", EnvDownloadDir: missing,
	})); err == nil {
		t.Fatal("want error for missing download dir")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("download dir was created: %v", err)
	}

	if _, err := Load(env(map[string]string{
		EnvURL: "http://localhost", EnvToken: secret, EnvTeam: "t", EnvDownloadDir: tmp,
	})); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("writability probe left files behind: %v", entries)
	}
}
