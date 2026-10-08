package main

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Input allowlists for every handler that builds an ESL command (Loi 5 U46,
// U60, U61). FreeSWITCH reads these commands by splitting on spaces, quotes
// and its own separators, so a value outside its shape is part of the command,
// not data. Each value is matched against the shape its callers actually send
// and anything else is refused with 400 (respondError logs it) before a
// command is built. The SvelteKit proxies apply the same allowlists
// (src/lib/server/fsApiAllowlist.ts in fusion-svelte); this is the backstop.

var (
	// A hangup cause name (NORMAL_CLEARING) or a Q.850 code.
	hangupCausePattern = regexp.MustCompile(`^(?:[A-Z][A-Z_]{0,63}|[0-9]{1,3})$`)
	// A dialplan extension, number or feature code. No leading "-", which
	// uuid_transfer would read as a flag.
	dialTargetPattern = regexp.MustCompile(`^[A-Za-z0-9*#+][A-Za-z0-9*#+_.-]{0,63}$`)
	// What the conference loopback dials: digits, +, * and #.
	conferenceTargetPattern = regexp.MustCompile(`^[0-9+*#]{1,20}$`)
	// A toll_allow class list: comma-separated names.
	tollAllowPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}(?:,[A-Za-z0-9_-]{1,32}){0,31}$`)
	dtmfPattern      = regexp.MustCompile(`^[0-9A-Da-d*#wW]{1,64}$`)
	// A tenant's domain name, which is also its dialplan context.
	domainNamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,62})(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,62}))*$`)
	// An extension number, as in user/<extension>@<domain>.
	extensionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)
	// The short part of a mod_callcenter queue name, <short>@<domain>.
	queueShortPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	// The local part of an agent named <name>@<domain>.
	agentLocalPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	// A file name under a tenant's recordings folder.
	recordingPathPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*(?:/[A-Za-z0-9_-][A-Za-z0-9_.-]*)*\.(?:wav|mp3)$`)
	// fs-api's own ad-hoc conference rooms: sp-<call uuid>.
	conferenceRoomPattern = regexp.MustCompile(`^sp-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	smallNumberPattern    = regexp.MustCompile(`^[0-9]{1,6}$`)
	epochPattern          = regexp.MustCompile(`^[0-9]{1,10}$`)
)

const recordingsRoot = "/var/lib/freeswitch/recordings/"

const (
	minDTMFDurationMs = 40
	maxDTMFDurationMs = 2000
)

// agentStatuses are the statuses mod_callcenter accepts for `agent set status`.
var agentStatuses = map[string]bool{"Available": true, "Available (On Demand)": true, "On Break": true, "Logged Out": true}

var agentStates = map[string]bool{"Idle": true, "Waiting": true, "Receiving": true, "In a queue call": true, "Reserved": true}

var agentTypes = map[string]bool{"callback": true, "uuid-standby": true}

var tierStates = map[string]bool{"Ready": true, "Standby": true, "Offering": true, "Active Inbound": true, "No Answer": true}

// validAgentValue reports whether value is what `agent set <key>` accepts.
func validAgentValue(key, value string) bool {
	switch key {
	case "status":
		return agentStatuses[value]
	case "state":
		return agentStates[value]
	case "type":
		return agentTypes[value]
	case "contact":
		_, ok := contactDomain(value)
		return ok
	case "max_no_answer", "wrap_up_time", "reject_delay_time", "busy_delay_time", "no_answer_delay_time":
		return smallNumberPattern.MatchString(value)
	case "ready_time":
		return epochPattern.MatchString(value)
	}
	return false
}

// validTierValue reports whether value is what `tier set <key>` accepts.
func validTierValue(key, value string) bool {
	switch key {
	case "state":
		return tierStates[value]
	case "level", "position":
		return smallNumberPattern.MatchString(value)
	}
	return false
}

// contactDomain is the domain of a contact a caller may set:
// user/<extension>@<domain>, nothing else (no variables, no other endpoint).
func contactDomain(contact string) (string, bool) {
	rest, ok := strings.CutPrefix(contact, "user/")
	if !ok {
		return "", false
	}
	ext, domain, ok := strings.Cut(rest, "@")
	if !ok || !extensionPattern.MatchString(ext) || !domainNamePattern.MatchString(domain) {
		return "", false
	}
	return domain, true
}

// splitQualified splits <short>@<domain> with both parts in their shape.
func splitQualified(name string, short *regexp.Regexp) (string, bool) {
	local, domain, ok := strings.Cut(name, "@")
	if !ok || !short.MatchString(local) || !domainNamePattern.MatchString(domain) {
		return "", false
	}
	return domain, true
}

// validQueueName: mod_callcenter queues are <extension>@<domain>.
func validQueueName(name string) bool {
	_, ok := splitQualified(name, queueShortPattern)
	return ok
}

// validAgentName: an agent is its FusionPBX uuid, or <name>@<domain>.
func validAgentName(name string) bool {
	if canonicalUUID.MatchString(name) {
		return true
	}
	_, ok := splitQualified(name, agentLocalPattern)
	return ok
}

// contextAllowed reports whether the request may act in domain: unrestricted
// callers anywhere, restricted ones only in their X-Allowed-Contexts.
func contextAllowed(r *http.Request, domain string) bool {
	if isUnrestrictedAccess(r) {
		return true
	}
	for _, allowed := range getAllowedContexts(r) {
		if domain == allowed {
			return true
		}
	}
	return false
}

// serialAPI runs an api command with its reply matched to the request
// (eslSerialClient): a reply meant for another command must never decide
// which tenant an agent belongs to.
type serialAPI interface {
	API(ctx context.Context, cmd string) (string, error)
}

// unknownOwner is the owner of an agent whose contact names no tenant fs-api
// can read: no restricted caller may change it.
const unknownOwner = "(unknown)"

// agentOwner is the tenant an existing agent belongs to, read from
// `agent list` (agentRowOwner).
// found is false when no such agent is loaded; owner is "" only for an agent
// with no contact at all (one just added, before its contact is set), and
// unknownOwner when the contact names no tenant.
func (h *APIHandler) agentOwner(name string) (owner string, found bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), channelDumpTimeout)
	defer cancel()
	response, err := h.serial.API(ctx, "callcenter_config agent list "+name)
	if err != nil {
		return "", false, err
	}
	for _, row := range ParsePipeDelimited(response) {
		if row["name"] == name {
			return agentRowOwner(row), true, nil
		}
	}
	return "", false, nil
}

// agentRowOwner is the owner of one `agent list` row (see agentOwner): the
// contact's domain_name= (FusionPBX's config loader) or sip_invite_domain=
// (FusionPBX's agent page, call_center_agent_edit.php) variable and its
// user/<ext>@<domain> endpoint, which must agree when both are there (a
// contact naming two tenants is unknownOwner); else the agent's
// <name>@<domain> name.
func agentRowOwner(row map[string]string) string {
	contact := row["contact"]
	variable := contactVar(contact, "domain_name")
	if variable == "" {
		variable = contactVar(contact, "sip_invite_domain")
	}
	if variable != "" && !domainNamePattern.MatchString(variable) {
		return unknownOwner
	}
	// The endpoint follows the {...} list, whose values may hold ${...}, so
	// it starts after the list's last brace.
	endpoint := contact
	if strings.HasPrefix(endpoint, "{") {
		endpoint = endpoint[strings.LastIndex(endpoint, "}")+1:]
	}
	endpointDomain := ""
	if rest, ok := strings.CutPrefix(endpoint, "user/"); ok {
		if _, d, ok := strings.Cut(rest, "@"); ok && domainNamePattern.MatchString(d) {
			endpointDomain = d
		}
	}
	switch {
	case variable != "" && endpointDomain != "" && variable != endpointDomain:
		return unknownOwner
	case variable != "":
		return variable
	case endpointDomain != "":
		return endpointDomain
	}
	if d, ok := splitQualified(row["name"], agentLocalPattern); ok {
		return d
	}
	if contact != "" {
		return unknownOwner
	}
	return ""
}

// agentClaimTTL bounds how long a just-added agent waits for its contact.
const agentClaimTTL = 10 * time.Minute

// agentClaims remembers which tenant added each uuid-named agent, until its
// contact is set: only that tenant may then give it a contact, so two tenants
// can't race to claim one agent. The zero value is ready to use.
type agentClaims struct {
	mu     sync.Mutex
	owners map[string]agentClaim
}

type agentClaim struct {
	domain string
	at     time.Time
}

func (c *agentClaims) record(name, domain string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owners == nil {
		c.owners = map[string]agentClaim{}
	}
	now := time.Now()
	for n, claim := range c.owners {
		if now.Sub(claim.at) > agentClaimTTL {
			delete(c.owners, n)
		}
	}
	// The first confirmed add keeps the claim.
	if _, taken := c.owners[name]; !taken {
		c.owners[name] = agentClaim{domain: domain, at: now}
	}
}

// owner is the tenant that added name, or "" when none did (or too long ago).
func (c *agentClaims) owner(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	claim, ok := c.owners[name]
	if !ok || time.Since(claim.at) > agentClaimTTL {
		return ""
	}
	return claim.domain
}

func (c *agentClaims) clear(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.owners, name)
}

// authorizeAgent checks that a restricted caller may change agent `name`.
// claimContact is the contact being set, when the change is `set contact`:
// an agent with no contact yet belongs only to the tenant that added it
// (agentClaims), and may only be given a contact in that tenant. Writes the
// error response and returns false when refused.
func (h *APIHandler) authorizeAgent(w http.ResponseWriter, r *http.Request, name, claimContact string) bool {
	if isUnrestrictedAccess(r) {
		return true
	}
	owner, found, err := h.agentOwner(name)
	if err != nil {
		h.respondError(w, r, fmt.Sprintf("Failed to look up agent: %v", err), http.StatusBadGateway)
		return false
	}
	if !found {
		h.respondError(w, r, fmt.Sprintf("Agent %s not found", name), http.StatusNotFound)
		return false
	}
	// An agent with no contact yet belongs to the tenant that added it (if this
	// box recorded the add), and only in that tenant can it get its contact.
	if owner == "" {
		owner = h.agentClaims.owner(name)
		if contactOwner, _ := contactDomain(claimContact); claimContact != "" && contactOwner != owner {
			owner = ""
		}
	}
	if owner == "" || owner == unknownOwner || !contextAllowed(r, owner) {
		h.respondError(w, r, fmt.Sprintf("Agent %s is not in your allowed contexts", name), http.StatusForbidden)
		return false
	}
	return true
}

// normalizeTollAllow trims the spaces an admin may type around the commas of
// a toll class list ("domestic, international"); every other character still
// has to match tollAllowPattern.
func normalizeTollAllow(raw string) string {
	parts := strings.Split(raw, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return strings.Join(parts, ",")
}

// fsQuote single-quotes a value already matched against an allowlist that
// excludes quotes, so FreeSWITCH reads it as one argument.
func fsQuote(v string) string {
	return "'" + v + "'"
}
