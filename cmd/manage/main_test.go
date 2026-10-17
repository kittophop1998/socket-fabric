package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestManageParsingAndNoSecretOutput(t *testing.T) {
	for _, test := range []struct {
		args []string
		want int
	}{{nil, 0}, {[]string{"--help"}, 0}, {[]string{"operator", "create", "--help"}, 0}, {[]string{"unknown", "command"}, 2}, {[]string{"migrate", "down"}, 2}, {[]string{"operator", "bootstrap-admin", "--email", "admin@example.com"}, 2}, {[]string{"operator", "create", "--email", "not-an-email"}, 2}, {[]string{"operator", "create", "--password", "secret-input"}, 2}} {
		var out, errOut bytes.Buffer
		if got := run(context.Background(), test.args, strings.NewReader(""), &out, &errOut); got != test.want {
			t.Fatalf("%v exit=%d", test.args, got)
		}
		if strings.Contains(out.String()+errOut.String(), "secret-input") {
			t.Fatal("secret argument leaked")
		}
	}
}
