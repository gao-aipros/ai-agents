package api

import (
	"net/http"

	"github.com/noodle05/ai-agents/cmd/webui/internal/templates"
	"github.com/noodle05/ai-agents/tasklib"
)

type tokensResource struct {
	tokens   tasklib.TokenLedger
	renderer *templates.Renderer
}

func (tr *tokensResource) globalTokens(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	global, _ := tr.tokens.GetTokenStats(ctx, tasklib.StatsTotalKey())
	taskCount, _ := tr.tokens.GetTokenStatsTaskCount(ctx, tasklib.StatsTotalKey())

	workers := map[string]tasklib.TokenStats{}

	// Master is not in tasklib.WorkerTypes — query explicitly.
	if s, err := tr.tokens.GetTokenStats(ctx, tasklib.StatsWorkerKey("master")); err == nil && s != nil && s.HasAny() {
		workers["master"] = *s
	}

	for _, wt := range tasklib.WorkerTypes {
		s, err := tr.tokens.GetTokenStats(ctx, tasklib.StatsWorkerKey(wt))
		if err != nil || s == nil || !s.HasAny() {
			continue
		}
		workers[wt] = *s
	}

	if IsHTMX(r) {
		vm := &templates.TokenStatsView{}
		if global != nil && global.HasAny() {
			vm.TotalIn = tasklib.FormatTokenCount(global.InputTokens)
			vm.TotalOut = tasklib.FormatTokenCount(global.OutputTokens)
			vm.TaskCount = taskCount
			if s, ok := workers["master"]; ok {
				vm.Rows = append(vm.Rows, templates.TokenStatsRow{
					Agent:  "master",
					Input:  tasklib.FormatTokenCount(s.InputTokens),
					Output: tasklib.FormatTokenCount(s.OutputTokens),
				})
			}
			for _, wt := range tasklib.WorkerTypes {
				if s, ok := workers[wt]; ok {
					vm.Rows = append(vm.Rows, templates.TokenStatsRow{
						Agent:  wt,
						Input:  tasklib.FormatTokenCount(s.InputTokens),
						Output: tasklib.FormatTokenCount(s.OutputTokens),
					})
				}
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
