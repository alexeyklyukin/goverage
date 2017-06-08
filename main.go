package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/tools/cover"
)

const usageMessage = "" +
	`Usage:	goverage [flags] -coverprofile=coverage.out package...
`

var (
	coverprofile string
	covermode    string
	cpu          string
	parallel     string
	timeout      string
	short        bool
	v            bool
	race         bool
)

func init() {
	flag.StringVar(&coverprofile, "coverprofile", "coverage.out", "Write a coverage profile to the file after all tests have passed")
	flag.StringVar(&covermode, "covermode", "", "sent as covermode argument to go test")
	flag.StringVar(&cpu, "cpu", "", "sent as cpu argument to go test")
	flag.StringVar(&parallel, "parallel", "", "sent as parallel argument to go test")
	flag.StringVar(&timeout, "timeout", "", "sent as timeout argument to go test")
	flag.BoolVar(&short, "short", false, "sent as short argument to go test")
	flag.BoolVar(&v, "v", false, "sent as v argument to go test")
	flag.BoolVar(&race, "race", false, "enable data race detection")
}

func usage() {
	fmt.Fprintln(os.Stderr, usageMessage)
	fmt.Fprintln(os.Stderr, "Flags:")
	flag.PrintDefaults()
	os.Exit(2)
}

type ExitError struct {
	Msg  string
	Code int
}

func (e *ExitError) Error() string {
	return e.Msg
}

func main() {
	flag.Usage = usage
	flag.Parse()
	if err := run(coverprofile, flag.Args(), covermode, cpu, parallel, timeout, short, v); err != nil {
		code := 1
		if err, ok := err.(*ExitError); ok {
			code = err.Code
		}
		if err.Error() != "" {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(code)
	}
}

func run(coverprofile string, args []string, covermode, cpu, parallel, timeout string, short, v bool) error {
	if coverprofile == "" {
		usage()
		return nil
	}
	if race && covermode != "" && covermode != "atomic" {
		return fmt.Errorf("cannot use race flag and covermode=%s. See more detail on golang/go#12118.", covermode)
	}

	file, err := os.Create(coverprofile)
	if err != nil {
		return err
	}
	defer file.Close()
	// pkgs is packages to run tests and get coverage.
	var pkgs []string
	for _, pkg := range args {
		ps, err := getPkgs(pkg)
		if err != nil {
			return err
		}
		pkgs = append(pkgs, ps...)
	}
	if len(pkgs) == 0 {
		pkgs = []string{"."}
	}
	cpss := make([][]*cover.Profile, len(pkgs))
	hasFailedTest := false
	for i, pkg := range pkgs {
		if process, err := hasTests(pkg); (err != nil) || !process {
			if err != nil {
				log.Printf("got error when checking tests for package %q: %v", pkg, err)
			} else {
				log.Printf("Package %q has no tests defined", pkg)
			}
			continue
		}
		deps, err := getPackageDependencies(pkg, pkgs)
		if err != nil {
			log.Printf("got error when getting dependencies for package %q: %v", pkg, err)
			continue
		} else {
			log.Printf("%s package own depenencies: %v", pkg, deps)
		}
		coverpkg  := strings.Join(deps, ",")
		optionalArgs := buildOptionalTestArgs(coverpkg, covermode, cpu, parallel, timeout, short, v)
		cps, success, err := coverage(pkg, optionalArgs, v)
		if !success {
			hasFailedTest = true
		}
		if err != nil {
			// Do not return err here. It could be just tests are not found for the package.
			log.Printf("got error for package %q: %v", pkg, err)
			continue
		}
		if cps != nil {
			cpss[i] = cps
		}
	}
	dumpcp(file, mergeProfiles(cpss))
	if hasFailedTest {
		return &ExitError{Code: 1}
	}
	return nil
}

func getPackageDependencies(pkgName string, projectPackages []string) ([]string, error) {
	// call go list '{{ .Deps }}
	args := []string{"list", "-f", `{{ join .Deps " "}}`, pkgName}
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		return nil, err
	}
	allDeps := strings.Fields(strings.Trim(string(out), "\n"))
	ownDeps := make([]string, 0, len(allDeps))
	for _, dep := range(allDeps) {
		for _, pkg := range(projectPackages) {
			if dep == pkg {
				ownDeps = append(ownDeps, dep)
				break
			}
		}
	}
	return ownDeps, nil
}

func hasTests(pkgName string) (bool, error) {
	args := []string{"list", "-f", `{{ join .TestGoFiles " "}}`, pkgName}
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		return false, err
	}
	tests := strings.Fields(strings.Trim(string(out), "\n"))
	return len(tests) > 0, nil
}


// buildOptionalTestArgs returns common optional args for go test regardless
// target packages. coverpkg must not be empty.
func buildOptionalTestArgs(coverpkg, covermode, cpu, parallel, timeout string, short, v bool) []string {
	args := []string{"-coverpkg", coverpkg}
	if covermode != "" {
		args = append(args, "-covermode", covermode)
	}
	if cpu != "" {
		args = append(args, "-cpu", cpu)
	}
	if parallel != "" {
		args = append(args, "-parallel", parallel)
	}
	if timeout != "" {
		args = append(args, "-timeout", timeout)
	}
	if short {
		args = append(args, "-short")
	}
	if v {
		args = append(args, "-v")
	}
	if race {
		args = append(args, "-race")
	}
	return args
}

// getPkgs returns packages for measuring coverage. Returned packages doesn't
// contain vendor packages.
func getPkgs(pkg string) ([]string, error) {
	if pkg == "" {
		pkg = "./..."
	}
	out, err := exec.Command("go", "list", pkg).CombinedOutput()
	if err != nil {
		return nil, err
	}
	allPkgs := strings.Split(strings.Trim(string(out), "\n"), "\n")
	pkgs := make([]string, 0, len(allPkgs))
	for _, p := range allPkgs {
		if !strings.Contains(p, "/vendor/") {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs, nil
}

// coverage runs test for the given pkg and returns cover profile.
// success indicates "go test" succeeded or not. coverage may return profiles
// even when success=false. When "go test" fails, coverage outputs "go test"
// result to stdout even when verbose=false.
func coverage(pkg string, optArgs []string, verbose bool) (profiles []*cover.Profile, success bool, err error) {
	coverprofile, err := tmpProfileName()
	if err != nil {
		return nil, false, err
	}
	// Remove coverprofile created by "go test".
	defer os.Remove(coverprofile)
	args := append([]string{"test", pkg, "-coverprofile", coverprofile}, optArgs...)
	cmd := exec.Command("go", args...)
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	if verbose {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stdout = stdout
		cmd.Stderr = stderr
	}
	if err := cmd.Run(); err != nil {
		fmt.Fprint(os.Stdout, stdout.String())
		fmt.Fprint(os.Stderr, stderr.String())
		// "go test" can creates coverprofile even when "go test" failes, so do not
		// return error here if coverprofile is created.
		if !isExist(coverprofile) {
			return nil, false, fmt.Errorf("failed to run 'go test %v': %v", pkg, err)
		}
	} else {
		if !isExist(coverprofile) {
			// There are no test and coverprofile is not created.
			return nil, true, nil
		}
		success = true
	}
	profiles, err = cover.ParseProfiles(coverprofile)
	return profiles, success, err
}

func tmpProfileName() (string, error) {
	f, err := ioutil.TempFile("", "goverage")
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(f.Name()); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func isExist(filename string) bool {
	_, err := os.Stat(filename)
	return err == nil
}

// mergeProfiles merges cover profiles. It assumes target packages of each
// cover profile are same and sorted.
func mergeProfiles(cpss [][]*cover.Profile) []*cover.Profile {
	// skip head empty profiles ([no test files])
	for i, cps := range cpss {
		if len(cps) == 0 {
			continue // [no test files]
		}
		cpss = cpss[i:]
		break
	}
	if len(cpss) == 0 {
		return nil // empty
	} else if len(cpss) == 1 {
		return cpss[0] // only one profile
	}
	result, rest := cpss[0], cpss[1:]
	for i, profile := range result {
		for _, cps := range rest {
			if len(cps) == 0 {
				continue // [no test files]
			}
			cp := cps[i]
			for j, block := range cp.Blocks {
				switch profile.Mode {
				case "set":
					profile.Blocks[j].Count |= block.Count
				case "count", "atomic":
					profile.Blocks[j].Count += block.Count
				}
			}
		}
	}
	return result
}

// dumpcp dumps cover profile result to io.Writer.
func dumpcp(w io.Writer, cps []*cover.Profile) {
	if len(cps) == 0 {
		return
	}
	fmt.Fprintf(w, "mode: %v\n", cps[0].Mode)
	for _, cp := range cps {
		for _, b := range cp.Blocks {
			_ = b
			// ref: golang.org/x/tools/cover
			// name.go:line.column,line.column numberOfStatements count
			const blockFmt = "%s:%d.%d,%d.%d %d %d\n"
			fmt.Fprintf(w, blockFmt, cp.FileName, b.StartLine, b.StartCol, b.EndLine, b.EndCol, b.NumStmt, b.Count)
		}
	}
}
