package mattermost

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
)

// UploadFile uploads the first size bytes of data as filename to channelID with POST /files.
// It sends the multipart request Client4.UploadFile builds, but streams data instead of holding
// the file, and a copy of it, in memory. It goes through Client4's HTTP client, so errors are
// Client4's and WrapErr applies. data shorter than size fails the request.
func (c *Context) UploadFile(ctx context.Context, channelID, filename string, data io.Reader, size int64) (*model.FileUploadResponse, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("channel_id", channelID); err != nil {
		return nil, err
	}
	if _, err := mw.CreateFormFile("files", filename); err != nil {
		return nil, err
	}
	head := bytes.Clone(buf.Bytes())
	buf.Reset()
	if err := mw.Close(); err != nil {
		return nil, err
	}
	tail := buf.Bytes()

	body := io.MultiReader(bytes.NewReader(head), io.LimitReader(data, size), bytes.NewReader(tail))
	rq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.client.APIURL+"/files", body)
	if err != nil {
		return nil, err
	}
	rq.ContentLength = int64(len(head)) + size + int64(len(tail))
	rq.Header.Set("Content-Type", mw.FormDataContentType())
	rq.Header.Set(model.HeaderAuth, c.client.AuthType+" "+c.client.AuthToken)
	rp, err := c.client.HTTPClient.Do(rq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rp.Body.Close() }()
	if rp.StatusCode >= http.StatusMultipleChoices {
		return nil, model.AppErrorFromJSON(rp.Body)
	}
	res, _, err := model.DecodeJSONFromResponse[*model.FileUploadResponse](rp)
	return res, err
}
