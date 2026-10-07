package main

import (
	"context"
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

// filterAgentsByDomain keeps the agents owned by one of the allowed contexts,
// by the same rule that decides who may change an agent (agentRowOwner).
func filterAgentsByDomain(rows []map[string]string, allowedContexts []string) []map[string]string {
	filtered := make([]map[string]string, 0)
	for _, row := range rows {
		domain := agentRowOwner(row)
		if domain == "" || domain == unknownOwner {
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

// validateCCDomain checks a queue name: <short>@<domain> in its shape (400
// otherwise), in one of the caller's allowed contexts (403 otherwise).
// Writes the error response and returns false when refused.
func (h *APIHandler) validateCCDomain(w http.ResponseWriter, r *http.Request, entityName, entityType string) bool {
	if !validQueueName(entityName) {
		h.respondError(w, r, fmt.Sprintf("%s name must be <name>@<domain>", entityType), http.StatusBadRequest)
		return false
	}
	domain := extractDomain(entityName)
	if contextAllowed(r, domain) {
		return true
	}
	allowedList := strings.Join(getAllowedContexts(r), ", ")
	h.respondError(w, r,
		fmt.Sprintf("%s '%s' belongs to domain '%s' which is not in your allowed contexts: [%s]",
			entityType, entityName, domain, allowedList),
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

// sendCCCommand sends a callcenter_config command via ESL and returns the
// response. mod_callcenter reports a refused change as an "-ERR ..." reply
// body, which is returned as an ESL error rather than as success.
func (h *APIHandler) sendCCCommand(args string) (string, error) {
	cmd := fmt.Sprintf("api callcenter_config %s", args)
	response, err := h.eslClient.SendCommand(cmd)
	if err == nil && strings.HasPrefix(strings.TrimSpace(response), "-ERR") {
		return response, fmt.Errorf("ESL error: %s", strings.TrimSpace(response))
	}
	return response, err
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

	// Optional status filter, quoted: "On Break" is one argument.
	cmd := fmt.Sprintf("queue count agents %s", queueName)
	if status := r.URL.Query().Get("status"); status != "" {
		if !agentStatuses[status] {
			h.respondError(w, r, "status must be an agent status", http.StatusBadRequest)
			return
		}
		cmd = fmt.Sprintf("queue count agents %s %s", queueName, fsQuote(status))
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

	if !validAgentName(req.Name) {
		h.respondError(w, r, "name must be the agent's uuid or <name>@<domain>", http.StatusBadRequest)
		return
	}
	if !agentTypes[req.Type] {
		h.respondError(w, r, "type must be 'callback' or 'uuid-standby'", http.StatusBadRequest)
		return
	}
	// An agent named <name>@<domain> belongs to that domain. One named by its
	// uuid (FusionPBX's name) has no tenant until its contact is set; the
	// contact can only be set in one of the caller's own contexts.
	domain, qualified := splitQualified(req.Name, agentLocalPattern)
	if qualified && !contextAllowed(r, domain) {
		h.respondError(w, r, fmt.Sprintf("Agent domain '%s' is not in your allowed contexts", domain), http.StatusForbidden)
		return
	}
	// A uuid-named agent from a restricted caller is recorded as that tenant's
	// until its contact is set, so the caller must name exactly one tenant.
	claimFor := ""
	if !qualified && !isUnrestrictedAccess(r) {
		allowed := getAllowedContexts(r)
		if len(allowed) != 1 {
			h.respondError(w, r, "Adding an agent by uuid needs exactly one allowed context", http.StatusBadRequest)
			return
		}
		claimFor = allowed[0]
	}

	// On the serialized connection: whether this add succeeded decides the claim.
	ctx, cancel := context.WithTimeout(context.Background(), channelDumpTimeout)
	defer cancel()
	_, err := h.serial.API(ctx, fmt.Sprintf("callcenter_config agent add %s %s", req.Name, req.Type))
	if err != nil {
		h.respondError(w, r, fmt.Sprintf("Failed to add agent: %v", err), http.StatusBadGateway)
		return
	}

	if claimFor != "" {
		h.agentClaims.record(req.Name, claimFor)
	}
	h.respondSuccess(w, r, fmt.Sprintf("Agent %s added with type %s", req.Name, req.Type))
}

// CCDeleteAgent handles DELETE /v1/callcenter/agents/{agent_name}
func (h *APIHandler) CCDeleteAgent(w http.ResponseWriter, r *http.Request) {
	agentName := mux.Vars(r)["agent_name"]

	if !validAgentName(agentName) {
		h.respondError(w, r, "agent must be its uuid or <name>@<domain>", http.StatusBadRequest)
		return
	}
	if !h.authorizeAgent(w, r, agentName, "") {
		return
	}

	_, err := h.sendCCCommand(fmt.Sprintf("agent del %s", agentName))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to delete agent: %v", err), statusCode)
		return
	}
	h.agentClaims.clear(agentName)

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

	if !validAgentName(agentName) {
		h.respondError(w, r, "agent must be its uuid or <name>@<domain>", http.StatusBadRequest)
		return
	}
	if !validAgentValue(req.Key, req.Value) {
		h.respondError(w, r, fmt.Sprintf("%q can't be set to that value (keys: status, state, contact, type, max_no_answer, wrap_up_time, reject_delay_time, busy_delay_time, no_answer_delay_time, ready_time)", req.Key), http.StatusBadRequest)
		return
	}
	// A contact names the tenant the agent then rings in: it must be one of
	// the caller's own, whoever owned the agent before.
	claim := ""
	if req.Key == "contact" {
		domain, _ := contactDomain(req.Value)
		if !contextAllowed(r, domain) {
			h.respondError(w, r, fmt.Sprintf("Contact domain '%s' is not in your allowed contexts", domain), http.StatusForbidden)
			return
		}
		claim = req.Value
	}
	if !h.authorizeAgent(w, r, agentName, claim) {
		return
	}

	// Command format: agent set <key> <agent_name> <value>
	_, err := h.sendCCCommand(fmt.Sprintf("agent set %s %s %s", req.Key, agentName, fsQuote(req.Value)))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to set agent %s: %v", req.Key, err), statusCode)
		return
	}

	if req.Key == "contact" {
		h.agentClaims.clear(agentName)
	}
	h.respondSuccess(w, r, fmt.Sprintf("Agent %s %s set to '%s'", agentName, req.Key, req.Value))
}

// --- Tier handlers ---

// validateTierAgent checks a tier's agent: in its shape (400), and one of the
// caller's own tenant's agents (authorizeAgent).
func (h *APIHandler) validateTierAgent(w http.ResponseWriter, r *http.Request, agent string) bool {
	if !validAgentName(agent) {
		h.respondError(w, r, "agent must be its uuid or <name>@<domain>", http.StatusBadRequest)
		return false
	}
	return h.authorizeAgent(w, r, agent, "")
}

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

	if !h.validateCCDomain(w, r, req.Queue, "Queue") || !h.validateTierAgent(w, r, req.Agent) {
		return
	}
	if (req.Level != "" && !smallNumberPattern.MatchString(req.Level)) || (req.Position != "" && !smallNumberPattern.MatchString(req.Position)) {
		h.respondError(w, r, "level and position must be whole numbers", http.StatusBadRequest)
		return
	}
	if req.Position != "" && req.Level == "" {
		h.respondError(w, r, "position needs a level", http.StatusBadRequest)
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

	if !h.validateCCDomain(w, r, req.Queue, "Queue") || !h.validateTierAgent(w, r, req.Agent) {
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
	if !validTierValue(req.Key, req.Value) {
		h.respondError(w, r, fmt.Sprintf("%q can't be set to that value (keys: state, level, position)", req.Key), http.StatusBadRequest)
		return
	}
	if !h.validateCCDomain(w, r, req.Queue, "Queue") || !h.validateTierAgent(w, r, req.Agent) {
		return
	}

	// Command format: tier set <key> <queue> <agent> <value>
	_, err := h.sendCCCommand(fmt.Sprintf("tier set %s %s %s %s", req.Key, req.Queue, req.Agent, fsQuote(req.Value)))
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to set tier %s: %v", req.Key, err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Tier %s set to '%s' for agent %s in queue %s", req.Key, req.Value, req.Agent, req.Queue))
}
