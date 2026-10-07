package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"bht1/internal/cli"
	"bht1/internal/server"
)

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("bserve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: bserve ROOT [PORT] [flags]")
		fs.PrintDefaults()
	}
	host := fs.String("host", "127.0.0.1", "address to listen on")
	idle := 30 * time.Second
	fs.Var(cli.Seconds{D: &idle}, "idle-timeout", "close connections idle this long (seconds or duration)")
	quiet := fs.Bool("q", false, "log warnings only")
	debug := fs.Bool("debug", false, "log every skipped frame and connection end")
	maxConns := fs.Int("max-conns", 1024, "concurrent connections before accept blocks")

	pos, err := cli.Parse(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if len(pos) < 1 || len(pos) > 2 {
		fs.Usage()
		return 2
	}
	port := 9000
	if len(pos) == 2 {
		if port, err = strconv.Atoi(pos[1]); err != nil || port < 0 || port > 65535 {
			fmt.Fprintf(stderr, "bserve: bad port %q\n", pos[1])
			return 2
		}
	}

	level := slog.LevelInfo
	switch {
	case *quiet:
		level = slog.LevelWarn
	case *debug:
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	srv, err := server.New(server.Config{
		Root:        pos[0],
		Addr:        net.JoinHostPort(*host, strconv.Itoa(port)),
		IdleTimeout: idle,
		MaxConns:    *maxConns,
		Logger:      logger,
	})
	if err != nil {
		fmt.Fprintf(stderr, "bserve: %v\n", err)
		return 2
	}
	if err := srv.Listen(); err != nil {
		fmt.Fprintf(stderr, "bserve: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve() }()

	select {
	case err := <-errc:
		if err != nil {
			fmt.Fprintf(stderr, "bserve: %v\n", err)
			return 1
		}
		return 0
	case <-ctx.Done():
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown cut connections short", "err", err)
	}
	<-errc
	return 0
}
