package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/qiyuehuajin/agentspool/internal/client"
	"github.com/qiyuehuajin/agentspool/internal/logstream"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type sandboxLogReader interface {
	logs(context.Context, string, string, string, int64) (logstream.Page, error)
}

func (c *sandboxHelperClient) logs(ctx context.Context, sandboxID, operationID, stream string, after int64) (logstream.Page, error) {
	var page logstream.Page
	query := url.Values{"stream": {stream}, "after": {strconv.FormatInt(after, 10)}}
	path := "http://sandboxd/v1/sandboxes/" + url.PathEscape(sandboxID) + "/operations/" + url.PathEscape(operationID) + "/logs?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return page, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return page, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return page, fmt.Errorf("helper log read returned %s", resp.Status)
	}
	err = json.NewDecoder(resp.Body).Decode(&page)
	return page, err
}
func (w *Worker) streamSandboxLogs(ctx context.Context, claim sandboxOperationClaim, executionDone <-chan struct{}) error {
	reader, ok := w.sandbox.(sandboxLogReader)
	if !ok {
		return nil
	}
	offsets := map[string]int64{"stdout": 0, "stderr": 0}
	for {
		finished := false
		select {
		case <-executionDone:
			finished = true
		default:
		}
		pending := false
		advanced := false
		for _, stream := range []string{"stdout", "stderr"} {
			requestCtx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
			page, err := reader.logs(requestCtx, claim.Sandbox.ID, claim.Operation.ID, stream, offsets[stream])
			cancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				if finished {
					return err
				}
				pending = true
				continue
			}
			if len(page.Data) == 0 {
				continue
			}
			body := map[string]any{"stream": stream, "offset": page.Offset, "data_base64": page.Data, "truncated": page.Truncated}
			requestCtx, cancel = context.WithTimeout(ctx, w.cfg.RequestTimeout)
			err = w.api.DoJSON(requestCtx, http.MethodPost, "/api/v1/sandbox-operations/"+claim.Operation.ID+"/logs", nil, body, nil, map[string]string{"X-AgentsPool-Sandbox-Operation-Token": claim.OperationToken})
			cancel()
			if err != nil {
				var apiErr *client.APIError
				if errors.As(err, &apiErr) && apiErr.StatusCode < 500 {
					return err
				}
				pending = true
				continue
			}
			advanced = true
			offsets[stream] = page.NextAfter
			if page.NextAfter < page.EndOffset {
				pending = true
			}
		}
		if finished && !pending {
			return nil
		}
		if advanced && pending {
			continue
		}
		if !sleepContext(ctx, 200*time.Millisecond) {
			return ctx.Err()
		}
	}
}
