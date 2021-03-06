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

package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
	"k8s.io/klog"

	"sigs.k8s.io/kubetest2/pkg/exec"
	"sigs.k8s.io/kubetest2/pkg/metadata"
	"sigs.k8s.io/kubetest2/pkg/types"
)

// Main implements the kubetest2 deployer binary entrypoint
// Each deployer binary should invoke this, in addition to loading deployers
func Main(deployerName string, newDeployer types.NewDeployer) {
	// see cmd.go for the rest of the CLI boilerplate
	if err := Run(deployerName, newDeployer); err != nil {
		// only print the error if it's not an IncorrectUsage (which we've)
		// already output along with usage
		if _, isUsage := err.(types.IncorrectUsage); !isUsage {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		os.Exit(1)
	}
}

// RealMain contains nearly all of the application logic / control flow
// beyond the command line boilerplate
func RealMain(opts types.Options, d types.Deployer, tester types.Tester) (result error) {
	/*
		Now for the core kubetest2 logic:
		 - build
		 - cluster up
		 - test
		 - cluster down
		Throughout this, collecting metadata and writing it out on exit
	*/
	// TODO(bentheelder): signal handling & timeout

	klog.Infof("RunDir for this run: %q", opts.RunDir())

	// ensure the run dir
	if err := os.MkdirAll(opts.RunDir(), os.ModePerm); err != nil {
		return err
	}

	// setup the metadata writer
	junitRunner, err := os.Create(
		filepath.Join(opts.RunDir(), "junit_runner.xml"),
	)
	if err != nil {
		return errors.Wrap(err, "could not create runner output")
	}
	writer := metadata.NewWriter("kubetest2", junitRunner)
	// defer writing out the metadata on exit
	// NOTE: defer is LIFO, so this should actually be the finish time
	defer func() {
		// TODO(bentheelder): instead of keeping the first error, consider
		// a multi-error type
		if err := writer.Finish(); err != nil && result == nil {
			result = err
		}
		if err := junitRunner.Sync(); err != nil && result == nil {
			result = err
		}
		if err := junitRunner.Close(); err != nil && result == nil {
			result = err
		}
	}()

	klog.Infof("ID for this run: %q", opts.RunID())

	// build if specified
	if opts.ShouldBuild() {
		if err := writer.WrapStep("Build", d.Build); err != nil {
			// we do not continue to up / test etc. if build fails
			return err
		}
	}

	// ensure tearing down the cluster happens last, even if up or test fails.
	defer func() {
		if opts.ShouldDown() {
			// TODO(bentheelder): instead of keeping the first error, consider
			// a multi-error type
			if err := writer.WrapStep("Down", d.Down); err != nil && result == nil {
				result = err
			}
		}
	}()

	// up a cluster
	if opts.ShouldUp() {
		// TODO(bentheelder): this should write out to JUnit
		if err := writer.WrapStep("Up", d.Up); err != nil {
			// we do not continue to test if build fails
			return err
		}
	}

	// and finally test, if a test was specified
	if opts.ShouldTest() {
		test := exec.Command(tester.TesterPath, tester.TesterArgs...)
		exec.InheritOutput(test)

		envsForTester := os.Environ()
		// We expose both ARIFACTS and KUBETEST2_RUN_DIR so we can more granular about caching vs output in future.
		// also add run_dir to $PATH for locally built binaries
		updatedPath := opts.RunDir() + string(filepath.ListSeparator) + os.Getenv("PATH")
		envsForTester = append(envsForTester, fmt.Sprintf("%s=%s", "PATH", updatedPath))
		envsForTester = append(envsForTester, fmt.Sprintf("%s=%s", "ARTIFACTS", opts.RunDir()))
		envsForTester = append(envsForTester, fmt.Sprintf("%s=%s", "KUBETEST2_RUN_DIR", opts.RunDir()))
		envsForTester = append(envsForTester, fmt.Sprintf("%s=%s", "KUBETEST2_RUN_ID", opts.RunID()))
		// If the deployer provides a kubeconfig pass it to the tester
		// else assumes that it is handled offline by default methods like
		// ~/.kube/config
		if dWithKubeconfig, ok := d.(types.DeployerWithKubeconfig); ok {
			if kconfig, err := dWithKubeconfig.Kubeconfig(); err == nil {
				envsForTester = append(envsForTester, fmt.Sprintf("%s=%s", "KUBECONFIG", kconfig))
			}

		}
		test.SetEnv(envsForTester...)
		if err := writer.WrapStep("Test", test.Run); err != nil {
			return err
		}
	}

	return nil
}
