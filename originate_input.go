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

// originateTimingVars are the channel variables FreeSWITCH 1.10.12 reads that
// change how long an originate rings, waits or retries
// (switch_ivr_originate.c), or that stop fs-api's originate_timeout reaching
// the dialled leg: user_recurse_variables / group_recurse_variables false make
// the user/ and group/ endpoints drop the inherited variables, so their inner
// originate rings for its 60 s default (mod_dptools user_outgoing_channel,
// group_outgoing_channel); origination_nested_vars changes how the {..} list
// is parsed. Compared case-insensitively.
var originateTimingVars = map[string]bool{
	"originate_timeout":             true,
	"call_timeout":                  true, // SWITCH_CALL_TIMEOUT_VARIABLE, read by the user/ group/ lcr/ endpoints
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
	"user_recurse_variables":        true,
	"group_recurse_variables":       true,
	"origination_nested_vars":       true,
}

// nestedVarsMarker: switch_ivr_originate.c switches its {..} parsing when the
// dial string merely contains "origination_nested_vars=true" (switch_stristr),
// so the text is refused anywhere it would land in the dial string.
const nestedVarsMarker = "origination_nested_vars"

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
	if err := checkALegTiming(req.ALeg); err != nil {
		return nil, err
	}
	for name, v := range map[string]string{"aleg": req.ALeg, "caller_id_name": req.CallerIDName, "caller_id_number": req.CallerIDNumber} {
		if strings.Contains(strings.ToLower(v), nestedVarsMarker) {
			return nil, fmt.Errorf("%s may not contain %s", name, nestedVarsMarker)
		}
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
		if strings.Contains(strings.ToLower(val), nestedVarsMarker) {
			return nil, fmt.Errorf("channel_variables.%s may not contain %s", key, nestedVarsMarker)
		}
		if key == "" || hasSpaceOrControl(key) || strings.ContainsAny(key, inlineVarChars+retokenizeChars+varListSeparator+"=^") {
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

// serialEndpoints run their own serial failover inside one leg: group/ (a
// +F group expands to a "|" list) and lcr/ (routes tried in turn), each
// route getting the full ring timeout (mod_dptools group_outgoing_channel,
// mod_lcr lcr_outgoing_channel).
var serialEndpoints = []string{"group/", "lcr/"}

// checkALegTiming refuses dial strings whose legs can ring one after another,
// each for the full ring timeout, so the total outlasts the deadline:
//   - "|" is serial failover (switch_ivr_originate.c splits on it and gives
//     every try the whole timelimit);
//   - "$" expands variables or API calls (e.g. ${group_call(...+F)}) into
//     such a list after this check;
//   - group/ and lcr/ endpoints fail over serially inside one leg.
//
// "," (parallel) and ":_:" (enterprise: parallel threads sharing the
// timelimit) stay allowed.
func checkALegTiming(aleg string) error {
	if strings.ContainsAny(aleg, "|$") {
		return fmt.Errorf("aleg may not contain | (serial failover) or $ (variable expansion)")
	}
	for _, ent := range strings.Split(aleg, ":_:") {
		for _, leg := range strings.Split(ent, ",") {
			for _, ep := range serialEndpoints {
				if strings.HasPrefix(strings.ToLower(leg), ep) {
					return fmt.Errorf("aleg may not use the %s endpoint (serial failover)", ep)
				}
			}
		}
	}
	return nil
}
