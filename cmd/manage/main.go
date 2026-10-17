// Command manage performs explicit trusted database migrations/operator actions.
// It is separate from the public socketfabric developer CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"socketfabric/internal/adapters/outbound/auth"
	"socketfabric/internal/adapters/outbound/postgres"
	"socketfabric/internal/application/usecase"
	"socketfabric/internal/config"
	"socketfabric/internal/domain"
)

const help = `Trusted SocketFabric management tool (requires direct database access)

  manage migrate up
  manage migrate down --confirm-data-loss
  manage operator bootstrap-admin --email admin@example.com --password-stdin
  manage operator create --email viewer@example.com --role viewer --password-stdin
  manage operator set-password --email admin@example.com --password-stdin
  manage operator disable --email viewer@example.com
  manage operator enable --email viewer@example.com

Configuration: DATABASE_URL, APP_ENV and DATABASE_*; see docs/MANAGEMENT.md.
Passwords are read from stdin only and never printed. No default admin exists.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
func run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	if len(args) == 0 || len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(out, help)
		return 0
	}
	if len(args) < 2 {
		fmt.Fprintln(errOut, "error: expected command and action; use --help")
		return 2
	}
	command, action := args[0], args[1]
	if command != "migrate" && command != "operator" || command == "migrate" && action != "up" && action != "down" || command == "operator" && action != "bootstrap-admin" && action != "create" && action != "set-password" && action != "disable" && action != "enable" {
		fmt.Fprintln(errOut, "error: unknown management command; use --help")
		return 2
	}
	flags := flag.NewFlagSet("manage "+command+" "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	email := flags.String("email", "", "Operator email")
	role := flags.String("role", "viewer", "admin or viewer")
	passwordStdin := flags.Bool("password-stdin", false, "Read password from stdin")
	confirm := flags.Bool("confirm-data-loss", false, "Confirm destructive migration rollback")
	if err := flags.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, help)
			return 0
		}
		fmt.Fprintln(errOut, "error: invalid flags; use --help")
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(errOut, "error: unexpected arguments")
		return 2
	}
	password := ""
	if command == "migrate" && action == "down" && !*confirm {
		fmt.Fprintln(errOut, "error: down deletes management data; requires --confirm-data-loss")
		return 2
	}
	if command == "operator" {
		if _, err := domain.NormalizeEmail(*email); err != nil {
			fmt.Fprintln(errOut, "error: a valid --email is required")
			return 2
		}
		if action == "bootstrap-admin" || action == "create" || action == "set-password" {
			if !*passwordStdin {
				fmt.Fprintln(errOut, "error: --password-stdin is required")
				return 2
			}
			stopRead := context.AfterFunc(ctx, func() {
				if closer, ok := in.(io.Closer); ok {
					_ = closer.Close()
				}
			})
			body, err := io.ReadAll(io.LimitReader(in, 515))
			stopRead()
			if err != nil || len(body) > 514 {
				fmt.Fprintln(errOut, "error: cannot read bounded password input")
				return 2
			}
			password = strings.TrimSuffix(strings.TrimSuffix(string(body), "\n"), "\r")
			if password == "" {
				fmt.Fprintln(errOut, "error: password must not be empty")
				return 2
			}
		}
	}
	cfg, err := config.LoadManagement()
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		return 2
	}
	store, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.MaxConns, cfg.ConnectTimeout, cfg.QueryTimeout)
	if err != nil {
		fmt.Fprintln(errOut, "error: database configuration unavailable")
		return 1
	}
	defer store.Close()
	if command == "migrate" {
		err = store.Migrate(ctx, action == "down")
	} else {
		if err = store.CheckSchema(ctx); err == nil {
			service, createErr := usecase.NewManagement(store, auth.NewPasswords(), nil, slog.New(slog.NewJSONHandler(errOut, nil)), cfg.SessionTTL, cfg.MaxProjects, nil)
			err = createErr
			if err == nil {
				switch action {
				case "bootstrap-admin", "create":
					operatorRole := *role
					if action == "bootstrap-admin" {
						operatorRole = domain.RoleAdmin
					}
					_, err = service.ProvisionOperator(ctx, *email, password, operatorRole, action == "bootstrap-admin")
				case "set-password":
					err = service.UpdateOperatorSecurity(ctx, *email, password, "")
				case "disable":
					err = service.UpdateOperatorSecurity(ctx, *email, "", domain.StatusDisabled)
				case "enable":
					err = service.UpdateOperatorSecurity(ctx, *email, "", domain.StatusActive)
				}
			}
		}
	}
	password = ""
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			fmt.Fprintln(errOut, "error: operation conflicts with existing state (bootstrap is allowed only once)")
		case errors.Is(err, domain.ErrInvalid):
			fmt.Fprintln(errOut, "error: invalid operator configuration; password requires 15–128 characters and role admin/viewer")
		case errors.Is(err, domain.ErrResourceNotFound):
			fmt.Fprintln(errOut, "error: operator not found")
		default:
			fmt.Fprintln(errOut, "error: management operation unavailable; check database access and schema")
		}
		return 1
	}
	fmt.Fprintln(out, "Management operation completed.")
	return 0
}
