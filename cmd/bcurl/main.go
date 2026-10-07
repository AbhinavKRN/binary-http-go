package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"bht1/internal/cli"
	"bht1/internal/client"
	"bht1/internal/wire"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

type headerFlags wire.Headers

func (h *headerFlags) String() string { return fmt.Sprint(*h) }

func (h *headerFlags) Set(v string) error {
	name, value, ok := strings.Cut(v, ":")
	if !ok {
		return fmt.Errorf("expected name:value, got %q", v)
	}
	*h = append(*h, wire.Header{Name: strings.ToLower(strings.TrimSpace(name)), Value: strings.TrimSpace(value)})
	return nil
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bcurl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: bcurl [flags] [bht://]host[:port]/path...")
		fs.PrintDefaults()
	}
	var verbose, head bool
	fs.BoolVar(&verbose, "v", false, "hexdump every frame to stderr")
	fs.BoolVar(&verbose, "verbose", false, "same as -v")
	fs.BoolVar(&head, "I", false, "send HEAD instead of GET")
	fs.BoolVar(&head, "head", false, "same as -I")
	output := fs.String("o", "", "write bodies to this file instead of stdout")
	var headers headerFlags
	fs.Var(&headers, "H", "extra request header name:value (repeatable)")
	maxData := fs.Int("max-data", wire.MaxFrameSize, "largest DATA payload to send")
	timeout := 10 * time.Second
	fs.Var(cli.Seconds{D: &timeout}, "timeout", "connect timeout and longest silence (seconds or duration)")

	urls, err := cli.Parse(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return client.ExitOK
		}
		return client.ExitUsage
	}
	if len(urls) == 0 {
		fs.Usage()
		return client.ExitUsage
	}
	targets := make([]client.Target, 0, len(urls))
	for _, u := range urls {
		t, err := client.ParseURL(u)
		if err != nil {
			fmt.Fprintf(stderr, "bcurl: %v\n", err)
			return client.ExitUsage
		}
		targets = append(targets, t)
	}
	for _, t := range targets[1:] {
		if t.Authority() != targets[0].Authority() {
			fmt.Fprintf(stderr, "bcurl: all URLs must share one host:port, got %s and %s\n", targets[0].Authority(), t.Authority())
			return client.ExitUsage
		}
	}

	method := "GET"
	if head {
		method = "HEAD"
	}
	opts := client.Options{Timeout: timeout, MaxData: *maxData, Headers: wire.Headers(headers)}
	if verbose {
		opts.Trace = stderr
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := client.Fetch(ctx, targets, method, opts)

	var ce *client.ConnectError
	switch {
	case errors.As(err, &ce):
		fmt.Fprintf(stderr, "bcurl: cannot connect to %s: %v\n", targets[0].Authority(), ce.Err)
	case err != nil:
		fmt.Fprintf(stderr, "bcurl: %v\n", err)
	}
	if res == nil {
		return client.ExitCode(res, err)
	}
	for _, e := range res.Errors {
		fmt.Fprintf(stderr, "bcurl: server sent ERROR %d: %s\n", e.Status, e.Reason)
	}

	sink := stdout
	if *output != "" {
		f, err := os.Create(*output)
		if err != nil {
			fmt.Fprintf(stderr, "bcurl: %v\n", err)
			return client.ExitUsage
		}
		defer f.Close()
		sink = f
	}
	w := bufio.NewWriter(sink)
	for _, ex := range res.Exchanges {
		switch {
		case ex.Err != nil:
			fmt.Fprintf(stderr, "* %s %s -> failed: %v\n", ex.Method, ex.Target.Path, ex.Err)
		case ex.Response != nil:
			if verbose || ex.Response.Status >= 400 {
				fmt.Fprintf(stderr, "* %s %s -> %d (%d bytes)\n", ex.Method, ex.Target.Path, ex.Response.Status, len(ex.Response.Body))
			}
			w.Write(ex.Response.Body)
		}
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(stderr, "bcurl: writing output: %v\n", err)
		return client.ExitUsage
	}
	return client.ExitCode(res, err)
}
