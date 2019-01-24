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
	"bytes"
	"context"
	"flag"
	"io/ioutil"
	"os"
	"os/exec"
	slashpath "path"
	"path/filepath"
	"strings"

	"golang.org/x/exp/errors"
	"golang.org/x/exp/errors/fmt"
)

func init_(ctx context.Context, pctx *processContext, args []string) error {
	f := flag.NewFlagSet("gocloud init", flag.ContinueOnError)
	f.SetOutput(pctx.stderr)
	module := f.String("module", "", "import path of the module")
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		f.SetOutput(pctx.stdout)
		f.PrintDefaults()
		return nil
	} else if err != nil {
		return usagef("%v", err)
	}
	if f.NArg() > 1 {
		return usagef("too many arguments to init")
	}
	fmt.Fprintln(pctx.stdout, "Welcome to Go Cloud!")
	dir := f.Arg(0)
	if dir == "" {
		dir = pctx.dir
	} else {
		dir = pctx.abs(dir)
	}
	if *module == "" {
		for {
			fmt.Fprint(pctx.stdout, "Import path? ")
			m, err := pctx.readLine()
			if err != nil {
				return err
			}
			m = strings.TrimSpace(m)
			if m != "" {
				*module = m
				break
			}
		}
	}
	if err := os.MkdirAll(dir, 0777); err != nil {
		return err
	}
	{
		c := exec.Command("git", "init")
		c.Dir = dir
		c.Stdout = pctx.stderr
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
	}
	{
		c := exec.Command("go", "mod", "init", *module)
		c.Dir = dir
		c.Stdout = pctx.stderr
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
	}
	{
		c := exec.Command("go", "get",
			"gocloud.dev@v0.9.0",
			"github.com/google/wire@v0.2.1",
			"github.com/gorilla/mux@v1.6.2",
			"github.com/spf13/viper@v1.3.1")
		c.Dir = dir
		c.Stdout = pctx.stderr
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
	}
	{
		c := exec.Command("go", "mod", "edit", "-replace=gocloud.dev=github.com/zombiezen/go-cloud@cli")
		c.Dir = dir
		c.Stdout = pctx.stderr
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
	}
	{
		buf := new(bytes.Buffer)
		fmt.Fprintf(buf, "/%s\n", slashpath.Base(*module))
		fmt.Fprintln(buf, "*.tfstate")
		fmt.Fprintln(buf, "*.tfstate.*")
		fmt.Fprintln(buf, ".terraform/")
		fmt.Fprintln(buf, "terraform.tfvars")
		err := ioutil.WriteFile(filepath.Join(dir, ".gitignore"), buf.Bytes(), 0666)
		if err != nil {
			return err
		}
	}
	{
		const mainSource = `package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

func main() {
	ctx := context.Background()
	pflag.String("address", "localhost:8080", "address to listen on")
	pflag.Parse()
	config := viper.New()
	config.SetDefault("trace_fraction", 0.05)
	config.BindPFlags(pflag.CommandLine)
	srv, cleanup, err := setup(ctx, config)
	if err != nil {
		log.Fatal(err)
	}
	err = srv.ListenAndServe(config.GetString("address"))
	cleanup()
	if err != nil {
		log.Fatal(err)
	}
}

type application struct {
}

func (app *application) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	router := mux.NewRouter()
	router.HandleFunc("/", app.index)
	router.ServeHTTP(w, r)
}

func (app *application) index(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "Hello, World!")
}
`
		err := ioutil.WriteFile(filepath.Join(dir, "main.go"), []byte(mainSource), 0666)
		if err != nil {
			return err
		}
	}
	{
		const setupSource = `// +build wireinject

package main

import (
	"context"
	"net/http"
	"os"

	"github.com/google/wire"
	"github.com/spf13/viper"
	"go.opencensus.io/trace"
	"gocloud.dev/health"
	"gocloud.dev/requestlog"
	"gocloud.dev/server"
)

func setup(ctx context.Context, config *viper.Viper) (*server.Server, func(), error) {
	wire.Build(
		server.Set,
		application{},
		wire.Bind(new(http.Handler), new(application)),
		requestLogger,
		wire.Bind(new(requestlog.Logger), new(requestlog.NCSALogger)),
		traceSampler,
		healthChecks,
		wire.InterfaceValue(new(trace.Exporter), trace.Exporter(nil)),
	)
	return nil, nil, nil
}

func healthChecks() []health.Checker {
	return []health.Checker{
		// Add application health checks here:
	}
}

func requestLogger() *requestlog.NCSALogger {
	return requestlog.NewNCSALogger(os.Stdout, func(error) {})
}

func traceSampler(config *viper.Viper) trace.Sampler {
	return trace.ProbabilitySampler(config.GetFloat64("trace_fraction"))
}
`
		err := ioutil.WriteFile(filepath.Join(dir, "setup.go"), []byte(setupSource), 0666)
		if err != nil {
			return err
		}
	}
	{
		c := exec.Command("wire", "./...")
		c.Dir = dir
		c.Stdout = pctx.stderr
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
	}
	return nil
}
