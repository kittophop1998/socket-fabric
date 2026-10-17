package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testKey = "cli-test-publisher-key-32-bytes-minimum"

func testApplication(env map[string]string, stdin string, client *http.Client) (application, *bytes.Buffer, *bytes.Buffer) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return application{
		stdin: strings.NewReader(stdin), stdout: stdout, stderr: stderr, httpClient: client,
		lookupEnv: func(name string) (string, bool) { value, ok := env[name]; return value, ok },
		build:     buildInfo{"test-version", "test-commit", "test-time", "test-go", "test-os", "test-arch"},
	}, stdout, stderr
}

func publisherEnv(address string) map[string]string {
	return map[string]string{"SOCKETFABRIC_URL": address, "SOCKETFABRIC_PROJECT_ID": "project-a", "SOCKETFABRIC_API_KEY": testKey}
}

func publishArgs(data string) []string {
	return []string{"publish", "--room", "room-123", "--event", "resource.updated", "--data", data, "--json"}
}

func decodeOutput[T any](t *testing.T, output *bytes.Buffer) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	return result
}

func TestHelpAndVersionBypassConfiguration(t *testing.T) {
	for _, args := range [][]string{
		{}, {"--help"}, {"-h"}, {"status", "--help"}, {"ping", "--help"},
		{"publish", "--help"}, {"config"}, {"config", "--help"}, {"config", "validate", "--help"}, {"version", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			app, out, stderr := testApplication(map[string]string{"SOCKETFABRIC_URL": "invalid", "SOCKETFABRIC_TIMEOUT": "invalid"}, "", nil)
			if code := app.run(context.Background(), args); code != 0 || !strings.Contains(out.String(), "Usage:") || stderr.Len() != 0 {
				t.Fatalf("help exit=%d stderr=%s", code, stderr)
			}
		})
	}
	for _, jsonMode := range []bool{false, true} {
		app, out, stderr := testApplication(map[string]string{"SOCKETFABRIC_URL": "invalid"}, "", nil)
		args := []string{"version"}
		if jsonMode {
			args = append(args, "--json")
		}
		if code := app.run(context.Background(), args); code != 0 || stderr.Len() != 0 {
			t.Fatalf("version exit=%d stderr=%s", code, stderr)
		}
		if jsonMode {
			result := decodeOutput[buildInfo](t, out)
			if result != app.build {
				t.Fatal("build metadata changed")
			}
		} else if !strings.Contains(out.String(), "socketfabric test-version") {
			t.Fatal("missing version")
		}
	}
}

func TestParsingRejectsUnsupportedArgumentsWithoutEcho(t *testing.T) {
	for _, args := range [][]string{
		{testKey}, {"status", testKey}, {"config", testKey}, {"config", "validate", testKey},
		{"version", "--url", testKey}, {"publish", "--api-key", testKey},
		{"--json=" + testKey, "version"}, {"--" + testKey}, {"ping", "--" + testKey},
		{"status", "--timeout"}, {"config", "--url", "http://localhost", "validate"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			app, out, stderr := testApplication(publisherEnv("http://localhost"), "", nil)
			if code := app.run(context.Background(), args); code != 2 || out.Len() != 0 || strings.Contains(stderr.String(), testKey) {
				t.Fatalf("argument failure exit=%d; secret printed=%t", code, strings.Contains(stderr.String(), testKey))
			}
		})
	}
}

func TestConfigurationDefaultsPrecedenceAndNoNetwork(t *testing.T) {
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("config validate must not make a network request")
		return nil, errors.New("unexpected request")
	})}
	for _, test := range []struct {
		name string
		env  map[string]string
		args []string
		url  string
		proj string
		wait string
		key  bool
	}{
		{"defaults", nil, []string{"config", "validate", "--json"}, "http://localhost:8080", "", "5s", false},
		{"environment", publisherEnv("https://example.com/proxy/"), []string{"config", "validate", "--json"}, "https://example.com/proxy/", "project-a", "5s", true},
		{"flags override invalid env", map[string]string{"SOCKETFABRIC_URL": "invalid", "SOCKETFABRIC_TIMEOUT": "invalid", "SOCKETFABRIC_PROJECT_ID": "bad project"}, []string{"--url", "http://localhost:8081", "--timeout", "2s", "--project", "project-b", "--json", "config", "validate"}, "http://localhost:8081", "project-b", "2s", false},
		{"suffix overrides prefix", nil, []string{"--url", "http://localhost:8081", "--project", "before", "config", "validate", "--url", "http://localhost:8082", "--project", "after", "--timeout", "1s", "--json"}, "http://localhost:8082", "after", "1s", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, out, stderr := testApplication(test.env, "", client)
			if code := app.run(context.Background(), test.args); code != 0 || stderr.Len() != 0 {
				t.Fatalf("config exit=%d stderr=%s", code, stderr)
			}
			result := decodeOutput[struct {
				URL, ProjectID, Timeout string
				APIKeyConfigured        bool
			}](t, out)
			if result.URL != test.url || result.ProjectID != test.proj || result.Timeout != test.wait || result.APIKeyConfigured != test.key {
				t.Fatal("unexpected effective configuration")
			}
			if strings.Contains(out.String(), testKey) {
				t.Fatal("configuration leaked credentials")
			}
		})
	}
}

func TestInvalidConfiguration(t *testing.T) {
	for _, address := range []string{"", "relative", "ws://localhost", "http://", "http://localhost:0", "http://localhost:65536", "http://localhost:", "http://localhost:bad", "https://user:" + testKey + "@example.com", "http://localhost?", "http://localhost?key=" + testKey, "http://localhost#" + testKey} {
		app, out, stderr := testApplication(map[string]string{"SOCKETFABRIC_URL": address}, "", nil)
		if code := app.run(context.Background(), []string{"config", "validate", "--json"}); code != 2 || out.Len() != 0 || strings.Contains(stderr.String(), testKey) {
			t.Fatalf("invalid URL exit=%d", code)
		}
	}
	for _, change := range []map[string]string{
		{"SOCKETFABRIC_TIMEOUT": ""}, {"SOCKETFABRIC_TIMEOUT": "0s"}, {"SOCKETFABRIC_TIMEOUT": "-1s"}, {"SOCKETFABRIC_TIMEOUT": testKey},
		{"SOCKETFABRIC_PROJECT_ID": "bad project"}, {"SOCKETFABRIC_API_KEY": "short"}, {"SOCKETFABRIC_API_KEY": testKey + "\n"},
	} {
		app, _, stderr := testApplication(change, "", nil)
		if code := app.run(context.Background(), []string{"config", "validate"}); code != 2 || strings.Contains(stderr.String(), testKey) {
			t.Fatalf("invalid config exit=%d", code)
		}
	}
	app, _, _ := testApplication(publisherEnv("http://localhost"), "", nil)
	if code := app.run(context.Background(), []string{"config", "validate", "--url", ""}); code != 2 {
		t.Fatal("explicit empty flag must override environment")
	}
}

func TestMissingPublishConfiguration(t *testing.T) {
	for _, env := range []map[string]string{
		nil, {"SOCKETFABRIC_PROJECT_ID": "project-a"}, {"SOCKETFABRIC_API_KEY": testKey},
		{"SOCKETFABRIC_PROJECT_ID": "*", "SOCKETFABRIC_API_KEY": testKey},
	} {
		app, out, stderr := testApplication(env, "", nil)
		if code := app.run(context.Background(), publishArgs("null")); code != 2 || out.Len() != 0 || strings.Contains(stderr.String(), testKey) {
			t.Fatalf("missing configuration exit=%d", code)
		}
	}
}

func TestConfigAndVersionRedactCredentialCollisions(t *testing.T) {
	app, out, _ := testApplication(publisherEnv("http://localhost/"+testKey), "", nil)
	if code := app.run(context.Background(), []string{"config", "validate", "--project", testKey, "--json"}); code != 0 || strings.Contains(out.String(), testKey) || !strings.Contains(out.String(), "[redacted]") {
		t.Fatal("config did not redact key in other fields")
	}
	for _, mode := range [][]string{{"version"}, {"version", "--json"}} {
		app, out, _ := testApplication(publisherEnv("http://localhost"), "", nil)
		app.build.Version = testKey
		if code := app.run(context.Background(), mode); code != 0 || strings.Contains(out.String(), testKey) {
			t.Fatal("version did not redact credential")
		}
	}
	key := `escaped-cli-key-with-at-least-32-bytes+/?="`
	for _, escaped := range []string{url.PathEscape(key), url.QueryEscape(key)} {
		env := publisherEnv("http://localhost/" + escaped)
		env["SOCKETFABRIC_API_KEY"] = key
		app, out, _ := testApplication(env, "", nil)
		if code := app.run(context.Background(), []string{"config", "validate", "--json"}); code != 0 || strings.Contains(out.String(), escaped) || !strings.Contains(out.String(), "[redacted]") {
			t.Fatal("escaped credential appeared in configuration output")
		}
	}
}

func TestInputCancellationReadFailureAndOutputFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()
	done := make(chan *commandError, 1)
	go func() { _, err := readData(ctx, "-", reader); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil || err.exit != 130 {
			t.Fatal("stdin cancellation must return 130")
		}
	case <-time.After(time.Second):
		t.Fatal("stdin read did not cancel")
	}
	app, _, stderr := testApplication(publisherEnv("http://localhost"), "", nil)
	app.stdin = failingIO{}
	if code := app.run(context.Background(), publishArgs("-")); code != 1 || strings.Contains(stderr.String(), testKey) {
		t.Fatalf("input read error exit=%d", code)
	}
	app, _, stderr = testApplication(nil, "", nil)
	app.stdout = failingIO{}
	if code := app.run(context.Background(), []string{"version", "--json"}); code != 1 || !strings.Contains(stderr.String(), "output_failed") {
		t.Fatalf("output error exit=%d", code)
	}
	app, _, _ = testApplication(nil, "", nil)
	if code := app.run(ctx, []string{"config", "validate"}); code != 130 {
		t.Fatalf("pre-cancellation exit=%d", code)
	}
}

type failingIO struct{}

func (failingIO) Read([]byte) (int, error)  { return 0, errors.New(testKey) }
func (failingIO) Write([]byte) (int, error) { return 0, errors.New(testKey) }

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Inherited stdin is different from io.Pipe/os.Pipe on Unix: its descriptor
// may be blocking and outside Go's poller. Exercise it in a disposable process.
func TestStdinSignalCancellation(t *testing.T) {
	if os.Getenv("SOCKETFABRIC_TEST_STDIN_PROCESS") == "1" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		app, _, _ := testApplication(publisherEnv("http://127.0.0.1:1"), "", nil)
		app.stdin, app.stdout, app.stderr = notifiedStdin{os.Stdin}, os.Stdout, os.Stderr
		code := app.run(ctx, publishArgs("-"))
		stop()
		os.Exit(code)
	}
	if os.PathSeparator == '\\' {
		t.Skip("Windows cannot send Unix signals through os.Process.Signal; cross-builds checked separately")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, executable, "-test.run=^TestStdinSignalCancellation$")
			child.Env = append(os.Environ(), "SOCKETFABRIC_TEST_STDIN_PROCESS=1")
			input, err := child.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			stderr, err := child.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(stderr)
			if line, err := reader.ReadString('\n'); err != nil || line != "stdin-ready\n" {
				t.Fatal("child did not start its inherited stdin read")
			}
			// Let the inherited descriptor enter its blocking system read.
			time.Sleep(25 * time.Millisecond)
			if err := child.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			output, _ := io.ReadAll(reader)
			err = child.Wait()
			var exit *exec.ExitError
			if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != 130 || !bytes.Contains(output, []byte(`"code":"canceled"`)) {
				t.Fatalf("stdin signal did not exit 130: %v", err)
			}
		})
	}
}

type notifiedStdin struct{ *os.File }

func (r notifiedStdin) Read(buffer []byte) (int, error) {
	_, _ = io.WriteString(os.Stderr, "stdin-ready\n")
	return r.File.Read(buffer)
}
