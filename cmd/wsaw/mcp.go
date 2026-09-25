package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/logging"
	"github.com/pflege-de-labs/wsaw/internal/mcp"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// cmdMCP serves the stored results to an LLM client over MCP, read-only
// (Story 5.34).
func cmdMCP(ctx context.Context, args []string) error {
	return serveMCP(ctx, args, os.Stdin, os.Stdout, os.Stderr)
}

// serveMCP is cmdMCP with its streams passed in. stdout carries the protocol
// and nothing else, so logs and flag errors go to stderr.
func serveMCP(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(errOut)

	var cf configFlags

	cf.register(fs)

	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := cf.loadWithoutTargets()
	if err != nil {
		return err
	}

	// A registry of its own, so a DSN resolved from configuration cannot reach
	// the log with its password intact.
	secrets := &secret.Registry{}

	opts, err := app.StoreOptions(cfg, secrets)
	if err != nil {
		return err
	}

	logger, _, err := logging.New(logging.Options{
		Level:   cfg.Logging.Level,
		Format:  cfg.Logging.Format,
		Output:  errOut,
		Secrets: secrets,
	})
	if err != nil {
		return err
	}

	opts.Logger = logger

	if err := requireExistingStore(opts); err != nil {
		return err
	}

	// OpenForInspection applies no migration: bringing a schema up to date is
	// a write, and this command promises none (AC2).
	st, err := store.OpenForInspection(ctx, opts)
	if err != nil {
		return err
	}

	defer func() { _ = st.Close() }() // Nothing was written, so a close error loses nothing.

	logger.Info("mcp server ready", "store", opts.Location())

	return mcp.New(st, version, logger).Serve(ctx, in, out)
}

// requireExistingStore refuses a local store that is not there. Opening one
// creates the SQLite file and the artifact directory, and a read-only command
// that leaves an empty store behind has written something (AC2).
func requireExistingStore(opts store.Options) error {
	if opts.Driver == "" || opts.Driver == store.DriverSQLite {
		if err := requireExisting(opts.Path, "the result database"); err != nil {
			return err
		}
	}

	dir := opts.ArtifactDir
	if !store.IsArtifactURL(dir) {
		return requireExisting(dir, "the artifact directory")
	}

	// Only a file:// bucket is created by opening it. A remote bucket is not,
	// and a malformed URL is reported by the store with its reason.
	if u, err := url.Parse(dir); err == nil && u.Scheme == "file" {
		return requireExisting(u.Path, "the artifact directory")
	}

	return nil
}

func requireExisting(path, what string) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s %s does not exist; wsaw mcp reads an existing store and creates nothing", what, path)
		}

		return fmt.Errorf("checking %s %s: %w", what, path, err)
	}

	return nil
}
