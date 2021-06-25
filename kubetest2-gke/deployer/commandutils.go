/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package deployer

import (
	"bytes"
	"fmt"
	"io"
	"os"
	osexec "os/exec"

	"path/filepath"
	"strings"

	"k8s.io/klog"

	"sigs.k8s.io/kubetest2/pkg/exec"
)

func (d *Deployer) PrepareGcpIfNeeded(projectID string) error {
	// TODO(RonWeber): This is an almost direct copy/paste from kubetest's prepareGcp()
	// It badly needs refactored.

	var endpoint string
	switch env := d.Environment; {
	case env == "test":
		endpoint = "https://test-container.sandbox.googleapis.com/"
	case env == "staging":
		endpoint = "https://staging-container.sandbox.googleapis.com/"
	case env == "staging2":
		endpoint = "https://staging2-container.sandbox.googleapis.com/"
	case env == "prod":
		endpoint = "https://container.googleapis.com/"
	case urlRe.MatchString(env):
		endpoint = env
	default:
		return fmt.Errorf("--environment must be one of {test,staging,staging2,prod} or match %v, found %q", urlRe, env)
	}

	if err := os.Setenv("CLOUDSDK_CORE_PRINT_UNHANDLED_TRACEBACKS", "1"); err != nil {
		return fmt.Errorf("could not set CLOUDSDK_CORE_PRINT_UNHANDLED_TRACEBACKS=1: %v", err)
	}
	if err := os.Setenv("CLOUDSDK_API_ENDPOINT_OVERRIDES_CONTAINER", endpoint); err != nil {
		return err
	}

	if err := runWithOutput(exec.RawCommand("gcloud config set project " + projectID)); err != nil {
		return fmt.Errorf("failed to set project %s: %w", projectID, err)
	}

	// gcloud creds may have changed
	if err := activateServiceAccount(d.GCPServiceAccount); err != nil {
		return err
	}

	if !d.GCPSSHKeyIgnored {
		// Ensure ssh keys exist
		klog.V(1).Info("Checking existing of GCP ssh keys...")
		k := filepath.Join(home(".ssh"), "google_compute_engine")
		if _, err := os.Stat(k); err != nil {
			return err
		}
		pk := k + ".pub"
		if _, err := os.Stat(pk); err != nil {
			return err
		}
	}

	//TODO(RonWeber): kubemark
	return nil
}

// Activate service account if set or do nothing.
func activateServiceAccount(path string) error {
	if path == "" {
		return nil
	}
	return runWithOutput(exec.RawCommand("gcloud auth activate-service-account --key-file=" + path))
}

// Get the project number for the given project ID.
func getProjectNumber(projectID string) (string, error) {
	// Get the service project number.
	projectNum, err := exec.Output(exec.RawCommand(
		fmt.Sprintf("gcloud projects describe %s --format=value(projectNumber)", projectID)))
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(projectNum)), nil
}

// home returns $HOME/part/part/part
func home(parts ...string) string {
	p := append([]string{os.Getenv("HOME")}, parts...)
	return filepath.Join(p...)
}

func getClusterCredentials(project, loc, cluster string) error {
	// Get gcloud to create the file.
	if err := runWithOutput(exec.Command("gcloud",
		containerArgs("clusters", "get-credentials", cluster, "--project="+project, loc)...),
	); err != nil {
		return fmt.Errorf("error executing get-credentials: %v", err)
	}

	return nil
}

func containerArgs(args ...string) []string {
	return append(append([]string{}, "container"), args...)
}

func runWithNoOutput(cmd exec.Cmd) error {
	exec.NoOutput(cmd)
	return cmd.Run()
}

func runWithOutput(cmd exec.Cmd) error {
	exec.InheritOutput(cmd)
	return cmd.Run()
}

func runWithOutputAndReturn(cmd exec.Cmd) (string, error) {
	var buf bytes.Buffer

	exec.SetOutput(cmd, io.MultiWriter(os.Stdout, &buf), io.MultiWriter(os.Stderr, &buf))
	if err := cmd.Run(); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

// execError returns a string format of err including stderr if the
// err is an ExitError, useful for errors from e.g. exec.Cmd.Output().
func execError(err error) string {
	if ee, ok := err.(*osexec.ExitError); ok {
		return fmt.Sprintf("%v (output: %q)", err, string(ee.Stderr))
	}
	return err.Error()
}
