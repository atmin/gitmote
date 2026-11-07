// Package scaleway is a thin client for Scaleway Serverless Jobs, the CI runner
// substrate (docs/architecture/ci.md). A job definition is created once, out
// of band. The app injects per-run env at start, so one definition serves all
// repos. The client is fire-and-forget. The caller triggers a run and never
// waits on it. The runner reports back over the internal API.
package scaleway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// triggerTimeout bounds the single start POST. The job itself runs
// asynchronously on Scaleway, well beyond this timeout.
const triggerTimeout = 10 * time.Second

// JobsClient starts runs of one Scaleway Serverless Job definition.
type JobsClient struct {
	secretKey       string
	region          string
	jobDefinitionID string
	httpClient      *http.Client
}

// NewJobsClient returns a client for the given job definition. When
// jobDefinitionID is empty (local dev or tests), Trigger does nothing.
func NewJobsClient(secretKey, region, jobDefinitionID string) *JobsClient {
	return &JobsClient{
		secretKey:       secretKey,
		region:          region,
		jobDefinitionID: jobDefinitionID,
		httpClient:      &http.Client{Timeout: triggerTimeout},
	}
}

// Trigger starts one job run with env injected as its environment variables.
// When the client has no job definition configured, it does nothing and
// returns nil. A response status >= 300 is an error, with the response body
// attached.
func (c *JobsClient) Trigger(ctx context.Context, env map[string]string) error {
	if c.jobDefinitionID == "" {
		return nil
	}

	url := fmt.Sprintf(
		"https://api.scaleway.com/serverless-jobs/v1alpha2/regions/%s/job-definitions/%s/start",
		c.region, c.jobDefinitionID,
	)
	body, err := json.Marshal(map[string]any{"environment_variables": env})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth-Token", c.secretKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("scaleway: start job definition %s returned %d: %s",
			c.jobDefinitionID, resp.StatusCode, msg)
	}
	return nil
}
