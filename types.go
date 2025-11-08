package main

// Request/Response Structures
type SuccessResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type ErrorResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type HangupRequest struct {
	Cause string `json:"cause"`
}

type TransferRequest struct {
	Destination string `json:"destination"`        // Required: destination extension
	Dialplan    string `json:"dialplan,omitempty"` // Optional: dialplan type (e.g., "XML")
	Context     string `json:"context,omitempty"`  // Optional: dialplan context
	Leg         string `json:"leg,omitempty"`      // Optional: "aleg" (default), "bleg", or "both"
}

type BridgeRequest struct {
	UUIDA string `json:"uuid_a"`
	UUIDB string `json:"uuid_b"`
}

type HoldRequest struct {
	Action string `json:"action"`
}

type RecordRequest struct {
	Action   string `json:"action"`
	Filename string `json:"filename,omitempty"`
}

type DTMFRequest struct {
	Digits   string `json:"digits"`
	Duration int    `json:"duration,omitempty"`
}

type OriginateRequest struct {
	ALeg             string                 `json:"aleg"`
	BLeg             string                 `json:"bleg"`
	Dialplan         string                 `json:"dialplan,omitempty"`
	Context          string                 `json:"context,omitempty"`
	CallerIDName     string                 `json:"caller_id_name,omitempty"`
	CallerIDNumber   string                 `json:"caller_id_number,omitempty"`
	TimeoutSec       int                    `json:"timeout_sec,omitempty"`
	ChannelVariables map[string]interface{} `json:"channel_variables,omitempty"`
}
