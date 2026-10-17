package main

import (
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type configuration struct {
	base    *url.URL
	timeout time.Duration
}

func loadOptions(lookup func(string) (string, bool)) options {
	value := func(name, fallback string) string {
		if result, ok := lookup(name); ok {
			return result
		}
		return fallback
	}
	return options{
		url: value("SOCKETFABRIC_URL", "http://localhost:8080"), timeout: value("SOCKETFABRIC_TIMEOUT", "5s"),
		project: value("SOCKETFABRIC_PROJECT_ID", ""), key: value("SOCKETFABRIC_API_KEY", ""),
	}
}

func validate(o options, command string) (configuration, *commandError) {
	invalid := func(message string) (configuration, *commandError) {
		return configuration{}, &commandError{Code: "invalid_config", Message: message, exit: 2}
	}
	base, err := url.Parse(o.url)
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") ||
		base.Host == "" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" ||
		base.ForceQuery || base.Fragment != "" || base.Opaque != "" || strings.HasSuffix(base.Host, ":") {
		return invalid("Set --url or SOCKETFABRIC_URL to an absolute HTTP(S) URL without credentials, query or fragment.")
	}
	if port := base.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return invalid("Gateway URL port must be between 1 and 65535.")
		}
	}
	timeout, err := time.ParseDuration(o.timeout)
	if err != nil || timeout <= 0 {
		return invalid("Set --timeout or SOCKETFABRIC_TIMEOUT to a positive duration, such as 5s.")
	}
	if command == "publish" || command == "config validate" {
		if (command == "publish" || o.project != "") && !validIdentifier(o.project) {
			return invalid("Set --project or SOCKETFABRIC_PROJECT_ID to 1-128 ASCII letters/digits or . _ : -.")
		}
		if command == "publish" && o.key == "" {
			return invalid("Set SOCKETFABRIC_API_KEY to the publisher key for this project. Credentials are accepted only through the environment.")
		}
		if o.key != "" && (len(o.key) < 32 || strings.ContainsFunc(o.key, func(ch rune) bool { return unicode.IsSpace(ch) || unicode.IsControl(ch) })) {
			return invalid("SOCKETFABRIC_API_KEY must contain at least 32 bytes without whitespace or control characters.")
		}
	}
	return configuration{base, timeout}, nil
}

// Mirrors the public wire contract without importing the server's domain.
// SDK validation remains the authority immediately before a publish request.
func validIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i := range len(value) {
		ch := value[i]
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == ':' || ch == '-') {
			return false
		}
	}
	return true
}
