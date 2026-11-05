package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/lcvoelker/SystemActivity/internal/db"
	"github.com/lcvoelker/SystemActivity/internal/session"
	"github.com/lcvoelker/SystemActivity/internal/storage"
	"github.com/lcvoelker/SystemActivity/internal/watcher"
)

const usage = `usage:
  filemaster migrate [up|status|version]                   as fm_owner (MIGRATE_DATABASE_URL)
  filemaster migrate [down|down-to N|redo|reset] --yes    roll back; lists what it drops first
  filemaster watch [-offline] [-idle 5m] <directory>       as fm_app   (DATABASE_URL)
  filemaster sessions [-n 10]                              recent sessions and their kind
  filemaster session <id>                                  one session's summary, with file names
  filemaster history <file>                                content versions of a file
  filemaster prune                                         delete data past the 3-day retention now

watch, sessions, history and prune apply pending migrations first when
MIGRATE_DATABASE_URL is set; leave it unset to keep owner credentials
out of the watcher.`

func main() {
	// .env is optional; variables already set in the environment win.
	_ = godotenv.Load()

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "migrate":
		err = runMigrate(ctx, os.Args[2:])
	case "watch":
		err = runWatch(ctx, os.Args[2:])
	case "sessions":
		err = runSessions(ctx, os.Args[2:])
	case "session":
		err = runSession(ctx, os.Args[2:])
	case "history":
		err = runHistory(ctx, os.Args[2:])
	case "prune":
		err = runPrune(ctx)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func runMigrate(ctx context.Context, args []string) error {
	dsn, err := requireEnv("MIGRATE_DATABASE_URL")
	if err != nil {
		return err
	}
	command := "up"
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}

	switch command {
	case "down", "down-to", "redo", "reset":
		var confirmed bool
		args, confirmed = takeFlag(args, "--yes", "-yes")
		plan, err := storage.RollbackPlan(ctx, dsn, command, args)
		if err != nil {
			return err
		}
		if len(plan) == 0 {
			fmt.Println("No applied migrations to undo.")
			return nil
		}
		fmt.Printf("migrate %s will undo:\n", command)
		for _, u := range plan {
			fmt.Printf("  %05d %s\n", u.Version, u.File)
			for _, d := range u.Drops {
				fmt.Printf("      %s\n", d)
			}
		}
		if command == "redo" {
			fmt.Println("and then apply it again; data in what it drops is not restored.")
		}
		if !confirmed {
			return fmt.Errorf("rolling back deletes the data listed above; re-run with --yes to proceed")
		}
	}
	return storage.Migrate(ctx, dsn, command, args...)
}

// takeFlag removes every occurrence of the given flag spellings from args
// and reports whether any was present.
func takeFlag(args []string, names ...string) ([]string, bool) {
	var rest []string
	found := false
	for _, a := range args {
		if slices.Contains(names, a) {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, found
}

func runWatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	offline := fs.Bool("offline", false, "print events without saving them to Postgres")
	idle := fs.Duration("idle", 5*time.Minute, "inactivity gap that ends a session")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: filemaster watch [-offline] [-idle 5m] <directory>")
	}
	root, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return err
	}

	var store session.Store = &session.OfflineStore{}
	if !*offline {
		pool, err := connect(ctx)
		if err != nil {
			return fmt.Errorf("connect to Postgres (use -offline to run without it): %w", err)
		}
		defer pool.Close()
		pg := storage.NewStore(pool, root)
		defer pg.Close() // runs before pool.Close, saving buffered activity
		store = pg

		if n, err := pg.CloseOrphans(ctx); err != nil {
			log.Printf("warning: closing sessions left open by a killed watcher: %v", err)
		} else if n > 0 {
			fmt.Printf("Closed %d session(s) left open by a previous watcher that was killed.\n", n)
		}
		go pg.RunRetention(ctx)
	}

	fmt.Println("Watching: ", root)
	fmt.Println("Type 'end' (or press Ctrl+C) to print the session summary and stop the watcher.")

	w, err := watcher.New()
	if err != nil {
		return err
	}
	if err := w.AddRecursive(root); err != nil {
		return err
	}

	manager := session.NewManager(root, *idle, store)
	events := make(chan session.Event, 100)
	end := make(chan struct{})

	go func() {
		reader := bufio.NewScanner(os.Stdin)
		for reader.Scan() {
			if strings.EqualFold(strings.TrimSpace(reader.Text()), "end") {
				close(end)
				return
			}
		}
	}()

	go w.Start(events)

	for {
		select {
		case <-ctx.Done():
			return finish(manager)
		case <-end:
			return finish(manager)
		case e := <-events:
			fmt.Printf("EVENT: %s %s\n", e.Op, e.Path)
			if err := manager.AddEvent(ctx, e); err != nil {
				log.Printf("warning: %v", err)
			}
		}
	}
}

// finish uses a fresh context because ctx may already be cancelled by Ctrl+C.
func finish(manager *session.Manager) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := manager.EndCurrentSession(ctx)
	return err
}

func runSessions(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	n := fs.Int("n", 10, "number of sessions to show")
	fs.Parse(args)

	q, closeDB, err := openQueries(ctx)
	if err != nil {
		return err
	}
	defer closeDB()

	sessions, err := q.RecentSessions(ctx, int32(*n))
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tENDED BY\tSTARTED\tDURATION\tEVENTS\tFILES\tCREATE\tWRITE\tRENAME\tREMOVE\tROOT")
	for _, s := range sessions {
		duration, kind, endedBy := "active", "-", "-"
		if s.EndedAt != nil {
			duration = s.EndedAt.Sub(s.StartedAt).Round(time.Second).String()
		}
		if s.Kind.Valid {
			kind = session.Kind(s.Kind.SessionKind).Label()
		}
		if s.EndReason.Valid {
			endedBy = string(s.EndReason.SessionEnd)
			if s.EndReason.SessionEnd == db.SessionEndRecovered {
				duration = "~" + duration // end time is the last saved activity
			}
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
			s.SessionID, kind, endedBy, s.StartedAt.Local().Format(time.DateTime), duration,
			s.TotalEvents, s.UniqueFiles, s.Creates, s.Writes, s.Renames, s.Removes, s.RootPath)
	}
	return tw.Flush()
}

// runSession prints a stored session's summary, with file names, in the
// same format as the summary printed when a watch ends.
func runSession(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: filemaster session <id>")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("session id %q is not a number", args[0])
	}

	q, closeDB, err := openQueries(ctx)
	if err != nil {
		return err
	}
	defer closeDB()

	s, err := q.SessionDetail(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("no session %d (sessions older than 3 days are deleted)", id)
	}
	if err != nil {
		return err
	}
	files, err := q.SessionFiles(ctx, id)
	if err != nil {
		return err
	}

	summary := session.Summary{
		SessionID: s.SessionID,
		Start:     s.StartedAt,
		End:       s.StartedAt,
		Counts: session.Counts{
			Creates: int(s.Creates),
			Writes:  int(s.Writes),
			Renames: int(s.Renames),
			Removes: int(s.Removes),
		},
	}
	switch {
	case s.EndedAt != nil:
		summary.End = *s.EndedAt
	case s.LastActivityAt != nil:
		summary.End = *s.LastActivityAt
	}
	if s.EndReason.Valid {
		summary.EndReason = session.EndReason(s.EndReason.SessionEnd)
	}
	perFile := make(map[string]session.Counts) // a file's totals across save windows
	for _, f := range files {
		oldPath := ""
		if f.OldPath != nil {
			oldPath = *f.OldPath
		}
		c := session.Counts{
			Creates: int(f.Creates),
			Writes:  int(f.Writes),
			Renames: int(f.Renames),
			Removes: int(f.Removes),
		}
		summary.Files.AddSaved(s.RootPath, f.Path, oldPath, c)
		t := perFile[f.Path]
		t.Creates, t.Writes = t.Creates+c.Creates, t.Writes+c.Writes
		t.Renames, t.Removes = t.Renames+c.Renames, t.Removes+c.Removes
		perFile[f.Path] = t
	}
	if s.Kind.Valid {
		summary.Kind = session.Kind(s.Kind.SessionKind)
	} else { // still open: classify what has been saved so far
		totals := make([]session.Counts, 0, len(perFile))
		for _, c := range perFile {
			totals = append(totals, c)
		}
		summary.Kind = session.Classify(session.BehaviorFromFiles(totals))
	}
	summary.Print(os.Stdout)
	if s.EndedAt == nil {
		fmt.Println("(still open: activity in a running watcher's buffer isn't saved yet)")
	}
	return nil
}

func runHistory(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: filemaster history <file>")
	}
	path, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}

	q, closeDB, err := openQueries(ctx)
	if err != nil {
		return err
	}
	defer closeDB()

	versions, err := q.FileHistory(ctx, path)
	if err != nil {
		return err
	}
	if len(versions) == 0 {
		fmt.Printf("no recorded versions for %s\n", path)
		return nil
	}

	fmt.Println(path)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SHA256\tSIZE\tFROM\tTO")
	var current []byte
	for _, v := range versions {
		to := "current"
		if v.ValidTo != nil {
			to = v.ValidTo.Local().Format(time.DateTime)
		} else {
			current = v.Sha256
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n",
			hex.EncodeToString(v.Sha256)[:12], v.SizeBytes, v.ValidFrom.Local().Format(time.DateTime), to)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if current != nil {
		dupes, err := q.PathsWithContent(ctx, db.PathsWithContentParams{Sha256: current, ExcludePath: path})
		if err != nil {
			return err
		}
		if len(dupes) > 0 {
			fmt.Println("\nSame content currently at:")
			for _, d := range dupes {
				fmt.Println("  " + d)
			}
		}
	}
	return nil
}

func runPrune(ctx context.Context) error {
	q, closeDB, err := openQueries(ctx)
	if err != nil {
		return err
	}
	defer closeDB()

	res, err := storage.PruneExpired(ctx, q)
	if err != nil {
		return err
	}
	fmt.Printf("Dropped %d day partitions and deleted %d sessions.\n", res.DroppedPartitions, res.DeletedSessions)
	return nil
}

func openQueries(ctx context.Context) (*db.Queries, func(), error) {
	pool, err := connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	return db.New(pool), pool.Close, nil
}

// connect opens the app-role pool, first applying any pending migrations
// as the schema owner when MIGRATE_DATABASE_URL is set. Leave it unset to
// keep owner credentials away from the watcher; upgrades then need
// `filemaster migrate up`.
func connect(ctx context.Context) (*pgxpool.Pool, error) {
	dsn, err := requireEnv("DATABASE_URL")
	if err != nil {
		return nil, err
	}
	pool, applied, err := storage.Connect(ctx, dsn, os.Getenv("MIGRATE_DATABASE_URL"))
	for _, v := range applied {
		fmt.Printf("Applied database migration %05d.\n", v)
	}
	return pool, err
}

func requireEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is not set (copy .env.example to .env and fill it in)", name)
	}
	return v, nil
}
