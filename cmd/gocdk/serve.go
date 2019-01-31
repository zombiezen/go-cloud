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
	"encoding/json"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"time"

	"golang.org/x/exp/errors"
	"golang.org/x/exp/errors/fmt"
	"golang.org/x/sys/unix"
)

func serve(ctx context.Context, pctx *processContext, args []string) error {
	f := flag.NewFlagSet("gocdk serve", flag.ContinueOnError)
	f.SetOutput(pctx.stderr)
	address := f.String("address", "localhost:8080", "address to serve on")
	serverPort := f.Int("server_port", 9090, "first server port")
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		f.SetOutput(pctx.stdout)
		f.PrintDefaults()
		return nil
	} else if err != nil {
		return usagef("%v", err)
	}
	if f.NArg() > 0 {
		return usagef("too many arguments to serve")
	}
	// TODO(light): Use inotify etc.
	watcher := make(chan os.Signal, 1)
	signal.Notify(watcher, unix.SIGUSR1)

	buildDir, err := ioutil.TempDir("", "gocdk-build")
	if err != nil {
		return err
	}
	defer os.RemoveAll(buildDir)

	myProxy := new(proxy)
	httpDone := make(chan error)
	go func() {
		srv := &http.Server{
			Addr:    *address,
			Handler: myProxy,
		}
		go func() {
			// TODO(light): Also wait for this.
			<-ctx.Done()
			srv.Shutdown(context.Background())
		}()
		httpDone <- srv.ListenAndServe()
	}()
	defer func() { <-httpDone }()
	serverPathA := filepath.Join(buildDir, "serverA")
	serverPathB := filepath.Join(buildDir, "serverB")
	configPathA := filepath.Join(buildDir, "configA.json")
	configPathB := filepath.Join(buildDir, "configB.json")
	portA, portB := *serverPort, (*serverPort)+1
	urlA := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("localhost:%d", portA),
		Path:   "/",
	}
	urlB := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("localhost:%d", portB),
		Path:   "/",
	}
	nextIsA := true
	var currServer *exec.Cmd
	defer func() {
		if currServer != nil {
			if err := currServer.Process.Signal(unix.SIGKILL); err != nil {
				log.Printf("kill server:", err)
			}
			currServer.Wait()
		}
	}()
	firstBuild := true
	for {
		log.Println("starting build")
		var serverPath string
		var configPath string
		var port int
		var serverURL *url.URL
		if nextIsA {
			serverPath, configPath, port, serverURL = serverPathA, configPathA, portA, urlA
		} else {
			serverPath, configPath, port, serverURL = serverPathB, configPathB, portB, urlB
		}
		err := buildServer(ctx, pctx.dir, serverPath, configPath, pctx.stderr)
		if err != nil {
			log.Printf("build error: %v", err)
			if firstBuild {
				myProxy.setBuildError(err)
			}
			select {
			case <-watcher:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		// Run server.
		newServer := exec.Command(serverPath, fmt.Sprintf("--address=localhost:%d", port), "--config="+configPath)
		newServer.Stdout = pctx.stdout
		newServer.Stderr = pctx.stderr
		if err := newServer.Start(); err != nil {
			log.Printf("running server: %v", err)
			if firstBuild {
				myProxy.setBuildError(err)
			}
			select {
			case <-watcher:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		// Server must become alive in 30 seconds.
		log.Println("starting new server")
		aliveCtx, aliveCancel := context.WithTimeout(ctx, 30*time.Second)
		err = waitForHealthy(aliveCtx, serverURL.ResolveReference(&url.URL{Path: "/healthz/liveness"}).String())
		aliveCancel()
		if err != nil {
			log.Print("server did not become healthy")
			select {
			case <-watcher:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		// Wait for server to be ready.
		err = waitForHealthy(ctx, serverURL.ResolveReference(&url.URL{Path: "/healthz/readiness"}).String())
		if err != nil {
			return err
		}

		// Cut over to new; kill old.
		myProxy.setBackend(serverURL)
		if firstBuild {
			// TODO(light): Actually check address from listener.
			log.Printf("serving at http://%s", *address)
		} else {
			log.Print("serving new version")
		}
		if currServer != nil {
			if err := currServer.Process.Signal(unix.SIGKILL); err != nil {
				log.Printf("kill server:", err)
			}
			currServer.Wait()
		}
		nextIsA = !nextIsA
		currServer = newServer
		firstBuild = false

		// Wait for code change or interrupt.
		select {
		case <-watcher:
			continue
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func buildServer(ctx context.Context, dir string, exePath string, cfgPath string, logOut io.Writer) error {
	{
		c := exec.Command("terraform", "refresh", "-input=false")
		c.Dir = filepath.Join(dir, "environments", "dev")
		c.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
		c.Stdout = logOut
		c.Stderr = logOut
		if err := c.Run(); err != nil {
			return fmt.Errorf("build server: refresh terraform state: %w", err)
		}
	}
	{
		c := exec.Command("terraform", "output", "-json", "server_config")
		c.Dir = filepath.Join(dir, "environments", "dev")
		c.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
		outBuf := new(bytes.Buffer)
		c.Stdout = outBuf
		c.Stderr = logOut
		if err := c.Run(); err != nil {
			return fmt.Errorf("build server: get server config: %w", err)
		}
		var outputVar struct {
			Value json.RawMessage
		}
		if err := json.Unmarshal(outBuf.Bytes(), &outputVar); err != nil {
			return fmt.Errorf("build server: get server config: %w", err)
		}
		if err := ioutil.WriteFile(cfgPath, []byte(outputVar.Value), 0666); err != nil {
			return fmt.Errorf("build server: write server config: %w", err)
		}
	}
	{
		c := exec.Command("wire", "./...")
		c.Dir = dir
		c.Env = append(os.Environ(), "GO111MODULE=on")
		c.Stdout = logOut
		c.Stderr = logOut
		if err := c.Run(); err != nil {
			return fmt.Errorf("build server: %w", err)
		}
	}
	{
		c := exec.Command("go", "build", "-o", exePath)
		c.Dir = dir
		c.Env = append(os.Environ(), "GO111MODULE=on")
		c.Stdout = logOut
		c.Stderr = logOut
		if err := c.Run(); err != nil {
			return fmt.Errorf("build server: %w", err)
		}
	}
	return nil
}

func waitForHealthy(ctx context.Context, urlstr string) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		req, err := http.NewRequest("GET", urlstr, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		if err == nil && 200 <= resp.StatusCode && resp.StatusCode < 300 {
			return nil
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type proxy struct {
	backend atomic.Value
}

func (p *proxy) setBackend(target *url.URL) {
	p.backend.Store(httputil.NewSingleHostReverseProxy(target))
}

func (p *proxy) setBuildError(e error) {
	p.backend.Store(e)
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch b := p.backend.Load().(type) {
	case nil:
		http.Error(w, "waiting for initial build...", http.StatusBadGateway)
	case error:
		http.Error(w, b.Error(), http.StatusInternalServerError)
	case *httputil.ReverseProxy:
		b.ServeHTTP(w, r)
	default:
		panic("unreachable")
	}
}
