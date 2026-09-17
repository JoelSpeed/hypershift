package main

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
	"k8s.io/apimachinery/pkg/util/sets"
)

func main() {
	if err := verifyAPIDependencies(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✅ API dependencies verification passed")

	if err := verifyLocalModuleVersions(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✅ In-repo module versions verification passed")
}

func verifyAPIDependencies() error {
	// Find the repository root and locate the API module
	repoRoot, err := findRepoRoot()
	if err != nil {
		return fmt.Errorf("failed to find repository root: %w", err)
	}

	apiModPath := filepath.Join(repoRoot, "api")

	// Load allowed dependencies from the .imports_allowed file
	allowedAPIModules, err := loadAllowedImports(apiModPath)
	if err != nil {
		return fmt.Errorf("failed to load allowed imports: %w", err)
	}

	// Read the go.mod file
	goModPath := filepath.Join(apiModPath, "go.mod")
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", goModPath, err)
	}

	// Parse the go.mod file
	modFile, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return fmt.Errorf("failed to parse %s: %w", goModPath, err)
	}

	// Check required dependencies
	var violations []string
	for _, req := range modFile.Require {
		if req.Indirect {
			// Skip indirect dependencies as they're managed transitively
			continue
		}

		modulePath := req.Mod.Path
		if !allowedAPIModules.Has(modulePath) {
			violations = append(violations, modulePath)
		}
	}

	if len(violations) > 0 {
		return fmt.Errorf(`❌ Unauthorized API dependencies detected:

%s

The HyperShift API module has strict dependency restrictions to maintain:
- API stability and compatibility
- Minimal dependency footprint
- Clear separation between API and implementation

Before adding any new dependencies to the API module, you must:

1. Consult with API reviewers to discuss alternatives
2. Ensure the dependency is absolutely necessary for the API layer
3. Verify it doesn't introduce breaking changes or version conflicts
4. Update the allowlist in api/.imports_allowed after approval

If this dependency is approved by API reviewers, add it to the allowlist in:
api/.imports_allowed

For questions, reach out to the HyperShift API review team.`,
			formatViolations(violations))
	}

	return nil
}

// verifyLocalModuleVersions checks that no in-repo module replaced into the root module
// requires a newer version of anything than the root module itself does.
//
// This is not a style rule. Minimal version selection resolves the root module's build over
// the union of the requirements of everything in its graph, and a local replace puts the
// replaced module's go.mod into that graph. So a `go mod tidy` inside api/ or
// externalplatform/ that picks up a newer release silently upgrades and re-vendors the whole
// root module the next time anyone runs `make deps`, in a commit that has nothing to do with
// dependencies. It is invisible in review and only shows up as unexplained vendor churn.
func verifyLocalModuleVersions() error {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return fmt.Errorf("failed to find repository root: %w", err)
	}

	rootMod, err := parseModFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		return err
	}

	rootVersions := map[string]string{}
	for _, req := range rootMod.Require {
		rootVersions[req.Mod.Path] = req.Mod.Version
	}

	// Locally replaced modules are the ones whose go.mod joins the root's graph. Their own
	// versions are meaningless pseudo-versions, so they are skipped in the comparison below.
	localModules := map[string]string{}
	for _, replace := range rootMod.Replace {
		if replace.New.Version != "" || !strings.HasPrefix(replace.New.Path, "./") {
			continue
		}
		localModules[replace.Old.Path] = filepath.Join(repoRoot, replace.New.Path)
	}

	var violations []string
	for _, path := range slices.Sorted(maps.Keys(localModules)) {
		localMod, err := parseModFile(filepath.Join(localModules[path], "go.mod"))
		if err != nil {
			return err
		}
		for _, req := range localMod.Require {
			if _, isLocal := localModules[req.Mod.Path]; isLocal {
				continue
			}
			rootVersion, required := rootVersions[req.Mod.Path]
			if !required || semver.Compare(req.Mod.Version, rootVersion) <= 0 {
				continue
			}
			violations = append(violations, fmt.Sprintf("  • %s requires %s %s, above the root module's %s",
				path, req.Mod.Path, req.Mod.Version, rootVersion))
		}
	}

	if len(violations) > 0 {
		return fmt.Errorf(`❌ In-repo modules require newer dependencies than the root module:

%s

Minimum version selection will raise the root module to these versions the next time
anyone runs 'make deps', re-vendoring the repository from a commit that did not ask for it.

Pin them back down in the in-repo module, then re-tidy:

	cd <module> && go get <dependency>@<root version> && go mod tidy

Raise the root module first if the newer version is genuinely wanted.`,
			strings.Join(violations, "\n"))
	}

	return nil
}

func parseModFile(path string) (*modfile.File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	modFile, err := modfile.Parse(path, data, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	return modFile, nil
}

func formatViolations(violations []string) string {
	var formatted []string
	for _, v := range violations {
		formatted = append(formatted, fmt.Sprintf("  • %s", v))
	}
	return strings.Join(formatted, "\n")
}

func loadAllowedImports(apiModPath string) (sets.Set[string], error) {
	allowedImportsPath := filepath.Join(apiModPath, ".imports_allowed")

	file, err := os.Open(allowedImportsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", allowedImportsPath, err)
	}
	defer file.Close()

	allowedModules := sets.New[string]()
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		allowedModules.Insert(line)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", allowedImportsPath, err)
	}

	return allowedModules, nil
}

func findRepoRoot() (string, error) {
	// Start from current working directory and walk up to find .git directory
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get working directory: %w", err)
	}

	dir := cwd
	for {
		// Check if .git directory exists
		if fileExists(filepath.Join(dir, ".git")) {
			return dir, nil
		}

		// Check if we've reached the root
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", fmt.Errorf("could not find repository root (no .git directory found)")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
