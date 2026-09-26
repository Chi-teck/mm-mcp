package tools

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Chi-teck/mm-mcp/internal/config"
	"github.com/Chi-teck/mm-mcp/internal/mattermost"
)

const (
	// maxDownload is the download size ceiling, above the server's upload cap (MaxFileSize, 100 MiB by default).
	maxDownload = 256 << 20
	// sequentialNames covers x.txt, x-1.txt … x-99.txt; after that a random suffix is tried.
	sequentialNames = 100
	// maxNameAttempts bounds the name search so a directory that refuses every name ends the call.
	maxNameAttempts = sequentialNames + 10
	// maxErrorBody bounds how much of a failed download's body is read for its message.
	maxErrorBody = 64 << 10
	// maxNameBytes is NAME_MAX on Linux and macOS; longer names fail with ENAMETOOLONG.
	maxNameBytes = 255
)

type getFileIn struct {
	FileID string `json:"file_id" jsonschema:"26-char file id"`
	Name   string `json:"name,omitempty" jsonschema:"Preferred file name"`
}

// registerFiles adds get_file.
func registerFiles(s *mcp.Server, c *mattermost.Context) {
	where := c.DownloadDir()
	if where == "" {
		where = "the " + config.EnvDownloadDir + " directory (currently unset, so every call fails)"
	}
	addTool(s, "get_file",
		"Download a Mattermost file attachment by id into "+where+" and return the saved path.",
		func(ctx context.Context, in getFileIn) (string, error) { return getFile(ctx, c, in) })
}

// getFile streams the attachment into the download directory under a name no other file holds.
func getFile(ctx context.Context, c *mattermost.Context, in getFileIn) (string, error) {
	dir := c.DownloadDir()
	if dir == "" {
		return "", fmt.Errorf("get_file needs %s: set it to an existing, writable directory", config.EnvDownloadDir)
	}
	// The id becomes a path segment of an authenticated request; anything but an id could
	// point the token at another endpoint and save its answer to disk.
	if err := checkID("file id", in.FileID); err != nil {
		return "", err
	}
	path := "/api/v4/files/" + in.FileID
	resp, err := c.RawDo(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return "", fmt.Errorf("file download %s failed: %w", in.FileID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		location := resp.Header.Get("Location")
		if location == "" {
			location = noLocationText
		}
		return "", fmt.Errorf("file download %s redirected to %s; refusing to follow it", in.FileID, location)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &mattermost.APIError{Status: resp.StatusCode, Path: path, Message: serverMessage(resp)}
	}
	if resp.ContentLength > maxDownload {
		size, lim := sizePair(resp.ContentLength, maxDownload)
		return "", fmt.Errorf("file %s is %s, over the %s limit", in.FileID, size, lim)
	}

	name := in.Name
	if name == "" {
		name = dispositionName(resp.Header.Get("Content-Disposition"))
	}
	name = sanitizeName(name)
	if name == "" {
		name = in.FileID + ".bin"
	}
	f, target, err := createUnique(dir, name)
	if err != nil {
		return "", err
	}
	if err := stream(f, resp.Body, in.FileID); err != nil {
		_ = os.Remove(target)
		return "", err
	}
	return fmt.Sprintf("Saved %s (file id: %s)", target, in.FileID), nil
}

// serverMessage returns the server message of a failed download, as the Client4 path
// would report it.
func serverMessage(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return mattermost.ServerMessage(resp.Header.Get("Content-Type"), raw)
}

// dispositionName returns the filename of a Content-Disposition header (the RFC 5987
// `filename*` form wins, as mime decodes it into "filename"), or "".
func dispositionName(header string) string {
	if header == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(header)
	if err != nil {
		return rawDispositionName(header)
	}
	return params["filename"]
}

// rawDispositionName extracts the filename from a header mime rejects. Mattermost builds
// the `filename*` value with url.PathEscape, which leaves the tspecials `@`, `:` and `=` raw,
// so the whole header fails to parse for names like "logo@2x.png".
func rawDispositionName(header string) string {
	lower := strings.ToLower(header)
	const extended = "filename*=utf-8''"
	if i := strings.Index(lower, extended); i >= 0 {
		value, _, _ := strings.Cut(header[i+len(extended):], ";")
		if name, err := url.PathUnescape(strings.TrimSpace(value)); err == nil {
			return name
		}
	}
	const quoted = `filename="`
	if i := strings.Index(lower, quoted); i >= 0 {
		if name, _, ok := strings.Cut(header[i+len(quoted):], `"`); ok {
			return name
		}
	}
	return ""
}

// sanitizeName makes name a single plain path element: separators and control characters
// become `_`; "", "." and ".." come back as "" so the caller falls back to `<file_id>.bin`.
func sanitizeName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || unicode.IsControl(r) || r == unicode.ReplacementChar {
			return '_'
		}
		return r
	}, name)
	if name == "." || name == ".." {
		return ""
	}
	return name
}

// createUnique claims a free name in dir with O_CREATE|O_EXCL: name, name-1 … name-99, then
// random 6-char suffixes, maxNameAttempts in total. The caller writes through the returned file.
func createUnique(dir, name string) (*os.File, string, error) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	if base == "" { // ".bashrc": a dotfile has no extension
		base, ext = name, ""
	}
	if len(ext) > maxNameBytes/2 { // a huge "extension" is just part of the name
		base, ext = name, ""
	}
	for attempt := range maxNameAttempts {
		suffix := ""
		switch {
		case attempt == 0:
		case attempt < sequentialNames:
			suffix = fmt.Sprintf("-%d", attempt)
		default:
			suffix = "-" + strings.ToLower(rand.Text()[:6])
		}
		candidate := clipBytes(base, maxNameBytes-len(suffix)-len(ext)) + suffix + ext
		target := filepath.Join(dir, candidate)
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // name is sanitized to one path element
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("cannot save %s: %w", name, err)
		}
		return f, target, nil
	}
	return nil, "", fmt.Errorf("no free name for %s in %s after %d attempts", name, dir, maxNameAttempts)
}

// clipBytes cuts s to at most n bytes on a rune boundary.
func clipBytes(s string, n int) string {
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// stream copies body into f, refusing more than maxDownload bytes, and closes f. The caller
// removes the file when it returns an error.
func stream(f *os.File, body io.Reader, fileID string) error {
	n, err := io.Copy(f, io.LimitReader(body, maxDownload+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("file download %s failed: %w", fileID, err)
	}
	if n > maxDownload {
		return fmt.Errorf("file %s is over the %s limit", fileID, mattermost.HumanSize(maxDownload))
	}
	return nil
}
