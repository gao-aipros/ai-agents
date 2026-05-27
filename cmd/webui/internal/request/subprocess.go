package request

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/noodle05/ai-agents/tasklib"
)

func (h *Handler) runSubprocess(ctx context.Context, cancel context.CancelFunc, threadID, requestID string, args []string) {
	defer h.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			h.logger.Warn(fmt.Sprintf("panic in subprocess goroutine thread=%s: %v", threadID, r))
			h.writeErrorMessage(ctx, threadID, fmt.Sprintf("internal error: panic in handler: %v", r))
		}
	}()

	// Cleanup: release semaphore, request lock, remove cancel registration
	defer func() {
		<-h.sem
		cleanCtx, cleanCancel := cleanupCtx()
		defer cleanCancel()
		h.requests.ReleaseRequestLock(cleanCtx, threadID)
		h.history.UpdateThreadLastActivity(cleanCtx, threadID)

		h.mu.Lock()
		delete(h.cancels, threadID)
		h.mu.Unlock()

		if h.cfg.TestNotify != nil {
			h.cfg.TestNotify <- threadID
		}
	}()

	h.logger.Info(fmt.Sprintf("thread=%s request=%s spawning claude -p", threadID, requestID))

	cmd := exec.CommandContext(ctx, h.cfg.ClaudePath, args...)
	cmd.Dir = filepath.Join(h.cfg.Paths.WorkspaceDir, threadID)
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "THREAD=") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, "THREAD="+threadID)
	if err := os.MkdirAll(cmd.Dir, 0755); err != nil {
		h.writeErrorMessage(ctx, threadID, fmt.Sprintf("failed to create workspace dir: %v", err))
		return
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		h.writeErrorMessage(ctx, threadID, fmt.Sprintf("failed to create stdout pipe: %v", err))
		return
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		h.writeErrorMessage(ctx, threadID, fmt.Sprintf("failed to start claude: %v", err))
		return
	}

	if h.isCancelled(ctx) {
		return
	}

	// Read stdout. In "text" mode we accumulate plain text and write each
	// line as a "plan" message. In "stream-json" mode we dispatch JSON messages
	// (rollback path) and detect completion via the "result" message.
	var fullStdout strings.Builder
	var streamCompleted bool
	var masterStats tasklib.TokenStats
	if h.cfg.OutputFormat == "stream-json" {
		streamCompleted, masterStats = h.processStreamJSON(ctx, threadID, stdout)

		// Persist master token stats to thread and global counters
		persistCtx, persistCancel := cleanupCtx()
		h.sysOps.PersistMasterTokenStats(persistCtx, threadID, masterStats)
		persistCancel()
	} else {
		h.processPlainText(ctx, threadID, stdout, &fullStdout)
	}

	// Wait for the process to exit (or kill it if timeout/cancelled).
	var waitErr error
	if ctx.Err() != nil {
		done := make(chan struct{})
		go func() {
			waitErr = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Signal(syscall.SIGKILL)
			<-done
		}
	} else {
		waitErr = cmd.Wait()
	}

	// Determine completion.
	// Text mode: use exit code + accumulated stdout.
	// Stream-json mode: rely on processStreamJSON's result message;
	// if none arrived (crash), fall back to stderr/exit code.
	if h.cfg.OutputFormat == "stream-json" {
		if !streamCompleted {
			// Stream-json process exited without emitting a result message.
			var errContent string
			if ctx.Err() == context.DeadlineExceeded {
				errContent = fmt.Sprintf("Master agent timed out after %s", h.cfg.RequestTimeout)
			} else if ctx.Err() == context.Canceled {
				errContent = "Request cancelled"
			} else {
				errContent = strings.TrimSpace(stderrBuf.String())
				if errContent == "" && waitErr != nil {
					errContent = fmt.Sprintf("claude exited with error: %v", waitErr)
				} else if errContent == "" {
					errContent = "claude exited without emitting a result message"
				}
			}
			h.logger.Info(fmt.Sprintf("thread=%s claude stderr: %s", threadID, errContent))
			h.writeErrorMessage(ctx, threadID, errContent)
		}
	} else {
		// Text mode (and any unrecognized mode, including "" default):
		// use exit code + accumulated stdout.
		if ctx.Err() == context.DeadlineExceeded {
			h.writeErrorMessage(ctx, threadID, fmt.Sprintf("Master agent timed out after %s", h.cfg.RequestTimeout))
		} else if ctx.Err() == context.Canceled {
			h.writeErrorMessage(ctx, threadID, "Request cancelled")
		} else if waitErr != nil {
			errContent := strings.TrimSpace(stderrBuf.String())
			if errContent == "" {
				errContent = fmt.Sprintf("claude exited with error: %v", waitErr)
			}
			h.logger.Info(fmt.Sprintf("thread=%s claude stderr: %s", threadID, errContent))
			h.writeErrorMessage(ctx, threadID, errContent)
		} else {
			result := strings.TrimSpace(fullStdout.String())
			if result == "" {
				errDetail := strings.TrimSpace(stderrBuf.String())
				errMsg := "claude exited without producing output"
				if errDetail != "" {
					errMsg = fmt.Sprintf("claude exited without producing output (stderr: %s)", errDetail)
					h.logger.Info(fmt.Sprintf("thread=%s claude stderr: %s", threadID, errDetail))
				}
				h.writeErrorMessage(ctx, threadID, errMsg)
			} else {
				h.writeResponseMessage(ctx, threadID, result)
			}
		}
	}
}

// sessionFileExists checks whether a session file for the given UUID exists
// under the Claude sessions directory.
func sessionFileExists(sessionsDir, sessionID string) bool {
	projectsDir := filepath.Join(sessionsDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return false
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionFile := filepath.Join(projectsDir, entry.Name(), sessionID+".jsonl")
		if _, err := os.Stat(sessionFile); err == nil {
			return true
		}
	}
	return false
}
