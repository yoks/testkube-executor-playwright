package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	junit "github.com/joshdk/go-junit"
	"github.com/kelseyhightower/envconfig"
	"github.com/kubeshop/testkube/pkg/api/v1/testkube"
	"github.com/kubeshop/testkube/pkg/executor"
	"github.com/kubeshop/testkube/pkg/executor/content"
	"github.com/kubeshop/testkube/pkg/executor/output"
	"github.com/kubeshop/testkube/pkg/executor/scraper"
	"github.com/kubeshop/testkube/pkg/executor/secret"
)

type Params struct {
	Endpoint        string // RUNNER_ENDPOINT
	AccessKeyID     string // RUNNER_ACCESSKEYID
	SecretAccessKey string // RUNNER_SECRETACCESSKEY
	Location        string // RUNNER_LOCATION
	Token           string // RUNNER_TOKEN
	Ssl             bool   // RUNNER_SSL
	ScrapperEnabled bool   // RUNNER_SCRAPPERENABLED
	GitUsername     string // RUNNER_GITUSERNAME
	GitToken        string // RUNNER_GITTOKEN
	Datadir         string // RUNNER_DATADIR
}

func NewPlaywrightRunner() (*PlaywrightRunner, error) {
	var params Params
	err := envconfig.Process("runner", &params)
	if err != nil {
		return nil, err
	}

	runner := &PlaywrightRunner{
		Fetcher: content.NewFetcher(""),
		Scraper: scraper.NewMinioScraper(
			params.Endpoint,
			params.AccessKeyID,
			params.SecretAccessKey,
			params.Location,
			params.Token,
			params.Ssl,
		),
		Params: params,
	}

	return runner, nil
}

// PlaywrightRunner - implements runner interface used in worker to start test execution
type PlaywrightRunner struct {
	Params  Params
	Fetcher content.ContentFetcher
	Scraper scraper.Scraper
}

func (r *PlaywrightRunner) Run(execution testkube.Execution) (result testkube.ExecutionResult, err error) {
	// make some validation
	err = r.Validate(execution)
	if err != nil {
		return result, err
	}

	// check that the datadir exists
	_, err = os.Stat(r.Params.Datadir)
	if errors.Is(err, os.ErrNotExist) {
		return result, err
	}

	if execution.Content.IsFile() {
		output.PrintEvent("using file", execution)

		// TODO add playwright project structure
		// TODO checkout this repo with `skeleton` path
		// TODO overwrite skeleton/playwright/integration/test.js
		//      file with execution content git file
		return result, fmt.Errorf("passing playwright test as single file not implemented yet")
	}

	runPath := filepath.Join(r.Params.Datadir, "repo", execution.Content.Repository.Path)
	if execution.Content.Repository.WorkingDir != "" {
		runPath = filepath.Join(r.Params.Datadir, "repo", execution.Content.Repository.WorkingDir)
	}

	if _, err := os.Stat(filepath.Join(runPath, "package.json")); err == nil {
		// be gentle to different playwright versions, run from local yarn deps
		out, err := executor.Run(runPath, "yarn", nil, "install")
		if err != nil {
			return result, fmt.Errorf("yarn install error: %w\n\n%s", err, out)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		out, err := executor.Run(runPath, "yarn", nil, "init", "--yes")
		if err != nil {
			return result, fmt.Errorf("yarn init error: %w\n\n%s", err, out)
		}

		out, err = executor.Run(runPath, "yarn", nil, "install", "playwright", "--save-dev")
		if err != nil {
			return result, fmt.Errorf("yarn install playwright error: %w\n\n%s", err, out)
		}
	} else {
		return result, fmt.Errorf("checking package.json file: %w", err)
	}

	// handle project local Playwright version install (`Playwright` app)
	out, err := executor.Run(runPath, "yarn", nil, "run", "playwright", "install")
	if err != nil {
		return result, fmt.Errorf("playwright binary install error: %w\n\n%s", err, out)
	}

	// convert executor env variables to os env variables
	for key, value := range execution.Envs {
		if err = os.Setenv(key, value); err != nil {
			return result, fmt.Errorf("setting env var: %w", err)
		}
	}

	envManager := secret.NewEnvManagerWithVars(execution.Variables)
	envManager.GetVars(execution.Variables)

	junitReportPath := filepath.Join(runPath, "src/test-results/junit.xml")

	os.Setenv("PLAYWRIGHT_JUNIT_OUTPUT_NAME", junitReportPath)
	args := []string{"run", "e2e", "--reporter", "junit"}

	// append args from execution
	args = append(args, execution.Args...)

	// run playwright inside repo directory ignore execution error in case of failed test
	out, err = executor.Run(runPath, "yarn", envManager, args...)
	out = envManager.Obfuscate(out)
	suites, serr := junit.IngestFile(junitReportPath)
	result = MapJunitToExecutionResults(out, suites)

	// scrape artifacts first even if there are errors above
	if r.Params.ScrapperEnabled && err != nil {

		executor.Run(runPath, "tar", nil, "-czvf", "test-results.tar.gz", "src/test-results")
		executor.Run(runPath, "mkdir", nil, "test-results")
		executor.Run(runPath, "mv", nil, "test-results.tar.gz", "test-results/test-results.tar.gz")

		directories := []string{
			filepath.Join(runPath, "test-results"),
		}
		err := r.Scraper.Scrape(execution.Id, directories)
		if err != nil {
			return result.WithErrors(fmt.Errorf("scrape artifacts error: %w", err)), nil
		}
	}

	return result.WithErrors(err, serr), nil
}

// Validate checks if Execution has valid data in context of Playwright executor
// Playwright executor runs currently only based on playwright project
func (r *PlaywrightRunner) Validate(execution testkube.Execution) error {

	if execution.Content == nil {
		return fmt.Errorf("can't find any content to run in execution data: %+v", execution)
	}

	if execution.Content.Repository == nil {
		return fmt.Errorf("playwright executor handle only repository based tests, but repository is nil")
	}

	if execution.Content.Repository.Branch == "" && execution.Content.Repository.Commit == "" {
		return fmt.Errorf("can't find branch or commit in params, repo:%+v", execution.Content.Repository)
	}

	return nil
}

func MapJunitToExecutionResults(out []byte, suites []junit.Suite) (result testkube.ExecutionResult) {
	status := testkube.PASSED_ExecutionStatus
	result.Status = &status
	result.Output = string(out)
	result.OutputType = "text/plain"

	for _, suite := range suites {
		for _, test := range suite.Tests {

			result.Steps = append(
				result.Steps,
				testkube.ExecutionStepResult{
					Name:     fmt.Sprintf("%s - %s", suite.Name, test.Name),
					Duration: test.Duration.String(),
					Status:   MapStatus(test.Status),
				})
		}

		// TODO parse sub suites recursively

	}

	return result
}

func MapStatus(in junit.Status) (out string) {
	switch string(in) {
	case "passed":
		return string(testkube.PASSED_ExecutionStatus)
	default:
		return string(testkube.FAILED_ExecutionStatus)
	}
}
