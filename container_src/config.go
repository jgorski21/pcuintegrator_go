package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// EstimateMode decides what happens when ClickUp has no estimate but Productive does.
//
// Productive omits `initial_estimate` from the body when it is nil (parity with .NET),
// so a diff on that field can never be resolved by writing — the PATCH changes nothing
// and the next run sees the same diff. Forever. See README "wieczny churn".
type EstimateMode int

const (
	// EstimateIgnoreNil: no ClickUp estimate => estimate is not compared at all.
	// No writes, no churn, human-entered Productive values survive. Default.
	EstimateIgnoreNil EstimateMode = iota
	// EstimateClearNil: no ClickUp estimate => send explicit `"initial_estimate": null`.
	// Converges, but clears values people typed into Productive by hand.
	EstimateClearNil
)

// Config holds every tunable. All ClickUp/Productive ids are compiled-in defaults
// matching what the .NET app hardcoded across PCUIntegratorService.cs and
// MappingExtensions.cs; every one of them is overridable by env so the whole
// integration can be pointed at scratch lists for a safe write test.
type Config struct {
	// --- secrets (required, no defaults) ---
	ClickUpToken    string
	ProductiveToken string
	ProductiveOrgID string

	// --- endpoints ---
	ClickUpBaseURL    string
	ProductiveBaseURL string

	// --- ids ---
	ClickUpListID        string
	ProductiveProjectID  string
	ProductiveTaskListID string
	CFClickUpID          string // Productive custom field holding the ClickUp task id
	CFClickUpTags        string // Productive custom field holding ", "-joined tags
	StatusIDOpen         string // Productive workflow_status id for Open
	StatusIDDone         string // Productive workflow_status id for Done

	// --- mappings ---
	ClickUpDoneStatuses  map[string]bool // ClickUp status name => Done
	ProductiveDoneNames  map[string]bool // Productive workflow status name => Done
	EstimateFieldIDs     []string        // "Szybka wycena" drop_down field ids, in order
	EstimateLabelMinutes map[string]int  // drop_down label => minutes

	// --- behaviour ---
	IncludeClosed     bool
	MergeCustomFields bool
	EstimateMode      EstimateMode
	AllowReparent     bool

	// TitleMaxRunes is Productive's own limit on `title`. Exceeding it returns
	// 422 "Attribute is too long (maximum is 140 characters)". .NET never checked
	// the write status, so every ClickUp task with a longer title has silently
	// failed to sync since day one.
	//
	// The truncation happens when mapping ClickUp -> Task, NOT when building the
	// body: Productive stores the truncated title, so comparing an untruncated
	// ClickUp title against it would differ forever and PATCH on every run.
	TitleMaxRunes int

	// MaxSubtaskDepth is how deep Productive accepts nested subtasks. Exceeding it
	// returns 422 "invalid level of subtasks (data/attributes/parent_task)".
	// Anything deeper is flattened to a top-level task with a warning; children of
	// a flattened task start a fresh level, so as much structure as Productive
	// allows is preserved.
	MaxSubtaskDepth int

	// --- limits ---
	ProductiveRPS      float64 // sustained; documented ceiling is 4000/30min = 2.22
	ProductiveBurstRPS float64 // burst; documented ceiling is 100/10s = 10
	ClickUpRPS         float64 // documented 100/min per token on most plans
	MaxCreates         int
	MaxWrites          int
	MaxPages           int
	MaxRetries         int
	SyncTimeout        time.Duration // must stay below the 15min SIGTERM->SIGKILL window
	HTTPTimeout        time.Duration

	// --- process ---
	Port     string
	LogLevel slog.Level
}

// defaultEstimateLabels mirrors MappingExtensions.EstimateLabelToMinutes exactly,
// including the gaps: "12h - 16h" and "32h - 40h" are absent upstream and stay
// absent here. An unmapped label produces a warning, never an error.
func defaultEstimateLabels() map[string]int {
	return map[string]int{
		"< 30m":     20,
		"1h - 2h":   90,
		"3h - 12h":  600,
		"16h - 32h": 1500,
		"> 40h":     3900,
	}
}

func LoadConfig() (Config, error) {
	c := Config{
		ClickUpToken:    os.Getenv("CLICKUP_TOKEN"),
		ProductiveToken: os.Getenv("PRODUCTIVE_TOKEN"),
		ProductiveOrgID: os.Getenv("PRODUCTIVE_ORG_ID"),

		ClickUpBaseURL:    envStr("CLICKUP_BASE_URL", "https://api.clickup.com/api/v2/"),
		ProductiveBaseURL: envStr("PRODUCTIVE_BASE_URL", "https://api.productive.io/api/v2/"),

		ClickUpListID:        envStr("CLICKUP_LIST_ID", "900501332334"),
		ProductiveProjectID:  envStr("PRODUCTIVE_PROJECT_ID", "860646"),
		ProductiveTaskListID: envStr("PRODUCTIVE_TASK_LIST_ID", "2385788"),
		CFClickUpID:          envStr("PRODUCTIVE_CF_CLICKUP_ID", "242457"),
		CFClickUpTags:        envStr("PRODUCTIVE_CF_CLICKUP_TAGS", "242565"),
		StatusIDOpen:         envStr("PRODUCTIVE_STATUS_OPEN_ID", "161082"),
		StatusIDDone:         envStr("PRODUCTIVE_STATUS_DONE_ID", "161083"),

		ClickUpDoneStatuses: envSet("CLICKUP_DONE_STATUSES", "zawieszone,gotowe do wydania,wydane"),
		ProductiveDoneNames: envSet("PRODUCTIVE_DONE_STATUS_NAMES", "Closed"),
		EstimateFieldIDs: envList("CLICKUP_ESTIMATE_FIELD_IDS",
			"33afbaec-1fae-49be-9e9f-35eb789ef911,611a0462-acb0-4c72-addc-927e62b116d6"),

		IncludeClosed:     envBool("CLICKUP_INCLUDE_CLOSED", false),
		MergeCustomFields: envBool("MERGE_CUSTOM_FIELDS", true),
		AllowReparent:     envBool("ALLOW_REPARENT", false),
		TitleMaxRunes:     envInt("PRODUCTIVE_TITLE_MAX", 140),
		MaxSubtaskDepth:   envInt("PRODUCTIVE_MAX_SUBTASK_DEPTH", 5),

		ProductiveRPS:      envFloat("PRODUCTIVE_RPS", 1.0),
		ProductiveBurstRPS: envFloat("PRODUCTIVE_BURST_RPS", 8.0),
		ClickUpRPS:         envFloat("CLICKUP_RPS", 1.5),
		MaxCreates:         envInt("MAX_CREATES", 25),
		MaxWrites:          envInt("MAX_WRITES", 300),
		MaxPages:           envInt("MAX_PAGES", 200),
		MaxRetries:         envInt("MAX_RETRIES", 4),
		SyncTimeout:        envDur("SYNC_TIMEOUT", 10*time.Minute),
		HTTPTimeout:        envDur("HTTP_TIMEOUT", 60*time.Second),

		Port: envStr("PORT", "8080"),
	}

	labels, err := envLabelMinutes("CLICKUP_ESTIMATE_LABEL_MINUTES", defaultEstimateLabels())
	if err != nil {
		return c, err
	}
	c.EstimateLabelMinutes = labels

	switch mode := strings.ToLower(envStr("ESTIMATE_CLEAR_MODE", "ignore")); mode {
	case "ignore":
		c.EstimateMode = EstimateIgnoreNil
	case "null", "clear":
		c.EstimateMode = EstimateClearNil
	default:
		return c, fmt.Errorf("ESTIMATE_CLEAR_MODE: expected ignore|null, got %q", mode)
	}

	switch lvl := strings.ToLower(envStr("LOG_LEVEL", "info")); lvl {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn", "warning":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		return c, fmt.Errorf("LOG_LEVEL: expected debug|info|warn|error, got %q", lvl)
	}

	return c, c.validate()
}

// validate fails fast and loudly. A blank PRODUCTIVE_TOKEN is the single most
// dangerous misconfiguration in this program: it makes every read 401, and a read
// that fails without aborting looks exactly like "Productive is empty" — which
// would POST a duplicate of every ClickUp task. fetchProductive() aborts on
// non-2xx for that reason, and this check stops it one step earlier.
func (c Config) validate() error {
	var missing []string
	for _, kv := range []struct{ name, val string }{
		{"CLICKUP_TOKEN", c.ClickUpToken},
		{"PRODUCTIVE_TOKEN", c.ProductiveToken},
		{"PRODUCTIVE_ORG_ID", c.ProductiveOrgID},
		{"CLICKUP_LIST_ID", c.ClickUpListID},
		{"PRODUCTIVE_PROJECT_ID", c.ProductiveProjectID},
		{"PRODUCTIVE_TASK_LIST_ID", c.ProductiveTaskListID},
		{"PRODUCTIVE_CF_CLICKUP_ID", c.CFClickUpID},
		{"PRODUCTIVE_CF_CLICKUP_TAGS", c.CFClickUpTags},
		{"PRODUCTIVE_STATUS_OPEN_ID", c.StatusIDOpen},
		{"PRODUCTIVE_STATUS_DONE_ID", c.StatusIDDone},
	} {
		if strings.TrimSpace(kv.val) == "" {
			missing = append(missing, kv.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}

	if c.ProductiveRPS <= 0 || c.ProductiveBurstRPS <= 0 || c.ClickUpRPS <= 0 {
		return fmt.Errorf("rate limits must be > 0 (productive=%v burst=%v clickup=%v)",
			c.ProductiveRPS, c.ProductiveBurstRPS, c.ClickUpRPS)
	}
	// 4000 requests / 30 min = 2.222 rps is the documented sustained ceiling and it
	// is shared by the whole Productive organization, not just this integration.
	if c.ProductiveRPS > 2.2 {
		return fmt.Errorf("PRODUCTIVE_RPS=%v exceeds the documented sustained ceiling (4000/30min = 2.22 rps, org-wide)", c.ProductiveRPS)
	}
	if c.ProductiveBurstRPS > 10 {
		return fmt.Errorf("PRODUCTIVE_BURST_RPS=%v exceeds the documented burst ceiling (100/10s = 10 rps)", c.ProductiveBurstRPS)
	}
	// Cloudflare sends SIGTERM then SIGKILL 15 minutes later. A deadline above that
	// guarantees being killed mid-write in exactly the case the deadline exists for.
	if c.SyncTimeout <= 0 || c.SyncTimeout > 12*time.Minute {
		return fmt.Errorf("SYNC_TIMEOUT=%v must be in (0, 12m] — Cloudflare SIGKILLs 15m after SIGTERM", c.SyncTimeout)
	}
	if c.MaxCreates < 0 || c.MaxWrites < 0 {
		return fmt.Errorf("MAX_CREATES and MAX_WRITES must be >= 0")
	}
	if c.MaxPages <= 0 {
		return fmt.Errorf("MAX_PAGES must be > 0")
	}
	if c.TitleMaxRunes <= 0 {
		return fmt.Errorf("PRODUCTIVE_TITLE_MAX must be > 0")
	}
	if c.MaxSubtaskDepth < 0 {
		return fmt.Errorf("PRODUCTIVE_MAX_SUBTASK_DEPTH must be >= 0 (0 disables subtasks entirely)")
	}
	if len(c.EstimateLabelMinutes) == 0 {
		return fmt.Errorf("CLICKUP_ESTIMATE_LABEL_MINUTES resolved to an empty map")
	}
	return nil
}

// statusIDFor maps the internal enum to the Productive workflow_status id written
// on POST/PATCH. Parity with MappingExtensions.StatusDictionary.
func (c Config) statusIDFor(s Status) string {
	if s == StatusDone {
		return c.StatusIDDone
	}
	return c.StatusIDOpen
}

// --- env helpers ---

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return def
	}
	return f
}

func envDur(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return d
}

// envList splits on "," and trims each element. Labels and status names contain
// spaces but never commas, so this is sufficient.
func envList(key, def string) []string {
	raw := envStr(key, def)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envSet(key, def string) map[string]bool {
	out := map[string]bool{}
	for _, v := range envList(key, def) {
		out[v] = true
	}
	return out
}

// envLabelMinutes parses a JSON object of label => minutes. JSON rather than a
// delimited string because the labels contain "<", ">", "-" and spaces.
func envLabelMinutes(key string, def map[string]int) (map[string]int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def, nil
	}
	var m map[string]int
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("%s: parsed to an empty map", key)
	}
	return m, nil
}
