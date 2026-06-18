package main

import "testing"

func TestSanitizeDialDestination(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain extension", "200", "200"},
		{"e164", "+15146272886", "+15146272886"},
		{"feature code", "*97", "*97"},
		{"strips formatting", "(514) 627-2886", "5146272886"},
		{"strips injection flags", "200 -both &exec", "200"},
		{"strips shell", "200;rm -rf /", "200"},
		{"non-dialable", "abc", ""},
		{"empty", "", ""},
		{"over-long rejected", "123456789012345678901", ""}, // 21 chars > 20
		{"max length kept", "12345678901234567890", "12345678901234567890"},
	}
	for _, c := range cases {
		if got := sanitizeDialDestination(c.in); got != c.want {
			t.Errorf("%s: sanitizeDialDestination(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestSanitizeTollAllow(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"classes with commas", "domestic,international,local", "domestic,international,local"},
		{"keeps underscore/dash", "us_48,intl-premium", "us_48,intl-premium"},
		{"strips delimiter chars", "domestic:bad]inject}x", "domesticbadinjectx"},
		{"strips spaces/quotes", "domestic, 'evil'", "domestic,evil"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := sanitizeTollAllow(c.in); got != c.want {
			t.Errorf("%s: sanitizeTollAllow(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

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

	// Injection chars are sanitized out of both values.
	want = "[^^:loopback_export=toll_allow,outbound_caller_id_number:toll_allow=domesticevil:outbound_caller_id_number=15551234]loopback/200/f1-dev.emaktech.com"
	if got := buildConferenceDialString("domestic]evil", "1 555-1234 ;rm", ctx, "200"); got != want {
		t.Errorf("sanitized: got %q", got)
	}
}
