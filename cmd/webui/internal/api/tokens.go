package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/noodle05/ai-agents/cmd/webui/internal/templates"
	"github.com/noodle05/ai-agents/tasklib"
)

type tokensResource struct {
	tokens   tasklib.TokenLedger
	sysOps   tasklib.SystemOps
	renderer *templates.Renderer
}

func (tr *tokensResource) globalTokens(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	global, _ := tr.tokens.GetTokenStats(ctx, tasklib.StatsTotalKey())
	taskCount, _ := tr.tokens.GetTokenStatsTaskCount(ctx, tasklib.StatsTotalKey())

	workers := map[string]tasklib.TokenStats{}

	// Discover agent types dynamically from stats:total_tokens:* keys
	keys, err := tr.sysOps.ScanKeys(ctx, "stats:total_tokens:*", 100)
	if err != nil {
		slog.Warn("tokens: ScanKeys failed", "error", err)
	}
	for _, key := range keys {
		// Parse agent type from key: "stats:total_tokens:<agent_type>"
		// Key format: stats:total_tokens:<agent_type> (3 parts)
		if !strings.HasPrefix(key, "stats:total_tokens:") {
			continue
		}
		agentType := strings.TrimPrefix(key, "stats:total_tokens:")
		if agentType == "" {
			continue
		}
		s, err := tr.tokens.GetTokenStats(ctx, tasklib.StatsAgentKey(agentType))
		if err != nil || s == nil || !s.HasAny() {
			continue
		}
		workers[agentType] = *s
	}

	if IsHTMX(r) {
		vm := &templates.TokenStatsView{}
		if global != nil && global.HasAny() {
			vm.TotalIn = tasklib.FormatTokenCount(global.InputTokens)
			vm.TotalOut = tasklib.FormatTokenCount(global.OutputTokens)
			vm.TaskCount = taskCount
			for agentType, s := range workers {
				vm.Rows = append(vm.Rows, templates.TokenStatsRow{
					Agent:  agentType,
					Input:  tasklib.FormatTokenCount(s.InputTokens),
					Output: tasklib.FormatTokenCount(s.OutputTokens),
				})
			}
		}
		Partial(w, tr.renderer, "token-stats", vm)
	} else {
		total := map[string]interface{}{}
		if global != nil && global.HasAny() {
			total = map[string]interface{}{
				"input_tokens":  global.InputTokens,
				"output_tokens": global.OutputTokens,
				"cache_read":    global.CacheReadTokens,
				"cache_write":   global.CacheWriteTokens,
				"reasoning":     global.ReasoningTokens,
			}
		}
		Respond(w, r, http.StatusOK, map[string]interface{}{
			"total":      total,
			"task_count": taskCount,
			"workers":    workers,
		})
	}
}
