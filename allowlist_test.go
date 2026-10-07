package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/mux"
)

// Loi 5 U46/U60/U61: every handler that builds an ESL command accepts only
// values in a known-good shape. These tests first pin the exact commands the
// legitimate callers produce (the SvelteKit softphone, /api/v2/call/transfer,
// fs-ai through the calls_v2 proxy, the active-calls page, the call-center
// dashboard and the chatbot / KAI call-center mirror), then check that
// everything else is refused (400, or 403 for another tenant's call, queue or
// agent) before any command is sent.

const (
	alCall   = "a1b2c3d4-0000-4000-8000-000000000001"
	alCall2  = "a1b2c3d4-0000-4000-8000-000000000002"
	alDomain = "acme.example.com"
	alOther  = "other.example.com"
	alAgent  = "33333333-3333-4333-8333-333333333333" // acme's (FusionPBX contact)
	alAgentB = "77777777-7777-4777-8777-777777777777" // other's
	alAgentN = "55555555-5555-4555-8555-555555555555" // just added, no contact yet
	alAgentU = "66666666-6666-4666-8666-666666666666" // contact names no tenant
)

// recordingESL is an ESLClient that records each command and answers from a
// per-test function.
type recordingESL struct {
	mu    sync.Mutex
	cmds  []string
	reply func(cmd string) (string, error)
}

func (r *recordingESL) SendCommand(cmd string) (string, error) {
	r.mu.Lock()
	r.cmds = append(r.cmds, cmd)
	r.mu.Unlock()
	if r.reply != nil {
		return r.reply(cmd)
	}
	return "+OK", nil
}

func (r *recordingESL) Close() error { return nil }

func (r *recordingESL) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cmds...)
}

// writes are the commands that change something: reads (getvar, list) excluded.
// Agent ownership never reads the shared connection (serial), so an
// `agent list` here would be a regression the tests catch as a write.
func (r *recordingESL) writes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.cmds {
		if strings.HasPrefix(c, "api uuid_getvar ") {
			continue
		}
		out = append(out, c)
	}
	return out
}

// fakeDumps answers uuid_dump from per-call channel variables; tests change
// them (a conference join) as the commands go out.
type fakeDumps struct {
	mu    sync.Mutex
	calls map[string]map[string]any
	// left, when set for a call, is how many more dumps succeed before it is gone.
	left map[string]int
}

func newFakeDumps(domains map[string]string) *fakeDumps {
	d := &fakeDumps{calls: map[string]map[string]any{}}
	for call, domain := range domains {
		d.calls[call] = map[string]any{"Unique-ID": call, "variable_accountcode": domain, "variable_domain_name": domain}
	}
	return d
}

func (d *fakeDumps) set(call, key, value string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls[call][key] = value
}

func (d *fakeDumps) ChannelDump(_ context.Context, callUUID string) (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	vars, ok := d.calls[callUUID]
	if n, limited := d.left[callUUID]; limited {
		if n == 0 {
			return nil, errChannelGone
		}
		d.left[callUUID] = n - 1
	}
	if !ok {
		return nil, errChannelGone
	}
	out := map[string]any{}
	for k, v := range vars {
		out[k] = v
	}
	return out, nil
}

// agentList is `callcenter_config agent list` output: a FusionPBX agent of
// each tenant, one just added without a contact, and a chatbot-era runtime
// agent named <ext>@<domain>.
const agentList = "name|instance_id|uuid|type|contact|status\n" +
	alAgent + "|single_box||callback|{call_timeout=25,domain_name=" + alDomain + ",domain_uuid=x}user/104@" + alDomain + "|Available\n" +
	alAgentB + "|single_box||callback|{call_timeout=25,domain_name=" + alOther + ",domain_uuid=y}user/104@" + alOther + "|Available\n" +
	alAgentN + "|single_box||callback||Logged Out\n" +
	alAgentU + "|single_box||callback|sofia/gateway/carrier/15145550100|Available\n" +
	"105@" + alDomain + "|single_box||callback|user/105@" + alDomain + "|Available\n" +
	"+OK\n"

// fakeSerial is the serialized connection: `agent list <name>` answers from
// agentList, anything else goes to (and is recorded by) the shared fake.
type fakeSerial struct{ shared *recordingESL }

func (f fakeSerial) API(_ context.Context, cmd string) (string, error) {
	if name, ok := strings.CutPrefix(cmd, "callcenter_config agent list "); ok {
		for _, line := range strings.Split(agentList, "\n") {
			if strings.HasPrefix(line, name+"|") {
				return "name|instance_id|uuid|type|contact|status\n" + line + "\n+OK\n", nil
			}
		}
		return "+OK\n", nil
	}
	reply, err := f.shared.SendCommand("api " + cmd)
	if err == nil && strings.HasPrefix(reply, "-ERR") {
		return "", fmt.Errorf("ESL error: %s", reply)
	}
	return reply, err
}

const alCallB = "a1b2c3d4-0000-4000-8000-00000000000b" // another tenant's call

func newAllowlistHandler() (*APIHandler, *recordingESL) {
	h, esl, _ := newAllowlistHandlerWithDumps()
	return h, esl
}

func newAllowlistHandlerWithDumps() (*APIHandler, *recordingESL, *fakeDumps) {
	dumps := newFakeDumps(map[string]string{alCall: alDomain, alCall2: alDomain, alCallB: alOther})
	esl := &recordingESL{}
	// A conference transfer joins the leg to the room, as FreeSWITCH would.
	esl.reply = func(cmd string) (string, error) {
		if f := strings.Fields(cmd); len(f) == 5 && f[1] == "uuid_transfer" && strings.HasPrefix(f[3], "conference:") {
			dumps.set(f[2], "variable_conference_name", strings.TrimSuffix(strings.TrimPrefix(f[3], "conference:"), "@softphone"))
		}
		return "+OK", nil
	}
	return &APIHandler{eslClient: esl, channelDumps: dumps, serial: fakeSerial{shared: esl}}, esl, dumps
}

// serve routes one request through the real router, as tenant `contexts`
// ("" = no X-Allowed-Contexts header).
func serve(h *APIHandler, method, path, contexts string, body any) *httptest.ResponseRecorder {
	r := mux.NewRouter()
	r.Use(requestIDMiddleware, contextAuthMiddleware)
	v1 := r.PathPrefix("/v1").Subrouter()
	v1.HandleFunc("/calls/{uuid}/hangup", h.HangupCall).Methods("POST")
	v1.HandleFunc("/calls/{uuid}/transfer", h.TransferCall).Methods("POST")
	v1.HandleFunc("/calls/bridge", h.BridgeCalls).Methods("POST")
	v1.HandleFunc("/calls/{uuid}/answer", h.AnswerCall).Methods("POST")
	v1.HandleFunc("/calls/{uuid}/hold", h.ControlHold).Methods("POST")
	v1.HandleFunc("/calls/{uuid}/record", h.ControlRecording).Methods("POST")
	v1.HandleFunc("/calls/{uuid}/dtmf", h.SendDTMF).Methods("POST")
	v1.HandleFunc("/calls/{uuid}/park", h.ParkCall).Methods("POST")
	v1.HandleFunc("/calls/{uuid}/conference", h.AddToConference).Methods("POST")
	v1.HandleFunc("/calls/{uuid}", h.GetCallDetails).Methods("GET")
	cc := v1.PathPrefix("/callcenter").Subrouter()
	cc.HandleFunc("/queues/{queue_name}/agents/count", h.CCCountQueueAgents).Methods("GET")
	cc.HandleFunc("/queues/{queue_name}/members", h.CCListQueueMembers).Methods("GET")
	cc.HandleFunc("/queues/{queue_name}/load", h.CCLoadQueue).Methods("POST")
	cc.HandleFunc("/queues/{queue_name}/unload", h.CCUnloadQueue).Methods("POST")
	cc.HandleFunc("/agents", h.CCAddAgent).Methods("POST")
	cc.HandleFunc("/agents/{agent_name}", h.CCDeleteAgent).Methods("DELETE")
	cc.HandleFunc("/agents/{agent_name}", h.CCSetAgent).Methods("PUT")
	cc.HandleFunc("/tiers", h.CCAddTier).Methods("POST")
	cc.HandleFunc("/tiers", h.CCDeleteTier).Methods("DELETE")
	cc.HandleFunc("/tiers", h.CCSetTier).Methods("PUT")

	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if contexts != "" {
		req.Header.Set("X-Allowed-Contexts", contexts)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

type alCase struct {
	name, method, path, contexts string
	body                         any
	want                         []string // the write commands sent, in order
}

func runAccepted(t *testing.T, cases []alCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, esl := newAllowlistHandler()
			rec := serve(h, c.method, c.path, c.contexts, c.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if got := esl.writes(); strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Fatalf("commands\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

func runRefused(t *testing.T, wantStatus int, cases []alCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, esl := newAllowlistHandler()
			rec := serve(h, c.method, c.path, c.contexts, c.body)
			if rec.Code != wantStatus {
				t.Fatalf("status %d, want %d: %s", rec.Code, wantStatus, rec.Body)
			}
			if w := esl.writes(); len(w) != 0 {
				t.Fatalf("refused request still sent %q", w)
			}
		})
	}
}

// ─── Call control ───────────────────────────────────────────────────────────

func TestCallControlLegitimateCallers(t *testing.T) {
	runAccepted(t, []alCase{
		{"active-calls hangup", "POST", "/v1/calls/" + alCall + "/hangup", alDomain,
			map[string]any{"cause": "NORMAL_CLEARING"}, []string{"api uuid_kill " + alCall + " NORMAL_CLEARING"}},
		{"hangup without a body", "POST", "/v1/calls/" + alCall + "/hangup", alDomain,
			nil, []string{"api uuid_kill " + alCall + " NORMAL_CLEARING"}},
		{"fs-ai transfer", "POST", "/v1/calls/" + alCall + "/transfer", alDomain,
			map[string]any{"destination": "1001", "context": alDomain}, []string{"api uuid_transfer " + alCall + " 1001 XML " + alDomain}},
		{"fs-ai transfer to a number", "POST", "/v1/calls/" + alCall + "/transfer", alDomain,
			map[string]any{"destination": "+15145550100", "context": alDomain}, []string{"api uuid_transfer " + alCall + " +15145550100 XML " + alDomain}},
		{"softphone transfer", "POST", "/v1/calls/" + alCall + "/transfer", alDomain,
			map[string]any{"destination": "*99101", "leg": "bleg", "tollAllow": "domestic"}, []string{"api uuid_transfer " + alCall + " -bleg *99101"}},
		{"v2 call transfer", "POST", "/v1/calls/" + alCall + "/transfer", alDomain,
			map[string]any{"destination": "2000", "context": alDomain, "leg": "aleg", "dialplan": "XML"}, []string{"api uuid_transfer " + alCall + " 2000 XML " + alDomain}},
		{"hold", "POST", "/v1/calls/" + alCall + "/hold", alDomain,
			map[string]any{"action": "unhold"}, []string{"api uuid_hold off " + alCall}},
		{"dtmf", "POST", "/v1/calls/" + alCall + "/dtmf", alDomain,
			map[string]any{"digits": "12#*"}, []string{"api uuid_send_dtmf " + alCall + " 12#*@100"}},
		{"park", "POST", "/v1/calls/" + alCall + "/park", alDomain, nil, []string{"api uuid_park " + alCall}},
		{"answer", "POST", "/v1/calls/" + alCall + "/answer", alDomain, nil, []string{"api uuid_answer " + alCall}},
		{"bridge", "POST", "/v1/calls/bridge", alDomain,
			map[string]any{"uuid_a": alCall, "uuid_b": alCall2}, []string{"api uuid_bridge " + alCall + " " + alCall2}},
		{"recording stop", "POST", "/v1/calls/" + alCall + "/record", alDomain,
			map[string]any{"action": "stop"}, []string{"api uuid_record " + alCall + " stop all"}},
	})
}

// The softphone's conference add on a call already in its room: the
// destination is dialled through the call's own dialplan with the
// extension's toll classes (spaces an admin typed around the commas trimmed)
// and the outbound caller ID read from the call's own dump.
func TestConferenceSoftphoneRequest(t *testing.T) {
	for name, toll := range map[string]string{"plain list": "domestic,international", "spaced list": "domestic, international"} {
		t.Run(name, func(t *testing.T) {
			h, esl, dumps := newAllowlistHandlerWithDumps()
			dumps.set(alCall, "variable_conference_name", "sp-"+alCall)
			dumps.set(alCall, "variable_outbound_caller_id_number", "+15147386000")
			rec := serve(h, "POST", "/v1/calls/"+alCall+"/conference", alDomain, map[string]any{"destination": "+15145550100", "tollAllow": toll})
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			want := "api conference sp-" + alCall + " bgdial [^^:loopback_export=toll_allow,outbound_caller_id_number:toll_allow=domestic,international:outbound_caller_id_number=+15147386000]loopback/+15145550100/" + alDomain
			if w := esl.writes(); len(w) != 1 || w[0] != want {
				t.Fatalf("commands %q\nwant %q", w, want)
			}
		})
	}
}

// The first conference add moves the call and its bridged partner (read from
// the call's own dump, and of the same tenant) into a new room, then dials.
func TestConferenceFirstAddMovesBothLegs(t *testing.T) {
	h, esl, dumps := newAllowlistHandlerWithDumps()
	dumps.set(alCall, "variable_bridge_uuid", alCall2)
	rec := serve(h, "POST", "/v1/calls/"+alCall+"/conference", alDomain, map[string]any{"destination": "1001"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	room := "sp-" + alCall
	want := []string{
		"api uuid_setvar " + alCall + " park_after_bridge true",
		"api uuid_setvar " + alCall + " hangup_after_bridge false",
		"api uuid_setvar " + alCall2 + " park_after_bridge true",
		"api uuid_setvar " + alCall2 + " hangup_after_bridge false",
		"api uuid_transfer " + alCall + " conference:" + room + "@softphone inline",
		"api uuid_transfer " + alCall2 + " conference:" + room + "@softphone inline",
		"api conference " + room + " bgdial loopback/1001/" + alDomain,
	}
	if got := esl.writes(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands\n got: %q\nwant: %q", got, want)
	}
}

// A partner leg with its own accountcode but the call's domain is the same
// tenant (an extension with a custom accountcode), and is moved.
func TestConferencePartnerWithCustomAccountcode(t *testing.T) {
	h, esl, dumps := newAllowlistHandlerWithDumps()
	dumps.set(alCall, "variable_bridge_uuid", alCall2)
	dumps.set(alCall2, "variable_accountcode", "custom-acct")
	if rec := serve(h, "POST", "/v1/calls/"+alCall+"/conference", alDomain, map[string]any{"destination": "1001"}); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s (commands %q)", rec.Code, rec.Body, esl.writes())
	}
}

// A partner that has already hung up is a 409, not a tenant refusal.
func TestConferencePartnerGone(t *testing.T) {
	h, esl, dumps := newAllowlistHandlerWithDumps()
	dumps.set(alCall, "variable_bridge_uuid", "a1b2c3d4-0000-4000-8000-0000000000ff")
	if rec := serve(h, "POST", "/v1/calls/"+alCall+"/conference", alDomain, map[string]any{"destination": "1001"}); rec.Code != http.StatusConflict || len(esl.writes()) != 0 {
		t.Fatalf("status %d, commands %q", rec.Code, esl.writes())
	}
}

// Call details return the requested call's row of `show calls`, never the
// box's first row, which may be another tenant's call.
func TestCallDetailsReturnsTheRequestedCall(t *testing.T) {
	h, esl := newAllowlistHandler()
	esl.reply = func(cmd string) (string, error) {
		switch cmd {
		case "api show calls as json":
			return `{"row_count":2,"rows":[{"uuid":"` + alCallB + `","b_uuid":"","cid_num":"5145550000"},{"uuid":"` + alCall + `","b_uuid":"","cid_num":"5145550199"}]}`, nil
		case "api show channels as json":
			return `{"rows":[]}`, nil
		}
		return "+OK", nil
	}
	rec := serve(h, "GET", "/v1/calls/"+alCall, alDomain, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		CallInfo map[string]any `json:"call_info"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.CallInfo["uuid"] != alCall || body.CallInfo["cid_num"] != "5145550199" {
		t.Fatalf("call_info %v (%v)", body.CallInfo, err)
	}
}

// A partner leg of another tenant, or a room or partner id outside its
// shape, stops the add before anything is moved.
func TestConferenceRefusesForeignOrMalformedLegs(t *testing.T) {
	for name, tc := range map[string]struct {
		key, value string
		status     int
	}{
		"partner of another tenant": {"variable_bridge_uuid", alCallB, http.StatusForbidden},
		"partner id malformed":      {"variable_bridge_uuid", "not a uuid", http.StatusBadGateway},
		"room name malformed":       {"variable_conference_name", "room x", http.StatusConflict},
		"another conference's room": {"variable_conference_name", "3000-" + alOther, http.StatusConflict},
	} {
		t.Run(name, func(t *testing.T) {
			h, esl, dumps := newAllowlistHandlerWithDumps()
			dumps.set(alCall, tc.key, tc.value)
			rec := serve(h, "POST", "/v1/calls/"+alCall+"/conference", alDomain, map[string]any{"destination": "1001"})
			if rec.Code != tc.status || len(esl.writes()) != 0 {
				t.Fatalf("status %d (want %d), commands %q", rec.Code, tc.status, esl.writes())
			}
		})
	}
}

// An unrestricted caller (no X-Allowed-Contexts) still gets the shape checks,
// but no tenant ownership check.
func TestCallcenterUnrestrictedCaller(t *testing.T) {
	h, esl := newAllowlistHandler()
	if rec := serve(h, "PUT", "/v1/callcenter/agents/"+alAgentB, "", map[string]any{"key": "status", "value": "Available"}); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec := serve(h, "PUT", "/v1/callcenter/agents/"+alAgentB, "", map[string]any{"key": "status", "value": "Away"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("shape check skipped: %d", rec.Code)
	}
	if w := esl.writes(); len(w) != 1 {
		t.Fatalf("commands %q", w)
	}
}

func TestCallControlRefusesValuesOutsideTheirShape(t *testing.T) {
	bad := []string{"-1001", "1001 1002", "1001'", "{a=b}1001", "1001\n1002", "a,b", "a:b", "&x(y)", "${x}", "a|b", ""}
	var cases []alCase
	for _, d := range bad {
		cases = append(cases, alCase{"transfer to " + d, "POST", "/v1/calls/" + alCall + "/transfer", alDomain, map[string]any{"destination": d}, nil})
	}
	cases = append(cases,
		alCase{"transfer dialplan other than XML", "POST", "/v1/calls/" + alCall + "/transfer", alDomain, map[string]any{"destination": "1001", "dialplan": "inline", "context": alDomain}, nil},
		alCase{"transfer dialplan without a context", "POST", "/v1/calls/" + alCall + "/transfer", alDomain, map[string]any{"destination": "1001", "dialplan": "XML"}, nil},
		alCase{"transfer context outside the shape", "POST", "/v1/calls/" + alCall + "/transfer", "", map[string]any{"destination": "1001", "context": "a b"}, nil},
		alCase{"hangup cause with a space", "POST", "/v1/calls/" + alCall + "/hangup", alDomain, map[string]any{"cause": "NORMAL CLEARING"}, nil},
		alCase{"hangup cause lower case", "POST", "/v1/calls/" + alCall + "/hangup", alDomain, map[string]any{"cause": "normal_clearing"}, nil},
		alCase{"dtmf digits with a space", "POST", "/v1/calls/" + alCall + "/dtmf", alDomain, map[string]any{"digits": "1 2"}, nil},
		alCase{"dtmf digits with @", "POST", "/v1/calls/" + alCall + "/dtmf", alDomain, map[string]any{"digits": "1@9"}, nil},
		alCase{"dtmf duration too long", "POST", "/v1/calls/" + alCall + "/dtmf", alDomain, map[string]any{"digits": "1", "duration": 5000}, nil},
		alCase{"dtmf duration negative", "POST", "/v1/calls/" + alCall + "/dtmf", alDomain, map[string]any{"digits": "1", "duration": -1}, nil},
		alCase{"recording outside the tenant folder", "POST", "/v1/calls/" + alCall + "/record", alDomain, map[string]any{"action": "start", "filename": "/tmp/x.wav"}, nil},
		alCase{"recording in another tenant's folder", "POST", "/v1/calls/" + alCall + "/record", alDomain, map[string]any{"action": "start", "filename": "/var/lib/freeswitch/recordings/" + alOther + "/x.wav"}, nil},
		alCase{"recording climbing out", "POST", "/v1/calls/" + alCall + "/record", alDomain, map[string]any{"action": "start", "filename": "/var/lib/freeswitch/recordings/" + alDomain + "/../x.wav"}, nil},
		alCase{"recording that isn't audio", "POST", "/v1/calls/" + alCall + "/record", alDomain, map[string]any{"action": "start", "filename": "/var/lib/freeswitch/recordings/" + alDomain + "/x.txt"}, nil},
		alCase{"recording name with a space", "POST", "/v1/calls/" + alCall + "/record", alDomain, map[string]any{"action": "start", "filename": "/var/lib/freeswitch/recordings/" + alDomain + "/x y.wav"}, nil},
		alCase{"conference destination with letters", "POST", "/v1/calls/" + alCall + "/conference", alDomain, map[string]any{"destination": "1001abc"}, nil},
		alCase{"conference destination with a space", "POST", "/v1/calls/" + alCall + "/conference", alDomain, map[string]any{"destination": "1001 2"}, nil},
		alCase{"conference toll classes outside the shape", "POST", "/v1/calls/" + alCall + "/conference", alDomain, map[string]any{"destination": "1001", "tollAllow": "domestic]x"}, nil},
		alCase{"bridge to a non-canonical uuid", "POST", "/v1/calls/bridge", alDomain, map[string]any{"uuid_a": alCall, "uuid_b": strings.ToUpper(alCall2)}, nil},
		alCase{"call uuid in braces", "POST", "/v1/calls/{" + alCall + "}/park", alDomain, nil, nil},
	)
	runRefused(t, http.StatusBadRequest, cases)
}

func TestCallControlRefusesAnotherTenantsContext(t *testing.T) {
	runRefused(t, http.StatusForbidden, []alCase{
		{"transfer into another tenant's context", "POST", "/v1/calls/" + alCall + "/transfer", alDomain, map[string]any{"destination": "1001", "context": alOther}, nil},
		{"hangup another tenant's call", "POST", "/v1/calls/" + alCall + "/hangup", alOther, nil, nil},
		{"conference add on another tenant's call", "POST", "/v1/calls/" + alCallB + "/conference", alDomain, map[string]any{"destination": "1001"}, nil},
	})
}

// ─── Call center ────────────────────────────────────────────────────────────

func TestCallcenterLegitimateCallers(t *testing.T) {
	runAccepted(t, []alCase{
		{"dashboard status change", "PUT", "/v1/callcenter/agents/" + alAgent, alDomain,
			map[string]any{"key": "status", "value": "On Break", "domain": alDomain}, []string{"api callcenter_config agent set status " + alAgent + " 'On Break'"}},
		{"agents tab no-answer delay", "PUT", "/v1/callcenter/agents/" + alAgent, alDomain,
			map[string]any{"key": "no_answer_delay_time", "value": "25", "domain": alDomain}, []string{"api callcenter_config agent set no_answer_delay_time " + alAgent + " '25'"}},
		{"agents tab add", "POST", "/v1/callcenter/agents", alDomain,
			map[string]any{"name": "1001@" + alDomain, "type": "callback", "domain": alDomain}, []string{"api callcenter_config agent add 1001@" + alDomain + " callback"}},
		{"agents tab delete", "DELETE", "/v1/callcenter/agents/" + alAgent, alDomain,
			map[string]any{"domain": alDomain}, []string{"api callcenter_config agent del " + alAgent}},
		{"mirror: new agent", "POST", "/v1/callcenter/agents", alDomain,
			map[string]any{"name": "88888888-8888-4888-8888-888888888888", "type": "callback", "domain": alDomain}, []string{"api callcenter_config agent add 88888888-8888-4888-8888-888888888888 callback"}},
		{"mirror: tier add", "POST", "/v1/callcenter/tiers", alDomain,
			map[string]any{"queue": "99200@" + alDomain, "agent": alAgent, "level": "1", "position": "2", "domain": alDomain}, []string{"api callcenter_config tier add 99200@" + alDomain + " " + alAgent + " 1 2"}},
		{"queue detail tier set", "PUT", "/v1/callcenter/tiers", alDomain,
			map[string]any{"queue": "99200@" + alDomain, "agent": alAgent, "key": "level", "value": "2", "domain": alDomain}, []string{"api callcenter_config tier set level 99200@" + alDomain + " " + alAgent + " '2'"}},
		{"mirror: tier del of a chatbot-era agent", "DELETE", "/v1/callcenter/tiers", alDomain,
			map[string]any{"queue": "99200@" + alDomain, "agent": "105@" + alDomain, "domain": alDomain}, []string{"api callcenter_config tier del 99200@" + alDomain + " 105@" + alDomain}},
		{"mirror: queue load", "POST", "/v1/callcenter/queues/99105@" + alDomain + "/load", alDomain,
			map[string]any{"domain": alDomain}, []string{"api callcenter_config queue load 99105@" + alDomain}},
		{"mirror: queue unload", "POST", "/v1/callcenter/queues/99105@" + alDomain + "/unload", alDomain,
			map[string]any{"domain": alDomain}, []string{"api callcenter_config queue unload 99105@" + alDomain}},
	})
}

func TestCallcenterCountsAgentsByStatus(t *testing.T) {
	h, esl := newAllowlistHandler()
	esl.reply = func(string) (string, error) { return "3\n", nil }
	rec := serve(h, "GET", "/v1/callcenter/queues/99200@"+alDomain+"/agents/count?status=On%20Break", alDomain, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := esl.commands(); len(got) != 1 || got[0] != "api callcenter_config queue count agents 99200@"+alDomain+" 'On Break'" {
		t.Fatalf("commands %q", got)
	}
	if rec := serve(h, "GET", "/v1/callcenter/queues/99200@"+alDomain+"/agents/count?status=Busy", alDomain, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown status: %d", rec.Code)
	}
}

func TestCallcenterRefusesValuesOutsideTheirShape(t *testing.T) {
	q := "99200@" + alDomain
	runRefused(t, http.StatusBadRequest, []alCase{
		{"agent add with a space", "POST", "/v1/callcenter/agents", alDomain, map[string]any{"name": "10 01@" + alDomain, "type": "callback"}, nil},
		{"agent add without a tenant", "POST", "/v1/callcenter/agents", alDomain, map[string]any{"name": "1001", "type": "callback"}, nil},
		{"agent add unknown type", "POST", "/v1/callcenter/agents", alDomain, map[string]any{"name": "1001@" + alDomain, "type": "other"}, nil},
		{"agent set unknown key", "PUT", "/v1/callcenter/agents/" + alAgent, alDomain, map[string]any{"key": "name", "value": "x"}, nil},
		{"agent set status outside the list", "PUT", "/v1/callcenter/agents/" + alAgent, alDomain, map[string]any{"key": "status", "value": "Away"}, nil},
		{"agent set status with a quote", "PUT", "/v1/callcenter/agents/" + alAgent, alDomain, map[string]any{"key": "status", "value": "On Break'"}, nil},
		{"agent set contact with variables", "PUT", "/v1/callcenter/agents/" + alAgent, alDomain, map[string]any{"key": "contact", "value": "{a=b}user/104@" + alDomain}, nil},
		{"agent set contact through another endpoint", "PUT", "/v1/callcenter/agents/" + alAgent, alDomain, map[string]any{"key": "contact", "value": "loopback/104/" + alDomain}, nil},
		{"agent set number that isn't one", "PUT", "/v1/callcenter/agents/" + alAgent, alDomain, map[string]any{"key": "wrap_up_time", "value": "10 20"}, nil},
		{"agent name with a space", "DELETE", "/v1/callcenter/agents/a%20b@" + alDomain, alDomain, map[string]any{}, nil},
		{"tier queue with a space", "POST", "/v1/callcenter/tiers", alDomain, map[string]any{"queue": "99 200@" + alDomain, "agent": alAgent}, nil},
		{"tier level that isn't a number", "POST", "/v1/callcenter/tiers", alDomain, map[string]any{"queue": q, "agent": alAgent, "level": "1 2"}, nil},
		{"tier set unknown key", "PUT", "/v1/callcenter/tiers", alDomain, map[string]any{"queue": q, "agent": alAgent, "key": "agent", "value": "x"}, nil},
		{"tier set state outside the list", "PUT", "/v1/callcenter/tiers", alDomain, map[string]any{"queue": q, "agent": alAgent, "key": "state", "value": "Busy"}, nil},
		{"queue load with a space", "POST", "/v1/callcenter/queues/99%20200@" + alDomain + "/load", alDomain, map[string]any{}, nil},
	})
}

func TestCallcenterRefusesAnotherTenantsAgentOrQueue(t *testing.T) {
	q := "99200@" + alDomain
	runRefused(t, http.StatusForbidden, []alCase{
		{"set another tenant's agent", "PUT", "/v1/callcenter/agents/" + alAgentB, alDomain, map[string]any{"key": "status", "value": "Logged Out", "domain": alDomain}, nil},
		{"delete another tenant's agent", "DELETE", "/v1/callcenter/agents/" + alAgentB, alDomain, map[string]any{"domain": alDomain}, nil},
		{"tier with another tenant's agent", "POST", "/v1/callcenter/tiers", alDomain, map[string]any{"queue": q, "agent": alAgentB}, nil},
		{"tier in another tenant's queue", "POST", "/v1/callcenter/tiers", alDomain, map[string]any{"queue": "99200@" + alOther, "agent": alAgent}, nil},
		{"agent add in another tenant", "POST", "/v1/callcenter/agents", alDomain, map[string]any{"name": "1001@" + alOther, "type": "callback", "domain": alDomain}, nil},
		{"claim a just-added agent for another tenant", "PUT", "/v1/callcenter/agents/" + alAgentN, alDomain, map[string]any{"key": "contact", "value": "user/104@" + alOther}, nil},
		{"claim an agent whose contact names no tenant", "PUT", "/v1/callcenter/agents/" + alAgentU, alDomain, map[string]any{"key": "contact", "value": "user/104@" + alDomain}, nil},
		{"change a just-added agent before it has a contact", "PUT", "/v1/callcenter/agents/" + alAgentN, alDomain, map[string]any{"key": "status", "value": "Available"}, nil},
		{"load another tenant's queue", "POST", "/v1/callcenter/queues/99200@" + alOther + "/load", alDomain, map[string]any{}, nil},
	})
}

func TestCallcenterAgentNotFound(t *testing.T) {
	h, esl := newAllowlistHandler()
	rec := serve(h, "PUT", "/v1/callcenter/agents/99999999-9999-4999-8999-999999999999", alDomain, map[string]any{"key": "status", "value": "Available"})
	if rec.Code != http.StatusNotFound || len(esl.writes()) != 0 {
		t.Fatalf("status %d, writes %q", rec.Code, esl.writes())
	}
}

// The tenant's agent list shows the agents the ownership rule gives it: the
// FusionPBX agent and the <ext>@<domain> agent the call-center sync creates,
// not another tenant's, a just-added one or one whose contact names no tenant.
func TestCallcenterAgentListFilter(t *testing.T) {
	rows := filterAgentsByDomain(ParsePipeDelimited(agentList), []string{alDomain})
	var names []string
	for _, r := range rows {
		names = append(names, r["name"])
	}
	if strings.Join(names, ",") != alAgent+",105@"+alDomain {
		t.Fatalf("agents %q", names)
	}
}

// A change mod_callcenter refuses ("-ERR ...") is an error, not a 200.
func TestCallcenterReportsRefusedCommand(t *testing.T) {
	h, esl := newAllowlistHandler()
	esl.reply = func(string) (string, error) { return "-ERR Agent already exist!\n", nil }
	rec := serve(h, "POST", "/v1/callcenter/agents", alDomain, map[string]any{"name": "1001@" + alDomain, "type": "callback"})
	if rec.Code < 500 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

// A call bridged to another tenant's leg on the same box: details name the
// other leg but withhold its channel variables.
func TestCallDetailsWithholdsAnotherTenantsLeg(t *testing.T) {
	h, esl := newAllowlistHandler()
	esl.reply = func(cmd string) (string, error) {
		switch cmd {
		case "api show calls as json":
			return `{"row_count":1,"rows":[{"uuid":"` + alCall + `","b_uuid":"` + alCallB + `"}]}`, nil
		case "api show channels as json":
			return `{"rows":[]}`, nil
		}
		return "+OK", nil
	}
	rec := serve(h, "GET", "/v1/calls/"+alCall, alDomain, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		ALeg struct{ Details map[string]any } `json:"aleg"`
		BLeg struct {
			UUID    string         `json:"uuid"`
			Details map[string]any `json:"details"`
		} `json:"bleg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ALeg.Details["variable_accountcode"] != alDomain || body.BLeg.UUID != alCallB || body.BLeg.Details != nil {
		t.Fatalf("aleg %v, bleg %s %v", body.ALeg.Details, body.BLeg.UUID, body.BLeg.Details)
	}
}

// The call-center mirror's new agent: added by uuid, then given its contact
// by the same tenant. Another tenant can't claim it, before or after.
func TestCallcenterAgentClaim(t *testing.T) {
	newAgent := alAgentN // in the agent list with no contact, as just added
	add := map[string]any{"name": newAgent, "type": "callback"}
	contact := func(domain string) map[string]any {
		return map[string]any{"key": "contact", "value": "user/104@" + domain}
	}

	h, esl := newAllowlistHandler()
	if rec := serve(h, "POST", "/v1/callcenter/agents", alDomain, add); rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(h, "PUT", "/v1/callcenter/agents/"+newAgent, alOther, contact(alOther)); rec.Code != http.StatusForbidden {
		t.Fatalf("another tenant claimed it first: %d", rec.Code)
	}
	if rec := serve(h, "PUT", "/v1/callcenter/agents/"+newAgent, alDomain, contact(alDomain)); rec.Code != http.StatusOK {
		t.Fatalf("owner's contact: %d %s", rec.Code, rec.Body)
	}
	want := []string{
		"api callcenter_config agent add " + newAgent + " callback",
		"api callcenter_config agent set contact " + newAgent + " 'user/104@" + alDomain + "'",
	}
	if got := esl.writes(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands %q", got)
	}

	// Nobody recorded adding it (fs-api restarted, or another box): no claim.
	h2, esl2 := newAllowlistHandler()
	if rec := serve(h2, "PUT", "/v1/callcenter/agents/"+newAgent, alDomain, contact(alDomain)); rec.Code != http.StatusForbidden || len(esl2.writes()) != 0 {
		t.Fatalf("unrecorded claim: %d %q", rec.Code, esl2.writes())
	}

	// A uuid add needs exactly one allowed context to record.
	if rec := serve(h2, "POST", "/v1/callcenter/agents", alDomain+","+alOther, add); rec.Code != http.StatusBadRequest {
		t.Fatalf("multi-context uuid add: %d", rec.Code)
	}
}

// Asking for the B-leg of a call bridged to another tenant: if the B-leg's
// own dump disappears mid-request, the other tenant's A-leg details are
// still withheld (the requested leg's tenant comes from its authorization).
func TestCallDetailsKeepsTheAuthorizedTenant(t *testing.T) {
	h, esl, dumps := newAllowlistHandlerWithDumps()
	dumps.left = map[string]int{alCallB: 1} // authorization reads it; the details read finds it gone
	esl.reply = func(cmd string) (string, error) {
		switch cmd {
		case "api show calls as json":
			return `{"row_count":1,"rows":[{"uuid":"` + alCall + `","b_uuid":"` + alCallB + `"}]}`, nil
		case "api show channels as json":
			return `{"rows":[]}`, nil
		}
		return "+OK", nil
	}
	rec := serve(h, "GET", "/v1/calls/"+alCallB, alOther, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		ALeg struct {
			Details map[string]any `json:"details"`
		} `json:"aleg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.ALeg.Details != nil {
		t.Fatalf("another tenant's A-leg details returned: %v (%v)", body.ALeg.Details, err)
	}
}

// A second add of the same agent (the duplicate FreeSWITCH refuses) never
// moves the claim: the first confirmed add keeps it.
func TestCallcenterDuplicateAddKeepsFirstClaim(t *testing.T) {
	h, esl := newAllowlistHandler()
	add := map[string]any{"name": alAgentN, "type": "callback"}
	if rec := serve(h, "POST", "/v1/callcenter/agents", alDomain, add); rec.Code != http.StatusOK {
		t.Fatalf("first add: %d", rec.Code)
	}
	esl.reply = func(cmd string) (string, error) {
		if strings.Contains(cmd, " agent add ") {
			return "-ERR Agent already exist!\n", nil
		}
		return "+OK", nil
	}
	if rec := serve(h, "POST", "/v1/callcenter/agents", alOther, add); rec.Code < 500 {
		t.Fatalf("duplicate add: %d", rec.Code)
	}
	h.agentClaims.record(alAgentN, alOther) // even a recorded second claim doesn't replace the first
	if rec := serve(h, "PUT", "/v1/callcenter/agents/"+alAgentN, alOther, map[string]any{"key": "contact", "value": "user/104@" + alOther}); rec.Code != http.StatusForbidden {
		t.Fatalf("second tenant took the agent: %d", rec.Code)
	}
}

// domain_name= is read only as a whole key, not the tail of another key.
func TestExtractDomainFromContactWholeKey(t *testing.T) {
	for contact, want := range map[string]string{
		"{call_timeout=25,domain_name=acme.example.com}user/1@acme.example.com": "acme.example.com",
		"{domain_name=acme.example.com}user/1@acme.example.com":                 "acme.example.com",
		"{x_domain_name=other.example.com}user/1@acme.example.com":              "",
		"{x_domain_name=other.example.com,domain_name=acme.example.com}user/1":  "acme.example.com",
	} {
		if got := ExtractDomainFromContact(contact); got != want {
			t.Errorf("%q: got %q, want %q", contact, got, want)
		}
	}
}

// The tenant that added an agent may delete it before it has a contact (a
// half-finished add), and the delete ends its claim.
func TestCallcenterClaimantDeletesUnfinishedAgent(t *testing.T) {
	h, esl := newAllowlistHandler()
	if rec := serve(h, "POST", "/v1/callcenter/agents", alDomain, map[string]any{"name": alAgentN, "type": "callback"}); rec.Code != http.StatusOK {
		t.Fatalf("add: %d", rec.Code)
	}
	if rec := serve(h, "DELETE", "/v1/callcenter/agents/"+alAgentN, alOther, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("another tenant deleted it: %d", rec.Code)
	}
	if rec := serve(h, "DELETE", "/v1/callcenter/agents/"+alAgentN, alDomain, nil); rec.Code != http.StatusOK {
		t.Fatalf("claimant delete: %d %s", rec.Code, rec.Body)
	}
	if h.agentClaims.owner(alAgentN) != "" {
		t.Fatalf("claim kept after delete")
	}
	if w := esl.writes(); len(w) != 2 || w[1] != "api callcenter_config agent del "+alAgentN {
		t.Fatalf("commands %q", w)
	}
}

// Unrestricted (internal) callers keep both legs' details.
func TestCallDetailsUnrestrictedSeesBothLegs(t *testing.T) {
	h, esl := newAllowlistHandler()
	esl.reply = func(cmd string) (string, error) {
		switch cmd {
		case "api show calls as json":
			return `{"row_count":1,"rows":[{"uuid":"` + alCall + `","b_uuid":"` + alCallB + `"}]}`, nil
		case "api show channels as json":
			return `{"rows":[]}`, nil
		}
		return "+OK", nil
	}
	rec := serve(h, "GET", "/v1/calls/"+alCall, "", nil)
	var body struct {
		BLeg struct {
			Details map[string]any `json:"details"`
		} `json:"bleg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK || body.BLeg.Details == nil {
		t.Fatalf("status %d, bleg details %v (%v)", rec.Code, body.BLeg.Details, err)
	}
}
