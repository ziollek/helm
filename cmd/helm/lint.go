/*
Copyright The Helm Authors.

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

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/lint/support"
)

const (
	rootScope = "."
	globalKey = "global"
)

var longLintHelp = `
This command takes a path to a chart and runs a series of tests to verify that
the chart is well-formed.

If the linter encounters things that will cause the chart to fail installation,
it will emit [ERROR] messages. If it encounters issues that break with convention
or recommendation, it will emit [WARNING] messages.
`

type scopedChart struct {
	chart *chart.Chart
	path  string
}

func newLintCmd(out io.Writer) *cobra.Command {
	client := action.NewLint()
	valueOpts := &values.Options{}
	var kubeVersion string

	cmd := &cobra.Command{
		Use:   "lint PATH",
		Short: "examine a chart for possible issues",
		Long:  longLintHelp,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return fmt.Errorf("invalid call, expected path to a single chart, got: %s", args)
			}
			path := rootScope
			if len(args) > 0 {
				path = args[0]
			}

			if kubeVersion != "" {
				parsedKubeVersion, err := chartutil.ParseKubeVersion(kubeVersion)
				if err != nil {
					return fmt.Errorf("invalid kube version '%s': %s", kubeVersion, err)
				}
				client.KubeVersion = parsedKubeVersion
			}

			rootChart, err := loader.Load(path)
			if err != nil {
				return fmt.Errorf("cannot load chart, due to: %s", err)
			}
			charts := map[string]scopedChart{rootScope: {rootChart, path}}

			if client.WithSubcharts {
				if err = loadScopedCharts(path, rootChart, charts); err != nil {
					return fmt.Errorf("cannot load sub-charts, due to: %s", err)
				}
			}

			client.Namespace = settings.Namespace()
			vals, err := valueOpts.MergeValuesWithBase(getter.All(settings), rootChart.Values)
			if err != nil {
				return err
			}

			var message strings.Builder
			failed := 0
			errorsOrWarnings := 0

			for scope, single := range charts {
				result := client.Run([]string{single.path}, getValuesForScope(scope, vals))

				// If there is no errors/warnings and quiet flag is set
				// go to the next chart
				hasWarningsOrErrors := action.HasWarningsOrErrors(result)
				if hasWarningsOrErrors {
					errorsOrWarnings++
				}
				if client.Quiet && !hasWarningsOrErrors {
					continue
				}

				fmt.Fprintf(&message, "==> Linting chart: name=%s, scope=%s, path=%s\n", single.chart.Name(), scope, single.path)

				// All the Errors that are generated by a chart
				// that failed a lint will be included in the
				// results.Messages so we only need to print
				// the Errors if there are no Messages.
				if len(result.Messages) == 0 {
					for _, err := range result.Errors {
						fmt.Fprintf(&message, "Error %s\n", err)
					}
				}

				for _, msg := range result.Messages {
					if !client.Quiet || msg.Severity > support.InfoSev {
						fmt.Fprintf(&message, "%s\n", msg)
					}
				}

				if len(result.Errors) != 0 {
					failed++
				}

				// Adding extra new line here to break up the
				// results, stops this from being a big wall of
				// text and makes it easier to follow.
				fmt.Fprint(&message, "\n")
			}

			fmt.Fprint(out, message.String())

			summary := fmt.Sprintf("%d chart(s) linted, %d chart(s) failed", len(charts), failed)
			if failed > 0 {
				return errors.New(summary)
			}
			if !client.Quiet || errorsOrWarnings > 0 {
				fmt.Fprintln(out, summary)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.BoolVar(&client.Strict, "strict", false, "fail on lint warnings")
	f.BoolVar(&client.WithSubcharts, "with-subcharts", false, "lint dependent charts")
	f.BoolVar(&client.Quiet, "quiet", false, "print only warnings and errors")
	f.StringVar(&kubeVersion, "kube-version", "", "Kubernetes version used for capabilities and deprecation checks")
	addValueOptionsFlags(f, valueOpts)

	return cmd
}

func loadScopedCharts(path string, rootChart *chart.Chart, charts map[string]scopedChart) error {
	subCharts := []scopedChart{}
	err := filepath.Walk(filepath.Join(path, "charts"), func(subPath string, info os.FileInfo, _ error) error {
		var subChart *chart.Chart
		var err error
		if info.Name() == "Chart.yaml" {
			if subChart, err = loader.Load(filepath.Dir(subPath)); err != nil {
				return fmt.Errorf("cannot load sub-chart from %s, due to: %s", subPath, err)
			}
			subCharts = append(subCharts, scopedChart{subChart, filepath.Dir(subPath)})
		} else if strings.HasSuffix(subPath, ".tgz") || strings.HasSuffix(subPath, ".tar.gz") {
			if subChart, err = loader.Load(subPath); err != nil {
				return fmt.Errorf("cannot load sub-chart from %s, due to: %s", subPath, err)
			}
			subCharts = append(subCharts, scopedChart{subChart, subPath})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, sub := range subCharts {
		for _, metaSub := range rootChart.Metadata.Dependencies {
			if metaSub.Name == sub.chart.Name() && metaSub.Version == sub.chart.Metadata.Version {
				if metaSub.Alias != "" {
					charts[metaSub.Alias] = sub
				} else {
					charts[metaSub.Name] = sub
				}
			}
		}
	}
	return nil
}

func getValuesForScope(scope string, vals map[string]interface{}) map[string]interface{} {
	if scope == rootScope {
		return vals
	}
	result := map[string]interface{}{globalKey: nil}
	if _, ok := vals[scope]; ok {
		if _, ok = vals[scope].(map[string]interface{}); ok {
			result = vals[scope].(map[string]interface{})
		}
	}
	if _, ok := vals[globalKey]; ok {
		result[globalKey] = vals[globalKey]
	}
	return result
}
