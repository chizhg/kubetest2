/*
Copyright 2020 The Kubernetes Authors.

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
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/pkg/math"
	"golang.org/x/sync/errgroup"
	"k8s.io/klog"

	"sigs.k8s.io/kubetest2/pkg/exec"
	"sigs.k8s.io/kubetest2/pkg/metadata"
)

// Deployer implementation methods below
func (d *Deployer) Up() error {
	if err := d.Init(); err != nil {
		return err
	}

	defer func() {
		if d.RepoRoot == "" {
			klog.Warningf("repo-root not supplied, skip dumping cluster logs")
			return
		}
		if err := d.DumpClusterLogs(); err != nil {
			klog.Warningf("Dumping cluster logs at the end of Up() failed: %v", err)
		}
	}()

	// Only run prepare once for the first GCP project.
	if err := d.PrepareGcpIfNeeded(d.Projects[0]); err != nil {
		return err
	}
	if err := d.CreateNetwork(); err != nil {
		return err
	}
	if err := d.CreateClusters(); err != nil {
		return fmt.Errorf("error creating the clusters: %w", err)
	}

	if err := d.TestSetup(); err != nil {
		return fmt.Errorf("error running setup for the tests: %w", err)
	}

	return nil
}

func (d *Deployer) CreateClusters() error {
	klog.V(2).Infof("Environment: %v", os.Environ())

	totalTryCount := math.Max(len(d.Regions), len(d.Zones))
	for retryCount := 0; retryCount < totalTryCount; retryCount++ {
		d.retryCount = retryCount

		if err := d.CreateSubnets(); err != nil {
			return err
		}
		if err := d.SetupNetwork(); err != nil {
			return err
		}

		eg := new(errgroup.Group)
		locationArg := locationFlag(d.Regions, d.Zones, retryCount)
		for i := range d.Projects {
			project := d.Projects[i]
			clusters := d.projectClustersLayout[project]
			subNetworkArgs := subNetworkArgs(d.Autopilot, d.Projects, regionFromLocation(d.Regions, d.Zones, d.retryCount), d.Network, i)
			for j := range clusters {
				cluster := clusters[j]
				eg.Go(
					func() error {
						return d.CreateCluster(project, cluster, subNetworkArgs, locationArg)
					},
				)
			}
		}

		r := retryCount
		if err := eg.Wait(); err != nil {
			if d.isRetryableError(err) {
				go func() {
					d.DeleteClusters(r)
					if err := d.DeleteSubnets(r); err != nil {
						log.Printf("Warning: error encountered deleting subnets: %v", err)
					}
				}()
			} else {
				return fmt.Errorf("error creating clusters: %v", err)
			}

		} else {
			return nil
		}
	}

	return nil
}

// isRetryableError checks if the error happens during cluster creation can be potentially solved by retrying or not.
func (d *Deployer) isRetryableError(err error) bool {
	for _, regx := range d.retryableErrorPatternsCompiled {
		if regx.MatchString(err.Error()) {
			return true
		}
	}
	return false
}

func (d *Deployer) CreateCluster(project string, cluster cluster, subNetworkArgs []string, locationArg string) error {
	privateClusterArgs := privateClusterArgs(d.Projects, d.Network, d.PrivateClusterAccessLevel, d.PrivateClusterMasterIPRanges, cluster)
	// Create the cluster
	args := d.createCommand()
	args = append(args,
		"--project="+project,
		locationArg,
		"--network="+transformNetworkName(d.Projects, d.Network),
	)
	// A few args are not supported in GKE Autopilot cluster creation, so they should be left unset.
	// https://cloud.google.com/sdk/gcloud/reference/container/clusters/create-auto
	if !d.Autopilot {
		args = append(args, "--machine-type="+d.MachineType)
		args = append(args, "--num-nodes="+strconv.Itoa(d.NumNodes))
		args = append(args, "--image-type="+d.ImageType)
	}

	if d.WorkloadIdentityEnabled {
		args = append(args, fmt.Sprintf("--workload-pool=%s.svc.id.goog", project))
	}
	if d.ReleaseChannel != "" {
		args = append(args, "--release-channel="+d.ReleaseChannel)
		if d.Version == "latest" {
			// If latest is specified, get the latest version from server config for this channel.
			actualVersion, err := resolveLatestVersionInChannel(locationArg, d.ReleaseChannel)
			if err != nil {
				return err
			}
			klog.V(0).Infof("Using the latest version %q in %q channel", actualVersion, d.ReleaseChannel)
			args = append(args, "--cluster-version="+actualVersion)
		} else {
			args = append(args, "--cluster-version="+d.Version)
		}
	} else {
		args = append(args, "--cluster-version="+d.Version)
	}
	args = append(args, subNetworkArgs...)
	args = append(args, privateClusterArgs...)
	args = append(args, cluster.name)
	output, err := runWithOutputAndReturn(exec.Command("gcloud", args...))
	if err != nil {
		//parse output for match with regex error
		return fmt.Errorf("error creating cluster: %v, output: %q", err, output)
	}
	return nil
}

func (d *Deployer) createCommand() []string {
	// Use the --create-command flag if it's explicitly specified.
	if d.CreateCommandFlag != "" {
		return strings.Fields(d.CreateCommandFlag)
	}

	fs := make([]string, 0)
	if d.GcloudCommandGroup != "" {
		fs = append(fs, d.GcloudCommandGroup)
	}
	fs = append(fs, "container", "clusters")
	if d.Autopilot {
		fs = append(fs, "create-auto")
	} else {
		fs = append(fs, "create")
	}
	fs = append(fs, "--quiet")
	fs = append(fs, strings.Fields(d.GcloudExtraFlags)...)
	return fs
}

func (d *Deployer) IsUp() (up bool, err error) {
	if err := d.PrepareGcpIfNeeded(d.Projects[0]); err != nil {
		return false, err
	}

	kubeconfigFiles, err := d.Kubeconfig()
	if err != nil {
		return false, err
	}

	for _, kubeconfig := range strings.Split(kubeconfigFiles, string(os.PathListSeparator)) {
		// naively assume that if the api server reports nodes, the cluster is up
		lines, err := exec.CombinedOutputLines(
			exec.RawCommand("kubectl get nodes -o=name --kubeconfig=" + kubeconfig),
		)
		if err != nil {
			return false, metadata.NewJUnitError(err, strings.Join(lines, "\n"))
		}
		if len(lines) == 0 {
			return false, fmt.Errorf("no nodes active for given kubeconfig %q", kubeconfig)
		}
	}

	return true, nil
}

func (d *Deployer) TestSetup() error {
	if d.testPrepared {
		// Ensure setup is a singleton.
		return nil
	}

	// Only run prepare once for the first GCP project.
	if err := d.PrepareGcpIfNeeded(d.Projects[0]); err != nil {
		return err
	}
	if _, err := d.Kubeconfig(); err != nil {
		return err
	}
	if err := d.GetInstanceGroups(); err != nil {
		return err
	}
	if err := d.EnsureFirewallRules(); err != nil {
		return err
	}
	d.testPrepared = true
	return nil
}

// Kubeconfig returns a path to a kubeconfig file for the cluster in
// a temp directory, creating one if one does not exist.
// It also sets the KUBECONFIG environment variable appropriately.
func (d *Deployer) Kubeconfig() (string, error) {
	if d.kubecfgPath != "" {
		return d.kubecfgPath, nil
	}

	tmpdir, err := ioutil.TempDir("", "kubetest2-gke")
	if err != nil {
		return "", err
	}

	kubecfgFiles := make([]string, 0)
	for _, project := range d.Projects {
		for _, cluster := range d.projectClustersLayout[project] {
			filename := filepath.Join(tmpdir, fmt.Sprintf("kubecfg-%s-%s", project, cluster.name))
			if err := GetClusterCredentials(project, locationFlag(d.Regions, d.Zones, d.retryCount), cluster.name, filename); err != nil {
				return "", err
			}
			kubecfgFiles = append(kubecfgFiles, filename)
		}
	}

	d.kubecfgPath = strings.Join(kubecfgFiles, string(os.PathListSeparator))
	return d.kubecfgPath, nil
}

// verifyCommonFlags validates flags for up phase.
func (d *Deployer) VerifyUpFlags() error {
	if len(d.Projects) == 0 && d.BoskosProjectsRequested <= 0 {
		return fmt.Errorf("either --project or --projects-requested with a value larger than 0 must be set for GKE deployment")
	}

	if len(d.Clusters) == 0 {
		if len(d.Projects) > 1 || d.BoskosProjectsRequested > 1 {
			return fmt.Errorf("explicit --cluster-name must be set for multi-project profile")
		}
		if err := d.UpOptions.Validate(); err != nil {
			return err
		}
		d.Clusters = generateClusterNames(d.UpOptions.NumClusters, d.kubetest2CommonOptions.RunID())
	} else {
		klog.V(0).Infof("explicit --cluster-name specified, ignoring --num-clusters")
	}
	if err := d.VerifyNetworkFlags(); err != nil {
		return err
	}
	if err := d.VerifyLocationFlags(); err != nil {
		return err
	}
	if d.NumNodes <= 0 {
		return fmt.Errorf("--num-nodes must be larger than 0")
	}
	if err := validateVersion(d.Version); err != nil {
		return err
	}
	return nil
}

func generateClusterNames(numClusters int, uid string) []string {
	clusters := make([]string, numClusters)
	for i := 1; i <= numClusters; i++ {
		// Naming convention: https://cloud.google.com/sdk/gcloud/reference/container/clusters/create#POSITIONAL-ARGUMENTS
		// must start with an alphabet, max length 40

		// 4 characters for kt2- prefix (short for kubetest2)
		const fixedClusterNamePrefix = "kt2-"
		// 3 characters -99 suffix
		clusterNameSuffix := strconv.Itoa(i)
		// trim the uid to only use the first 33 characters
		var id string
		if uid != "" {
			const maxIDLength = 33
			if len(uid) > maxIDLength {
				id = uid[:maxIDLength]
			} else {
				id = uid
			}
			id += "-"
		}
		clusters[i-1] = fixedClusterNamePrefix + id + clusterNameSuffix
	}
	return clusters
}

func validateVersion(version string) error {
	switch version {
	case "latest", "":
		return nil
	default:
		re, err := regexp.Compile(`(\d)\.(\d)+(\.(\d)*(.*))?`)
		if err != nil {
			return err
		}
		if !re.MatchString(version) {
			return fmt.Errorf("unknown version %q", version)
		}
	}
	return nil
}
