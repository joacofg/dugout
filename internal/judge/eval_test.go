package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// evalPair is one fixed pair of the eval and whether it is the same task.
type evalPair struct {
	A, B      string
	Duplicate bool
	Note      string
}

func loadPairs(t *testing.T) []evalPair {
	t.Helper()
	b, err := os.ReadFile("testdata/pairs.json")
	if err != nil {
		t.Fatal(err)
	}
	var pairs []evalPair
	if err := json.Unmarshal(b, &pairs); err != nil {
		t.Fatal(err)
	}
	return pairs
}

// TestPairsWellFormed keeps the eval's set honest: both kinds, no pair twice,
// every intent within the 280 runes an agent may declare.
func TestPairsWellFormed(t *testing.T) {
	seen := map[[2]string]bool{}
	dups := 0
	for _, p := range loadPairs(t) {
		a, b := Order(p.A, p.B)
		if seen[[2]string{a, b}] || a == b || len([]rune(a)) > 280 || len([]rune(b)) > 280 {
			t.Errorf("bad pair %+v", p)
		}
		seen[[2]string{a, b}] = true
		if p.Duplicate {
			dups++
		}
	}
	if dups < 10 || len(seen)-dups < 10 {
		t.Fatalf("%d duplicates and %d not: want at least 10 of each", dups, len(seen)-dups)
	}
}

// evalBudget stops the eval if it ever costs more than this, in dollars.
const evalBudget = 1.0

// cliJudge asks the same model, with the same system prompt, user turn and
// answer schema as Anthropic builds, through a logged-in Claude Code CLI
// (`claude -p`), for an eval where no API key is at hand. It is test-only:
// the shipped judge always calls the API. The CLI wraps the request in its
// own (it adds a little context and turns the schema into a tool), so its
// tokens and cost are close to the API path's, not the same.
type cliJudge struct {
	model string
	dir   string // an empty working directory, so no project context loads
}

func (c cliJudge) Name() string { return "claude-cli " + c.model }

func (c cliJudge) Judge(ctx context.Context, a, b string) (Verdict, error) {
	a, b = Order(a, b)
	sch, _ := json.Marshal(schema)
	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", c.model, "--output-format", "json", "--system-prompt", system,
		"--json-schema", string(sch), "--effort", "low", "--tools", "", "--strict-mcp-config", "--no-session-persistence",
		"--setting-sources", "", Prompt(a, b))
	cmd.Dir = c.dir
	cmd.Stdin = nil
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return Verdict{}, fmt.Errorf("claude -p: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var res struct {
		IsError          bool            `json:"is_error"`
		Subtype          string          `json:"subtype"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		TotalCostUSD     float64         `json:"total_cost_usd"`
		Usage            struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return Verdict{}, fmt.Errorf("claude -p answered no JSON: %.200q", out)
	}
	u := Usage{InputTokens: res.Usage.InputTokens + res.Usage.CacheCreationInputTokens + res.Usage.CacheReadInputTokens,
		OutputTokens: res.Usage.OutputTokens, USD: res.TotalCostUSD, Priced: true}
	if res.IsError {
		return Verdict{Usage: u}, fmt.Errorf("claude -p: %s: %.200q", res.Subtype, res.Result)
	}
	text := string(res.StructuredOutput)
	if len(res.StructuredOutput) == 0 || text == "null" {
		text = res.Result
	}
	v, err := Parse(text)
	v.Usage = u
	return v, err
}

// TestEval runs the fixed pairs through the real model, once each, and
// reports false positives, false negatives and what it cost. It runs only
// with DUGOUT_JUDGE_EVAL=1, since it spends money: through the API with
// ANTHROPIC_API_KEY set, else through the logged-in Claude Code CLI (cliJudge).
// DUGOUT_JUDGE_MODEL picks the model.
func TestEval(t *testing.T) {
	if os.Getenv("DUGOUT_JUDGE_EVAL") != "1" {
		t.Skip("set DUGOUT_JUDGE_EVAL=1 (and ANTHROPIC_API_KEY, or a logged-in claude CLI) to run the eval against the real model")
	}
	model := os.Getenv(EnvModel)
	if model == "" {
		model = DefaultModel
	}
	var j Judge
	if key := os.Getenv(EnvAPIKey); key != "" {
		j = NewAnthropic(key, model)
	} else if _, err := exec.LookPath("claude"); err == nil {
		j = cliJudge{model: model, dir: t.TempDir()}
	} else {
		t.Fatal(errors.New("neither " + EnvAPIKey + " nor the claude CLI is available"))
	}
	var fp, fn, errs int
	var total Usage
	var report strings.Builder
	start := time.Now()
	for i, p := range loadPairs(t) {
		if total.USD > evalBudget {
			t.Fatalf("over the eval's budget of $%.2f after %d pairs", evalBudget, i)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		v, err := j.Judge(ctx, p.A, p.B)
		cancel()
		total.InputTokens += v.Usage.InputTokens
		total.OutputTokens += v.Usage.OutputTokens
		total.USD += v.Usage.USD
		mark := "ok"
		switch {
		case err != nil:
			errs++
			mark = "ERROR " + err.Error()
		case v.Duplicate && !p.Duplicate:
			fp++
			mark = "FALSE POSITIVE"
		case !v.Duplicate && p.Duplicate:
			fn++
			mark = "FALSE NEGATIVE"
		}
		fmt.Fprintf(&report, "%2d %-14s want=%-5v got=%-5v %q | %q | %s\n", i+1, mark, p.Duplicate, v.Duplicate, p.A, p.B, v.Reason)
	}
	pairs := loadPairs(t)
	dups := 0
	for _, p := range pairs {
		if p.Duplicate {
			dups++
		}
	}
	summary := fmt.Sprintf("judge %s: %d pairs (%d duplicates, %d not): %d false positives, %d false negatives, %d errors; %d input and %d output tokens, $%.5f ($%.6f per comparison), %s",
		j.Name(), len(pairs), dups, len(pairs)-dups, fp, fn, errs, total.InputTokens, total.OutputTokens, total.USD, total.USD/float64(len(pairs)),
		time.Since(start).Round(time.Second))
	t.Log("\n" + report.String() + summary)
	if out := os.Getenv("DUGOUT_JUDGE_EVAL_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(report.String()+summary+"\n"), 0o644); err != nil {
			t.Error(err)
		}
	}
	if errs > 0 {
		t.Fatalf("%d comparisons failed", errs)
	}
}
