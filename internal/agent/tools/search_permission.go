package tools

import (
	"context"
	"fmt"
	"path/filepath"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/permission"
)

// askToSearch asks before a search of dir outside the working directory,
// as view and ls ask before reading there, and as Claude Code asks before
// its Grep and Glob read there: dir as written, or where it leads when it
// is a link (or runs through one), since a search given a link searches
// what the link leads to. stop is set when the search must not go on,
// with resp the answer to give when the user said no.
func askToSearch(ctx context.Context, permissions permission.Service, call fantasy.ToolCall, toolName, dir, workingDir string, params any) (resp fantasy.ToolResponse, stop bool, err error) {
	outside, real := fsext.LeadsOut(workingDir, dir)
	if !outside {
		return fantasy.ToolResponse{}, false, nil
	}
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, true, fmt.Errorf("session ID is required for searching outside the working directory")
	}
	granted, err := permissions.Request(ctx, permission.CreatePermissionRequest{
		SessionID:   sessionID,
		Path:        real,
		ToolCallID:  call.ID,
		ToolName:    toolName,
		Action:      "search",
		Description: outsideRequest("Search", workingDir, dir, real),
		Params:      params,
	})
	if err != nil {
		return fantasy.ToolResponse{}, true, err
	}
	if !granted {
		return NewPermissionDeniedResponse(), true, nil
	}
	return fantasy.ToolResponse{}, false, nil
}

// outsideRequest is the description of a request to read outside the
// working directory: what is asked (Read file, List directory, Search) and
// where; when a link in the working directory leads there, where it leads
// and the link, so the question shows what the read would really get.
func outsideRequest(what, workingDir, path, real string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if real != path && fsext.HasPrefix(path, workingDir) {
		return fmt.Sprintf("%s outside working directory: %s (the link %s leads there)", what, real, path)
	}
	return fmt.Sprintf("%s outside working directory: %s", what, path)
}
