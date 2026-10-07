package main

import (
	"fmt"
	"strings"
	"unicode"
)

// Originate input checks that keep the ring timeout fs-api waits for equal to
// the one FreeSWITCH uses (U48). This is not the full input allowlist (U46):
// it closes only the routes around the timeout.
//
// FreeSWITCH splits `api originate`'s arguments on spaces, grouping single
// quotes and honouring backslash escapes (switch_utils.c
// separate_string_blank_delim / cleanup_separated_string), and a leading "^^x"
// switches the delimiter to x. The ring timeout is the seventh argument
// (mod_commands.c originate_function), and channel variables set inline in
// the dial string ({..}, [..], <..>) or in channel_variables can override or
// extend it (switch_ivr_originate.c). So:
//   - aleg, dialplan, context and caller_id_number may not contain
//     whitespace, control characters, quotes or backslashes;
//   - aleg and bleg may not set inline variables ({ } [ ] < >);
//   - bleg may contain spaces only as an application, &app(args), which is
//     sent single-quoted as one argument;
//   - channel_variables may not set originate timing, and their keys and
//     values may not contain the separators FreeSWITCH parses inside {..}.

// originateTimingVars are the channel variables switch_ivr_originate.c
// (FreeSWITCH 1.10.12) reads that change how long an originate rings, waits
// or retries. Compared case-insensitively.
var originateTimingVars = map[string]bool{
	"originate_timeout":             true,
	"leg_timeout":                   true,
	"leg_progress_timeout":          true,
	"progress_timeout":              true,
	"originate_retries":             true,
	"originate_retry_sleep_ms":      true,
	"originate_retry_min_period_ms": true,
	"originate_retry_timeout":       true,
	"originate_continue_on_timeout": true,
	"originate_delay_start":         true,
	"leg_delay_start":               true,
	"group_confirm_key":             true,
	"group_confirm_file":            true,
	"group_confirm_timeout":         true,
	"group_confirm_read_timeout":    true,
	"group_confirm_cancel_timeout":  true,
}

const (
	inlineVarChars   = "{}[]<>"
	retokenizeChars  = "'\\"
	varListSeparator = ","
)

// hasSpaceOrControl reports whitespace or control characters.
func hasSpaceOrControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// isAppBLeg: bleg runs a dialplan application, &app or &app(args).
func isAppBLeg(bleg string) bool {
	return strings.HasPrefix(bleg, "&") && (!strings.Contains(bleg, "(") || strings.HasSuffix(bleg, ")"))
}

// validateOriginateInput checks req (bleg already defaulted) and returns the
// channel_variables as key=value assignments to send.
func validateOriginateInput(req *OriginateRequest) ([]string, error) {
	if hasSpaceOrControl(req.ALeg) || strings.ContainsAny(req.ALeg, inlineVarChars+retokenizeChars+"^") {
		return nil, fmt.Errorf("aleg must be a dial string without spaces, quotes, backslashes or inline variables ({ } [ ] < >)")
	}
	if hasControl(req.BLeg) || strings.ContainsAny(req.BLeg, inlineVarChars+retokenizeChars) {
		return nil, fmt.Errorf("bleg may not contain quotes, backslashes, control characters or inline variables ({ } [ ] < >)")
	}
	if hasSpaceOrControl(req.BLeg) && !isAppBLeg(req.BLeg) {
		return nil, fmt.Errorf("bleg may contain spaces only as an application, &app(args)")
	}
	for name, v := range map[string]string{"dialplan": req.Dialplan, "context": req.Context, "caller_id_number": req.CallerIDNumber} {
		if hasSpaceOrControl(v) || strings.ContainsAny(v, inlineVarChars+retokenizeChars+varListSeparator) {
			return nil, fmt.Errorf("%s may not contain spaces, quotes, backslashes, commas or brackets", name)
		}
	}
	if hasControl(req.CallerIDName) || strings.ContainsAny(req.CallerIDName, inlineVarChars+retokenizeChars+varListSeparator) {
		return nil, fmt.Errorf("caller_id_name may not contain quotes, backslashes, commas or brackets")
	}

	vars := make([]string, 0, len(req.ChannelVariables))
	for key, value := range req.ChannelVariables {
		if originateTimingVars[strings.ToLower(key)] {
			return nil, fmt.Errorf("channel_variables.%s is not accepted: the ring time is set by timeout_sec", key)
		}
		val := fmt.Sprintf("%v", value)
		if key == "" || hasSpaceOrControl(key) || strings.ContainsAny(key, inlineVarChars+retokenizeChars+varListSeparator+"=") {
			return nil, fmt.Errorf("channel_variables has an invalid key %q", key)
		}
		if hasSpaceOrControl(val) || strings.ContainsAny(val, inlineVarChars+retokenizeChars+varListSeparator) {
			return nil, fmt.Errorf("channel_variables.%s may not contain spaces, quotes, backslashes, commas or brackets", key)
		}
		vars = append(vars, key+"="+val)
	}
	return vars, nil
}

// originateBLegArg is bleg as one originate argument: an application with
// spaces in its arguments is single-quoted (FreeSWITCH strips the quotes).
func originateBLegArg(bleg string) string {
	if hasSpaceOrControl(bleg) {
		return "'" + bleg + "'"
	}
	return bleg
}
