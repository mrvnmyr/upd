package main

// Go Build Tool v0.5 (2025-07-28) (755a3c7609f9d349)
// https://github.com/mrvnmyr/go-build-tool
//
// This is a simple standalone binary that builds the actual project.
//
// You could do the same thing with OS specific shell scripts, but we want to be
// cross-platform and not require much other than 'go' and 'git'.
//
//
// USAGE
//
// Copy this into a single subdirectory below your go project root (e.g.:
// "./build-tool/main.go") and tell people to build the project via running:
//
// $ go run ./build-tool/main.go
//
// To see all supported options/CLI flags, run:
//
// $ go run ./build-tool/main.go -h

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

const CONFIG_FILE_NAME = "build-tool-config.json"

var (
	buildHookPrePath  string
	buildHookPostPath string
)

var (
	flagBuildAll  = false
	flagDebug     = false
	flagNoGoGet   = false
	flagNoSymlink = false
	configPath    = ""
	config        BuildConfig

	currentBinPath = ""
)

type BuildConfig struct {
	BinName   string            `json:"binName"`
	Env       map[string]string `json:"env"`
	Platforms [][]string        `json:"platforms"`
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func debugf(fmtStr string, v ...interface{}) {
	if flagDebug {
		fmt.Printf(fmtStr, v...)
	}
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false // file doesn't exist or other error
	}
	mode := info.Mode()
	// Check if it's a regular file and executable by **someone**
	return mode.IsRegular() && (mode&0111 != 0)
}

func init() {
	scriptExt := "sh"
	if runtime.GOOS == "windows" {
		scriptExt = "bat"
	}

	buildHookPrePath = fmt.Sprintf("build-hook-pre.%s", scriptExt)
	buildHookPostPath = fmt.Sprintf("build-hook-post.%s", scriptExt)
}

func run(args []string, envMap map[string]string) {
	cmd := exec.Command(args[0], args[1:]...)

	// Set env vars: inherit, then override/add entry.Env
	env := os.Environ()
	if envMap != nil {
		for k, v := range envMap {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
	}
	cmd.Env = env

	if flagDebug {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	debugf("Running '%s'...\n", args)

	err := cmd.Run()
	check(err)
}

// EnsureSymlink ensures that `from` is a symlink pointing to `to`.
// It follows the logic:
// - If `from` does not exist, create symlink from → to.
// - If `from` exists and is not a symlink, do nothing.
// - If `from` exists and is a symlink:
//   - If it already points to `to`, do nothing.
//   - If not, recreate the symlink to point to `to`.
func ensureSymlink(from, to string) error {
	info, err := os.Lstat(from)
	if os.IsNotExist(err) {
		// 'from' does not exist; create symlink
		return os.Symlink(to, from)
	}
	if err != nil {
		return fmt.Errorf("failed to stat %q: %w", from, err)
	}

	if info.Mode()&os.ModeSymlink == 0 {
		// from exists but is not a symlink; do nothing
		return nil
	}

	// from is a symlink; read where it points to
	linkDest, err := os.Readlink(from)
	if err != nil {
		return fmt.Errorf("failed to read symlink %q: %w", from, err)
	}

	absTo, err := filepath.Abs(to)
	if err != nil {
		return fmt.Errorf("failed to get absolute path of %q: %w", to, err)
	}
	absLinkDest, err := filepath.Abs(filepath.Join(filepath.Dir(from), linkDest))
	if err != nil {
		return fmt.Errorf("failed to resolve absolute symlink target %q: %w", linkDest, err)
	}

	if absTo == absLinkDest {
		// Symlink already points to the correct location
		return nil
	}

	// Remove old symlink and recreate it
	if err := os.Remove(from); err != nil {
		return fmt.Errorf("failed to remove existing symlink %q: %w", from, err)
	}
	return os.Symlink(to, from)
}

func findDirUpwardsContaining(filename string) (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		// Construct full path to file in this directory
		fullPath := filepath.Join(dir, filename)
		if _, err := os.Stat(fullPath); err == nil {
			return dir, nil // Found the file
		}

		// Move up one directory
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the root directory, stop
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("file %s not found in any parent directory", filename)
}

func determineBinName() {
	if config.BinName != "" {
		// can be overriden in build.json
		return
	}

	readSuccessfullyFromGoMod := func() bool {
		// Try to read project name from ../go.mod
		goModPath, err := findDirUpwardsContaining("go.mod")
		if err != nil {
			return false
		}

		goModPath = filepath.Join(goModPath, "go.mod")

		goModContents, err := os.ReadFile(goModPath)
		if err == nil {
			debugf("Reading project name from: %s\n", goModPath)
			for _, line := range bytes.Split(goModContents, []byte{'\n'}) {
				if bytes.HasPrefix(line, []byte("module ")) {
					fields := bytes.Fields(line)
					if len(fields) >= 2 {
						modulePath := string(fields[1])
						config.BinName = filepath.Base(modulePath)
						debugf("New config BinName: %s\n", config.BinName)
					}
					return true
				}
			}
		}
		return false
	}

	if !readSuccessfullyFromGoMod() {
		config.BinName = "bin"
	}
}

func parseCLIFlags() {
	flag.BoolVar(&flagBuildAll, "a", false, "Build all defined GOOS/GOARCH targets")
	flag.BoolVar(&flagBuildAll, "all", false, "Build all defined GOOS/GOARCH targets (same as -a)")
	flag.BoolVar(&flagDebug, "d", false, "Enable debug mode")
	flag.BoolVar(&flagDebug, "debug", false, "Enable debug mode (same as -d)")
	flag.BoolVar(&flagNoGoGet, "nogg", false, "Don't run 'go get' before building")
	flag.BoolVar(&flagNoGoGet, "no-go-get", false, "Don't run 'go get' before building (same as -nogg)")
	flag.BoolVar(&flagNoSymlink, "nos", false, "Don't generate a symlink for the current target")
	flag.BoolVar(&flagNoSymlink, "no-symlink", false, "Don't generate a symlink for the current target (same as -nos)")

	flag.Usage = func() {
		fmt.Printf("To build a target for your current platform,\nrun this program without arguments.\n\n")
		flag.PrintDefaults()
	}

	// Parse flags
	flag.Parse()
}

func main() {
	parseCLIFlags()

	{ // cd to project root
		dir, err := findDirUpwardsContaining(CONFIG_FILE_NAME)
		check(err)

		err = os.Chdir(dir)
		check(err)

		cwd, err := os.Getwd()
		check(err)

		configPath = filepath.Join(dir, CONFIG_FILE_NAME)
		buildHookPrePath = filepath.Join(dir, buildHookPrePath)
		buildHookPostPath = filepath.Join(dir, buildHookPostPath)

		debugf("Current directory: %s\n", cwd)
		debugf("Config Path: %s\n", configPath)
		debugf("Build Hook Pre Path: %s\n", buildHookPrePath)
		debugf("Build Hook Post Path: %s\n", buildHookPostPath)
	}

	{ // parse 'build-oat.json'
		contents, err := os.ReadFile(configPath)
		check(err)

		err = json.Unmarshal(contents, &config)
		if err != nil {
			panic(err)
		}

		determineBinName()

		debugf("Config: %+v\n", config)
	}

	// RunEntry describes a single process to launch
	type RunEntry struct {
		Args []string
		Env  map[string]string
	}

	var entries []RunEntry

	// 'run go get' first
	if !flagNoGoGet {
		run([]string{"go", "get"}, nil)
	}

	{ // add all GOOS/GOARCH combinations from the config
		for _, triplet := range config.Platforms {
			goos := strings.ToLower(triplet[0])
			goarch := strings.ToLower(triplet[1])

			isCurrentPlatform := ((goos == runtime.GOOS) && (goarch == runtime.GOARCH))

			if flagBuildAll || isCurrentPlatform {
				binExtension := ""
				if goos == "windows" {
					binExtension = ".exe"
				}

				fileSuffix := fmt.Sprintf("%s_%s%s", goos, goarch, binExtension)
				fileName := fmt.Sprintf("%s_%s", config.BinName, fileSuffix)
				filePath := fmt.Sprintf("./bin/%s", fileName)

				if isCurrentPlatform {
					currentBinPath = filePath
				}

				env := map[string]string{
					"GOOS":   goos,
					"GOARCH": goarch,
				}

				// spread config.Env into env
				for k, v := range config.Env {
					env[k] = v
				}

				// append
				entries = append(entries, RunEntry{
					Args: []string{
						"go",
						"build",
						"-o",
						filePath,
					},
					Env: env,
				})
			}

		}
	}

	// symlink current GOOS/GOARCH
	if !flagNoSymlink {
		var currentSymlinkPath = ""
		if runtime.GOOS == "windows" {
			currentSymlinkPath = fmt.Sprintf("%s.exe", config.BinName)
		} else {
			currentSymlinkPath = fmt.Sprintf("%s", config.BinName)
		}

		err := ensureSymlink(currentSymlinkPath, currentBinPath)
		check(err)
	}

	{ // run it all
		debugf("Building...\n")

		// Result holds the outcome of running a RunEntry
		type Result struct {
			Entry    RunEntry
			Stdout   string
			Stderr   string
			ExitCode int
			Err      error
		}

		runEntry := func(entry RunEntry) Result {
			if len(entry.Args) == 0 {
				return Result{Entry: entry, Err: fmt.Errorf("no command specified")}
			}

			cmd := exec.Command(entry.Args[0], entry.Args[1:]...)

			// Set env vars: inherit, then override/add entry.Env
			env := os.Environ()
			for k, v := range entry.Env {
				env = append(env, fmt.Sprintf("%s=%s", k, v))
			}
			cmd.Env = env

			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			err := cmd.Run()
			exitCode := 0
			if err != nil {
				// Extract exit code if possible
				if exitErr, ok := err.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				} else {
					exitCode = -1
				}
			} else if cmd.ProcessState != nil {
				exitCode = cmd.ProcessState.ExitCode()
			}

			return Result{
				Entry:    entry,
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				ExitCode: exitCode,
				Err:      err,
			}
		}

		{ // optionally run 'build-hook-pre' if existing
			if isExecutable(buildHookPrePath) {
				run([]string{buildHookPrePath}, nil)
			}
		}

		{
			var (
				numWorkers = runtime.NumCPU()
				jobs       = make(chan RunEntry)
				results    = make(chan Result, len(entries))
			)

			{ // Run all Entries in parallel
				var wg sync.WaitGroup

				// Start workers
				for i := 0; i < numWorkers; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for entry := range jobs {
							results <- runEntry(entry)
						}
					}()
				}

				// Send jobs
				go func() {
					for _, entry := range entries {
						jobs <- entry
					}
					close(jobs)
				}()

				// Wait for all workers to finish
				go func() {
					wg.Wait()
					close(results)
				}()
			}

			{ // Print the results
				var sortedResults []Result
				{ // sort results according to string representation of result.Entry.Args - as we run it all in parallel which ofc mixes up "insertion order"
					for result := range results {
						sortedResults = append(sortedResults, result)
					}

					sort.Slice(sortedResults, func(i, j int) bool {
						return fmt.Sprintf("%v", sortedResults[i].Entry.Args) < fmt.Sprintf("%v", sortedResults[j].Entry.Args)
					})
				}

				var failures []Result
				for _, result := range sortedResults {
					if flagDebug {
						debugf("---\nCommand: %v\nEnv: %v\n", result.Entry.Args, result.Entry.Env)
						if result.ExitCode != 0 {
							debugf("Exit Code: %d\n", result.ExitCode)
						}
						if result.Stdout != "" {
							debugf("Stdout: %s\n", result.Stdout)
						}
						if result.Stderr != "" {
							debugf("Stderr: %s\n", result.Stderr)
						}
					}
					if result.Err != nil || result.ExitCode != 0 {
						failures = append(failures, result)
					}
				}

				if len(failures) > 0 {
					fmt.Fprintf(os.Stderr, "XXX : Failures:\n")
					for _, fail := range failures {
						fmt.Fprintf(os.Stderr, "Command: %v\nExit code: %d\nStdout: %sStderr: %sError: %v\n---\n",
							fail.Entry.Args, fail.ExitCode, fail.Stdout, fail.Stderr, fail.Err)
					}
					os.Exit(1)
				}

				if !flagBuildAll {
					debugf("\nAll builds succeeded. (Only Current GOOS/GOARCH, pass -all to build all targets)\n")
				} else {
					debugf("\nAll builds succeeded.\n")
				}
			}
		}

		{ // optionally run 'build-hook-post' if existing
			if isExecutable(buildHookPostPath) {
				run([]string{buildHookPostPath}, nil)
			}
		}
	}
}
