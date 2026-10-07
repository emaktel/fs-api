package main

import "testing"

func TestBuildConferenceDialString(t *testing.T) {
	const ctx = "f1-dev.emaktech.com"

	// No toll_allow, no CID: plain loopback.
	if got := buildConferenceDialString("", "", ctx, "200"); got != "loopback/200/f1-dev.emaktech.com" {
		t.Errorf("plain: got %q", got)
	}

	// CID only (internal add, no toll): exported so it reaches the dialplan leg.
	want := "[^^:loopback_export=outbound_caller_id_number:outbound_caller_id_number=15147386000]loopback/200/f1-dev.emaktech.com"
	if got := buildConferenceDialString("", "15147386000", ctx, "200"); got != want {
		t.Errorf("cid-only: got %q, want %q", got, want)
	}

	// toll_allow + CID (external): both exported; alternate delimiter preserves the comma lists.
	want = "[^^:loopback_export=toll_allow,outbound_caller_id_number:toll_allow=domestic,international:outbound_caller_id_number=+15147386000]loopback/15551234567/f1-dev.emaktech.com"
	if got := buildConferenceDialString("domestic,international", "+15147386000", ctx, "15551234567"); got != want {
		t.Errorf("external: got %q, want %q", got, want)
	}

	// The caller ID number, read from the call, is reduced to digits and +.
	want = "[^^:loopback_export=outbound_caller_id_number:outbound_caller_id_number=15551234]loopback/200/f1-dev.emaktech.com"
	if got := buildConferenceDialString("", "1 555-1234 x", ctx, "200"); got != want {
		t.Errorf("sanitized: got %q", got)
	}
}
