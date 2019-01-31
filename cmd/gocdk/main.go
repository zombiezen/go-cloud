// Copyright 2019 The Go Cloud Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"context"
	"flag"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"

	"golang.org/x/exp/errors"
	"golang.org/x/exp/errors/fmt"
	"golang.org/x/sys/unix"
)

func main() {
	pctx, err := osProcessContext()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gg:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sig, unix.SIGTERM, unix.SIGINT)
	go func() {
		select {
		case <-sig:
			cancel()
		case <-done:
		}
	}()
	err = gocdk(ctx, pctx, os.Args[1:])
	close(done)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		if isUsage(err) {
			os.Exit(64)
		}
		os.Exit(1)
	}
}

func gocdk(ctx context.Context, pctx *processContext, args []string) error {
	globalFlags := flag.NewFlagSet("gocdk", flag.ContinueOnError)
	globalFlags.SetOutput(pctx.stderr)
	if err := globalFlags.Parse(args); err == flag.ErrHelp {
		globalFlags.SetOutput(pctx.stdout)
		globalFlags.PrintDefaults()
		return nil
	} else if err != nil {
		return usagef("%v", err)
	}
	if globalFlags.NArg() == 0 {
		globalFlags.SetOutput(pctx.stdout)
		globalFlags.Usage()
		return nil
	}
	var err error
	switch name, args := globalFlags.Arg(0), globalFlags.Args()[1:]; name {
	case "init":
		err = init_(ctx, pctx, args)
	case "add":
		err = add(ctx, pctx, args)
	case "serve":
		err = serve(ctx, pctx, args)
	case "deploy":
		err = deploy(ctx, pctx, args)
	default:
		return usagef("unknown command %s", name)
	}
	if err != nil {
		return fmt.Errorf("gocdk: %w", err)
	}
	return nil
}

type processContext struct {
	dir string
	env []string

	stdin  *bufio.Scanner
	stdout io.Writer
	stderr io.Writer

	lookPath func(string) (string, error)
}

// osProcessContext returns the default process context from global variables.
func osProcessContext() (*processContext, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return &processContext{
		dir:      dir,
		env:      os.Environ(),
		stdin:    bufio.NewScanner(os.Stdin),
		stdout:   os.Stdout,
		stderr:   os.Stderr,
		lookPath: exec.LookPath,
	}, nil
}

func (pctx *processContext) readLine() (string, error) {
	if !pctx.stdin.Scan() {
		if err := pctx.stdin.Err(); err != nil {
			return "", err
		}
		return "", io.ErrUnexpectedEOF
	}
	return pctx.stdin.Text(), nil
}

func (pctx *processContext) abs(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(pctx.dir, path)
}

// getenv is like os.Getenv but reads from the given list of environment
// variables.
func getenv(environ []string, name string) string {
	// Later entries take precedence.
	for i := len(environ) - 1; i >= 0; i-- {
		e := environ[i]
		if strings.HasPrefix(e, name) && strings.HasPrefix(e[len(name):], "=") {
			return e[len(name)+1:]
		}
	}
	return ""
}

type usageError string

func usagef(format string, args ...interface{}) error {
	e := usageError(fmt.Sprintf(format, args...))
	return &e
}

func (ue *usageError) Error() string {
	return "gg: usage: " + string(*ue)
}

func isUsage(e error) bool {
	return errors.As(e, new(usageError))
}
