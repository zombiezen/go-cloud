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
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	slashpath "path"
	"path/filepath"
	"strings"

	"golang.org/x/exp/errors"
	"golang.org/x/exp/errors/fmt"
)

func deploy(ctx context.Context, pctx *processContext, args []string) error {
	f := flag.NewFlagSet("gocloud deploy", flag.ContinueOnError)
	f.SetOutput(pctx.stderr)
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		f.SetOutput(pctx.stdout)
		f.PrintDefaults()
		return nil
	} else if err != nil {
		return usagef("%v", err)
	}
	if f.NArg() > 0 {
		return usagef("too many arguments to deploy")
	}
	var shortName string
	{
		c := exec.Command("go", "list")
		c.Dir = pctx.dir
		sb := new(strings.Builder)
		c.Stdout = sb
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
		shortName = slashpath.Base(strings.TrimSuffix(sb.String(), "\n"))
	}

	// Generate missing files.
	dirEntries, err := ioutil.ReadDir(pctx.dir)
	if err != nil {
		return err
	}
	hasTerraform, hasCloudBuild, hasDockerfile := false, false, false
	for _, info := range dirEntries {
		name := info.Name()
		if name == "cloudbuild.yaml" {
			hasCloudBuild = true
		}
		if name == "Dockerfile" {
			hasDockerfile = true
		}
		if filepath.Ext(name) == ".tf" && info.Mode().IsRegular() {
			hasTerraform = true
		}
	}
	var projectID string
	if hasTerraform {
		{
			c := exec.Command("terraform", "refresh", "-input=false")
			c.Dir = pctx.dir
			c.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
			c.Stdout = pctx.stderr
			c.Stderr = pctx.stderr
			if err := c.Run(); err != nil {
				return err
			}
		}
		{
			c := exec.Command("terraform", "output", "-json", "gcp_project")
			c.Dir = pctx.dir
			c.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
			buf := new(bytes.Buffer)
			c.Stdout = buf
			c.Stderr = pctx.stderr
			if err := c.Run(); err != nil {
				return err
			}
			var gcpProject struct {
				Value string
			}
			if err := json.Unmarshal(buf.Bytes(), &gcpProject); err != nil {
				return fmt.Errorf("parse terraform output for gcp_project: %w", err)
			}
			projectID = gcpProject.Value
		}
	} else {
		fmt.Fprintln(pctx.stdout, "Go to https://console.cloud.google.com/projectcreate and get a project ID.")
		for {
			fmt.Fprint(pctx.stdout, "Project ID: ")
			var err error
			projectID, err = pctx.readLine()
			if err != nil {
				return err
			}
			projectID = strings.TrimSpace(projectID)
			if projectID != "" {
				break
			}
		}

		buf := new(bytes.Buffer)
		fmt.Fprintf(buf, "provider \"google\" {\n")
		fmt.Fprintf(buf, "	version = \"~>1.15\"\n")
		fmt.Fprintf(buf, "	project = %q\n", projectID)
		fmt.Fprintf(buf, "}\n")
		fmt.Fprintln(buf)
		fmt.Fprintf(buf, "resource \"google_project_service\" \"cloudbuild\" {\n")
		fmt.Fprintf(buf, "  service            = \"cloudbuild.googleapis.com\"\n")
		fmt.Fprintf(buf, "  disable_on_destroy = false\n")
		fmt.Fprintf(buf, "}\n")
		fmt.Fprintln(buf)
		fmt.Fprintf(buf, "resource \"google_project_service\" \"trace\" {\n")
		fmt.Fprintf(buf, "  service            = \"cloudtrace.googleapis.com\"\n")
		fmt.Fprintf(buf, "  disable_on_destroy = false\n")
		fmt.Fprintf(buf, "}\n")
		err := ioutil.WriteFile(filepath.Join(pctx.dir, "main.tf"), buf.Bytes(), 0666)
		if err != nil {
			return err
		}

		buf.Reset()
		fmt.Fprintf(buf, "output \"gcp_project\" {\n")
		fmt.Fprintf(buf, "	value       = %q\n", projectID)
		fmt.Fprintf(buf, "	description = \"The GCP project ID.\"\n")
		fmt.Fprintf(buf, "}\n")
		err = ioutil.WriteFile(filepath.Join(pctx.dir, "outputs.tf"), buf.Bytes(), 0666)
		if err != nil {
			return err
		}
	}
	if !hasCloudBuild {
		// TODO(light): Properly escape short name.
		buf := new(bytes.Buffer)
		fmt.Fprintf(buf, "steps:\n")
		fmt.Fprintf(buf, "- name: 'gcr.io/cloud-builders/docker'\n")
		fmt.Fprintf(buf, "  args: ['build', '-t', 'gcr.io/$PROJECT_ID/%s:$BUILD_ID', '.']\n", shortName)
		fmt.Fprintf(buf, "images:\n")
		fmt.Fprintf(buf, "- 'gcr.io/$PROJECT_ID/%s:$BUILD_ID'\n", shortName)
		err := ioutil.WriteFile(filepath.Join(pctx.dir, "cloudbuild.yaml"), buf.Bytes(), 0666)
		if err != nil {
			return err
		}
	}
	if !hasDockerfile {
		// TODO(light): Properly escape short name.
		buf := new(bytes.Buffer)
		fmt.Fprintf(buf, "# Step 1: Build Go binary.\n")
		fmt.Fprintf(buf, "FROM golang:1.11 as build\n")
		fmt.Fprintf(buf, "ENV GO111MODULE on\n")
		fmt.Fprintf(buf, "COPY . /go/src/%s\n", shortName)
		fmt.Fprintf(buf, "WORKDIR /go/src/%s\n", shortName)
		fmt.Fprintf(buf, "RUN go build\n")
		fmt.Fprintf(buf, "\n")
		fmt.Fprintf(buf, "# Step 2: Create image with built Go binary.\n")
		fmt.Fprintf(buf, "FROM debian:stretch-slim\n")
		fmt.Fprintf(buf, "COPY --from=build /go/src/%s/%s /\n", shortName, shortName)
		fmt.Fprintf(buf, "# Expose 8080 for health checks\n")
		fmt.Fprintf(buf, "EXPOSE 8080\n")
		fmt.Fprintf(buf, "ENTRYPOINT [%q]\n", shortName)
		err := ioutil.WriteFile(filepath.Join(pctx.dir, "Dockerfile"), buf.Bytes(), 0666)
		if err != nil {
			return err
		}
	}

	// Run commands.
	{
		c := exec.Command("terraform", "init", "-input=false")
		c.Dir = pctx.dir
		c.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
		c.Stdout = pctx.stderr
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
	}
	planFile, err := ioutil.TempFile("", "gocloud-tfplan")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(planFile.Name()); err != nil {
			log.Printf("Cleaning up plan: %v", err)
		}
	}()
	{
		c := exec.Command("terraform", "plan", "-input=false", "-out="+planFile.Name())
		c.Dir = pctx.dir
		c.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
		c.Stdout = pctx.stderr
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
	}
	fmt.Fprint(pctx.stdout, "OK to proceed (yes)? ")
	if line, err := pctx.readLine(); err != nil {
		return err
	} else if line != "yes" {
		return errors.New("canceled")
	}
	{
		c := exec.Command("terraform", "apply", "-input=false", planFile.Name())
		c.Dir = pctx.dir
		c.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
		c.Stdout = pctx.stderr
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
	}
	{
		c := exec.Command("gcloud", "--project="+projectID, "builds", "submit", ".")
		c.Dir = pctx.dir
		c.Stdout = pctx.stderr
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
	}
	return nil
}
