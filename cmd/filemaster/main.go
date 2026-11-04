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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/joho/godotenv"

	"github.com/lcvoelker/SystemActivity/internal/db"
	"github.com/lcvoelker/SystemActivity/internal/session"
	"github.com/lcvoelker/SystemActivity/internal/storage"
	"github.com/lcvoelker/SystemActivity/internal/watcher"
)

const usage = `usage:
  filemaster migrate [up|down|status|version|redo|reset]   as fm_owner (MIGRATE_DATABASE_URL)
  filemaster watch [-offline] [-idle 5m] <directory>       as fm_app   (DATABASE_URL)
  filemaster sessions [-n 10]                              recent sessions and their kind
  filemaster history <file>                                content versions of a file
  filemaster prune                                         delete data past the 3-day retention now`

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
	return storage.Migrate(ctx, dsn, command, args...)
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
		dsn, err := requireEnv("DATABASE_URL")
		if err != nil {
			return err
		}
		pool, err := storage.NewPool(ctx, dsn)
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

	manager := session.NewManager(*idle, store)
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
	fmt.Fprintln(tw, "ID\tKIND\tSTARTED\tDURATION\tEVENTS\tFILES\tCREATE\tWRITE\tRENAME\tREMOVE\tROOT")
	for _, s := range sessions {
		duration, kind := "active", "-"
		if s.EndedAt != nil {
			duration = s.EndedAt.Sub(s.StartedAt).Round(time.Second).String()
		}
		if s.Kind.Valid {
			kind = string(s.Kind.SessionKind)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
			s.SessionID, kind, s.StartedAt.Local().Format(time.DateTime), duration,
			s.TotalEvents, s.UniqueFiles, s.Creates, s.Writes, s.Renames, s.Removes, s.RootPath)
	}
	return tw.Flush()
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
	dsn, err := requireEnv("DATABASE_URL")
	if err != nil {
		return nil, nil, err
	}
	pool, err := storage.NewPool(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	return db.New(pool), pool.Close, nil
}

func requireEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is not set (copy .env.example to .env and fill it in)", name)
	}
	return v, nil
}
