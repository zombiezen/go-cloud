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
	cryptorand "crypto/rand"
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	mathrand "math/rand"
	"os"
	"os/exec"
	slashpath "path"
	"path/filepath"
	"strings"

	"golang.org/x/exp/errors"
	"golang.org/x/exp/errors/fmt"
)

func deploy(ctx context.Context, pctx *processContext, args []string) error {
	f := flag.NewFlagSet("gocdk deploy", flag.ContinueOnError)
	deployEnv := f.String("env", "prod", "Environment to deploy")
	runTerraform := f.Bool("terraform", true, "Apply Terraform")
	useCloudBuild := f.Bool("cloudbuild", false, "Use Cloud Build instead of local Docker")
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
	hasDockerfile := false
	if _, err := os.Stat(filepath.Join(pctx.dir, "Dockerfile")); err == nil {
		hasDockerfile = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check for Dockerfile: %w", err)
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
		fmt.Fprintf(buf, "EXPOSE 80\n")
		fmt.Fprintf(buf, "ENTRYPOINT [%q, %q]\n", "/"+shortName, "--address=:80")
		err := ioutil.WriteFile(filepath.Join(pctx.dir, "Dockerfile"), buf.Bytes(), 0666)
		if err != nil {
			return err
		}
	}

	envDir := filepath.Join(pctx.dir, "environments", *deployEnv)
	if *deployEnv == "prod" {
		hasTerraform := false
		dirEntries, err := ioutil.ReadDir(envDir)
		if err == nil {
			for _, info := range dirEntries {
				name := info.Name()
				if filepath.Ext(name) == ".tf" && info.Mode().IsRegular() {
					hasTerraform = true
					break
				}
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("deploy: check prod environment: %w", err)
		}
		if !hasTerraform {
			fmt.Fprintln(pctx.stdout, "Go to https://console.cloud.google.com/projectcreate and get a project ID.")
			var projectID string
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

			if err := os.MkdirAll(envDir, 0777); err != nil {
				return fmt.Errorf("deploy: create prod environment: %w", err)
			}
			buf := new(bytes.Buffer)
			fmt.Fprintf(buf, "provider \"google\" {\n")
			fmt.Fprintf(buf, "	version = \"~>1.15\"\n")
			fmt.Fprintf(buf, "	project = %q\n", projectID)
			fmt.Fprintf(buf, "}\n")
			fmt.Fprintln(buf)
			buf.WriteString(`resource "google_project_service" "cloudbuild" {
  service            = "cloudbuild.googleapis.com"
  disable_on_destroy = false
}

resource "google_project_service" "compute" {
  service            = "compute.googleapis.com"
  disable_on_destroy = false
}

resource "google_project_service" "trace" {
  service            = "cloudtrace.googleapis.com"
  disable_on_destroy = false
}

resource "google_compute_firewall" "allow_http" {
  name = "default-allow-http"
  network = "default"
  source_ranges = ["0.0.0.0/0"]
  target_tags = ["http-server"]
  allow {
    protocol = "tcp"
    ports = ["80"]
  }
}

data "google_compute_image" "cos" {
  family  = "cos-stable"
  project = "cos-cloud"
}` + "\n")
			fmt.Fprintln(buf)
			fmt.Fprintf(buf, "resource \"google_compute_instance_group\" \"servers\" {\n")
			fmt.Fprintf(buf, "  name        = %q\n", shortName+"-servers")
			fmt.Fprintf(buf, "  description = %q\n", "Instances running the "+shortName+" service.")
			fmt.Fprintf(buf, "  zone        = %q\n", "us-central1-a")
			fmt.Fprintf(buf, "  depends_on  = [\"google_project_service.compute\"]\n")
			fmt.Fprintf(buf, "}\n")
			fmt.Fprintln(buf)
			fmt.Fprintf(buf, "resource \"google_compute_instance_template\" \"server\" {\n")
			fmt.Fprintf(buf, "  name         = %q\n", shortName+"-server")
			fmt.Fprintf(buf, "  description  = %q\n", "Machine template for running the "+shortName+" service.")
			fmt.Fprintf(buf, "  machine_type = \"f1-micro\"\n")
			fmt.Fprintf(buf, "  tags         = [\"http-server\"]\n")
			fmt.Fprintln(buf)
			buf.WriteString(`  disk {
			source_image = "${data.google_compute_image.cos.self_link}"
			boot         = true
			auto_delete  = true
			disk_size_gb = 10
		}`)
			fmt.Fprintln(buf)
			buf.WriteString(`  network_interface {
			network = "default"
			access_config {}
		}`)
			fmt.Fprintf(buf, "  service_account {\n")
			fmt.Fprintf(buf, "    scopes = [\"cloud-platform\"]\n")
			fmt.Fprintf(buf, "  }\n")
			fmt.Fprintf(buf, "  depends_on  = [\"google_project_service.compute\"]\n")
			fmt.Fprintf(buf, "}\n")
			err := ioutil.WriteFile(filepath.Join(envDir, "main.tf"), buf.Bytes(), 0666)
			if err != nil {
				return err
			}

			buf.Reset()
			fmt.Fprintf(buf, "output \"gcp_project\" {\n")
			fmt.Fprintf(buf, "	value       = %q\n", projectID)
			fmt.Fprintf(buf, "	description = \"The GCP project ID.\"\n")
			fmt.Fprintf(buf, "}\n\n")
			buf.WriteString(`output "gce_zone" {
  value       = "${google_compute_instance_group.servers.zone}"
  description = "Compute Engine zone to create instances in."
}

output "gce_instance_group" {
  value       = "${google_compute_instance_group.servers.name}"
  description = "Compute Engine group to create instances in."
}

output "gce_instance_template" {
  value       = "${google_compute_instance_template.server.name}"
  description = "Compute Engine instance template to use when creating instances."
}`)
			err = ioutil.WriteFile(filepath.Join(envDir, "outputs.tf"), buf.Bytes(), 0666)
			if err != nil {
				return err
			}
		}
	}

	// Run commands.
	{
		c := exec.Command("terraform", "init", "-input=false")
		c.Dir = envDir
		c.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
		c.Stdout = pctx.stderr
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
	}
	if *runTerraform {
		planFile, err := ioutil.TempFile("", "gocdk-tfplan")
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
			c.Dir = envDir
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
			c.Dir = envDir
			c.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
			c.Stdout = pctx.stderr
			c.Stderr = pctx.stderr
			if err := c.Run(); err != nil {
				return err
			}
		}
	}
	type tfString struct {
		Value string
	}
	var tfOutput struct {
		GCPProjectID        tfString `json:"gcp_project"`
		GCEZone             tfString `json:"gce_zone"`
		GCEInstanceGroup    tfString `json:"gce_instance_group"`
		GCEInstanceTemplate tfString `json:"gce_instance_template"`
		ServerConfig        struct {
			Value json.RawMessage
		} `json:"server_config"`
	}
	{
		c := exec.Command("terraform", "output", "-json")
		c.Dir = envDir
		c.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
		out := new(bytes.Buffer)
		c.Stdout = out
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
		if err := json.Unmarshal(out.Bytes(), &tfOutput); err != nil {
			return err
		}
	}
	imageName := "gcr.io/" + tfOutput.GCPProjectID.Value + "/" + shortName
	if *useCloudBuild {
		c := exec.Command("gcloud", "--project="+tfOutput.GCPProjectID.Value, "builds", "submit", "-t", imageName, ".")
		c.Dir = pctx.dir
		c.Stdout = pctx.stderr
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
	} else {
		{
			c := exec.Command("docker", "build", "-t", imageName, ".")
			c.Dir = pctx.dir
			c.Stdout = pctx.stderr
			c.Stderr = pctx.stderr
			if err := c.Run(); err != nil {
				return err
			}
		}
		var digest string
		{
			// TODO(light): Get the exact image from the build step or generate a unique-ish tag.
			c := exec.Command("docker", "image", "inspect", "-f", "{{index .RepoDigests 0}}", imageName)
			c.Dir = pctx.dir
			out := new(strings.Builder)
			c.Stdout = out
			c.Stderr = pctx.stderr
			if err := c.Run(); err != nil {
				return err
			}
			digest = strings.TrimSuffix(out.String(), "\n")
		}
		{
			c := exec.Command("gcloud", "--project="+tfOutput.GCPProjectID.Value, "auth", "configure-docker")
			c.Dir = pctx.dir
			c.Stdout = pctx.stderr
			c.Stderr = pctx.stderr
			if err := c.Run(); err != nil {
				return err
			}
		}
		{
			c := exec.Command("docker", "push", imageName)
			c.Dir = pctx.dir
			c.Stdout = pctx.stderr
			c.Stderr = pctx.stderr
			if err := c.Run(); err != nil {
				return err
			}
		}
		imageName += "@" + digest
	}
	instanceSuffix, err := randomDigits(6)
	if err != nil {
		return err
	}
	instanceName := shortName + "-" + instanceSuffix
	cloudConfig := map[string]interface{}{
		"write_files": []map[string]interface{}{
			{
				"path":        "/etc/mycontainer/config.json",
				"permissions": 0644,
				"owner":       "root",
				"content":     string(tfOutput.ServerConfig.Value),
			},
			{
				"path":        "/etc/systemd/system/mycontainer.service",
				"permissions": 0644,
				"owner":       "root",
				"content": "[Unit]\nDescription = Run container built by Go CDK\n" +
					"Wants=gcr-online.target\n" +
					"After=gcr-online.target\n" +
					"[Service]\n" +
					"Environment=\"HOME=/home/mycontainer\"\n" +
					"ExecStartPre=/usr/bin/docker-credential-gcr configure-docker\n" +
					"ExecStart=/usr/bin/docker run --rm -p 80:80 -v /etc/mycontainer:/etc/mycontainer:ro --name=mycontainer " + imageName + " --config=/etc/mycontainer/config.json\n" +
					"ExecStop=/usr/bin/docker stop mycontainer\n" +
					"ExecStopPost=/usr/bin/docker rm mycontainer\n",
			},
		},
		"runcmd": []string{
			"systemctl daemon-reload",
			"systemctl start mycontainer.service",
		},
	}
	cloudConfigJSON, err := json.Marshal(cloudConfig)
	if err != nil {
		return err
	}
	cloudConfigFile, err := ioutil.TempFile("", "gocdk-cloud-config")
	if err != nil {
		return err
	}
	defer func() {
		name := cloudConfigFile.Name()
		cloudConfigFile.Close()
		os.Remove(name)
	}()
	if _, err := cloudConfigFile.WriteString("#cloud-config\n"); err != nil {
		return err
	}
	if _, err := cloudConfigFile.Write(cloudConfigJSON); err != nil {
		return err
	}
	{
		c := exec.Command("gcloud", "--project="+tfOutput.GCPProjectID.Value, "compute", "instances", "create",
			instanceName,
			"--zone="+tfOutput.GCEZone.Value,
			"--metadata-from-file=user-data="+cloudConfigFile.Name(),
			"--source-instance-template="+tfOutput.GCEInstanceTemplate.Value,
		)
		c.Dir = pctx.dir
		c.Stdout = pctx.stderr
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
	}
	{
		c := exec.Command("gcloud", "--project="+tfOutput.GCPProjectID.Value, "compute", "instance-groups", "unmanaged", "add-instances",
			tfOutput.GCEInstanceGroup.Value,
			"--instances="+instanceName,
			"--zone="+tfOutput.GCEZone.Value,
		)
		c.Dir = pctx.dir
		c.Stdout = pctx.stderr
		c.Stderr = pctx.stderr
		if err := c.Run(); err != nil {
			return err
		}
	}

	return nil
}

func randomDigits(n int) (string, error) {
	var seedBytes [8]byte
	if _, err := cryptorand.Read(seedBytes[:]); err != nil {
		return "", fmt.Errorf("random digits: %w", err)
	}
	seed := int64(seedBytes[0]) |
		int64(seedBytes[1])<<8 |
		int64(seedBytes[2])<<16 |
		int64(seedBytes[3])<<24 |
		int64(seedBytes[4])<<32 |
		int64(seedBytes[5])<<40 |
		int64(seedBytes[6])<<48 |
		int64(seedBytes[7])<<56
	prng := mathrand.New(mathrand.NewSource(seed))
	sb := new(strings.Builder)
	for i := 0; i < n; i++ {
		sb.WriteByte('0' + byte(prng.Intn(10)))
	}
	return sb.String(), nil
}
