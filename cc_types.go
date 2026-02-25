package main

// Callcenter request types

type AgentAddRequest struct {
	Name   string `json:"name"`   // UUID
	Type   string `json:"type"`   // callback or uuid-standby
	Domain string `json:"domain"` // for auth validation
}

type AgentSetRequest struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Domain string `json:"domain"` // for auth validation
}

type AgentDelRequest struct {
	Domain string `json:"domain"` // for auth validation
}

type TierAddRequest struct {
	Queue    string `json:"queue"`
	Agent    string `json:"agent"`
	Level    string `json:"level,omitempty"`
	Position string `json:"position,omitempty"`
}

type TierDelRequest struct {
	Queue string `json:"queue"`
	Agent string `json:"agent"`
}

type TierSetRequest struct {
	Queue string `json:"queue"`
	Agent string `json:"agent"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Callcenter response types

type CCListResponse struct {
	Status   string              `json:"status"`
	RowCount int                 `json:"row_count"`
	Rows     []map[string]string `json:"rows"`
}

type CCCountResponse struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}

// Validation maps for allowed set keys

var validAgentSetKeys = map[string]bool{
	"status":           true,
	"state":            true,
	"contact":          true,
	"type":             true,
	"max_no_answer":    true,
	"wrap_up_time":     true,
	"reject_delay_time": true,
	"busy_delay_time":  true,
	"ready_time":       true,
}

var validTierSetKeys = map[string]bool{
	"state":    true,
	"level":    true,
	"position": true,
}
