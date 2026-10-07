package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Originate input allowlist (Loi 5 U46; supersedes the U48 timing checks,
// which it contains). Originate builds one `api originate` command from the
// request, so every field is matched against the one shape a caller needs and
// anything else is refused with 400:
//   - aleg: a single user/<extension>@<domain>, the domain in the caller's
//     allowed contexts (checked by the handler);
//   - bleg: &park() (the default) or a dialplan extension, number or feature
//     code, which the answered A-leg is sent to in the dialplan of `context`
//     (required then, so it never routes in FreeSWITCH's default context);
//   - dialplan: XML; context: a domain name in the caller's allowed contexts;
//   - caller_id_number: digits with an optional leading +;
//   - caller_id_name: letters, digits, spaces and . , ' _ ( ) # + - only (no
//     separator FreeSWITCH reads in a dial string, such as : | = or brackets),
//     sent escaped (fsVarValueArg);
//   - channel_variables: only origination_uuid, a canonical uuid (the call's
//     uuid, chosen by the caller);
//   - callback_url: refused (no caller sends it, and fs-api would POST the
//     call's events to it from the PBX host).
// The ring timeout is always fs-api's own (timeout_sec), first in the {..}
// list and the seventh argument, as U48 requires.

const parkBLeg = "&park()"

// nestedVarsMarker: switch_ivr_originate.c switches its {..} parsing when the
// dial string merely contains "origination_nested_vars=true" (switch_stristr),
// so the text is refused in the caller ID name, the one free-text field.
const nestedVarsMarker = "origination_nested_vars"

var (
	callerIDNumberPattern = regexp.MustCompile(`^\+?[0-9]{1,20}$`)
	callerIDNamePattern   = regexp.MustCompile(`^[\p{L}\p{M}\p{N} .,'_()#+-]{0,64}$`)
)

// validateOriginateInput checks req (bleg already defaulted) and returns the
// domain of its A-leg, which the caller must be allowed to dial in, and the
// channel variables to send as key=value assignments.
func validateOriginateInput(req *OriginateRequest) (string, []string, error) {
	alegDomain, ok := contactDomain(req.ALeg)
	if !ok {
		return "", nil, fmt.Errorf("aleg must be user/<extension>@<domain>")
	}
	if req.BLeg != parkBLeg && !dialTargetPattern.MatchString(req.BLeg) {
		return "", nil, fmt.Errorf("bleg must be %s or an extension, number or feature code", parkBLeg)
	}
	if req.BLeg != parkBLeg && req.Context == "" {
		return "", nil, fmt.Errorf("a bleg extension or number needs a context")
	}
	if req.Dialplan != "" && req.Dialplan != "XML" {
		return "", nil, fmt.Errorf("dialplan must be XML")
	}
	if req.Context != "" && !domainNamePattern.MatchString(req.Context) {
		return "", nil, fmt.Errorf("context must be a domain name")
	}
	if req.CallerIDNumber != "" && !callerIDNumberPattern.MatchString(req.CallerIDNumber) {
		return "", nil, fmt.Errorf("caller_id_number must be digits with an optional leading +")
	}
	// Apostrophes, commas and spaces in a caller ID name are escaped
	// (fsVarValueArg); everything outside the name pattern is refused.
	if !callerIDNamePattern.MatchString(req.CallerIDName) || strings.Contains(strings.ToLower(req.CallerIDName), nestedVarsMarker) {
		return "", nil, fmt.Errorf("caller_id_name may contain only letters, digits, spaces and . , ' _ ( ) # + -")
	}
	if req.CallbackURL != "" {
		return "", nil, fmt.Errorf("callback_url is not accepted")
	}
	var vars []string
	for key, value := range req.ChannelVariables {
		id, ok := value.(string)
		if key != "origination_uuid" || !ok || !canonicalUUID.MatchString(id) {
			return "", nil, fmt.Errorf("channel_variables accepts only origination_uuid, a canonical uuid")
		}
		vars = append(vars, key+"="+id)
	}
	return alegDomain, vars, nil
}

// fsVarValueArg encodes v as the value of a {..} variable inside an
// `api originate` argument, so FreeSWITCH reads back exactly v (leading and
// trailing spaces trimmed, as FreeSWITCH would). The value passes three
// splitters (switch_utils.c), each of which unescapes once, so it is escaped
// three times, innermost first:
//   - the variable's "key=value" split on '=' (pairs of apostrophes would
//     otherwise be read as quotes): \ and \';
//   - the {..} list split on ',': \ \' and \,;
//   - the command split on ' ' (originate_function): \ \' and a space as \s.
func fsVarValueArg(v string) string {
	v = strings.TrimSpace(v)
	v = fsEscape(v, `\'`, false)
	v = fsEscape(v, `\',`, false)
	return fsEscape(v, `\'`, true)
}

// fsEscape puts a backslash before every byte of special, and writes spaces
// as \s when spaces is set.
func fsEscape(s, special string, spaces bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case spaces && c == ' ':
			b.WriteString(`\s`)
		case strings.IndexByte(special, c) >= 0:
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
