package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ParsePipeDelimited parses FreeSWITCH callcenter_config pipe-delimited output
// into a slice of maps. The first non-empty line is treated as the header row
// with field names separated by '|'. Each subsequent data row is split the same way.
// Skips empty lines and "+OK" terminators. Returns empty slice (not nil) for no data.
func ParsePipeDelimited(raw string) []map[string]string {
	result := make([]map[string]string, 0)

	lines := strings.Split(raw, "\n")

	var headers []string
	headerParsed := false

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines and +OK terminators
		if line == "" || strings.HasPrefix(line, "+OK") {
			continue
		}

		if !headerParsed {
			// First non-empty, non-+OK line is the header
			headers = strings.Split(line, "|")
			for i, h := range headers {
				headers[i] = strings.TrimSpace(h)
			}
			headerParsed = true
			continue
		}

		// Data row
		fields := strings.Split(line, "|")
		row := make(map[string]string)
		for i, h := range headers {
			if i < len(fields) {
				row[h] = strings.TrimSpace(fields[i])
			} else {
				row[h] = ""
			}
		}
		result = append(result, row)
	}

	return result
}

// ParsePlainCount parses an integer from FreeSWITCH count command output.
func ParsePlainCount(raw string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	// Sometimes the response has +OK or extra text; try to find a number
	lines := strings.Split(trimmed, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "+OK") {
			continue
		}
		n, err := strconv.Atoi(line)
		if err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("could not parse count from: %s", trimmed)
}

// ExtractDomainFromContact extracts the domain_name value from a FreeSWITCH
// agent contact field. The contact field contains key=value pairs and we look
// for "domain_name=<value>". Returns empty string if not found.
func ExtractDomainFromContact(contact string) string {
	// Look for domain_name= in the contact string
	const prefix = "domain_name="
	idx := strings.Index(contact, prefix)
	if idx == -1 {
		return ""
	}

	// Extract the value after domain_name=
	start := idx + len(prefix)
	if start >= len(contact) {
		return ""
	}

	// The value ends at the next delimiter (comma, space, curly brace, or end of string)
	rest := contact[start:]
	for i, ch := range rest {
		if ch == ',' || ch == ' ' || ch == '}' || ch == '\'' || ch == '"' {
			return rest[:i]
		}
	}
	return rest
}
