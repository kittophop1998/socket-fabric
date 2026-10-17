// The socketfabric command uses only the public HTTP contracts and Go SDK.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
)

// Set only at link time; command execution never mutates build metadata.
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

type buildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
	GoVersion string `json:"goVersion"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

type application struct {
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	lookupEnv  func(string) (string, bool)
	httpClient *http.Client
	build      buildInfo
}

type commandError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
	exit       int
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	app := application{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, lookupEnv: os.LookupEnv,
		build: buildInfo{version, commit, buildTime, runtime.Version(), runtime.GOOS, runtime.GOARCH},
	}
	code := app.run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

type options struct {
	url, timeout, project, key string
	room, event, data          string
	json                       bool
}

func (a application) run(ctx context.Context, args []string) int {
	o := loadOptions(a.lookupEnv)
	command, help, err := parse(args, &o)
	if err != nil {
		return a.fail(o.json, err)
	}
	if help {
		return a.finish(false, nil, helpText(command))
	}
	if command == "version" {
		info := a.build
		info.Version, info.Commit, info.BuildTime = o.safe(info.Version), o.safe(info.Commit), o.safe(info.BuildTime)
		info.GoVersion, info.OS, info.Arch = o.safe(info.GoVersion), o.safe(info.OS), o.safe(info.Arch)
		return a.finish(o.json, info, fmt.Sprintf("socketfabric %s\ncommit: %s\nbuilt: %s\nGo: %s\nplatform: %s/%s\n", info.Version, info.Commit, info.BuildTime, info.GoVersion, info.OS, info.Arch))
	}
	cfg, err := validate(o, command)
	if err != nil {
		return a.fail(o.json, err)
	}
	if err := ctx.Err(); err != nil {
		return a.fail(o.json, contextFailure(err))
	}
	switch command {
	case "config validate":
		result := struct {
			URL              string `json:"url"`
			ProjectID        string `json:"projectId,omitempty"`
			Timeout          string `json:"timeout"`
			APIKeyConfigured bool   `json:"apiKeyConfigured"`
		}{o.safe(cfg.base.String()), o.safe(o.project), cfg.timeout.String(), o.key != ""}
		return a.finish(o.json, result, fmt.Sprintf("Configuration valid\nGateway: %s\nProject: %s\nTimeout: %s\nAPI key configured: %t\n", result.URL, result.ProjectID, result.Timeout, result.APIKeyConfigured))
	case "status", "ping":
		return a.diagnose(ctx, command, o, cfg)
	case "publish":
		return a.publish(ctx, o, cfg)
	default:
		return a.fail(o.json, usageFailure())
	}
}

func parse(args []string, o *options) (command string, help bool, failure *commandError) {
	root := newFlags("socketfabric", o, true, true)
	if err := root.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", true, nil
		}
		return "", false, usageFailure()
	}
	remaining := root.Args()
	if len(remaining) == 0 {
		return "", true, nil
	}
	command, remaining = remaining[0], remaining[1:]
	if command == "config" {
		if len(remaining) == 0 || len(remaining) == 1 && (remaining[0] == "--help" || remaining[0] == "-h") {
			return "config", true, nil
		}
		if remaining[0] != "validate" {
			return command, false, usageFailure()
		}
		command, remaining = "config validate", remaining[1:]
	}
	switch command {
	case "status", "ping", "publish", "config validate", "version":
	default:
		return command, false, usageFailure()
	}
	flags := newFlags(command, o, command != "version", command == "publish" || command == "config validate")
	if command == "publish" {
		flags.StringVar(&o.room, "room", "", "target room ID (required)")
		flags.StringVar(&o.event, "event", "", "event type (required)")
		flags.StringVar(&o.data, "data", "", "one JSON value, or - to read stdin (required)")
	}
	if err := flags.Parse(remaining); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return command, true, nil
		}
		return command, false, usageFailure()
	}
	if flags.NArg() != 0 {
		return command, false, usageFailure()
	}
	return command, false, nil
}

func newFlags(name string, o *options, network, project bool) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	// The flag package's errors include untrusted argument values. Keep those
	// out of diagnostics, even for unknown flags or malformed bool values.
	flags.SetOutput(io.Discard)
	flags.BoolVar(&o.json, "json", o.json, "machine-readable output")
	if network {
		flags.StringVar(&o.url, "url", o.url, "gateway HTTP(S) base URL")
		flags.StringVar(&o.timeout, "timeout", o.timeout, "network command timeout")
	}
	if project {
		flags.StringVar(&o.project, "project", o.project, "publisher project ID")
	}
	return flags
}

func (o options) safe(value string) string {
	if o.key != "" {
		for _, secret := range []string{o.key, url.PathEscape(o.key), url.QueryEscape(o.key)} {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}

func (a application) finish(asJSON bool, result any, human string) int {
	var err error
	if asJSON {
		err = json.NewEncoder(a.stdout).Encode(result)
	} else {
		_, err = io.WriteString(a.stdout, human)
	}
	if err != nil {
		return a.fail(asJSON, &commandError{Code: "output_failed", Message: "Unable to write command output.", exit: 1})
	}
	return 0
}

func (a application) fail(asJSON bool, err *commandError) int {
	if asJSON {
		_ = json.NewEncoder(a.stderr).Encode(struct {
			Error *commandError `json:"error"`
		}{err})
	} else {
		_, _ = fmt.Fprintln(a.stderr, "socketfabric: "+err.Message)
	}
	return err.exit
}

func usageFailure() *commandError {
	return &commandError{Code: "invalid_arguments", Message: "Invalid command or arguments. Run socketfabric --help, or socketfabric <command> --help.", exit: 2}
}

func contextFailure(err error) *commandError {
	if errors.Is(err, context.Canceled) {
		return &commandError{Code: "canceled", Message: "Command canceled.", exit: 130}
	}
	return &commandError{Code: "timeout", Message: "Gateway request timed out. Check connectivity or increase --timeout. A publish may already have been accepted; do not retry blindly.", exit: 1}
}

func helpText(command string) string {
	intro := "SocketFabric developer CLI\n\nUsage: socketfabric [common flags] <command> [flags]\n\n"
	if command != "" {
		intro = "Usage: socketfabric " + command + " [flags]\n\n"
	}
	return intro + `Commands:
  status            Check this gateway's /healthz and /readyz
  ping              Measure HTTP connectivity through /healthz
  publish           Publish one event through the existing Go SDK
  config validate   Validate configuration without network requests
  version           Print version/build metadata

Common flags (before the command or after its final verb):
  --url URL         HTTP(S) gateway (SOCKETFABRIC_URL; default http://localhost:8080)
  --timeout VALUE   Network command budget (SOCKETFABRIC_TIMEOUT; default 5s)
  --project ID      Publish/config project (SOCKETFABRIC_PROJECT_ID)
  --json            JSON results and errors; available for all result commands
  -h, --help        Show help without requiring configuration

Publish flags:
  --room ID         Target room (required)
  --event TYPE      Event type (required)
  --data JSON       One JSON value, including null; use - to read stdin

Authentication: set SOCKETFABRIC_API_KEY in the environment. There is no
API-key flag, token signer, config-file storage or automatic publish retry.
Publish requires a project and a project-scoped key of at least 32 bytes.
Diagnostics need no credentials. status succeeds only when both checks pass;
ping measures HTTP connectivity, not WebSocket/browser event delivery.
Common flags only apply where relevant; version accepts --json and --help.

Examples:
  socketfabric status --url http://localhost:8081 --json
  socketfabric ping --timeout 2s
  socketfabric publish --project project-a --room room-123 --event resource.updated --data '{"status":"completed"}'
  socketfabric publish --project project-a --room room-123 --event resource.updated --data - < event.json
  socketfabric config validate --json
  socketfabric version --json

A publish success means API acceptance and local queueing, not confirmed
browser delivery. Diagnostics describe one gateway, not full cluster health.
Exit codes: 0 success; 1 remote/network/timeout/output; 2 input/config; 130 canceled.
`
}
