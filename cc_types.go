package main

// Callcenter request types

// AgentAddRequest and AgentSetRequest: callers also send a `domain` field,
// which is ignored. An agent's tenant comes from its name or contact
// (agentOwner), never from the request.
type AgentAddRequest struct {
	Name string `json:"name"` // the FusionPBX agent uuid, or <name>@<domain>
	Type string `json:"type"` // callback or uuid-standby
}

type AgentSetRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
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
