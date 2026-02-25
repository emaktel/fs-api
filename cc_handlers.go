package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
)

// --- Domain helpers ---

// extractDomain extracts the domain part from a "name@domain" string.
func extractDomain(nameAtDomain string) string {
	parts := strings.SplitN(nameAtDomain, "@", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return ""
}

// isDomainAllowed checks if the domain portion of a "name@domain" string
// is in the list of allowed contexts.
func isDomainAllowed(nameAtDomain string, allowedContexts []string) bool {
	domain := extractDomain(nameAtDomain)
	if domain == "" {
		return false
	}
	for _, ctx := range allowedContexts {
		if domain == ctx {
			return true
		}
	}
	return false
}

// filterByDomain filters rows where the given fieldName (e.g. "name" or "queue")
// contains a "name@domain" value whose domain matches one of the allowed contexts.
func filterByDomain(rows []map[string]string, fieldName string, allowedContexts []string) []map[string]string {
	filtered := make([]map[string]string, 0)
	for _, row := range rows {
		val := row[fieldName]
		if isDomainAllowed(val, allowedContexts) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

// filterAgentsByDomain filters agent rows by extracting the domain from the
// "contact" field using ExtractDomainFromContact.
func filterAgentsByDomain(rows []map[string]string, allowedContexts []string) []map[string]string {
	filtered := make([]map[string]string, 0)
	for _, row := range rows {
		contact := row["contact"]
		domain := ExtractDomainFromContact(contact)
		if domain == "" {
			continue
		}
		for _, ctx := range allowedContexts {
			if domain == ctx {
				filtered = append(filtered, row)
				break
			}
		}
	}
	return filtered
}

// validateCCDomain pre-validates domain for write ops on queues/tiers where
// the entity name is in "name@domain" format. Returns true if allowed,
// false if forbidden (and writes error response).
func (h *APIHandler) validateCCDomain(w http.ResponseWriter, r *http.Request, entityName, entityType string) bool {
	if isUnrestrictedAccess(r) {
		return true
	}
	allowedContexts := getAllowedContexts(r)
	if isDomainAllowed(entityName, allowedContexts) {
		return true
	}
	domain := extractDomain(entityName)
	allowedList := strings.Join(allowedContexts, ", ")
	h.respondError(w, r,
		fmt.Sprintf("%s '%s' belongs to domain '%s' which is not in your allowed contexts: [%s]",
			entityType, entityName, domain, allowedList),
		http.StatusForbidden)
	return false
}

// validateCCDomainRaw pre-validates a raw domain string (for agent write ops
// where domain comes from the request body). Returns true if allowed.
func (h *APIHandler) validateCCDomainRaw(w http.ResponseWriter, r *http.Request, domain, entityType string) bool {
	if isUnrestrictedAccess(r) {
		return true
	}
	allowedContexts := getAllowedContexts(r)
	for _, ctx := range allowedContexts {
		if domain == ctx {
			return true
		}
	}
	allowedList := strings.Join(allowedContexts, ", ")
	h.respondError(w, r,
		fmt.Sprintf("%s domain '%s' is not in your allowed contexts: [%s]",
			entityType, domain, allowedList),
		http.StatusForbidden)
	return false
}

// respondJSON writes a JSON response with the X-Request-ID header.
func (h *APIHandler) respondJSON(w http.ResponseWriter, r *http.Request, data interface{}) {
	requestID := getRequestID(r)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(data)
}

// sendCCCommand sends a callcenter_config command via ESL and returns the response.
func (h *APIHandler) sendCCCommand(args string) (string, error) {
	cmd := fmt.Sprintf("api callcenter_config %s", args)
	return h.eslClient.SendCommand(cmd)
}

// --- Queue handlers ---

// CCListQueues handles GET /v1/callcenter/queues
func (h *APIHandler) CCListQueues(w http.ResponseWriter, r *http.Request) {
	response, err := h.sendCCCommand("queue list")
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to list queues: %v", err), statusCode)
		return
	}

	rows := ParsePipeDelimited(response)

	if !isUnrestrictedAccess(r) {
		rows = filterByDomain(rows, "name", getAllowedContexts(r))
	}

	h.respondJSON(w, r, CCListResponse{
		Status:   "success",
		RowCount: len(rows),
		Rows:     rows,
	})
}

// CCCountQueues handles GET /v1/callcenter/queues/count
func (h *APIHandler) CCCountQueues(w http.ResponseWriter, r *http.Request) {
	if isUnrestrictedAccess(r) {
		response, err := h.sendCCCommand("queue count")
		if err != nil {
			statusCode := h.getErrorStatusCode(err)
			h.respondError(w, r, fmt.Sprintf("Failed to count queues: %v", err), statusCode)
			return
		}
		count, err := ParsePlainCount(response)
		if err != nil {
			h.respondError(w, r, fmt.Sprintf("Failed to parse queue count: %v", err), http.StatusInternalServerError)
			return
		}
		h.respondJSON(w, r, CCCountResponse{Status: "success", Count: count})
		return
	}

	// Restricted: list + filter + count
	response, err := h.sendCCCommand("queue list")
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to list queues: %v", err), statusCode)
		return
	}
	rows := ParsePipeDelimited(response)
	rows = filterByDomain(rows, "name", getAllowedContexts(r))
	h.respondJSON(w, r, CCCountResponse{Status: "success", Count: len(rows)})
}

// CCListQueueAgents handles GET /v1/callcenter/queues/{queue_name}/agents
func (h *APIHandler) CCListQueueAgents(w http.ResponseWriter, r *http.Request) {
	queueName := mux.Vars(r)["queue_name"]
	if !h.validateCCDomain(w, r, queueName, "Queue") {
		return
	}

	response, err := h.sendCCCommand(fmt.Sprintf("queue list agents %s", queueName))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to list queue agents: %v", err), statusCode)
		return
	}

	rows := ParsePipeDelimited(response)
	h.respondJSON(w, r, CCListResponse{
		Status:   "success",
		RowCount: len(rows),
		Rows:     rows,
	})
}

// CCListQueueMembers handles GET /v1/callcenter/queues/{queue_name}/members
func (h *APIHandler) CCListQueueMembers(w http.ResponseWriter, r *http.Request) {
	queueName := mux.Vars(r)["queue_name"]
	if !h.validateCCDomain(w, r, queueName, "Queue") {
		return
	}

	response, err := h.sendCCCommand(fmt.Sprintf("queue list members %s", queueName))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to list queue members: %v", err), statusCode)
		return
	}

	rows := ParsePipeDelimited(response)
	h.respondJSON(w, r, CCListResponse{
		Status:   "success",
		RowCount: len(rows),
		Rows:     rows,
	})
}

// CCListQueueTiers handles GET /v1/callcenter/queues/{queue_name}/tiers
func (h *APIHandler) CCListQueueTiers(w http.ResponseWriter, r *http.Request) {
	queueName := mux.Vars(r)["queue_name"]
	if !h.validateCCDomain(w, r, queueName, "Queue") {
		return
	}

	response, err := h.sendCCCommand(fmt.Sprintf("queue list tiers %s", queueName))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to list queue tiers: %v", err), statusCode)
		return
	}

	rows := ParsePipeDelimited(response)
	h.respondJSON(w, r, CCListResponse{
		Status:   "success",
		RowCount: len(rows),
		Rows:     rows,
	})
}

// CCCountQueueAgents handles GET /v1/callcenter/queues/{queue_name}/agents/count
func (h *APIHandler) CCCountQueueAgents(w http.ResponseWriter, r *http.Request) {
	queueName := mux.Vars(r)["queue_name"]
	if !h.validateCCDomain(w, r, queueName, "Queue") {
		return
	}

	// Build count command with optional status filter
	cmd := fmt.Sprintf("queue count agents %s", queueName)
	if status := r.URL.Query().Get("status"); status != "" {
		cmd = fmt.Sprintf("queue count agents %s %s", queueName, status)
	}

	response, err := h.sendCCCommand(cmd)
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to count queue agents: %v", err), statusCode)
		return
	}

	count, err := ParsePlainCount(response)
	if err != nil {
		h.respondError(w, r, fmt.Sprintf("Failed to parse agent count: %v", err), http.StatusInternalServerError)
		return
	}

	h.respondJSON(w, r, CCCountResponse{Status: "success", Count: count})
}

// CCCountQueueMembers handles GET /v1/callcenter/queues/{queue_name}/members/count
func (h *APIHandler) CCCountQueueMembers(w http.ResponseWriter, r *http.Request) {
	queueName := mux.Vars(r)["queue_name"]
	if !h.validateCCDomain(w, r, queueName, "Queue") {
		return
	}

	response, err := h.sendCCCommand(fmt.Sprintf("queue count members %s", queueName))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to count queue members: %v", err), statusCode)
		return
	}

	count, err := ParsePlainCount(response)
	if err != nil {
		h.respondError(w, r, fmt.Sprintf("Failed to parse member count: %v", err), http.StatusInternalServerError)
		return
	}

	h.respondJSON(w, r, CCCountResponse{Status: "success", Count: count})
}

// CCCountQueueTiers handles GET /v1/callcenter/queues/{queue_name}/tiers/count
func (h *APIHandler) CCCountQueueTiers(w http.ResponseWriter, r *http.Request) {
	queueName := mux.Vars(r)["queue_name"]
	if !h.validateCCDomain(w, r, queueName, "Queue") {
		return
	}

	response, err := h.sendCCCommand(fmt.Sprintf("queue count tiers %s", queueName))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to count queue tiers: %v", err), statusCode)
		return
	}

	count, err := ParsePlainCount(response)
	if err != nil {
		h.respondError(w, r, fmt.Sprintf("Failed to parse tier count: %v", err), http.StatusInternalServerError)
		return
	}

	h.respondJSON(w, r, CCCountResponse{Status: "success", Count: count})
}

// CCLoadQueue handles POST /v1/callcenter/queues/{queue_name}/load
func (h *APIHandler) CCLoadQueue(w http.ResponseWriter, r *http.Request) {
	queueName := mux.Vars(r)["queue_name"]
	if !h.validateCCDomain(w, r, queueName, "Queue") {
		return
	}

	_, err := h.sendCCCommand(fmt.Sprintf("queue load %s", queueName))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to load queue: %v", err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Queue %s loaded", queueName))
}

// CCUnloadQueue handles POST /v1/callcenter/queues/{queue_name}/unload
func (h *APIHandler) CCUnloadQueue(w http.ResponseWriter, r *http.Request) {
	queueName := mux.Vars(r)["queue_name"]
	if !h.validateCCDomain(w, r, queueName, "Queue") {
		return
	}

	_, err := h.sendCCCommand(fmt.Sprintf("queue unload %s", queueName))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to unload queue: %v", err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Queue %s unloaded", queueName))
}

// CCReloadQueue handles POST /v1/callcenter/queues/{queue_name}/reload
func (h *APIHandler) CCReloadQueue(w http.ResponseWriter, r *http.Request) {
	queueName := mux.Vars(r)["queue_name"]
	if !h.validateCCDomain(w, r, queueName, "Queue") {
		return
	}

	_, err := h.sendCCCommand(fmt.Sprintf("queue reload %s", queueName))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to reload queue: %v", err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Queue %s reloaded", queueName))
}

// --- Agent handlers ---

// CCListAgents handles GET /v1/callcenter/agents
func (h *APIHandler) CCListAgents(w http.ResponseWriter, r *http.Request) {
	response, err := h.sendCCCommand("agent list")
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to list agents: %v", err), statusCode)
		return
	}

	rows := ParsePipeDelimited(response)

	if !isUnrestrictedAccess(r) {
		rows = filterAgentsByDomain(rows, getAllowedContexts(r))
	}

	h.respondJSON(w, r, CCListResponse{
		Status:   "success",
		RowCount: len(rows),
		Rows:     rows,
	})
}

// CCAddAgent handles POST /v1/callcenter/agents
func (h *APIHandler) CCAddAgent(w http.ResponseWriter, r *http.Request) {
	var req AgentAddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, r, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		h.respondError(w, r, "name is required", http.StatusBadRequest)
		return
	}
	if req.Type == "" {
		h.respondError(w, r, "type is required", http.StatusBadRequest)
		return
	}
	if req.Type != "callback" && req.Type != "uuid-standby" {
		h.respondError(w, r, "type must be 'callback' or 'uuid-standby'", http.StatusBadRequest)
		return
	}

	// Validate domain for auth
	if req.Domain == "" && !isUnrestrictedAccess(r) {
		h.respondError(w, r, "domain is required for authorization", http.StatusBadRequest)
		return
	}
	if req.Domain != "" {
		if !h.validateCCDomainRaw(w, r, req.Domain, "Agent") {
			return
		}
	}

	_, err := h.sendCCCommand(fmt.Sprintf("agent add %s %s", req.Name, req.Type))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to add agent: %v", err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Agent %s added with type %s", req.Name, req.Type))
}

// CCDeleteAgent handles DELETE /v1/callcenter/agents/{agent_name}
func (h *APIHandler) CCDeleteAgent(w http.ResponseWriter, r *http.Request) {
	agentName := mux.Vars(r)["agent_name"]

	var req AgentDelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Allow empty body for unrestricted access
		if !isUnrestrictedAccess(r) {
			h.respondError(w, r, "Invalid request body: domain is required for authorization", http.StatusBadRequest)
			return
		}
	}

	// Validate domain for auth
	if req.Domain == "" && !isUnrestrictedAccess(r) {
		h.respondError(w, r, "domain is required for authorization", http.StatusBadRequest)
		return
	}
	if req.Domain != "" {
		if !h.validateCCDomainRaw(w, r, req.Domain, "Agent") {
			return
		}
	}

	_, err := h.sendCCCommand(fmt.Sprintf("agent del %s", agentName))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to delete agent: %v", err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Agent %s deleted", agentName))
}

// CCSetAgent handles PUT /v1/callcenter/agents/{agent_name}
func (h *APIHandler) CCSetAgent(w http.ResponseWriter, r *http.Request) {
	agentName := mux.Vars(r)["agent_name"]

	var req AgentSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, r, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Key == "" {
		h.respondError(w, r, "key is required", http.StatusBadRequest)
		return
	}
	if !validAgentSetKeys[req.Key] {
		h.respondError(w, r, fmt.Sprintf("invalid key '%s': must be one of: status, state, contact, type, max_no_answer, wrap_up_time, reject_delay_time, busy_delay_time, ready_time", req.Key), http.StatusBadRequest)
		return
	}

	// Validate domain for auth
	if req.Domain == "" && !isUnrestrictedAccess(r) {
		h.respondError(w, r, "domain is required for authorization", http.StatusBadRequest)
		return
	}
	if req.Domain != "" {
		if !h.validateCCDomainRaw(w, r, req.Domain, "Agent") {
			return
		}
	}

	// Command format: agent set <key> <agent_name> <value>
	_, err := h.sendCCCommand(fmt.Sprintf("agent set %s %s '%s'", req.Key, agentName, req.Value))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to set agent %s: %v", req.Key, err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Agent %s %s set to '%s'", agentName, req.Key, req.Value))
}

// --- Tier handlers ---

// CCListTiers handles GET /v1/callcenter/tiers
func (h *APIHandler) CCListTiers(w http.ResponseWriter, r *http.Request) {
	response, err := h.sendCCCommand("tier list")
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to list tiers: %v", err), statusCode)
		return
	}

	rows := ParsePipeDelimited(response)

	if !isUnrestrictedAccess(r) {
		rows = filterByDomain(rows, "queue", getAllowedContexts(r))
	}

	h.respondJSON(w, r, CCListResponse{
		Status:   "success",
		RowCount: len(rows),
		Rows:     rows,
	})
}

// CCAddTier handles POST /v1/callcenter/tiers
func (h *APIHandler) CCAddTier(w http.ResponseWriter, r *http.Request) {
	var req TierAddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, r, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Queue == "" {
		h.respondError(w, r, "queue is required", http.StatusBadRequest)
		return
	}
	if req.Agent == "" {
		h.respondError(w, r, "agent is required", http.StatusBadRequest)
		return
	}

	// Validate queue domain for auth
	if !h.validateCCDomain(w, r, req.Queue, "Queue") {
		return
	}

	// Build command: tier add <queue> <agent> [level] [position]
	cmd := fmt.Sprintf("tier add %s %s", req.Queue, req.Agent)
	if req.Level != "" {
		cmd += " " + req.Level
	}
	if req.Position != "" {
		cmd += " " + req.Position
	}

	_, err := h.sendCCCommand(cmd)
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to add tier: %v", err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Tier added: agent %s to queue %s", req.Agent, req.Queue))
}

// CCDeleteTier handles DELETE /v1/callcenter/tiers
func (h *APIHandler) CCDeleteTier(w http.ResponseWriter, r *http.Request) {
	var req TierDelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, r, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Queue == "" {
		h.respondError(w, r, "queue is required", http.StatusBadRequest)
		return
	}
	if req.Agent == "" {
		h.respondError(w, r, "agent is required", http.StatusBadRequest)
		return
	}

	// Validate queue domain for auth
	if !h.validateCCDomain(w, r, req.Queue, "Queue") {
		return
	}

	// Command format: tier del <queue> <agent> (queue first!)
	_, err := h.sendCCCommand(fmt.Sprintf("tier del %s %s", req.Queue, req.Agent))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to delete tier: %v", err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Tier deleted: agent %s from queue %s", req.Agent, req.Queue))
}

// CCSetTier handles PUT /v1/callcenter/tiers
func (h *APIHandler) CCSetTier(w http.ResponseWriter, r *http.Request) {
	var req TierSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, r, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Queue == "" {
		h.respondError(w, r, "queue is required", http.StatusBadRequest)
		return
	}
	if req.Agent == "" {
		h.respondError(w, r, "agent is required", http.StatusBadRequest)
		return
	}
	if req.Key == "" {
		h.respondError(w, r, "key is required", http.StatusBadRequest)
		return
	}
	if !validTierSetKeys[req.Key] {
		h.respondError(w, r, fmt.Sprintf("invalid key '%s': must be one of: state, level, position", req.Key), http.StatusBadRequest)
		return
	}

	// Validate queue domain for auth
	if !h.validateCCDomain(w, r, req.Queue, "Queue") {
		return
	}

	// Command format: tier set <key> <queue> <agent> <value>
	_, err := h.sendCCCommand(fmt.Sprintf("tier set %s %s %s '%s'", req.Key, req.Queue, req.Agent, req.Value))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to set tier %s: %v", req.Key, err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Tier %s set to '%s' for agent %s in queue %s", req.Key, req.Value, req.Agent, req.Queue))
}
