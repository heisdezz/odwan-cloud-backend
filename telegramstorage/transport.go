package telegramstorage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// TgNAS expects sendDocument to return a document. Telegram can instead return
// video/audio/animation metadata. Keep its acknowledged file ID in either case.
type documentTransport struct{ next http.RoundTripper }
type prefixedBody struct {
	io.Reader
	io.Closer
}

func (t documentTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	if !strings.HasSuffix(request.URL.Path, "/sendDocument") {
		return next.RoundTrip(request)
	}
	mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return next.RoundTrip(request)
	}
	// Prepend a form field while streaming the original multipart body unchanged.
	prefix := []byte("--" + params["boundary"] + "\r\nContent-Disposition: form-data; name=\"disable_content_type_detection\"\r\n\r\ntrue\r\n")
	cloned := request.Clone(request.Context())
	cloned.Body = &prefixedBody{Reader: io.MultiReader(bytes.NewReader(prefix), request.Body), Closer: request.Body}
	if request.ContentLength > 0 {
		cloned.ContentLength = request.ContentLength + int64(len(prefix))
	} else {
		cloned.ContentLength = -1
	}
	if request.GetBody != nil {
		cloned.GetBody = func() (io.ReadCloser, error) {
			body, err := request.GetBody()
			if err != nil {
				return nil, err
			}
			return &prefixedBody{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}, nil
		}
	}
	response, err := next.RoundTrip(cloned)
	if err != nil {
		return response, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response, nil
	}
	const maxResponse = 2 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	response.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read Telegram upload acknowledgement: %w", err)
	}
	if len(data) > maxResponse {
		return nil, fmt.Errorf("Telegram upload acknowledgement exceeds 2 MiB")
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) == nil && string(envelope["ok"]) == "true" {
		var result map[string]json.RawMessage
		if json.Unmarshal(envelope["result"], &result) == nil && !hasFileID(result["document"]) {
			for _, kind := range []string{"video", "audio", "animation"} {
				if hasFileID(result[kind]) {
					result["document"] = result[kind]
					envelope["result"], _ = json.Marshal(result)
					data, _ = json.Marshal(envelope)
					break
				}
			}
		}
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	response.ContentLength = int64(len(data))
	response.Header.Set("Content-Length", fmt.Sprint(len(data)))
	return response, nil
}

func hasFileID(data json.RawMessage) bool {
	var file struct {
		ID string `json:"file_id"`
	}
	return json.Unmarshal(data, &file) == nil && file.ID != ""
}
