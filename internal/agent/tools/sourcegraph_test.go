package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestFormatSourcegraphResults(t *testing.T) {
	t.Parallel()

	result := map[string]any{
		"data": map[string]any{
			"search": map[string]any{
				"results": map[string]any{
					"matchCount":  float64(2),
					"resultCount": float64(1),
					"limitHit":    true,
					"results": []any{
						map[string]any{
							"__typename": "FileMatch",
							"repository": map[string]any{"name": "owner/repo"},
							"file": map[string]any{
								"path":    "main.go",
								"url":     "https://example.com/owner/repo/main.go",
								"content": "package main\n\nfunc main() {\n\tprintln(\"match\")\n}\n",
							},
							// Sourcegraph numbers lines from 0: the
							// println line, the fourth, is line 3.
							"lineMatches": []any{
								map[string]any{
									"lineNumber": float64(3),
									"preview":    "\tprintln(\"match\")",
								},
							},
						},
					},
				},
			},
		},
	}

	got, err := formatSourcegraphResults(result, 1, 10)
	require.NoError(t, err)
	require.Contains(t, got, "# Sourcegraph Search Results")
	require.Contains(t, got, "Found 2 matches across 1 results")
	require.Contains(t, got, "(Result limit reached, try a more specific query)")
	require.Contains(t, got, "## Result 1: owner/repo/main.go")
	require.Contains(t, got, "URL: https://example.com/owner/repo/main.go")
	require.Contains(t, got, "3| func main() {")
	require.Contains(t, got, "4|  \tprintln(\"match\")")
	require.Contains(t, got, "5| }")
	require.Equal(t, 1, strings.Count(got, "println"), "the matched line is shown once")
}

func TestFormatSourcegraphResultsRespectsCount(t *testing.T) {
	t.Parallel()

	result := map[string]any{
		"data": map[string]any{
			"search": map[string]any{
				"results": map[string]any{
					"results": []any{
						map[string]any{
							"__typename": "FileMatch",
							"repository": map[string]any{"name": "owner/repo"},
							"file":       map[string]any{"path": "first.go"},
						},
						map[string]any{
							"__typename": "FileMatch",
							"repository": map[string]any{"name": "owner/repo"},
							"file":       map[string]any{"path": "second.go"},
						},
					},
				},
			},
		},
	}

	got, err := formatSourcegraphResults(result, 1, 1)
	require.NoError(t, err)
	require.Contains(t, got, "owner/repo/first.go")
	require.NotContains(t, got, "owner/repo/second.go")
}

func TestFormatSourcegraphResultsErrorsAndNoResults(t *testing.T) {
	t.Parallel()

	errorResult := map[string]any{
		"errors": []any{
			map[string]any{"message": "bad query"},
			map[string]any{"message": "timeout"},
		},
	}
	got, err := formatSourcegraphResults(errorResult, 1, 10)
	require.NoError(t, err)
	require.Equal(t, "## Sourcegraph API Error\n\n- bad query\n- timeout\n", got)

	noResult := map[string]any{
		"data": map[string]any{
			"search": map[string]any{
				"results": map[string]any{
					"matchCount":  float64(0),
					"resultCount": float64(0),
					"results":     []any{},
				},
			},
		},
	}
	got, err = formatSourcegraphResults(noResult, 1, 10)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(got, "No results found. Try a different query.\n"))
}

// sourcegraphStub stands in for Sourcegraph's API: it records the query
// and answers with no results after delay, or not at all once the
// request's context ends.
type sourcegraphStub struct {
	delay time.Duration
	mu    sync.Mutex
	query string
}

func (s *sourcegraphStub) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var sent struct {
		Variables struct {
			Query string `json:"query"`
		} `json:"variables"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.query = sent.Variables.Query
	s.mu.Unlock()
	select {
	case <-time.After(s.delay):
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"data":{"search":{"results":{"matchCount":0,"resultCount":0,"results":[]}}}}`)),
	}, nil
}

func (s *sourcegraphStub) sent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.query
}

// runSourcegraph runs the tool, with client, on input.
func runSourcegraph(t *testing.T, client *http.Client, input string) fantasy.ToolResponse {
	t.Helper()
	resp, err := NewSourcegraphTool(client).Run(context.Background(), fantasy.ToolCall{ID: "call-1", Name: SourcegraphToolName, Input: input})
	require.NoError(t, err)
	return resp
}

// An agent on the AI box asked for timeouts of 60, 90 and 119 seconds,
// and every search was cut off at 30 by its client, as Sourcegraph took
// 32 to 80 seconds to answer. The search's own timeout bounds it now,
// whatever the client's.
func TestSourcegraphWaitsAsLongAsItsTimeout(t *testing.T) {
	t.Parallel()

	stub := &sourcegraphStub{delay: 300 * time.Millisecond}
	resp := runSourcegraph(t, &http.Client{Timeout: 100 * time.Millisecond, Transport: stub}, `{"query":"needle","timeout":5}`)
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "No results found")
}

// Sourcegraph is given three quarters of the tool's wait as its own
// timeout: filter, so a slow search comes back with what it found; a
// query with its own timeout: keeps it.
func TestSourcegraphGivesTheSearchItsOwnTimeout(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		`{"query":"needle"}`:                     "needle timeout:45s",
		`{"query":"needle","timeout":120}`:       "needle timeout:90s",
		`{"query":"needle","timeout":500}`:       "needle timeout:90s",
		`{"query":"needle","timeout":1}`:         "needle timeout:1s",
		`{"query":"needle TIMEOUT:5s lang:go"}`:  "needle TIMEOUT:5s lang:go",
		`{"query":"repo:x needle","timeout":30}`: "repo:x needle timeout:22s",
	} {
		stub := &sourcegraphStub{}
		resp := runSourcegraph(t, &http.Client{Transport: stub}, input)
		require.False(t, resp.IsError, resp.Content)
		require.Equal(t, want, stub.sent(), input)
	}
}

// A search Sourcegraph does not answer in time says so, and how to make
// it faster.
func TestSourcegraphSaysItDidNotAnswerInTime(t *testing.T) {
	t.Parallel()

	resp := runSourcegraph(t, &http.Client{Transport: &sourcegraphStub{delay: time.Minute}}, `{"query":"needle","timeout":1}`)
	require.True(t, resp.IsError)
	require.Equal(t, "Sourcegraph did not answer within 1 seconds. A literal search over every repository is slow: "+
		"narrow it with repo:, lang: or file:, or give a longer timeout (at most 120).", resp.Content)
}
