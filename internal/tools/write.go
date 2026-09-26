package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Chi-teck/mm-mcp/internal/config"
	"github.com/Chi-teck/mm-mcp/internal/mattermost"
)

// scheduleLayout renders the send time of a scheduled post in the server process's local zone.
const scheduleLayout = "2006-01-02 15:04 MST"

// maxAttachments is the most file ids Post.IsValid accepts (300 runes of JSON, 10 ids).
const maxAttachments = 10

// orphanNote explains why orphaned upload ids are named in an error.
const orphanNote = "are orphaned on the server and cannot be deleted " +
	"(Mattermost only deletes a file with the post that carries it)"

type createPostIn struct {
	Channel      string   `json:"channel" jsonschema:"Channel name or 26-char id"`
	Message      string   `json:"message" jsonschema:"Message text (markdown supported)"`
	ThreadRootID string   `json:"thread_root_id,omitempty" jsonschema:"Root post id to reply in a thread"`
	ScheduleAt   string   `json:"schedule_at,omitempty" jsonschema:"Send later instead of now: 30m, 2h, 3d, ISO date/datetime, or epoch ms (future, at most 365 days out)"`
	Attachments  []string `json:"attachments,omitempty" jsonschema:"Local file paths to attach; relative paths resolve against the server's working directory"`
}

// registerWrite adds create_post.
func registerWrite(s *mcp.Server, c *mattermost.Context) {
	addTool(s, "create_post",
		"Post a message to a Mattermost channel, optionally as a thread reply (thread_root_id) with file "+
			"attachments (local paths inside the upload root), now or scheduled for later (schedule_at). "+
			"Scheduled posts are listed with the api tool (GET /posts/scheduled/team/{team_id}?includeDirectChannels=true) "+
			"and canceled with DELETE /posts/schedule/<id>.",
		func(ctx context.Context, in createPostIn) (string, error) { return createPost(ctx, c, in) })
}

// attachment is one attachment argument after confinement: the path as given, its absolute
// form, its symlink-resolved path, and that path relative to the upload root, which is what is
// opened. missing marks a path that did not resolve because it does not exist; it is reported as
// missing, never opened.
type attachment struct {
	arg, abs, real, rel string
	missing             bool
	file                *os.File
	size                int64
}

// createPost implements create_post: every check runs before
// the first network write.
func createPost(ctx context.Context, c *mattermost.Context, in createPostIn) (string, error) {
	if strings.TrimSpace(in.Message) == "" && len(in.Attachments) == 0 {
		return "", errors.New("refusing to post an empty message with no attachments")
	}
	var at int64
	if in.ScheduleAt != "" {
		var err error
		if at, err = mattermost.ParseSchedule(in.ScheduleAt, time.Now()); err != nil {
			return "", err
		}
	}
	if in.ThreadRootID != "" {
		if err := checkID("thread root id", in.ThreadRootID); err != nil {
			return "", err
		}
	}
	if len(in.Attachments) > maxAttachments {
		return "", fmt.Errorf("too many attachments: %d (a post carries at most %d files)", len(in.Attachments), maxAttachments)
	}
	dir, files, err := confine(c.UploadRoot(), in.Attachments)
	if err != nil {
		return "", err
	}
	if dir != nil {
		defer func() { _ = dir.Close() }()
	}
	ch, err := c.ResolveChannel(ctx, in.Channel)
	if err != nil {
		return "", err
	}
	if in.ThreadRootID != "" {
		if err := checkThreadRoot(ctx, c, in.ThreadRootID, ch); err != nil {
			return "", err
		}
	}
	defer closeAll(files)
	if err := openAll(ctx, c, dir, files); err != nil {
		return "", err
	}
	fileIDs, err := uploadAll(ctx, c, ch.Id, files)
	if err != nil {
		return "", err
	}

	filesNote := ""
	if len(fileIDs) > 0 {
		filesNote = fmt.Sprintf(", %d file(s)", len(fileIDs))
	}
	if in.ScheduleAt != "" {
		sp := &model.ScheduledPost{
			Draft:       model.Draft{ChannelId: ch.Id, RootId: in.ThreadRootID, Message: in.Message, FileIds: fileIDs},
			ScheduledAt: at,
		}
		created, _, err := c.Client().CreateScheduledPost(ctx, sp)
		if err != nil {
			return "", orphaned(fileIDs, "the post", mattermost.WrapErr("/api/v4/posts/schedule", err))
		}
		when := time.UnixMilli(at).Local().Format(scheduleLayout)
		return fmt.Sprintf("Scheduled for %s in %s (scheduled post id: %s%s)",
			when, c.ChannelLabel(ctx, ch), created.Id, filesNote), nil
	}
	post := &model.Post{ChannelId: ch.Id, RootId: in.ThreadRootID, Message: in.Message, FileIds: fileIDs}
	created, _, err := c.Client().CreatePost(ctx, post)
	if err != nil {
		return "", orphaned(fileIDs, "the post", mattermost.WrapErr("/api/v4/posts", err))
	}
	return fmt.Sprintf("Posted to %s (post id: %s%s)", c.ChannelLabel(ctx, ch), created.Id, filesNote), nil
}

// checkThreadRoot refuses a thread root the server would reject only after the uploads: one
// that is missing, itself a reply, or in another channel than ch.
func checkThreadRoot(ctx context.Context, c *mattermost.Context, id string, ch *model.Channel) error {
	root, err := threadRoot(ctx, c, id)
	if err != nil {
		return err
	}
	if root.ChannelId != ch.Id {
		return fmt.Errorf("thread root %s is not in %s", id, c.ChannelLabel(ctx, ch))
	}
	return nil
}

// threadRoot reads the post id and refuses it when it is missing or itself a reply.
func threadRoot(ctx context.Context, c *mattermost.Context, id string) (*model.Post, error) {
	root, _, err := c.Client().GetPost(ctx, id, "")
	if err != nil {
		err = mattermost.WrapErr("/api/v4/posts/"+id, err)
		if mattermost.IsNotFound(err) {
			return nil, fmt.Errorf("thread root %s not found — it is deleted, or in a channel this user cannot read", id)
		}
		return nil, err
	}
	if root.RootId != "" {
		return nil, fmt.Errorf("post %s is a reply, not a thread root — its thread root id is %s", id, root.RootId)
	}
	return root, nil
}

// confine resolves every attachment in order and requires its symlink-resolved path to sit
// strictly inside root. root is used as given: config.Load resolved it once at
// startup, so a symlink swapped in later cannot move it. A path that does not exist is passed
// on as missing; any other resolution error refuses it. root is opened before the checks and
// returned for openAll, so a component swapped after them cannot lead outside it; the caller
// closes the handle. It is nil when there are no attachments. Opening follows a symlink at root
// itself, so sameRoot then checks that the handle is still the root.
func confine(root string, args []string) (*os.Root, []*attachment, error) {
	if len(args) == 0 {
		return nil, nil, nil
	}
	if root == "" {
		return nil, nil, fmt.Errorf("attachments are disabled: set %s to a directory to allow them", config.EnvUploadRoot)
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, nil, fmt.Errorf("attachment root cannot be opened: %s (%s)", root, errnoText(err))
	}
	files, err := check(root, args)
	if err == nil {
		err = sameRoot(dir, root)
	}
	if err != nil {
		_ = dir.Close()
		return nil, nil, err
	}
	return dir, files, nil
}

// sameRoot refuses a handle that is not the directory at root now, without following a symlink
// there: one swapped in before the open and swapped back before the checks would otherwise
// leave the handle outside the root the checks passed against. A swap after this is harmless,
// as the handle no longer moves.
func sameRoot(dir *os.Root, root string) error {
	opened, err := dir.Stat(".")
	if err != nil {
		return fmt.Errorf("attachment root cannot be opened: %s (%s)", root, errnoText(err))
	}
	current, err := os.Lstat(root)
	if err != nil || !os.SameFile(opened, current) {
		return fmt.Errorf("attachment root changed while being checked: %s", root)
	}
	return nil
}

// check is confine's per-attachment resolution and containment test.
func check(root string, args []string) ([]*attachment, error) {
	files := make([]*attachment, 0, len(args))
	for _, arg := range args {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return nil, fmt.Errorf("attachment cannot be resolved: %s (%s)", arg, errnoText(err))
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if errors.Is(err, fs.ErrNotExist) {
			files = append(files, &attachment{arg: arg, abs: abs, missing: true})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("attachment cannot be resolved: %s (%s)", arg, errnoText(err))
		}
		rel, err := filepath.Rel(root, resolved)
		switch {
		case err == nil && rel == ".":
			return nil, fmt.Errorf("attachment is %s itself, not a file: %s", root, arg)
		case err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel):
			return nil, fmt.Errorf("attachment outside %s: %s (resolved to %s) — set %s to allow it",
				root, arg, resolved, config.EnvUploadRoot)
		}
		files = append(files, &attachment{arg: arg, abs: abs, real: resolved, rel: rel})
	}
	return files, nil
}

// errnoText is the reason of a path error without the operation and path it repeats.
func errnoText(err error) string {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err.Error()
	}
	return err.Error()
}

// openAll checks that every attachment exists and is a regular file and opens it through dir by
// its path relative to the root (the kernel refuses a walk that leaves dir). It then checks
// every size against the server's MaxFileSize, skipped when that can't be read.
func openAll(ctx context.Context, c *mattermost.Context, dir *os.Root, files []*attachment) error {
	for _, f := range files {
		if f.missing {
			return fmt.Errorf("attachment not found: %s (resolved to %s)", f.arg, f.abs)
		}
		// Stat before opening: opening a FIFO would block.
		fi, err := dir.Stat(f.rel)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("attachment not found: %s (resolved to %s)", f.arg, f.abs)
		}
		if err != nil {
			return fmt.Errorf("attachment cannot be read: %s (%s)", f.arg, errnoText(err))
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("attachment is not a regular file: %s (resolved to %s)", f.arg, f.real)
		}
		// O_NONBLOCK: a FIFO swapped in after the Stat must not block the open either; the fstat
		// below then refuses it. It does not affect reads of a regular file.
		file, err := dir.OpenFile(f.rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return fmt.Errorf("attachment cannot be read: %s (%s)", f.arg, errnoText(err))
		}
		f.file = file
		opened, err := file.Stat()
		if err != nil {
			return fmt.Errorf("attachment cannot be read: %s (%s)", f.arg, errnoText(err))
		}
		if !os.SameFile(fi, opened) || !opened.Mode().IsRegular() {
			return fmt.Errorf("attachment changed while being checked: %s (resolved to %s)", f.arg, f.real)
		}
		f.size = opened.Size()
	}
	if len(files) == 0 {
		return nil
	}
	limit, ok := c.MaxFileSize(ctx)
	if !ok {
		return nil
	}
	for _, f := range files {
		if f.size > limit {
			size, lim := sizePair(f.size, limit)
			return fmt.Errorf("attachment too large: %s is %s, over the server limit of %s", f.arg, size, lim)
		}
	}
	return nil
}

func closeAll(files []*attachment) {
	for _, f := range files {
		if f.file != nil {
			_ = f.file.Close()
		}
	}
}

// uploadAll uploads the opened attachments one by one and returns their file ids. A failure
// after earlier uploads succeeded names the orphaned ids.
func uploadAll(ctx context.Context, c *mattermost.Context, channelID string, files []*attachment) ([]string, error) {
	var ids []string
	for _, f := range files {
		// Send no more than the size that passed the checks: a file that grows afterward must
		// not bypass the size limit. The file is streamed, never held in memory.
		src := &fileReader{r: f.file, left: f.size}
		res, err := c.UploadFile(ctx, channelID, filepath.Base(f.abs), src, f.size)
		if readErr := src.failure(); readErr != nil {
			return nil, orphaned(ids, f.arg, fmt.Errorf("attachment cannot be read: %s (%s)", f.arg, errnoText(readErr)))
		}
		if err != nil {
			return nil, orphaned(ids, f.arg, mattermost.WrapErr("/api/v4/files", err))
		}
		for _, info := range res.FileInfos {
			ids = append(ids, info.Id)
		}
	}
	return ids, nil
}

// fileReader reads an attachment for upload and records why it could not supply the checked
// size: a read error, or io.ErrUnexpectedEOF when the file shrank. The HTTP transport may still
// read after the request returns, hence the lock.
type fileReader struct {
	mu   sync.Mutex
	r    io.Reader
	left int64
	err  error
}

func (r *fileReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.r.Read(p)
	r.left -= int64(n)
	if errors.Is(err, io.EOF) && r.left > 0 {
		err = io.ErrUnexpectedEOF
	}
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}
	return n, err
}

func (r *fileReader) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// orphaned returns err unchanged when nothing was uploaded; otherwise it names the uploaded ids
// left without a post.
func orphaned(ids []string, what string, err error) error {
	if len(ids) == 0 {
		return err
	}
	return fmt.Errorf("uploaded %d file(s), then %s failed — file ids %s %s; cause: %w",
		len(ids), what, strings.Join(ids, ", "), orphanNote, err)
}
