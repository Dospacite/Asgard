package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rousoftware/asgard/internal/app"
	"github.com/rousoftware/asgard/internal/auth"
	"github.com/rousoftware/asgard/internal/config"
	"github.com/rousoftware/asgard/internal/store"
	"golang.org/x/term"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("Asgard stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	command := "serve"
	args := os.Args[1:]
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	switch command {
	case "serve":
		application, err := app.New(cfg)
		if err != nil {
			return err
		}
		defer application.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return application.Run(ctx)
	case "admin":
		return adminCommand(cfg, args)
	case "doctor":
		return doctor(cfg)
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func adminCommand(cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: asgard admin create|reset-password --username <name>")
	}
	action := args[0]
	flags := flag.NewFlagSet("admin "+action, flag.ContinueOnError)
	username := flags.String("username", "", "administrator username")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*username) == "" {
		return errors.New("--username is required")
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	database, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	signer, err := auth.LoadOrCreateSigner(filepath.Join(cfg.DataDir, "keys", "jwt_ed25519.pem"), cfg.PublicURL)
	if err != nil {
		return err
	}
	service := auth.New(database, signer, cfg.PublicURL, cfg.SecureCookies, cfg.AccessTTL, cfg.RefreshTTL)
	password, err := readPassword()
	if err != nil {
		return err
	}
	switch action {
	case "create":
		user, err := service.CreateUser(context.Background(), *username, password)
		if err != nil {
			return err
		}
		fmt.Printf("Created administrator %s (%s)\n", user.Username, user.ID)
		return nil
	case "reset-password":
		if err := service.ResetPassword(context.Background(), *username, password); err != nil {
			return err
		}
		fmt.Printf("Reset password and revoked sessions for %s\n", *username)
		return nil
	default:
		return fmt.Errorf("unknown admin action %q", action)
	}
}

func readPassword() (string, error) {
	if value := os.Getenv("ASGARD_ADMIN_PASSWORD"); value != "" {
		return value, nil
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Password (minimum 14 characters): ")
		first, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(os.Stderr, "Confirm password: ")
		second, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(first) != string(second) {
			return "", errors.New("passwords do not match")
		}
		return string(first), nil
	}
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return "", errors.New("password input is required")
	}
	return scanner.Text(), scanner.Err()
}

func doctor(cfg config.Config) error {
	application, err := app.New(cfg)
	if err != nil {
		return err
	}
	defer application.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5_000_000_000)
	defer cancel()
	if err := application.Store.Health(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if err := application.Docker.Ping(ctx); err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	fmt.Println("database: ok")
	fmt.Println("docker: ok")
	fmt.Println("configuration: ok")
	return nil
}

func usage() {
	fmt.Println(`Asgard — a small single-host application cloud

Usage:
  asgard serve
  asgard admin create --username <name>
  asgard admin reset-password --username <name>
  asgard doctor
  asgard version`)
}
