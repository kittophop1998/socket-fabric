// The token command issues a short-lived client JWT without exposing publisher keys.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"socketfabric/internal/adapters/outbound/auth"
	"socketfabric/internal/config"
	"socketfabric/internal/domain"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, config.LoadAuth, time.Now))
}

func run(args []string, stdout, stderr io.Writer, loadAuth func() (config.Auth, error), now func() time.Time) int {
	flags := flag.NewFlagSet("token", flag.ContinueOnError)
	flags.SetOutput(stderr)
	project := flags.String("project", "", "configured project ID")
	user := flags.String("user", "", "client user ID")
	rooms := flags.String("rooms", "", "comma-separated allowed room IDs")
	ttl := flags.Duration("ttl", 5*time.Minute, "token lifetime (maximum 15m)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *project == "" || *user == "" || *rooms == "" {
		fmt.Fprintln(stderr, "--project, --user and --rooms are required; positional arguments are not supported")
		return 2
	}
	if *ttl < time.Second || *ttl > domain.MaxTokenTTL {
		fmt.Fprintln(stderr, "--ttl must be between 1s and 15m")
		return 2
	}
	allowedRooms := strings.Split(*rooms, ",")
	for index := range allowedRooms {
		allowedRooms[index] = strings.TrimSpace(allowedRooms[index])
	}
	issuedAt := now().UTC().Truncate(time.Second)
	scope := domain.ClientScope{
		UserID: *user, ProjectID: *project, Rooms: allowedRooms,
		IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(*ttl),
	}
	if err := scope.Validate(issuedAt); err != nil {
		fmt.Fprintln(stderr, "invalid project, user, rooms or token lifetime")
		return 2
	}
	cfg, err := loadAuth()
	if err != nil {
		fmt.Fprintln(stderr, "invalid authentication configuration")
		return 1
	}
	authenticator, err := auth.New(cfg.ClientTokenSecret, cfg.PublisherKeys)
	if err != nil {
		fmt.Fprintln(stderr, "invalid authentication configuration")
		return 1
	}
	token, err := authenticator.Issue(scope)
	if err != nil {
		fmt.Fprintln(stderr, "unable to issue token for the requested client scope")
		return 1
	}
	if _, err := fmt.Fprintln(stdout, token); err != nil {
		fmt.Fprintln(stderr, "unable to write token")
		return 1
	}
	return 0
}
