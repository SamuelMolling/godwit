package cli

// The shapes below are what godwit's JSON output has to keep looking like, declared here rather than
// borrowed from internal/report so a renamed field on the producer fails these tests instead of moving with them.

type planJSON struct {
	Version    int64           `json:"version"`
	Name       string          `json:"name"`
	Repeatable bool            `json:"repeatable,omitempty"`
	Direction  string          `json:"direction"`
	Statements []statementJSON `json:"statements"`
}

type statementJSON struct {
	SQL     string       `json:"sql"`
	Mode    string       `json:"mode"`
	Phase   string       `json:"phase,omitempty"`
	Batch   *batchJSON   `json:"batch,omitempty"`
	Assert  *assertJSON  `json:"assert,omitempty"`
	Hazards []hazardJSON `json:"hazards"`
}

type assertJSON struct {
	Op    string `json:"op"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type batchJSON struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Size  int    `json:"size"`
	Pause string `json:"pause,omitempty"`
}

type hazardJSON struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	Recipe string `json:"recipe,omitempty"`
}

type livePlanJSON struct {
	planJSON
	Applied        bool     `json:"applied"`
	Phase          string   `json:"phase"`
	AlreadyApplied bool     `json:"already_applied,omitempty"`
	Effect         string   `json:"effect,omitempty"`
	Note           string   `json:"note,omitempty"`
	Directives     []string `json:"directives,omitempty"`
	Expanded       bool     `json:"expanded,omitempty"`
	Notes          []string `json:"notes,omitempty"`
	Withheld       bool     `json:"withheld,omitempty"`
	Skipped        bool     `json:"skipped,omitempty"`
}

type dryRunJSON struct {
	Target     string           `json:"target"`
	Rollout    string           `json:"rollout"`
	Validated  bool             `json:"validated"`
	PlanID     string           `json:"plan_id,omitempty"`
	PlanKey    string           `json:"plan_key,omitempty"`
	Observed   *planObservation `json:"observed,omitempty"`
	Drift      string           `json:"drift,omitempty"`
	Stored     *storedPlan      `json:"stored,omitempty"`
	Migrations []livePlanJSON   `json:"migrations"`
}

type planObservation struct {
	HistoryHash       string   `json:"history_hash"`
	SchemaFingerprint string   `json:"schema_fingerprint"`
	AppliedCount      int32    `json:"applied_count"`
	NewestApplied     int64    `json:"newest_applied"`
	At                string   `json:"at"`
	IgnoredTables     []string `json:"ignored_tables,omitempty"`
}

type storedPlan struct {
	State           string   `json:"state"`
	RunID           string   `json:"run_id,omitempty"`
	SupersededBy    string   `json:"superseded_by,omitempty"`
	CreatedBy       string   `json:"created_by"`
	CreatedAt       string   `json:"created_at"`
	Source          string   `json:"source,omitempty"`
	Acked           []string `json:"acknowledged_hazards,omitempty"`
	AllowOutOfOrder bool     `json:"allow_out_of_order,omitempty"`
}
